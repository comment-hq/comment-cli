package commentbus

import "strings"

const (
	runtimeTermDefault      = "xterm-256color"
	runtimeColorTermDefault = "truecolor"
)

var runtimeTerminalColorKeys = []string{"TERM", "COLORTERM", "FORCE_COLOR", "CLICOLOR_FORCE"}

// normalizeRuntimeTerminalColorEnv returns env with the terminal color policy
// used for Comment.io-managed interactive runtimes. It intentionally ignores
// NO_COLOR for managed runtime launches so Claude/Codex-style UIs can render in
// color without changing the user's global shell environment.
func normalizeRuntimeTerminalColorEnv(env []string) []string {
	out := make([]string, 0, len(env)+len(runtimeTerminalColorKeys))
	seen := map[string]int{}
	for _, entry := range env {
		key := envKey(entry)
		if key == "NO_COLOR" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = len(out)
		out = append(out, entry)
	}
	set := func(key, value string) {
		entry := key + "=" + value
		if idx, ok := seen[key]; ok {
			out[idx] = entry
			return
		}
		seen[key] = len(out)
		out = append(out, entry)
	}
	if idx, ok := seen["TERM"]; !ok || !isUsableRuntimeTerm(envValue(out[idx])) {
		set("TERM", runtimeTermDefault)
	}
	if idx, ok := seen["COLORTERM"]; !ok || !isSafeRuntimeColorValue(envValue(out[idx])) || strings.TrimSpace(envValue(out[idx])) == "" {
		set("COLORTERM", runtimeColorTermDefault)
	}
	set("FORCE_COLOR", "1")
	set("CLICOLOR_FORCE", "1")
	return out
}

func runtimeTerminalColorEnv(env []string) []string {
	normalized := normalizeRuntimeTerminalColorEnv(env)
	out := make([]string, 0, len(runtimeTerminalColorKeys))
	for _, key := range runtimeTerminalColorKeys {
		if value, ok := envEntryValue(normalized, key); ok {
			out = append(out, key+"="+value)
		}
	}
	return out
}

func envEntryValue(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix), true
		}
	}
	return "", false
}

func isUsableRuntimeTerm(value string) bool {
	term := strings.TrimSpace(value)
	return term != "" && !strings.EqualFold(term, "dumb") && isSafeRuntimeColorValue(term)
}

func isSafeRuntimeColorValue(value string) bool {
	return len(value) <= 4096 && !strings.ContainsAny(value, "\r\n\x00") && !containsSecretValue(value)
}
