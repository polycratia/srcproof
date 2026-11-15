package ident

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/polycratia/srcproof/internal/gitrepo"
	"github.com/polycratia/srcproof/internal/lock"
)

const (
	repo   = "polycratia/srcproof"
	whole  = "0123456789abcdef0123456789abcdef01234567"
	older  = "00112233445566778899aabbccddeeff00112233"
	broken = "ffeeddccbbaa99887766554433221100ffeeddcc"
)

const goMod = "module github.com/polycratia/srcproof\n\ngo 1.24\n"

const lockGo = `package lock

import (
	"strings"

	"github.com/polycratia/srcproof/internal/links"
)

const Header = "srcproof-lock v1"

type Normalize struct {
	TrailingSpace bool
	Tabs          int
}

type Stack[T any] struct {
	items []T
	Depth int
}

func (s *Stack[T]) Push(item T) { s.items = append(s.items, item) }

func (s Stack[T]) Len() int { return len(s.items) }

func NewNormalize(tabs int) Normalize { return Normalize{Tabs: tabs} }

func Resolved(ref string) string { return strings.ToLower(ref) }

var Kinds = []links.Kind{}
`

var lockGoOlder = strings.Replace(lockGo, "func Resolved", "func Resolve", 1)

const brokenGo = "package ident\n\nfunc Broken( {\n"

// tree answers like gitrepo.Repo, without a git binary.
type tree map[string]map[string]string

func (t tree) put(commit, name, content string) {
	if t[commit] == nil {
		t[commit] = make(map[string]string)
	}
	t[commit][name] = content
}

