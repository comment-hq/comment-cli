package commentbus

import (
	"errors"
	"strings"
)

const llmsInstructionsEndpoint = "/llms.txt"

// startupOrientationPaths carries the runtime-resolved values the orientation
// builders inline in place of the literal env-var names. Empty strings cause
// the builders to fall back to URL-fetch phrasing.
type startupOrientationPaths struct {
	DocsRoot  string
	SyncRoot  string
	BrainRoot string
}

func formatTmuxStartupInstruction(baseURL string, handle string, paths startupOrientationPaths) (string, error) {
	if !ProfileRE.MatchString(handle) {
		return "", errors.New("invalid profile handle")
	}
	// Degrade gracefully when the path-inlined prompt overflows the 512-byte
	// tmux chunk on a deeply nested sync root: retry without DocsRoot, then
	// without SyncRoot, then bare URL fallback. Any earlier variant that
	// fits and passes safety validation wins.
	variants := []startupOrientationPaths{
		paths,
		{DocsRoot: paths.DocsRoot},
		{},
	}
	var lastErr error
	for _, v := range variants {
		text, err := renderTmuxStartupInstruction(baseURL, handle, v)
		if err == nil {
			return text, nil
		}
		lastErr = err
	}
	return "", lastErr
}

func renderTmuxStartupInstruction(baseURL string, handle string, paths startupOrientationPaths) (string, error) {
	llmsURL, err := buildNotificationURL(baseURL, llmsInstructionsEndpoint, nil)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("You are an agent running on comment.io with the handle ")
	b.WriteString(handle)
	b.WriteString(". Before handling Comment.io work, ")
	if paths.DocsRoot != "" {
		b.WriteString("read ")
		b.WriteString(paths.DocsRoot)
		b.WriteString("/llms.txt")
	} else {
		b.WriteString("fetch, read, and understand the full agent instructions at ")
		b.WriteString(llmsURL)
	}
	b.WriteString(". ")
	if paths.SyncRoot != "" {
		b.WriteString("Prefer ")
		b.WriteString(paths.SyncRoot)
		b.WriteString(" for broad local reads, and write through the Comment.io API or web UI.")
	} else {
		b.WriteString("Write through the Comment.io API or web UI.")
	}
	text := b.String()
	if err := validateStartupInstruction(text); err != nil {
		return "", err
	}
	return text, nil
}

// formatBotletsTmuxStartupInstruction returns a single-line tmux startup
// instruction tailored to Botlets bots. It names the bot, its brain root,
// the canonical startup-file order, and the agent docs location. Multi-line
// markdown orientation (BuildBotletsSetupOrientation) is intentionally not
// used here because tmux send-keys -l rejects newlines.
func formatBotletsTmuxStartupInstruction(baseURL string, botName string, botDisplayName string, handle string, paths startupOrientationPaths) (string, error) {
	if !ProfileRE.MatchString(handle) {
		return "", errors.New("invalid profile handle")
	}
	if strings.TrimSpace(botName) == "" || strings.ContainsAny(botName, "\r\n\x00`\"'$;&|<>") {
		return "", errors.New("invalid bot name")
	}
	displayName := strings.TrimSpace(botDisplayName)
	if displayName == "" {
		displayName = botName
	}
	if err := validateOrientationDisplayName(displayName); err != nil {
		return "", err
	}
	if paths.BrainRoot == "" {
		return "", errors.New("missing botlets brain root")
	}
	// Degrade gracefully when the path-inlined prompt overflows the 512-byte
	// tmux chunk: retry without DocsRoot (URL fallback for the agent-docs
	// pointer) before giving up. The brain root is the load-bearing piece of
	// botlets-specific orientation and is preserved across variants.
	variants := []startupOrientationPaths{
		paths,
		{BrainRoot: paths.BrainRoot},
	}
	var lastErr error
	hasBootstrap, _ := BotletsBootstrapPresent(paths.BrainRoot)
	for _, compact := range []bool{false, true} {
		for _, v := range variants {
			text, err := renderBotletsTmuxStartupInstruction(baseURL, botName, displayName, handle, v, hasBootstrap, compact)
			if err == nil {
				return text, nil
			}
			lastErr = err
		}
	}
	return "", lastErr
}

func renderBotletsTmuxStartupInstruction(baseURL string, botName string, botDisplayName string, handle string, paths startupOrientationPaths, hasBootstrap bool, compact bool) (string, error) {
	llmsURL, err := buildNotificationURL(baseURL, llmsInstructionsEndpoint, nil)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("You are ")
	b.WriteString(botDisplayName)
	b.WriteString(" (@")
	b.WriteString(handle)
	if compact {
		b.WriteString("), a Botlets bot. Brain: ")
	} else {
		b.WriteString("), a Botlets bot. Your brain is at ")
	}
	b.WriteString(paths.BrainRoot)
	// The today/yesterday daily-note read mandate is deliberately NOT inlined
	// here. This single-line tmux nudge is hard-capped at 512 bytes
	// (validateStartupInstruction) with effectively no headroom on long brain
	// roots, and it already directs the bot to read AGENTS.md, whose Session
	// Startup section carries the main-session daily-notes mandate. The richer
	// multi-line BuildBotletsSetupOrientation states it explicitly; this terse
	// boot keystroke relies on AGENTS.md to avoid overflowing the chunk limit.
	if compact {
		b.WriteString(". Read AGENTS.md, TOOLS.md, SOUL.md, IDENTITY.md, USER.md, MEMORY.md, HEARTBEAT.md. Read llms before writes; patch by slug/revision; new brain docs: POST /docs library_target bot; never runtime memory.")
	} else {
		b.WriteString(". Orient from AGENTS.md, TOOLS.md, SOUL.md, IDENTITY.md, USER.md, MEMORY.md, HEARTBEAT.md. For Comment.io work, read API docs first: ")
		if paths.DocsRoot != "" {
			b.WriteString("read ")
			b.WriteString(paths.DocsRoot)
			b.WriteString("/llms.txt")
		} else {
			b.WriteString("fetch ")
			b.WriteString(llmsURL)
		}
		b.WriteString(". Brain files are read-only; patch existing docs via API by header slug/revision; new brain docs: POST /docs with library_target {\"kind\":\"bot\"}; never runtime memory.")
	}
	if hasBootstrap {
		b.WriteString(" BOOTSTRAP.md exists; follow it now, remove it via Comment.io, then run comment sync once.")
	}
	text := b.String()
	if err := validateStartupInstruction(text); err != nil {
		return "", err
	}
	return text, nil
}

func validateStartupInstruction(text string) error {
	if strings.ContainsAny(text, "\r\n\x00") || containsSecretValue(text) || len(text) > 512 {
		return errors.New("invalid tmux startup instruction")
	}
	return nil
}
