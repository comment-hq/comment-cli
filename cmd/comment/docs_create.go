package main

import (
	"bytes"
	"context"
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
	"time"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

type docsCreateOptions struct {
	Home      string
	Profile   string
	BaseURL   string
	Anonymous bool
	JSON      bool
	File      string
}

// docsCreateResponse holds the POST /docs fields the CLI surfaces. The full
// response is preserved separately for --json output.
type docsCreateResponse struct {
	ID               string `json:"id"`
	Title            string `json:"title"`
	AccessToken      string `json:"access_token"`
	APIURL           string `json:"api_url"`
	ShareURL         string `json:"share_url"`
	IdentifyRequired bool   `json:"identify_required"`
	IdentifyURL      string `json:"identify_url"`
	// LibraryRepair is present on a 202 when the doc was created but its Library
	// folder placement could not be completed inline. state 'repair_enqueue_failed'
	// means placement won't happen (the doc is orphaned at the root).
	LibraryRepair *struct {
		RepairID string `json:"repair_id"`
		State    string `json:"state"`
	} `json:"library_repair"`
}

func runDocsCreate(args []string) error {
	options, err := parseDocsCreateArgs(args)
	if err != nil {
		return err
	}
	return docsCreateCommand(options, os.Stdout)
}

func parseDocsCreateArgs(args []string) (docsCreateOptions, error) {
	fs := flag.NewFlagSet("comment docs create", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	options := docsCreateOptions{}
	fs.StringVar(&options.Home, "home", "", "Comment.io home directory")
	fs.StringVar(&options.Profile, "profile", "", "registered agent profile handle to create as")
	fs.StringVar(&options.BaseURL, "base-url", "", "Comment.io API base URL; with a profile it must match the profile's base_url")
	fs.BoolVar(&options.Anonymous, "anonymous", false, "create anonymously even when agent profiles exist")
	fs.BoolVar(&options.JSON, "json", false, "print the raw create response as JSON")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return docsCreateOptions{}, err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return docsCreateOptions{}, errors.New("usage: comment docs create <file.md> [--profile <handle>] [--anonymous] [--json]")
	}
	options.File = rest[0]
	options.Profile = strings.TrimPrefix(options.Profile, "@")
	if options.Profile != "" && !commentbus.ProfileRE.MatchString(options.Profile) {
		return docsCreateOptions{}, errors.New("invalid profile")
	}
	if options.Profile != "" && options.Anonymous {
		return docsCreateOptions{}, errors.New("--profile and --anonymous cannot be combined")
	}
	return options, nil
}

func docsCreateCommand(options docsCreateOptions, out io.Writer) error {
	markdown, err := readDocsCreateMarkdown(options.File)
	if err != nil {
		return err
	}
	baseURL, agentSecret, identity, err := resolveDocsCreateIdentity(options)
	if err != nil {
		return err
	}
	raw, parsed, err := postDocsCreate(context.Background(), baseURL, agentSecret, markdown)
	if err != nil {
		return err
	}
	if options.JSON {
		fmt.Fprintln(out, strings.TrimSpace(string(raw)))
		return nil
	}
	printDocsCreateResult(out, baseURL, identity, parsed)
	return nil
}

// readDocsCreateMarkdown loads the markdown payload from a file path, or from
// stdin when the path is "-".
func readDocsCreateMarkdown(path string) (string, error) {
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
	} else {
		data, err = os.ReadFile(path)
		if err != nil {
			return "", err
		}
	}
	markdown := string(data)
	if strings.TrimSpace(markdown) == "" {
		return "", fmt.Errorf("%s is empty; POST /docs requires non-empty markdown", docsCreateSourceLabel(path))
	}
	return markdown, nil
}

func docsCreateSourceLabel(path string) string {
	if path == "-" {
		return "stdin"
	}
	return path
}

