package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

type botletsRunLogsOptions struct {
	Home        string
	BotletsHome string
	Bot         string
	RunID       string
	MessageID   string
	ClaudeHome  string
	CodexHome   string
	JSON        bool
	NoOpen      bool
	Limit       int
}

type botletsRunLogEntry struct {
	RunID             string                     `json:"run_id,omitempty"`
	MessageID         string                     `json:"message_id"`
	Kind              string                     `json:"kind,omitempty"`
	ScheduledFor      string                     `json:"scheduled_for,omitempty"`
	CreatedAt         string                     `json:"created_at,omitempty"`
	DeliveryState     string                     `json:"delivery_state,omitempty"`
	LocalSessionID    string                     `json:"local_session_id,omitempty"`
	SessionGeneration string                     `json:"session_generation,omitempty"`
	Command           string                     `json:"command"`
	Transcript        *botletsTranscriptMatch    `json:"transcript,omitempty"`
	Opened            bool                       `json:"opened,omitempty"`
	Message           commentbus.MessageEnvelope `json:"-"`
	SortTime          string                     `json:"-"`
}

type botletsTranscriptMatch struct {
	Runtime          string `json:"runtime"`
	Path             string `json:"path"`
	Line             int    `json:"line"`
	RuntimeSessionID string `json:"runtime_session_id,omitempty"`
	CWD              string `json:"cwd,omitempty"`
}

type botletsTranscriptCandidate struct {
	botletsTranscriptMatch
	score int
}

var (
	botletsRunIDRE         = regexpMustCompile(`^blr_[A-Za-z0-9_-]{8,128}$`)
	botletsOpenDefaultFile = openDefaultFile
)

