package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

var legacyClaudePluginMarkers = []string{
	"comment-io.js",
	"comment-io.mjs",
	"subscribe_agents",
	"channel_intro",
	"mcpServers",
	"claude/channel",
	"dangerously-load-development-channels",
}

type pluginDoctorFinding struct {
	Path    string   `json:"path"`
	Kind    string   `json:"kind"`
	Markers []string `json:"markers"`
}

type pluginDoctorResult struct {
	OK             bool                  `json:"ok"`
	Stale          bool                  `json:"stale"`
	Fixed          bool                  `json:"fixed"`
	ClaudeHome     string                `json:"claude_home"`
	CacheRoot      string                `json:"cache_root"`
	Findings       []pluginDoctorFinding `json:"findings"`
	RemovedPaths   []string              `json:"removed_paths,omitempty"`
	RepairCommands []string              `json:"repair_commands,omitempty"`
	Message        string                `json:"message"`
}

func runPlugin(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: comment plugin doctor [--claude-home ~/.claude] [--fix]")
	}
	switch args[0] {
	case "doctor":
		return runPluginDoctor(args[1:])
	default:
		return fmt.Errorf("unknown plugin command %q", args[0])
	}
}

func runPluginDoctor(args []string) error {
	fs := flag.NewFlagSet("comment plugin doctor", flag.ContinueOnError)
	claudeHome := fs.String("claude-home", "", "Claude Code config directory")
	fix := fs.Bool("fix", false, "remove stale Comment.io Claude Code plugin cache directories")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) > 0 {
		return errors.New("plugin doctor does not accept positional arguments")
	}
	resolvedClaudeHome, err := resolveClaudeHome(*claudeHome)
	if err != nil {
		return err
	}
	cacheRoot := filepath.Join(resolvedClaudeHome, "plugins", "cache")
	findings, err := scanClaudePluginCache(cacheRoot)
	if err != nil {
		return err
	}
	result := pluginDoctorResult{
		OK:         len(findings) == 0,
		Stale:      len(findings) > 0,
		Fixed:      false,
		ClaudeHome: resolvedClaudeHome,
		CacheRoot:  cacheRoot,
		Findings:   findings,
	}
	if len(findings) == 0 {
		result.Message = "Claude Code plugin cache has no stale Comment.io channel/MCP artifacts."
		return printJSON(result)
	}
	fixCommand := pluginDoctorFixCommand(resolvedClaudeHome)
	result.RepairCommands = []string{
		fixCommand,
		"claude plugin marketplace update comment-io-plugins",
		"claude plugin install comment-io@comment-io-plugins",
	}
	if *fix {
		removed, err := removeStaleClaudePluginCaches(cacheRoot, findings)
		if err != nil {
			return err
		}
		result.OK = true
		result.Fixed = true
		result.RemovedPaths = removed
		result.Message = "Removed stale Comment.io Claude Code plugin cache directories. Refresh the marketplace and reinstall the current plugin."
		return printJSON(result)
	}
	result.Message = fmt.Sprintf("Found stale Comment.io Claude Code plugin cache artifacts. Run `%s`, then `claude plugin marketplace update comment-io-plugins`, then reinstall the current plugin.", fixCommand)
	if err := printJSON(result); err != nil {
		return err
	}
	return cliExitError{Code: 2}
}

func pluginDoctorFixCommand(claudeHome string) string {
	return "comment plugin doctor --claude-home " + shellSingleQuote(claudeHome) + " --fix"
}

func shellSingleQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func resolveClaudeHome(value string) (string, error) {
	if value == "" {
		value = os.Getenv("CLAUDE_CONFIG_DIR")
	}
	if value == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		value = filepath.Join(home, ".claude")
	}
	if !filepath.IsAbs(value) {
		abs, err := filepath.Abs(value)
		if err != nil {
			return "", err
		}
		value = abs
	}
	return filepath.Clean(value), nil
}

func scanClaudePluginCache(cacheRoot string) ([]pluginDoctorFinding, error) {
	if _, err := os.Stat(cacheRoot); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var findings []pluginDoctorFinding
	err := filepath.WalkDir(cacheRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if d.Type()&os.ModeSymlink != 0 {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		rel, err := filepath.Rel(cacheRoot, path)
		if err != nil {
			return err
		}
		if knownClaudeCommentPluginCacheTarget(cacheRoot, rel) == "" {
			return nil
		}
		markers := staleMarkersForFile(path)
		if strings.Contains(strings.ToLower(filepath.ToSlash(rel)), "claude/channel") {
			markers = append(markers, "claude/channel")
			slices.Sort(markers)
			markers = slices.Compact(markers)
		}
		if len(markers) == 0 {
			return nil
		}
		findings = append(findings, pluginDoctorFinding{
			Path:    path,
			Kind:    "legacy_claude_plugin_cache",
			Markers: markers,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return findings, nil
}

func knownClaudeCommentPluginCacheTarget(cacheRoot string, rel string) string {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) < 2 {
		return ""
	}
	owner := strings.ToLower(parts[0])
	plugin := strings.ToLower(parts[1])
	known := (owner == "comment-io-plugins" && plugin == "comment-io") ||
		// Successive generations of GitHub org names own these cache paths:
		// botspring-ai (oldest), botlets-ai, comment-io (interim), and
		// comment-hq (current). Scan all of them so the doctor catches stale
		// markers regardless of which org the plugin was installed under.
		(owner == "botspring-ai" && plugin == "comment-io-claude-code-plugin") ||
		(owner == "botlets-ai" && plugin == "comment-io-claude-code-plugin") ||
		(owner == "comment-io" && plugin == "comment-io-claude-code-plugin") ||
		(owner == "comment-hq" && plugin == "comment-io-claude-code-plugin")
	if !known {
		return ""
	}
	return filepath.Join(cacheRoot, parts[0], parts[1])
}

func staleMarkersForFile(path string) []string {
	var markers []string
	base := filepath.Base(path)
	if base == "comment-io.js" || base == "comment-io.mjs" {
		markers = append(markers, base)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return markers
	}
	if len(data) > 2*1024*1024 {
		data = data[:2*1024*1024]
	}
	text := string(data)
	for _, marker := range legacyClaudePluginMarkers {
		if marker == base {
			continue
		}
		if strings.Contains(text, marker) {
			markers = append(markers, marker)
		}
	}
	slices.Sort(markers)
	return slices.Compact(markers)
}

func removeStaleClaudePluginCaches(cacheRoot string, findings []pluginDoctorFinding) ([]string, error) {
	targets := staleCacheTargets(cacheRoot, findings)
	for _, target := range targets {
		if err := os.RemoveAll(target); err != nil {
			return nil, err
		}
	}
	return targets, nil
}

func staleCacheTargets(cacheRoot string, findings []pluginDoctorFinding) []string {
	seen := map[string]bool{}
	var targets []string
	for _, finding := range findings {
		rel, err := filepath.Rel(cacheRoot, finding.Path)
		if err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
			continue
		}
		target := knownClaudeCommentPluginCacheTarget(cacheRoot, rel)
		if target == "" {
			continue
		}
		if seen[target] {
			continue
		}
		seen[target] = true
		targets = append(targets, target)
	}
	slices.Sort(targets)
	return targets
}
