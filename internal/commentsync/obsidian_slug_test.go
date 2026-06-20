package commentsync

import (
	"strings"
	"testing"
)

// TestSlugWikilinkClassMatchesServer pins the daemon's slug character class to
// what the server tags. The server's source of truth is commentDocLinkInfo in
// packages/shared/src/markdown-escape.ts (slug = [a-z0-9]{3,64}); this regex must
// accept exactly that and reject anything outside it, or a widened server slug
// would be left as a raw [[cmnt:…]] in the vault. If the server's class changes,
// update slugWikilinkRE in lockstep and this test will flag the boundary shift.
func TestSlugWikilinkClassMatchesServer(t *testing.T) {
	match := func(slug string) bool { return slugWikilinkRE.MatchString("[[cmnt:" + slug + "]]") }
	accept := []string{"abc", strings.Repeat("a", 64), "a1b2c3", "000"}     // 3-char min, 64-char max, lowercase alnum
	reject := []string{"ab", strings.Repeat("a", 65), "Abc", "ab-c", "a_b"} // too short/long, uppercase, hyphen, underscore
	for _, s := range accept {
		if !match(s) {
			t.Errorf("slug %q (in [a-z0-9]{3,64}) must match the server-tagged class", s)
		}
	}
	for _, s := range reject {
		if match(s) {
			t.Errorf("slug %q (outside [a-z0-9]{3,64}) must NOT match — daemon/server class drift", s)
		}
	}
}

func TestSlugsToBasenames(t *testing.T) {
	resolve := func(slug string) string {
		switch slug {
		case "abc123":
			return "Spaced Repetition"
		case "def456":
			return "Active Recall"
		case "hash99":
			return "Roadmap # Q3" // valid filename, but # is an Obsidian anchor
		case "brak11":
			return "Spec [draft]" // ] would close the wikilink early
		}
		return "" // unresolvable: not in vault / collision
	}
	base := "https://comt.dev"
	cases := []struct{ name, in, want string }{
		{"bare tagged slug → basename", "See [[cmnt:abc123]] here", "See [[Spaced Repetition]] here"},
		{"tagged slug+label → basename|label", "See [[cmnt:abc123|the note]] here", "See [[Spaced Repetition|the note]] here"},
		{"table-escaped pipe preserved", `| [[cmnt:abc123\|x]] |`, `| [[Spaced Repetition\|x]] |`},
		{"unresolvable bare → reconstructed URL", "See [[cmnt:zzz999]] here", "See https://comt.dev/d/zzz999 here"},
		{"unresolvable label → reconstructed md link", "See [[cmnt:zzz999|gone]] here", "See [gone](https://comt.dev/d/zzz999) here"},
		// A resolved basename that isn't wikilink-safe (# / ^ anchors, ] / [ / |)
		// must fall back to the URL form, not emit a broken [[...]] wikilink.
		{"unsafe basename (#) bare → URL", "See [[cmnt:hash99]] here", "See https://comt.dev/d/hash99 here"},
		{"unsafe basename (#) label → md link", "See [[cmnt:hash99|plan]] here", "See [plan](https://comt.dev/d/hash99) here"},
		{"unsafe basename ([]) bare → URL", "See [[cmnt:brak11]] here", "See https://comt.dev/d/brak11 here"},
		// The sentinel is what makes these safe: a user-authored wikilink that
		// looks like a slug must NOT be touched, in prose OR inside code.
		{"user wikilink (slug-shaped, no sentinel) untouched in prose", "See [[abc123]] and [[note]]", "See [[abc123]] and [[note]]"},
		{"user wikilink (caps/space) untouched", "See [[My Note]]", "See [[My Note]]"},
		{"slug-shaped text inside a fenced code block untouched", "```\nUse [[abc123]] or [[note]] to link.\n```", "```\nUse [[abc123]] or [[note]] to link.\n```"},
		{"slug-shaped text inside an inline code span untouched", "Type `[[abc123]]` literally.", "Type `[[abc123]]` literally."},
		// Accepted edge (pinned, not a bug): the substitution is deliberately
		// fence-blind, so a user who hand-types the project-internal sentinel
		// form inside a code block IS rewritten. The server tokenizer never
		// emits the sentinel inside code, so this only fires when someone types
		// the internal marker by hand; the mirror is read-only. Re-adding
		// fence-awareness would reintroduce the Go markdown walker this refactor
		// exists to delete — so the behavior is documented here, not "fixed".
		{"literal sentinel inside code IS rewritten (accepted, fence-blind)", "```\n[[cmnt:abc123]]\n```", "```\n[[Spaced Repetition]]\n```"},
		{"non-doc text untouched", "no wikilinks here", "no wikilinks here"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := slugsToBasenames(tc.in, base, resolve); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestDocBasenameCaseInsensitiveExt(t *testing.T) {
	if docBasename("/v/Note.MD") != "Note" || docBasename("/v/Plan.Md") != "Plan" || docBasename("/v/x.md") != "x" {
		t.Fatalf("docBasename must strip .md case-insensitively")
	}
}
