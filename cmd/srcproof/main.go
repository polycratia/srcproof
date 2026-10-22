package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/polycratia/srcproof/internal/gitrepo"
	"github.com/polycratia/srcproof/internal/links"
)

const usage = `srcproof - repository links in prose must name a full commit

Usage:
  srcproof check [--repo owner/name=PATH]... <file.md>...

check reads the given Markdown files and reports every github.com blob, tree or
commit link, and every raw.githubusercontent.com link, whose ref is not exactly
40 lowercase hex characters.

--repo names a local clone of a repository and may be repeated. A pinned link
into a named repository is looked up in that clone, with replace refs off.

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
