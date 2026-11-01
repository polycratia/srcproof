package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/polycratia/srcproof/internal/gitrepo"
	"github.com/polycratia/srcproof/internal/links"
	"github.com/polycratia/srcproof/internal/lock"
	"github.com/polycratia/srcproof/internal/pin"
)

const usage = `srcproof - repository links in prose must name a full commit

Usage:
  srcproof check [--repo owner/name=PATH]... <file.md>...
  srcproof pin <file.md> [--repo owner/name=PATH]... [--write] [--tabs N]

check reads the given Markdown files and reports every github.com blob, tree or
commit link, and every raw.githubusercontent.com link, whose ref is not exactly
40 lowercase hex characters.

pin resolves the branch and tag links of one article to the commit each ref
reaches in the named clone. By default it only prints the link rewrites and the
lock lines it would make; --write rewrites the links to the 40-hex commit and
writes srcproof.lock beside the article. A ref with slashes makes
blob/feature/x/path ambiguous, so pin takes the longest prefix that exists as a
ref and refuses a name that is both a branch and a tag.

--repo names a local clone of a repository and may be repeated. A pinned link
into a named repository is looked up in that clone, with replace refs off.
--tabs N records in the lock file that a tab expands to N columns, 1 to 16.

Exit codes:
  0  every link names a commit, and every named clone has that commit
  1  at least one link does not name a full commit
  2  at least one link could not be decided, or the command was used wrongly
`

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch args[0] {
	case "check":
		os.Exit(check(args[1:]))
	case "pin":
		os.Exit(runPin(args[1:]))
	case "help", "-h", "--help":
		fmt.Fprint(os.Stdout, usage)
	default:
		fmt.Fprintf(os.Stderr, "srcproof: unknown subcommand %q\n\n%s", args[0], usage)
		os.Exit(2)
	}
}

func check(args []string) int {
	named := newClones()
	flags := flag.NewFlagSet("check", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flags.Var(named, "repo", "local clone of a repository, as owner/name=PATH; repeatable")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	paths := flags.Args()
	if len(paths) == 0 {
		fmt.Fprint(os.Stderr, "srcproof check: no Markdown files given\n\n"+usage)
		return 2
	}
	clones, err := named.open()
	if err != nil {
		fmt.Fprintf(os.Stderr, "srcproof: %v\n", err)
		return 2
	}
	defer func() {
		for _, clone := range clones {
			clone.Close()
		}
	}()

	refused, undecided, unreadable := 0, 0, 0
	for _, path := range paths {
		source, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "srcproof: %v\n", err)
			unreadable++
			continue
		}
		text := string(source)
		for _, refusal := range links.Check(path, text) {
			fmt.Fprintln(os.Stdout, refusal.String())
			refused++
		}
		for _, link := range links.Extract(path, text) {
			clone, named := clones[strings.ToLower(link.Owner+"/"+link.Name)]
			if !named || !link.Pinned() {
				continue
			}
			if err := clone.RequireCommit(link.Ref); err != nil {
				fmt.Fprintf(os.Stdout, "%s:%d: cannot decide: %v\n", link.File, link.Line, err)
				undecided++
			}
		}
	}
	if refused > 0 {
		fmt.Fprintf(os.Stderr, "srcproof: %d repository link(s) do not name a full commit\n", refused)
	}
	if undecided > 0 {
		fmt.Fprintf(os.Stderr, "srcproof: %d repository link(s) could not be decided\n", undecided)
		return 2
	}
	if refused > 0 || unreadable > 0 {
		return 1
	}
	return 0
}

