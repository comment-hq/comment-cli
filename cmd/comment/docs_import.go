package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"time"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

// docsImportOptions configures `comment docs import <folder>`.
type docsImportOptions struct {
	Home    string
	Profile string
	BaseURL string
	JSON    bool
	DryRun  bool
	Dir     string
}

// importManifest is a local, path-keyed record of what an import created, so a
// re-run is crash-safe and idempotent: a folder/file already recorded is
// skipped rather than duplicated. Keyed by the vault-relative path (stable
// across title edits), NOT the title (titles collide).
type importManifest struct {
	Folders map[string]string `json:"folders"`           // relative dir path -> folder id (lf_…)
	Docs    map[string]string `json:"docs"`              // relative file path -> doc slug
	Pending map[string]bool   `json:"pending,omitempty"` // file paths whose non-idempotent create was in-flight
}

// importManifestPath scopes the de-dup manifest to the DESTINATION (base URL +
// library owner): the same vault imported to two accounts/environments must not
// have the second skip files the first created (they're different Libraries).
// It keys on the OWNER, not the full agent handle, because the API places
// imported docs under the owning account's workspace (library_target: personal,
// derived from owner_handle_id) — so two of the same owner's profiles (e.g.
// max.codex and max.claude) import into one Library and must share one manifest,
// or re-importing the same vault under a sibling profile would duplicate it.
func importManifestPath(dir, baseURL, identity string) string {
	owner := importOwnerScope(identity)
	h := sha256.Sum256([]byte(baseURL + "\x00" + owner))
	return filepath.Join(dir, fmt.Sprintf(".comment-import.%x.json", h[:6]))
}

// importOwnerScope returns the owning account handle for an agent identity:
// agent handles are "owner.agentslug", so the owner is the segment before the
// first dot (a bare handle with no dot is its own owner).
func importOwnerScope(identity string) string {
	if owner, _, ok := strings.Cut(identity, "."); ok {
		return owner
	}
	return identity
}

func runDocsImport(args []string) error {
	options, err := parseDocsImportArgs(args)
	if err != nil {
		return err
	}
	return docsImportCommand(context.Background(), options, os.Stdout)
}

func parseDocsImportArgs(args []string) (docsImportOptions, error) {
	fs := flag.NewFlagSet("comment docs import", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	options := docsImportOptions{}
	fs.StringVar(&options.Home, "home", "", "Comment.io home directory")
	fs.StringVar(&options.Profile, "profile", "", "registered agent profile handle to import as")
	fs.StringVar(&options.BaseURL, "base-url", "", "Comment.io API base URL; with a profile it must match the profile's base_url")
	fs.BoolVar(&options.JSON, "json", false, "print a JSON summary instead of human-readable output")
	fs.BoolVar(&options.DryRun, "dry-run", false, "show what would be created without calling the API")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return docsImportOptions{}, err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return docsImportOptions{}, errors.New("usage: comment docs import <folder> [--profile <handle>] [--dry-run] [--json]")
	}
	options.Dir = rest[0]
	options.Profile = strings.TrimPrefix(options.Profile, "@")
	if options.Profile != "" && !commentbus.ProfileRE.MatchString(options.Profile) {
		return docsImportOptions{}, errors.New("invalid profile")
	}
	return options, nil
}

// importEntry is one markdown file to import, with its relative path + parent dir.
type importEntry struct {
	relPath string // forward-slash relative path from the import root
	relDir  string // forward-slash relative dir ("" for the root)
	absPath string
}

