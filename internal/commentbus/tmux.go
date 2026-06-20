package commentbus

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	tmuxNudgeChunkSize = 512
	tmuxCommandTimeout = 5 * time.Second
)

var (
	TmuxSessionNameRE = regexp.MustCompile(`^comment-[a-z0-9-]{3,80}$`)
	TmuxPaneTargetRE  = regexp.MustCompile(`^(?:%[0-9]{1,10}|comment-[a-z0-9-]{3,80}:[0-9]{1,3}\.[0-9]{1,3})$`)

	ErrTmuxSessionMissing = errors.New("tmux session is not running")
)

type TmuxController interface {
	HasSession(ctx context.Context, sessionName string) (bool, error)
	NewSession(ctx context.Context, options TmuxNewSessionOptions) error
	PaneTarget(ctx context.Context, sessionName string) (string, error)
	PaneBelongsToSession(ctx context.Context, sessionName string, paneTarget string) (bool, error)
	PaneCurrentCommand(ctx context.Context, paneTarget string) (string, error)
	CapturePane(ctx context.Context, paneTarget string, lines int) (string, error)
	SendLiteral(ctx context.Context, paneTarget string, text string) error
	// PasteText pushes multi-line text into the target pane via tmux's
	// paste-buffer (bracketed paste). Unlike SendLiteral, the text MAY
	// contain newlines. The buffer is consumed after pasting. Caller is
	// responsible for sending Enter to submit, just like with SendLiteral.
	PasteText(ctx context.Context, paneTarget string, text string) error
	SendEnter(ctx context.Context, paneTarget string) error
	KillSession(ctx context.Context, sessionName string) error
}

type TmuxNewSessionOptions struct {
	SessionName       string
	WorkingDir        string
	CommentHome       string
	BotletsHome       string
	Command           string
	OutputPipeCommand string
}

type ExecTmuxController struct {
	Binary string
}

// TmuxBinaryEnv pins the tmux binary the daemon uses. Set it to an absolute
// path to a known-good tmux to bypass discovery entirely.
const TmuxBinaryEnv = "COMMENT_IO_TMUX_BIN"

func NewExecTmuxController() ExecTmuxController {
	return ExecTmuxController{Binary: ResolveConfiguredTmuxBinary("")}
}

// ResolveConfiguredTmuxBinary picks the tmux binary name/path to use. An
// explicit value (e.g. the --tmux-bin flag) wins; otherwise COMMENT_IO_TMUX_BIN;
// otherwise the bare name "tmux". The bare name is resolved by command()
// against a fixed set of trusted directories — never the caller's $PATH — so a
// wrapper shim dropped on PATH cannot shadow the real binary.
func ResolveConfiguredTmuxBinary(explicit string) string {
	if v := strings.TrimSpace(explicit); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv(TmuxBinaryEnv)); v != "" {
		return v
	}
	return "tmux"
}

// ResolveDaemonTmuxBinary resolves the absolute tmux path the daemon would use,
// applying the same configured-binary + trusted-directory rules (never $PATH).
// Used by diagnostics (comment doctor) so they report the exact binary the
// daemon runs.
func ResolveDaemonTmuxBinary(explicit string) (string, error) {
	return resolveTrustedExecutableWithSearch(ResolveConfiguredTmuxBinary(explicit), "tmux binary", trustedExecutableSearchDirs(""))
}

// looksLikeWrapperScript reports whether the file at path begins with a "#!"
// shebang — i.e. it is a shell-script wrapper, not a real tmux binary. Some
// environments install a "smart tmux" shim on PATH that rewrites send-keys (and
// breaks the -L socket form); such a shim must never be selected as the
// daemon's tmux.
//
// It stats the path (following symlinks) and only opens it when it is a regular
// file. stat() never blocks, but open() on a FIFO or other special file named
// "tmux" in a trusted dir would hang every tmux operation — so a non-regular
// candidate is reported as "not a wrapper" and left for resolveTrustedExecutable
// to reject normally.
func looksLikeWrapperScript(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var buf [2]byte
	n, _ := io.ReadFull(f, buf[:])
	return n == 2 && buf[0] == '#' && buf[1] == '!'
}

