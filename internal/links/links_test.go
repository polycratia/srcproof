package links

import (
	"fmt"
	"strings"
	"testing"
)

const (
	pinned    = "0123456789abcdef0123456789abcdef01234567"
	shoutedPin = "0123456789ABCDEF0123456789ABCDEF01234567"
)

type expected struct {
	line    int
	ref     string
	reason  Reason
	message string
}

func TestCheck(t *testing.T) {
	cases := []struct {
		name   string
		source string
		want   []expected
	}{
		{
			name:   "inline link on a branch",
			source: "See [the parser](https://github.com/polycratia/srcproof/blob/main/internal/links/links.go).\n",
			want:   []expected{{line: 1, ref: "main", reason: ReasonName, message: "branch or tag"}},
		},
		{
			name:   "autolink on a tag",
			source: "Directory: <https://github.com/polycratia/srcproof/tree/v1.4.0/internal/links>\n",
			want:   []expected{{line: 1, ref: "v1.4.0", reason: ReasonName, message: "branch or tag"}},
		},
		{
			name:   "bare raw url on a branch",
			source: "Raw: https://raw.githubusercontent.com/polycratia/srcproof/main/go.mod\n",
			want:   []expected{{line: 1, ref: "main", reason: ReasonName, message: "branch or tag"}},
		},
		{
			name:   "raw url behind a refs prefix",
			source: "Raw: https://raw.githubusercontent.com/polycratia/srcproof/refs/heads/main/go.mod\n",
			want:   []expected{{line: 1, ref: "refs/heads/main", reason: ReasonName, message: "branch or tag"}},
		},
		{
			name:   "abbreviated sha",
			source: "Commit [0123456](https://github.com/polycratia/srcproof/commit/0123456) broke it.\n",
			want:   []expected{{line: 1, ref: "0123456", reason: ReasonAbbreviated, message: "abbreviated sha"}},
		},
		{
			name:   "uppercase sha",
			source: "Pinned: https://github.com/polycratia/srcproof/blob/" + shoutedPin + "/go.mod\n",
			want:   []expected{{line: 1, ref: shoutedPin, reason: ReasonUppercase, message: "uppercase sha"}},
		},
		{
			name: "branch link inside a fence is ignored",
			source: "Never write this:\n" +
				"```markdown\n" +
				"[parser](https://github.com/polycratia/srcproof/blob/main/internal/links/links.go)\n" +
				"```\n" +
				"Write this: https://github.com/polycratia/srcproof/blob/" + pinned + "/internal/links/links.go\n",
			want: nil,
		},
		{
			name:   "branch link inside a code span is ignored",
			source: "Avoid `https://github.com/polycratia/srcproof/blob/main/go.mod` in prose.\n",
			want:   nil,
		},
		{
			name:   "full commit with a line fragment passes",
			source: "See [parse](https://github.com/polycratia/srcproof/blob/" + pinned + "/internal/links/links.go#L10-L20).\n",
			want:   nil,
		},
		{
			name:   "commit link with a full sha passes",
			source: "Fixed in <https://github.com/polycratia/srcproof/commit/" + pinned + ">\n",
			want:   nil,
		},
		{
			name: "each refusal reports its own line",
			source: "One: https://github.com/polycratia/srcproof/tree/main\n" +
				"\n" +
				"Three: https://github.com/polycratia/srcproof/commit/0123456789ab\n",
			want: []expected{
				{line: 1, ref: "main", reason: ReasonName, message: "branch or tag"},
				{line: 3, ref: "0123456789ab", reason: ReasonAbbreviated, message: "abbreviated sha"},
			},
		},
		{
			name:   "other hosts are not repository links",
			source: "Docs: https://example.com/polycratia/srcproof/blob/main/go.mod\n",
			want:   nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Check("README.md", c.source)
			if len(got) != len(c.want) {
				t.Fatalf("got %d refusals %v, want %d", len(got), got, len(c.want))
			}
			for i, want := range c.want {
				if got[i].Line != want.line {
					t.Errorf("refusal %d: line %d, want %d", i, got[i].Line, want.line)
				}
				if got[i].Ref != want.ref {
					t.Errorf("refusal %d: ref %q, want %q", i, got[i].Ref, want.ref)
				}
				if got[i].Reason != want.reason {
					t.Errorf("refusal %d: reason %q, want %q", i, got[i].Reason, want.reason)
				}
				if !strings.Contains(got[i].Message, want.message) {
					t.Errorf("refusal %d: message %q does not mention %q", i, got[i].Message, want.message)
				}
				prefix := fmt.Sprintf("README.md:%d: ", want.line)
				if !strings.HasPrefix(got[i].String(), prefix) {
					t.Errorf("refusal %d: %q does not start with %q", i, got[i].String(), prefix)
				}
			}
		})
	}
}

func TestExtractReadsEveryLinkForm(t *testing.T) {
	source := "[a](https://github.com/polycratia/srcproof/blob/main/go.mod) " +
		"<https://github.com/polycratia/srcproof/tree/main> " +
		"https://raw.githubusercontent.com/polycratia/srcproof/main/go.mod\n"
	got := Extract("README.md", source)
	if len(got) != 3 {
		t.Fatalf("got %d links %v, want 3", len(got), got)
	}
	wantKinds := []Kind{KindBlob, KindTree, KindRaw}
	for i, kind := range wantKinds {
		if got[i].Kind != kind {
			t.Errorf("link %d: kind %q, want %q", i, got[i].Kind, kind)
		}
	}
}
