package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

func writeDocsCreateMarkdownFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notes.md")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeDocsCreateProfile(t *testing.T, home string, handle string, baseURL string) string {
	t.Helper()
	path := filepath.Join(home, "agents", handle+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]string{
		"handle":       handle,
		"agent_secret": "as_secret_" + handle,
		"base_url":     baseURL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeDocsCreateBotletsRegistry(t *testing.T, home string, botletsHome string, botName string, handle string, profilePath string) {
	t.Helper()
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := commentbus.WriteBusConfig(paths, commentbus.BusConfig{BotletsHome: botletsHome}); err != nil {
		t.Fatal(err)
	}
	writeDocsCreateBotletsRegistryFile(t, botletsHome, botName, handle, profilePath)
}

func writeDocsCreateBotletsRegistryFile(t *testing.T, botletsHome string, botName string, handle string, profilePath string) {
	t.Helper()
	if err := os.MkdirAll(botletsHome, 0o700); err != nil {
		t.Fatal(err)
	}
	registry := map[string]any{
		"bots": []map[string]any{{
			"name":               botName,
			"handle":             handle,
			"credential_profile": profilePath,
			"managed_session": map[string]any{
				"enabled": true,
				"runtime": "claude",
				"host":    "tmux",
			},
		}},
	}
	data, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(botletsHome, "registry.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

type docsCreateServerCall struct {
	Authorization string
	Markdown      string
}

func newDocsCreateServer(t *testing.T, status int, response map[string]any) (*httptest.Server, *docsCreateServerCall) {
	t.Helper()
	call := &docsCreateServerCall{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/docs" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		call.Authorization = r.Header.Get("Authorization")
		var body struct {
			Markdown string `json:"markdown"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode create body: %v", err)
		}
		call.Markdown = body.Markdown
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Errorf("encode create response: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	return server, call
}

func TestParseDocsCreateArgs(t *testing.T) {
	if _, err := parseDocsCreateArgs(nil); err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("missing file error = %v", err)
	}
	if _, err := parseDocsCreateArgs([]string{"a.md", "b.md"}); err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("extra positional error = %v", err)
	}
	if _, err := parseDocsCreateArgs([]string{"a.md", "--profile", "max.tester", "--anonymous"}); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("profile+anonymous error = %v", err)
	}
	if _, err := parseDocsCreateArgs([]string{"a.md", "--profile", "Not A Handle"}); err == nil || !strings.Contains(err.Error(), "invalid profile") {
		t.Fatalf("invalid profile error = %v", err)
	}
	options, err := parseDocsCreateArgs([]string{"--json", "a.md", "--profile", "@max.tester"})
	if err != nil {
		t.Fatal(err)
	}
	if options.File != "a.md" || options.Profile != "max.tester" || !options.JSON {
		t.Fatalf("options = %+v", options)
	}
}

func TestDocsCreateAnonymous(t *testing.T) {
	server, call := newDocsCreateServer(t, http.StatusCreated, map[string]any{
		"id":                "abc123",
		"title":             "Hello",
		"revision":          1,
		"access_token":      "tok_anon",
		"share_url":         "/d/abc123?token=tok_anon",
		"identify_required": true,
		"identify_url":      "/agents/identify",
	})
	file := writeDocsCreateMarkdownFile(t, "# Hello\n\nBody.\n")
	var out bytes.Buffer
	err := docsCreateCommand(docsCreateOptions{
		Home:      filepath.Join(t.TempDir(), ".comment-io"),
		BaseURL:   server.URL,
		Anonymous: true,
		File:      file,
	}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if call.Authorization != "" {
		t.Fatalf("anonymous create sent Authorization %q", call.Authorization)
	}
	if call.Markdown != "# Hello\n\nBody.\n" {
		t.Fatalf("markdown = %q", call.Markdown)
	}
	output := out.String()
	for _, want := range []string{
		`Created "Hello" (abc123) anonymously`,
		"Share URL: " + server.URL + "/d/abc123?token=tok_anon",
		"Access token: tok_anon",
		"POST " + server.URL + "/agents/identify",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
}

func TestDocsCreateWithExplicitProfile(t *testing.T) {
	server, call := newDocsCreateServer(t, http.StatusCreated, map[string]any{
		"id":        "abc123",
		"title":     "Hello",
		"share_url": "/d/abc123?token=tok",
	})
	home := filepath.Join(t.TempDir(), ".comment-io")
	writeDocsCreateProfile(t, home, "max.tester", server.URL)
	writeDocsCreateProfile(t, home, "max.other", server.URL)
	file := writeDocsCreateMarkdownFile(t, "# Hello\n")
	var out bytes.Buffer
	err := docsCreateCommand(docsCreateOptions{
		Home:    home,
		Profile: "max.tester",
		File:    file,
	}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if call.Authorization != "Bearer as_secret_max.tester" {
		t.Fatalf("Authorization = %q", call.Authorization)
	}
	output := out.String()
	if !strings.Contains(output, "as @max.tester") {
		t.Fatalf("output missing profile identity:\n%s", output)
	}
	if strings.Contains(output, "Access token") {
		t.Fatalf("registered create should not print the access token:\n%s", output)
	}
}

func TestDocsCreateAutoSelectsSingleProfile(t *testing.T) {
	server, call := newDocsCreateServer(t, http.StatusCreated, map[string]any{
		"id":    "abc123",
		"title": "Hello",
	})
	home := filepath.Join(t.TempDir(), ".comment-io")
	writeDocsCreateProfile(t, home, "max.tester", server.URL)
	file := writeDocsCreateMarkdownFile(t, "# Hello\n")
	var out bytes.Buffer
	if err := docsCreateCommand(docsCreateOptions{Home: home, File: file}, &out); err != nil {
		t.Fatal(err)
	}
	if call.Authorization != "Bearer as_secret_max.tester" {
		t.Fatalf("Authorization = %q", call.Authorization)
	}
}

func TestDocsCreateDoesNotAutoSelectSingleBotletsProfile(t *testing.T) {
	server, call := newDocsCreateServer(t, http.StatusCreated, map[string]any{
		"id":                "abc123",
		"title":             "Hello",
		"access_token":      "tok_anon",
		"share_url":         "/d/abc123?token=tok_anon",
		"identify_required": true,
		"identify_url":      "/agents/identify",
	})
	home := filepath.Join(t.TempDir(), ".comment-io")
	profilePath := writeDocsCreateProfile(t, home, "max.default", server.URL)
	writeDocsCreateBotletsRegistry(t, home, filepath.Join(t.TempDir(), "botlets"), "default", "max.default", profilePath)
	file := writeDocsCreateMarkdownFile(t, "# Hello\n")
	var out bytes.Buffer
	if err := docsCreateCommand(docsCreateOptions{Home: home, BaseURL: server.URL, File: file}, &out); err != nil {
		t.Fatal(err)
	}
	if call.Authorization != "" {
		t.Fatalf("Authorization = %q, want anonymous create", call.Authorization)
	}
	if !strings.Contains(out.String(), `Created "Hello" (abc123) anonymously`) {
		t.Fatalf("output missing anonymous identity:\n%s", out.String())
	}
}

func TestDocsCreateAutoSelectsSingleNonBotletsProfile(t *testing.T) {
	server, call := newDocsCreateServer(t, http.StatusCreated, map[string]any{
		"id":    "abc123",
		"title": "Hello",
	})
	home := filepath.Join(t.TempDir(), ".comment-io")
	botProfilePath := writeDocsCreateProfile(t, home, "max.default", server.URL)
	writeDocsCreateProfile(t, home, "max.worker", server.URL)
	writeDocsCreateBotletsRegistry(t, home, filepath.Join(t.TempDir(), "botlets"), "default", "max.default", botProfilePath)
	file := writeDocsCreateMarkdownFile(t, "# Hello\n")
	var out bytes.Buffer
	if err := docsCreateCommand(docsCreateOptions{Home: home, File: file}, &out); err != nil {
		t.Fatal(err)
	}
	if call.Authorization != "Bearer as_secret_max.worker" {
		t.Fatalf("Authorization = %q", call.Authorization)
	}
	if !strings.Contains(out.String(), "as @max.worker") {
		t.Fatalf("output missing non-Botlets profile identity:\n%s", out.String())
	}
}

func TestDocsCreateIgnoresDefaultBotletsHomeWithoutConfig(t *testing.T) {
	server, call := newDocsCreateServer(t, http.StatusCreated, map[string]any{
		"id":    "abc123",
		"title": "Hello",
	})
	home := filepath.Join(t.TempDir(), ".comment-io")
	profilePath := writeDocsCreateProfile(t, home, "max.default", server.URL)
	unrelatedBotletsHome := filepath.Join(t.TempDir(), "unrelated-botlets")
	writeDocsCreateBotletsRegistryFile(t, unrelatedBotletsHome, "default", "max.default", profilePath)
	t.Setenv("BOTLETS_HOME", unrelatedBotletsHome)
	file := writeDocsCreateMarkdownFile(t, "# Hello\n")
	var out bytes.Buffer
	if err := docsCreateCommand(docsCreateOptions{Home: home, File: file}, &out); err != nil {
		t.Fatal(err)
	}
	if call.Authorization != "Bearer as_secret_max.default" {
		t.Fatalf("Authorization = %q", call.Authorization)
	}
	if !strings.Contains(out.String(), "as @max.default") {
		t.Fatalf("output missing profile identity:\n%s", out.String())
	}
}

func TestDocsCreateImplicitSelectionFailsWhenBotletsConfigInvalid(t *testing.T) {
	server, call := newDocsCreateServer(t, http.StatusCreated, map[string]any{
		"id":    "abc123",
		"title": "Hello",
	})
	home := filepath.Join(t.TempDir(), ".comment-io")
	writeDocsCreateProfile(t, home, "max.default", server.URL)
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.Bus, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Bus, "config.json"), []byte("{not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file := writeDocsCreateMarkdownFile(t, "# Hello\n")
	err = docsCreateCommand(docsCreateOptions{Home: home, File: file}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected invalid Botlets config to block implicit profile selection")
	}
	if !strings.Contains(err.Error(), "could not load Botlets config") || !strings.Contains(err.Error(), "--profile") || !strings.Contains(err.Error(), "--anonymous") {
		t.Fatalf("error missing guidance: %v", err)
	}
	if call.Authorization != "" || call.Markdown != "" {
		t.Fatalf("create request should not have been sent: %+v", call)
	}
}

func TestDocsCreateMultipleProfilesRequireChoice(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".comment-io")
	writeDocsCreateProfile(t, home, "max.tester", "https://comment.example")
	writeDocsCreateProfile(t, home, "max.other", "https://comment.example")
	file := writeDocsCreateMarkdownFile(t, "# Hello\n")
	err := docsCreateCommand(docsCreateOptions{Home: home, File: file}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected error for ambiguous profiles")
	}
	for _, want := range []string{"max.other", "max.tester", "--profile", "--anonymous"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q: %v", want, err)
		}
	}
}

func TestDocsCreateCorruptProfileDoesNotFallBackToAnonymous(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".comment-io")
	agentsDir := filepath.Join(home, "agents")
	if err := os.MkdirAll(agentsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "max.tester.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	file := writeDocsCreateMarkdownFile(t, "# Hello\n")
	err := docsCreateCommand(docsCreateOptions{Home: home, File: file}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected error instead of silent anonymous fallback")
	}
	for _, want := range []string{"could not load agent profiles", "INVALID_AGENT_PROFILE", "--anonymous"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q: %v", want, err)
		}
	}
}

func TestDocsCreateUsesServerAPIURL(t *testing.T) {
	server, _ := newDocsCreateServer(t, http.StatusCreated, map[string]any{
		"id":      "abc123",
		"title":   "Hello",
		"api_url": "/docs/abc123",
	})
	file := writeDocsCreateMarkdownFile(t, "# Hello\n")
	var out bytes.Buffer
	err := docsCreateCommand(docsCreateOptions{
		Home:      filepath.Join(t.TempDir(), ".comment-io"),
		BaseURL:   server.URL,
		Anonymous: true,
		File:      file,
	}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "API URL:   "+server.URL+"/docs/abc123") {
		t.Fatalf("output missing server api_url:\n%s", out.String())
	}
}

func TestDocsCreateAnonymousFallbackWithoutProfiles(t *testing.T) {
	server, call := newDocsCreateServer(t, http.StatusCreated, map[string]any{
		"id":    "abc123",
		"title": "Hello",
	})
	file := writeDocsCreateMarkdownFile(t, "# Hello\n")
	var out bytes.Buffer
	err := docsCreateCommand(docsCreateOptions{
		Home:    filepath.Join(t.TempDir(), ".comment-io"),
		BaseURL: server.URL,
		File:    file,
	}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if call.Authorization != "" {
		t.Fatalf("Authorization = %q", call.Authorization)
	}
	if !strings.Contains(out.String(), "anonymously") {
		t.Fatalf("output missing anonymous identity:\n%s", out.String())
	}
}

func TestDocsCreateProfileBaseURLMismatch(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".comment-io")
	writeDocsCreateProfile(t, home, "max.tester", "https://comment.example")
	file := writeDocsCreateMarkdownFile(t, "# Hello\n")
	err := docsCreateCommand(docsCreateOptions{
		Home:    home,
		Profile: "max.tester",
		BaseURL: "https://elsewhere.example",
		File:    file,
	}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "--base-url must match profile") {
		t.Fatalf("base URL mismatch error = %v", err)
	}
}

func TestDocsCreateEmptyFile(t *testing.T) {
	file := writeDocsCreateMarkdownFile(t, "  \n\n")
	err := docsCreateCommand(docsCreateOptions{
		Home:      filepath.Join(t.TempDir(), ".comment-io"),
		Anonymous: true,
		File:      file,
	}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty file error = %v", err)
	}
}

func TestDocsCreateMissingFile(t *testing.T) {
	err := docsCreateCommand(docsCreateOptions{
		Home:      filepath.Join(t.TempDir(), ".comment-io"),
		Anonymous: true,
		File:      filepath.Join(t.TempDir(), "missing.md"),
	}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestDocsCreateServerError(t *testing.T) {
	server, _ := newDocsCreateServer(t, http.StatusBadRequest, map[string]any{
		"error": "UNEXPECTED_FIELD",
	})
	file := writeDocsCreateMarkdownFile(t, "# Hello\n")
	err := docsCreateCommand(docsCreateOptions{
		Home:      filepath.Join(t.TempDir(), ".comment-io"),
		BaseURL:   server.URL,
		Anonymous: true,
		File:      file,
	}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "HTTP 400") || !strings.Contains(err.Error(), "UNEXPECTED_FIELD") {
		t.Fatalf("server error = %v", err)
	}
}

func TestDocsCreateAcceptsLibraryRepairStatus(t *testing.T) {
	server, _ := newDocsCreateServer(t, http.StatusAccepted, map[string]any{
		"id":    "abc123",
		"title": "Hello",
	})
	file := writeDocsCreateMarkdownFile(t, "# Hello\n")
	if err := docsCreateCommand(docsCreateOptions{
		Home:      filepath.Join(t.TempDir(), ".comment-io"),
		BaseURL:   server.URL,
		Anonymous: true,
		File:      file,
	}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
}

func TestDocsCreateJSONOutput(t *testing.T) {
	server, _ := newDocsCreateServer(t, http.StatusCreated, map[string]any{
		"id":           "abc123",
		"title":        "Hello",
		"access_token": "tok_anon",
	})
	file := writeDocsCreateMarkdownFile(t, "# Hello\n")
	var out bytes.Buffer
	err := docsCreateCommand(docsCreateOptions{
		Home:      filepath.Join(t.TempDir(), ".comment-io"),
		BaseURL:   server.URL,
		Anonymous: true,
		JSON:      true,
		File:      file,
	}, &out)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\n%s", err, out.String())
	}
	if parsed["id"] != "abc123" || parsed["access_token"] != "tok_anon" {
		t.Fatalf("json output = %v", parsed)
	}
}
