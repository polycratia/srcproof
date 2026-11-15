// Package ident checks the code spans of an article against the Go code at the
// commits the article pins: a name has to exist there, and a path has to be in
// the tree at one of them.
package ident

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/polycratia/srcproof/internal/gitrepo"
	"github.com/polycratia/srcproof/internal/links"
	"github.com/polycratia/srcproof/internal/lock"
)

// Reason names what was decided about one code span.
type Reason string

const (
	ReasonFound       Reason = "found"
	ReasonAllowed     Reason = "allowed"
	ReasonPredeclared Reason = "predeclared"
	ReasonExternal    Reason = "external"
	ReasonUnknown     Reason = "unknown-name"
	ReasonMissingPath Reason = "missing-path"
	ReasonUndecided   Reason = "undecided"
)

// Severity is how a reason weighs on the gate.
type Severity string

const (
	Pass      Severity = "pass"
	Fail      Severity = "fail"
	Undecided Severity = "undecided"
	Listed    Severity = "listed"
)

// Kind tells what a code span is read as.
type Kind string

const (
	KindIdent Kind = "identifier"
	KindPath  Kind = "path"
)

// nearestNames is how many existing names an unknown one is offered.
const nearestNames = 3

var (
	identShape = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*$`)
	pathShape  = regexp.MustCompile(`^[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)*/?$`)
	upperRune  = regexp.MustCompile(`[A-Z]`)
)

// extensions are the suffixes that make a dotted span without a slash a file
// of the repository rather than a qualified name.
var extensions = map[string]bool{
	"go": true, "mod": true, "sum": true, "md": true, "lock": true,
	"txt": true, "json": true, "yml": true, "yaml": true, "toml": true,
}

var predeclared = map[string]bool{
	"any": true, "bool": true, "byte": true, "comparable": true,
	"complex64": true, "complex128": true, "error": true, "float32": true,
	"float64": true, "int": true, "int8": true, "int16": true, "int32": true,
	"int64": true, "rune": true, "string": true, "uint": true, "uint8": true,
	"uint16": true, "uint32": true, "uint64": true, "uintptr": true,
	"true": true, "false": true, "iota": true, "nil": true,
	"append": true, "cap": true, "clear": true, "close": true,
	"complex": true, "copy": true, "delete": true, "imag": true,
	"len": true, "make": true, "max": true, "min": true, "new": true,
	"panic": true, "print": true, "println": true, "real": true,
	"recover": true,
}

// Tree reads the files of a commit, the way gitrepo.Repo does.
type Tree interface {
	Files(commit string) ([]string, error)
	Blob(commit, path string) ([]byte, error)
}

// Commit is one commit an article pins, in one repository.
type Commit struct {
	Repo string
	SHA  string
}

// Span is one code span of an article, with where it was written.
type Span struct {
	File string
	Line int
	Text string
}

// Result is what was decided about one code span.
type Result struct {
	Span    Span
	Kind    Kind
	Name    string
	Reason  Reason
	Nearest []string
	Message string
}

// Severity is how the result weighs on the gate.
func (r Result) Severity() Severity {
	switch r.Reason {
	case ReasonFound, ReasonAllowed, ReasonPredeclared:
		return Pass
	case ReasonExternal:
		return Listed
	case ReasonUndecided:
		return Undecided
	}
	return Fail
}

// String renders a result as file:line: message.
func (r Result) String() string {
	return fmt.Sprintf("%s:%d: %s", r.Span.File, r.Span.Line, r.Message)
}

// Table is the symbol table of one commit.
type Table struct {
	Repo     string
	Commit   string
	Names    map[string]bool
	Paths    map[string]bool
	External map[string]bool
	Broken   []string
}

// Request is one article to check, with the clones its commits are read in.
type Request struct {
	Article string
	Source  string
	Lock    lock.File
	Trees   map[string]Tree
}

// Check decides every code span of an article that is shaped like a Go name or
// like a repository path. Without a clone for a pinned repository nothing is
// read and nothing is reported: a code span is prose, not a recorded claim.
func Check(req Request) ([]Result, error) {
	tables, err := buildTables(req)
	if err != nil {
		return nil, err
	}
	if len(tables) == 0 {
		return nil, nil
	}
	name := filepath.Base(req.Article)
	allowed := make(map[string]bool)
	for _, record := range req.Lock.Allows {
		if record.Article == name {
			allowed[record.Word] = true
		}
	}
	var results []Result
	for _, span := range Spans(req.Article, req.Source) {
		if result, ok := decide(span, tables, allowed); ok {
			results = append(results, result)
		}
	}
	return results, nil
}

// Pinned returns the commits an article pins, once per repository and commit.
func Pinned(file, source string) []Commit {
	var found []Commit
	seen := make(map[string]bool)
	for _, link := range links.Extract(file, source) {
		if !link.Pinned() {
			continue
		}
		commit := Commit{Repo: strings.ToLower(link.Owner + "/" + link.Name), SHA: link.Ref}
		key := commit.Repo + " " + commit.SHA
		if seen[key] {
			continue
		}
		seen[key] = true
		found = append(found, commit)
	}
	return found
}

func buildTables(req Request) ([]*Table, error) {
	var tables []*Table
	for _, commit := range Pinned(req.Article, req.Source) {
		tree, named := req.Trees[commit.Repo]
		if !named {
			continue
		}
		table, err := Build(commit.Repo, commit.SHA, tree)
		switch {
		// A commit the clone does not have is reported where the link itself is
		// checked, and nothing can be read out of it here.
		case errors.Is(err, gitrepo.ErrMissingHistory):
			continue
		case err != nil:
			return nil, fmt.Errorf("%s: %w", req.Article, err)
		}
		tables = append(tables, table)
	}
	return tables, nil
}

// Build reads the Go code of one commit into a symbol table. A file that does
// not parse is recorded as broken rather than guessed at, so a name that is
// missing from what was read can still be told apart from a name that may be
// in the part that could not be.
func Build(repo, commit string, tree Tree) (*Table, error) {
	files, err := tree.Files(commit)
	if err != nil {
		return nil, err
	}
	table := &Table{
		Repo:     repo,
		Commit:   commit,
		Names:    make(map[string]bool),
		Paths:    make(map[string]bool),
		External: make(map[string]bool),
	}
	for _, name := range files {
		table.Paths[name] = true
		for dir := path.Dir(name); dir != "." && dir != "/" && dir != ""; dir = path.Dir(dir) {
			table.Paths[dir] = true
		}
	}
	module := ""
	if data, err := tree.Blob(commit, "go.mod"); err == nil {
		module = modulePath(string(data))
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if !strings.HasSuffix(name, ".go") || skipped(name) {
			continue
		}
		data, err := tree.Blob(commit, name)
		switch {
		case errors.Is(err, gitrepo.ErrMissingPath), errors.Is(err, gitrepo.ErrNotQuotable):
			continue
		case err != nil:
			return nil, err
		}
		parsed, err := parser.ParseFile(fset, name, data, parser.SkipObjectResolution)
		if err != nil {
			table.Broken = append(table.Broken, name)
			continue
		}
		table.add(parsed, module)
	}
	sort.Strings(table.Broken)
	return table, nil
}

// add records the names one file declares, each on its own and under the
// package it is declared in, so both Header and lock.Header are found.
func (t *Table) add(file *ast.File, module string) {
	pkg := file.Name.Name
	t.Names[pkg] = true
	for _, spec := range file.Imports {
		if name, external := importName(spec, module); external {
			t.External[name] = true
		}
	}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil || len(d.Recv.List) == 0 {
				t.record(pkg, d.Name.Name)
				continue
			}
			if receiver := typeName(d.Recv.List[0].Type); receiver != "" {
				t.record(pkg, receiver+"."+d.Name.Name)
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					t.record(pkg, s.Name.Name)
					t.addFields(pkg, s.Name.Name, s.Type)
				case *ast.ValueSpec:
					for _, name := range s.Names {
						t.record(pkg, name.Name)
					}
				}
			}
		}
	}
}

func (t *Table) record(pkg, name string) {
	t.Names[name] = true
	t.Names[pkg+"."+name] = true
}

func (t *Table) addFields(pkg, owner string, expr ast.Expr) {
	structure, ok := expr.(*ast.StructType)
	if !ok || structure.Fields == nil {
		return
	}
	for _, field := range structure.Fields.List {
		if len(field.Names) == 0 {
			if embedded := typeName(field.Type); embedded != "" {
				t.record(pkg, owner+"."+embedded)
			}
			continue
		}
		for _, name := range field.Names {
			t.record(pkg, owner+"."+name.Name)
		}
	}
}

// typeName reads the name a receiver or an embedded field is written with. A
// generic receiver is written as Type[T], so the type parameters are peeled
// off before the name is taken.
func typeName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.StarExpr:
		return typeName(e.X)
	case *ast.ParenExpr:
		return typeName(e.X)
	case *ast.IndexExpr:
		return typeName(e.X)
	case *ast.IndexListExpr:
		return typeName(e.X)
	case *ast.SelectorExpr:
		return e.Sel.Name
	case *ast.Ident:
		return e.Name
	}
	return ""
}

func importName(spec *ast.ImportSpec, module string) (string, bool) {
	quoted := strings.Trim(spec.Path.Value, `"`)
	if quoted == "" {
		return "", false
	}
	if module != "" && (quoted == module || strings.HasPrefix(quoted, module+"/")) {
		return "", false
	}
	name := path.Base(quoted)
	if spec.Name != nil {
		name = spec.Name.Name
	}
	if name == "" || name == "_" || name == "." {
		return "", false
	}
	return name, true
}

