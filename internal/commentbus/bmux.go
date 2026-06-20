package commentbus

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	BmuxBinaryEnv       = "COMMENT_IO_BMUX_BIN"
	bmuxCommandTimeout  = 5 * time.Second
	bmuxLaunchRows      = 24
	bmuxLaunchCols      = 80
	bmuxProtocolVersion = 2
	bmuxMaxFrame        = 1024 * 1024
	bmuxMsgHello        = 0x01
	bmuxMsgSubscribe    = 0x05
	bmuxMsgOK           = 0x81
	bmuxMsgError        = 0x82
	bmuxMsgOutput       = 0x84
	bmuxMsgSessionEnd   = 0x85
)

const bmuxChildShell = "/bin/sh"

var (
	bmuxSocketWaitLimit                       = 2 * time.Second
	bmuxProcessIdentityFn                     = bmuxProcessIdentity
	terminatePersistedBmuxChildProcessGroupFn = terminatePersistedBmuxChildProcessGroup
)

type ExecBmuxController struct {
	Binary string
	Paths  Paths
}

func NewExecBmuxController(paths Paths, explicit string) ExecBmuxController {
	return ExecBmuxController{Binary: ResolveConfiguredBmuxBinary(explicit), Paths: paths}
}

func ResolveConfiguredBmuxBinary(explicit string) string {
	if v := strings.TrimSpace(explicit); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv(BmuxBinaryEnv)); v != "" {
		return v
	}
	return "bmux"
}

func TrustedBmuxBinaryPath(explicit string) (string, error) {
	return resolveTrustedExecutableWithSearchAndEnv(ResolveConfiguredBmuxBinary(explicit), "bmux binary", trustedExecutableSearchDirs(""), BmuxBinaryEnv)
}

func BmuxSocketPathForSession(paths Paths, sessionName string) (string, error) {
	if err := validateTmuxSessionName(sessionName); err != nil {
		return "", err
	}
	dir, err := bmuxControlDir(paths)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, sessionName+".sock"), nil
}

func BmuxTokenFileForSession(paths Paths, sessionName string) (string, error) {
	if err := validateTmuxSessionName(sessionName); err != nil {
		return "", err
	}
	dir, err := bmuxControlDir(paths)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, sessionName+".token"), nil
}

func bmuxControlDir(paths Paths) (string, error) {
	if paths.Bus == "" {
		return "", errors.New("comment bus path is not configured")
	}
	dir := filepath.Join(paths.Bus, "bmux")
	if !isSafeAbsoluteLocalPath(dir) {
		return "", errors.New("invalid bmux control directory")
	}
	return dir, nil
}

func (c ExecBmuxController) HasSession(ctx context.Context, sessionName string) (bool, error) {
	_, err := c.status(ctx, sessionName)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, ErrTmuxSessionMissing) {
		return false, nil
	}
	return false, err
}

func (c ExecBmuxController) BmuxStatus(ctx context.Context, sessionName string) (bmuxStatus, error) {
	return c.status(ctx, sessionName)
}

