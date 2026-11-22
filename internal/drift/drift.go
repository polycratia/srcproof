// Package drift compares the quotes an article pinned against another commit
// and tells a block whose bytes moved from a block whose bytes changed. The
// lock it reads is never written: a moved quote is proposed beside it.
package drift

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/polycratia/srcproof/internal/gitrepo"
	"github.com/polycratia/srcproof/internal/ident"
	"github.com/polycratia/srcproof/internal/lock"
	"github.com/polycratia/srcproof/internal/quote"
)

// MaxBlobs and MaxBytes bound the search for bytes that moved, so a quote
// whose bytes are nowhere does not read a whole repository to say so.
const (
	MaxBlobs = 200
	MaxBytes = 8 << 20
)

// ProposedSuffix is added to the lock path for the file a moved quote writes.
const ProposedSuffix = ".proposed"

// Class is what was decided about one recorded quote.
type Class string

const (
	Unchanged Class = "unchanged"
	Moved     Class = "moved"
	Changed   Class = "changed"
	Deleted   Class = "deleted"
	Undecided Class = "undecided"
)

// Repo is the clone a quote is read in, the way gitrepo.Repo reads one.
type Repo interface {
	Commit(name string) (string, error)
	Blob(commit, path string) ([]byte, error)
	BlobID(commit, path string) (string, error)
	Files(commit string) ([]string, error)
	Renames(from, to string) (map[string]string, error)
	Added(from, to string) ([]string, error)
}

// Result is what was decided about one recorded quote. Proposed is the quote
// line a moved block would be recorded under.
type Result struct {
	Quote    lock.Quote
	Against  string
	Class    Class
	Proposed lock.Quote
	Bounded  bool
	Message  string
}

// String renders a result as article: message.
func (r Result) String() string {
	return fmt.Sprintf("%s: %s", r.Quote.Article, r.Message)
}

// Name is a code span that names something at the pinned commit and nothing at
// the commit the comparison is made against.
type Name struct {
	Span ident.Span
	Repo string
	From string
	To   string
}

// String renders a name as file:line: message.
func (n Name) String() string {
	return fmt.Sprintf("%s:%d: the code span %q is in %s at commit %s and names nothing at commit %s",
		n.Span.File, n.Span.Line, n.Span.Text, n.Repo, n.From, n.To)
}

// Request is one article to compare, with the clones its quotes are read in.
// Against is the ref the pinned quotes are compared with; empty means HEAD.
type Request struct {
	Article  string
	Source   string
	Lock     lock.File
	LockPath string
	Against  string
	Clones   map[string]Repo
}

// Report is what the comparison found.
type Report struct {
	Results      []Result
	Gone         []Name
	Proposed     lock.File
	ProposedText string
	ProposedPath string
}

// Count is how many results fall in one class.
func (r Report) Count(class Class) int {
	found := 0
	for _, result := range r.Results {
		if result.Class == class {
			found++
		}
	}
	return found
}

// Write puts the proposed lock beside the article. The lock itself is not
// opened here, let alone written: what moved is offered, never applied.
func (r Report) Write() error {
	if r.ProposedText == "" {
		return nil
	}
	return os.WriteFile(r.ProposedPath, []byte(r.ProposedText), 0o644)
}

// Check decides every quote the lock records for one article. Bytes found
// exactly once somewhere else moved and are proposed at their new place; bytes
// that are missing, or found more than once, changed; a path that is gone with
// no match anywhere was deleted.
func Check(req Request) (Report, error) {
	checker := &checker{
		req:      req,
		heads:    make(map[string]string),
		renamed:  make(map[string]map[string]string),
		appeared: make(map[string][]string),
	}
	name := filepath.Base(req.Article)
	var records []lock.Quote
	for _, record := range req.Lock.Quotes {
		if record.Article == name {
			records = append(records, record)
		}
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].Path != records[j].Path {
			return records[i].Path < records[j].Path
		}
		if records[i].Start != records[j].Start {
			return records[i].Start < records[j].Start
		}
		return records[i].Digest < records[j].Digest
	})
	report := Report{ProposedPath: req.LockPath + ProposedSuffix}
	moved := make(map[string]lock.Quote)
	for _, record := range records {
		result, err := checker.decide(record)
		if err != nil {
			return Report{}, err
		}
		report.Results = append(report.Results, result)
		if result.Class == Moved {
			moved[record.Key()] = result.Proposed
		}
	}
	gone, err := checker.names()
	if err != nil {
		return Report{}, err
	}
	report.Gone = gone
	if len(moved) == 0 {
		return report, nil
	}
	proposed := req.Lock
	proposed.Quotes = make([]lock.Quote, len(req.Lock.Quotes))
	copy(proposed.Quotes, req.Lock.Quotes)
	for i, record := range proposed.Quotes {
		if replacement, ok := moved[record.Key()]; ok {
			proposed.Quotes[i] = replacement
		}
	}
	text, err := proposed.Format()
	if err != nil {
		return Report{}, fmt.Errorf("%s: %w", report.ProposedPath, err)
	}
	report.Proposed, report.ProposedText = proposed, text
	return report, nil
}