// IsTmuxWrapperScript is the exported form of looksLikeWrapperScript for
// diagnostics.
func IsTmuxWrapperScript(path string) bool { return looksLikeWrapperScript(path) }

func (c ExecTmuxController) HasSession(ctx context.Context, sessionName string) (bool, error) {
	if err := validateTmuxSessionName(sessionName); err != nil {
		return false, err
	}
	socketName, err := TmuxSocketNameForSession(sessionName)
	if err != nil {
		return false, err
	}
	out, err := c.outputCombined(ctx, socketName, "has-session", "-t", sessionName)
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && tmuxHasSessionMissingOutput(string(out)) {
		return false, nil
	}
	return false, err
}

func (c ExecTmuxController) NewSession(ctx context.Context, options TmuxNewSessionOptions) error {
	if err := validateTmuxSessionName(options.SessionName); err != nil {
		return err
	}
	socketName, err := TmuxSocketNameForSession(options.SessionName)
	if err != nil {
		return err
	}
	workingDir, err := resolveTrustedTmuxLaunchDir(options.WorkingDir, "tmux working directory")
	if err != nil {
		return err
	}
	commentHome, err := resolveTrustedTmuxLaunchDir(options.CommentHome, "comment home")
	if err != nil {
		return err
	}
	botletsHome, err := resolveTrustedTmuxLaunchDir(options.BotletsHome, "botlets home")
	if err != nil {
		return err
	}
	if options.Command == "" || strings.ContainsAny(options.Command, "\r\n\x00") || containsSecretValue(options.Command) {
		return errors.New("invalid tmux launch command")
	}
	if strings.ContainsAny(options.OutputPipeCommand, "\r\n\x00") || containsSecretValue(options.OutputPipeCommand) {
		return errors.New("invalid tmux output pipe command")
	}
	args := []string{
		"new-session",
		"-d",
		"-s", options.SessionName,
		"-c", workingDir,
		"-e", "COMMENT_IO_HOME=" + commentHome,
		"-e", "BOTLETS_HOME=" + botletsHome,
	}
	// Forward the daemon-resolved environment (COMMENT_IO_ENV + any base-URL
	// override) into the pane so the session-exec hop and the runtime it execs
	// resolve the same deployment as the daemon. Empty for production.
	for _, entry := range RuntimeEnvironmentVars() {
		args = append(args, "-e", entry)
	}
	args = append(args, "--", options.Command)
	if options.OutputPipeCommand != "" {
		args = append(args, ";", "pipe-pane", "-t", options.SessionName, options.OutputPipeCommand)
	}
	return c.run(ctx, socketName, args...)
}

func (c ExecTmuxController) PaneTarget(ctx context.Context, sessionName string) (string, error) {
	if err := validateTmuxSessionName(sessionName); err != nil {
		return "", err
	}
	socketName, err := TmuxSocketNameForSession(sessionName)
	if err != nil {
		return "", err
	}
	out, err := c.outputCombined(ctx, socketName, "display-message", "-t", sessionName, "-p", "#{session_name}:#{window_index}.#{pane_index}")
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && tmuxHasSessionMissingOutput(string(out)) {
			return "", ErrTmuxSessionMissing
		}
		return "", err
	}
	target := strings.TrimSpace(string(out))
	if err := validateTmuxPaneTarget(target); err != nil {
		return "", err
	}
	return target, nil
}

func (c ExecTmuxController) PaneCurrentCommand(ctx context.Context, paneTarget string) (string, error) {
	if err := validateTmuxPaneTarget(paneTarget); err != nil {
		return "", err
	}
	socketName, err := TmuxSocketNameForPaneTarget(paneTarget)
	if err != nil {
		return "", err
	}
	out, err := c.outputCombined(ctx, socketName, "display-message", "-t", paneTarget, "-p", "#{pane_current_command}")
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && tmuxHasSessionMissingOutput(string(out)) {
			return "", ErrTmuxSessionMissing
		}
		return "", err
	}
	command := strings.TrimSpace(string(out))
	if command == "" || strings.ContainsAny(command, "/\\\r\n\x00") || len(command) > 128 {
		return "", errors.New("invalid tmux pane command")
	}
	return command, nil
}

