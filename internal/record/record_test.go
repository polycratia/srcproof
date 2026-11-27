package record

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/polycratia/srcproof/internal/gitrepo"
	"github.com/polycratia/srcproof/internal/lock"
	"github.com/polycratia/srcproof/internal/quote"
)

const (
	article   = "article.md"
	page      = "https://polycratia.com/articles/pinned-prose?print=1&lang=en"
	repo      = "polycratia/srcproof"
	sha       = "0123456789abcdef0123456789abcdef01234567"
	fixed     = "fedcba9876543210fedcba9876543210fedcba98"
	lockPath  = "internal/lock/lock.go"
	quotePath = "internal/quote/quote.go"
)

const lockBody = "func (q Quote) Lines() int {\n" +
	"\treturn q.End - q.Start + 1\n" +
	"}\n"

const quoteBody = "func blobLines(blob []byte) []string {\n" +
	"\treturn nil\n" +
	"}\n"

const lockBlob = "package lock\n\n" + lockBody

const quoteBlob = "package quote\n\n" + quoteBody

func permalinkOf(path string) string {
	return "https://github.com/" + repo + "/blob/" + sha + "/" + path + "#L3-L5"
}

func source() string {
	return "# Pinned prose\n\n" +
		"Lines 3 to 5 of [" + lockPath + "](" + permalinkOf(lockPath) + "):\n\n" +
		"```go\n" + lockBody + "```\n\n" +
		"Lines 3 to 5 of [" + quotePath + "](" + permalinkOf(quotePath) + "):\n\n" +
		"```go\n" + quoteBody + "```\n\n" +
		"A block with no permalink beside it is neither checked nor claimed:\n\n" +
		"```text\nnothing is pinned here\n```\n\n" +
		"The tree is at https://github.com/" + repo + "/tree/" + sha + " and the fix is in\n" +
		"https://github.com/" + repo + "/commit/" + fixed + ".\n"
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

// checked pins every anchored block of an article and then checks it, the way
// pin --write and check do, so a record is built from real quote results.
func checked(t *testing.T, text string) []quote.Result {
	t.Helper()
	n, err := lock.NewNormalize(0)
	if err != nil {
		t.Fatalf("NewNormalize: %v", err)
	}
	file := lock.File{Normalize: n}
	for _, block := range quote.Blocks(article, text) {
		if block.Anchor == nil {
			continue
		}
		anchor := *block.Anchor
		file.Quotes = append(file.Quotes, lock.Quote{
			Article: article,
			Digest:  quote.Digest(block.Lines, n),
			Repo:    anchor.Repo,
			SHA:     anchor.SHA,
			Path:    anchor.Path,
			Start:   anchor.Start,
			End:     anchor.End,
		})
	}
	if _, err := file.Format(); err != nil {
		t.Fatalf("Format: %v", err)
	}
	results, err := quote.Check(quote.Request{
		Article: article,
		Source:  text,
		Lock:    file,
		Clones: map[string]quote.Blobs{repo: clone{
			sha + " " + lockPath:  lockBlob,
			sha + " " + quotePath: quoteBlob,
		}},
	})
	if err != nil {
		t.Fatalf("quote.Check: %v", err)
	}
	for _, result := range results {
		switch result.Severity() {
		case quote.Fail, quote.Undecided:
			t.Fatalf("%s", result.String())
		}
	}
	return results
}

func build(t *testing.T, text string, results []quote.Result) Record {
	t.Helper()
	rec, err := Build(Request{Article: article, Source: text, URL: page, Quotes: results})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return rec
}

func TestTheRecordHoldsEveryCheckedClaim(t *testing.T) {
	text := source()
	rec := build(t, text, checked(t, text))

	var doc Document
	if err := json.Unmarshal([]byte(rec.Text), &doc); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, rec.Text)
	}
	if doc.Context != Context || doc.Type != "TechArticle" || doc.URL != page {
		t.Errorf("document %+v", doc)
	}
	if doc.Identifier.Type != "PropertyValue" || doc.Identifier.PropertyID != HashProperty || len(doc.Identifier.Value) != 64 {
		t.Errorf("identifier %+v", doc.Identifier)
	}
	if len(doc.IsBasedOn) != 2 {
		t.Fatalf("got %d sources %+v, want 2 (the unanchored block is not a claim)", len(doc.IsBasedOn), doc.IsBasedOn)
	}
	for i, url := range []string{permalinkOf(lockPath), permalinkOf(quotePath)} {
		got := doc.IsBasedOn[i]
		if got.Type != "SoftwareSourceCode" || got.URL != url || got.Version != sha ||
			got.CodeRepository != "https://github.com/"+repo {
			t.Errorf("source %d: %+v, want %q", i, got, url)
		}
		if got.Identifier.Type != "PropertyValue" || got.Identifier.PropertyID != HashProperty || len(got.Identifier.Value) != 64 {
			t.Errorf("source %d: identifier %+v", i, got.Identifier)
		}
	}
	if len(doc.Citation) != 2 {
		t.Fatalf("got %d citations %+v, want 2", len(doc.Citation), doc.Citation)
	}
	cited := []string{
		"https://github.com/" + repo + "/commit/" + fixed,
		"https://github.com/" + repo + "/tree/" + sha,
	}
	for i, url := range cited {
		if doc.Citation[i].Type != "WebPage" || doc.Citation[i].URL != url {
			t.Errorf("citation %d: %+v, want %q", i, doc.Citation[i], url)
		}
	}
}

func TestTheSameInputsWriteTheSameBytes(t *testing.T) {
	text := source()
	results := checked(t, text)
	golden := build(t, text, results).Text

	if again := build(t, text, results).Text; again != golden {
		t.Fatalf("two runs wrote different bytes:\n%s\n%s", golden, again)
	}
	reordered := make([]quote.Result, len(results))
	for i, result := range results {
		reordered[len(results)-1-i] = result
	}
	if shuffled := build(t, text, reordered).Text; shuffled != golden {
		t.Fatalf("reordered input wrote different bytes:\n%s\n%s", golden, shuffled)
	}

	const prefix = "{\n  \"@context\": \"https://schema.org\",\n  \"@type\": \"TechArticle\",\n"
	if !strings.HasPrefix(golden, prefix) {
		t.Errorf("the record is not indented with two spaces:\n%s", golden)
	}
	if !strings.HasSuffix(golden, "}\n") || strings.HasSuffix(golden, "\n\n") {
		t.Errorf("the record does not end with one newline:\n%s", golden)
	}
	if !strings.Contains(golden, "&lang=en") || strings.Contains(golden, `\u0026`) {
		t.Errorf("the record escapes HTML:\n%s", golden)
	}
	for _, word := range []string{"dateCreated", "dateModified", "datePublished"} {
		if strings.Contains(golden, word) {
			t.Errorf("the record carries %s", word)
		}
	}
}

func TestARecordWithoutClaimsOrAURLIsRefused(t *testing.T) {
	prose := "Prose that quotes nothing and pins nothing.\n"
	if _, err := Build(Request{Article: article, Source: prose, URL: page}); !errors.Is(err, ErrNoClaims) {
		t.Errorf("an article with no claim: err %v, want %v", err, ErrNoClaims)
	}
	text := source()
	if _, err := Build(Request{Article: article, Source: text, Quotes: checked(t, text)}); !errors.Is(err, ErrNoURL) {
		t.Errorf("a record without a URL: err %v, want %v", err, ErrNoURL)
	}
}