func modulePath(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if rest, found := strings.CutPrefix(strings.TrimSpace(line), "module "); found {
			return strings.Trim(strings.TrimSpace(rest), `"`)
		}
	}
	return ""
}

func skipped(name string) bool {
	for _, segment := range strings.Split(name, "/") {
		if segment == "vendor" || segment == "testdata" {
			return true
		}
	}
	return false
}

func decide(span Span, tables []*Table, allowed map[string]bool) (Result, bool) {
	name := strings.TrimSuffix(span.Text, "()")
	called := name != span.Text
	switch {
	case !called && looksLikePath(span.Text):
		return decidePath(span, strings.TrimSuffix(span.Text, "/"), tables, allowed), true
	case identShape.MatchString(name) && (called || strings.ContainsAny(name, "._") || upperRune.MatchString(name)):
		return decideName(span, name, tables, allowed), true
	}
	return Result{}, false
}

func decidePath(span Span, name string, tables []*Table, allowed map[string]bool) Result {
	result := Result{Span: span, Kind: KindPath, Name: name}
	if allowed[span.Text] || allowed[name] {
		result.Reason = ReasonAllowed
		result.Message = fmt.Sprintf("%s is on an allow line of the lock", name)
		return result
	}
	for _, table := range tables {
		if table.Paths[name] {
			result.Reason = ReasonFound
			result.Message = fmt.Sprintf("%s is in the tree of %s at commit %s", name, table.Repo, table.Commit)
			return result
		}
	}
	result.Nearest = nearest(name, pathsOf(tables))
	result.Reason = ReasonMissingPath
	result.Message = fmt.Sprintf("the code span %q is not a path in the tree at %s%s",
		name, where(tables), suggestion("paths", result.Nearest))
	return result
}