// match is one place the bytes of a quote were found.
type match struct {
	Path  string
	Start int
}

type checker struct {
	req      Request
	heads    map[string]string
	renamed  map[string]map[string]string
	appeared map[string][]string
}

func (c *checker) head(key string, clone Repo) (string, error) {
	if head, found := c.heads[key]; found {
		return head, nil
	}
	head, err := clone.Commit(c.req.Against)
	if err != nil {
		return "", err
	}
	c.heads[key] = head
	return head, nil
}

func (c *checker) renames(key string, clone Repo, from, to string) (map[string]string, error) {
	cached := key + " " + from + " " + to
	if renames, found := c.renamed[cached]; found {
		return renames, nil
	}
	renames, err := clone.Renames(from, to)
	if err != nil {
		return nil, err
	}
	c.renamed[cached] = renames
	return renames, nil
}

func (c *checker) added(key string, clone Repo, from, to string) ([]string, error) {
	cached := key + " " + from + " " + to
	if added, found := c.appeared[cached]; found {
		return added, nil
	}
	added, err := clone.Added(from, to)
	if err != nil {
		return nil, err
	}
	c.appeared[cached] = added
	return added, nil
}

func (c *checker) decide(record lock.Quote) (Result, error) {
	result := Result{Quote: record, Class: Undecided}
	key := strings.ToLower(record.Repo)
	clone, named := c.req.Clones[key]
	if !named {
		result.Message = fmt.Sprintf("cannot decide: no clone was named for %s; name it as --repo %s=PATH", record.Repo, record.Repo)
		return result, nil
	}
	head, err := c.head(key, clone)
	if err != nil {
		return undecide(result, err)
	}
	result.Against = head
	pinnedID, err := clone.BlobID(record.SHA, record.Path)
	if err != nil {
		// The lock says these bytes were there; a pinned commit that does not
		// hold them leaves nothing to compare against.
		if missing(err) {
			result.Message = fmt.Sprintf("cannot decide: %v; the lock records a quote of a path the pinned commit does not hold", err)
			return result, nil
		}
		return undecide(result, err)
	}
	present := true
	headID, err := clone.BlobID(head, record.Path)
	switch {
	case missing(err):
		present = false
	case err != nil:
		return undecide(result, err)
	}
	if present && headID == pinnedID {
		result.Class = Unchanged
		result.Message = fmt.Sprintf("lines %s of %s are unchanged at commit %s: the blob there is still %s",
			record.Range(), record.Path, head, pinnedID)
		return result, nil
	}
	left := &budget{blobs: MaxBlobs, bytes: MaxBytes}
	searched := make(map[string]bool)
	if present {
		searched[record.Path] = true
		lines, read, err := left.read(clone, head, record.Path)
		if err != nil {
			return undecide(result, err)
		}
		if read && record.End <= len(lines) && c.digest(lines[record.Start-1:record.End]) == record.Digest {
			result.Class = Unchanged
			result.Message = fmt.Sprintf("lines %s of %s still read the same at commit %s, though the blob around them changed",
				record.Range(), record.Path, head)
			return result, nil
		}
		if found := c.search(lines, record, record.Path); len(found) > 0 {
			return c.verdict(result, record, head, "the same file", found, left), nil
		}
	}
	renames, err := c.renames(key, clone, record.SHA, head)
	if err != nil {
		return undecide(result, err)
	}
	if target := renames[record.Path]; target != "" && !searched[target] {
		searched[target] = true
		lines, _, err := left.read(clone, head, target)
		if err != nil {
			return undecide(result, err)
		}
		if found := c.search(lines, record, target); len(found) > 0 {
			return c.verdict(result, record, head, "the rename target", found, left), nil
		}
	}
	added, err := c.added(key, clone, record.SHA, head)
	if err != nil {
		return undecide(result, err)
	}
	var found []match
	for _, path := range added {
		if searched[path] {
			continue
		}
		searched[path] = true
		lines, _, err := left.read(clone, head, path)
		if err != nil {
			return undecide(result, err)
		}
		if left.spent {
			break
		}
		found = append(found, c.search(lines, record, path)...)
		if len(found) > 1 {
			break
		}
	}
	if len(found) > 0 {
		return c.verdict(result, record, head, "a file added between the commits", found, left), nil
	}
	result.Bounded = left.spent
	nowhere := "their bytes are nowhere the search looked" + bound(left)
	if present {
		result.Class = Changed
		result.Message = fmt.Sprintf("lines %s of %s changed at commit %s: %s", record.Range(), record.Path, head, nowhere)
		return result, nil
	}
	result.Class = Deleted
	result.Message = fmt.Sprintf("%s is not in commit %s and %s", record.Path, head, nowhere)
	return result, nil
}