// scanImportDir walks dir and returns the markdown files + the ordered set of
// relative directories that contain them (parent-before-child).
func scanImportDir(dir string) (entries []importEntry, dirs []string, err error) {
	dirSet := map[string]struct{}{}
	walkErr := filepath.WalkDir(dir, func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			// Skip the reserved manifest and hidden dirs (.git, .obsidian, …).
			if path != dir && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		// Only import regular files. This skips symlinks (WalkDir reports the link
		// itself; a later os.ReadFile would follow it — a `note.md -> ~/.ssh/config`
		// link in a shared vault could exfiltrate files outside the import root)
		// AND non-regular files like FIFOs/device nodes, whose os.ReadFile would
		// block waiting for a writer (or stream a device) — an untrusted vault must
		// not be able to hang the import. Use Info() (a stat), not just Type():
		// some filesystems leave the dir-entry type bits unset (0), and
		// FileMode(0).IsRegular() is true, which would let a FIFO slip through.
		info, infoErr := d.Info()
		if infoErr != nil {
			if errors.Is(infoErr, os.ErrNotExist) {
				return nil // raced away between walk and stat
			}
			return infoErr
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") || strings.ToLower(filepath.Ext(d.Name())) != ".md" {
			return nil
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		relDir := filepath.ToSlash(filepath.Dir(rel))
		if relDir == "." {
			relDir = ""
		}
		entries = append(entries, importEntry{relPath: rel, relDir: relDir, absPath: path})
		// Record this dir and every ancestor up to the root.
		for d := relDir; d != ""; d = parentDir(d) {
			dirSet[d] = struct{}{}
		}
		return nil
	})
	if walkErr != nil {
		return nil, nil, walkErr
	}
	for d := range dirSet {
		dirs = append(dirs, d)
	}
	// Parent-before-child: fewer slashes first, then lexical for stability.
	sort.Slice(dirs, func(i, j int) bool {
		di, dj := strings.Count(dirs[i], "/"), strings.Count(dirs[j], "/")
		if di != dj {
			return di < dj
		}
		return dirs[i] < dirs[j]
	})
	sort.Slice(entries, func(i, j int) bool { return entries[i].relPath < entries[j].relPath })
	return entries, dirs, nil
}

func parentDir(rel string) string {
	idx := strings.LastIndex(rel, "/")
	if idx < 0 {
		return ""
	}
	return rel[:idx]
}

func baseName(rel string) string {
	if idx := strings.LastIndex(rel, "/"); idx >= 0 {
		return rel[idx+1:]
	}
	return rel
}

type importResult struct {
	FoldersCreated int      `json:"folders_created"`
	FoldersSkipped int      `json:"folders_skipped"`
	DocsCreated    int      `json:"docs_created"`
	DocsSkipped    int      `json:"docs_skipped"`
	Failed         []string `json:"failed,omitempty"`
}