func (c ExecTmuxController) CapturePane(ctx context.Context, paneTarget string, lines int) (string, error) {
	if err := validateTmuxPaneTarget(paneTarget); err != nil {
		return "", err
	}
	socketName, err := TmuxSocketNameForPaneTarget(paneTarget)
	if err != nil {
		return "", err
	}
	if lines <= 0 {
		lines = 80
	}
	if lines > 500 {
		lines = 500
	}
	out, err := c.outputCombined(ctx, socketName, "capture-pane", "-t", paneTarget, "-p", "-S", "-"+strconv.Itoa(lines))
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && tmuxHasSessionMissingOutput(string(out)) {
			return "", ErrTmuxSessionMissing
		}
		return "", err
	}
	return string(out), nil
}

func (c ExecTmuxController) PaneBelongsToSession(ctx context.Context, sessionName string, paneTarget string) (bool, error) {
	if err := validateTmuxSessionName(sessionName); err != nil {
		return false, err
	}
	if err := validateTmuxPaneTarget(paneTarget); err != nil {
		return false, err
	}
	if targetSession, ok := TmuxPaneTargetSession(paneTarget); ok {
		return targetSession == sessionName, nil
	}
	out, err := c.outputCombined(ctx, "", "display-message", "-t", paneTarget, "-p", "#{session_name}")
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && tmuxHasSessionMissingOutput(string(out)) {
			return false, ErrTmuxSessionMissing
		}
		return false, err
	}
	return strings.TrimSpace(string(out)) == sessionName, nil
}

func (c ExecTmuxController) SendLiteral(ctx context.Context, paneTarget string, text string) error {
	if err := validateTmuxPaneTarget(paneTarget); err != nil {
		return err
	}
	socketName, err := TmuxSocketNameForPaneTarget(paneTarget)
	if err != nil {
		return err
	}
	if text == "" || strings.ContainsAny(text, "\r\n\x00") {
		return errors.New("invalid tmux literal")
	}
	return c.runTarget(ctx, socketName, "send-keys", "-t", paneTarget, "-l", text)
}

func (c ExecTmuxController) SendEnter(ctx context.Context, paneTarget string) error {
	if err := validateTmuxPaneTarget(paneTarget); err != nil {
		return err
	}
	socketName, err := TmuxSocketNameForPaneTarget(paneTarget)
	if err != nil {
		return err
	}
	return c.runTarget(ctx, socketName, "send-keys", "-t", paneTarget, "Enter")
}

// PasteText loads `text` (which MAY contain newlines) into a named tmux
// paste buffer and pastes it into the target pane with bracketed-paste
// mode so the receiving program (e.g. Claude Code) treats the whole block
// as a single paste event rather than line-by-line submissions. The
// buffer is deleted after pasting. Caller is responsible for sending
// Enter to submit the resulting input.
//
// Both the load and the paste go through `c.command()` so they inherit
// the trusted binary path, safeProcessEnv, and the tmux command timeout
// every other tmux operation uses. A per-call named buffer (-b NAME)
// isolates this paste from concurrent calls — tmux's implicit current
// buffer is global state and would race when two startup-orientation
// sends fire for different panes at the same time.
func (c ExecTmuxController) PasteText(ctx context.Context, paneTarget string, text string) error {
	if err := validateTmuxPaneTarget(paneTarget); err != nil {
		return err
	}
	socketName, err := TmuxSocketNameForPaneTarget(paneTarget)
	if err != nil {
		return err
	}
	if text == "" || strings.ContainsRune(text, '\x00') || containsSecretValue(text) {
		return errors.New("invalid tmux paste text")
	}
	bufferName, err := randomTmuxBufferName()
	if err != nil {
		return err
	}
	if err := c.loadTmuxBuffer(ctx, socketName, bufferName, text); err != nil {
		return err
	}
	// -r keeps LF characters in the buffer as LF instead of tmux's
	// default LF→CR replacement (which would turn every newline into an
	// Enter keypress and submit each line as its own prompt — defeats the
	// "single pasted block" intent and matters on TUIs that don't fully
	// implement bracketed paste).
	if err := c.runTarget(ctx, socketName, "paste-buffer", "-b", bufferName, "-p", "-r", "-d", "-t", paneTarget); err != nil {
		// paste-buffer -d only fires on success; clean up the named buffer
		// best-effort so a failed paste doesn't leak tmux server state.
		_, _ = c.outputCombined(ctx, socketName, "delete-buffer", "-b", bufferName)
		return err
	}
	return nil
}

