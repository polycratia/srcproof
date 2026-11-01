// Package pin resolves the branch and tag links of an article to the commits
// those refs reach and records what it resolved in a lock file.
package pin

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/polycratia/srcproof/internal/gitrepo"
	"github.com/polycratia/srcproof/internal/links"
	"github.com/polycratia/srcproof/internal/lock"
)

// LockName is the file pin writes beside the article.
const LockName = "srcproof.lock"

// ErrNoClone says a link names a repository no clone was given for.
var ErrNoClone = errors.New("no clone was named for the repository")

// Resolver answers which commit a branch or tag name reaches in one clone.
type Resolver interface {
	Resolve(name string) (gitrepo.Ref, error)
}

// LockPath is where the lock file of an article lives.
func LockPath(article string) string {
	return filepath.Join(filepath.Dir(article), LockName)
}

// UnresolvedError names a link whose ref, and every longer prefix of it, is
// absent from the clone.
type UnresolvedError struct {
	Repo  string
	Names []string
}

func (e *UnresolvedError) Error() string {
	shortest := e.Names[len(e.Names)-1]
	if len(e.Names) == 1 {
		return fmt.Sprintf("%q is not a branch or a tag in the clone of %s", shortest, e.Repo)
	}
	return fmt.Sprintf("%q is not a branch or a tag in the clone of %s, and neither is any longer prefix up to %q", shortest, e.Repo, e.Names[0])
}

func (e *UnresolvedError) Unwrap() error { return gitrepo.ErrMissingRef }

// Problem is a link pin would not pin, with where it was written.
type Problem struct {
	Link links.Link
	Err  error
}

func (p Problem) Error() string {
	return fmt.Sprintf("%s:%d: %v", p.Link.File, p.Link.Line, p.Err)
}

func (p Problem) Unwrap() error { return p.Err }

// Rewrite is one link that would be rewritten to the commit its ref reaches.
type Rewrite struct {
	Line int
	Col  int
	Ref  string
	SHA  string
	From string
	To   string
}

// Request is one article to pin.
type Request struct {
	Article   string
	Source    string
	Clones    map[string]Resolver
	Base      lock.File
	Normalize lock.Normalize
}

// Plan is everything pin would do to an article, held apart from doing it.
type Plan struct {
	Article  string
	LockPath string
	Source   string
	Pinned   string
	LockText string
	Lock     lock.File
	Rewrites []Rewrite
	Problems []Problem
}

// LockLines is the lock file pin would write, line by line.
func (p Plan) LockLines() []string {
	if p.LockText == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(p.LockText, "\n"), "\n")
}

// Write rewrites the article and writes the lock file beside it.
func (p Plan) Write() error {
	mode := fs.FileMode(0o644)
	if info, err := os.Stat(p.Article); err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.WriteFile(p.Article, []byte(p.Pinned), mode); err != nil {
		return err
	}
	return os.WriteFile(p.LockPath, []byte(p.LockText), 0o644)
}

// Load reads the lock file at path. A file that is not there is not an error.
func Load(path string) (lock.File, bool, error) {
	text, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return lock.File{}, false, nil
	}
	if err != nil {
		return lock.File{}, false, err
	}
	file, err := lock.Parse(string(text))
	if err != nil {
		return lock.File{}, false, fmt.Errorf("%s: %w", path, err)
	}
	return file, true, nil
}

// Resolve reads the links of an article and reports the rewrites and the lock
// file they would make. Nothing is written, a ref resolved once is recorded
// once, and the records of other articles are carried over untouched.
func Resolve(req Request) (Plan, error) {
	name := filepath.Base(req.Article)
	plan := Plan{
		Article:  req.Article,
		LockPath: LockPath(req.Article),
		Source:   req.Source,
		Pinned:   req.Source,
	}
	file := lock.File{
		Normalize: req.Normalize,
		Quotes:    req.Base.Quotes,
		Allows:    req.Base.Allows,
	}
	for _, record := range req.Base.Links {
		if record.Article != name {
			file.Links = append(file.Links, record)
		}
	}
	recorded := make(map[string]bool)
	for _, link := range links.Extract(req.Article, req.Source) {
		if link.Pinned() {
			continue
		}
		repo := link.Owner + "/" + link.Name
		resolver, named := req.Clones[strings.ToLower(repo)]
		if !named {
			plan.Problems = append(plan.Problems, Problem{
				Link: link,
				Err:  fmt.Errorf("%w %s; name it as --repo %s=PATH", ErrNoClone, repo, repo),
			})
			continue
		}
		ref, err := resolveRef(resolver, repo, link.Candidates())
		if err != nil {
			plan.Problems = append(plan.Problems, Problem{Link: link, Err: err})
			continue
		}
		url, ok := link.Pin(ref.Name, ref.SHA)
		if !ok {
			plan.Problems = append(plan.Problems, Problem{
				Link: link,
				Err:  fmt.Errorf("the ref %q is not where it was read in %s", ref.Name, link.URL),
			})
			continue
		}
		record := lock.Link{Article: name, Repo: repo, SHA: ref.SHA, Ref: ref.Name}
		if !recorded[record.Key()] {
			recorded[record.Key()] = true
			file.Links = append(file.Links, record)
		}
		plan.Rewrites = append(plan.Rewrites, Rewrite{
			Line: link.Line, Col: link.Col, Ref: ref.Name, SHA: ref.SHA, From: link.URL, To: url,
		})
	}
	plan.Lock = file
	text, err := file.Format()
	if err != nil {
		return plan, fmt.Errorf("%s: %w", plan.LockPath, err)
	}
	plan.LockText = text
	plan.Pinned = apply(req.Source, plan.Rewrites)
	return plan, nil
}

// resolveRef takes the longest candidate that exists as a ref. A candidate
// that is both a branch and a tag stops the search instead of falling back to
// a shorter name: the link would then be pinned to a commit a reader following
// the other ref would not arrive at.
func resolveRef(resolver Resolver, repo string, candidates []string) (gitrepo.Ref, error) {
	tried := make([]string, 0, len(candidates))
	for _, name := range candidates {
		ref, err := resolver.Resolve(name)
		switch {
		case err == nil:
			return ref, nil
		case errors.Is(err, gitrepo.ErrMissingRef), errors.Is(err, gitrepo.ErrRefName):
			tried = append(tried, name)
		default:
			return gitrepo.Ref{}, err
		}
	}
	return gitrepo.Ref{}, &UnresolvedError{Repo: repo, Names: tried}
}

// apply rewrites a line from its right edge inwards, so the column of a link
// is still the column it was read at after an earlier link on the same line
// has grown into a 40-character commit.
func apply(source string, rewrites []Rewrite) string {
	if len(rewrites) == 0 {
		return source
	}
	grouped := make(map[int][]Rewrite)
	for _, rewrite := range rewrites {
		grouped[rewrite.Line] = append(grouped[rewrite.Line], rewrite)
	}
	lines := strings.Split(source, "\n")
	for number, group := range grouped {
		line := lines[number-1]
		for i := len(group) - 1; i >= 0; i-- {
			rewrite := group[i]
			end := rewrite.Col + len(rewrite.From)
			if end > len(line) || line[rewrite.Col:end] != rewrite.From {
				continue
			}
			line = line[:rewrite.Col] + rewrite.To + line[end:]
		}
		lines[number-1] = line
	}
	return strings.Join(lines, "\n")
}
