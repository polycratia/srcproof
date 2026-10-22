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
)

const (
	modeSymlink   = "120000"
	modeSubmodule = "160000"
)

var objectName = regexp.MustCompile(`^[0-9a-f]{40}$`)

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
