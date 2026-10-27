// Package lock reads and writes srcproof.lock, the record of which commit each
// repository link in an article was pinned to.
package lock

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Header is the first line of every lock file.
const Header = "srcproof-lock v1"

// MaxTabs is the widest tab expansion the format records.
const MaxTabs = 16

var (
	commitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)
	digestSHA = regexp.MustCompile(`^[0-9a-f]{64}$`)
	repoName  = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)
)

// Normalize is how text is squared up before a quote is compared.
type Normalize struct {
	TrailingSpace bool
	Tabs          int
}

// NewNormalize returns the normalization for a tab width. Zero leaves tabs as
// they are; trailing space is always ignored.
func NewNormalize(tabs int) (Normalize, error) {
	if tabs < 0 || tabs > MaxTabs {
		return Normalize{}, fmt.Errorf("tabs=%d is outside 1..%d", tabs, MaxTabs)
	}
	return Normalize{TrailingSpace: true, Tabs: tabs}, nil
}

func (n Normalize) line() string {
	fields := []string{"normalize"}
	if n.TrailingSpace {
		fields = append(fields, "trailing-space")
	}
	if n.Tabs > 0 {
		fields = append(fields, "tabs="+strconv.Itoa(n.Tabs))
	}
	if len(fields) == 1 {
		fields = append(fields, "none")
	}
	return strings.Join(fields, " ")
}

// Link records the commit a repository link was pinned to and the ref it named.
type Link struct {
	Article string
	Repo    string
	SHA     string
	Ref     string
}

// Key is what makes two link directives the same record.
func (l Link) Key() string { return l.Article + " " + l.Repo + " " + l.Ref }

func (l Link) line() string {
	return strings.Join([]string{"link", l.Article, l.Repo, l.SHA, l.Ref}, " ")
}

func (l Link) validate() error {
	if err := validPath("article", l.Article); err != nil {
		return err
	}
	if !repoName.MatchString(l.Repo) {
		return fmt.Errorf("%q does not name a repository as owner/name", l.Repo)
	}
	if !commitSHA.MatchString(l.SHA) {
		return fmt.Errorf("%q is not a 40-character lowercase commit", l.SHA)
	}
	return validRef(l.Ref)
}

// Quote records the digest of the code an article cites at a commit.
type Quote struct {
	Article string
	Repo    string
	SHA     string
	Path    string
	Digest  string
}

// Key is what makes two quote directives the same record.
func (q Quote) Key() string { return q.Article + " " + q.Repo + " " + q.Path }

func (q Quote) line() string {
	return strings.Join([]string{"quote", q.Article, q.Repo, q.SHA, q.Path, q.Digest}, " ")
}

func (q Quote) validate() error {
	if err := validPath("article", q.Article); err != nil {
		return err
	}
	if !repoName.MatchString(q.Repo) {
		return fmt.Errorf("%q does not name a repository as owner/name", q.Repo)
	}
	if !commitSHA.MatchString(q.SHA) {
		return fmt.Errorf("%q is not a 40-character lowercase commit", q.SHA)
	}
	if err := validPath("quoted path", q.Path); err != nil {
		return err
	}
	if !digestSHA.MatchString(q.Digest) {
		return fmt.Errorf("%q is not a 64-character lowercase digest", q.Digest)
	}
	return nil
}

// Allow records a word of an article that the gate is told not to report.
type Allow struct {
	Article string
	Word    string
}

// Key is what makes two allow directives the same record.
func (a Allow) Key() string { return a.Article + " " + a.Word }

func (a Allow) line() string {
	return strings.Join([]string{"allow", a.Article, a.Word}, " ")
}

func (a Allow) validate() error {
	if err := validPath("article", a.Article); err != nil {
		return err
	}
	if a.Word == "" || strings.ContainsAny(a.Word, " \t") {
		return fmt.Errorf("%q is not a single word", a.Word)
	}
	return nil
}

// File is one lock file.
type File struct {
	Normalize Normalize
	Links     []Link
	Quotes    []Quote
	Allows    []Allow
}

// Empty reports whether the file records nothing.
func (f File) Empty() bool { return len(f.Links)+len(f.Quotes)+len(f.Allows) == 0 }

