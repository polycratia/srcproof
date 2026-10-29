// Package gitrepo reads objects out of a local git clone by running the git
// binary. Replace refs are off and no configuration is read from the machine
// or from the user, so the bytes a commit names are the bytes that come back.
package gitrepo

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// The ways a read can fail. A caller that must choose between a verdict and
// "cannot decide" compares against ErrMissingHistory.
var (
	ErrMissingHistory = errors.New("commit is not in the clone")
	ErrMissingPath    = errors.New("path is not in the commit")
	ErrNotQuotable    = errors.New("tree entry is not quotable code")
	ErrObjectName     = errors.New("not a 40-character lowercase object name")
	ErrBadPath        = errors.New("not a path that can be read out of a commit")
	ErrMissingRef     = errors.New("ref is not in the clone")
	ErrAmbiguousRef   = errors.New("ref is both a branch and a tag")
	ErrRefName        = errors.New("not a ref name git may be asked about")
)

const (
	modeSymlink   = "120000"
	modeSubmodule = "160000"
)

var (
	objectName = regexp.MustCompile(`^[0-9a-f]{40}$`)
	refName    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
)

// MissingHistoryError names the commit and the clone that does not have it. A
// shallow clone is worded apart, because there the absence says nothing about
// whether the commit exists.
type MissingHistoryError struct {
	Commit  string
	Dir     string
	Shallow bool
}

func (e *MissingHistoryError) Error() string {
	if e.Shallow {
		return fmt.Sprintf("commit %s is not in the shallow clone at %s; its history was truncated, so nothing can be decided from it", e.Commit, e.Dir)
	}
	return fmt.Sprintf("commit %s is not in the clone at %s; fetch it before deciding", e.Commit, e.Dir)
}

func (e *MissingHistoryError) Unwrap() error { return ErrMissingHistory }

// NotQuotableError names a tree entry whose bytes cannot stand in for code.
type NotQuotableError struct {
	Commit string
	Path   string
	Mode   string
	Kind   string
}

func (e *NotQuotableError) Error() string {
	return fmt.Sprintf("%s in commit %s is a %s (mode %s), not quotable code", e.Path, e.Commit, e.Kind, e.Mode)
}

func (e *NotQuotableError) Unwrap() error { return ErrNotQuotable }

// AmbiguousRefError names a ref that exists as both a branch and a tag.
type AmbiguousRefError struct {
	Name   string
	Branch string
	Tag    string
	Dir    string
}

func (e *AmbiguousRefError) Error() string {
	return fmt.Sprintf("%q in the clone at %s is both the branch %s and the tag %s; one of them has to be named as refs/heads/%s or refs/tags/%s", e.Name, e.Dir, e.Branch, e.Tag, e.Name, e.Name)
}

func (e *AmbiguousRefError) Unwrap() error { return ErrAmbiguousRef }

// Ref is a branch or tag name together with the commit it reaches.
type Ref struct {
	Name   string
	Full   string
	Branch bool
	Tag    bool
	SHA    string
}

// Repo is a local clone that is open for reading.
type Repo struct {
	dir string

	mu      sync.Mutex
	batch   *batch
	shallow *bool
}

// Open prepares an existing clone for reading.
func Open(dir string) (*Repo, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	repo := &Repo{dir: abs}
	if _, err := repo.run("rev-parse", "--git-dir"); err != nil {
		return nil, fmt.Errorf("%s is not a git clone: %w", abs, err)
	}
	return repo, nil
}

// Dir is the directory the clone was opened at.
func (r *Repo) Dir() string { return r.dir }

// Close stops the reader process.
func (r *Repo) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.batch == nil {
		return nil
	}
	running := r.batch
	r.batch = nil
	return running.close()
}

