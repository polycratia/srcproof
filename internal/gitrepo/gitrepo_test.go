package gitrepo

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const absent = "0123456789abcdef0123456789abcdef01234567"

func TestBlobReadsThePinnedBytes(t *testing.T) {
	dir := newRepo(t)
	first := commit(t, dir, "first", map[string]string{"internal/links/links.go": "package links // first\n"})
	second := commit(t, dir, "second", map[string]string{"internal/links/links.go": "package links // second\n"})
	repo := open(t, dir)

	for _, c := range []struct{ commit, want string }{
		{first, "package links // first\n"},
		{second, "package links // second\n"},
	} {
		got, err := repo.Blob(c.commit, "internal/links/links.go")
		if err != nil {
			t.Fatalf("Blob(%s): %v", c.commit, err)
		}
		if string(got) != c.want {
			t.Errorf("Blob(%s) = %q, want %q", c.commit, got, c.want)
		}
	}
}

func TestBlobIgnoresReplaceRefs(t *testing.T) {
	const original = "module example.com/original\n"
	const forged = "module example.com/forged\n"

	dir := newRepo(t)
	head := commit(t, dir, "first", map[string]string{"go.mod": original})
	oid := git(t, dir, "rev-parse", "HEAD:go.mod")
	replacement := gitInput(t, dir, forged, "hash-object", "-w", "--stdin")
	git(t, dir, "replace", oid, replacement)
	if got := git(t, dir, "cat-file", "blob", oid); got != strings.TrimSpace(forged) {
		t.Skipf("git replace did not take effect, got %q", got)
	}

	repo := open(t, dir)
	got, err := repo.Blob(head, "go.mod")
	if err != nil {
		t.Fatalf("Blob: %v", err)
	}
	if string(got) != original {
		t.Errorf("Blob = %q, want the original %q", got, original)
	}
}

func TestBlobRefusesASymlink(t *testing.T) {
	dir := newRepo(t)
	commit(t, dir, "first", map[string]string{"go.mod": "module example.com/one\n"})
	if err := os.Symlink("go.mod", filepath.Join(dir, "link.mod")); err != nil {
		t.Skipf("symlinks are unavailable here: %v", err)
	}
	head := commit(t, dir, "add a symlink", nil)

	_, err := open(t, dir).Blob(head, "link.mod")
	if !errors.Is(err, ErrNotQuotable) {
		t.Fatalf("Blob on a symlink: %v, want ErrNotQuotable", err)
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("message %q does not say symlink", err)
	}
}

func TestBlobRefusesASubmodule(t *testing.T) {
	dir := newRepo(t)
	head := commit(t, dir, "first", map[string]string{"go.mod": "module example.com/one\n"})
	git(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+head+",vendor/dep")
	tree := git(t, dir, "write-tree")
	gitlink := gitInput(t, dir, "add a gitlink\n", "commit-tree", tree, "-p", head)

	_, err := open(t, dir).Blob(gitlink, "vendor/dep")
	if !errors.Is(err, ErrNotQuotable) {
		t.Fatalf("Blob on a submodule: %v, want ErrNotQuotable", err)
	}
	if !strings.Contains(err.Error(), "submodule") {
		t.Errorf("message %q does not say submodule", err)
	}
}

func TestAMissingCommitIsNotAPass(t *testing.T) {
	dir := newRepo(t)
	commit(t, dir, "first", map[string]string{"go.mod": "module example.com/one\n"})
	repo := open(t, dir)

	err := repo.RequireCommit(absent)
	if !errors.Is(err, ErrMissingHistory) {
		t.Fatalf("RequireCommit: %v, want ErrMissingHistory", err)
	}
	if !strings.Contains(err.Error(), absent) {
		t.Errorf("message %q does not name the commit", err)
	}
	if strings.Contains(err.Error(), "shallow") {
		t.Errorf("a complete clone was worded as shallow: %v", err)
	}
	if _, err := repo.Blob(absent, "go.mod"); !errors.Is(err, ErrMissingHistory) {
		t.Fatalf("Blob: %v, want ErrMissingHistory", err)
	}
}

func TestMissingHistoryIsWordedForAShallowClone(t *testing.T) {
	origin := newRepo(t)
	first := commit(t, origin, "first", map[string]string{"go.mod": "module example.com/one\n"})
	commit(t, origin, "second", map[string]string{"go.mod": "module example.com/two\n"})

	parent := t.TempDir()
	dest := filepath.Join(parent, "shallow")
	git(t, parent, "clone", "--quiet", "--depth", "1", "file://"+origin, dest)
	if got := git(t, dest, "rev-parse", "--is-shallow-repository"); got != "true" {
		t.Skipf("the clone is not shallow here, got %q", got)
	}

	err := open(t, dest).RequireCommit(first)
	if !errors.Is(err, ErrMissingHistory) {
		t.Fatalf("RequireCommit: %v, want ErrMissingHistory", err)
	}
	if !strings.Contains(err.Error(), "shallow") {
		t.Errorf("message %q does not say the clone is shallow", err)
	}
}

func TestNamesGitIsNeverAskedAreRefusedHere(t *testing.T) {
	dir := newRepo(t)
	head := commit(t, dir, "first", map[string]string{"go.mod": "module example.com/one\n"})
	repo := open(t, dir)

	for _, name := range []string{"main", "HEAD", strings.ToUpper(absent), absent[:7], head + "^", ""} {
		if _, err := repo.Blob(name, "go.mod"); !errors.Is(err, ErrObjectName) {
			t.Errorf("Blob(%q): %v, want ErrObjectName", name, err)
		}
		if err := repo.RequireCommit(name); !errors.Is(err, ErrObjectName) {
			t.Errorf("RequireCommit(%q): %v, want ErrObjectName", name, err)
		}
	}
	for _, path := range []string{"", "/etc/passwd", "../outside", "go\nmod"} {
		if _, err := repo.Blob(head, path); !errors.Is(err, ErrBadPath) {
			t.Errorf("Blob path %q: %v, want ErrBadPath", path, err)
		}
	}
}

func TestBlobRefusesWhatIsNotAFile(t *testing.T) {
	dir := newRepo(t)
	head := commit(t, dir, "first", map[string]string{"internal/links/links.go": "package links\n"})
	repo := open(t, dir)

	if _, err := repo.Blob(head, "internal/links/missing.go"); !errors.Is(err, ErrMissingPath) {
		t.Errorf("Blob on a missing path: %v, want ErrMissingPath", err)
	}
	if _, err := repo.Blob(head, "internal/links"); !errors.Is(err, ErrNotQuotable) {
		t.Errorf("Blob on a directory: %v, want ErrNotQuotable", err)
	}
}

func newRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	git(t, dir, "init", "--quiet")
	git(t, dir, "config", "user.name", "srcproof test")
	git(t, dir, "config", "user.email", "test@example.invalid")
	git(t, dir, "config", "commit.gpgsign", "false")
	return dir
}

func open(t *testing.T, dir string) *Repo {
	t.Helper()
	repo, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { repo.Close() })
	return repo
}

func commit(t *testing.T, dir, message string, files map[string]string) string {
	t.Helper()
	for name, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "--quiet", "-m", message)
	return git(t, dir, "rev-parse", "HEAD")
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return gitInput(t, dir, "", args...)
}

func gitInput(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}
