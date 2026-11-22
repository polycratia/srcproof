package drift

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/polycratia/srcproof/internal/gitrepo"
	"github.com/polycratia/srcproof/internal/lock"
	"github.com/polycratia/srcproof/internal/quote"
)

const (
	article   = "article.md"
	repo      = "polycratia/srcproof"
	quoted    = "internal/lock/lock.go"
	pinnedSHA = "0123456789abcdef0123456789abcdef01234567"
	headSHA   = "ffeeddccbbaa99887766554433221100ffeeddcc"
)

const body = "func (q Quote) Lines() int {\n" +
	"\treturn q.End - q.Start + 1\n" +
	"}\n"

const pinnedBlob = "package lock\n\n" + body

var quotedLines = []string{
	"func (q Quote) Lines() int {",
	"\treturn q.End - q.Start + 1",
	"}",
}

// clone answers like gitrepo.Repo, without a git binary, and counts the blobs
// it was asked to read so the search bound can be seen.
type clone struct {
	trees   map[string]map[string]string
	refs    map[string]string
	renames map[string]string
	added   []string
	reads   int
}

func (c *clone) Commit(name string) (string, error) {
	if name == "" {
		name = "HEAD"
	}
	if sha, found := c.refs[name]; found {
		return sha, nil
	}
	if _, found := c.trees[name]; found {
		return name, nil
	}
	return "", fmt.Errorf("%q is not a branch or a tag: %w", name, gitrepo.ErrMissingRef)
}

func (c *clone) tree(commit string) (map[string]string, error) {
	tree, found := c.trees[commit]
	if !found {
		return nil, &gitrepo.MissingHistoryError{Commit: commit, Dir: "/clone"}
	}
	return tree, nil
}

func (c *clone) Blob(commit, path string) ([]byte, error) {
	tree, err := c.tree(commit)
	if err != nil {
		return nil, err
	}
	text, found := tree[path]
	if !found {
		return nil, fmt.Errorf("%s is not in commit %s: %w", path, commit, gitrepo.ErrMissingPath)
	}
	c.reads++
	return []byte(text), nil
}

func (c *clone) BlobID(commit, path string) (string, error) {
	tree, err := c.tree(commit)
	if err != nil {
		return "", err
	}
	text, found := tree[path]
	if !found {
		return "", fmt.Errorf("%s is not in commit %s: %w", path, commit, gitrepo.ErrMissingPath)
	}
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])[:40], nil
}