// RequireCommit reports that commit can be read here. A commit the clone does
// not have returns an error wrapping ErrMissingHistory.
func (r *Repo) RequireCommit(commit string) error {
	if !objectName.MatchString(commit) {
		return fmt.Errorf("%q: %w", commit, ErrObjectName)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requireCommit(commit)
}

// Resolve finds name as a branch or a tag and reports the commit it reaches. A
// name that is both a branch and a tag is refused instead of resolved: git
// prefers one of them, and a reader following the other name would arrive at a
// different commit than the one recorded.
func (r *Repo) Resolve(name string) (Ref, error) {
	if err := validRefName(name); err != nil {
		return Ref{}, err
	}
	if strings.HasPrefix(name, "refs/") {
		sha, found, err := r.refCommit(name)
		switch {
		case err != nil:
			return Ref{}, err
		case !found:
			return Ref{}, r.missingRef(name)
		}
		return Ref{
			Name: name, Full: name, SHA: sha,
			Branch: strings.HasPrefix(name, "refs/heads/"),
			Tag:    strings.HasPrefix(name, "refs/tags/"),
		}, nil
	}
	branch, isBranch, err := r.refCommit("refs/heads/" + name)
	if err != nil {
		return Ref{}, err
	}
	tag, isTag, err := r.refCommit("refs/tags/" + name)
	if err != nil {
		return Ref{}, err
	}
	switch {
	case isBranch && isTag:
		return Ref{}, &AmbiguousRefError{Name: name, Branch: branch, Tag: tag, Dir: r.dir}
	case isBranch:
		return Ref{Name: name, Full: "refs/heads/" + name, Branch: true, SHA: branch}, nil
	case isTag:
		return Ref{Name: name, Full: "refs/tags/" + name, Tag: true, SHA: tag}, nil
	}
	return Ref{}, r.missingRef(name)
}

func (r *Repo) missingRef(name string) error {
	return fmt.Errorf("%q is not a branch or a tag in the clone at %s: %w", name, r.dir, ErrMissingRef)
}

// refCommit reports the commit a fully qualified ref reaches. A ref that is
// absent and a ref git declines to peel answer the same way here: there is
// nothing to record for that name.
func (r *Repo) refCommit(full string) (string, bool, error) {
	out, err := r.run("rev-parse", "--verify", "--quiet", "--end-of-options", full+"^{commit}")
	if err != nil {
		return "", false, nil
	}
	sha := strings.TrimSpace(out)
	if !objectName.MatchString(sha) {
		return "", false, fmt.Errorf("git resolved %s to %q: %w", full, sha, ErrObjectName)
	}
	return sha, true, nil
}

func validRefName(name string) error {
	switch {
	case name == "",
		!refName.MatchString(name),
		strings.Contains(name, ".."),
		strings.Contains(name, "//"),
		strings.HasSuffix(name, "/"),
		strings.HasSuffix(name, ".lock"):
		return fmt.Errorf("%q: %w", name, ErrRefName)
	}
	return nil
}

// Blob returns the bytes of path at commit. A symlink and a submodule entry
// are refused: neither carries code that can be quoted.
func (r *Repo) Blob(commit, path string) ([]byte, error) {
	if !objectName.MatchString(commit) {
		return nil, fmt.Errorf("%q: %w", commit, ErrObjectName)
	}
	clean, err := cleanPath(path)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.requireCommit(commit); err != nil {
		return nil, err
	}
	entry, found, err := r.entry(commit, clean)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("%s is not in commit %s: %w", clean, commit, ErrMissingPath)
	}
	switch {
	case entry.mode == modeSymlink:
		return nil, &NotQuotableError{Commit: commit, Path: clean, Mode: entry.mode, Kind: "symlink"}
	case entry.mode == modeSubmodule:
		return nil, &NotQuotableError{Commit: commit, Path: clean, Mode: entry.mode, Kind: "submodule"}
	case entry.typ != "blob":
		return nil, &NotQuotableError{Commit: commit, Path: clean, Mode: entry.mode, Kind: entry.typ}
	}
	if !objectName.MatchString(entry.oid) {
		return nil, fmt.Errorf("git named the object %q: %w", entry.oid, ErrObjectName)
	}
	object, found, err := r.read(entry.oid)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("blob %s of %s in commit %s is absent: %w", entry.oid, clean, commit, ErrMissingHistory)
	}
	return object.data, nil
}

func (r *Repo) requireCommit(commit string) error {
	object, found, err := r.read(commit + "^{commit}")
	if err != nil {
		return err
	}
	if !found || object.typ != "commit" {
		return &MissingHistoryError{Commit: commit, Dir: r.dir, Shallow: r.isShallow()}
	}
	return nil
}

type treeEntry struct {
	mode string
	typ  string
	oid  string
}