func runBotletsLogs(args []string) error {
	fs := flag.NewFlagSet("comment botlets logs", flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	botletsHome := fs.String("botlets-home", "", "Botlets home directory")
	bot := fs.String("bot", "", "bot name or handle")
	runID := fs.String("run", "", "Botlets run id")
	messageID := fs.String("message", "", "local message id")
	claudeHome := fs.String("claude-home", "", "Claude Code home directory")
	codexHome := fs.String("codex-home", "", "Codex home directory")
	jsonOut := fs.Bool("json", false, "print JSON")
	noOpen := fs.Bool("no-open", false, "do not open the matched transcript")
	limit := fs.Int("limit", 20, "recent run count when listing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) > 0 {
		return errors.New("botlets logs does not accept positional arguments")
	}
	return runBotletsLogsWithOptions(botletsRunLogsOptions{
		Home:        *home,
		BotletsHome: *botletsHome,
		Bot:         *bot,
		RunID:       strings.TrimSpace(*runID),
		MessageID:   strings.TrimSpace(*messageID),
		ClaudeHome:  *claudeHome,
		CodexHome:   *codexHome,
		JSON:        *jsonOut,
		NoOpen:      *noOpen,
		Limit:       *limit,
	})
}

func runBotletsLogsWithOptions(options botletsRunLogsOptions) error {
	if strings.TrimSpace(options.Bot) == "" {
		return errors.New("botlets logs requires --bot")
	}
	if options.RunID != "" && !botletsRunIDRE.MatchString(options.RunID) {
		return errors.New("invalid Botlets run id")
	}
	if options.MessageID != "" && !commentbus.LocalMessageIDRE.MatchString(options.MessageID) {
		return errors.New("invalid local message id")
	}
	if options.RunID != "" && options.MessageID != "" {
		return errors.New("pass either --run or --message, not both")
	}
	if options.Limit <= 0 {
		options.Limit = 20
	}
	if options.Limit > 50 {
		options.Limit = 50
	}
	paths, err := resolveCLIPaths(options.Home)
	if err != nil {
		return err
	}
	botletsHome, err := commentbus.ResolveBotletsHome(firstNonEmpty(options.BotletsHome, persistedCLIBotletsHome(paths, "")))
	if err != nil {
		return err
	}
	state, profileErrors := commentbus.LoadProfileState(context.Background(), commentbus.ProfileLoadOptions{
		Paths:       paths,
		BotletsHome: botletsHome,
	})
	entry, _, ok, selectErr := selectBotletsStatusBot(state, strings.TrimSpace(options.Bot))
	if selectErr != nil {
		return selectErr
	}
	if !ok {
		if len(profileErrors) > 0 {
			return fmt.Errorf("Botlets bot %q was not found; profile load reported: %s", options.Bot, profileErrors[0].Message)
		}
		return fmt.Errorf("Botlets bot %q was not found", options.Bot)
	}
	store, err := commentbus.OpenExistingStore(context.Background(), paths)
	if err != nil {
		if errors.Is(err, commentbus.ErrStoreNotInitialized) {
			return errors.New("local message history is not initialized; run `comment bus install` or start the Botlets runtime first")
		}
		return err
	}
	defer store.Close()
	if err := store.BackfillBotIdentityColumns(context.Background(), state.BotRegistry); err != nil {
		return err
	}

	targetMode := options.RunID != "" || options.MessageID != ""
	if !targetMode {
		entries, err := listBotletsRunLogEntries(context.Background(), store, entry, options.Limit)
		if err != nil {
			return err
		}
		if options.JSON {
			return printJSON(map[string]any{
				"bot":     entry.Name,
				"handle":  entry.Handle,
				"runs":    entries,
				"message": "pass --run or --message to open a matched Claude/Codex transcript",
			})
		}
		printBotletsRunLogList(entry, entries)
		return nil
	}

	message, err := findBotletsRunLogMessage(context.Background(), store, entry, options.RunID, options.MessageID)
	if err != nil {
		return err
	}
	runID := botletsRunIDFromMessage(message)
	transcript, err := findBotletsTranscript(options, entry, message)
	if err != nil {
		return err
	}
	result := botletsRunLogEntryFromMessage(entry, message)
	result.Transcript = transcript
	if !options.JSON && !options.NoOpen {
		if err := botletsOpenDefaultFile(transcript.Path); err != nil {
			return fmt.Errorf("open transcript: %w", err)
		}
		result.Opened = true
	}
	if options.JSON {
		return printJSON(map[string]any{
			"bot":     entry.Name,
			"handle":  entry.Handle,
			"run":     result,
			"opened":  false,
			"run_id":  runID,
			"message": "transcript matched the local run notification line",
		})
	}
	printBotletsRunLogResult(result)
	return nil
}

func listBotletsRunLogEntries(ctx context.Context, store *commentbus.Store, bot commentbus.BotRegistryEntry, limit int) ([]botletsRunLogEntry, error) {
	messages, err := listAllBotletsTaskMessages(ctx, store, bot)
	if err != nil {
		return nil, err
	}
	entries := make([]botletsRunLogEntry, 0, len(messages))
	for _, message := range messages {
		runID := botletsRunIDFromMessage(message)
		if runID == "" {
			continue
		}
		entries = append(entries, botletsRunLogEntryFromMessage(bot, message))
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].SortTime == entries[j].SortTime {
			return entries[i].MessageID > entries[j].MessageID
		}
		return entries[i].SortTime > entries[j].SortTime
	})
	if len(entries) > limit {
		entries = entries[:limit]
	}
	return entries, nil
}

