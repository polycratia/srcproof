package pin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/polycratia/srcproof/internal/gitrepo"
	"github.com/polycratia/srcproof/internal/lock"
)

const (
	onBranch = "0123456789abcdef0123456789abcdef01234567"
	onPrefix = "00112233445566778899aabbccddeeff00112233"
	onTag    = "ffeeddccbbaa99887766554433221100ffeeddcc"
)

// clone answers like gitrepo.Repo.Resolve, without a git binary.
type clone struct {
	branches map[string]string
	tags     map[string]string
}

func (c clone) Resolve(name string) (gitrepo.Ref, error) {
	branch, isBranch := c.branches[name]
	tag, isTag := c.tags[name]
	switch {
	case isBranch && isTag:
		return gitrepo.Ref{}, &gitrepo.AmbiguousRefError{Name: name, Branch: branch, Tag: tag, Dir: "/clone"}
	case isBranch:
		return gitrepo.Ref{Name: name, Full: "refs/heads/" + name, Branch: true, SHA: branch}, nil
	case isTag:
		return gitrepo.Ref{Name: name, Full: "refs/tags/" + name, Tag: true, SHA: tag}, nil
	}
	return gitrepo.Ref{}, fmt.Errorf("%q is not a branch or a tag: %w", name, gitrepo.ErrMissingRef)
}

