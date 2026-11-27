// Package record writes the JSON-LD record of an article: a schema.org
// TechArticle that names the article by the digest of its bytes and names the
// code every checked quote pins. The same inputs write the same bytes, so a
// published record can be compared with a freshly built one.
package record

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"

	"github.com/polycratia/srcproof/internal/links"
	"github.com/polycratia/srcproof/internal/lock"
	"github.com/polycratia/srcproof/internal/quote"
)

// Suffix is added to the article path for the file a record is written to.
const Suffix = ".srcproof.jsonld"

// Context is the vocabulary the document is read under and Indent is the one
// indentation it is written with.
const (
	Context = "https://schema.org"
	Indent  = "  "
)

// HashProperty is the property id every digest in the record rides under.
// schema.org puts sha256 on MediaObject, so the digest of an article and the
// digest of a quote are written as a PropertyValue instead of as a property of
// the thing they name.
const HashProperty = "sha256"

const (
	typeArticle = "TechArticle"
	typeSource  = "SoftwareSourceCode"
	typeCited   = "WebPage"
	typeValue   = "PropertyValue"
)

// The ways a record is refused.
var (
	ErrNoURL    = errors.New("a record names the URL the article is published at")
	ErrNoClaims = errors.New("the article neither quotes code nor pins a link; there is no claim to record")
)

// Path is where the record of an article is written.
func Path(article string) string { return article + Suffix }

// Hash is one digest, written as a PropertyValue.
type Hash struct {
	Type       string `json:"@type"`
	PropertyID string `json:"propertyID"`
	Value      string `json:"value"`
}

// Source is one checked quote: the repository, the commit it was taken at, the
// permalink with its line range, and the digest of the quoted block.
type Source struct {
	Type           string `json:"@type"`
	CodeRepository string `json:"codeRepository"`
	Version        string `json:"version"`
	URL            string `json:"url"`
	Identifier     Hash   `json:"identifier"`
}

// Cited is one pinned link the article does not quote.
type Cited struct {
	Type string `json:"@type"`
	URL  string `json:"url"`
}

// Document is the record as it is written.
type Document struct {
	Context    string   `json:"@context"`
	Type       string   `json:"@type"`
	URL        string   `json:"url"`
	Identifier Hash     `json:"identifier"`
	IsBasedOn  []Source `json:"isBasedOn,omitempty"`
	Citation   []Cited  `json:"citation,omitempty"`
}

// Request is one article to record, with what check decided about its blocks.
type Request struct {
	Article string
	Source  string
	URL     string
	Quotes  []quote.Result
}

// Record is the document and the bytes it is written as.
type Record struct {
	Document Document
	Text     string
}

// Build renders the record of one article. Everything in it is derived from
// the article bytes and from what was decided about them, and the arrays are
// sorted, so the same claims in another order write the same bytes.
func Build(req Request) (Record, error) {
	if strings.TrimSpace(req.URL) == "" {
		return Record{}, ErrNoURL
	}
	doc := Document{
		Context:    Context,
		Type:       typeArticle,
		URL:        req.URL,
		Identifier: hashOf([]byte(req.Source)),
	}
	anchored := make(map[string]bool)
	recorded := make(map[string]bool)
	var claims []claim
	for _, result := range req.Quotes {
		if result.Block.Anchor != nil {
			anchored[result.Block.Anchor.URL] = true
		}
		if result.Reason != quote.ReasonMatch {
			continue
		}
		record := result.Quote
		key := strings.Join([]string{record.Repo, record.SHA, record.Path, record.Range(), record.Digest}, " ")
		if recorded[key] {
			continue
		}
		recorded[key] = true
		claims = append(claims, claim{record: record, source: Source{
			Type:           typeSource,
			CodeRepository: "https://github.com/" + record.Repo,
			Version:        record.SHA,
			URL:            permalink(record),
			Identifier:     Hash{Type: typeValue, PropertyID: HashProperty, Value: record.Digest},
		}})
	}
	sort.Slice(claims, func(i, j int) bool { return claims[i].less(claims[j]) })
	for _, c := range claims {
		doc.IsBasedOn = append(doc.IsBasedOn, c.source)
	}
	cited := make(map[string]bool)
	for _, link := range links.Extract(req.Article, req.Source) {
		if !link.Pinned() || anchored[link.URL] || cited[link.URL] {
			continue
		}
		cited[link.URL] = true
		doc.Citation = append(doc.Citation, Cited{Type: typeCited, URL: link.URL})
	}
	sort.Slice(doc.Citation, func(i, j int) bool { return doc.Citation[i].URL < doc.Citation[j].URL })
	if len(doc.IsBasedOn)+len(doc.Citation) == 0 {
		return Record{}, ErrNoClaims
	}
	text, err := marshal(doc)
	if err != nil {
		return Record{}, err
	}
	return Record{Document: doc, Text: text}, nil
}

// claim carries the record a source was built from, so the array is sorted by
// repository, commit, path and start line and not by the rendered strings.
type claim struct {
	record lock.Quote
	source Source
}

func (c claim) less(other claim) bool {
	a, b := c.record, other.record
	switch {
	case a.Repo != b.Repo:
		return a.Repo < b.Repo
	case a.SHA != b.SHA:
		return a.SHA < b.SHA
	case a.Path != b.Path:
		return a.Path < b.Path
	case a.Start != b.Start:
		return a.Start < b.Start
	case a.End != b.End:
		return a.End < b.End
	}
	return a.Digest < b.Digest
}

// permalink is built from the record rather than read out of the article, so
// two links that name the same lines in different spellings write one URL.
func permalink(q lock.Quote) string {
	lines := "#L" + strconv.Itoa(q.Start)
	if q.End != q.Start {
		lines += "-L" + strconv.Itoa(q.End)
	}
	return "https://github.com/" + q.Repo + "/blob/" + q.SHA + "/" + q.Path + lines
}

func hashOf(data []byte) Hash {
	sum := sha256.Sum256(data)
	return Hash{Type: typeValue, PropertyID: HashProperty, Value: hex.EncodeToString(sum[:])}
}

// marshal writes the document the one way the format allows: HTML escaping off
// so a URL keeps the characters it was given, two spaces of indent, and the
// one trailing newline the encoder adds.
func marshal(doc Document) (string, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", Indent)
	if err := encoder.Encode(doc); err != nil {
		return "", err
	}
	return out.String(), nil
}
