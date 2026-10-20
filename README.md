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

`check` exits 1 when any link is refused and 0 otherwise.

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

## Development

```
make test
```

---

Made by [polycratia](https://polycratia.com).