func decideName(span Span, name string, tables []*Table, allowed map[string]bool) Result {
	result := Result{Span: span, Kind: KindIdent, Name: name}
	switch {
	case allowed[span.Text] || allowed[name]:
		result.Reason = ReasonAllowed
		result.Message = fmt.Sprintf("%s is on an allow line of the lock", name)
		return result
	case !strings.Contains(name, ".") && predeclared[name]:
		result.Reason = ReasonPredeclared
		result.Message = fmt.Sprintf("%s is predeclared in Go", name)
		return result
	}
	for _, table := range tables {
		if table.Names[name] {
			result.Reason = ReasonFound
			result.Message = fmt.Sprintf("%s is in the Go code of %s at commit %s", name, table.Repo, table.Commit)
			return result
		}
	}
	if qualifier, _, qualified := strings.Cut(name, "."); qualified {
		for _, table := range tables {
			if table.External[qualifier] {
				result.Reason = ReasonExternal
				result.Message = fmt.Sprintf("%s is read through %q, a package imported from outside the repository: external, not checked", name, qualifier)
				return result
			}
		}
	}
	// A file that did not parse may hold the name, so the gate cannot call it
	// missing; it says so and leaves the verdict undecided.
	if broken := brokenOf(tables); len(broken) > 0 {
		result.Reason = ReasonUndecided
		result.Message = fmt.Sprintf("cannot decide: %q is not in the Go code at %s, and %s did not parse there",
			name, where(tables), strings.Join(broken, ", "))
		return result
	}
	result.Nearest = nearest(name, namesOf(tables))
	result.Reason = ReasonUnknown
	result.Message = fmt.Sprintf("the code span %q names nothing in the Go code at %s%s",
		name, where(tables), suggestion("names", result.Nearest))
	return result
}

func looksLikePath(text string) bool {
	if !pathShape.MatchString(text) {
		return false
	}
	if strings.Contains(text, "/") {
		return true
	}
	at := strings.LastIndexByte(text, '.')
	return at > 0 && extensions[strings.ToLower(text[at+1:])]
}

