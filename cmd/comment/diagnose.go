package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

// Files included in the diagnostics bundle. Order is deterministic so tests
// and human reviewers see the same layout every time.
type bundleEntry struct {
	// archive name within the tarball
	name string
	// absolute path on disk; empty for synthetic entries
	source string
	// raw bytes for synthetic entries (e.g. system.json)
	body []byte
	// whether absence is OK (true for logs that haven't been written yet)
	optional bool
}

const (
	maxUploadBytes        = 25 * 1024 * 1024
	uploadResponseTimeout = 30 * time.Second
)

func runDiagnose(args []string) error {
	fs := flag.NewFlagSet("comment diagnose", flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	output := fs.String("output", "", "Write bundle to this path instead of ~/.comment-io/diagnostics/")
	includeHistory := fs.Bool("include-history", false, "Include bus/history.sqlite (may contain message content)")
	var includeFiles stringListFlag
	fs.Var(&includeFiles, "include-file", "Include an additional file in the diagnostic bundle")
	uploadFlag := fs.Bool("upload", false, "Upload without prompting")
	noUploadFlag := fs.Bool("no-upload", false, "Skip the upload step entirely")
	baseURL := fs.String("base-url", "", "Override the diagnostics upload base URL")
	yes := fs.Bool("yes", false, "Alias for --upload")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	if len(fs.Args()) > 0 {
		return errors.New("diagnose does not accept positional arguments")
	}
	if *uploadFlag && *noUploadFlag {
		return errors.New("--upload and --no-upload are mutually exclusive")
	}

	paths, err := resolveCLIPaths(*home)
	if err != nil {
		return err
	}

	bundleBytes, entries, err := buildDiagnoseBundle(paths, *includeHistory, includeFiles)
	if err != nil {
		return err
	}

	bundlePath, err := writeDiagnoseBundle(paths, *output, bundleBytes)
	if err != nil {
		return err
	}

	printBundleSummary(os.Stdout, entries, bundlePath, len(bundleBytes))

	if *noUploadFlag {
		fmt.Fprintln(os.Stdout, "Skipping upload (--no-upload).")
		return nil
	}

	confirmed := *uploadFlag || *yes
	if !confirmed {
		ok, promptErr := promptYesNo(os.Stdin, os.Stdout, "Send this bundle to Comment.io support?")
		if promptErr != nil {
			return promptErr
		}
		if !ok {
			fmt.Fprintln(os.Stdout, "Not uploaded. Bundle kept at the path above.")
			return nil
		}
	}

	target := resolveDiagnoseBaseURL(*baseURL)
	id, err := uploadDiagnoseBundle(context.Background(), target, bundleBytes)
	if err != nil {
		return fmt.Errorf("upload failed: %w", err)
	}
	fmt.Fprintf(os.Stdout, "Uploaded. Reference ID: %s\n", id)
	return nil
}

func buildDiagnoseBundle(paths commentbus.Paths, includeHistory bool, includeFiles []string) ([]byte, []bundleEntry, error) {
	entries := []bundleEntry{
		{name: "logs/commentd.jsonl", source: filepath.Join(paths.Logs, "commentd.jsonl"), optional: true},
		{name: "logs/commentd.out.log", source: filepath.Join(paths.Logs, "commentd.out.log"), optional: true},
		{name: "logs/commentd.err.log", source: filepath.Join(paths.Logs, "commentd.err.log"), optional: true},
	}
	if includeHistory {
		entries = append(entries, bundleEntry{
			name:     "bus/history.sqlite",
			source:   paths.History,
			optional: true,
		})
	}
	for i, path := range includeFiles {
		entry, err := diagnoseAttachmentEntry(path, i)
		if err != nil {
			return nil, nil, err
		}
		entries = append(entries, entry)
	}

	systemJSON, err := collectSystemInfo(paths, includeHistory)
	if err != nil {
		return nil, nil, err
	}
	entries = append(entries, bundleEntry{name: "system.json", body: systemJSON})

	resolved, err := materializeEntries(entries)
	if err != nil {
		return nil, nil, err
	}

	buf, err := writeTarGz(resolved)
	if err != nil {
		return nil, nil, err
	}
	return buf, resolved, nil
}

func diagnoseAttachmentEntry(path string, index int) (bundleEntry, error) {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		abs, err := filepath.Abs(clean)
		if err != nil {
			return bundleEntry{}, fmt.Errorf("resolve include-file: %w", err)
		}
		clean = abs
	}
	info, err := os.Stat(clean)
	if err != nil {
		return bundleEntry{}, fmt.Errorf("read include-file: %w", err)
	}
	if info.IsDir() {
		return bundleEntry{}, errors.New("include-file must be a file, not a directory")
	}
	name := sanitizeDiagnoseAttachmentName(filepath.Base(clean))
	if name == "" {
		name = "attachment.txt"
	}
	if index > 0 {
		name = fmt.Sprintf("%02d-%s", index+1, name)
	}
	return bundleEntry{name: "attachments/" + name, source: clean}, nil
}

func sanitizeDiagnoseAttachmentName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == string(filepath.Separator) {
		return ""
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.' || r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), ".-")
	if len(out) > 80 {
		out = out[:80]
		out = strings.Trim(out, ".-")
	}
	return out
}