func runPin(args []string) int {
	named := newClones()
	flags := flag.NewFlagSet("pin", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flags.Var(named, "repo", "local clone of a repository, as owner/name=PATH; repeatable")
	write := flags.Bool("write", false, "rewrite the links and write srcproof.lock")
	tabs := flags.Int("tabs", 0, "record that a tab expands to N columns, 1 to 16")
	paths, err := parseInterleaved(flags, args)
	if err != nil {
		return 2
	}
	if len(paths) != 1 {
		fmt.Fprint(os.Stderr, "srcproof pin: name exactly one Markdown file\n\n"+usage)
		return 2
	}
	article := paths[0]
	normalize, err := lock.NewNormalize(*tabs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "srcproof: %v\n", err)
		return 2
	}
	base, found, err := pin.Load(pin.LockPath(article))
	if err != nil {
		fmt.Fprintf(os.Stderr, "srcproof: %v\n", err)
		return 2
	}
	// A lock file keeps the normalization it was written with unless --tabs
	// says otherwise, so pinning one article does not change how the quotes of
	// another are compared.
	if found && !isSet(flags, "tabs") {
		normalize = base.Normalize
	}
	source, err := os.ReadFile(article)
	if err != nil {
		fmt.Fprintf(os.Stderr, "srcproof: %v\n", err)
		return 2
	}
	clones, err := named.open()
	if err != nil {
		fmt.Fprintf(os.Stderr, "srcproof: %v\n", err)
		return 2
	}
	defer func() {
		for _, clone := range clones {
			clone.Close()
		}
	}()
	resolvers := make(map[string]pin.Resolver, len(clones))
	for key, clone := range clones {
		resolvers[key] = clone
	}

	plan, err := pin.Resolve(pin.Request{
		Article:   article,
		Source:    string(source),
		Clones:    resolvers,
		Base:      base,
		Normalize: normalize,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "srcproof: %v\n", err)
		return 2
	}
	for _, rewrite := range plan.Rewrites {
		fmt.Fprintf(os.Stdout, "%s:%d: %s -> %s\n", article, rewrite.Line, rewrite.Ref, rewrite.SHA)
	}
	for _, line := range plan.LockLines() {
		fmt.Fprintf(os.Stdout, "%s: %s\n", plan.LockPath, line)
	}
	for _, problem := range plan.Problems {
		fmt.Fprintln(os.Stdout, problem.Error())
	}
	if len(plan.Problems) > 0 {
		fmt.Fprintf(os.Stderr, "srcproof: %d repository link(s) could not be pinned; nothing was written\n", len(plan.Problems))
		return 2
	}
	if !*write {
		fmt.Fprintf(os.Stderr, "srcproof: %d link(s) would be rewritten; run again with --write\n", len(plan.Rewrites))
		return 0
	}
	if err := plan.Write(); err != nil {
		fmt.Fprintf(os.Stderr, "srcproof: %v\n", err)
		return 2
	}
	fmt.Fprintf(os.Stderr, "srcproof: rewrote %d link(s) in %s and wrote %s\n", len(plan.Rewrites), article, plan.LockPath)
	return 0
}

// parseInterleaved reads flags written after the file name too, so
// "pin FILE --repo owner/name=PATH" works the way it is documented.
func parseInterleaved(flags *flag.FlagSet, args []string) ([]string, error) {
	var operands []string
	for {
		if err := flags.Parse(args); err != nil {
			return nil, err
		}
		args = flags.Args()
		if len(args) == 0 {
			return operands, nil
		}
		operands = append(operands, args[0])
		args = args[1:]
	}
}

func isSet(flags *flag.FlagSet, name string) bool {
	set := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

// clones collects the --repo flags: one local clone per owner/name.
type clones struct {
	paths map[string]string
	given []string
}

func newClones() *clones {
	return &clones{paths: make(map[string]string)}
}

func (c *clones) String() string { return strings.Join(c.given, ",") }

func (c *clones) Set(value string) error {
	spec, path, ok := strings.Cut(value, "=")
	if !ok {
		return fmt.Errorf("%q is not owner/name=PATH", value)
	}
	owner, name, ok := strings.Cut(spec, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return fmt.Errorf("%q does not name a repository as owner/name", spec)
	}
	if path == "" {
		return fmt.Errorf("%q names no directory for %s", value, spec)
	}
	key := strings.ToLower(spec)
	if _, taken := c.paths[key]; taken {
		return fmt.Errorf("repository %s is named more than once", spec)
	}
	c.paths[key] = path
	c.given = append(c.given, spec+"="+path)
	return nil
}

func (c *clones) open() (map[string]*gitrepo.Repo, error) {
	opened := make(map[string]*gitrepo.Repo, len(c.paths))
	for key, path := range c.paths {
		repo, err := gitrepo.Open(path)
		if err != nil {
			for _, done := range opened {
				done.Close()
			}
			return nil, err
		}
		opened[key] = repo
	}
	return opened, nil
}
