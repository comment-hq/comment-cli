package commentsync

import (
	"strings"
	"testing"
)

// renderRoundTripClean asserts the data-loss-critical invariant: a freshly
// rendered projection file is CLEAN against the body hash writeProjection would
// store for it (sha256 of the presented body, via projectionBodyForDirtyCheck),
// so the dirty-check never falsely flags an untouched flavored file.
func renderRoundTripClean(t *testing.T, rendered []byte) string {
	t.Helper()
	bodyHash := sha256Hex(string(projectionBodyForDirtyCheck(rendered)))
	if !localProjectionMatchesHash(rendered, bodyHash) {
		t.Fatalf("freshly rendered file must be clean against its stored body hash\n%s", rendered)
	}
	return bodyHash
}

// obsidianSlugOkfDoc builds what the server's format=obsidian-slug-okf returns:
// OKF frontmatter (with x-comment) + a body whose cross-doc links are already
// tagged [[cmnt:slug|label]] by the shared server-side tokenizer. The plain
// flavor instead fetches format=obsidian-slug (CANONICAL-framed, the doc's own
// frontmatter verbatim) — tests build that inline.
func obsidianSlugOkfDoc(slug, body string) string {
	return "---\ntype: concept\nx-comment:\n  slug: " + slug + "\n---\n\n" + body
}

func TestRenderObsidianLinksFlavor(t *testing.T) {
	// The doc carries its OWN leading frontmatter (resource: book). The plain
	// flavor must preserve it verbatim in the mirror — dropping it was the P2
	// regression of routing plain+obsidian through the OKF-absorbing serializer.
	canonical := "---\nresource: book\n---\n\n# Note\n\nSee [the other](https://comt.dev/docs/def456) doc.\n"
	projection := projectionResponse{
		Slug:        "abc123",
		Markdown:    canonical,
		Revision:    2,
		ContentHash: sha256Hex(canonical),
	}
	resolve := func(slug string) string {
		if slug == "def456" {
			return "Active Recall"
		}
		return ""
	}
	// format=obsidian-slug is CANONICAL-framed: the doc's own frontmatter is
	// preserved verbatim, only the body's cross-doc link is tagged.
	slugDoc := "---\nresource: book\n---\n\n# Note\n\nSee [[cmnt:def456|the other]] doc.\n"
	fr := flavorRender{flavor: syncFlavor{Frontmatter: flavorFrontmatterPlain, Links: flavorLinksObsidian}, obsidianSlugDocument: []byte(slugDoc), obsidianSlugPresent: true, resolve: resolve}
	rendered := renderProjectionFile(projection, "https://comt.dev", snapshotRow{}, fr)

	// Plain HTML header is preserved; the slug tag is substituted for the vault basename.
	if !strings.Contains(string(rendered), projectionHeaderStart) {
		t.Fatal("plain frontmatter must keep the HTML projection header")
	}
	body := string(projectionBodyForDirtyCheck(rendered))
	// Finding 1: the doc's own frontmatter must survive into the mirror.
	if !strings.Contains(body, "---\nresource: book\n---") {
		t.Fatalf("plain+obsidian must preserve the doc's own frontmatter, got %q", body)
	}
	if !strings.Contains(body, "See [[Active Recall|the other]] doc.") {
		t.Fatalf("slug tag must become the vault basename wikilink, got %q", body)
	}
	// Presented body diverges from canonical → BodyContentHash != ContentHash,
	// but the file is still clean against its presented-body hash.
	if renderRoundTripClean(t, rendered) == projection.ContentHash {
		t.Fatal("obsidian body hash must differ from the canonical content hash")
	}
}

// An empty document is valid (empty markdown is accepted by the API), so
// format=obsidian-slug returns a zero-length body. The plain+obsidian mirror
// must reproduce it as an empty body — and, critically, the PRESENT-but-empty
// egress must win over a stale projection.Markdown. (Race: the doc is cleared
// between the projection fetch (stale non-empty Markdown) and the obsidian-slug
// fetch (now empty). Gating on obsidianSlugPresent, not byte length, is what
// makes the authoritative empty body win instead of mirroring stale content.)
func TestRenderObsidianEmptyDoc(t *testing.T) {
	projection := projectionResponse{
		Slug:        "abc123",
		Markdown:    "# STALE pre-clear content\n", // captured before the doc was cleared
		ContentHash: sha256Hex("# STALE pre-clear content\n"),
	}
	fr := flavorRender{
		flavor:               syncFlavor{Frontmatter: flavorFrontmatterPlain, Links: flavorLinksObsidian},
		obsidianSlugDocument: []byte(""),
		obsidianSlugPresent:  true,
		resolve:              func(string) string { return "" },
	}
	rendered := renderProjectionFile(projection, "https://comt.dev", snapshotRow{}, fr)
	if !strings.Contains(string(rendered), projectionHeaderStart) {
		t.Fatal("empty obsidian doc must still get the plain projection header")
	}
	body := string(projectionBodyForDirtyCheck(rendered))
	if strings.Contains(body, "STALE") {
		t.Fatalf("present empty egress must win over stale projection.Markdown, got %q", body)
	}
	if body != "" {
		t.Fatalf("empty doc must mirror an empty body, got %q", body)
	}
	renderRoundTripClean(t, rendered)
}