func (t tree) Files(commit string) ([]string, error) {
	files, found := t[commit]
	if !found {
		return nil, &gitrepo.MissingHistoryError{Commit: commit, Dir: "/clone"}
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func (t tree) Blob(commit, name string) ([]byte, error) {
	files, found := t[commit]
	if !found {
		return nil, &gitrepo.MissingHistoryError{Commit: commit, Dir: "/clone"}
	}
	content, found := files[name]
	if !found {
		return nil, fmt.Errorf("%s is not in commit %s: %w", name, commit, gitrepo.ErrMissingPath)
	}
	return []byte(content), nil
}

func clone() tree {
	files := tree{}
	for _, commit := range []string{whole, older, broken} {
		files.put(commit, "go.mod", goMod)
		files.put(commit, "README.md", "# srcproof\n")
		files.put(commit, "vendor/example.com/dep/dep.go", "package dep\n\nfunc Vendored() {}\n")
		files.put(commit, "internal/lock/testdata/sample.go", "package testdata\n\nfunc Fixture() {}\n")
	}
	files.put(whole, "internal/lock/lock.go", lockGo)
	files.put(broken, "internal/lock/lock.go", lockGo)
	files.put(broken, "internal/ident/broken.go", brokenGo)
	files.put(older, "internal/lock/lock.go", lockGoOlder)
	return files
}

// article pins one link per commit and then writes the prose under them.
func article(commits []string, body string) string {
	var out strings.Builder
	for _, commit := range commits {
		out.WriteString("See [lock](https://github.com/" + repo + "/blob/" + commit + "/internal/lock/lock.go).\n\n")
	}
	out.WriteString(body)
	return out.String()
}

func decided(t *testing.T, source string, allows ...lock.Allow) []Result {
	t.Helper()
	results, err := Check(Request{
		Article: "article.md",
		Source:  source,
		Lock:    lock.File{Normalize: lock.Normalize{TrailingSpace: true}, Allows: allows},
		Trees:   map[string]Tree{repo: clone()},
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return results
}

func find(t *testing.T, results []Result, name string) Result {
	t.Helper()
	for _, result := range results {
		if result.Name == name {
			return result
		}
	}
	t.Fatalf("no result for %q in %+v", name, results)
	return Result{}
}

func offers(nearest []string, name string) bool {
	for _, candidate := range nearest {
		if candidate == name {
			return true
		}
	}
	return false
}

func TestNamesAtThePinnedCommitPass(t *testing.T) {
	source := article([]string{whole}, "The `Stack.Push` method, the `Stack.Depth` field, `NewNormalize()`, "+
		"`Header`, `Normalize` and `lock.Stack.Len()` are all there; `len()` is predeclared.\n")
	results := decided(t, source)
	if len(results) != 7 {
		t.Fatalf("got %d results %+v, want 7", len(results), results)
	}
	for _, result := range results {
		if result.Severity() != Pass {
			t.Errorf("%s: %s", result.Name, result.Message)
		}
	}
	if find(t, results, "len").Reason != ReasonPredeclared {
		t.Errorf("len was not read as predeclared")
	}
}

func TestARenamedFunctionIsReportedWithTheNearestNames(t *testing.T) {
	source := article([]string{whole}, "Call `Resolve()` to pin a ref.\n")
	result := find(t, decided(t, source), "Resolve")
	if result.Reason != ReasonUnknown || result.Severity() != Fail {
		t.Fatalf("reason %q, severity %q: %s", result.Reason, result.Severity(), result.Message)
	}
	if !offers(result.Nearest, "Resolved") {
		t.Errorf("nearest %v does not offer Resolved", result.Nearest)
	}
	if len(result.Nearest) > 3 {
		t.Errorf("nearest %v is more than three names", result.Nearest)
	}
	if !strings.Contains(result.Message, "Resolved") {
		t.Errorf("message %q does not offer a nearest name", result.Message)
	}
	if !strings.HasPrefix(result.String(), "article.md:3: ") {
		t.Errorf("%q does not start at article.md:3", result.String())
	}
}

func TestTheUnionOfThePinnedCommitsIsSearched(t *testing.T) {
	source := article([]string{whole, older}, "Both `Resolve()` and `Resolved()` existed.\n")
	results := decided(t, source)
	for _, name := range []string{"Resolve", "Resolved"} {
		if result := find(t, results, name); result.Severity() != Pass {
			t.Errorf("%s: %s", name, result.Message)
		}
	}
}

func TestAnAllowedWordPasses(t *testing.T) {
	source := article([]string{whole}, "The `ctsafe_mask()` helper lives elsewhere.\n")
	if result := find(t, decided(t, source), "ctsafe_mask"); result.Severity() != Fail {
		t.Fatalf("severity %q without an allow line: %s", result.Severity(), result.Message)
	}
	allowed := decided(t, source, lock.Allow{Article: "article.md", Word: "ctsafe_mask"})
	if result := find(t, allowed, "ctsafe_mask"); result.Reason != ReasonAllowed {
		t.Fatalf("reason %q: %s", result.Reason, result.Message)
	}
}

func TestAnExternalQualifierIsNotChecked(t *testing.T) {
	source := article([]string{whole}, "It calls `strings.ToLower` and `strings.Nonesuch`.\n")
	results := decided(t, source)
	for _, name := range []string{"strings.ToLower", "strings.Nonesuch"} {
		result := find(t, results, name)
		if result.Reason != ReasonExternal || result.Severity() != Listed {
			t.Fatalf("%s: reason %q, severity %q", name, result.Reason, result.Severity())
		}
		if !strings.Contains(result.Message, "external, not checked") {
			t.Errorf("message %q does not say external, not checked", result.Message)
		}
	}
	inside := find(t, decided(t, article([]string{whole}, "The `lock.Nonesuch` line.\n")), "lock.Nonesuch")
	if inside.Reason != ReasonUnknown {
		t.Errorf("a qualifier of this repository was not checked: %s", inside.Message)
	}
}

func TestAMissingPathIsReported(t *testing.T) {
	source := article([]string{whole}, "Read `internal/lock/lock.go`, `internal/lock`, `go.mod` and `internal/lock/missing.go`.\n")
	results := decided(t, source)
	for _, name := range []string{"internal/lock/lock.go", "internal/lock", "go.mod"} {
		result := find(t, results, name)
		if result.Kind != KindPath || result.Severity() != Pass {
			t.Errorf("%s: kind %q, %s", name, result.Kind, result.Message)
		}
	}
	result := find(t, results, "internal/lock/missing.go")
	if result.Reason != ReasonMissingPath || result.Severity() != Fail {
		t.Fatalf("reason %q, severity %q: %s", result.Reason, result.Severity(), result.Message)
	}
	if !offers(result.Nearest, "internal/lock/lock.go") {
		t.Errorf("nearest %v does not offer internal/lock/lock.go", result.Nearest)
	}
}

func TestAFileThatDoesNotParseLeavesAnUnknownNameUndecided(t *testing.T) {
	result := find(t, decided(t, article([]string{broken}, "Call `Resolve()` here.\n")), "Resolve")
	if result.Reason != ReasonUndecided || result.Severity() != Undecided {
		t.Fatalf("reason %q, severity %q: %s", result.Reason, result.Severity(), result.Message)
	}
	if !strings.Contains(result.Message, "internal/ident/broken.go") {
		t.Errorf("message %q does not name the file that did not parse", result.Message)
	}
	found := find(t, decided(t, article([]string{broken}, "Call `Resolved()` here.\n")), "Resolved")
	if found.Severity() != Pass {
		t.Errorf("a name that is there was not found: %s", found.Message)
	}
}

func TestVendorAndTestdataAreNotRead(t *testing.T) {
	source := article([]string{whole}, "Neither `Vendored()` nor `Fixture()` is of this code.\n")
	results := decided(t, source)
	for _, name := range []string{"Vendored", "Fixture"} {
		if result := find(t, results, name); result.Severity() != Fail {
			t.Errorf("%s: severity %q: %s", name, result.Severity(), result.Message)
		}
	}
}

func TestSpansInsideAFenceAreNotChecked(t *testing.T) {
	source := article([]string{whole}, "```go\n`Nonesuch()`\n```\n")
	if results := decided(t, source); len(results) != 0 {
		t.Fatalf("got %d results %+v, want none", len(results), results)
	}
}

func TestWithoutACloneNothingIsChecked(t *testing.T) {
	results, err := Check(Request{
		Article: "article.md",
		Source:  article([]string{whole}, "Call `Nonesuch()`.\n"),
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("got %d results %+v, want none", len(results), results)
	}
}
