// Package quote finds the fenced code blocks an article presents as quotes and
// checks each one against the bytes of the blob it names.
package quote

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/polycratia/srcproof/internal/gitrepo"
	"github.com/polycratia/srcproof/internal/links"
	"github.com/polycratia/srcproof/internal/lock"
)

// Reason names what was decided about one code block.
type Reason string

const (
	ReasonMatch      Reason = "match"
	ReasonEdited     Reason = "edited-since-pin"
	ReasonDrift      Reason = "drift"
	ReasonRange      Reason = "range"
	ReasonAnchor     Reason = "anchor"
	ReasonUnanchored Reason = "unanchored"
	ReasonUndecided  Reason = "undecided"
)

// Severity is how a reason weighs on the gate.
type Severity string

const (
	Pass      Severity = "pass"
	Fail      Severity = "fail"
	Undecided Severity = "undecided"
	Listed    Severity = "listed"
)

var fragment = regexp.MustCompile(`^L([0-9]+)(?:-L([0-9]+))?$`)

// Anchor is the line-range permalink that makes a code block a quote.
type Anchor struct {
	Line  int
	URL   string
	Repo  string
	SHA   string
	Path  string
	Start int
	End   int
}

// Range is the START-END the permalink names, as the lock writes it.
func (a Anchor) Range() string { return strconv.Itoa(a.Start) + "-" + strconv.Itoa(a.End) }

// Lines is how many lines of the blob the permalink names.
func (a Anchor) Lines() int { return a.End - a.Start + 1 }

func (a Anchor) differs(record lock.Quote) string {
	switch {
	case !strings.EqualFold(a.Repo, record.Repo), a.SHA != record.SHA, a.Path != record.Path:
		return fmt.Sprintf("the permalink names %s but the lock records %s",
			location(a.Repo, a.SHA, a.Path, a.Range()),
			location(record.Repo, record.SHA, record.Path, record.Range()))
	case a.Start != record.Start, a.End != record.End:
		return fmt.Sprintf("the permalink names lines %s of %s but the lock records %s", a.Range(), a.Path, record.Range())
	}
	return ""
}

func location(repo, sha, path, lines string) string {
	return fmt.Sprintf("lines %s of %s in %s at commit %s", lines, path, repo, sha)
}

// Block is one fenced code block. A block with an Anchor is a quote; a block
// without one is neither checked nor claimed.
type Block struct {
	File   string
	Fence  int
	Close  int
	Info   string
	Lines  []string
	Anchor *Anchor
}

// Result is what was decided about one block.
type Result struct {
	Block   Block
	Digest  string
	Quote   lock.Quote
	Reason  Reason
	Message string
}

// Severity is how the result weighs on the gate.
func (r Result) Severity() Severity {
	switch r.Reason {
	case ReasonMatch:
		return Pass
	case ReasonUnanchored:
		return Listed
	case ReasonUndecided:
		return Undecided
	}
	return Fail
}

// String renders a result as file:line: message.
func (r Result) String() string {
	return fmt.Sprintf("%s:%d: %s", r.Block.File, r.Block.Fence, r.Message)
}

// Blobs reads the bytes of a path at a commit, the way gitrepo.Repo does.
type Blobs interface {
	Blob(commit, path string) ([]byte, error)
}

// Request is one article to check, with the clones its quotes are read in.
type Request struct {
	Article string
	Source  string
	Lock    lock.File
	Clones  map[string]Blobs
}

