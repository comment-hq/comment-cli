package commentsync

import (
	"bytes"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const (
	projectionFormatVersion  = 1
	projectionHeaderMaxSize  = 8 * 1024
	projectionHeaderStart    = "<!-- comment.io:projection\n"
	projectionHeaderEnd      = "comment.io:projection:end -->"
	localAgentDocsDirName    = "_Comment.io Docs"
	localAgentDocsMarkerName = ".comment-agent-docs.json"
)

var projectionContentHashRE = regexp.MustCompile(`^[a-f0-9]{64}$`)

// okfFrontmatterXCommentRE matches the server-owned top-level `x-comment:` key
// that disambiguates an OKF projection frontmatter from a document whose body
// merely starts with `---` (a thematic break or a user's own frontmatter — the
// latter never carries x-comment, which the server strips on ingress). Anchored
// to a line start within the frontmatter block.
var okfFrontmatterXCommentRE = regexp.MustCompile(`(?m)^x-comment:`)

// okfFrontmatterMaxSize bounds the OKF frontmatter close-delimiter scan. It is
// far larger than the legacy 8 KiB HTML-header cap because an OKF manifest can
// carry sizable metadata; it is still bounded well under the 2 MiB okf fetch cap.
const okfFrontmatterMaxSize = 256 * 1024

// splitOkfFrontmatter splits an OKF-flavored projection file into its leading
// YAML frontmatter prefix (including the opening/closing `---` delimiters and
// the blank-line separator that follows) and the body, such that
// prefix+body == data. It returns ok=false unless the data starts with a `---`
// frontmatter block that contains the server-owned `x-comment:` key, so a plain
// document body that merely starts with `---` is never misread as frontmatter.
func splitOkfFrontmatter(data []byte) (prefix, body []byte, ok bool) {
	var openLen int
	switch {
	case bytes.HasPrefix(data, []byte("---\n")):
		openLen = 4
	case bytes.HasPrefix(data, []byte("---\r\n")):
		openLen = 5
	default:
		return nil, nil, false
	}
	// Use a larger cap than the legacy HTML projection header: an OKF manifest
	// can legitimately carry a long description, many tags, or extension
	// metadata. Capping at the 8 KiB header size would fail the close-delimiter
	// scan and make buildFlavorRender abort the whole sync root.
	limit := len(data)
	if limit > okfFrontmatterMaxSize {
		limit = okfFrontmatterMaxSize
	}
	inner := data[openLen:limit]
	rel := findFrontmatterClose(inner)
	if rel < 0 {
		return nil, nil, false
	}
	// The frontmatter text (between the delimiters) must contain x-comment.
	if !okfFrontmatterXCommentRE.Match(inner[:rel]) {
		return nil, nil, false
	}
	// findFrontmatterClose already verified the line at openLen+rel is a `---`
	// close (tolerating trailing whitespace); advance past that whole line.
	bodyStart := advancePastLine(data, openLen+rel)
	switch {
	case bytes.HasPrefix(data[bodyStart:], []byte("\r\n")):
		bodyStart += 2
	case bytes.HasPrefix(data[bodyStart:], []byte("\n")):
		bodyStart++
	}
	return data[:bodyStart], data[bodyStart:], true
}

// advancePastLine returns the index just after the newline that terminates the
// line beginning at lineStart, or len(data) if the line has no terminator.
func advancePastLine(data []byte, lineStart int) int {
	nl := bytes.IndexByte(data[lineStart:], '\n')
	if nl < 0 {
		return len(data)
	}
	return lineStart + nl + 1
}

// findFrontmatterClose returns the index within inner (the content after the
// opening `---\n`) where the closing `---` delimiter line begins, or -1. Only a
// `---` close is recognized (matching the body-extraction switch and the
// server's OKF output); a YAML `...` close is intentionally not treated as a
// frontmatter terminator so detection and extraction stay symmetric.
func findFrontmatterClose(inner []byte) int {
	for off := 0; off < len(inner); {
		nl := bytes.IndexByte(inner[off:], '\n')
		var line []byte
		if nl < 0 {
			line = inner[off:]
		} else {
			line = inner[off : off+nl]
		}
		// Tolerate trailing whitespace on the close delimiter (`--- `): YAML/
		// CommonMark frontmatter allows it, and the body must still be peeled.
		trimmed := bytes.TrimRight(line, " \t\r")
		if bytes.Equal(trimmed, []byte("---")) {
			return off
		}
		if nl < 0 {
			break
		}
		off += nl + 1
	}
	return -1
}

type projectionFileParts struct {
	Header map[string]string
	Body   []byte
	OK     bool
}

// flavorRender carries the per-sync-root presentation flavor and the materials
// a non-plain flavor needs: the server's format=okf document (for the okf
// frontmatter layer) and a slug→note-name resolver (for the obsidian links
// layer). The zero value renders the legacy plain projection.
// slugNameResolver maps a doc slug to its local vault note name (basename), or
// "" when the doc isn't resolvable in this vault (not synced, or a basename
// collision). Used by slugsToBasenames to substitute server-tagged slug
// wikilinks for local vault names.
type slugNameResolver func(slug string) string

type flavorRender struct {
	flavor syncFlavor
	// okfDocument is the server's format=okf bytes (OKF frontmatter + plain
	// body), fetched for the okf+plain flavor.
	okfDocument []byte
	// obsidianSlugDocument is the server's slug-tagged bytes, fetched whenever
	// links=obsidian — its frontmatter framing tracks the frontmatter flavor:
	// format=obsidian-slug-okf (OKF frontmatter + slug-tagged body) for the okf
	// flavor, or format=obsidian-slug (the doc's CANONICAL markdown — its own
	// leading frontmatter preserved verbatim, only body links tagged) for the
	// plain flavor. The single shared server-side tokenizer tags cross-doc links
	// as [[cmnt:slug|label]]; the daemon substitutes each slug for its local
	// vault basename (slugsToBasenames).
	obsidianSlugDocument []byte
	// obsidianSlugPresent records that the obsidian-slug egress was actually
	// fetched (set by buildFlavorRender on success). It is the presence signal —
	// NOT len(obsidianSlugDocument) > 0 — because an empty document is valid and
	// yields a zero-length egress body: byte-length presence would mistake that
	// for "no egress" and fall back to the (possibly stale, if the doc was just
	// cleared) projection.Markdown.
	obsidianSlugPresent bool
	resolve             slugNameResolver
}

// renderProjectionFile renders a document's on-disk projection in the sync
// root's flavor. The frontmatter layer (plain HTML-comment header vs the
// server's OKF frontmatter) and the links layer (verbatim body vs body
// rewritten to [[wikilinks]]) compose independently. The body bytes are always
// reproducible from these inputs, so the presented-body hash
// (placementMeta.BodyContentHash) stays recomputable for the dirty-check.
func renderProjectionFile(projection projectionResponse, baseURL string, row snapshotRow, fr flavorRender) []byte {
	prefix, baseBody := projectionPrefixAndBody(projection, baseURL, row, fr)
	presentedBody := baseBody
	if fr.flavor.Links == flavorLinksObsidian && fr.resolve != nil {
		// baseBody is the server's slug-tagged body ([[slug|label]] placeholders
		// produced once by the shared TS tokenizer). Substitute each slug for its
		// local vault basename (or a reconstructed canonical URL when the linked
		// doc isn't resolvable in this vault) — vault-local resolution the server
		// can't do.
		presentedBody = slugsToBasenames(baseBody, baseURL, fr.resolve)
	}
	out := make([]byte, 0, len(prefix)+len(presentedBody))
	out = append(out, prefix...)
	out = append(out, presentedBody...)
	return out
}

// projectionPrefixAndBody returns the header/frontmatter prefix and the base
// body for a projection. When links=obsidian the server returned a slug-tagged
// document (obsidianSlugDocument) framed for the requested frontmatter flavor:
//   - okf flavor: an OKF-wrapped doc (obsidian-slug-okf) — its OKF frontmatter
//     is the prefix and its slug-tagged prose is the base body.
//   - plain flavor: a CANONICAL-framed doc (obsidian-slug) whose own leading
//     frontmatter is preserved verbatim — so the WHOLE doc (frontmatter + prose)
//     is the base body under the plain HTML header. Splitting off a frontmatter
//     here would drop the user's own frontmatter (data loss); we don't.
// Otherwise okf+plain uses the format=okf document, and plain+plain uses the
// canonical markdown. A missing or unparsable server document falls back to the
// plain header so a file is never written half-formed.
func projectionPrefixAndBody(projection projectionResponse, baseURL string, row snapshotRow, fr flavorRender) (string, string) {
	if fr.obsidianSlugPresent {
		if fr.flavor.Frontmatter == flavorFrontmatterOKF {
			if okfPrefix, body, ok := splitOkfFrontmatter(fr.obsidianSlugDocument); ok {
				return string(okfPrefix), string(body)
			}
			// Unparsable OKF → fall through to the plain fallback below.
		} else {
			return plainProjectionPrefix(projection, baseURL, row, true), string(fr.obsidianSlugDocument)
		}
	}
	if fr.flavor.Frontmatter == flavorFrontmatterOKF && len(fr.okfDocument) > 0 {
		if prefix, body, ok := splitOkfFrontmatter(fr.okfDocument); ok {
			return string(prefix), string(body)
		}
	}
	return plainProjectionPrefix(projection, baseURL, row, fr.flavor.Links == flavorLinksObsidian), projection.Markdown
}

// slugWikilinkRE matches a server-emitted slug-tagged wikilink: the format=
// obsidian-slug serializer tags every cross-doc link with the cmnt: sentinel —
// [[cmnt:slug]] or [[cmnt:slug|label]] / [[cmnt:slug\|label]] (a table-row-
// escaped alias pipe). Matching the sentinel (not a bare [[slug]]) means a
// user-authored wikilink that merely looks like a slug — [[abc123]], [[note]],
// in prose OR inside a code block — is never touched: it carries no cmnt:
// prefix, only links this pipeline tagged do. The sentinel is stripped when
// written to the vault.
//
// This substitution is deliberately NOT fence-aware (re-adding a Go markdown
// walker is exactly the drift-prone duplication this single-serializer refactor
// deletes). The one accepted edge: a user who LITERALLY types the project-
// internal sentinel form — [[cmnt:<3-64 lowercase-alnum>]] — inside a code block
// would have it rewritten. The server tokenizer never EMITS the sentinel inside
// code, so hitting this requires hand-typing an internal marker by hand; the
// mirror is read-only so the canonical doc is unaffected. TestSlugsToBasenames
// pins this behavior.
//
// CROSS-RUNTIME COUPLING: the slug character class below ([a-z0-9]{3,64}) must
// match what the SERVER actually tags. The source of truth is commentDocLinkInfo
// in packages/shared/src/markdown-escape.ts (its `/^\/(?:d|docs)\/([a-z0-9]{3,64})\/?$/`
// path regex) — that function decides which links become [[cmnt:slug]]. If the
// server ever widens the slug alphabet (hyphens, uppercase, longer), widen this
// regex IN LOCKSTEP or the daemon silently stops substituting the new shapes and
// leaves raw [[cmnt:…]] in vaults. TestSlugWikilinkClassMatchesServer pins the
// boundary cases.
var slugWikilinkRE = regexp.MustCompile(`\[\[cmnt:([a-z0-9]{3,64})(?:(\\?\|)([^\]]*))?\]\]`)

// wikilinkSafe mirrors the TS guard (packages/shared/src/obsidian.ts): a value is
// safe to embed inside [[...]] only when it's non-empty and free of Obsidian
// wikilink control chars ([ ] | # ^ CR LF). The server tags links with the
// always-safe cmnt:slug sentinel and wikilinkSafe-checks the LABEL before
// emitting an alias — but the daemon substitutes the LOCAL vault basename, which
// the server never sees, so the basename must be re-checked here. A note named
// e.g. "Roadmap # Q3" or "Spec [draft]" would otherwise emit a wikilink Obsidian
// mis-targets (# / ^ are anchors, ] ends the link); such links fall back to a URL.
func wikilinkSafe(s string) bool {
	return len(s) > 0 && !strings.ContainsAny(s, "[]|#^\r\n")
}

// slugsToBasenames replaces each server-tagged [[cmnt:slug|label]] with the
// linked doc's local vault basename: [[basename|label]] when the doc resolves to
// a wikilink-safe basename, else a reconstructed absolute canonical URL
// ([label](baseURL/d/slug) — the slug round trips losslessly so the link still
// works in Obsidian). The alias separator (raw `|` or table-escaped `\|`) is
// preserved byte-for-byte.
func slugsToBasenames(body, baseURL string, resolve slugNameResolver) string {
	return slugWikilinkRE.ReplaceAllStringFunc(body, func(m string) string {
		g := slugWikilinkRE.FindStringSubmatch(m)
		slug, sep, label := g[1], g[2], g[3]
		if name := resolve(slug); wikilinkSafe(name) {
			if sep == "" {
				return "[[" + name + "]]"
			}
			return "[[" + name + sep + label + "]]"
		}
		url := strings.TrimRight(baseURL, "/") + "/d/" + slug
		if sep == "" {
			return url
		}
		return "[" + label + "](" + url + ")"
	})
}

// plainProjectionPrefix builds the legacy HTML-comment projection header,
// including the trailing newline that separates it from the body.
func plainProjectionPrefix(projection projectionResponse, baseURL string, row snapshotRow, linksRewritten bool) string {
	bodyHash := projection.ContentHash
	if bodyHash == "" {
		bodyHash = sha256Hex(projection.Markdown)
	}
	header := []string{
		"<!-- comment.io:projection",
		"format-version: " + strconv.Itoa(projectionFormatVersion),
		"read-only: true",
		"source: Comment.io",
		"canonical-url: " + projectionCanonicalURL(projection, baseURL),
		"slug: " + cleanProjectionHeaderValue(projection.Slug),
		"revision: " + strconv.Itoa(projection.Revision),
		"content-sha256: " + cleanProjectionHeaderValue(bodyHash),
		"llms: " + cleanProjectionHeaderValue(strings.TrimRight(baseURL, "/")+"/llms.txt"),
		"local-docs-root: " + localAgentDocsDirName,
		"local-sync-docs: " + cleanProjectionHeaderValue(strings.TrimRight(baseURL, "/")+"/llms/local-sync.txt"),
	}
	header = append(header, projectionMetadataHeaderLines(row)...)
	header = append(header, []string{
		"",
		"This file is a read-only local projection. Do not edit it directly or",
		"use filesystem Edit/Write tools here.",
		"To change the document, use the Comment.io API or web UI. For bot",
		"memory, update canonical brain docs such as MEMORY.md or USER.md through",
		"the API; do not use runtime-local memory as the durable source of truth.",
		"For API writes, use the slug/revision fields in this header: GET",
		"/docs/{slug}, then PATCH /docs/{slug} with base_revision and edits.",
	}...)
	if linksRewritten {
		// The body below is presented with cross-doc links rewritten to Obsidian
		// [[wikilinks]] — it is NOT the canonical Markdown. Fetch the canonical
		// form from the API before composing a PATCH.
		header = append(header,
			"The body below is presented with cross-doc links rewritten to Obsidian",
			"[[wikilinks]]; it is NOT canonical. Fetch GET /docs/{slug} for the",
			"canonical Markdown before composing a PATCH. For local agent",
			"instructions, read the top-level "+localAgentDocsDirName+"/ folder.",
		)
	} else {
		header = append(header,
			"The canonical Markdown body starts after this comment. For local agent",
			"instructions, read the top-level "+localAgentDocsDirName+"/ folder.",
		)
	}
	header = append(header, projectionHeaderEnd, "")
	return strings.Join(header, "\n") + "\n"
}

func projectionMetadataHeaderLines(row snapshotRow) []string {
	lines := []string{}
	if row.Section != "" {
		lines = append(lines, "section: "+cleanProjectionHeaderValue(row.Section))
	}
	if row.Section != "botlets-brains" {
		return lines
	}
	for _, item := range []struct {
		key   string
		value string
	}{
		{"botlets-owner-handle", row.BotletsOwnerHandle},
		{"botlets-bot-slug", row.BotletsBotSlug},
		{"botlets-bot-local-name", row.BotletsBotLocalName},
		{"botlets-bot-id", row.BotletsBotID},
		{"botlets-bot-handle", row.BotletsBotHandle},
		{"botlets-bot-agent-id", row.BotletsBotAgentID},
		{"botlets-brain-container-id", row.BotletsBrainContainerID},
		{"botlets-brain-root-folder-id", row.BotletsBrainRootFolderID},
		{"botlets-brain-node-id", row.BotletsBrainNodeID},
	} {
		if strings.TrimSpace(item.value) != "" {
			lines = append(lines, item.key+": "+cleanProjectionHeaderValue(item.value))
		}
	}
	return lines
}

func parseProjectionFile(data []byte) projectionFileParts {
	// An OKF-flavored projection leads with a YAML frontmatter block carrying
	// the server-owned x-comment key (never the HTML-comment header). Detect it
	// first so the dirty-check body excludes the frontmatter, keeping the body
	// hash canonical across the okf flavor.
	if _, body, ok := splitOkfFrontmatter(data); ok {
		return projectionFileParts{Body: body, OK: true}
	}
	if !bytes.HasPrefix(data, []byte(projectionHeaderStart)) {
		return projectionFileParts{Body: data}
	}
	limit := len(data)
	if limit > projectionHeaderMaxSize {
		limit = projectionHeaderMaxSize
	}
	endAt := bytes.Index(data[:limit], []byte(projectionHeaderEnd))
	if endAt < 0 {
		return projectionFileParts{Body: data}
	}
	headerEnd := endAt + len(projectionHeaderEnd)
	bodyStart := headerEnd
	switch {
	case bytes.HasPrefix(data[bodyStart:], []byte("\r\n\r\n")):
		bodyStart += 4
	case bytes.HasPrefix(data[bodyStart:], []byte("\n\n")):
		bodyStart += 2
	case bytes.HasPrefix(data[bodyStart:], []byte("\r\n")):
		bodyStart += 2
	case bytes.HasPrefix(data[bodyStart:], []byte("\n")):
		bodyStart++
	}
	headerText := string(data[len(projectionHeaderStart):endAt])
	header := map[string]string{}
	for _, line := range strings.Split(headerText, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key != "" {
			header[key] = value
		}
	}
	if !isValidProjectionHeader(header) {
		return projectionFileParts{Body: data}
	}
	return projectionFileParts{Header: header, Body: data[bodyStart:], OK: true}
}

func projectionBodyForDirtyCheck(data []byte) []byte {
	parsed := parseProjectionFile(data)
	if parsed.OK {
		return parsed.Body
	}
	return data
}

func projectionBodyForDirtyCheckWithExpectedHash(data []byte, expectedHash string) []byte {
	parsed := parseProjectionFile(data)
	if parsed.OK {
		return parsed.Body
	}
	if expectedHash == "" || !bytes.HasPrefix(data, []byte(projectionHeaderStart)) {
		return data
	}
	limit := len(data)
	if limit > projectionHeaderMaxSize {
		limit = projectionHeaderMaxSize
	}
	endAt := bytes.Index(data[:limit], []byte(projectionHeaderEnd))
	if endAt < 0 {
		return data
	}
	bodyStart := endAt + len(projectionHeaderEnd)
	switch {
	case bytes.HasPrefix(data[bodyStart:], []byte("\r\n\r\n")):
		bodyStart += 4
	case bytes.HasPrefix(data[bodyStart:], []byte("\n\n")):
		bodyStart += 2
	case bytes.HasPrefix(data[bodyStart:], []byte("\r\n")):
		bodyStart += 2
	case bytes.HasPrefix(data[bodyStart:], []byte("\n")):
		bodyStart++
	}
	if sha256Hex(string(data[bodyStart:])) == expectedHash {
		return data[bodyStart:]
	}
	return data
}

func projectionCanonicalURL(projection projectionResponse, baseURL string) string {
	canonical := strings.TrimSpace(projection.CanonicalURL)
	if canonical == "" {
		canonical = strings.TrimRight(baseURL, "/") + "/docs/" + projection.Slug
	}
	if parsed, err := url.Parse(canonical); err == nil {
		parsed.RawQuery = ""
		parsed.Fragment = ""
		canonical = parsed.String()
	}
	return cleanProjectionHeaderValue(canonical)
}

func cleanProjectionHeaderValue(value string) string {
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return strings.TrimSpace(value)
}

func isValidProjectionHeader(header map[string]string) bool {
	if header["format-version"] != strconv.Itoa(projectionFormatVersion) {
		return false
	}
	if header["slug"] == "" || header["revision"] == "" {
		return false
	}
	if _, err := strconv.Atoi(header["revision"]); err != nil {
		return false
	}
	return projectionContentHashRE.MatchString(header["content-sha256"])
}