// Format renders the file with its directives sorted. A directive the parser
// would refuse is refused here too, so Format(Parse(x)) returns x for every
// file Parse accepts.
func (f File) Format() (string, error) {
	if f.Normalize.Tabs < 0 || f.Normalize.Tabs > MaxTabs {
		return "", fmt.Errorf("tabs=%d is outside 1..%d", f.Normalize.Tabs, MaxTabs)
	}
	lines := make([]string, 0, len(f.Links)+len(f.Quotes)+len(f.Allows))
	keys := make(map[string]bool)
	add := func(key, line string, err error) error {
		if err != nil {
			return err
		}
		if keys[key] {
			return fmt.Errorf("%s is recorded twice", key)
		}
		keys[key] = true
		lines = append(lines, line)
		return nil
	}
	for _, l := range f.Links {
		if err := add("link "+l.Key(), l.line(), l.validate()); err != nil {
			return "", err
		}
	}
	for _, q := range f.Quotes {
		if err := add("quote "+q.Key(), q.line(), q.validate()); err != nil {
			return "", err
		}
	}
	for _, a := range f.Allows {
		if err := add("allow "+a.Key(), a.line(), a.validate()); err != nil {
			return "", err
		}
	}
	sort.Strings(lines)
	var out strings.Builder
	out.WriteString(Header + "\n")
	out.WriteString(f.Normalize.line() + "\n")
	for _, line := range lines {
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.String(), nil
}

// Parse reads a lock file. An unknown directive, a duplicate key, hex of the
// wrong length and lines out of order are all refused rather than repaired.
func Parse(text string) (File, error) {
	if text == "" || !strings.HasSuffix(text, "\n") {
		return File{}, errors.New("a lock file ends with a newline")
	}
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if len(lines) < 2 {
		return File{}, fmt.Errorf("a lock file starts with %q and a normalize line", Header)
	}
	if lines[0] != Header {
		return File{}, fmt.Errorf("line 1: %q is not the header %q", lines[0], Header)
	}
	normalize, err := parseNormalize(lines[1])
	if err != nil {
		return File{}, fmt.Errorf("line 2: %w", err)
	}
	file := &File{Normalize: normalize}
	keys := make(map[string]bool)
	previous := ""
	for i, line := range lines[2:] {
		number := i + 3
		switch {
		case line == previous:
			return File{}, fmt.Errorf("line %d: %q is written twice", number, line)
		case line < previous:
			return File{}, fmt.Errorf("line %d: %q sorts before %q on the line above", number, line, previous)
		}
		previous = line
		fields := strings.Split(line, " ")
		for _, field := range fields {
			if field == "" {
				return File{}, fmt.Errorf("line %d: fields are separated by one space", number)
			}
		}
		key, err := file.add(fields)
		if err != nil {
			return File{}, fmt.Errorf("line %d: %w", number, err)
		}
		if keys[key] {
			return File{}, fmt.Errorf("line %d: %s is recorded twice", number, key)
		}
		keys[key] = true
	}
	return *file, nil
}

func (f *File) add(fields []string) (string, error) {
	switch fields[0] {
	case "link":
		if len(fields) != 5 {
			return "", fmt.Errorf("link takes ARTICLE REPO SHA REF, not %d fields", len(fields)-1)
		}
		l := Link{Article: fields[1], Repo: fields[2], SHA: fields[3], Ref: fields[4]}
		if err := l.validate(); err != nil {
			return "", err
		}
		f.Links = append(f.Links, l)
		return "link " + l.Key(), nil
	case "quote":
		if len(fields) != 6 {
			return "", fmt.Errorf("quote takes ARTICLE REPO SHA PATH DIGEST, not %d fields", len(fields)-1)
		}
		q := Quote{Article: fields[1], Repo: fields[2], SHA: fields[3], Path: fields[4], Digest: fields[5]}
		if err := q.validate(); err != nil {
			return "", err
		}
		f.Quotes = append(f.Quotes, q)
		return "quote " + q.Key(), nil
	case "allow":
		if len(fields) != 3 {
			return "", fmt.Errorf("allow takes ARTICLE WORD, not %d fields", len(fields)-1)
		}
		a := Allow{Article: fields[1], Word: fields[2]}
		if err := a.validate(); err != nil {
			return "", err
		}
		f.Allows = append(f.Allows, a)
		return "allow " + a.Key(), nil
	}
	return "", fmt.Errorf("%q is not a directive of this format", fields[0])
}

// parseNormalize reads the normalize line and then compares it against the
// line it would write, so a normalization written in another order or twice is
// refused instead of being silently rewritten.
func parseNormalize(line string) (Normalize, error) {
	fields := strings.Split(line, " ")
	if fields[0] != "normalize" {
		return Normalize{}, fmt.Errorf("%q is not a normalize line", line)
	}
	var n Normalize
	none := false
	for _, field := range fields[1:] {
		switch {
		case field == "trailing-space":
			n.TrailingSpace = true
		case field == "none":
			none = true
		case strings.HasPrefix(field, "tabs="):
			width, err := strconv.Atoi(strings.TrimPrefix(field, "tabs="))
			if err != nil || width < 1 || width > MaxTabs {
				return Normalize{}, fmt.Errorf("%q is not tabs=1..%d", field, MaxTabs)
			}
			n.Tabs = width
		default:
			return Normalize{}, fmt.Errorf("%q is not a normalization", field)
		}
	}
	if none && (n.TrailingSpace || n.Tabs > 0) {
		return Normalize{}, errors.New("none is written on its own")
	}
	if got := n.line(); got != line {
		return Normalize{}, fmt.Errorf("the normalization is written %q, not %q", line, got)
	}
	return n, nil
}

func validPath(what, path string) error {
	switch {
	case path == "":
		return fmt.Errorf("the %s is empty", what)
	case strings.ContainsAny(path, " \t\\"):
		return fmt.Errorf("the %s %q holds a space or a backslash", what, path)
	case strings.HasPrefix(path, "/"):
		return fmt.Errorf("the %s %q is absolute; write it relative to the lock file", what, path)
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("the %s %q does not reach a file beside the lock file", what, path)
		}
	}
	return nil
}

func validRef(ref string) error {
	switch {
	case ref == "":
		return errors.New("the ref is empty")
	case strings.ContainsAny(ref, " \t\\"):
		return fmt.Errorf("the ref %q holds a space or a backslash", ref)
	case strings.HasPrefix(ref, "/"), strings.HasSuffix(ref, "/"), strings.Contains(ref, "//"), strings.Contains(ref, ".."):
		return fmt.Errorf("%q is not a ref name", ref)
	}
	return nil
}
