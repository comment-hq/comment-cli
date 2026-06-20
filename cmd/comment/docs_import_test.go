package main

import (
	"bytes"
	"context"
	"errors"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParseDocsImportArgs(t *testing.T) {
	if _, err := parseDocsImportArgs(nil); err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("missing dir error = %v", err)
	}
	if _, err := parseDocsImportArgs([]string{"a", "b"}); err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("extra positional error = %v", err)
	}
	if _, err := parseDocsImportArgs([]string{"vault", "--profile", "Not A Handle"}); err == nil || !strings.Contains(err.Error(), "invalid profile") {
		t.Fatalf("invalid profile error = %v", err)
	}
	opts, err := parseDocsImportArgs([]string{"--dry-run", "vault", "--profile", "@max.tester"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Dir != "vault" || opts.Profile != "max.tester" || !opts.DryRun {
		t.Fatalf("opts = %+v", opts)
	}
}

func TestScanImportDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "root.md", "# Root")
	writeFile(t, dir, "A/note.md", "# A note")
	writeFile(t, dir, "A/B/deep.md", "# Deep")
	writeFile(t, dir, "A/image.png", "binary") // non-md, skipped
	writeFile(t, dir, ".obsidian/cfg.md", "x") // hidden dir, skipped
	writeFile(t, dir, "A/.hidden.md", "x")     // hidden file, skipped

	entries, dirs, err := scanImportDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, e := range entries {
		paths = append(paths, e.relPath)
	}
	// A symlinked .md (e.g. pointing outside the vault) must be skipped, never
	// followed and uploaded.
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("ssh key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "leak.md")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	entries, dirs, err = scanImportDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	paths = nil
	for _, e := range entries {
		paths = append(paths, e.relPath)
	}
	want := []string{"A/B/deep.md", "A/note.md", "root.md"} // no "leak.md"
	if fmt.Sprint(paths) != fmt.Sprint(want) {
		t.Fatalf("entries = %v, want %v (symlink must be skipped)", paths, want)
	}
	// dirs are parent-before-child: "A" before "A/B"
	if fmt.Sprint(dirs) != fmt.Sprint([]string{"A", "A/B"}) {
		t.Fatalf("dirs = %v", dirs)
	}
}

// importTestServer handles POST /docs/folders and POST /docs, minting ids and
// recording calls. Folder creation is idempotent by (parent,name).
type importTestServer struct {
	mu          sync.Mutex
	folderByKey map[string]string
	folderCalls int
	docCalls    int
	docTargets  []string // parentFolderId per doc create
}

func newImportTestServer(t *testing.T) (*httptest.Server, *importTestServer) {
	t.Helper()
	st := &importTestServer{folderByKey: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/docs/folders":
			var body struct {
				Name          string `json:"name"`
				LibraryTarget struct {
					ParentFolderID string `json:"parentFolderId"`
				} `json:"library_target"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			st.folderCalls++
			key := body.LibraryTarget.ParentFolderID + "/" + body.Name
			id, ok := st.folderByKey[key]
			status := http.StatusCreated
			if ok {
				status = http.StatusOK // idempotent re-create
			} else {
				id = fmt.Sprintf("lf_%d", len(st.folderByKey)+1)
				st.folderByKey[key] = id
			}
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "node": map[string]any{"id": id}})
		case "/docs":
			var body struct {
				LibraryTarget *struct {
					ParentFolderID string `json:"parentFolderId"`
				} `json:"library_target"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			st.docCalls++
			target := ""
			if body.LibraryTarget != nil {
				target = body.LibraryTarget.ParentFolderID
			}
			st.docTargets = append(st.docTargets, target)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": fmt.Sprintf("slug%d", st.docCalls), "title": "t", "revision": 1})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, st
}

func TestDocsImportEndToEndAndIdempotentRerun(t *testing.T) {
	vault := t.TempDir()
	writeFile(t, vault, "root.md", "# Root")
	writeFile(t, vault, "A/note.md", "# A note")
	writeFile(t, vault, "A/B/deep.md", "# Deep")

	srv, st := newImportTestServer(t)
	home := filepath.Join(t.TempDir(), ".comment-io")
	writeDocsCreateProfile(t, home, "max.tester", srv.URL)
	opts := docsImportOptions{
		Home:    home,
		Profile: "max.tester",
		Dir:     vault,
	}

	var out bytes.Buffer
	if err := docsImportCommand(context.Background(), opts, &out); err != nil {
		t.Fatal(err)
	}
	if st.folderCalls != 2 { // A, A/B
		t.Fatalf("folder calls = %d, want 2", st.folderCalls)
	}
	if st.docCalls != 3 {
		t.Fatalf("doc calls = %d, want 3", st.docCalls)
	}
	// Exactly one doc (root.md) lands in the personal root; the other two land in
	// two distinct folders (A and A/B). Order-independent (entries are sorted).
	rootCount := 0
	nonRoot := map[string]bool{}
	for _, tgt := range st.docTargets {
		if tgt == "" {
			rootCount++
		} else {
			nonRoot[tgt] = true
		}
	}
	if rootCount != 1 {
		t.Fatalf("docs at personal root = %d, want 1", rootCount)
	}
	if len(nonRoot) != 2 {
		t.Fatalf("distinct non-root folders = %d, want 2 (A, A/B)", len(nonRoot))
	}
	// Manifest written (destination-scoped filename).
	if matches, _ := filepath.Glob(filepath.Join(vault, ".comment-import.*.json")); len(matches) == 0 {
		t.Fatal("manifest not written")
	}

	// Re-run: everything already recorded → no new doc creates.
	docsBefore := st.docCalls
	var out2 bytes.Buffer
	if err := docsImportCommand(context.Background(), opts, &out2); err != nil {
		t.Fatal(err)
	}
	if st.docCalls != docsBefore {
		t.Fatalf("re-run created %d new docs, want 0 (idempotent)", st.docCalls-docsBefore)
	}
	if !strings.Contains(out2.String(), "3 skipped") {
		t.Fatalf("re-run output = %q, want 3 skipped", out2.String())
	}
}

func TestDocsImportFailsAndDoesNotRehomeWhenFolderFails(t *testing.T) {
	vault := t.TempDir()
	writeFile(t, vault, "A/note.md", "# A note")
	writeFile(t, vault, "A/B/deep.md", "# Deep")

	// /docs/folders always fails; a doc must NEVER be created without its folder.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/docs/folders" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"nope"}`))
			return
		}
		t.Errorf("a doc was created at %s despite its folder failing (silent re-home)", r.URL.Path)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)
	home := filepath.Join(t.TempDir(), ".comment-io")
	writeDocsCreateProfile(t, home, "max.tester", srv.URL)

	var out bytes.Buffer
	err := docsImportCommand(context.Background(), docsImportOptions{Home: home, Profile: "max.tester", Dir: vault}, &out)
	if err == nil {
		t.Fatal("expected a non-zero error when items fail to import")
	}
}

