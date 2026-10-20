package main

import (
	"fmt"
	"os"

	"github.com/polycratia/srcproof/internal/links"
)

const usage = `srcproof - repository links in prose must name a full commit

Usage:
  srcproof check <file.md>...

check reads the given Markdown files and reports every github.com blob, tree or
commit link, and every raw.githubusercontent.com link, whose ref is not exactly
40 lowercase hex characters. It exits 1 when a link is refused.
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

func check(paths []string) int {
	if len(paths) == 0 {
		fmt.Fprint(os.Stderr, "srcproof check: no Markdown files given\n\n"+usage)
		return 2
	}
	refused := 0
	unreadable := 0
	for _, path := range paths {
		source, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "srcproof: %v\n", err)
			unreadable++
			continue
		}
		for _, refusal := range links.Check(path, string(source)) {
			fmt.Fprintln(os.Stdout, refusal.String())
			refused++
		}
	}
	if refused > 0 {
		fmt.Fprintf(os.Stderr, "srcproof: %d repository link(s) do not name a full commit\n", refused)
	}
	if refused > 0 || unreadable > 0 {
		return 1
	}
	return 0
}