// resolveDocsCreateIdentity picks the identity for the create request:
// an explicit --profile, otherwise the single configured non-Botlets agent
// profile, otherwise anonymous. Multiple non-Botlets profiles require an
// explicit choice so a doc is never silently attributed to the wrong registered
// agent. Botlets profiles are intentionally excluded from implicit selection:
// a general worklog created as a live bot's handle can route human steering to
// the bot instead of the coding session that owns the work.
func resolveDocsCreateIdentity(options docsCreateOptions) (baseURL string, agentSecret string, identity string, err error) {
	anonymousBaseURL := func() string {
		if options.BaseURL != "" {
			return strings.TrimSuffix(options.BaseURL, "/")
		}
		return commentbus.DefaultBaseURL()
	}
	if options.Anonymous {
		return anonymousBaseURL(), "", "", nil
	}
	paths, err := resolveCLIPaths(options.Home)
	if err != nil {
		return "", "", "", err
	}
	profiles, profileErrors := commentbus.LoadAgentProfiles(context.Background(), paths, "")
	if options.Profile != "" {
		profile, ok := profiles[options.Profile]
		if !ok {
			return "", "", "", fmt.Errorf("agent profile %q was not found under %s%s", options.Profile, filepath.Join(paths.Home, "agents"), profileErrorSuffix(profileErrors, options.Profile))
		}
		baseURL, err := docsCreateProfileBaseURL(profile, options.BaseURL)
		if err != nil {
			return "", "", "", err
		}
		return baseURL, profile.AgentSecret, profile.Handle, nil
	}

	// Refuse to pick an identity implicitly when profile loading reported
	// problems: an unreadable agents dir or a corrupt profile file would
	// otherwise silently downgrade the create to anonymous (or pick the wrong
	// remaining profile) when the user believed a registered identity applied.
	if len(profileErrors) > 0 {
		details := make([]string, 0, len(profileErrors))
		for _, loadError := range profileErrors {
			detail := loadError.Code
			if loadError.Profile != "" {
				detail += " " + loadError.Profile
			}
			if loadError.Message != "" {
				detail += ": " + loadError.Message
			}
			details = append(details, detail)
		}
		return "", "", "", fmt.Errorf("could not load agent profiles (%s); fix the profile files under %s, pass --profile <handle>, or pass --anonymous to create without an identity", strings.Join(details, "; "), filepath.Join(paths.Home, "agents"))
	}
	if len(profiles) == 0 {
		return anonymousBaseURL(), "", "", nil
	}

	botletsHome, hasBotletsHome, err := docsCreateImplicitBotletsHome(paths)
	if err != nil {
		return "", "", "", err
	}
	state := commentbus.ProfileState{
		AgentProfiles: profiles,
		BotRegistry:   map[string]commentbus.BotRegistryEntry{},
	}
	if hasBotletsHome {
		var stateErrors []commentbus.ProfileReloadError
		state, stateErrors = commentbus.LoadProfileState(context.Background(), commentbus.ProfileLoadOptions{
			Paths:       paths,
			BotletsHome: botletsHome,
		})
		if len(stateErrors) > 0 {
			details := make([]string, 0, len(stateErrors))
			for _, loadError := range stateErrors {
				detail := loadError.Code
				if loadError.Profile != "" {
					detail += " " + loadError.Profile
				}
				if loadError.Bot != "" {
					detail += " bot:" + loadError.Bot
				}
				if loadError.Message != "" {
					detail += ": " + loadError.Message
				}
				details = append(details, detail)
			}
			return "", "", "", fmt.Errorf("could not load agent/Botlets profiles (%s); fix the profile files under %s or the Botlets registry, pass --profile <handle>, or pass --anonymous to create without an identity", strings.Join(details, "; "), filepath.Join(paths.Home, "agents"))
		}
	}
	profiles = docsCreateImplicitProfiles(state)
	switch len(profiles) {
	case 0:
		return anonymousBaseURL(), "", "", nil
	case 1:
		for _, profile := range profiles {
			baseURL, err := docsCreateProfileBaseURL(profile, options.BaseURL)
			if err != nil {
				return "", "", "", err
			}
			return baseURL, profile.AgentSecret, profile.Handle, nil
		}
	}
	handles := make([]string, 0, len(profiles))
	for handle := range profiles {
		handles = append(handles, handle)
	}
	sort.Strings(handles)
	return "", "", "", fmt.Errorf("multiple non-Botlets agent profiles are configured (%s); pass --profile <handle> to pick one or --anonymous to create without an identity", strings.Join(handles, ", "))
}