func findBotletsRunLogMessage(ctx context.Context, store *commentbus.Store, bot commentbus.BotRegistryEntry, runID string, messageID string) (commentbus.MessageEnvelope, error) {
	if messageID != "" {
		message, err := store.GetInboxMessageByID(ctx, messageID)
		if err != nil {
			if errors.Is(err, commentbus.ErrMessageNotFound) {
				return commentbus.MessageEnvelope{}, fmt.Errorf("local message %s was not found", messageID)
			}
			return commentbus.MessageEnvelope{}, err
		}
		if message.Kind != "botlets.task" || !botletsRunLogMessageMatchesBot(bot, message) {
			return commentbus.MessageEnvelope{}, fmt.Errorf("local message %s is not a Botlets task for %s", messageID, bot.Handle)
		}
		return message, nil
	}
	cursor := ""
	for {
		messages, err := listBotletsTaskMessagePage(ctx, store, bot, cursor)
		if err != nil {
			return commentbus.MessageEnvelope{}, err
		}
		for _, message := range messages {
			if botletsRunLogMessageMatchesBot(bot, message) && botletsRunIDFromMessage(message) == runID {
				return message, nil
			}
		}
		if len(messages) < 200 {
			break
		}
		nextCursor := messages[len(messages)-1].ID
		if nextCursor == cursor {
			break
		}
		cursor = nextCursor
	}
	return commentbus.MessageEnvelope{}, fmt.Errorf("Botlets run %s was not found in local message history for %s", runID, bot.Handle)
}

func listAllBotletsTaskMessages(ctx context.Context, store *commentbus.Store, bot commentbus.BotRegistryEntry) ([]commentbus.MessageEnvelope, error) {
	var out []commentbus.MessageEnvelope
	cursor := ""
	for {
		page, err := listBotletsTaskMessagePage(ctx, store, bot, cursor)
		if err != nil {
			return nil, err
		}
		for _, message := range page {
			if botletsRunLogMessageMatchesBot(bot, message) {
				out = append(out, message)
			}
		}
		if len(page) < 200 {
			break
		}
		nextCursor := page[len(page)-1].ID
		if nextCursor == cursor {
			break
		}
		cursor = nextCursor
	}
	return out, nil
}

func listBotletsTaskMessagePage(ctx context.Context, store *commentbus.Store, bot commentbus.BotRegistryEntry, cursor string) ([]commentbus.MessageEnvelope, error) {
	if bot.BotID != "" || bot.StableBotAgentID() != "" {
		return store.ListInboxMessagesByBotIdentity(ctx, bot.BotID, bot.StableBotAgentID(), []string{"botlets.task"}, 200, cursor)
	}
	return store.ListInboxMessages(ctx, commentbus.MessageListFilter{
		Profile: bot.Handle,
		BotName: bot.Name,
		Kinds:   []string{"botlets.task"},
		Limit:   200,
		Cursor:  cursor,
	})
}

func botletsRunLogMessageMatchesBot(bot commentbus.BotRegistryEntry, message commentbus.MessageEnvelope) bool {
	if bot.MatchesStableIdentity(message.BotID, message.BotAgentID) {
		return true
	}
	return bot.MatchesProfile(message.Profile) && bot.MatchesSlug(message.BotName)
}

func botletsRunLogEntryFromMessage(bot commentbus.BotRegistryEntry, message commentbus.MessageEnvelope) botletsRunLogEntry {
	runID := botletsRunIDFromMessage(message)
	scheduledFor := botletsMessageRefString(message, "scheduled_for")
	sortTime := firstNonEmpty(scheduledFor, message.CreatedAt)
	entry := botletsRunLogEntry{
		RunID:         runID,
		MessageID:     message.ID,
		Kind:          botletsMessageRefString(message, "task_kind"),
		ScheduledFor:  scheduledFor,
		CreatedAt:     message.CreatedAt,
		DeliveryState: message.Delivery.State,
		Command:       botletsRunLogCommand(bot.Handle, runID, message.ID),
		Message:       message,
		SortTime:      sortTime,
	}
	if message.Delivery.SessionID != nil {
		entry.LocalSessionID = *message.Delivery.SessionID
	}
	if message.Delivery.SessionGeneration != nil {
		entry.SessionGeneration = *message.Delivery.SessionGeneration
	}
	return entry
}

func botletsRunIDFromMessage(message commentbus.MessageEnvelope) string {
	if runID := botletsMessageRefString(message, "run_id"); runID != "" {
		return runID
	}
	return botletsMessageRefString(message, "task_id")
}