func docsImportCommand(ctx context.Context, options docsImportOptions, out io.Writer) error {
	info, err := os.Stat(options.Dir)
	if err != nil {
		return fmt.Errorf("read import folder: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a folder; `comment docs import` takes a directory", options.Dir)
	}
	// Resolve a symlinked root: filepath.WalkDir does NOT follow a symlink root,
	// so a vault reached via a symlink would otherwise scan as empty. The manifest
	// is read/written under the resolved real directory too.
	dir := options.Dir
	if resolved, rerr := filepath.EvalSymlinks(options.Dir); rerr == nil {
		dir = resolved
	}

	entries, dirs, err := scanImportDir(dir)
	if err != nil {
		return fmt.Errorf("scan import folder: %w", err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("no .md files found under %s", options.Dir)
	}
	// The folders API trims a folder name before deriving its deterministic node
	// id, so sibling directories whose names differ only by surrounding
	// whitespace (e.g. `Notes` and `Notes `) would silently collapse into one
	// server folder. Reject that up front rather than merge them.
	if err := checkFolderTrimCollisions(dirs); err != nil {
		return err
	}

	// Resolve identity first — a LOCAL read (profile files), no API call — so the
	// manifest is scoped to this destination and the dry-run preview can honor it.
	baseURL, agentSecret, identity, err := resolveDocsCreateIdentity(docsCreateOptions{
		Home:    options.Home,
		Profile: options.Profile,
		BaseURL: options.BaseURL,
	})
	if err != nil {
		// Wrap with import-specific guidance: the shared resolver's message can
		// suggest `--anonymous`, which `comment docs import` does not accept (a
		// registered profile is required), so lead with the correct fix.
		return fmt.Errorf("comment docs import needs a registered --profile <handle> (anonymous import is not supported; docs must land in your Library): %w", err)
	}
	// A vault imports into YOUR Library (library_target: personal), which needs a
	// registered identity. Anonymous import would orphan one per-doc token per
	// file with no way to reopen them — so require a profile.
	if agentSecret == "" {
		return errors.New("`comment docs import` needs a registered agent: pass --profile <handle> (anonymous import isn't supported — docs must land in your Library)")
	}

	manifestPath := importManifestPath(dir, baseURL, identity)
	manifest, err := loadImportManifest(manifestPath)
	if err != nil {
		return err
	}

	if options.DryRun {
		return reportImportDryRun(out, options.JSON, manifest, dirs, entries)
	}

	// Fail fast if the manifest can't be written: it is the ONLY de-dup record,
	// so an unwritable folder would silently create duplicates on every re-run.
	if err := saveImportManifest(manifestPath, manifest); err != nil {
		return fmt.Errorf("cannot write the import manifest under %s — the import folder must be writable to be resumable/idempotent: %w", dir, err)
	}
	result := importResult{}

	// 1) Folders, parent-first. Idempotent server-side + skipped when recorded.
	// A folder is only created once its parent folder id exists; otherwise it (and
	// its descendant files) are failed, NOT silently re-homed at the personal root.
	for _, d := range dirs {
		if _, ok := manifest.Folders[d]; ok {
			result.FoldersSkipped++
			continue
		}
		parent := parentDir(d)
		if parent != "" {
			if _, ok := manifest.Folders[parent]; !ok {
				result.Failed = append(result.Failed, fmt.Sprintf("folder %s: parent folder %q was not created", d, parent))
				continue
			}
		}
		id, ferr := postDocsFolder(ctx, baseURL, agentSecret, baseName(d), manifest.Folders[parent])
		if ferr != nil {
			result.Failed = append(result.Failed, fmt.Sprintf("folder %s: %v", d, ferr))
			continue
		}
		manifest.Folders[d] = id
		result.FoldersCreated++
		if err := saveImportManifest(manifestPath, manifest); err != nil { // persist as we go so a crash is resumable
			return fmt.Errorf("manifest write failed mid-import (folder %s); stopping to avoid duplicate creates on retry: %w", d, err)
		}
	}

	// 2) One doc per file, placed in its folder. Skipped when already recorded;
	// failed (not re-homed) when its folder wasn't created.
	for _, e := range entries {
		if _, ok := manifest.Docs[e.relPath]; ok {
			result.DocsSkipped++
			continue
		}
		// A pending marker means a prior run was interrupted AFTER starting the
		// create but BEFORE recording it. POST /docs is not idempotent, so we
		// can't know whether the doc was created — surface it for manual review
		// instead of silently duplicating (re-create) or silently skipping.
		if manifest.Pending[e.relPath] {
			result.Failed = append(result.Failed, fmt.Sprintf("doc %s: a previous import was interrupted while creating this doc; it may or may not exist in your Library — verify there, then delete the doc (or remove %q from the manifest's \"pending\") and re-run", e.relPath, e.relPath))
			continue
		}
		if e.relDir != "" {
			if _, ok := manifest.Folders[e.relDir]; !ok {
				result.Failed = append(result.Failed, fmt.Sprintf("doc %s: folder %q was not created", e.relPath, e.relDir))
				continue
			}
		}
		markdown, rerr := os.ReadFile(e.absPath)
		if rerr != nil {
			result.Failed = append(result.Failed, fmt.Sprintf("read %s: %v", e.relPath, rerr))
			continue
		}
		// Record the create as in-flight BEFORE the non-idempotent POST so a crash
		// in the gap below is recoverable (detected on the next run, above).
		manifest.Pending[e.relPath] = true
		if err := saveImportManifest(manifestPath, manifest); err != nil {
			return fmt.Errorf("manifest write failed before creating doc %s: %w", e.relPath, err)
		}
		parsed, definitelyNotCreated, derr := postDocsCreateInTarget(ctx, baseURL, agentSecret, string(markdown), manifest.Folders[e.relDir])
		if derr != nil {
			// Only clear the marker when the server DEFINITIVELY did not create the
			// doc (a 4xx, or the request was never sent): then a retry can safely
			// re-attempt. For an ambiguous failure (transport error, 5xx, or a
			// parse failure after a 2xx) the doc may exist, so keep the marker — the
			// next run surfaces it for verification instead of risking a duplicate.
			if definitelyNotCreated {
				delete(manifest.Pending, e.relPath)
				if serr := saveImportManifest(manifestPath, manifest); serr != nil {
					// The marker is cleared in memory but not on disk; stop so the
					// next run doesn't wrongly block this file as uncertain.
					return fmt.Errorf("manifest write failed clearing the marker for doc %s (server rejected it): %w", e.relPath, serr)
				}
			}
			result.Failed = append(result.Failed, fmt.Sprintf("doc %s: %v", e.relPath, derr))
			continue
		}
		manifest.Docs[e.relPath] = parsed.ID
		delete(manifest.Pending, e.relPath)
		result.DocsCreated++
		if err := saveImportManifest(manifestPath, manifest); err != nil {
			return fmt.Errorf("manifest write failed mid-import (doc %s); stopping to avoid duplicate creates on retry: %w", e.relPath, err)
		}
	}

	if err := saveImportManifest(manifestPath, manifest); err != nil {
		return fmt.Errorf("final manifest write failed: %w", err)
	}
	printImportResult(out, options, identity, result)
	if len(result.Failed) > 0 {
		return fmt.Errorf("%d item(s) failed to import (re-run to retry; already-imported items are skipped)", len(result.Failed))
	}
	return nil
}

func printImportResult(out io.Writer, options docsImportOptions, identity string, result importResult) {
	if options.JSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		_ = enc.Encode(result)
		return
	}
	fmt.Fprintf(out, "Imported as %s: %d doc(s) created (%d skipped), %d folder(s) created (%d skipped).\n",
		identity, result.DocsCreated, result.DocsSkipped, result.FoldersCreated, result.FoldersSkipped)
	if len(result.Failed) > 0 {
		fmt.Fprintf(out, "%d item(s) failed (re-run to retry — already-imported items are skipped):\n", len(result.Failed))
		for _, f := range result.Failed {
			fmt.Fprintf(out, "  - %s\n", f)
		}
	}
}

