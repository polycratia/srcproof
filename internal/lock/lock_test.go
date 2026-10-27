package lock

import (
	"strings"
	"testing"
)

const (
	sha    = "0123456789abcdef0123456789abcdef01234567"
	older  = "00112233445566778899aabbccddeeff00112233"
	digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

const roundTrip = Header + "\n" +
	"normalize trailing-space tabs=4\n" +
	"allow article.md ctsafe\n" +
	"link article.md polycratia/srcproof " + sha + " feature/x\n" +
	"link other.md polycratia/srcproof " + older + " v1.4.0\n" +
	"quote article.md polycratia/srcproof " + sha + " internal/links/links.go " + digest + "\n"

func TestFormatOfParseIsTheSameFile(t *testing.T) {
	file, err := Parse(roundTrip)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if file.Normalize != (Normalize{TrailingSpace: true, Tabs: 4}) {
		t.Errorf("normalize %+v", file.Normalize)
	}
	if len(file.Links) != 2 || len(file.Quotes) != 1 || len(file.Allows) != 1 {
		t.Fatalf("read %d links, %d quotes, %d allows", len(file.Links), len(file.Quotes), len(file.Allows))
	}
	if file.Links[0].Ref != "feature/x" || file.Links[0].SHA != sha {
		t.Errorf("first link %+v", file.Links[0])
	}
	got, err := file.Format()
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	if got != roundTrip {
		t.Errorf("Format(Parse(x)) = %q, want %q", got, roundTrip)
	}
}

func TestFormatSortsAndParseReadsItBack(t *testing.T) {
	normalize, err := NewNormalize(0)
	if err != nil {
		t.Fatalf("NewNormalize: %v", err)
	}
	file := File{
		Normalize: normalize,
		Links: []Link{
			{Article: "other.md", Repo: "polycratia/srcproof", SHA: older, Ref: "v1.4.0"},
			{Article: "article.md", Repo: "polycratia/srcproof", SHA: sha, Ref: "feature/x"},
		},
		Allows: []Allow{{Article: "article.md", Word: "ctsafe"}},
	}
	text, err := file.Format()
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	want := Header + "\n" +
		"normalize trailing-space\n" +
		"allow article.md ctsafe\n" +
		"link article.md polycratia/srcproof " + sha + " feature/x\n" +
		"link other.md polycratia/srcproof " + older + " v1.4.0\n"
	if text != want {
		t.Fatalf("Format = %q, want %q", text, want)
	}
	read, err := Parse(text)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	again, err := read.Format()
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	if again != text {
		t.Errorf("the second Format = %q, want %q", again, text)
	}
}

func TestFormatRefusesADuplicateKey(t *testing.T) {
	file := File{
		Normalize: Normalize{TrailingSpace: true},
		Links: []Link{
			{Article: "article.md", Repo: "polycratia/srcproof", SHA: sha, Ref: "main"},
			{Article: "article.md", Repo: "polycratia/srcproof", SHA: older, Ref: "main"},
		},
	}
	if _, err := file.Format(); err == nil {
		t.Fatal("Format accepted the same link key twice")
	}
}

func TestParseRefuses(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"no trailing newline", Header + "\nnormalize trailing-space"},
		{"another header", "srcproof-lock v2\nnormalize trailing-space\n"},
		{"no normalize line", Header + "\n"},
		{"bare normalize", Header + "\nnormalize\n"},
		{"normalize out of order", Header + "\nnormalize tabs=4 trailing-space\n"},
		{"tabs out of range", Header + "\nnormalize trailing-space tabs=99\n"},
		{"none beside a normalization", Header + "\nnormalize trailing-space none\n"},
		{"unknown directive", Header + "\nnormalize trailing-space\npin article.md polycratia/srcproof " + sha + " main\n"},
		{"too few fields", Header + "\nnormalize trailing-space\nlink article.md polycratia/srcproof " + sha + "\n"},
		{"two spaces between fields", Header + "\nnormalize trailing-space\nallow  article.md ctsafe\n"},
		{"short sha", Header + "\nnormalize trailing-space\nlink article.md polycratia/srcproof " + sha[:39] + " main\n"},
		{"uppercase sha", Header + "\nnormalize trailing-space\nlink article.md polycratia/srcproof " + strings.ToUpper(sha) + " main\n"},
		{"short digest", Header + "\nnormalize trailing-space\nquote article.md polycratia/srcproof " + sha + " go.mod " + digest[:63] + "\n"},
		{"unsorted lines", Header + "\nnormalize trailing-space\nlink other.md polycratia/srcproof " + sha + " main\nlink article.md polycratia/srcproof " + sha + " main\n"},
		{"the same line twice", Header + "\nnormalize trailing-space\nallow article.md ctsafe\nallow article.md ctsafe\n"},
		{"the same key twice", Header + "\nnormalize trailing-space\nlink article.md polycratia/srcproof " + older + " main\nlink article.md polycratia/srcproof " + sha + " main\n"},
		{"absolute article", Header + "\nnormalize trailing-space\nallow /etc/passwd ctsafe\n"},
		{"article above the lock", Header + "\nnormalize trailing-space\nallow ../article.md ctsafe\n"},
		{"repo without an owner", Header + "\nnormalize trailing-space\nlink article.md srcproof " + sha + " main\n"},
		{"empty file", ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Parse(c.text); err == nil {
				t.Fatalf("Parse accepted %q", c.text)
			}
		})
	}
}

func TestNewNormalizeRefusesAWidthTheFormatCannotRecord(t *testing.T) {
	if _, err := NewNormalize(MaxTabs + 1); err == nil {
		t.Errorf("NewNormalize(%d) was accepted", MaxTabs+1)
	}
	if _, err := NewNormalize(-1); err == nil {
		t.Error("NewNormalize(-1) was accepted")
	}
}