func botletsMessageRefString(message commentbus.MessageEnvelope, key string) string {
	if message.Refs == nil {
		return ""
	}
	value, ok := message.Refs[key]
	if !ok {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case fmt.Stringer:
		return strings.TrimSpace(typed.String())
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
}

func botletsRunLogCommand(botHandle string, runID string, messageID string) string {
	if runID != "" {
		return "comment botlets logs --bot " + shellQuoteArg(botHandle) + " --run " + shellQuoteArg(runID)
	}
	return "comment botlets logs --bot " + shellQuoteArg(botHandle) + " --message " + shellQuoteArg(messageID)
}

func printBotletsRunLogList(bot commentbus.BotRegistryEntry, entries []botletsRunLogEntry) {
	fmt.Printf("bot: %s (%s)\n", bot.Name, bot.Handle)
	if len(entries) == 0 {
		fmt.Println("runs: none found in local message history")
		return
	}
	fmt.Println("runs:")
	for _, entry := range entries {
		when := firstNonEmpty(entry.ScheduledFor, entry.CreatedAt)
		fmt.Printf("- %s  %s  %s  %s\n", when, entry.RunID, entry.MessageID, entry.DeliveryState)
		fmt.Printf("  open: %s\n", entry.Command)
	}
}

func printBotletsRunLogResult(entry botletsRunLogEntry) {
	fmt.Printf("run: %s\n", entry.RunID)
	fmt.Printf("message: %s\n", entry.MessageID)
	if entry.Transcript != nil {
		fmt.Printf("transcript: %s:%d\n", entry.Transcript.Path, entry.Transcript.Line)
		fmt.Printf("runtime: %s\n", entry.Transcript.Runtime)
		if entry.Transcript.RuntimeSessionID != "" {
			fmt.Printf("runtime_session_id: %s\n", entry.Transcript.RuntimeSessionID)
		}
		if entry.Transcript.CWD != "" {
			fmt.Printf("cwd: %s\n", entry.Transcript.CWD)
		}
	}
	if entry.Opened && entry.Transcript != nil {
		fmt.Printf("opened: %s\n", entry.Transcript.Path)
	}
}

func findBotletsTranscript(options botletsRunLogsOptions, bot commentbus.BotRegistryEntry, message commentbus.MessageEnvelope) (*botletsTranscriptMatch, error) {
	runID := botletsRunIDFromMessage(message)
	messageID := message.ID
	roots, err := botletsTranscriptRoots(options)
	if err != nil {
		return nil, err
	}
	var best *botletsTranscriptCandidate
	for _, root := range roots {
		candidate, err := findBotletsTranscriptInRoot(root, bot.ManagedSession.Runtime, messageID, runID)
		if err != nil {
			return nil, err
		}
		if candidate == nil {
			continue
		}
		if best == nil || candidate.score < best.score || candidate.score == best.score && candidate.Path < best.Path {
			best = candidate
		}
	}
	if best == nil {
		return nil, fmt.Errorf("local run message %s was found, but no Claude/Codex transcript line matched run %s", messageID, runID)
	}
	match := best.botletsTranscriptMatch
	return &match, nil
}

type botletsTranscriptRoot struct {
	Path    string
	Runtime string
}

func botletsTranscriptRoots(options botletsRunLogsOptions) ([]botletsTranscriptRoot, error) {
	claudeHome, err := resolveOptionalHome(options.ClaudeHome, ".claude")
	if err != nil {
		return nil, err
	}
	codexHome, err := resolveOptionalHome(options.CodexHome, ".codex")
	if err != nil {
		return nil, err
	}
	return []botletsTranscriptRoot{
		{Path: filepath.Join(claudeHome, "projects"), Runtime: "claude"},
		{Path: filepath.Join(codexHome, "sessions"), Runtime: "codex"},
		{Path: filepath.Join(codexHome, "archived_sessions"), Runtime: "codex"},
	}, nil
}

func resolveOptionalHome(value string, defaultDir string) (string, error) {
	if strings.TrimSpace(value) == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, defaultDir), nil
	}
	return commentbus.ExpandHome(value)
}

