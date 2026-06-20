package commentsync

import (
	"fmt"
	"strings"
)

// Sync-root presentation flavors.
//
// A sync root presents each projected document on disk in one of a small set
// of flavors. Flavor is split across two orthogonal axes so they compose:
//
//   - Frontmatter: how the per-file header/metadata block is written.
//   - Links:       how cross-document links in the body are rewritten.
//
// The legacy projection (an HTML-comment header followed by the canonical
// Markdown body, with body links untouched) is flavorFrontmatterPlain +
// flavorLinksPlain. Plain is the zero value: an empty string in config or
// placement metadata means plain, so existing sync state needs no migration.
//
// CRITICAL invariant for the dirty-check: whatever bytes a flavor writes as the
// document *body* must be reproducible from the server projection alone, so the
// daemon can recompute the presented-body hash (placementMeta.BodyContentHash)
// and compare it against the on-disk body. The canonical server content hash is
// kept separately (placementMeta.ContentHash) to detect server-side changes.
// See projection_file.go (parseProjectionFile / projectionBodyForDirtyCheck)
// and sync.go (writeProjection / localProjectionMatchesHash).
const (
	flavorFrontmatterPlain = "plain"
	flavorFrontmatterOKF   = "okf"

	flavorLinksPlain    = "plain"
	flavorLinksObsidian = "obsidian"
)

// syncFlavor is the normalized, validated presentation flavor for a sync root.
type syncFlavor struct {
	Frontmatter string
	Links       string
}

// plainFlavor is the legacy presentation: HTML-comment header + canonical body.
func plainFlavor() syncFlavor {
	return syncFlavor{Frontmatter: flavorFrontmatterPlain, Links: flavorLinksPlain}
}

// IsPlain reports whether both axes are plain, i.e. the on-disk body is the
// verbatim canonical Markdown and the legacy header is used.
func (f syncFlavor) IsPlain() bool {
	return f.Frontmatter == flavorFrontmatterPlain && f.Links == flavorLinksPlain
}

// normalizeFlavor coerces raw config values to a known-good flavor, falling
// back to plain for empty or unrecognized values. Unknown values fall back to
// plain rather than erroring so a forward-dated config can never wedge an older
// daemon into refusing to sync.
func normalizeFlavor(frontmatter, links string) syncFlavor {
	out := plainFlavor()
	switch strings.ToLower(strings.TrimSpace(frontmatter)) {
	case flavorFrontmatterOKF:
		out.Frontmatter = flavorFrontmatterOKF
	}
	switch strings.ToLower(strings.TrimSpace(links)) {
	case flavorLinksObsidian:
		out.Links = flavorLinksObsidian
	}
	return out
}

// configFlavor returns the normalized flavor for a sync-root config.
func configFlavor(cfg Config) syncFlavor {
	return normalizeFlavor(cfg.FrontmatterFlavor, cfg.LinksFlavor)
}

// ConfigFrontmatterFlavor / ConfigLinksFlavor expose the normalized flavor of a
// config for CLI/status output (so plain reads as "plain", not "").
func ConfigFrontmatterFlavor(cfg Config) string { return configFlavor(cfg).Frontmatter }
func ConfigLinksFlavor(cfg Config) string       { return configFlavor(cfg).Links }

// resolveLoginFlavor picks the flavor for a Login: each axis prefers the
// explicit option, else the existing root's value (so a re-login without flavor
// flags is flavor-preserving), else plain.
func resolveLoginFlavor(opts Options, previous Config, hasPrevious bool) syncFlavor {
	frontmatter := opts.FrontmatterFlavor
	if frontmatter == "" && hasPrevious {
		frontmatter = previous.FrontmatterFlavor
	}
	links := opts.LinksFlavor
	if links == "" && hasPrevious {
		links = previous.LinksFlavor
	}
	return normalizeFlavor(frontmatter, links)
}

// ValidateLoginFlavorFlags rejects an explicit (non-empty) login flag whose
// value isn't a known flavor, so a typo like `--frontmatter-flavor ofk` fails
// loudly instead of silently coercing to plain. Empty (preserve/default) is OK.
func ValidateLoginFlavorFlags(frontmatter, links string) error {
	if v := strings.ToLower(strings.TrimSpace(frontmatter)); v != "" && v != flavorFrontmatterPlain && v != flavorFrontmatterOKF {
		return fmt.Errorf("invalid --frontmatter-flavor %q (expected %q or %q)", frontmatter, flavorFrontmatterPlain, flavorFrontmatterOKF)
	}
	if v := strings.ToLower(strings.TrimSpace(links)); v != "" && v != flavorLinksPlain && v != flavorLinksObsidian {
		return fmt.Errorf("invalid --links-flavor %q (expected %q or %q)", links, flavorLinksPlain, flavorLinksObsidian)
	}
	return nil
}

// nonPlainFlavorValue returns value unless it equals plainValue, in which case
// it returns "" so an omitempty JSON field stays absent. This keeps plain
// placement metadata byte-identical to the pre-flavor schema (no churn, no
// migration of existing sync state).
func nonPlainFlavorValue(value, plainValue string) string {
	if value == plainValue {
		return ""
	}
	return value
}
