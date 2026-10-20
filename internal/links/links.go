// Package links extracts repository links from Markdown and refuses every link
// whose ref is not a full 40-character lowercase commit sha.
package links

import (
	"fmt"
	"regexp"
	"strings"
)

// Kind tells which flavour of repository link was found.
type Kind string

const (
	KindBlob   Kind = "blob"
	KindTree   Kind = "tree"
	KindCommit Kind = "commit"
	KindRaw    Kind = "raw"
)

// Link is one repository link together with where it was written.
type Link struct {
	File string
	Line int
	URL  string
	Kind Kind
	Ref  string
}

// Reason names why a link was refused.
type Reason string

const (
	ReasonName        Reason = "branch-or-tag"
	ReasonAbbreviated Reason = "abbreviated-sha"
	ReasonUppercase   Reason = "uppercase-sha"
)

// Refusal is a link that does not name a full commit.
type Refusal struct {
	Link
	Reason  Reason
	Message string
}

// String renders a refusal as file:line: message.
func (r Refusal) String() string {
	return fmt.Sprintf("%s:%d: %s", r.File, r.Line, r.Message)
}

const refLength = 40

// minAbbrev is the shortest abbreviation git will produce; shorter hex looking
// refs are far more likely to be branch names.
const minAbbrev = 4

var (
	fullCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)
	hexOnly    = regexp.MustCompile(`^[0-9a-fA-F]+$`)
	urlPattern = regexp.MustCompile(`https?://[^\s<>()\[\]"']+`)
)

// Check reports every repository link in source that fails to name a commit.
func Check(file, source string) []Refusal {
	var refusals []Refusal
	for _, link := range Extract(file, source) {
		if refusal, ok := refuse(link); ok {
			refusals = append(refusals, refusal)
		}
	}
	return refusals
}

// Extract returns the repository links of source. Fenced code blocks and code
// spans are skipped so documentation may show a link it does not endorse.
func Extract(file, source string) []Link {
	var found []Link
	var fenceChar byte
	fenceLen := 0
	inFence := false
	for i, raw := range strings.Split(source, "\n") {
		line := strings.TrimRight(raw, "\r")
		if char, run, ok := fenceMarker(line); ok {
			switch {
			case !inFence:
				inFence, fenceChar, fenceLen = true, char, run
			case char == fenceChar && run >= fenceLen && closesFence(line, char, run):
				inFence = false
			}
			continue
		}
		if inFence {
			continue
		}
		for _, candidate := range findURLs(stripCodeSpans(line)) {
			link, ok := parse(candidate)
			if !ok {
				continue
			}
			link.File, link.Line = file, i+1
			found = append(found, link)
		}
	}
	return found
}

func refuse(link Link) (Refusal, bool) {
	ref := link.Ref
	switch {
	case fullCommit.MatchString(ref):
		return Refusal{}, false
	case len(ref) == refLength && hexOnly.MatchString(ref):
		return Refusal{
			Link:    link,
			Reason:  ReasonUppercase,
			Message: fmt.Sprintf("repository link names the uppercase sha %q; write the same commit as 40 lowercase hex characters", ref),
		}, true
	case len(ref) >= minAbbrev && len(ref) < refLength && hexOnly.MatchString(ref):
		return Refusal{
			Link:    link,
			Reason:  ReasonAbbreviated,
			Message: fmt.Sprintf("repository link names the abbreviated sha %q of %d characters; name the full 40-character commit", ref, len(ref)),
		}, true
	default:
		return Refusal{
			Link:    link,
			Reason:  ReasonName,
			Message: fmt.Sprintf("repository link names the branch or tag %q; name a full 40-character commit instead", ref),
		}, true
	}
}

func parse(rawURL string) (Link, bool) {
	rest, ok := trimScheme(rawURL)
	if !ok {
		return Link{}, false
	}
	if i := strings.IndexAny(rest, "#?"); i >= 0 {
		rest = rest[:i]
	}
	host, path, _ := strings.Cut(rest, "/")
	segments := strings.Split(strings.Trim(path, "/"), "/")
	switch strings.ToLower(host) {
	case "github.com", "www.github.com":
		if len(segments) < 4 {
			return Link{}, false
		}
		kind, ok := githubKind(segments[2])
		if !ok {
			return Link{}, false
		}
		return Link{URL: rawURL, Kind: kind, Ref: segments[3]}, true
	case "raw.githubusercontent.com":
		if len(segments) < 4 {
			return Link{}, false
		}
		ref := segments[2]
		if ref == "refs" && len(segments) >= 6 {
			ref = strings.Join(segments[2:5], "/")
		}
		return Link{URL: rawURL, Kind: KindRaw, Ref: ref}, true
	}
	return Link{}, false
}

func githubKind(segment string) (Kind, bool) {
	switch segment {
	case "blob":
		return KindBlob, true
	case "tree":
		return KindTree, true
	case "commit":
		return KindCommit, true
	}
	return "", false
}

func trimScheme(url string) (string, bool) {
	for _, scheme := range []string{"https://", "http://"} {
		if strings.HasPrefix(url, scheme) {
			return url[len(scheme):], true
		}
	}
	return "", false
}

func findURLs(line string) []string {
	matches := urlPattern.FindAllString(line, -1)
	candidates := make([]string, 0, len(matches))
	for _, match := range matches {
		candidates = append(candidates, strings.TrimRight(match, ".,;:!?"))
	}
	return candidates
}

func stripCodeSpans(line string) string {
	var out strings.Builder
	i := 0
	for i < len(line) {
		if line[i] != '`' {
			out.WriteByte(line[i])
			i++
			continue
		}
		open := run(line, i)
		close := -1
		for j := i + open; j < len(line); {
			if line[j] != '`' {
				j++
				continue
			}
			if length := run(line, j); length == open {
				close = j
				break
			} else {
				j += length
			}
		}
		if close < 0 {
			out.WriteString(line[i : i+open])
			i += open
			continue
		}
		out.WriteString(strings.Repeat(" ", close+open-i))
		i = close + open
	}
	return out.String()
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