func TestDocsImportRequiresRegisteredAgent(t *testing.T) {
	vault := t.TempDir()
	writeFile(t, vault, "a.md", "# A")
	// empty home → no profiles → import must not proceed anonymously
	home := filepath.Join(t.TempDir(), ".comment-io")
	var out bytes.Buffer
	err := docsImportCommand(context.Background(), docsImportOptions{Home: home, BaseURL: "http://fake", Dir: vault}, &out)
	if err == nil {
		t.Fatal("expected import to be rejected without a registered agent")
	}
}

func TestDocsImportRejectsCorruptManifest(t *testing.T) {
	vault := t.TempDir()
	writeFile(t, vault, "a.md", "# A")
	home := filepath.Join(t.TempDir(), ".comment-io")
	writeDocsCreateProfile(t, home, "max.tester", "http://fake")
	// a corrupt manifest (at the destination-scoped path) must error, not reset
	if err := os.WriteFile(importManifestPath(vault, "http://fake", "max.tester"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := docsImportCommand(context.Background(), docsImportOptions{Home: home, Profile: "max.tester", Dir: vault}, &out)
	if err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("expected corrupt-manifest error, got %v", err)
	}
}

func TestDocsImportDryRun(t *testing.T) {
	vault := t.TempDir()
	writeFile(t, vault, "A/note.md", "# A note")
	home := filepath.Join(t.TempDir(), ".comment-io")
	writeDocsCreateProfile(t, home, "max.tester", "http://fake")
	var out bytes.Buffer
	if err := docsImportCommand(context.Background(), docsImportOptions{Home: home, Profile: "max.tester", Dir: vault, DryRun: true}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Dry run") || !strings.Contains(out.String(), "A/note.md") {
		t.Fatalf("dry run output = %q", out.String())
	}
}

func TestDocsImportDryRunJSON(t *testing.T) {
	vault := t.TempDir()
	writeFile(t, vault, "A/note.md", "# A note")
	home := filepath.Join(t.TempDir(), ".comment-io")
	writeDocsCreateProfile(t, home, "max.tester", "http://fake")
	var out bytes.Buffer
	if err := docsImportCommand(context.Background(), docsImportOptions{Home: home, Profile: "max.tester", Dir: vault, DryRun: true, JSON: true}, &out); err != nil {
		t.Fatal(err)
	}
	var got struct {
		DryRun          bool     `json:"dry_run"`
		FoldersToCreate []string `json:"folders_to_create"`
		DocsToCreate    []string `json:"docs_to_create"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("--dry-run --json did not emit JSON: %v (%q)", err, out.String())
	}
	if !got.DryRun || fmt.Sprint(got.FoldersToCreate) != fmt.Sprint([]string{"A"}) || fmt.Sprint(got.DocsToCreate) != fmt.Sprint([]string{"A/note.md"}) {
		t.Fatalf("dry-run json = %+v", got)
	}
}

func TestImportOwnerScopeSharesManifestAcrossProfiles(t *testing.T) {
	dir := t.TempDir()
	base := "https://comt.dev"
	// Two profiles of the SAME owner must resolve to the SAME manifest file.
	if importManifestPath(dir, base, "max.codex") != importManifestPath(dir, base, "max.claude") {
		t.Fatal("same-owner profiles must share one import manifest")
	}
	// A bare handle is its own owner.
	if importOwnerScope("max") != "max" {
		t.Fatalf("bare handle owner = %q, want max", importOwnerScope("max"))
	}
	// Different owners must NOT collide.
	if importManifestPath(dir, base, "max.codex") == importManifestPath(dir, base, "jon.codex") {
		t.Fatal("different owners must not share a manifest")
	}
}

func TestImportDryRunEmitsEmptyArraysNotNull(t *testing.T) {
	var buf bytes.Buffer
	manifest := importManifest{Folders: map[string]string{}, Docs: map[string]string{}}
	if err := reportImportDryRun(&buf, true, manifest, nil, nil); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `"folders_to_create": []`) || !strings.Contains(out, `"docs_to_create": []`) {
		t.Fatalf("empty dry-run must emit [] arrays, got:\n%s", out)
	}
}

func TestImportPendingMarkerSurfacesInterruptedCreate(t *testing.T) {
	// Simulate a manifest left with an in-flight (pending) create — a prior run
	// killed after POST started but before recording. loadImportManifest must
	// preserve it so the import flow can surface it rather than re-create.
	dir := t.TempDir()
	path := importManifestPath(dir, "https://comt.dev", "max.codex")
	m := importManifest{
		Folders: map[string]string{},
		Docs:    map[string]string{"done.md": "slug1"},
		Pending: map[string]bool{"maybe.md": true},
	}
	if err := saveImportManifest(path, m); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := loadImportManifest(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !loaded.Pending["maybe.md"] {
		t.Fatal("pending in-flight create must survive a manifest round-trip")
	}
	if loaded.Docs["done.md"] != "slug1" {
		t.Fatal("recorded docs must survive alongside pending")
	}
}

func TestLoadImportManifestInitializesPendingForFreshImport(t *testing.T) {
	// A missing manifest (fresh import) must return a non-nil Pending map, or the
	// first manifest.Pending[path]=true assignment panics on a nil map.
	dir := t.TempDir()
	m, err := loadImportManifest(importManifestPath(dir, "https://comt.dev", "max.codex"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if m.Pending == nil {
		t.Fatal("Pending must be initialized for a missing manifest")
	}
	m.Pending["x.md"] = true // must not panic
}

func TestImportDryRunSeparatesPendingDocs(t *testing.T) {
	var buf bytes.Buffer
	manifest := importManifest{
		Folders: map[string]string{},
		Docs:    map[string]string{},
		Pending: map[string]bool{"maybe.md": true},
	}
	entries := []importEntry{{relPath: "maybe.md"}, {relPath: "fresh.md"}}
	if err := reportImportDryRun(&buf, true, manifest, nil, entries); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `"docs_pending_verification": [`) || !strings.Contains(out, "maybe.md") {
		t.Fatalf("pending doc must be reported under docs_pending_verification, got:\n%s", out)
	}
	// The pending file must NOT appear in docs_to_create (only fresh.md should).
	if strings.Contains(out, `"docs_to_create": [`+"\n    \"maybe.md\"") {
		t.Fatalf("pending doc must not be listed as to_create, got:\n%s", out)
	}
}

func TestCheckFolderTrimCollisions(t *testing.T) {
	if err := checkFolderTrimCollisions([]string{"Notes", "Notes ", "Other"}); err == nil {
		t.Fatal("sibling folders that collapse after trimming must be rejected")
	}
	if err := checkFolderTrimCollisions([]string{"Notes", "a/Notes", "Other"}); err != nil {
		t.Fatalf("same trimmed name under DIFFERENT parents is fine, got %v", err)
	}
}

func TestJSTrimStripsBOM(t *testing.T) {
	if jsTrim("Notes\ufeff") != "Notes" || jsTrim("  x  ") != "x" {
		t.Fatalf("jsTrim must strip BOM + whitespace like JS trim")
	}
	// Sibling folders differing only by a BOM must collide.
	if err := checkFolderTrimCollisions([]string{"Notes", "Notes\ufeff"}); err == nil {
		t.Fatal("BOM-only folder difference must be rejected (matches server trim)")
	}
}

func TestDoDocsRequestRejectsNonHTTPScheme(t *testing.T) {
	_, _, err := doDocsRequest(context.Background(), "ftp://example.com", "secret", "/docs", []byte("{}"))
	if err == nil || !errors.Is(err, errDocsRequestNotSent) {
		t.Fatalf("non-HTTP scheme must be errDocsRequestNotSent, got %v", err)
	}
}
