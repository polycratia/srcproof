package quote

import (
	"strings"
	"testing"

	"github.com/polycratia/srcproof/internal/gitrepo"
	"github.com/polycratia/srcproof/internal/lock"
)

const (
	article = "article.md"
	repo    = "polycratia/srcproof"
	quoted  = "internal/lock/lock.go"
	sha     = "0123456789abcdef0123456789abcdef01234567"
)

const blob = "package lock\n" +
	"\n" +
	"func (q Quote) Lines() int {\n" +
	"\treturn q.End - q.Start + 1\n" +
	"}\n"

const body = "```go\n" +
	"func (q Quote) Lines() int {\n" +
	"\treturn q.End - q.Start + 1\n" +
	"}\n" +
	"```\n"

func permalink(lines string) string {
	return "https://github.com/" + repo + "/blob/" + sha + "/" + quoted + "#" + lines
}

func before(lines string) string {
	return "A range covers [this much](" + permalink(lines) + "):\n\n" + body
}

func after(lines string) string {
	return body + "\nThat is [Lines](" + permalink(lines) + ").\n"
}

// clone answers like gitrepo.Repo.Blob, without a git binary.
type clone map[string]string

func (c clone) Blob(commit, path string) ([]byte, error) {
	text, found := c[commit+" "+path]
	if !found {
		return nil, &gitrepo.MissingHistoryError{Commit: commit, Dir: "/clone"}
	}
	return []byte(text), nil
}

func normalize(t *testing.T, tabs int) lock.Normalize {
	t.Helper()
	n, err := lock.NewNormalize(tabs)
	if err != nil {
		t.Fatalf("NewNormalize: %v", err)
	}
	return n
}

func block(t *testing.T, source string) Block {
	t.Helper()
	blocks := Blocks(article, source)
	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want 1", len(blocks))
	}
	return blocks[0]
}

func record(t *testing.T, source string, n lock.Normalize, start, end int) lock.Quote {
	t.Helper()
	return lock.Quote{
		Article: article,
		Digest:  Digest(block(t, source).Lines, n),
		Repo:    repo,
		SHA:     sha,
		Path:    quoted,
		Start:   start,
		End:     end,
	}
}

func request(t *testing.T, source string, n lock.Normalize, content string, records ...lock.Quote) Request {
	t.Helper()
	file := lock.File{Normalize: n, Quotes: records}
	if _, err := file.Format(); err != nil {
		t.Fatalf("Format: %v", err)
	}
	return Request{
		Article: article,
		Source:  source,
		Lock:    file,
		Clones:  map[string]Blobs{repo: clone{sha + " " + quoted: content}},
	}
}

func only(t *testing.T, req Request) Result {
	t.Helper()
	results, err := Check(req)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results %+v, want 1", len(results), results)
	}
	return results[0]
}

func TestAQuoteThatMatchesTheBlobPasses(t *testing.T) {
	n := normalize(t, 0)
	for _, source := range []string{before("L3-L5"), after("L3-L5")} {
		result := only(t, request(t, source, n, blob, record(t, source, n, 3, 5)))
		if result.Reason != ReasonMatch {
			t.Fatalf("reason %q: %s", result.Reason, result.Message)
		}
		if result.Severity() != Pass {
			t.Errorf("severity %q, want %q", result.Severity(), Pass)
		}
		anchor := result.Block.Anchor
		if anchor == nil || anchor.Start != 3 || anchor.End != 5 || anchor.Path != quoted {
			t.Fatalf("anchor %+v", anchor)
		}
		if len(result.Block.Lines) != 3 || result.Block.Info != "go" {
			t.Errorf("block %+v", result.Block)
		}
	}
}