func (c *clone) Files(commit string) ([]string, error) {
	tree, err := c.tree(commit)
	if err != nil {
		return nil, err
	}
	var names []string
	for name := range tree {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func (c *clone) Renames(from, to string) (map[string]string, error) { return c.renames, nil }

func (c *clone) Added(from, to string) ([]string, error) { return c.added, nil }

func at(head map[string]string) *clone {
	return &clone{
		trees: map[string]map[string]string{
			pinnedSHA: {quoted: pinnedBlob},
			headSHA:   head,
		},
		refs: map[string]string{"HEAD": headSHA},
	}
}

func normalize(t *testing.T) lock.Normalize {
	t.Helper()
	n, err := lock.NewNormalize(0)
	if err != nil {
		t.Fatalf("NewNormalize: %v", err)
	}
	return n
}

func record(t *testing.T) lock.Quote {
	t.Helper()
	return lock.Quote{
		Article: article,
		Digest:  quote.Digest(quotedLines, normalize(t)),
		Repo:    repo,
		SHA:     pinnedSHA,
		Path:    quoted,
		Start:   3,
		End:     5,
	}
}

func request(t *testing.T, c *clone, records ...lock.Quote) Request {
	t.Helper()
	file := lock.File{Normalize: normalize(t), Quotes: records}
	if _, err := file.Format(); err != nil {
		t.Fatalf("Format: %v", err)
	}
	return Request{
		Article:  filepath.Join("docs", article),
		Lock:     file,
		LockPath: filepath.Join("docs", "srcproof.lock"),
		Against:  "HEAD",
		Clones:   map[string]Repo{repo: c},
	}
}

func only(t *testing.T, req Request) (Report, Result) {
	t.Helper()
	report, err := Check(req)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Results) != 1 {
		t.Fatalf("got %d results %+v, want 1", len(report.Results), report.Results)
	}
	return report, report.Results[0]
}

func TestTheSameBlobIsUnchanged(t *testing.T) {
	report, result := only(t, request(t, at(map[string]string{quoted: pinnedBlob}), record(t)))
	if result.Class != Unchanged {
		t.Fatalf("class %q: %s", result.Class, result.Message)
	}
	if report.ProposedText != "" {
		t.Errorf("an unchanged quote proposed a lock: %q", report.ProposedText)
	}
}

func TestTheSameRangeInAChangedFileIsUnchanged(t *testing.T) {
	head := pinnedBlob + "\nvar nothing = 1\n"
	_, result := only(t, request(t, at(map[string]string{quoted: head}), record(t)))
	if result.Class != Unchanged {
		t.Fatalf("class %q: %s", result.Class, result.Message)
	}
}

func TestShiftedLinesMoveAndProposeALockBesideTheOne(t *testing.T) {
	dir := t.TempDir()
	head := "// a header line added above the function\n\n" + pinnedBlob
	other := lock.Quote{
		Article: "other.md", Digest: strings.Repeat("ab", 32),
		Repo: repo, SHA: pinnedSHA, Path: "go.mod", Start: 1, End: 2,
	}
	req := request(t, at(map[string]string{quoted: head}), record(t), other)
	req.Article = filepath.Join(dir, article)
	req.LockPath = filepath.Join(dir, "srcproof.lock")
	before, err := req.Lock.Format()
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	if err := os.WriteFile(req.LockPath, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}
	report, result := only(t, req)
	if result.Class != Moved {
		t.Fatalf("class %q: %s", result.Class, result.Message)
	}
	if result.Proposed.Start != 5 || result.Proposed.End != 7 || result.Proposed.Path != quoted {
		t.Errorf("proposed %+v", result.Proposed)
	}
	if result.Proposed.SHA != headSHA || result.Proposed.Digest != record(t).Digest {
		t.Errorf("proposed %+v", result.Proposed)
	}
	if err := report.Write(); err != nil {
		t.Fatalf("Write: %v", err)
	}
	kept, err := os.ReadFile(req.LockPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(kept) != before {
		t.Errorf("the lock was rewritten: %q", kept)
	}
	proposed, err := os.ReadFile(req.LockPath + ProposedSuffix)
	if err != nil {
		t.Fatalf("the proposed lock was not written: %v", err)
	}
	if string(proposed) != report.ProposedText {
		t.Errorf("the written proposal %q is not the planned %q", proposed, report.ProposedText)
	}
	want := "quote " + article + " " + record(t).Digest + " " + repo + " " + headSHA + " " + quoted + " 5-7\n"
	if !strings.Contains(report.ProposedText, want) {
		t.Errorf("the proposal %q does not hold %q", report.ProposedText, want)
	}
	if !strings.Contains(report.ProposedText, "quote other.md ") {
		t.Errorf("the quote of another article was dropped: %q", report.ProposedText)
	}
	if _, err := lock.Parse(report.ProposedText); err != nil {
		t.Errorf("Parse of the proposal: %v", err)
	}
}

func TestTheRenameTargetIsMoved(t *testing.T) {
	moved := "internal/lock/record.go"
	c := at(map[string]string{moved: pinnedBlob})
	c.renames = map[string]string{quoted: moved}
	_, result := only(t, request(t, c, record(t)))
	if result.Class != Moved {
		t.Fatalf("class %q: %s", result.Class, result.Message)
	}
	if result.Proposed.Path != moved || result.Proposed.Start != 3 || result.Proposed.End != 5 {
		t.Errorf("proposed %+v", result.Proposed)
	}
}

func TestAFileAddedBetweenTheCommitsIsMoved(t *testing.T) {
	added := "internal/lock/quote.go"
	c := at(map[string]string{"go.mod": "module x\n", added: "package lock\n\n" + body})
	c.added = []string{added}
	_, result := only(t, request(t, c, record(t)))
	if result.Class != Moved {
		t.Fatalf("class %q: %s", result.Class, result.Message)
	}
	if result.Proposed.Path != added || result.Proposed.Start != 3 {
		t.Errorf("proposed %+v", result.Proposed)
	}
}

func TestBytesGoneFromAPathThatStaysAreChanged(t *testing.T) {
	head := strings.Replace(pinnedBlob, "+ 1", "+ 2", 1)
	_, result := only(t, request(t, at(map[string]string{quoted: head}), record(t)))
	if result.Class != Changed {
		t.Fatalf("class %q: %s", result.Class, result.Message)
	}
	if !strings.Contains(result.Message, "nowhere") {
		t.Errorf("message %q does not say the bytes are nowhere", result.Message)
	}
}

func TestADoubleMatchIsChangedAndNotMoved(t *testing.T) {
	head := "// one\n" + body + "\n" + body
	_, result := only(t, request(t, at(map[string]string{quoted: head}), record(t)))
	if result.Class != Changed {
		t.Fatalf("class %q: %s", result.Class, result.Message)
	}
	if result.Proposed != (lock.Quote{}) {
		t.Errorf("an ambiguous match was proposed: %+v", result.Proposed)
	}
	if !strings.Contains(result.Message, "2 places") {
		t.Errorf("message %q does not say the bytes are at two places", result.Message)
	}
}

func TestAGonePathWithNoMatchIsDeleted(t *testing.T) {
	_, result := only(t, request(t, at(map[string]string{"go.mod": "module x\n"}), record(t)))
	if result.Class != Deleted {
		t.Fatalf("class %q: %s", result.Class, result.Message)
	}
}

func TestTheSearchStopsAtTheBound(t *testing.T) {
	head := map[string]string{"go.mod": "module x\n"}
	var added []string
	for i := 0; i < MaxBlobs+50; i++ {
		path := fmt.Sprintf("internal/lock/part%03d.go", i)
		head[path] = "package lock\n\nvar filler = 1\n"
		added = append(added, path)
	}
	c := at(head)
	c.added = added
	_, result := only(t, request(t, c, record(t)))
	if result.Class != Deleted {
		t.Fatalf("class %q: %s", result.Class, result.Message)
	}
	if !result.Bounded || !strings.Contains(result.Message, "bound") {
		t.Errorf("the bound was not reported: %+v", result)
	}
	if c.reads != MaxBlobs {
		t.Errorf("read %d blobs, want %d", c.reads, MaxBlobs)
	}
}

func TestAMissingHistoryIsUndecided(t *testing.T) {
	c := at(map[string]string{quoted: pinnedBlob})
	c.refs["HEAD"] = strings.Repeat("a", 40)
	_, result := only(t, request(t, c, record(t)))
	if result.Class != Undecided {
		t.Fatalf("class %q: %s", result.Class, result.Message)
	}
}

func TestAnIdentifierGoneAtTheHeadIsListed(t *testing.T) {
	pinnedCode := "package lock\n\ntype Quote struct{}\n\nfunc (q Quote) Lines() int { return 1 }\n"
	headCode := "package lock\n\ntype Quote struct{}\n\nfunc (q Quote) Count() int { return 1 }\n"
	c := &clone{
		trees: map[string]map[string]string{
			pinnedSHA: {quoted: pinnedCode},
			headSHA:   {quoted: headCode},
		},
		refs: map[string]string{"HEAD": headSHA},
	}
	req := request(t, c)
	req.Source = "`Quote.Lines` is at https://github.com/" + repo + "/blob/" + pinnedSHA + "/" + quoted + "\n"
	report, err := Check(req)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Gone) != 1 {
		t.Fatalf("got %d gone name(s) %+v, want 1", len(report.Gone), report.Gone)
	}
	if report.Gone[0].Span.Text != "Quote.Lines" {
		t.Errorf("gone %+v", report.Gone[0])
	}
	if !strings.Contains(report.Gone[0].String(), headSHA) {
		t.Errorf("message %q does not name the commit it is missing from", report.Gone[0])
	}
}