func (c ExecTmuxController) loadTmuxBuffer(ctx context.Context, socketName string, bufferName string, text string) error {
	cmd, commandCtx, cancel, err := c.command(ctx, socketName, "load-buffer", "-b", bufferName, "-")
	if err != nil {
		return err
	}
	defer cancel()
	cmd.Stdin = strings.NewReader(text)
	out, runErr := cmd.CombinedOutput()
	if commandCtx.Err() != nil {
		return commandCtx.Err()
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) && tmuxHasSessionMissingOutput(string(out)) {
			return ErrTmuxSessionMissing
		}
		return fmt.Errorf("tmux load-buffer failed: %w (%s)", runErr, strings.TrimSpace(string(out)))
	}
	return nil
}

func randomTmuxBufferName() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("tmux paste buffer name: %w", err)
	}
	return "comment-paste-" + hex.EncodeToString(bytes), nil
}

func (c ExecTmuxController) KillSession(ctx context.Context, sessionName string) error {
	if err := validateTmuxSessionName(sessionName); err != nil {
		return err
	}
	socketName, err := TmuxSocketNameForSession(sessionName)
	if err != nil {
		return err
	}
	out, err := c.outputCombined(ctx, socketName, "kill-session", "-t", sessionName)
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && tmuxHasSessionMissingOutput(string(out)) {
		return ErrTmuxSessionMissing
	}
	return err
}

func (c ExecTmuxController) run(ctx context.Context, socketName string, args ...string) error {
	_, err := c.output(ctx, socketName, args...)
	return err
}

func (c ExecTmuxController) runTarget(ctx context.Context, socketName string, args ...string) error {
	out, err := c.outputCombined(ctx, socketName, args...)
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && tmuxHasSessionMissingOutput(string(out)) {
		return ErrTmuxSessionMissing
	}
	return err
}

func (c ExecTmuxController) output(ctx context.Context, socketName string, args ...string) ([]byte, error) {
	cmd, commandCtx, cancel, err := c.command(ctx, socketName, args...)
	if err != nil {
		return nil, err
	}
	defer cancel()
	out, err := cmd.Output()
	if commandCtx.Err() != nil {
		return nil, commandCtx.Err()
	}
	return out, err
}

func (c ExecTmuxController) outputCombined(ctx context.Context, socketName string, args ...string) ([]byte, error) {
	cmd, commandCtx, cancel, err := c.command(ctx, socketName, args...)
	if err != nil {
		return nil, err
	}
	defer cancel()
	out, err := cmd.CombinedOutput()
	if commandCtx.Err() != nil {
		return nil, commandCtx.Err()
	}
	return out, err
}