// Check decides every code block of an article. A block is looked up in the
// lock by the digest of its own text, so a block that no longer hashes to a
// record was edited since pin; a record is only compared against the blob when
// its range holds exactly as many lines as the block.
func Check(req Request) ([]Result, error) {
	name := filepath.Base(req.Article)
	records := make(map[string]lock.Quote)
	for _, record := range req.Lock.Quotes {
		if record.Article == name {
			records[record.Digest] = record
		}
	}
	var results []Result
	for _, block := range Blocks(req.Article, req.Source) {
		result, err := check(req, records, block)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return results, nil
}

func check(req Request, records map[string]lock.Quote, block Block) (Result, error) {
	if block.Anchor == nil {
		return Result{
			Block:   block,
			Reason:  ReasonUnanchored,
			Message: "the code block carries no line-range permalink in the paragraph before or after it; it is unanchored, so it is neither checked nor claimed",
		}, nil
	}
	anchor := *block.Anchor
	digest := Digest(block.Lines, req.Lock.Normalize)
	record, pinned := records[digest]
	if !pinned {
		return Result{
			Block:   block,
			Digest:  digest,
			Reason:  ReasonEdited,
			Message: fmt.Sprintf("the block quoting %s is not the block that was pinned; it was edited since pin", anchor.URL),
		}, nil
	}
	result := Result{Block: block, Digest: digest, Quote: record}
	if record.Lines() != len(block.Lines) {
		result.Reason = ReasonRange
		result.Message = fmt.Sprintf("%s covers %d line(s) of %s but the block holds %d; name the lines the block quotes and nothing else",
			record.Range(), record.Lines(), record.Path, len(block.Lines))
		return result, nil
	}
	if message := anchor.differs(record); message != "" {
		result.Reason = ReasonAnchor
		result.Message = message + "; run pin again"
		return result, nil
	}
	clone, named := req.Clones[strings.ToLower(record.Repo)]
	if !named {
		result.Reason = ReasonUndecided
		result.Message = fmt.Sprintf("cannot decide: no clone was named for %s; name it as --repo %s=PATH", record.Repo, record.Repo)
		return result, nil
	}
	blob, err := clone.Blob(record.SHA, record.Path)
	switch {
	case errors.Is(err, gitrepo.ErrMissingHistory):
		result.Reason = ReasonUndecided
		result.Message = fmt.Sprintf("cannot decide: %v", err)
		return result, nil
	case errors.Is(err, gitrepo.ErrMissingPath), errors.Is(err, gitrepo.ErrNotQuotable):
		result.Reason = ReasonDrift
		result.Message = err.Error()
		return result, nil
	case err != nil:
		return Result{}, fmt.Errorf("%s:%d: %w", block.File, block.Fence, err)
	}
	source := blobLines(blob)
	if record.End > len(source) {
		result.Reason = ReasonRange
		result.Message = fmt.Sprintf("%s names line %d of %s but that file holds %d line(s) at commit %s",
			record.Range(), record.End, record.Path, len(source), record.SHA)
		return result, nil
	}
	for i, line := range source[record.Start-1 : record.End] {
		want := normalized(line, req.Lock.Normalize)
		got := normalized(block.Lines[i], req.Lock.Normalize)
		if want == got {
			continue
		}
		result.Reason = ReasonDrift
		result.Message = fmt.Sprintf("line %d of %s reads %q at commit %s but the block reads %q",
			record.Start+i, record.Path, want, record.SHA, got)
		return result, nil
	}
	result.Reason = ReasonMatch
	result.Message = fmt.Sprintf("the block is %s, byte for byte", location(record.Repo, record.SHA, record.Path, record.Range()))
	return result, nil
}

// Digest is the sha256 of a block after normalization, as the lock records it.
func Digest(lines []string, n lock.Normalize) string {
	sum := sha256.New()
	for _, line := range lines {
		sum.Write([]byte(normalized(line, n)))
		sum.Write([]byte{'\n'})
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// normalized squares up one line by the normalizations the lock names and by
// nothing else: a comparison that is not recorded is one a reader cannot
// repeat.
func normalized(line string, n lock.Normalize) string {
	if n.Tabs > 0 {
		line = expandTabs(line, n.Tabs)
	}
	if n.TrailingSpace {
		line = strings.TrimRight(line, " \t\r")
	}
	return line
}

func expandTabs(line string, width int) string {
	if !strings.Contains(line, "\t") {
		return line
	}
	var out strings.Builder
	column := 0
	for i := 0; i < len(line); i++ {
		if line[i] != '\t' {
			out.WriteByte(line[i])
			column++
			continue
		}
		spaces := width - column%width
		out.WriteString(strings.Repeat(" ", spaces))
		column += spaces
	}
	return out.String()
}

func blobLines(blob []byte) []string {
	text := string(blob)
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

// Blocks returns the fenced code blocks of an article, each with the permalink
// that anchors it when the paragraph before or after it holds one.
func Blocks(file, source string) []Block {
	lines := strings.Split(source, "\n")
	inFence := make([]bool, len(lines))
	var blocks []Block
	for i := 0; i < len(lines); {
		indent, char, run, ok := fenceMarker(trimCR(lines[i]))
		if !ok {
			i++
			continue
		}
		open := i
		info := strings.TrimSpace(trimCR(lines[open])[indent+run:])
		var body []string
		closed := -1
		for j := open + 1; j < len(lines); j++ {
			line := trimCR(lines[j])
			if at, c, length, ok := fenceMarker(line); ok && c == char && length >= run && strings.TrimSpace(line[at+length:]) == "" {
				closed = j
				break
			}
			body = append(body, strip(line, indent))
		}
		if closed < 0 {
			i = open + 1
			continue
		}
		for k := open; k <= closed; k++ {
			inFence[k] = true
		}
		blocks = append(blocks, Block{File: file, Fence: open + 1, Close: closed + 1, Info: info, Lines: body})
		i = closed + 1
	}
	byLine := make(map[int][]links.Link)
	for _, link := range links.Extract(file, source) {
		byLine[link.Line] = append(byLine[link.Line], link)
	}
	for i := range blocks {
		if anchor, ok := anchorFor(blocks[i], lines, inFence, byLine); ok {
			blocks[i].Anchor = &anchor
		}
	}
	return blocks
}

func anchorFor(block Block, lines []string, inFence []bool, byLine map[int][]links.Link) (Anchor, bool) {
	paragraphs := [][]int{
		paragraph(lines, inFence, block.Fence-1, -1),
		paragraph(lines, inFence, block.Close-1, 1),
	}
	for _, numbers := range paragraphs {
		for _, number := range numbers {
			for _, link := range byLine[number] {
				if anchor, ok := anchorOf(link); ok {
					return anchor, true
				}
			}
		}
	}
	return Anchor{}, false
}

// paragraph returns the line numbers of the run of non-blank lines nearest a
// fence in one direction. Another fence ends the search: a permalink inside a
// code block is shown, not claimed.
func paragraph(lines []string, inFence []bool, fence, step int) []int {
	i := fence + step
	for ; i >= 0 && i < len(lines); i += step {
		if inFence[i] {
			return nil
		}
		if strings.TrimSpace(lines[i]) != "" {
			break
		}
	}
	var numbers []int
	for ; i >= 0 && i < len(lines); i += step {
		if inFence[i] || strings.TrimSpace(lines[i]) == "" {
			break
		}
		numbers = append(numbers, i+1)
	}
	sort.Ints(numbers)
	return numbers
}

func anchorOf(link links.Link) (Anchor, bool) {
	if link.Kind != links.KindBlob || !link.Pinned() {
		return Anchor{}, false
	}
	marker := "/blob/" + link.Ref + "/"
	at := strings.Index(link.URL, marker)
	if at < 0 {
		return Anchor{}, false
	}
	path, lines, ok := strings.Cut(link.URL[at+len(marker):], "#")
	if !ok {
		return Anchor{}, false
	}
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	fields := fragment.FindStringSubmatch(lines)
	if path == "" || fields == nil {
		return Anchor{}, false
	}
	start, err := strconv.Atoi(fields[1])
	if err != nil || start < 1 {
		return Anchor{}, false
	}
	end := start
	if fields[2] != "" {
		end, err = strconv.Atoi(fields[2])
		if err != nil || end < start {
			return Anchor{}, false
		}
	}
	return Anchor{
		Line: link.Line, URL: link.URL, Repo: link.Owner + "/" + link.Name,
		SHA: link.Ref, Path: path, Start: start, End: end,
	}, true
}

func fenceMarker(line string) (indent int, char byte, run int, ok bool) {
	for indent < len(line) && line[indent] == ' ' {
		indent++
	}
	if indent > 3 || indent >= len(line) {
		return 0, 0, 0, false
	}
	char = line[indent]
	if char != '`' && char != '~' {
		return 0, 0, 0, false
	}
	for indent+run < len(line) && line[indent+run] == char {
		run++
	}
	if run < 3 {
		return 0, 0, 0, false
	}
	return indent, char, run, true
}

// strip removes the indentation the fence itself was written with, so a block
// inside a list item quotes the code and not the list.
func strip(line string, indent int) string {
	i := 0
	for i < indent && i < len(line) && line[i] == ' ' {
		i++
	}
	return line[i:]
}

func trimCR(line string) string { return strings.TrimRight(line, "\r") }
