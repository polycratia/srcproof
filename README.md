# srcproof

A lockfile and gate for prose that cites code. A link into a repository is only
worth reading if it points at code that cannot move underneath it, so srcproof
requires every repository link in Markdown to name a commit.

`check` enforces the pin. Every `github.com` blob, tree or commit link, and
every `raw.githubusercontent.com` link, must name a ref that is exactly 40
lowercase hex characters. A branch name, a tag, an abbreviated sha and an
uppercase sha are each refused with their own message and a `file:line`.
`pin` is how a branch link becomes a pinned one.

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

## Pinning a branch link

`pin` resolves the branch and tag links of one article and records what it
resolved:

```
dist/srcproof pin docs/article.md --repo polycratia/srcproof=.
dist/srcproof pin docs/article.md --repo polycratia/srcproof=. --write
```

Without `--write` nothing is touched: the link rewrites and the lock lines are
printed and that is all. With `--write` every resolved link is rewritten to its
40-hex commit and `srcproof.lock` is written beside the article.

Each ref is resolved with `git rev-parse --verify --end-of-options REF^{commit}`
in the named clone. A ref with slashes makes `blob/feature/x/path` ambiguous,
so the longest prefix that exists as a ref wins: with a branch `feature/x`, the
link `blob/feature/x/go.mod` records `feature/x` and not `feature`. A name that
is both a branch and a tag is refused instead of guessed, because a reader
following the other ref would arrive at a different commit; name it as
`refs/heads/x` or `refs/tags/x`. A ref that is nowhere in the clone, and a link
whose repository was not named with `--repo`, are reported and nothing is
written.

## The lock file

`srcproof.lock` is format v1: the header, a `normalize` line, then sorted
one-line directives.

```
srcproof-lock v1
normalize trailing-space
allow article.md ctsafe
link article.md polycratia/srcproof 0123456789abcdef0123456789abcdef01234567 feature/x
```

Normalization is `trailing-space` on its own by default; `--tabs N` adds
`tabs=N` for an N between 1 and 16, and an existing lock keeps the
normalization it was written with unless `--tabs` says otherwise. The parser
refuses an unknown directive, a duplicate key, hex of the wrong length and
lines out of order, so `format(parse(x)) == x` for every file it accepts.
Records of other articles are carried over as they are.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | every link names a commit, and every named clone has that commit |
| 1 | at least one link does not name a full commit |
| 2 | at least one link could not be decided, or the command was used wrongly |

`pin` uses the same codes: 0 when every link resolved, whether or not it was
asked to write, and 2 when a ref could not be resolved.

## Development

```
make test
```

The tests for `internal/gitrepo` build throwaway clones with the git binary and
skip when git is unavailable. The tests for `internal/pin` resolve refs through
a stand-in and need no git.

---

Made by [polycratia](https://polycratia.com).
