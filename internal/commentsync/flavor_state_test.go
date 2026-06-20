package commentsync

import (
	"context"
	"testing"
	"time"
)

// TestPlacementFlavorRoundTripsThroughSQLite guards the persistence fix: the
// flavor fields must survive an upsert→get/list cycle through the placements
// table, or placementNeedsLocalRepair reads every non-plain placement back as
// plain (breaking flavor-switch detection + re-rendering on every 304 event).
func TestPlacementFlavorRoundTripsThroughSQLite(t *testing.T) {
	paths, err := resolvePaths(t.TempDir())
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	if err := ensurePrivateDirs(paths); err != nil {
		t.Fatalf("ensure private dirs: %v", err)
	}
	ctx := context.Background()
	state, err := openSyncState(ctx, paths)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()

	meta := placementMeta{
		VisibleInstanceID: "doc-1",
		Slug:              "abc123",
		Section:           "my-files",
		Path:              "/tmp/Note.md",
		ContentHash:       "deadbeef",
		BodyContentHash:   "deadbeef",
		Revision:          2,
		LastSeenSnapshot:  "lse1",
		UpdatedAt:         time.Unix(0, 0).UTC(),
		FrontmatterFlavor: flavorFrontmatterOKF,
		LinksFlavor:       flavorLinksObsidian,
	}
	if err := state.upsertPlacement(ctx, meta); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, ok, err := state.getPlacement(ctx, "doc-1")
	if err != nil || !ok {
		t.Fatalf("getPlacement: ok=%v err=%v", ok, err)
	}
	if got.FrontmatterFlavor != flavorFrontmatterOKF || got.LinksFlavor != flavorLinksObsidian {
		t.Fatalf("getPlacement lost flavor: %+v", got)
	}
	if placementFlavor(got).IsPlain() {
		t.Fatal("persisted okf/obsidian placement must not read back as plain")
	}

	list, err := state.listPlacements(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("listPlacements: n=%d err=%v", len(list), err)
	}
	if list[0].FrontmatterFlavor != flavorFrontmatterOKF || list[0].LinksFlavor != flavorLinksObsidian {
		t.Fatalf("listPlacements lost flavor: %+v", list[0])
	}
}

func TestSlugBasenameResolverCollisionsLeaveURL(t *testing.T) {
	// Two docs in different folders share the basename "Plan" → ambiguous, so
	// both resolve to "" (link stays a URL). A unique basename resolves normally.
	resolve := slugBasenameResolver(map[string]string{
		"a": "/vault/Team A/Plan.md",
		"b": "/vault/Team B/Plan.md",
		"c": "/vault/Unique Note.md",
	})
	if resolve("a") != "" || resolve("b") != "" {
		t.Fatalf("colliding basenames must resolve to empty (left as URL)")
	}
	if resolve("c") != "Unique Note" {
		t.Fatalf("unique basename must resolve, got %q", resolve("c"))
	}
}

// TestPlacementLocallyDirtyOKFFrontmatter guards that an OKF frontmatter-only
// edit is detected as dirty (→ preserved to recovery) even though the body hash
// is unchanged, while an untouched file reads clean.
func TestPlacementLocallyDirtyOKFFrontmatter(t *testing.T) {
	okfDoc := "---\ntype: concept\ntitle: Original\nx-comment:\n  slug: abc123\n---\n\n# Body\n"
	projection := projectionResponse{Slug: "abc123", Markdown: "# Body\n", ContentHash: sha256Hex("# Body\n")}
	fr := flavorRender{flavor: syncFlavor{Frontmatter: flavorFrontmatterOKF, Links: flavorLinksPlain}, okfDocument: []byte(okfDoc)}
	rendered := renderProjectionFile(projection, "https://comt.dev", snapshotRow{}, fr)

	prev := placementMeta{
		FrontmatterFlavor:      flavorFrontmatterOKF,
		BodyContentHash:        sha256Hex(string(projectionBodyForDirtyCheck(rendered))),
		RenderedProjectionHash: sha256Hex(string(rendered)),
	}
	if placementLocallyDirty(rendered, prev) {
		t.Fatal("an untouched OKF file must read clean")
	}
	// Edit only the frontmatter (title), keep the body — body hash unchanged.
	edited := []byte("---\ntype: concept\ntitle: HAND EDITED\nx-comment:\n  slug: abc123\n---\n\n# Body\n")
	if !placementLocallyDirty(edited, prev) {
		t.Fatal("an OKF frontmatter-only edit must be detected as dirty (preserve to recovery)")
	}
}