// verdict reads one step of the search: bytes in exactly one place moved, and
// bytes in several places name no one range, so the quote changed.
func (c *checker) verdict(result Result, record lock.Quote, head, where string, found []match, left *budget) Result {
	result.Bounded = left.spent
	if len(found) == 1 {
		proposed := record
		proposed.SHA = head
		proposed.Path = found[0].Path
		proposed.Start = found[0].Start
		proposed.End = found[0].Start + record.Lines() - 1
		result.Class = Moved
		result.Proposed = proposed
		result.Message = fmt.Sprintf("the bytes of lines %s of %s sit at lines %s of %s at commit %s (%s); the quote moved",
			record.Range(), record.Path, proposed.Range(), proposed.Path, head, where)
		return result
	}
	result.Class = Changed
	result.Message = fmt.Sprintf("the bytes of lines %s of %s are at %d places at commit %s (%s): %s; no one range is the quote",
		record.Range(), record.Path, len(found), head, where, places(found, record.Lines()))
	return result
}

func (c *checker) digest(lines []string) string {
	return quote.Digest(lines, c.req.Lock.Normalize)
}

func (c *checker) search(lines []string, record lock.Quote, path string) []match {
	span := record.Lines()
	var found []match
	for i := 0; i+span <= len(lines); i++ {
		if c.digest(lines[i:i+span]) == record.Digest {
			found = append(found, match{Path: path, Start: i + 1})
		}
	}
	return found
}

// names lists the code spans of the article that are in the code at a commit
// it pins and nowhere at the commit the comparison is made against.
func (c *checker) names() ([]Name, error) {
	spans := ident.Spans(c.req.Article, c.req.Source)
	if len(spans) == 0 {
		return nil, nil
	}
	var gone []Name
	seen := make(map[string]bool)
	for _, commit := range ident.Pinned(c.req.Article, c.req.Source) {
		clone, named := c.req.Clones[commit.Repo]
		if !named {
			continue
		}
		head, err := c.head(commit.Repo, clone)
		switch {
		case undecidable(err):
			continue
		case err != nil:
			return nil, err
		}
		if head == commit.SHA {
			continue
		}
		from, err := ident.Build(commit.Repo, commit.SHA, clone)
		switch {
		case undecidable(err):
			continue
		case err != nil:
			return nil, err
		}
		to, err := ident.Build(commit.Repo, head, clone)
		switch {
		case undecidable(err):
			continue
		case err != nil:
			return nil, err
		}
		for _, span := range spans {
			name := strings.TrimSuffix(span.Text, "()")
			path := strings.TrimSuffix(span.Text, "/")
			switch {
			case from.Names[name] && !to.Names[name]:
			case from.Paths[path] && !to.Paths[path]:
			default:
				continue
			}
			key := commit.Repo + " " + span.Text
			if seen[key] {
				continue
			}
			seen[key] = true
			gone = append(gone, Name{Span: span, Repo: commit.Repo, From: commit.SHA, To: head})
		}
	}
	sort.Slice(gone, func(i, j int) bool {
		if gone[i].Span.Line != gone[j].Span.Line {
			return gone[i].Span.Line < gone[j].Span.Line
		}
		return gone[i].Span.Text < gone[j].Span.Text
	})
	return gone, nil
}

// budget is what is left of the search bound. A read past it is not made, and
// the result says the search stopped rather than that the bytes are gone.
type budget struct {
	blobs int
	bytes int
	spent bool
}

func (b *budget) read(clone Repo, commit, path string) ([]string, bool, error) {
	if b.blobs <= 0 || b.bytes <= 0 {
		b.spent = true
		return nil, false, nil
	}
	blob, err := clone.Blob(commit, path)
	switch {
	case missing(err):
		return nil, false, nil
	case err != nil:
		return nil, false, err
	}
	b.blobs--
	b.bytes -= len(blob)
	return blobLines(blob), true, nil
}

func bound(left *budget) string {
	if !left.spent {
		return ""
	}
	return fmt.Sprintf("; the search stopped at the bound of %d blobs or %d MiB", MaxBlobs, MaxBytes>>20)
}

func places(found []match, span int) string {
	parts := make([]string, 0, len(found))
	for _, at := range found {
		parts = append(parts, fmt.Sprintf("%s %d-%d", at.Path, at.Start, at.Start+span-1))
	}
	return strings.Join(parts, ", ")
}

func undecide(result Result, err error) (Result, error) {
	if !undecidable(err) {
		return Result{}, err
	}
	result.Class = Undecided
	result.Message = fmt.Sprintf("cannot decide: %v", err)
	return result, nil
}

func undecidable(err error) bool {
	return err != nil && (errors.Is(err, gitrepo.ErrMissingHistory) ||
		errors.Is(err, gitrepo.ErrMissingRef) ||
		errors.Is(err, gitrepo.ErrAmbiguousRef) ||
		errors.Is(err, gitrepo.ErrRefName) ||
		errors.Is(err, gitrepo.ErrObjectName))
}

func missing(err error) bool {
	return errors.Is(err, gitrepo.ErrMissingPath) || errors.Is(err, gitrepo.ErrNotQuotable)
}

func blobLines(blob []byte) []string {
	text := string(blob)
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}
