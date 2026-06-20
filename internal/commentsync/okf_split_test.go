package commentsync

import "testing"

func TestSplitOkfFrontmatter(t *testing.T) {
	okf := "---\ntype: concept\ntitle: Spaced Repetition\nx-comment:\n  slug: abc123\n  revision: 3\n  content_hash: deadbeef\n---\n\n# Spaced Repetition\n\nBody with a [link](/d/def456).\n"
	prefix, body, ok := splitOkfFrontmatter([]byte(okf))
	if !ok {
		t.Fatal("expected OKF frontmatter to split")
	}
	if string(prefix)+string(body) != okf {
		t.Fatalf("prefix+body must reconstruct the input\n prefix=%q\n body=%q", prefix, body)
	}
	wantBody := "# Spaced Repetition\n\nBody with a [link](/d/def456).\n"
	if string(body) != wantBody {
		t.Fatalf("body = %q, want %q", body, wantBody)
	}
	// parseProjectionFile must extract the same body (drives the dirty-check).
	parts := parseProjectionFile([]byte(okf))
	if !parts.OK || string(parts.Body) != wantBody {
		t.Fatalf("parseProjectionFile OKF: ok=%v body=%q", parts.OK, parts.Body)
	}
}

func TestSplitOkfFrontmatterRejectsNonOkf(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		// A thematic break / horizontal rule at the top of a plain body.
		{"thematic break", "---\n\nSome text below a rule.\n"},
		// A user's own YAML frontmatter WITHOUT the server-owned x-comment key.
		{"user frontmatter no x-comment", "---\ntitle: My Note\ntags:\n  - a\n---\n\nBody.\n"},
		// No closing delimiter.
		{"unterminated", "---\ntype: concept\nx-comment:\n  slug: a\nbody without close\n"},
		// Plain HTML-header projection (must stay on the HTML path).
		{"html header", projectionHeaderStart + "slug: abc123\n" + projectionHeaderEnd + "\n\n# Body\n"},
		// Not starting with a frontmatter fence at all.
		{"plain markdown", "# Title\n\nJust content.\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, ok := splitOkfFrontmatter([]byte(tc.in)); ok {
				t.Fatalf("splitOkfFrontmatter must NOT split %q", tc.in)
			}
		})
	}
}

// A doc whose OKF body itself contains `---` lines must not confuse the split —
// only the FIRST frontmatter block (with x-comment) is the header.
func TestSplitOkfFrontmatterBodyWithDashes(t *testing.T) {
	okf := "---\ntype: concept\nx-comment:\n  slug: abc123\n---\nIntro.\n\n---\n\nA thematic break inside the body.\n"
	_, body, ok := splitOkfFrontmatter([]byte(okf))
	if !ok {
		t.Fatal("expected split")
	}
	want := "Intro.\n\n---\n\nA thematic break inside the body.\n"
	if string(body) != want {
		t.Fatalf("body = %q, want %q", body, want)
	}
}