func (c ExecTmuxController) command(ctx context.Context, socketName string, args ...string) (*exec.Cmd, context.Context, context.CancelFunc, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, tmuxCommandTimeout)
	if socketName != "" {
		if err := validateTmuxSocketName(socketName); err != nil {
			cancel()
			return nil, nil, nil, err
		}
		args = append([]string{"-L", socketName}, args...)
	}
	binary := c.Binary
	if binary == "" {
		// Zero-value controllers (e.g. comment uninstall's fallback cleanup)
		// must still honor an explicit COMMENT_IO_TMUX_BIN pin rather than
		// silently normalizing to bare "tmux".
		binary = ResolveConfiguredTmuxBinary("")
	}
	binary, err := resolveTrustedExecutableWithSearch(binary, "tmux binary", trustedExecutableSearchDirs(""))
	if err != nil {
		cancel()
		// A bare "tmux" that isn't in any trusted directory means tmux is not
		// installed; translate it into ErrTmuxNotInstalled so callers can show a
		// platform-specific install hint instead of an opaque launch failure. An
		// unusable explicit pin (path with a separator) does NOT wrap the
		// not-found sentinel and stays a plain error.
		if errors.Is(err, errTrustedExecutableNotFound) {
			return nil, nil, nil, fmt.Errorf("%w: %v", ErrTmuxNotInstalled, err)
		}
		return nil, nil, nil, err
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = safeProcessEnv(os.Environ())
	return cmd, ctx, cancel, nil
}

// TmuxSocketNameForSession returns the per-session tmux socket used to isolate
// managed runtimes from the default shared tmux server.
func TmuxSocketNameForSession(sessionName string) (string, error) {
	if err := validateTmuxSessionName(sessionName); err != nil {
		return "", err
	}
	socketName := sessionName
	if err := validateTmuxSocketName(socketName); err != nil {
		return "", err
	}
	return socketName, nil
}

// TmuxSocketNameForPaneTarget derives the isolated tmux socket from a
// session-qualified pane target. Raw %pane targets are legacy/default-server
// references and intentionally return an empty socket name.
func TmuxSocketNameForPaneTarget(paneTarget string) (string, error) {
	if err := validateTmuxPaneTarget(paneTarget); err != nil {
		return "", err
	}
	sessionName, ok := TmuxPaneTargetSession(paneTarget)
	if !ok {
		return "", nil
	}
	return TmuxSocketNameForSession(sessionName)
}

// TmuxPaneTargetSession extracts the session name from a
// session:window.pane target.
func TmuxPaneTargetSession(paneTarget string) (string, bool) {
	index := strings.IndexRune(paneTarget, ':')
	if index <= 0 {
		return "", false
	}
	sessionName := paneTarget[:index]
	if validateTmuxSessionName(sessionName) != nil {
		return "", false
	}
	return sessionName, true
}

func tmuxHasSessionMissingOutput(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "can't find session") ||
		strings.Contains(lower, "can't find pane") ||
		strings.Contains(lower, "can't find window") ||
		strings.Contains(lower, "no server running") ||
		strings.Contains(lower, "no such file or directory")
}

type SessionNudgeLocks struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func (l *SessionNudgeLocks) lock(sessionName string) func() {
	l.mu.Lock()
	if l.locks == nil {
		l.locks = map[string]*sync.Mutex{}
	}
	lock := l.locks[sessionName]
	if lock == nil {
		lock = &sync.Mutex{}
		l.locks[sessionName] = lock
	}
	l.mu.Unlock()
	lock.Lock()
	return lock.Unlock
}

func tmuxSessionName(botName string, sessionID string) (string, error) {
	if !isBotName(botName) {
		return "", errors.New("invalid bot name")
	}
	if !LocalSessionIDRE.MatchString(sessionID) {
		return "", errors.New("invalid session id")
	}
	sum := sha256.Sum256([]byte(sessionID))
	suffix := hex.EncodeToString(sum[:])[:12]
	name := "comment-" + botName + "-" + suffix
	if err := validateTmuxSessionName(name); err != nil {
		return "", err
	}
	return name, nil
}

func sessionExecCommand(commentBinary string, sessionID string, generation string) (string, error) {
	if !LocalSessionIDRE.MatchString(sessionID) || !LocalSessionGenerationIDRE.MatchString(generation) {
		return "", errors.New("invalid managed-session id")
	}
	if !isSafeAbsoluteLocalPath(commentBinary) {
		return "", errors.New("invalid comment binary path")
	}
	return shellQuote(commentBinary) + " session-exec " + sessionID + " " + generation, nil
}