func (r *Repo) entry(commit, path string) (treeEntry, bool, error) {
	out, err := r.run("ls-tree", "--full-tree", "-z", commit, "--", path)
	if err != nil {
		return treeEntry{}, false, err
	}
	for _, record := range strings.Split(out, "\x00") {
		if record == "" {
			continue
		}
		info, name, ok := strings.Cut(record, "\t")
		if !ok || name != path {
			continue
		}
		fields := strings.Fields(info)
		if len(fields) != 3 {
			continue
		}
		return treeEntry{mode: fields[0], typ: fields[1], oid: fields[2]}, true, nil
	}
	return treeEntry{}, false, nil
}

func (r *Repo) isShallow() bool {
	if r.shallow != nil {
		return *r.shallow
	}
	shallow := false
	if out, err := r.run("rev-parse", "--is-shallow-repository"); err == nil {
		shallow = strings.TrimSpace(out) == "true"
	}
	r.shallow = &shallow
	return shallow
}

func (r *Repo) command(args ...string) *exec.Cmd {
	options := []string{"--no-replace-objects", "-c", "core.quotepath=false", "-C", r.dir}
	cmd := exec.Command("git", append(options, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_NO_REPLACE_OBJECTS=1",
		"GIT_TERMINAL_PROMPT=0",
	)
	return cmd
}

func (r *Repo) run(args ...string) (string, error) {
	cmd := r.command(args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if message := strings.TrimSpace(stderr.String()); message != "" {
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), message)
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return stdout.String(), nil
}

// read answers one request on the long-lived reader, starting it on first use.
// A desynchronised stream is unusable, so the process is dropped and the next
// call starts a fresh one. The caller holds r.mu.
func (r *Repo) read(spec string) (object, bool, error) {
	if r.batch == nil {
		started, err := r.startBatch()
		if err != nil {
			return object{}, false, err
		}
		r.batch = started
	}
	found, ok, err := r.batch.request(spec)
	if err != nil {
		r.batch.close()
		r.batch = nil
	}
	return found, ok, err
}

type object struct {
	oid  string
	typ  string
	data []byte
}

type batch struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	out   *bufio.Reader
}

func (r *Repo) startBatch() (*batch, error) {
	cmd := r.command("cat-file", "--batch")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("git cat-file --batch: %w", err)
	}
	return &batch{cmd: cmd, stdin: stdin, out: bufio.NewReader(stdout)}, nil
}

func (b *batch) close() error {
	b.stdin.Close()
	return b.cmd.Wait()
}

func (b *batch) request(spec string) (object, bool, error) {
	if _, err := fmt.Fprintf(b.stdin, "%s\n", spec); err != nil {
		return object{}, false, fmt.Errorf("git cat-file --batch: %w", err)
	}
	header, err := b.out.ReadString('\n')
	if err != nil {
		return object{}, false, fmt.Errorf("git cat-file --batch: %w", err)
	}
	header = strings.TrimSuffix(header, "\n")
	if strings.HasSuffix(header, " missing") {
		return object{}, false, nil
	}
	fields := strings.Fields(header)
	if len(fields) != 3 {
		return object{}, false, fmt.Errorf("git cat-file --batch: unreadable header %q", header)
	}
	size, err := strconv.Atoi(fields[2])
	if err != nil || size < 0 {
		return object{}, false, fmt.Errorf("git cat-file --batch: unreadable size in %q", header)
	}
	data := make([]byte, size+1)
	if _, err := io.ReadFull(b.out, data); err != nil {
		return object{}, false, fmt.Errorf("git cat-file --batch: %w", err)
	}
	return object{oid: fields[0], typ: fields[1], data: data[:size]}, true, nil
}

func cleanPath(path string) (string, error) {
	if strings.ContainsAny(path, "\x00\n") {
		return "", fmt.Errorf("path %q holds a byte git cannot be asked about: %w", path, ErrBadPath)
	}
	clean := strings.TrimPrefix(filepath.ToSlash(path), "./")
	switch {
	case clean == "", strings.HasPrefix(clean, "/"), clean == "..", strings.HasPrefix(clean, "../"):
		return "", fmt.Errorf("path %q is not inside a commit: %w", path, ErrBadPath)
	}
	return clean, nil
}