func findBotletsTranscriptInRoot(root botletsTranscriptRoot, preferredRuntime string, messageID string, runID string) (*botletsTranscriptCandidate, error) {
	if _, err := os.Stat(root.Path); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var best *botletsTranscriptCandidate
	err := filepath.WalkDir(root.Path, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 || !strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		candidate, scanErr := scanBotletsTranscriptFile(path, root.Runtime, preferredRuntime, messageID, runID)
		if scanErr != nil {
			return scanErr
		}
		if candidate == nil {
			return nil
		}
		if best == nil || candidate.score < best.score || candidate.score == best.score && candidate.Path < best.Path {
			best = candidate
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return best, nil
}

func scanBotletsTranscriptFile(path string, runtimeName string, preferredRuntime string, messageID string, runID string) (*botletsTranscriptCandidate, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 128*1024)
	var best *botletsTranscriptCandidate
	fileSessionID := ""
	fileCWD := ""
	lineNo := 0
	for {
		line, readErr := reader.ReadString('\n')
		if len(line) > 0 {
			lineNo++
			candidate, sessionID, cwd := classifyBotletsTranscriptLine([]byte(line), runtimeName, preferredRuntime, path, lineNo, messageID, runID)
			if fileSessionID == "" && sessionID != "" {
				fileSessionID = sessionID
			}
			if fileCWD == "" && cwd != "" {
				fileCWD = cwd
			}
			if candidate != nil {
				if candidate.RuntimeSessionID == "" {
					candidate.RuntimeSessionID = firstNonEmpty(sessionID, fileSessionID)
				}
				if candidate.CWD == "" {
					candidate.CWD = firstNonEmpty(cwd, fileCWD)
				}
				if best == nil || candidate.score < best.score {
					best = candidate
				}
			}
		}
		if readErr == nil {
			continue
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		return nil, readErr
	}
	if best != nil {
		if best.RuntimeSessionID == "" {
			best.RuntimeSessionID = fileSessionID
		}
		if best.CWD == "" {
			best.CWD = fileCWD
		}
	}
	return best, nil
}

func classifyBotletsTranscriptLine(line []byte, runtimeName string, preferredRuntime string, path string, lineNo int, messageID string, runID string) (*botletsTranscriptCandidate, string, string) {
	text := string(line)
	hasMessageID := strings.Contains(text, messageID)
	hasRunID := runID != "" && strings.Contains(text, runID)
	if !hasMessageID && !hasRunID && !strings.Contains(text, "sessionId") && !strings.Contains(text, "session_meta") {
		return nil, "", ""
	}
	var root map[string]any
	if err := json.Unmarshal(line, &root); err != nil {
		if hasMessageID || hasRunID {
			return botletsTranscriptCandidateForLine(runtimeName, preferredRuntime, path, lineNo, "", "", 60, hasMessageID), "", ""
		}
		return nil, "", ""
	}
	sessionID, cwd := botletsTranscriptLineMetadata(root, runtimeName)
	if !hasMessageID && !hasRunID {
		return nil, sessionID, cwd
	}
	if botletsTranscriptLineIsToolNoise(root) {
		return nil, sessionID, cwd
	}
	if content, ok := botletsTranscriptLineUserContent(root, runtimeName); ok {
		if strings.Contains(content, messageID) || runID != "" && strings.Contains(content, runID) {
			score := 0
			if !strings.Contains(content, messageID) {
				score += 2
			}
			if strings.Contains(strings.ToLower(content), "comment.io message") || strings.Contains(content, "Botlets Task") {
				score -= 4
			}
			return botletsTranscriptCandidateForLine(runtimeName, preferredRuntime, path, lineNo, sessionID, cwd, score, strings.Contains(content, messageID)), sessionID, cwd
		}
	}
	return botletsTranscriptCandidateForLine(runtimeName, preferredRuntime, path, lineNo, sessionID, cwd, 50, hasMessageID), sessionID, cwd
}

func botletsTranscriptCandidateForLine(runtimeName string, preferredRuntime string, path string, lineNo int, sessionID string, cwd string, score int, hasMessageID bool) *botletsTranscriptCandidate {
	if preferredRuntime != "" && runtimeName == preferredRuntime {
		score -= 3
	}
	if hasMessageID {
		score -= 2
	}
	lowerPath := strings.ToLower(path)
	lowerCWD := strings.ToLower(cwd)
	if strings.Contains(lowerPath, "botlets") || strings.Contains(lowerCWD, "botlets") {
		score -= 5
	}
	return &botletsTranscriptCandidate{
		botletsTranscriptMatch: botletsTranscriptMatch{
			Runtime:          runtimeName,
			Path:             path,
			Line:             lineNo,
			RuntimeSessionID: sessionID,
			CWD:              cwd,
		},
		score: score,
	}
}

func botletsTranscriptLineMetadata(root map[string]any, runtimeName string) (string, string) {
	sessionID := jsonPathString(root, "sessionId")
	cwd := jsonPathString(root, "cwd")
	if runtimeName == "codex" {
		if payload, ok := root["payload"].(map[string]any); ok {
			sessionID = firstNonEmpty(sessionID, jsonPathString(payload, "id"), jsonPathString(payload, "session_id"))
			cwd = firstNonEmpty(cwd, jsonPathString(payload, "cwd"))
		}
	}
	return sessionID, cwd
}

func botletsTranscriptLineIsToolNoise(root map[string]any) bool {
	rootType := jsonPathString(root, "type")
	payload, _ := root["payload"].(map[string]any)
	payloadType := jsonPathString(payload, "type")
	return rootType == "tool_result" ||
		rootType == "function_call" ||
		rootType == "function_call_output" ||
		payloadType == "tool_result" ||
		payloadType == "function_call" ||
		payloadType == "function_call_output"
}

func botletsTranscriptLineUserContent(root map[string]any, runtimeName string) (string, bool) {
	if runtimeName == "claude" && jsonPathString(root, "type") == "user" {
		message, _ := root["message"].(map[string]any)
		if jsonPathString(message, "role") == "user" {
			return jsonValueText(message["content"]), true
		}
	}
	if runtimeName == "codex" && jsonPathString(root, "type") == "response_item" {
		payload, _ := root["payload"].(map[string]any)
		if jsonPathString(payload, "type") == "message" && jsonPathString(payload, "role") == "user" {
			return jsonValueText(payload["content"]), true
		}
	}
	return "", false
}

func jsonPathString(value any, keys ...string) string {
	current := value
	for _, key := range keys {
		object, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = object[key]
	}
	text, _ := current.(string)
	return strings.TrimSpace(text)
}

func jsonValueText(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			if text := jsonValueText(item); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		if text, ok := typed["text"].(string); ok {
			return text
		}
		if text, ok := typed["content"].(string); ok {
			return text
		}
		parts := make([]string, 0, len(typed))
		for _, value := range typed {
			if text := jsonValueText(value); text != "" {
				parts = append(parts, text)
			}
		}
		sort.Strings(parts)
		return strings.Join(parts, "\n")
	default:
		return fmt.Sprint(typed)
	}
}

func shellQuoteArg(value string) string {
	if value != "" && strings.IndexFunc(value, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.' || r == '/' || r == ':' || r == '@')
	}) == -1 {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func openDefaultFile(path string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", path).Start()
	case "linux":
		return exec.Command("xdg-open", path).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", path).Start()
	default:
		return fmt.Errorf("opening files is not supported on %s", runtime.GOOS)
	}
}

func regexpMustCompile(expr string) *regexp.Regexp {
	compiled, err := regexp.Compile(expr)
	if err != nil {
		panic(err)
	}
	return compiled
}
