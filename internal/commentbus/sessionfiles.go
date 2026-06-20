package commentbus

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type SessionFileSnapshot struct {
	Path    string
	Exists  bool
	Size    int64
	Lines   int64
	ModTime time.Time
}

type CodexRolloutMatch struct {
	Path      string
	SessionID string
	ModTime   time.Time
}

var codexRolloutFilenameRE = regexp.MustCompile(`^rollout-.+-([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\.jsonl$`)

const codexRolloutMaxLineBytes = 4 * 1024 * 1024

func ClaudeSessionFilePath(userHome string, cwd string, runtimeSessionRef string) (string, error) {
	if strings.TrimSpace(userHome) == "" || !filepath.IsAbs(userHome) || !isSafeAbsoluteLocalPath(userHome) {
		return "", errors.New("invalid Claude home")
	}
	if strings.TrimSpace(cwd) == "" || !filepath.IsAbs(cwd) || !isSafeAbsoluteLocalPath(cwd) {
		return "", errors.New("invalid Claude working directory")
	}
	if !isRuntimeSessionRef("claude", runtimeSessionRef) {
		return "", errors.New("invalid Claude session id")
	}
	return filepath.Join(userHome, ".claude", "projects", claudeProjectPathKey(cwd), runtimeSessionRef+".jsonl"), nil
}

func WaitForCodexRolloutCorrelation(ctx context.Context, userHome string, cwd string, nonce string, timeout time.Duration, poll time.Duration) (CodexRolloutMatch, bool, error) {
	if timeout <= 0 {
		timeout = transientRuntimeStartupInputReadyWait
	}
	if poll <= 0 {
		poll = transientRuntimeStartupInputReadyPoll
	}
	deadline := time.Now().Add(timeout)
	for {
		if ctx != nil && ctx.Err() != nil {
			return CodexRolloutMatch{}, false, ctx.Err()
		}
		match, ok, err := FindCodexRolloutCorrelation(userHome, cwd, nonce, time.Now())
		if err != nil || ok {
			return match, ok, err
		}
		if !time.Now().Before(deadline) {
			return CodexRolloutMatch{}, false, nil
		}
		sleepCtx := ctx
		if sleepCtx == nil {
			sleepCtx = context.Background()
		}
		if !sleepWithContext(sleepCtx, poll) {
			return CodexRolloutMatch{}, false, sleepCtx.Err()
		}
	}
}

func FindCodexRolloutCorrelation(userHome string, cwd string, nonce string, now time.Time) (CodexRolloutMatch, bool, error) {
	if strings.TrimSpace(userHome) == "" || !filepath.IsAbs(userHome) || !isSafeAbsoluteLocalPath(userHome) {
		return CodexRolloutMatch{}, false, errors.New("invalid Codex home")
	}
	if strings.TrimSpace(cwd) == "" || !filepath.IsAbs(cwd) || !isSafeAbsoluteLocalPath(cwd) {
		return CodexRolloutMatch{}, false, errors.New("invalid Codex working directory")
	}
	if !isCodexRolloutNonce(nonce) {
		return CodexRolloutMatch{}, false, errors.New("invalid Codex rollout nonce")
	}
	paths, err := codexRolloutCandidatePaths(userHome, now)
	if err != nil {
		return CodexRolloutMatch{}, false, err
	}
	var matches []CodexRolloutMatch
	for _, path := range paths {
		match, ok, err := inspectCodexRollout(path, cwd, nonce)
		if err != nil {
			continue
		}
		if ok {
			matches = append(matches, match)
		}
	}
	if len(matches) == 0 {
		return CodexRolloutMatch{}, false, nil
	}
	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i].ModTime.After(matches[j].ModTime)
	})
	return matches[0], true, nil
}