func (c ExecBmuxController) NewSession(ctx context.Context, options TmuxNewSessionOptions) error {
	if err := validateTmuxSessionName(options.SessionName); err != nil {
		return err
	}
	workingDir, err := resolveTrustedTmuxLaunchDir(options.WorkingDir, "bmux working directory")
	if err != nil {
		return err
	}
	if _, err := resolveTrustedTmuxLaunchDir(options.CommentHome, "comment home"); err != nil {
		return err
	}
	if _, err := resolveTrustedTmuxLaunchDir(options.BotletsHome, "botlets home"); err != nil {
		return err
	}
	if options.Command == "" || strings.ContainsAny(options.Command, "\r\n\x00") || containsSecretValue(options.Command) {
		return errors.New("invalid bmux launch command")
	}
	if strings.ContainsAny(options.OutputPipeCommand, "\r\n\x00") || containsSecretValue(options.OutputPipeCommand) {
		return errors.New("invalid bmux output pipe command")
	}
	socketPath, err := BmuxSocketPathForSession(c.Paths, options.SessionName)
	if err != nil {
		return err
	}
	tokenFile, err := BmuxTokenFileForSession(c.Paths, options.SessionName)
	if err != nil {
		return err
	}
	if bmuxSocketHasLiveServer(socketPath) {
		return errors.New("bmux session socket is already in use")
	}
	if err := writeBmuxTokenFile(tokenFile); err != nil {
		return err
	}
	binary, err := c.binaryPath()
	if err != nil {
		return err
	}
	launcher := strings.Join([]string{
		shellQuote(binary),
		"-S", shellQuote(socketPath),
		"--token-file", shellQuote(tokenFile),
		"--rows", strconv.Itoa(bmuxLaunchRows),
		"--cols", strconv.Itoa(bmuxLaunchCols),
		"--", shellQuote(bmuxChildShell), "-lc", shellQuote(options.Command),
	}, " ")
	if options.OutputPipeCommand != "" {
		launcher = launcher + " | " + options.OutputPipeCommand
	} else {
		launcher = launcher + " >/dev/null"
	}
	cmd := exec.Command("/bin/sh", "-c", launcher)
	configureDetachedBmuxCommand(cmd)
	cmd.Dir = workingDir
	cmd.Env = append(
		append(safeProcessEnv(os.Environ()), RuntimeEnvironmentVars()...),
		"COMMENT_IO_HOME="+options.CommentHome,
		"BOTLETS_HOME="+options.BotletsHome,
	)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return err
	}
	waitDone := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(waitDone)
	}()
	status, err := c.waitForBmuxReady(ctx, options.SessionName)
	if err != nil {
		c.cleanupFailedBmuxLaunch(options.SessionName, cmd, waitDone)
		return err
	}
	if err := c.writeBmuxSessionCredential(options.SessionName, status); err != nil {
		c.cleanupFailedBmuxLaunch(options.SessionName, cmd, waitDone)
		return err
	}
	return nil
}

func writeBmuxTokenFile(path string) error {
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return fmt.Errorf("generate bmux token: %w", err)
	}
	return WritePrivateFileAtomic(path, []byte(hex.EncodeToString(token)+"\n"), 0o600)
}