// reportImportDryRun previews the import WITHOUT calling the API, honoring the
// destination manifest so a retry preview shows only what is actually left to
// create (not every discovered file).
func reportImportDryRun(out io.Writer, asJSON bool, manifest importManifest, dirs []string, entries []importEntry) error {
	// Initialize as empty (non-nil) slices so the --json schema is stable: empty
	// lists encode as `[]`, not `null`, so scripts can always iterate them.
	newFolders, newDocs, pendingDocs := []string{}, []string{}, []string{}
	skipFolders, skipDocs := 0, 0
	for _, d := range dirs {
		if _, ok := manifest.Folders[d]; ok {
			skipFolders++
		} else {
			newFolders = append(newFolders, d)
		}
	}
	for _, e := range entries {
		if _, ok := manifest.Docs[e.relPath]; ok {
			skipDocs++
		} else if manifest.Pending[e.relPath] {
			// A real import would NOT create this (a prior run was interrupted
			// mid-create); it would be surfaced for manual verification. Reflect
			// that in the preview instead of listing it under docs_to_create.
			pendingDocs = append(pendingDocs, e.relPath)
		} else {
			newDocs = append(newDocs, e.relPath)
		}
	}
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{
			"dry_run":                  true,
			"folders_to_create":        newFolders,
			"folders_already_imported": skipFolders,
			"docs_to_create":           newDocs,
			"docs_already_imported":    skipDocs,
			"docs_pending_verification": pendingDocs,
		})
	}
	fmt.Fprintf(out, "Dry run: would create %d folder(s) and %d doc(s); %d folder(s) and %d doc(s) already imported to this destination.\n",
		len(newFolders), len(newDocs), skipFolders, skipDocs)
	for _, d := range newFolders {
		fmt.Fprintf(out, "  folder: %s\n", d)
	}
	for _, p := range newDocs {
		fmt.Fprintf(out, "  doc:    %s\n", p)
	}
	for _, p := range pendingDocs {
		fmt.Fprintf(out, "  doc (needs verification — interrupted earlier): %s\n", p)
	}
	return nil
}