func TestRenderOkfFrontmatterFlavor(t *testing.T) {
	okfDoc := "---\ntype: concept\ntitle: Spaced Repetition\nx-comment:\n  slug: abc123\n  revision: 4\n  content_hash: feedface\n---\n\n# Spaced Repetition\n\nLinked [other](https://comt.dev/docs/def456).\n"
	projection := projectionResponse{
		Slug:        "abc123",
		Markdown:    "# Spaced Repetition\n\nLinked [other](https://comt.dev/docs/def456).\n",
		Revision:    4,
		ContentHash: sha256Hex("# Spaced Repetition\n\nLinked [other](https://comt.dev/docs/def456).\n"),
	}

	// okf + plain links: the file is the server's OKF document verbatim.
	frPlain := flavorRender{flavor: syncFlavor{Frontmatter: flavorFrontmatterOKF, Links: flavorLinksPlain}, okfDocument: []byte(okfDoc)}
	rendered := renderProjectionFile(projection, "https://comt.dev", snapshotRow{}, frPlain)
	if string(rendered) != okfDoc {
		t.Fatalf("okf+plain must reproduce the server OKF document verbatim\n got: %q", rendered)
	}
	if strings.Contains(string(rendered), projectionHeaderStart) {
		t.Fatal("okf frontmatter must NOT carry the HTML projection header")
	}
	renderRoundTripClean(t, rendered)

	// okf + obsidian: the obsidian-slug-okf doc carries OKF frontmatter + a
	// slug-tagged body. Frontmatter is preserved; slug tags become vault basenames.
	resolve := func(slug string) string {
		if slug == "def456" {
			return "Active Recall"
		}
		return ""
	}
	slugDoc := obsidianSlugOkfDoc("abc123", "# Spaced Repetition\n\nLinked [[cmnt:def456|other]].\n")
	frBoth := flavorRender{flavor: syncFlavor{Frontmatter: flavorFrontmatterOKF, Links: flavorLinksObsidian}, obsidianSlugDocument: []byte(slugDoc), obsidianSlugPresent: true, resolve: resolve}
	both := renderProjectionFile(projection, "https://comt.dev", snapshotRow{}, frBoth)
	if !strings.HasPrefix(string(both), "---\ntype: concept\nx-comment:") {
		t.Fatalf("okf+obsidian must keep the OKF frontmatter, got prefix %q", string(both)[:60])
	}
	body := string(projectionBodyForDirtyCheck(both))
	if !strings.Contains(body, "Linked [[Active Recall|other]].") {
		t.Fatalf("okf+obsidian slug tag must become the vault basename, got %q", body)
	}
	renderRoundTripClean(t, both)
}

// A missing OKF document (fetch failure path) must fall back to the plain header
// rather than writing a half-formed file.
func TestRenderOkfFallsBackToPlainWhenDocMissing(t *testing.T) {
	projection := projectionResponse{
		Slug:        "abc123",
		Markdown:    "# Body\n",
		ContentHash: sha256Hex("# Body\n"),
	}
	fr := flavorRender{flavor: syncFlavor{Frontmatter: flavorFrontmatterOKF, Links: flavorLinksPlain}} // okfDocument nil
	rendered := renderProjectionFile(projection, "https://comt.dev", snapshotRow{}, fr)
	if !strings.Contains(string(rendered), projectionHeaderStart) {
		t.Fatal("missing OKF document must fall back to the plain header")
	}
	renderRoundTripClean(t, rendered)
}

func TestPlacementFlavorSwitchTriggersRepair(t *testing.T) {
	// A placement written as plain must be flagged for repair when the root's
	// flavor is now okf (the flavor-switch lifecycle).
	plain := placementMeta{} // empty flavor fields == plain
	if placementFlavor(plain) != plainFlavor() {
		t.Fatal("empty placement flavor must normalize to plain")
	}
	okf := placementMeta{FrontmatterFlavor: flavorFrontmatterOKF}
	if placementFlavor(okf) == plainFlavor() {
		t.Fatal("okf placement must not read as plain")
	}
}

// A large OKF frontmatter (>8 KiB) must still be recognized, not abort the sync.
func TestSplitOkfFrontmatterLargeManifest(t *testing.T) {
	big := "---\ntype: concept\nx-comment:\n  slug: abc123\n"
	for i := 0; i < 2000; i++ {
		big += "  tag" + string(rune('a'+i%26)) + ": value-with-some-length-padding\n"
	}
	big += "---\n\n# Body\n"
	if _, _, ok := splitOkfFrontmatter([]byte(big)); !ok {
		t.Fatalf("OKF frontmatter over 8 KiB (%d bytes) must still split", len(big))
	}
}

func TestObsidianHeaderFlagsRewrittenBody(t *testing.T) {
	projection := projectionResponse{Slug: "abc123", Markdown: "x\n", ContentHash: sha256Hex("x\n")}
	resolve := func(slug string) string {
		if slug == "def456" {
			return "Active Recall"
		}
		return ""
	}
	slugDoc := "See [[cmnt:def456|x]].\n" // canonical-framed (plain flavor)
	fr := flavorRender{flavor: syncFlavor{Frontmatter: flavorFrontmatterPlain, Links: flavorLinksObsidian}, obsidianSlugDocument: []byte(slugDoc), obsidianSlugPresent: true, resolve: resolve}
	out := string(renderProjectionFile(projection, "https://comt.dev", snapshotRow{}, fr))
	if !strings.Contains(out, "is NOT canonical") {
		t.Fatalf("obsidian header must warn the body is not canonical, got:\n%s", out)
	}
}