func (c ExecBmuxController) waitForBmuxReady(ctx context.Context, sessionName string) (bmuxStatus, error) {
	deadline := time.Now().Add(bmuxSocketWaitLimit)
	var lastErr error
	for {
		if ctx != nil && ctx.Err() != nil {
			return bmuxStatus{}, ctx.Err()
		}
		if status, err := c.status(ctx, sessionName); err == nil {
			return status, nil
		} else {
			if !errors.Is(err, ErrTmuxSessionMissing) {
				return bmuxStatus{}, err
			}
			lastErr = err
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return bmuxStatus{}, fmt.Errorf("bmux session did not become ready: %w", lastErr)
			}
			return bmuxStatus{}, errors.New("bmux session did not become ready")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (c ExecBmuxController) writeBmuxSessionCredential(sessionName string, status bmuxStatus) error {
	if status.childPID == 0 {
		return errors.New("bmux status missing child pid")
	}
	if status.childPGID == 0 {
		return errors.New("bmux status missing child process group")
	}
	credential, err := readBmuxSessionCredential(c.Paths, sessionName)
	if err != nil {
		return err
	}
	identity, err := bmuxProcessIdentityFn(status.childPID)
	if err != nil {
		return fmt.Errorf("identify bmux child process: %w", err)
	}
	credential.SessionName = sessionName
	credential.ChildPID = status.childPID
	credential.ChildPGID = status.childPGID
	credential.ChildIdentity = identity
	return writeBmuxSessionCredential(c.Paths, sessionName, credential)
}

func bmuxSocketHasLiveServer(socketPath string) bool {
	conn, err := net.DialTimeout("unix", socketPath, 100*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func (c ExecBmuxController) cleanupFailedBmuxLaunch(sessionName string, cmd *exec.Cmd, waitDone <-chan struct{}) {
	_ = c.KillSession(context.Background(), sessionName)
	terminateDetachedBmuxCommand(cmd)
	select {
	case <-waitDone:
	case <-time.After(500 * time.Millisecond):
	}
}

func (c ExecBmuxController) PaneTarget(_ context.Context, sessionName string) (string, error) {
	return BmuxSocketPathForSession(c.Paths, sessionName)
}

func (c ExecBmuxController) PaneBelongsToSession(_ context.Context, sessionName string, paneTarget string) (bool, error) {
	socketPath, err := BmuxSocketPathForSession(c.Paths, sessionName)
	if err != nil {
		return false, err
	}
	return filepath.Clean(paneTarget) == filepath.Clean(socketPath), nil
}

func (c ExecBmuxController) PaneCurrentCommand(ctx context.Context, paneTarget string) (string, error) {
	sessionName, err := c.sessionNameForSocket(paneTarget)
	if err != nil {
		return "", err
	}
	status, err := c.status(ctx, sessionName)
	if err != nil {
		return "", err
	}
	if !status.childAlive {
		return "", ErrTmuxSessionMissing
	}
	return foregroundCommandName(status.foreground), nil
}

func foregroundCommandName(command string) string {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return ""
	}
	return filepath.Base(fields[0])
}

func (c ExecBmuxController) CapturePane(context.Context, string, int) (string, error) {
	return "", errors.New("bmux does not provide rendered screen capture")
}

func (c ExecBmuxController) SendLiteral(ctx context.Context, paneTarget string, text string) error {
	if text == "" || strings.ContainsAny(text, "\r\n\x00") || containsSecretValue(text) {
		return errors.New("invalid bmux literal")
	}
	return c.sendRaw(ctx, paneTarget, text)
}

func (c ExecBmuxController) sendRaw(ctx context.Context, paneTarget string, text string) error {
	if text == "" || strings.ContainsRune(text, '\x00') || containsSecretValue(text) {
		return errors.New("invalid bmux input")
	}
	sessionName, err := c.sessionNameForSocket(paneTarget)
	if err != nil {
		return err
	}
	return c.run(ctx, sessionName, "send-keys", "-l", text)
}

func (c ExecBmuxController) PasteText(ctx context.Context, paneTarget string, text string) error {
	if text == "" || strings.ContainsRune(text, '\x00') || containsSecretValue(text) {
		return errors.New("invalid bmux paste text")
	}
	return c.sendRaw(ctx, paneTarget, "\x1b[200~"+text+"\x1b[201~")
}

func (c ExecBmuxController) SendEnter(ctx context.Context, paneTarget string) error {
	sessionName, err := c.sessionNameForSocket(paneTarget)
	if err != nil {
		return err
	}
	return c.run(ctx, sessionName, "send-keys", "-l", "\r")
}

func (c ExecBmuxController) KillSession(ctx context.Context, sessionName string) error {
	err := c.run(ctx, sessionName, "kill")
	if err == nil {
		return removeBmuxSessionCredential(c.Paths, sessionName)
	}
	if errors.Is(err, ErrTmuxSessionMissing) ||
		c.sessionGoneAfterCredentialRemoval(sessionName) ||
		c.sessionSocketMissing(sessionName) {
		fallbackErr := c.killPersistedChildProcessGroup(sessionName)
		if fallbackErr == nil {
			return removeBmuxSessionCredential(c.Paths, sessionName)
		}
		if errors.Is(fallbackErr, ErrTmuxSessionMissing) {
			return removeBmuxSessionCredential(c.Paths, sessionName)
		}
		return fallbackErr
	}
	return err
}

func (c ExecBmuxController) sessionSocketMissing(sessionName string) bool {
	socketPath, err := BmuxSocketPathForSession(c.Paths, sessionName)
	if err != nil {
		return false
	}
	return !bmuxSocketHasLiveServer(socketPath)
}

func (c ExecBmuxController) sessionGoneAfterCredentialRemoval(sessionName string) bool {
	tokenFile, err := BmuxTokenFileForSession(c.Paths, sessionName)
	if err != nil {
		return false
	}
	if _, err := os.Stat(tokenFile); !errors.Is(err, os.ErrNotExist) {
		return false
	}
	return c.sessionSocketMissing(sessionName)
}

func (c ExecBmuxController) killPersistedChildProcessGroup(sessionName string) error {
	credential, err := readBmuxSessionCredential(c.Paths, sessionName)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrTmuxSessionMissing
		}
		return err
	}
	if credential.ChildPID == 0 || credential.ChildPGID == 0 || credential.ChildIdentity == "" {
		return ErrTmuxSessionMissing
	}
	currentIdentity, err := bmuxProcessIdentityFn(credential.ChildPID)
	if err != nil {
		return err
	}
	if currentIdentity != credential.ChildIdentity {
		return ErrTmuxSessionMissing
	}
	return terminatePersistedBmuxChildProcessGroupFn(credential.ChildPID, credential.ChildPGID, credential.ChildIdentity)
}

func (c ExecBmuxController) WaitForOutput(ctx context.Context, sessionName string, needle []byte, timeout time.Duration) (bool, error) {
	if len(needle) == 0 {
		return true, nil
	}
	conn, err := c.subscribe(ctx, sessionName)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	if timeout <= 0 {
		timeout = transientRuntimeStartupInputReadyWait
	}
	deadline := time.Now().Add(timeout)
	tail := []byte{}
	for {
		if ctx != nil && ctx.Err() != nil {
			return false, ctx.Err()
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		frame, err := readBmuxFrameUntil(conn, 100*time.Millisecond, deadline)
		if err != nil {
			if isNetTimeout(err) || errors.Is(err, context.DeadlineExceeded) {
				continue
			}
			if errors.Is(err, io.EOF) {
				return false, ErrTmuxSessionMissing
			}
			return false, err
		}
		if len(frame) == 0 {
			continue
		}
		switch frame[0] {
		case bmuxMsgOutput:
			tail = append(tail, frame[1:]...)
			if bytesContains(tail, needle) {
				return true, nil
			}
			if keep := len(needle) - 1; keep > 0 && len(tail) > keep {
				tail = append([]byte(nil), tail[len(tail)-keep:]...)
			}
		case bmuxMsgSessionEnd:
			return false, nil
		case bmuxMsgError:
			return false, errors.New(string(frame[1:]))
		}
	}
}

func (c ExecBmuxController) subscribe(ctx context.Context, sessionName string) (net.Conn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateTmuxSessionName(sessionName); err != nil {
		return nil, err
	}
	socketPath, err := BmuxSocketPathForSession(c.Paths, sessionName)
	if err != nil {
		return nil, err
	}
	token, err := readBmuxToken(c.Paths, sessionName)
	if err != nil {
		return nil, err
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, ErrTmuxSessionMissing
	}
	if err := writeBmuxFrame(conn, append([]byte{bmuxMsgHello, bmuxProtocolVersion}, token[:]...)); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := expectBmuxOK(conn, "bmux hello"); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := writeBmuxFrame(conn, []byte{bmuxMsgSubscribe}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := expectBmuxOK(conn, "bmux subscribe"); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func readBmuxToken(paths Paths, sessionName string) ([32]byte, error) {
	credential, err := readBmuxSessionCredential(paths, sessionName)
	if err != nil {
		return [32]byte{}, err
	}
	decoded, err := hex.DecodeString(credential.TokenHex)
	if err != nil {
		return [32]byte{}, err
	}
	if len(decoded) != 32 {
		return [32]byte{}, errors.New("invalid bmux token length")
	}
	var token [32]byte
	copy(token[:], decoded)
	return token, nil
}

type bmuxSessionCredential struct {
	TokenHex      string
	SessionName   string
	ChildPID      uint32
	ChildPGID     uint32
	ChildIdentity string
}

func readBmuxSessionCredential(paths Paths, sessionName string) (bmuxSessionCredential, error) {
	tokenFile, err := BmuxTokenFileForSession(paths, sessionName)
	if err != nil {
		return bmuxSessionCredential{}, err
	}
	file, err := OpenPrivateFile(paths.Bus, tokenFile, "bmux token file")
	if err != nil {
		return bmuxSessionCredential{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 4096))
	if err != nil {
		return bmuxSessionCredential{}, err
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return bmuxSessionCredential{}, errors.New("missing bmux token")
	}
	credential := bmuxSessionCredential{TokenHex: strings.TrimSpace(lines[0])}
	decoded, err := hex.DecodeString(credential.TokenHex)
	if err != nil {
		return bmuxSessionCredential{}, err
	}
	if len(decoded) != 32 {
		return bmuxSessionCredential{}, errors.New("invalid bmux token length")
	}
	for _, line := range lines[1:] {
		key, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch key {
		case "session_name":
			if err := validateTmuxSessionName(value); err != nil {
				return bmuxSessionCredential{}, err
			}
			if value != sessionName {
				return bmuxSessionCredential{}, errors.New("bmux credential session mismatch")
			}
			credential.SessionName = value
		case "child_pid":
			pid, err := strconv.ParseUint(value, 10, 32)
			if err != nil {
				return bmuxSessionCredential{}, err
			}
			credential.ChildPID = uint32(pid)
		case "child_pgid":
			pgid, err := strconv.ParseUint(value, 10, 32)
			if err != nil {
				return bmuxSessionCredential{}, err
			}
			credential.ChildPGID = uint32(pgid)
		case "child_identity":
			if value == "" || strings.ContainsAny(value, "\r\n\x00") {
				return bmuxSessionCredential{}, errors.New("invalid bmux child process identity")
			}
			credential.ChildIdentity = value
		}
	}
	return credential, nil
}

func writeBmuxSessionCredential(paths Paths, sessionName string, credential bmuxSessionCredential) error {
	if err := validateTmuxSessionName(sessionName); err != nil {
		return err
	}
	if credential.SessionName != sessionName {
		return errors.New("bmux credential session mismatch")
	}
	decoded, err := hex.DecodeString(credential.TokenHex)
	if err != nil {
		return err
	}
	if len(decoded) != 32 {
		return errors.New("invalid bmux token length")
	}
	tokenFile, err := BmuxTokenFileForSession(paths, sessionName)
	if err != nil {
		return err
	}
	if credential.ChildIdentity == "" || strings.ContainsAny(credential.ChildIdentity, "\r\n\x00") {
		return errors.New("invalid bmux child process identity")
	}
	data := fmt.Sprintf("%s\nsession_name %s\nchild_pid %d\nchild_pgid %d\nchild_identity %s\n", credential.TokenHex, credential.SessionName, credential.ChildPID, credential.ChildPGID, credential.ChildIdentity)
	return WritePrivateFileAtomic(tokenFile, []byte(data), 0o600)
}

func removeBmuxSessionCredential(paths Paths, sessionName string) error {
	tokenFile, err := BmuxTokenFileForSession(paths, sessionName)
	if err != nil {
		return err
	}
	if err := os.Remove(tokenFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func expectBmuxOK(conn net.Conn, label string) error {
	_ = conn.SetReadDeadline(time.Now().Add(bmuxCommandTimeout))
	frame, err := readBmuxFrame(conn)
	if err != nil {
		return err
	}
	if len(frame) == 0 {
		return errors.New(label + " returned empty frame")
	}
	switch frame[0] {
	case bmuxMsgOK:
		return nil
	case bmuxMsgError:
		return errors.New(string(frame[1:]))
	default:
		return fmt.Errorf("%s returned unexpected frame 0x%x", label, frame[0])
	}
}

func writeBmuxFrame(w io.Writer, payload []byte) error {
	if len(payload) == 0 || len(payload) > bmuxMaxFrame {
		return errors.New("invalid bmux frame")
	}
	var prefix [4]byte
	binary.LittleEndian.PutUint32(prefix[:], uint32(len(payload)))
	if _, err := w.Write(prefix[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func readBmuxFrame(r io.Reader) ([]byte, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return nil, err
	}
	length := binary.LittleEndian.Uint32(prefix[:])
	if length == 0 || length > bmuxMaxFrame {
		return nil, errors.New("invalid bmux frame length")
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func readBmuxFrameUntil(conn net.Conn, step time.Duration, deadline time.Time) ([]byte, error) {
	var prefix [4]byte
	if err := readBmuxFullUntil(conn, prefix[:], step, deadline, true); err != nil {
		return nil, err
	}
	length := binary.LittleEndian.Uint32(prefix[:])
	if length == 0 || length > bmuxMaxFrame {
		return nil, errors.New("invalid bmux frame length")
	}
	payload := make([]byte, length)
	if err := readBmuxFullUntil(conn, payload, step, deadline, false); err != nil {
		return nil, err
	}
	return payload, nil
}

func readBmuxFullUntil(conn net.Conn, buf []byte, step time.Duration, deadline time.Time, returnIdleTimeout bool) error {
	filled := 0
	for filled < len(buf) {
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return context.DeadlineExceeded
		}
		readDeadline := time.Now().Add(step)
		if !deadline.IsZero() && readDeadline.After(deadline) {
			readDeadline = deadline
		}
		_ = conn.SetReadDeadline(readDeadline)
		n, err := conn.Read(buf[filled:])
		if n > 0 {
			filled += n
			if filled == len(buf) {
				return nil
			}
		}
		if err == nil {
			if n == 0 {
				return io.ErrNoProgress
			}
			continue
		}
		if isNetTimeout(err) {
			if filled == 0 && returnIdleTimeout {
				return err
			}
			continue
		}
		if errors.Is(err, io.EOF) && filled > 0 {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	return nil
}

func isNetTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func bytesContains(haystack []byte, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	if len(haystack) < len(needle) {
		return false
	}
	for i := 0; i <= len(haystack)-len(needle); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func (c ExecBmuxController) sessionNameForSocket(paneTarget string) (string, error) {
	if err := validatePaneTargetForHost(SessionHostBmux, paneTarget); err != nil {
		return "", err
	}
	base := filepath.Base(paneTarget)
	sessionName := strings.TrimSuffix(base, ".sock")
	if err := validateTmuxSessionName(sessionName); err != nil {
		return "", err
	}
	socketPath, err := BmuxSocketPathForSession(c.Paths, sessionName)
	if err != nil {
		return "", err
	}
	if filepath.Clean(socketPath) != filepath.Clean(paneTarget) {
		return "", errors.New("bmux socket path does not match session")
	}
	return sessionName, nil
}

type bmuxStatus struct {
	childAlive bool
	childPID   uint32
	childPGID  uint32
	foreground string
}

func (c ExecBmuxController) status(ctx context.Context, sessionName string) (bmuxStatus, error) {
	out, err := c.output(ctx, sessionName, "status")
	if err != nil {
		return bmuxStatus{}, err
	}
	status := bmuxStatus{}
	for _, line := range strings.Split(string(out), "\n") {
		key, value, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		switch key {
		case "pid":
			if parsed, err := strconv.ParseUint(value, 10, 32); err == nil {
				status.childPID = uint32(parsed)
			}
		case "child_alive":
			status.childAlive = value == "true"
		case "child_pgid":
			if parsed, err := strconv.ParseUint(value, 10, 32); err == nil {
				status.childPGID = uint32(parsed)
			}
		case "foreground":
			status.foreground = strings.TrimSpace(value)
		}
	}
	return status, nil
}

func (c ExecBmuxController) run(ctx context.Context, sessionName string, args ...string) error {
	_, err := c.output(ctx, sessionName, args...)
	return err
}

func (c ExecBmuxController) output(ctx context.Context, sessionName string, args ...string) ([]byte, error) {
	cmd, commandCtx, cancel, err := c.command(ctx, sessionName, args...)
	if err != nil {
		return nil, err
	}
	defer cancel()
	out, err := cmd.CombinedOutput()
	if commandCtx.Err() != nil {
		return nil, commandCtx.Err()
	}
	if err != nil {
		if bmuxMissingSessionOutput(string(out)) {
			return out, ErrTmuxSessionMissing
		}
		return out, fmt.Errorf("bmux %s failed: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (c ExecBmuxController) command(ctx context.Context, sessionName string, args ...string) (*exec.Cmd, context.Context, context.CancelFunc, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateTmuxSessionName(sessionName); err != nil {
		return nil, nil, nil, err
	}
	socketPath, err := BmuxSocketPathForSession(c.Paths, sessionName)
	if err != nil {
		return nil, nil, nil, err
	}
	tokenFile, err := BmuxTokenFileForSession(c.Paths, sessionName)
	if err != nil {
		return nil, nil, nil, err
	}
	binary, err := c.binaryPath()
	if err != nil {
		return nil, nil, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, bmuxCommandTimeout)
	if len(args) == 0 {
		cancel()
		return nil, nil, nil, errors.New("missing bmux command")
	}
	fullArgs := []string{args[0], "-S", socketPath}
	fullArgs = append(fullArgs, args[1:]...)
	cmd := exec.CommandContext(ctx, binary, fullArgs...)
	cmd.Env = BmuxClientEnv(os.Environ(), tokenFile)
	return cmd, ctx, cancel, nil
}

func (c ExecBmuxController) binaryPath() (string, error) {
	path, err := TrustedBmuxBinaryPath(c.Binary)
	if err != nil {
		// A bare "bmux" missing from every trusted directory means bmux is not
		// installed; translate it into ErrBmuxNotInstalled so callers can show a
		// clear manual install hint instead of an opaque launch failure (bmux is
		// no longer auto-installed; it is an explicit opt-in host). An unusable
		// explicit pin (path with a separator)
		// does NOT wrap the not-found sentinel and stays a plain error.
		if errors.Is(err, errTrustedExecutableNotFound) {
			return "", fmt.Errorf("%w: %v", ErrBmuxNotInstalled, err)
		}
		return "", err
	}
	return path, nil
}

func BmuxClientEnv(base []string, tokenFile string) []string {
	return append(safeProcessEnv(base), "BMUX_TOKEN_FILE="+tokenFile)
}

func bmuxMissingSessionOutput(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "failed to connect") ||
		strings.Contains(lower, "connection refused")
}