func formatTmuxNudge(botName string, messageID string) (string, error) {
	if !isBotName(botName) || !LocalMessageIDRE.MatchString(messageID) {
		return "", errors.New("invalid nudge input")
	}
	text := "# comment.io message for " + botName + ": run comment messages receive " + messageID + ". If no visible reply is needed run comment activity complete " + messageID
	if strings.ContainsAny(text, "`\"'$;&|<>\r\n\x00") {
		return "", errors.New("unsafe nudge text")
	}
	return text, nil
}

func chunkTmuxText(text string, maxSize int) []string {
	if maxSize <= 0 || len(text) <= maxSize {
		return []string{text}
	}
	chunks := make([]string, 0, (len(text)+maxSize-1)/maxSize)
	for len(text) > 0 {
		end := maxSize
		if len(text) < end {
			end = len(text)
		}
		chunks = append(chunks, text[:end])
		text = text[end:]
	}
	return chunks
}

func validateTmuxSessionName(value string) error {
	if !TmuxSessionNameRE.MatchString(value) || containsSecretValue(value) {
		return errors.New("invalid tmux session name")
	}
	return nil
}

func validateTmuxSocketName(value string) error {
	if !TmuxSessionNameRE.MatchString(value) || strings.ContainsRune(value, filepath.Separator) || containsSecretValue(value) {
		return errors.New("invalid tmux socket name")
	}
	return nil
}

func validateTmuxPaneTarget(value string) error {
	if !TmuxPaneTargetRE.MatchString(value) || containsSecretValue(value) {
		return errors.New("invalid tmux pane target")
	}
	return nil
}

func isSafeAbsoluteLocalPath(value string) bool {
	return value != "" && len(value) <= 4096 && filepath.IsAbs(value) && !strings.ContainsAny(value, "\r\n\x00") && !containsSecretValue(value)
}

func resolveTrustedExecutable(value string, label string) (string, error) {
	if value == "" || strings.ContainsAny(value, "\r\n\x00") || containsSecretValue(value) {
		return "", errors.New("invalid " + label)
	}
	resolved := value
	if !strings.ContainsRune(value, filepath.Separator) {
		path, err := exec.LookPath(value)
		if err != nil {
			return "", err
		}
		resolved = path
	} else if !filepath.IsAbs(value) {
		return "", errors.New(label + " must be absolute")
	}
	if !isSafeAbsoluteLocalPath(resolved) {
		return "", errors.New("invalid " + label)
	}
	resolved = filepath.Clean(resolved)
	info, err := os.Lstat(resolved)
	if err != nil {
		return "", errors.New(label + " must exist")
	}
	if err := validateTrustedExecutableReferencePath(resolved, info, label); err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		resolved, err = filepath.EvalSymlinks(resolved)
		if err != nil {
			return "", err
		}
		if err := validateTrustedExecutablePath(resolved, label); err != nil {
			return "", err
		}
		return resolved, nil
	}
	resolved, err = filepath.EvalSymlinks(resolved)
	if err != nil {
		return "", err
	}
	if err := validateTrustedExecutablePath(resolved, label); err != nil {
		return "", err
	}
	return resolved, nil
}

func resolveTrustedExecutableWithSearch(value string, label string, searchDirs []string) (string, error) {
	return resolveTrustedExecutableWithSearchAndEnv(value, label, searchDirs, TmuxBinaryEnv)
}

