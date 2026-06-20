package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/comment-hq/comment-cli/internal/commentsync"
)

// `comment skill install` is the native equivalent of the `/skill` detector
// shell script: it fingerprints the coding agent(s) on this machine and writes
// the one Comment.io skill (a pointer to /llms.txt) into each agent's native
// skills/comment/SKILL.md, or an AGENTS.md fallback when none is found. The
// skill is just downloaded text — no fetched code is executed.

func runSkill(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: comment skill install [--base-url URL]")
	}
	switch args[0] {
	case "install":
		return runSkillInstall(args[1:])
	default:
		return fmt.Errorf("unknown skill command %q\n\nusage: comment skill install [--base-url URL]", args[0])
	}
}

type skillInstallResult struct {
	OK        bool     `json:"ok"`
	Installed []string `json:"installed"`
	AgentsMD  string   `json:"agents_md,omitempty"`
}

func runSkillInstall(args []string) error {
	fs := flag.NewFlagSet("skill install", flag.ContinueOnError)
	baseURLFlag := fs.String("base-url", "", "Comment.io base URL (defaults to the configured base)")
	homeFlag := fs.String("home", "", "override the home directory (for testing)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	home := *homeFlag
	if home == "" {
		resolved, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("could not resolve the home directory: %w", err)
		}
		home = resolved
	}
	baseURL := strings.TrimRight(firstNonEmpty(*baseURLFlag, commentsync.DefaultBaseURL()), "/")

	result := skillInstallResult{Installed: []string{}}
	targets := detectAgentSkillTargets(home)
	if len(targets) == 0 {
		// No coding-agent skills directory — drop the AGENTS.md fallback where
		// agents that read AGENTS.md (Aider, Cline) will find it. Only the
		// fallback is fetched on this path; no SKILL.md is downloaded. Append to
		// (never clobber) an existing AGENTS.md, and skip if already present.
		dst := filepath.Join(home, ".agents", "AGENTS.md")
		existing, readErr := os.ReadFile(dst)
		if readErr == nil && bytes.Contains(existing, []byte(agentsFallbackMarker)) {
			result.AgentsMD = dst
		} else {
			agentsBody, agentsErr := skillHTTPGet(baseURL + "/agents.md")
			if agentsErr != nil {
				return fmt.Errorf("no coding-agent skills directory was found, and the AGENTS.md fallback could not be fetched: %w", agentsErr)
			}
			if readErr == nil {
				// Existing AGENTS.md without our marker — append, don't clobber.
				if err := skillAppendFile(dst, append([]byte("\n"), agentsBody...)); err != nil {
					return err
				}
			} else if err := skillWriteFile(dst, agentsBody); err != nil {
				return err
			}
			result.AgentsMD = dst
		}
	} else {
		// Each agent gets its own skill asset (OpenClaw has a channel-specific
		// one); fetch each distinct skill at most once.
		cache := map[string][]byte{}
		for _, tgt := range targets {
			body, ok := cache[tgt.skill]
			if !ok {
				fetched, err := skillHTTPGet(baseURL + tgt.skill)
				if err != nil {
					return fmt.Errorf("could not download %s: %w", tgt.skill, err)
				}
				cache[tgt.skill] = fetched
				body = fetched
			}
			dst := filepath.Join(tgt.dir, "comment", "SKILL.md")
			if err := skillWriteFile(dst, body); err != nil {
				return err
			}
			result.Installed = append(result.Installed, dst)
		}
	}
	result.OK = true
	return printJSON(result)
}

const (
	universalSkillPath = "/comment.SKILL.md"
	openClawSkillPath  = "/openclaw.SKILL.md"
	// agentsFallbackMarker must match AGENTS_FALLBACK_MARKER in
	// packages/shared/src/agent-docs.ts — the idempotency marker that leads the
	// /agents.md body so this installer can skip/append instead of clobbering an
	// existing ~/.agents/AGENTS.md.
	agentsFallbackMarker = "<!-- comment.io-agents-md -->"
)

// agentSkillTarget is one agent's skills directory plus the skill asset to
// install there (OpenClaw uses a channel-specific skill; everyone else gets the
// universal one).
type agentSkillTarget struct {
	dir   string
	skill string
}

// detectAgentSkillTargets returns the install targets for the coding agents
// present on this machine, mirroring the /skill detector shell script. A
// machine may host more than one agent, so every match is included.
func detectAgentSkillTargets(home string) []agentSkillTarget {
	var targets []agentSkillTarget
	add := func(dir, skill string) { targets = append(targets, agentSkillTarget{dir: dir, skill: skill}) }
	if v := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); v != "" {
		add(filepath.Join(v, "skills"), universalSkillPath)
	} else if os.Getenv("CLAUDE_CODE_SESSION_ID") != "" || skillDirExists(filepath.Join(home, ".claude")) {
		add(filepath.Join(home, ".claude", "skills"), universalSkillPath)
	}
	if v := strings.TrimSpace(os.Getenv("CODEX_HOME")); v != "" {
		add(filepath.Join(v, "skills"), universalSkillPath)
	} else if skillDirExists(filepath.Join(home, ".codex")) {
		add(filepath.Join(home, ".codex", "skills"), universalSkillPath)
	}
	// OpenClaw has its own channel-specific skill.
	if skillDirExists(filepath.Join(home, ".openclaw")) {
		add(filepath.Join(home, ".openclaw", "skills"), openClawSkillPath)
	}
	for _, name := range []string{".cursor", ".gemini"} {
		if skillDirExists(filepath.Join(home, name)) {
			add(filepath.Join(home, name, "skills"), universalSkillPath)
		}
	}
	return targets
}

func skillDirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func skillWriteFile(dst string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("could not create %s: %w", filepath.Dir(dst), err)
	}
	if err := os.WriteFile(dst, body, 0o644); err != nil {
		return fmt.Errorf("could not write %s: %w", dst, err)
	}
	return nil
}

// skillAppendFile appends body to dst (creating it + parents if needed), used to
// add the Comment.io section to a pre-existing AGENTS.md without clobbering it.
func skillAppendFile(dst string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("could not create %s: %w", filepath.Dir(dst), err)
	}
	f, err := os.OpenFile(dst, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("could not open %s: %w", dst, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(body); err != nil {
		return fmt.Errorf("could not append to %s: %w", dst, err)
	}
	return nil
}

func skillHTTPGet(url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d from %s", resp.StatusCode, url)
	}
	return io.ReadAll(resp.Body)
}