func where(tables []*Table) string {
	parts := make([]string, 0, len(tables))
	for _, table := range tables {
		parts = append(parts, fmt.Sprintf("%s commit %s", table.Repo, table.Commit))
	}
	return strings.Join(parts, ", ")
}

func suggestion(what string, nearest []string) string {
	if len(nearest) == 0 {
		return ""
	}
	return fmt.Sprintf("; the nearest %s are %s", what, strings.Join(nearest, ", "))
}

// nearest returns up to three existing names closest to an unknown one. A
// name further away than half the length of what was written is left out: a
// suggestion a reader cannot recognise is worse than none.
func nearest(name string, candidates []string) []string {
	limit := len(name) / 2
	if limit < 2 {
		limit = 2
	}
	type scored struct {
		name     string
		distance int
	}
	var found []scored
	for _, candidate := range candidates {
		if d := distance(name, candidate); d > 0 && d <= limit {
			found = append(found, scored{name: candidate, distance: d})
		}
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].distance != found[j].distance {
			return found[i].distance < found[j].distance
		}
		return found[i].name < found[j].name
	})
	if len(found) > nearestNames {
		found = found[:nearestNames]
	}
	out := make([]string, 0, len(found))
	for _, candidate := range found {
		out = append(out, candidate.name)
	}
	return out
}

func distance(a, b string) int {
	previous := make([]int, len(b)+1)
	current := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			current[j] = min(previous[j-1]+cost, previous[j]+1, current[j-1]+1)
		}
		previous, current = current, previous
	}
	return previous[len(b)]
}

func namesOf(tables []*Table) []string {
	return keys(tables, func(t *Table) map[string]bool { return t.Names })
}

func pathsOf(tables []*Table) []string {
	return keys(tables, func(t *Table) map[string]bool { return t.Paths })
}

func keys(tables []*Table, of func(*Table) map[string]bool) []string {
	seen := make(map[string]bool)
	var out []string
	for _, table := range tables {
		for key := range of(table) {
			if !seen[key] {
				seen[key] = true
				out = append(out, key)
			}
		}
	}
	sort.Strings(out)
	return out
}

func brokenOf(tables []*Table) []string {
	var out []string
	for _, table := range tables {
		out = append(out, table.Broken...)
	}
	sort.Strings(out)
	return out
}

// Spans returns the code spans of an article. Fenced code blocks are skipped,
// so a name a block only shows is not read as a claim about the code.
func Spans(file, source string) []Span {
	var spans []Span
	var fenceChar byte
	fenceLen := 0
	inFence := false
	for i, raw := range strings.Split(source, "\n") {
		line := strings.TrimRight(raw, "\r")
		if char, length, ok := fenceMarker(line); ok {
			switch {
			case !inFence:
				inFence, fenceChar, fenceLen = true, char, length
			case char == fenceChar && length >= fenceLen && closesFence(line, char, length):
				inFence = false
			}
			continue
		}
		if inFence {
			continue
		}
		for _, text := range codeSpans(line) {
			if text == "" {
				continue
			}
			spans = append(spans, Span{File: file, Line: i + 1, Text: text})
		}
	}
	return spans
}

func codeSpans(line string) []string {
	var spans []string
	i := 0
	for i < len(line) {
		if line[i] != '`' {
			i++
			continue
		}
		open := run(line, i)
		closed := -1
		for j := i + open; j < len(line); {
			if line[j] != '`' {
				j++
				continue
			}
			if length := run(line, j); length == open {
				closed = j
				break
			} else {
				j += length
			}
		}
		if closed < 0 {
			i += open
			continue
		}
		spans = append(spans, strings.TrimSpace(line[i+open:closed]))
		i = closed + open
	}
	return spans
}

func run(line string, start int) int {
	length := 0
	for start+length < len(line) && line[start+length] == line[start] {
		length++
	}
	return length
}

func fenceMarker(line string) (byte, int, bool) {
	i := 0
	for i < 3 && i < len(line) && line[i] == ' ' {
		i++
	}
	if i >= len(line) {
		return 0, 0, false
	}
	char := line[i]
	if char != '`' && char != '~' {
		return 0, 0, false
	}
	length := run(line, i)
	if length < 3 {
		return 0, 0, false
	}
	return char, length, true
}

func closesFence(line string, char byte, length int) bool {
	start := strings.IndexByte(line, char)
	return strings.TrimSpace(line[start+length:]) == ""
}