func resolveTrustedExecutableWithSearchAndEnv(value string, label string, searchDirs []string, envVar string) (string, error) {
	if value == "" || strings.ContainsAny(value, "\r\n\x00") || containsSecretValue(value) {
		return "", errors.New("invalid " + label)
	}
	if strings.ContainsRune(value, filepath.Separator) {
		return resolveTrustedExecutable(value, label)
	}
	for _, dir := range searchDirs {
		candidate := filepath.Join(dir, value)
		if _, err := os.Lstat(candidate); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return "", err
		}
		// Skip shell-script wrapper shims so they can't shadow a real binary
		// that lives later in the search path.
		if looksLikeWrapperScript(candidate) {
			continue
		}
		return resolveTrustedExecutable(candidate, label)
	}
	// Auto-discovery is limited to the fixed trusted directories — deliberately
	// NOT $PATH. Falling back to exec.LookPath here would let a shim that is
	// merely first on the caller's PATH (script or compiled) become the daemon's
	// tmux, defeating the trusted-dir guarantee. Fail loudly instead so the
	// operator installs a real binary or pins one with the relevant binary env.
	// Wrap errTrustedExecutableNotFound so the tmux layer can distinguish a
	// missing binary (→ install hint) from an unusable explicit pin.
	return "", fmt.Errorf("%w: %s not found in trusted directories; install it on a standard path or set %s to an absolute path", errTrustedExecutableNotFound, label, envVar)
}

func resolveTrustedTmuxLaunchDir(path string, label string) (string, error) {
	if !isSafeAbsoluteLocalPath(path) {
		return "", errors.New("invalid " + label)
	}
	path = filepath.Clean(path)
	if err := validateTrustedOriginalPathDir(filepath.Dir(path), label+" parent"); err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", errors.New(label + " must exist")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New(label + " must not be a symlink")
	}
	if !info.IsDir() {
		return "", errors.New(label + " must be a directory")
	}
	if err := validateCurrentUserOwner(info, label); err != nil {
		return "", err
	}
	if info.Mode().Perm()&0o022 != 0 {
		return "", errors.New(label + " must not be group- or world-writable")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if err := validateTrustedLaunchDirPath(resolved, label); err != nil {
		return "", err
	}
	return resolved, nil
}

func trustedRuntimeSearchPath(runtimePath string, extraDirs ...string) string {
	return strings.Join(trustedExecutableSearchDirs(runtimePath, extraDirs...), string(os.PathListSeparator))
}

func trustedExecutableSearchDirs(runtimePath string, extraDirs ...string) []string {
	candidates := []string{}
	candidates = append(candidates, extraDirs...)
	if runtimePath != "" {
		candidates = append(candidates, filepath.Dir(runtimePath))
	}
	candidates = append(candidates,
		"/opt/homebrew/bin",
		"/usr/local/bin",
		"/usr/bin",
		"/bin",
		"/usr/sbin",
		"/sbin",
	)
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates,
			filepath.Join(home, ".local", "bin"),
			filepath.Join(home, "bin"),
		)
	}
	seen := map[string]struct{}{}
	trusted := []string{}
	for _, candidate := range candidates {
		candidate = filepath.Clean(candidate)
		if candidate == "." {
			continue
		}
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		if err := validateTrustedSearchPathDir(candidate, "runtime PATH directory"); err != nil {
			continue
		}
		trusted = append(trusted, candidate)
	}
	return trusted
}

func safeProcessEnv(base []string) []string {
	out := make([]string, 0, len(base))
	seen := map[string]struct{}{}
	for _, entry := range base {
		key := envKey(entry)
		value := envValue(entry)
		if _, ok := seen[key]; ok {
			continue
		}
		if !isAllowedProcessEnvKey(key) || !isSafeProcessEnvValue(value) {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key+"="+value)
	}
	return out
}

func envKey(entry string) string {
	for i := 0; i < len(entry); i++ {
		if entry[i] == '=' {
			return entry[:i]
		}
	}
	return entry
}

func isAllowedProcessEnvKey(key string) bool {
	switch key {
	case "HOME", "USER", "LOGNAME", "TERM", "TERM_PROGRAM", "COLORTERM", "LANG":
		return true
	default:
		return strings.HasPrefix(key, "LC_")
	}
}

func isSafeProcessEnvValue(value string) bool {
	return len(value) <= 4096 && !strings.ContainsAny(value, "\r\n\x00") && !containsSecretValue(value)
}

func envValue(entry string) string {
	for i := 0; i < len(entry); i++ {
		if entry[i] == '=' {
			return entry[i+1:]
		}
	}
	return ""
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func commentExecutablePath() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	return expandHome(executable)
}