func docsCreateImplicitBotletsHome(paths commentbus.Paths) (string, bool, error) {
	config, ok, err := commentbus.ReadBusConfig(paths)
	if err != nil {
		return "", false, fmt.Errorf("could not load Botlets config; pass --profile <handle> to pick an identity explicitly or --anonymous to create without one: %w", err)
	}
	if !ok || config.BotletsHome == "" {
		return "", false, nil
	}
	return config.BotletsHome, true, nil
}

func docsCreateImplicitProfiles(state commentbus.ProfileState) map[string]commentbus.AgentProfile {
	out := map[string]commentbus.AgentProfile{}
	for handle, profile := range state.AgentProfiles {
		if docsCreateProfileIsBotlets(state, handle) {
			continue
		}
		out[handle] = profile
	}
	return out
}

func docsCreateProfileIsBotlets(state commentbus.ProfileState, handle string) bool {
	for _, bot := range state.BotRegistry {
		if bot.MatchesProfile(handle) {
			return true
		}
	}
	return false
}

// docsCreateProfileBaseURL refuses a --base-url that disagrees with the
// profile's own base_url so an agent_secret is never sent to a foreign host.
func docsCreateProfileBaseURL(profile commentbus.AgentProfile, override string) (string, error) {
	if override == "" {
		return profile.BaseURL, nil
	}
	normalized := strings.TrimSuffix(override, "/")
	if normalized != profile.BaseURL {
		return "", fmt.Errorf("--base-url must match profile %q base_url %s; use --anonymous to target another host", profile.Handle, profile.BaseURL)
	}
	return normalized, nil
}

func postDocsCreate(ctx context.Context, baseURL string, agentSecret string, markdown string) ([]byte, docsCreateResponse, error) {
	payload, err := json.Marshal(map[string]string{"markdown": markdown})
	if err != nil {
		return nil, docsCreateResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(baseURL, "/")+"/docs", bytes.NewReader(payload))
	if err != nil {
		return nil, docsCreateResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Comment-CLI-Version", version)
	if agentSecret != "" {
		req.Header.Set("Authorization", "Bearer "+agentSecret)
	}
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, docsCreateResponse{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, docsCreateResponse{}, err
	}
	// 202 is still a successful create: the doc exists but library placement
	// was queued for asynchronous repair.
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted {
		return nil, docsCreateResponse{}, fmt.Errorf("create failed: HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var parsed docsCreateResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, docsCreateResponse{}, fmt.Errorf("create succeeded but the response could not be parsed: %w", err)
	}
	return body, parsed, nil
}

func printDocsCreateResult(out io.Writer, baseURL string, identity string, parsed docsCreateResponse) {
	who := "anonymously"
	if identity != "" {
		who = "as @" + identity
	}
	fmt.Fprintf(out, "Created %q (%s) %s\n", parsed.Title, parsed.ID, who)
	if parsed.ShareURL != "" {
		fmt.Fprintf(out, "Share URL: %s%s\n", baseURL, parsed.ShareURL)
	}
	apiURL := parsed.APIURL
	if apiURL == "" {
		apiURL = "/docs/" + parsed.ID
	}
	fmt.Fprintf(out, "API URL:   %s%s\n", baseURL, apiURL)
	if identity == "" && parsed.AccessToken != "" {
		fmt.Fprintf(out, "Access token: %s\n", parsed.AccessToken)
		if parsed.IdentifyRequired {
			identifyURL := parsed.IdentifyURL
			if identifyURL == "" {
				identifyURL = "/agents/identify"
			}
			fmt.Fprintf(out, "Before editing, register a display name: POST %s%s with this token as the Bearer token (see %s/llms.txt).\n", baseURL, identifyURL, baseURL)
		}
	}
}
