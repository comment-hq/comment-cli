package commentsync

import "testing"

func TestNormalizeFlavorDefaultsToPlain(t *testing.T) {
	cases := []struct {
		name        string
		frontmatter string
		links       string
		want        syncFlavor
		wantPlain   bool
	}{
		{"empty", "", "", syncFlavor{flavorFrontmatterPlain, flavorLinksPlain}, true},
		{"explicit plain", "plain", "plain", syncFlavor{flavorFrontmatterPlain, flavorLinksPlain}, true},
		{"okf frontmatter", "okf", "", syncFlavor{flavorFrontmatterOKF, flavorLinksPlain}, false},
		{"obsidian links", "", "obsidian", syncFlavor{flavorFrontmatterPlain, flavorLinksObsidian}, false},
		{"both", "okf", "obsidian", syncFlavor{flavorFrontmatterOKF, flavorLinksObsidian}, false},
		{"case + space", "  OKF ", " Obsidian ", syncFlavor{flavorFrontmatterOKF, flavorLinksObsidian}, false},
		{"unknown falls back to plain", "yaml", "markdown", syncFlavor{flavorFrontmatterPlain, flavorLinksPlain}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeFlavor(tc.frontmatter, tc.links)
			if got != tc.want {
				t.Fatalf("normalizeFlavor(%q,%q) = %+v, want %+v", tc.frontmatter, tc.links, got, tc.want)
			}
			if got.IsPlain() != tc.wantPlain {
				t.Fatalf("IsPlain() = %v, want %v", got.IsPlain(), tc.wantPlain)
			}
		})
	}
}

func TestResolveLoginFlavorPreservesAndOverrides(t *testing.T) {
	prev := Config{FrontmatterFlavor: "okf", LinksFlavor: "obsidian"}
	// No flags on re-login → preserve the existing root's flavor.
	if got := resolveLoginFlavor(Options{}, prev, true); got.Frontmatter != flavorFrontmatterOKF || got.Links != flavorLinksObsidian {
		t.Fatalf("re-login without flags must preserve, got %+v", got)
	}
	// Explicit flags override per axis.
	if got := resolveLoginFlavor(Options{FrontmatterFlavor: "plain"}, prev, true); got.Frontmatter != flavorFrontmatterPlain || got.Links != flavorLinksObsidian {
		t.Fatalf("explicit frontmatter override must apply, preserving links: %+v", got)
	}
	// New root (no previous) defaults to plain.
	if got := resolveLoginFlavor(Options{}, Config{}, false); !got.IsPlain() {
		t.Fatalf("new root must default to plain, got %+v", got)
	}
	// Exported accessors normalize empty → plain.
	if ConfigFrontmatterFlavor(Config{}) != "plain" || ConfigLinksFlavor(Config{}) != "plain" {
		t.Fatal("exported flavor accessors must normalize empty to plain")
	}
}

func TestConfigFlavorReadsConfig(t *testing.T) {
	if got := configFlavor(Config{}); !got.IsPlain() {
		t.Fatalf("zero Config must be plain, got %+v", got)
	}
	got := configFlavor(Config{FrontmatterFlavor: "okf", LinksFlavor: "obsidian"})
	if got.Frontmatter != flavorFrontmatterOKF || got.Links != flavorLinksObsidian {
		t.Fatalf("configFlavor = %+v, want okf/obsidian", got)
	}
}

// TestPlainProjectionBodyHashIsPresentedBody guards the Phase 4a foundation:
// BodyContentHash is the hash of the *presented* on-disk body (what
// projectionBodyForDirtyCheck extracts), not the raw server content hash. For a
// plain projection the presented body is the verbatim canonical Markdown, so
// the two coincide — and a freshly-rendered file must read as clean.
func TestPlainProjectionBodyHashIsPresentedBody(t *testing.T) {
	projection := projectionResponse{
		Slug:        "abc123",
		Markdown:    "# Title\n\nBody text with a [link](/d/xyz789).\n",
		Revision:    3,
		ContentHash: sha256Hex("# Title\n\nBody text with a [link](/d/xyz789).\n"),
	}
	rendered := renderProjectionFile(projection, "https://comt.dev", snapshotRow{}, flavorRender{flavor: plainFlavor()})

	presentedBodyHash := sha256Hex(string(projectionBodyForDirtyCheck(rendered)))
	if presentedBodyHash != projection.ContentHash {
		t.Fatalf("plain presented-body hash %s != server content hash %s", presentedBodyHash, projection.ContentHash)
	}
	// The freshly-written file must be clean against the presented-body hash.
	if !localProjectionMatchesHash(rendered, presentedBodyHash) {
		t.Fatalf("freshly rendered plain projection must match its own presented-body hash")
	}
}

func TestValidateLoginFlavorFlags(t *testing.T) {
	if ValidateLoginFlavorFlags("okf", "obsidian") != nil || ValidateLoginFlavorFlags("", "") != nil {
		t.Fatal("valid/empty flavor flags must pass")
	}
	if ValidateLoginFlavorFlags("ofk", "") == nil {
		t.Fatal("a mistyped frontmatter flavor must be rejected")
	}
	if ValidateLoginFlavorFlags("", "wikilinks") == nil {
		t.Fatal("a mistyped links flavor must be rejected")
	}
}