// loadImportManifest reads the de-dup manifest. A MISSING file is a fresh import
// (empty maps). A PRESENT-but-corrupt file is an error, NOT silently reset to
// empty — resetting would treat every already-created doc as new and duplicate
// the whole import.
func loadImportManifest(path string) (importManifest, error) {
	// Initialize ALL maps here (not only after a successful read): the
	// missing-manifest path returns m early, and a fresh `comment docs import`
	// then writes manifest.Pending[...] — a nil Pending map would panic.
	m := importManifest{Folders: map[string]string{}, Docs: map[string]string{}, Pending: map[string]bool{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return m, nil
		}
		return m, fmt.Errorf("read import manifest %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("import manifest %s is corrupt (%w) — inspect or remove it before re-importing so already-created docs aren't duplicated", path, err)
	}
	if m.Folders == nil {
		m.Folders = map[string]string{}
	}
	if m.Docs == nil {
		m.Docs = map[string]string{}
	}
	if m.Pending == nil {
		m.Pending = map[string]bool{}
	}
	return m, nil
}

func saveImportManifest(path string, m importManifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	// Atomic write: a temp file in the same dir, fsynced, then renamed over the
	// target. The manifest is rewritten after every item, so an interrupted or
	// failed write must leave the PREVIOUS good manifest intact (a corrupt one
	// blocks resume / forces deletion, which would duplicate created docs).
	tmp, err := os.CreateTemp(filepath.Dir(path), ".comment-import-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// postDocsFolder creates one Library folder (POST /docs/folders), returning its
// id (lf_…). Idempotent server-side: re-creating the same folder returns 200.
func postDocsFolder(ctx context.Context, baseURL, agentSecret, name, parentFolderID string) (string, error) {
	target := map[string]any{"kind": "personal"}
	if parentFolderID != "" {
		target["parentFolderId"] = parentFolderID
	}
	payload, err := json.Marshal(map[string]any{"name": name, "library_target": target})
	if err != nil {
		return "", err
	}
	body, status, err := doDocsRequest(ctx, baseURL, agentSecret, "/docs/folders", payload)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return "", fmt.Errorf("HTTP %d %s", status, strings.TrimSpace(string(body)))
	}
	var parsed struct {
		Node struct {
			ID string `json:"id"`
		} `json:"node"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.Node.ID == "" {
		return "", fmt.Errorf("folder created but response had no node id: %s", strings.TrimSpace(string(body)))
	}
	return parsed.Node.ID, nil
}

// postDocsCreateInTarget creates one doc placed in the given folder (or the
// personal root when parentFolderID is "").
// postDocsCreateInTarget creates one doc. The bool return is definitelyNotCreated:
// true only when the server responded with a 4xx (it rejected the request, so no
// doc exists — safe to clear the in-flight marker and retry). For a transport
// error, a 5xx, or a parse failure after a 2xx, it is false: the create may have
// taken effect, so the marker must be kept and surfaced rather than risk a
// duplicate on retry (POST /docs is not idempotent).
func postDocsCreateInTarget(ctx context.Context, baseURL, agentSecret, markdown, parentFolderID string) (docsCreateResponse, bool, error) {
	// Always send a personal target — even at the root (no parentFolderId) — so the
	// doc lands in My Files. Omitting it leaves an ownerless/bot profile's doc with
	// no Library placement while the manifest marks it imported.
	target := map[string]any{"kind": "personal"}
	if parentFolderID != "" {
		target["parentFolderId"] = parentFolderID
	}
	body := map[string]any{"markdown": markdown, "library_target": target}
	payload, err := json.Marshal(body)
	if err != nil {
		return docsCreateResponse{}, true, err // never sent → no doc created
	}
	raw, status, err := doDocsRequest(ctx, baseURL, agentSecret, "/docs", payload)
	if err != nil {
		// A request that was never dispatched (e.g. malformed URL) created
		// nothing — safe to clear the marker and retry. A transport error after
		// dispatch is ambiguous (keep the marker).
		return docsCreateResponse{}, errors.Is(err, errDocsRequestNotSent), err
	}
	if status != http.StatusCreated && status != http.StatusAccepted {
		// A 4xx is a definitive rejection (no doc); a 5xx is ambiguous.
		definitelyNotCreated := status >= 400 && status < 500
		return docsCreateResponse{}, definitelyNotCreated, fmt.Errorf("HTTP %d %s", status, strings.TrimSpace(string(raw)))
	}
	var parsed docsCreateResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		// 2xx: the doc WAS created but we couldn't read its id — keep the marker.
		return docsCreateResponse{}, false, fmt.Errorf("create succeeded but the response could not be parsed: %w", err)
	}
	if parsed.ID == "" {
		// A 2xx with no id: the doc may exist but we have no slug to record or
		// recover it. Treat as ambiguous (keep the marker) rather than record an
		// empty slug that would make later runs skip the file forever.
		return docsCreateResponse{}, false, errors.New("create returned no document id")
	}
	if parsed.LibraryRepair != nil && parsed.LibraryRepair.State == "repair_enqueue_failed" {
		// 202: the doc was created but its folder placement failed and could not
		// be enqueued for repair — it is orphaned at the Library root, not in the
		// import's folder. Surface it (keep the in-flight marker) rather than
		// record it as cleanly imported in its intended folder.
		return parsed, false, fmt.Errorf("created but Library folder placement failed (repair %s); the doc is at your root, not in the imported folder", parsed.LibraryRepair.RepairID)
	}
	return parsed, false, nil
}

// doDocsRequest POSTs a JSON payload to a Comment.io API path and returns the
// body + status. Shared by the folder and doc creation calls.
// errDocsRequestNotSent marks a failure that occurred BEFORE the request left
// the process (e.g. a malformed URL), so the server definitely created nothing
// and a create can be safely retried after the cause is fixed.
var errDocsRequestNotSent = errors.New("request not sent")

// jsTrim trims the way JavaScript String.prototype.trim() does, which the
// /docs/folders handler uses to derive a folder's node id: Unicode whitespace
// PLUS U+FEFF (BOM/zero-width no-break space). Go's strings.TrimSpace does not
// strip U+FEFF, so matching it here keeps the collision preflight in sync.
func jsTrim(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || r == '\ufeff'
	})
}

// checkFolderTrimCollisions rejects sibling folders whose basenames collapse to
// the same value once the API trims surrounding whitespace (so both would map to
// one server folder and silently merge).
func checkFolderTrimCollisions(dirs []string) error {
	// key = parent + "\x00" + trimmed-basename -> the original basename seen.
	seen := map[string]string{}
	for _, d := range dirs {
		base := baseName(d)
		key := parentDir(d) + "\x00" + jsTrim(base)
		if prev, ok := seen[key]; ok && prev != base {
			return fmt.Errorf("folder names %q and %q under the same parent collapse to the same Library folder after trimming; rename one before importing", prev, base)
		}
		seen[key] = base
	}
	return nil
}

func doDocsRequest(ctx context.Context, baseURL, agentSecret, path string, payload []byte) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(baseURL, "/")+path, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %v", errDocsRequestNotSent, err)
	}
	// http.NewRequest accepts a non-HTTP base URL (e.g. ftp://… or a relative
	// URL) that client.Do then rejects before sending anything. Reject it here as
	// not-sent so an import retry can re-create the doc after the URL is fixed.
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return nil, 0, fmt.Errorf("%w: unsupported base URL scheme %q", errDocsRequestNotSent, req.URL.Scheme)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Comment-CLI-Version", version)
	if agentSecret != "" {
		req.Header.Set("Authorization", "Bearer "+agentSecret)
	}
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}
