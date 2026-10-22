# srcproof

A lockfile and gate for prose that cites code. A link into a repository is only
worth reading if it points at code that cannot move underneath it, so srcproof
requires every repository link in Markdown to name a commit.

This phase enforces the pin itself. Every `github.com` blob, tree or commit
link, and every `raw.githubusercontent.com` link, must name a ref that is
exactly 40 lowercase hex characters. A branch name, a tag, an abbreviated sha
and an uppercase sha are each refused with their own message and a `file:line`.

## Usage

```
go build -o dist/srcproof ./cmd/srcproof
dist/srcproof check README.md docs/design.md
```

## What is refused

| Ref in the link | Verdict |
| --- | --- |
| `main`, `v1.4.0`, `refs/heads/main` | branch or tag name |
| `0123456` | abbreviated sha |
| `0123456789ABCDEF...01234567` | uppercase sha |
| `0123456789abcdef...01234567` | accepted, `#L10-L20` and other fragments included |

Inline links, autolinks and bare URLs are all read. Links inside fenced code
blocks and code spans are left alone, so documentation can still show the form
it is warning against.

## Reading a local clone

A pinned link is only checked against real objects when the repository it names
is given as a local clone:

```
dist/srcproof check --repo polycratia/srcproof=. README.md
```

`--repo owner/name=PATH` may be repeated. Objects are read by running the git
binary with replace refs off and with no system or user configuration, and a
commit reaches git only after it is known to be 40 lowercase hex characters.
Symlink and submodule entries are refused rather than quoted.

A commit the clone does not have is never a pass: `check` says it cannot decide
and exits 2, with a different message when the clone is shallow and its history
was truncated.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | every link names a commit, and every named clone has that commit |
| 1 | at least one link does not name a full commit |
| 2 | at least one link could not be decided, or the command was used wrongly |

## Development

```
make test
```

The tests for `internal/gitrepo` build throwaway clones with the git binary and
skip when git is unavailable.

---

Made by [polycratia](https://polycratia.com).