func TestAOneByteChangeInTheBlobIsDrift(t *testing.T) {
	n := normalize(t, 0)
	source := before("L3-L5")
	moved := strings.Replace(blob, "+ 1", "+ 2", 1)
	result := only(t, request(t, source, n, moved, record(t, source, n, 3, 5)))
	if result.Reason != ReasonDrift {
		t.Fatalf("reason %q: %s", result.Reason, result.Message)
	}
	if result.Severity() != Fail {
		t.Errorf("severity %q, want %q", result.Severity(), Fail)
	}
	if !strings.Contains(result.Message, "line 4") {
		t.Errorf("message %q does not name the line that drifted", result.Message)
	}
}

func TestABlockEditedSincePinIsNotMatchedToARecord(t *testing.T) {
	n := normalize(t, 0)
	source := before("L3-L5")
	pinned := record(t, source, n, 3, 5)
	edited := strings.Replace(source, "+ 1", "+ 2", 1)
	result := only(t, request(t, edited, n, blob, pinned))
	if result.Reason != ReasonEdited {
		t.Fatalf("reason %q: %s", result.Reason, result.Message)
	}
	if result.Quote != (lock.Quote{}) {
		t.Errorf("an edited block was matched to %+v", result.Quote)
	}
}

func TestCRLFIsOnlyIgnoredWhenTheLockSaysTrailingSpace(t *testing.T) {
	source := before("L3-L5")
	crlf := strings.ReplaceAll(blob, "\n", "\r\n")

	trailing := normalize(t, 0)
	if !trailing.TrailingSpace {
		t.Fatalf("normalize %+v does not ignore trailing space", trailing)
	}
	result := only(t, request(t, source, trailing, crlf, record(t, source, trailing, 3, 5)))
	if result.Reason != ReasonMatch {
		t.Fatalf("under trailing-space: reason %q: %s", result.Reason, result.Message)
	}

	none := lock.Normalize{}
	result = only(t, request(t, source, none, crlf, record(t, source, none, 3, 5)))
	if result.Reason != ReasonDrift {
		t.Fatalf("under no normalization: reason %q: %s", result.Reason, result.Message)
	}
}

func TestAWidenedRangeFails(t *testing.T) {
	n := normalize(t, 0)
	source := before("L3-L6")

	wide := only(t, request(t, source, n, blob, record(t, source, n, 3, 6)))
	if wide.Reason != ReasonRange {
		t.Fatalf("a lock range of 4 lines for a block of 3: reason %q: %s", wide.Reason, wide.Message)
	}
	if !strings.Contains(wide.Message, "3-6") {
		t.Errorf("message %q does not name the range", wide.Message)
	}
	if wide.Severity() != Fail {
		t.Errorf("severity %q, want %q", wide.Severity(), Fail)
	}

	link := only(t, request(t, source, n, blob, record(t, source, n, 3, 5)))
	if link.Reason != ReasonAnchor {
		t.Fatalf("a permalink wider than the record: reason %q: %s", link.Reason, link.Message)
	}
	if !strings.Contains(link.Message, "3-6") || !strings.Contains(link.Message, "3-5") {
		t.Errorf("message %q does not name both ranges", link.Message)
	}
}

func TestAnUnanchoredBlockIsListedAndNotChecked(t *testing.T) {
	n := normalize(t, 0)
	source := "Some prose that names no permalink.\n\n" + body
	result := only(t, request(t, source, n, blob))
	if result.Reason != ReasonUnanchored {
		t.Fatalf("reason %q: %s", result.Reason, result.Message)
	}
	if result.Severity() != Listed {
		t.Errorf("severity %q, want %q", result.Severity(), Listed)
	}
	if result.Digest != "" || result.Quote != (lock.Quote{}) {
		t.Errorf("an unanchored block was claimed: %+v", result)
	}
}

func TestACommitTheCloneDoesNotHaveIsNotAPass(t *testing.T) {
	n := normalize(t, 0)
	source := before("L3-L5")
	req := request(t, source, n, blob, record(t, source, n, 3, 5))
	req.Clones = nil
	result := only(t, req)
	if result.Severity() != Undecided {
		t.Fatalf("severity %q: %s", result.Severity(), result.Message)
	}
}