// materializeEntries reads every on-disk source into memory and drops any
// optional entries that don't exist. Synthetic entries (with body set) pass
// through unchanged.
func materializeEntries(entries []bundleEntry) ([]bundleEntry, error) {
	out := make([]bundleEntry, 0, len(entries))
	for _, entry := range entries {
		if len(entry.body) > 0 || entry.source == "" {
			out = append(out, entry)
			continue
		}
		data, err := os.ReadFile(entry.source)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) && entry.optional {
				continue
			}
			return nil, fmt.Errorf("read %s: %w", entry.name, err)
		}
		entry.body = data
		out = append(out, entry)
	}
	return out, nil
}

func writeTarGz(entries []bundleEntry) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	now := time.Now().UTC()
	for _, entry := range entries {
		header := &tar.Header{
			Name:    entry.name,
			Mode:    0o600,
			Size:    int64(len(entry.body)),
			ModTime: now,
		}
		if err := tw.WriteHeader(header); err != nil {
			return nil, err
		}
		if _, err := tw.Write(entry.body); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// collectSystemInfo gathers non-sensitive host metadata for triage. Profile
// handles are included so a support engineer can find the right account; the
// agent secret is deliberately omitted.
func collectSystemInfo(paths commentbus.Paths, includeHistory bool) ([]byte, error) {
	profiles, profileErrors := commentbus.LoadAgentProfiles(context.Background(), paths, "")
	handles := make([]map[string]any, 0, len(profiles))
	for handle, profile := range profiles {
		handles = append(handles, map[string]any{
			"handle":   handle,
			"base_url": profile.BaseURL,
		})
	}
	sort.Slice(handles, func(i, j int) bool {
		return handles[i]["handle"].(string) < handles[j]["handle"].(string)
	})

	profileErrorList := make([]map[string]any, 0, len(profileErrors))
	for _, perr := range profileErrors {
		profileErrorList = append(profileErrorList, map[string]any{
			"code":    perr.Code,
			"message": perr.Message,
		})
	}

	storeInitialized := true
	if _, err := os.Stat(paths.History); errors.Is(err, os.ErrNotExist) {
		storeInitialized = false
	}

	info := map[string]any{
		"collected_at":      time.Now().UTC().Format(time.RFC3339),
		"cli_version":       version,
		"protocol_version":  commentbus.BusProtocolVersion,
		"go_version":        runtime.Version(),
		"os":                runtime.GOOS,
		"arch":              runtime.GOARCH,
		"home":              paths.Home,
		"store_initialized": storeInitialized,
		"include_history":   includeHistory,
		"profiles":          handles,
		"profile_errors":    profileErrorList,
	}
	return json.MarshalIndent(info, "", "  ")
}

func writeDiagnoseBundle(paths commentbus.Paths, output string, body []byte) (string, error) {
	if output == "" {
		dir := filepath.Join(paths.Home, "diagnostics")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", fmt.Errorf("create diagnostics dir: %w", err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return "", err
		}
		name := fmt.Sprintf("comment-diagnose-%s.tar.gz", time.Now().UTC().Format("20060102-150405"))
		output = filepath.Join(dir, name)
	}
	if err := os.WriteFile(output, body, 0o600); err != nil {
		return "", err
	}
	return output, nil
}

func printBundleSummary(w io.Writer, entries []bundleEntry, bundlePath string, compressedSize int) {
	fmt.Fprintln(w, "Diagnostics bundle contents:")
	nameWidth := 0
	for _, entry := range entries {
		if len(entry.name) > nameWidth {
			nameWidth = len(entry.name)
		}
	}
	for _, entry := range entries {
		fmt.Fprintf(w, "  %-*s  %s\n", nameWidth, entry.name, humanBytes(int64(len(entry.body))))
	}
	fmt.Fprintf(w, "\nBundle: %s (%s compressed)\n", bundlePath, humanBytes(int64(compressedSize)))
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for cur := n / unit; cur >= unit; cur /= unit {
		div *= unit
		exp++
	}
	suffix := "KMGTPE"[exp]
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), suffix)
}

func promptYesNo(in io.Reader, out io.Writer, question string) (bool, error) {
	fmt.Fprintf(out, "\n%s [y/N] ", question)
	reader := bufio.NewReader(in)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

func resolveDiagnoseBaseURL(override string) string {
	if override != "" {
		return strings.TrimSuffix(override, "/")
	}
	return commentbus.DefaultBaseURL()
}

func uploadDiagnoseBundle(ctx context.Context, baseURL string, body []byte) (string, error) {
	if len(body) > maxUploadBytes {
		return "", fmt.Errorf("bundle exceeds %d bytes", maxUploadBytes)
	}
	endpoint := strings.TrimSuffix(baseURL, "/") + "/api/diagnostics"
	ctx, cancel := context.WithTimeout(ctx, uploadResponseTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/gzip")
	req.Header.Set("User-Agent", fmt.Sprintf("comment-cli/%s", version))
	req.Header.Set("X-Comment-CLI-Version", version)
	req.Header.Set("X-Comment-CLI-OS-Arch", fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var parsed struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil || parsed.ID == "" {
		return "", fmt.Errorf("invalid server response: %s", strings.TrimSpace(string(respBody)))
	}
	return parsed.ID, nil
}