func request(t *testing.T, article, source string, c clone) Request {
	t.Helper()
	normalize, err := lock.NewNormalize(0)
	if err != nil {
		t.Fatalf("NewNormalize: %v", err)
	}
	return Request{
		Article:   article,
		Source:    source,
		Clones:    map[string]Resolver{"polycratia/srcproof": c},
		Normalize: normalize,
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestASlashRefTakesTheLongestPrefixThatExists(t *testing.T) {
	source := "See [links](https://github.com/polycratia/srcproof/blob/feature/x/internal/links/links.go#L10-L20).\n"
	branches := map[string]string{"feature": onPrefix, "feature/x": onBranch}
	plan, err := Resolve(request(t, filepath.Join("docs", "article.md"), source, clone{branches: branches}))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(plan.Problems) != 0 {
		t.Fatalf("problems %v", plan.Problems)
	}
	if len(plan.Rewrites) != 1 {
		t.Fatalf("got %d rewrites %+v, want 1", len(plan.Rewrites), plan.Rewrites)
	}
	if plan.Rewrites[0].Ref != "feature/x" || plan.Rewrites[0].SHA != onBranch {
		t.Errorf("rewrite %+v, want feature/x at %s", plan.Rewrites[0], onBranch)
	}
	want := "See [links](https://github.com/polycratia/srcproof/blob/" + onBranch + "/internal/links/links.go#L10-L20).\n"
	if plan.Pinned != want {
		t.Errorf("pinned %q, want %q", plan.Pinned, want)
	}
	if len(plan.Lock.Links) != 1 {
		t.Fatalf("got %d lock links %+v, want 1", len(plan.Lock.Links), plan.Lock.Links)
	}
	record := plan.Lock.Links[0]
	if record.Article != "article.md" || record.Ref != "feature/x" || record.SHA != onBranch {
		t.Errorf("lock link %+v", record)
	}
	if want := filepath.Join("docs", LockName); plan.LockPath != want {
		t.Errorf("lock path %q, want %q", plan.LockPath, want)
	}
}

func TestABranchAndTagCollisionIsRefused(t *testing.T) {
	source := "Soon: https://github.com/polycratia/srcproof/tree/release/internal\n"
	named := clone{
		branches: map[string]string{"release": onBranch},
		tags:     map[string]string{"release": onTag},
	}
	plan, err := Resolve(request(t, "article.md", source, named))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(plan.Problems) != 1 {
		t.Fatalf("got %d problems %v, want 1", len(plan.Problems), plan.Problems)
	}
	problem := plan.Problems[0]
	if !errors.Is(problem, gitrepo.ErrAmbiguousRef) {
		t.Errorf("problem %v, want ErrAmbiguousRef", problem)
	}
	if !strings.Contains(problem.Error(), "refs/heads/release") {
		t.Errorf("message %q does not offer refs/heads/release", problem)
	}
	if !strings.HasPrefix(problem.Error(), "article.md:1: ") {
		t.Errorf("message %q does not start at article.md:1", problem)
	}
	if len(plan.Rewrites) != 0 || plan.Pinned != source {
		t.Errorf("an ambiguous ref was rewritten: %q", plan.Pinned)
	}
	if len(plan.Lock.Links) != 0 {
		t.Errorf("an ambiguous ref was recorded: %+v", plan.Lock.Links)
	}
}

func TestADryRunLeavesBothFilesUntouched(t *testing.T) {
	dir := t.TempDir()
	article := filepath.Join(dir, "article.md")
	source := "Read https://github.com/polycratia/srcproof/blob/main/go.mod\n"
	if err := os.WriteFile(article, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	base, found, err := Load(LockPath(article))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if found {
		t.Fatal("a lock file was found in a fresh directory")
	}
	req := request(t, article, source, clone{branches: map[string]string{"main": onBranch}})
	req.Base = base
	plan, err := Resolve(req)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(plan.Rewrites) != 1 {
		t.Fatalf("got %d rewrites, want 1", len(plan.Rewrites))
	}
	if got := read(t, article); got != source {
		t.Errorf("a dry run changed the article: %q", got)
	}
	if _, err := os.Stat(plan.LockPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a dry run wrote %s: %v", plan.LockPath, err)
	}

	if err := plan.Write(); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := read(t, article)
	if got != plan.Pinned {
		t.Errorf("the written article %q is not the planned %q", got, plan.Pinned)
	}
	if !strings.Contains(got, "/blob/"+onBranch+"/go.mod") || strings.Contains(got, "/blob/main/") {
		t.Errorf("the link was not rewritten to a commit: %q", got)
	}
	if got := read(t, plan.LockPath); got != plan.LockText {
		t.Errorf("the written lock %q is not the planned %q", got, plan.LockText)
	}
}

func TestTheLockPinWritesRoundTrips(t *testing.T) {
	source := "One: https://github.com/polycratia/srcproof/blob/feature/x/go.mod\n" +
		"Two: https://github.com/polycratia/srcproof/tree/v1.4.0/internal\n" +
		"Again: https://github.com/polycratia/srcproof/blob/feature/x/internal/lock/lock.go\n"
	named := clone{
		branches: map[string]string{"feature/x": onBranch},
		tags:     map[string]string{"v1.4.0": onTag},
	}
	req := request(t, "article.md", source, named)
	base, err := lock.Parse(lock.Header + "\nnormalize trailing-space\n" +
		"allow other.md ctsafe\n" +
		"link article.md polycratia/srcproof " + onPrefix + " gone\n" +
		"link other.md polycratia/srcproof " + onPrefix + " main\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	req.Base = base
	plan, err := Resolve(req)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(plan.Rewrites) != 3 {
		t.Fatalf("got %d rewrites %+v, want 3", len(plan.Rewrites), plan.Rewrites)
	}
	if len(plan.Lock.Links) != 3 {
		t.Fatalf("got %d lock links %+v, want 3", len(plan.Lock.Links), plan.Lock.Links)
	}
	if len(plan.Lock.Allows) != 1 {
		t.Errorf("the allow of another article was dropped: %+v", plan.Lock.Allows)
	}
	if strings.Contains(plan.LockText, " gone\n") {
		t.Errorf("a stale link of this article was kept: %q", plan.LockText)
	}
	if !strings.Contains(plan.LockText, "link other.md polycratia/srcproof "+onPrefix+" main\n") {
		t.Errorf("the link of another article was dropped: %q", plan.LockText)
	}
	again, err := lock.Parse(plan.LockText)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	text, err := again.Format()
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	if text != plan.LockText {
		t.Errorf("Format(Parse(x)) = %q, want %q", text, plan.LockText)
	}
}