func ReadClaudeSessionFileSnapshot(userHome string, cwd string, runtimeSessionRef string) (SessionFileSnapshot, error) {
	path, err := ClaudeSessionFilePath(userHome, cwd, runtimeSessionRef)
	if err != nil {
		return SessionFileSnapshot{}, err
	}
	snapshot := SessionFileSnapshot{Path: path}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return snapshot, nil
		}
		return SessionFileSnapshot{}, err
	}
	if !info.Mode().IsRegular() {
		return SessionFileSnapshot{}, errors.New("Claude session file must be regular")
	}
	file, err := os.Open(path)
	if err != nil {
		return SessionFileSnapshot{}, err
	}
	defer file.Close()
	lines, err := countLines(file)
	if err != nil {
		return SessionFileSnapshot{}, err
	}
	snapshot.Exists = true
	snapshot.Size = info.Size()
	snapshot.Lines = lines
	snapshot.ModTime = info.ModTime()
	return snapshot, nil
}

func codexRolloutCandidatePaths(userHome string, now time.Time) ([]string, error) {
	if now.IsZero() {
		now = time.Now()
	}
	root := filepath.Join(userHome, ".codex", "sessions")
	dayDirs := codexRolloutDayDirs(root, now)
	var paths []string
	for _, dayDir := range dayDirs {
		entries, err := os.ReadDir(dayDir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !codexRolloutFilenameRE.MatchString(name) {
				continue
			}
			paths = append(paths, filepath.Join(dayDir, name))
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func codexRolloutDayDirs(root string, now time.Time) []string {
	seen := map[string]struct{}{}
	var dirs []string
	add := func(t time.Time) {
		dir := filepath.Join(root, t.Format("2006"), t.Format("01"), t.Format("02"))
		if _, ok := seen[dir]; ok {
			return
		}
		seen[dir] = struct{}{}
		dirs = append(dirs, dir)
	}
	for _, base := range []time.Time{now.Local(), now.UTC()} {
		for delta := -1; delta <= 1; delta++ {
			add(base.AddDate(0, 0, delta))
		}
	}
	return dirs
}

func inspectCodexRollout(path string, cwd string, nonce string) (CodexRolloutMatch, bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return CodexRolloutMatch{}, false, nil
		}
		return CodexRolloutMatch{}, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return CodexRolloutMatch{}, false, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return CodexRolloutMatch{}, false, err
	}
	defer file.Close()

	match := CodexRolloutMatch{
		Path:      path,
		SessionID: codexSessionIDFromRolloutPath(path),
		ModTime:   info.ModTime(),
	}
	cleanCWD := filepath.Clean(cwd)
	cwdMatched := false
	nonceMatched := false
	reader := bufio.NewReader(file)
	for {
		line, ok, err := readCodexRolloutJSONLine(reader)
		if err != nil {
			return CodexRolloutMatch{}, false, err
		}
		if !ok {
			break
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var entry any
		if err := json.Unmarshal(line, &entry); err != nil {
			// Codex may be actively appending while we poll; ignore partial lines
			// and wait for a complete nonce-bearing event on a later pass.
			continue
		}
		if id := codexSessionIDFromJSON(entry); id != "" && id != match.SessionID {
			return CodexRolloutMatch{}, false, nil
		}
		if codexJSONContainsCWD(entry, cleanCWD) {
			cwdMatched = true
		}
		if codexJSONUserMessageContains(entry, nonce) {
			nonceMatched = true
		}
	}
	if !cwdMatched || !nonceMatched || !isRuntimeSessionRef("codex", match.SessionID) {
		return CodexRolloutMatch{}, false, nil
	}
	return match, true, nil
}

func readCodexRolloutJSONLine(reader *bufio.Reader) ([]byte, bool, error) {
	var line []byte
	oversized := false
	for {
		part, err := reader.ReadSlice('\n')
		if len(part) > 0 && !oversized {
			if len(line)+len(part) > codexRolloutMaxLineBytes {
				oversized = true
				line = nil
			} else {
				line = append(line, part...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			if len(part) == 0 && len(line) == 0 && !oversized {
				return nil, false, nil
			}
			return line, true, nil
		}
		if err != nil {
			return nil, false, err
		}
		return line, true, nil
	}
}

func codexSessionIDFromRolloutPath(path string) string {
	matches := codexRolloutFilenameRE.FindStringSubmatch(filepath.Base(path))
	if len(matches) != 2 {
		return ""
	}
	id := strings.ToLower(matches[1])
	if !isRuntimeSessionRef("codex", id) {
		return ""
	}
	return id
}

func codexSessionIDFromJSON(value any) string {
	object, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	if object["type"] != "session_meta" {
		return ""
	}
	payload, ok := object["payload"].(map[string]any)
	if !ok {
		return ""
	}
	id, ok := payload["id"].(string)
	if !ok {
		return ""
	}
	id = strings.ToLower(strings.TrimSpace(id))
	if !isRuntimeSessionRef("codex", id) {
		return ""
	}
	return id
}

func codexJSONContainsCWD(value any, cwd string) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == "cwd" {
				if text, ok := child.(string); ok && filepath.Clean(text) == cwd {
					return true
				}
				continue
			}
			if codexJSONContainsCWD(child, cwd) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if codexJSONContainsCWD(child, cwd) {
				return true
			}
		}
	}
	return false
}

func codexJSONUserMessageContains(value any, nonce string) bool {
	object, ok := value.(map[string]any)
	if !ok {
		return false
	}
	payload, ok := object["payload"].(map[string]any)
	if !ok {
		return false
	}
	if object["type"] == "event_msg" && payload["type"] == "user_message" {
		return codexJSONContainsString(payload, nonce)
	}
	if object["type"] == "response_item" && payload["type"] == "message" && payload["role"] == "user" {
		return codexJSONContainsString(payload, nonce)
	}
	return false
}

func codexJSONContainsString(value any, needle string) bool {
	switch typed := value.(type) {
	case string:
		return strings.Contains(typed, needle)
	case map[string]any:
		for _, child := range typed {
			if codexJSONContainsString(child, needle) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if codexJSONContainsString(child, needle) {
				return true
			}
		}
	}
	return false
}

func isCodexRolloutNonce(nonce string) bool {
	return LocalOperationIDRE.MatchString(nonce)
}

func WaitForSessionFileGrowth(ctx context.Context, before SessionFileSnapshot, timeout time.Duration, poll time.Duration) (bool, error) {
	if before.Path == "" {
		return true, nil
	}
	if timeout <= 0 {
		timeout = transientRuntimeStartupInputReadyWait
	}
	if poll <= 0 {
		poll = transientRuntimeStartupInputReadyPoll
	}
	deadline := time.Now().Add(timeout)
	for {
		if ctx != nil && ctx.Err() != nil {
			return false, ctx.Err()
		}
		after, err := readSessionFileSnapshotAtPath(before.Path)
		if err != nil {
			return false, err
		}
		if after.Exists && (after.Lines > before.Lines || after.Size > before.Size) {
			return true, nil
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}
		sleepCtx := ctx
		if sleepCtx == nil {
			sleepCtx = context.Background()
		}
		if !sleepWithContext(sleepCtx, poll) {
			return false, sleepCtx.Err()
		}
	}
}

func readSessionFileSnapshotAtPath(path string) (SessionFileSnapshot, error) {
	snapshot := SessionFileSnapshot{Path: path}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return snapshot, nil
		}
		return SessionFileSnapshot{}, err
	}
	if !info.Mode().IsRegular() {
		return SessionFileSnapshot{}, errors.New("session file must be regular")
	}
	file, err := os.Open(path)
	if err != nil {
		return SessionFileSnapshot{}, err
	}
	defer file.Close()
	lines, err := countLines(file)
	if err != nil {
		return SessionFileSnapshot{}, err
	}
	snapshot.Exists = true
	snapshot.Size = info.Size()
	snapshot.Lines = lines
	snapshot.ModTime = info.ModTime()
	return snapshot, nil
}

func countLines(reader io.Reader) (int64, error) {
	var lines int64
	buf := make([]byte, 32*1024)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			lines += int64(bytes.Count(buf[:n], []byte{'\n'}))
		}
		if errors.Is(err, io.EOF) {
			return lines, nil
		}
		if err != nil {
			return 0, err
		}
	}
}

func claudeProjectPathKey(cwd string) string {
	cleaned := filepath.Clean(cwd)
	separator := string(os.PathSeparator)
	return strings.NewReplacer(separator, "-", ".", "-", " ", "-").Replace(cleaned)
}
