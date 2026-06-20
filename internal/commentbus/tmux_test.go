//go:build darwin || linux

package commentbus

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestExecTmuxControllerUsesValidatedArgv(t *testing.T) {
	dir := privateTestDir(t, "comment-bus-tmux-")
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "tmux.log")
	envPath := filepath.Join(dir, "tmux.env")
	tmuxPath := filepath.Join(dir, "tmux")
	script := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> ` + shellQuote(logPath) + `
env | sort >> ` + shellQuote(envPath) + `
state_dir=` + shellQuote(stateDir) + `
if [ "${1:-}" = "-L" ]; then
  shift 2
fi
cmd="$1"
shift
session=""
case "$cmd" in
  has-session)
    while [ "$#" -gt 0 ]; do
      case "$1" in
        -t) session="$2"; shift 2 ;;
        *) shift ;;
      esac
    done
    [ -f "$state_dir/$session" ]
    ;;
  new-session)
    while [ "$#" -gt 0 ]; do
      case "$1" in
        -s) session="$2"; shift 2 ;;
        --) break ;;
        *) shift ;;
      esac
    done
    printf '%s\n' "$session:0.0" > "$state_dir/$session"
    ;;
  display-message)
    format=""
    while [ "$#" -gt 0 ]; do
      case "$1" in
        -t) session="$2"; shift 2 ;;
        -p) format="$2"; shift 2 ;;
        *) shift ;;
      esac
    done
    case "$format" in
      '#{pane_current_command}')
        printf '%s\n' claude
        ;;
      '#{session_name}')
        printf '%s\n' comment-reviewer-abc123
        ;;
      '#{session_name}:#{window_index}.#{pane_index}')
        printf '%s\n' "${session%%:*}:0.0"
        ;;
      *)
        cat "$state_dir/${session%%:*}"
        ;;
    esac
    ;;
  capture-pane)
    printf '%s\n' "captured pane"
    ;;
  send-keys)
    ;;
  kill-session)
    while [ "$#" -gt 0 ]; do
      case "$1" in
        -t) session="$2"; shift 2 ;;
        *) shift ;;
      esac
    done
    rm -f "$state_dir/$session"
    ;;
esac
`
	if err := os.WriteFile(tmuxPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAI_API_KEY", "sk-test-secret-value")
	t.Setenv("AXIOM_TOKEN", "secret-token-value")
	t.Setenv("PATH", "/tmp/evil-path")
	t.Setenv("SHELL", "/tmp/evil-shell")
	controller := ExecTmuxController{Binary: tmuxPath}
	options := TmuxNewSessionOptions{
		SessionName:       "comment-reviewer-abc123",
		WorkingDir:        dir,
		CommentHome:       dir,
		BotletsHome:       dir,
		Command:           "'/tmp/comment bin' session-exec sess_abcdefghijklmnopqrst gen_abcdefghijklmnop",
		OutputPipeCommand: "'/tmp/comment bin' __runtime-tail --log '/tmp/runtime.log' --bytes 65536",
	}
	if err := controller.NewSession(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	live, err := controller.HasSession(context.Background(), options.SessionName)
	if err != nil || !live {
		t.Fatalf("HasSession = %v, %v", live, err)
	}
	target, err := controller.PaneTarget(context.Background(), options.SessionName)
	if err != nil {
		t.Fatal(err)
	}
	if target != "comment-reviewer-abc123:0.0" {
		t.Fatalf("pane target = %q", target)
	}
	command, err := controller.PaneCurrentCommand(context.Background(), target)
	if err != nil || command != "claude" {
		t.Fatalf("pane command = %q, %v", command, err)
	}
	belongs, err := controller.PaneBelongsToSession(context.Background(), options.SessionName, target)
	if err != nil || !belongs {
		t.Fatalf("PaneBelongsToSession = %v, %v", belongs, err)
	}
	capture, err := controller.CapturePane(context.Background(), target, 12)
	if err != nil || capture != "captured pane\n" {
		t.Fatalf("CapturePane = %q, %v", capture, err)
	}
	if err := controller.SendLiteral(context.Background(), target, "hello"); err != nil {
		t.Fatal(err)
	}
	if err := controller.SendEnter(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	if err := controller.KillSession(context.Background(), options.SessionName); err != nil {
		t.Fatal(err)
	}
	envData, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	env := string(envData)
	if strings.Contains(env, "OPENAI_API_KEY") || strings.Contains(env, "AXIOM_TOKEN") || strings.Contains(env, "secret-token-value") || strings.Contains(env, "PATH=") || strings.Contains(env, "SHELL=") {
		t.Fatalf("tmux command inherited secret-bearing env:\n%s", env)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(logData)
	for _, want := range []string{
		"-L comment-reviewer-abc123 new-session -d -s comment-reviewer-abc123",
		"-e COMMENT_IO_HOME=" + dir,
		"pipe-pane -t comment-reviewer-abc123 '/tmp/comment bin' __runtime-tail --log '/tmp/runtime.log' --bytes 65536",
		"-L comment-reviewer-abc123 has-session -t comment-reviewer-abc123",
		"-L comment-reviewer-abc123 display-message -t comment-reviewer-abc123 -p #{session_name}:#{window_index}.#{pane_index}",
		"-L comment-reviewer-abc123 display-message -t comment-reviewer-abc123:0.0 -p #{pane_current_command}",
		"-L comment-reviewer-abc123 capture-pane -t comment-reviewer-abc123:0.0 -p -S -12",
		"-L comment-reviewer-abc123 send-keys -t comment-reviewer-abc123:0.0 -l hello",
		"-L comment-reviewer-abc123 send-keys -t comment-reviewer-abc123:0.0 Enter",
		"-L comment-reviewer-abc123 kill-session -t comment-reviewer-abc123",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("tmux log missing %q:\n%s", want, log)
		}
	}
	if err := controller.SendLiteral(context.Background(), "bad;target", "hello"); err == nil {
		t.Fatal("expected invalid pane target rejection")
	}
}

func TestExecTmuxControllerDistinguishesMissingSessionFromInspectFailure(t *testing.T) {
	dir := privateTestDir(t, "comment-bus-tmux-")
	tmuxPath := filepath.Join(dir, "tmux")
	script := `#!/bin/sh
set -eu
session=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -t) session="$2"; shift 2 ;;
    *) shift ;;
  esac
done
case "$session" in
  comment-reviewer-abc123)
    echo "can't find session: comment-reviewer-abc123" >&2
    exit 1
    ;;
  comment-reviewer-def456)
    echo "permission denied opening tmux socket" >&2
    exit 1
    ;;
  *)
    exit 0
    ;;
esac
`
	if err := os.WriteFile(tmuxPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	controller := ExecTmuxController{Binary: tmuxPath}
	live, err := controller.HasSession(context.Background(), "comment-reviewer-abc123")
	if err != nil || live {
		t.Fatalf("missing HasSession = %v, %v", live, err)
	}
	if live, err := controller.HasSession(context.Background(), "comment-reviewer-def456"); err == nil || live {
		t.Fatalf("inspect-error HasSession = %v, %v", live, err)
	}
}

func TestExecTmuxControllerTreatsMissingKillAsMissingSession(t *testing.T) {
	dir := privateTestDir(t, "comment-bus-tmux-")
	tmuxPath := filepath.Join(dir, "tmux")
	script := `#!/bin/sh
echo "can't find session: comment-reviewer-abc123" >&2
exit 1
`
	if err := os.WriteFile(tmuxPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	controller := ExecTmuxController{Binary: tmuxPath}
	if err := controller.KillSession(context.Background(), "comment-reviewer-abc123"); !errors.Is(err, ErrTmuxSessionMissing) {
		t.Fatalf("missing KillSession err = %v", err)
	}
}

func TestExecTmuxControllerRejectsUntrustedPathBinary(t *testing.T) {
	dir := privateTestDir(t, "comment-bus-tmux-")
	badDir := filepath.Join(dir, "bad-bin")
	if err := os.MkdirAll(badDir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(badDir, 0o777); err != nil {
		t.Fatal(err)
	}
	tmuxPath := filepath.Join(badDir, "tmux")
	if err := os.WriteFile(tmuxPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	controller := ExecTmuxController{Binary: tmuxPath}
	if live, err := controller.HasSession(context.Background(), "comment-reviewer-abc123"); err == nil || live {
		t.Fatalf("expected untrusted tmux rejection, live=%v err=%v", live, err)
	}
}

func TestResolveTrustedExecutableWithSearchFindsTrustedTmuxWhenPathIsStripped(t *testing.T) {
	dir := privateTestDir(t, "comment-bus-tmux-")
	tmuxPath := filepath.Join(dir, "tmux")
	// Use a non-shebang stand-in: auto-discovery deliberately skips shell-script
	// wrapper shims (which start with "#!"), and a real tmux is a native binary,
	// not a script. A "#!" placeholder here would (correctly) be skipped.
	if err := os.WriteFile(tmuxPath, []byte{0x7f, 'E', 'L', 'F', 0x02, 0x01, 0x01, 0x00}, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Join(dir, "missing"))
	resolved, err := resolveTrustedExecutableWithSearch("tmux", "tmux binary", []string{dir})
	if err != nil {
		t.Fatal(err)
	}
	expected, err := filepath.EvalSymlinks(tmuxPath)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != expected {
		t.Fatalf("resolved tmux = %q, want %q", resolved, expected)
	}
}

func TestResolveRuntimeCommandAcceptsPrivateDirUnderStickyParent(t *testing.T) {
	root := privateTestDir(t, "comment-bus-runtime-")
	stickyParent := filepath.Join(root, "sticky")
	if err := os.MkdirAll(stickyParent, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stickyParent, os.ModeSticky|0o777); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(stickyParent, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	claudePath := filepath.Join(binDir, "claude")
	if err := os.WriteFile(claudePath, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveRuntimeCommandPath(claudePath); err != nil {
		t.Fatal(err)
	}
}

func TestExecTmuxControllerRejectsGroupWritableBinary(t *testing.T) {
	dir := privateTestDir(t, "comment-bus-tmux-")
	tmuxPath := filepath.Join(dir, "tmux")
	if err := os.WriteFile(tmuxPath, []byte("#!/bin/sh\nexit 0\n"), 0o770); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmuxPath, 0o770); err != nil {
		t.Fatal(err)
	}
	controller := ExecTmuxController{Binary: tmuxPath}
	if live, err := controller.HasSession(context.Background(), "comment-reviewer-abc123"); err == nil || live {
		t.Fatalf("expected group-writable tmux rejection, live=%v err=%v", live, err)
	}
}

func TestExecTmuxControllerRejectsArbitraryGroupWritableParent(t *testing.T) {
	dir := privateTestDir(t, "comment-bus-tmux-")
	parent := filepath.Join(dir, "shared-tools")
	binDir := filepath.Join(parent, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o775); err != nil {
		t.Fatal(err)
	}
	tmuxPath := filepath.Join(binDir, "tmux")
	if err := os.WriteFile(tmuxPath, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	controller := ExecTmuxController{Binary: tmuxPath}
	if live, err := controller.HasSession(context.Background(), "comment-reviewer-abc123"); err == nil || live {
		t.Fatalf("expected arbitrary group-writable parent rejection, live=%v err=%v", live, err)
	}
}

func TestGroupWritableHomebrewExceptionIsAnchored(t *testing.T) {
	if !isAllowedGroupWritableHomebrewDir("/opt/homebrew/Cellar", false) {
		t.Fatal("expected /opt/homebrew/Cellar group-write allowance")
	}
	if !isAllowedGroupWritableHomebrewDirForGOOS("/opt/homebrew/lib", false, "darwin") {
		t.Fatal("expected /opt/homebrew/lib group-write allowance on darwin")
	}
	if !isAllowedGroupWritableHomebrewDirForGOOS("/usr/local/lib", false, "darwin") {
		t.Fatal("expected /usr/local/lib group-write allowance on darwin")
	}
	if isAllowedGroupWritableHomebrewDirForGOOS("/opt/homebrew/lib", false, "linux") {
		t.Fatal("unexpected /opt/homebrew/lib group-write allowance on linux")
	}
	if isAllowedGroupWritableHomebrewDirForGOOS("/usr/local/lib", false, "linux") {
		t.Fatal("unexpected /usr/local/lib group-write allowance on linux")
	}
	if !isAllowedGroupWritableHomebrewDir("/opt/homebrew/bin", true) {
		t.Fatal("expected /opt/homebrew/bin search-path allowance")
	}
	if isAllowedGroupWritableHomebrewDir("/tmp/test/opt/homebrew/Cellar", false) {
		t.Fatal("unexpected unanchored Homebrew-shaped Cellar allowance")
	}
	if isAllowedGroupWritableHomebrewDir("/tmp/test/opt/homebrew/lib", false) {
		t.Fatal("unexpected unanchored Homebrew-shaped lib allowance")
	}
	if isAllowedGroupWritableHomebrewDir("/tmp/test/opt/homebrew/bin", true) {
		t.Fatal("unexpected unanchored Homebrew-shaped bin allowance")
	}
	if isAllowedGroupWritableHomebrewDir("/opt/homebrew/bin", false) {
		t.Fatal("executable parent checks should not allow group-writable Homebrew bin")
	}
}

func TestExecTmuxControllerAllowsHomebrewStyleSymlinkWithTrustedParents(t *testing.T) {
	dir := privateTestDir(t, "comment-bus-tmux-")
	homebrewBin := filepath.Join(dir, "opt", "homebrew", "bin")
	cellarDir := filepath.Join(dir, "opt", "homebrew", "Cellar")
	versionedBin := filepath.Join(cellarDir, "tmux", "3.5", "bin")
	for _, path := range []string{homebrewBin, cellarDir, versionedBin} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	realTmux := filepath.Join(versionedBin, "tmux")
	script := `#!/bin/sh
echo "can't find session: comment-reviewer-abc123" >&2
exit 1
`
	if err := os.WriteFile(realTmux, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realTmux, filepath.Join(homebrewBin, "tmux")); err != nil {
		t.Fatal(err)
	}
	controller := ExecTmuxController{Binary: filepath.Join(homebrewBin, "tmux")}
	live, err := controller.HasSession(context.Background(), "comment-reviewer-abc123")
	if err != nil || live {
		t.Fatalf("Homebrew-style tmux HasSession = %v, %v", live, err)
	}
}

func TestResolveTrustedExecutableAllowsHomebrewNpmGlobalPackage(t *testing.T) {
	const commentBinary = "/opt/homebrew/lib/node_modules/@comment-io/cli/dist/comment-darwin-arm64"
	libInfo, err := os.Lstat("/opt/homebrew/lib")
	if err != nil {
		t.Skip("/opt/homebrew/lib is not present")
	}
	if libInfo.Mode().Perm()&0o020 == 0 {
		t.Skip("/opt/homebrew/lib is not group-writable on this machine")
	}
	if _, err := os.Lstat(commentBinary); err != nil {
		t.Skip("installed Homebrew npm global comment binary is not present")
	}
	if _, err := resolveTrustedExecutable(commentBinary, "comment binary"); err != nil {
		t.Fatalf("expected Homebrew npm global package binary to be trusted: %v", err)
	}
}

func TestFormatTmuxNudgeIsShellInert(t *testing.T) {
	messageID := "msg_abcdefghijklmnopqrst"
	text, err := formatTmuxNudge("reviewer", messageID)
	if err != nil {
		t.Fatal(err)
	}
	if text != "# comment.io message for reviewer: run comment messages receive "+messageID+". If no visible reply is needed run comment activity complete "+messageID {
		t.Fatalf("nudge text = %q", text)
	}
	if strings.ContainsAny(text, "`\"'$;&|<>\r\n\x00") {
		t.Fatalf("nudge text contains shell metacharacter: %q", text)
	}
	if _, err := formatTmuxNudge("reviewer", "clm_remoteclaim"); err == nil {
		t.Fatal("expected raw cloud claim id rejection")
	}
}

func TestFormatProfileTmuxNudgeUsesCommentCommandPath(t *testing.T) {
	messageID := "msg_abcdefghijklmnopqrst"
	text, err := formatProfileTmuxNudge("max.reviewer", messageID, "/home/user/.comment-io/bus/bin/comment")
	if err != nil {
		t.Fatal(err)
	}
	want := "# comment.io message for max.reviewer: run '/home/user/.comment-io/bus/bin/comment' messages receive --profile max.reviewer " + messageID + " then ack or release. If no visible reply is needed run '/home/user/.comment-io/bus/bin/comment' activity complete " + messageID
	if text != want {
		t.Fatalf("nudge text = %q, want %q", text, want)
	}
	if strings.ContainsAny(text, "`\"$;&|<>\r\n\x00") {
		t.Fatalf("nudge text contains unsafe metacharacter: %q", text)
	}
}

func TestExecTmuxControllerRejectsUntrustedLaunchDirectory(t *testing.T) {
	dir := privateTestDir(t, "comment-bus-tmux-")
	badDir := filepath.Join(dir, "group-writable")
	if err := os.MkdirAll(badDir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(badDir, 0o777); err != nil {
		t.Fatal(err)
	}
	controller := ExecTmuxController{Binary: filepath.Join(dir, "tmux")}
	err := controller.NewSession(context.Background(), TmuxNewSessionOptions{
		SessionName: "comment-reviewer-abc123",
		WorkingDir:  badDir,
		CommentHome: dir,
		BotletsHome: dir,
		Command:     "'/tmp/comment' session-exec sess_abcdefghijklmnopqrst gen_abcdefghijklmnop",
	})
	if err == nil || !strings.Contains(err.Error(), "working directory") {
		t.Fatalf("expected launch dir rejection, got %v", err)
	}
}

func TestExecTmuxControllerRejectsLaunchDirectoryUnderUntrustedParent(t *testing.T) {
	dir := privateTestDir(t, "comment-bus-tmux-")
	parent := filepath.Join(dir, "shared")
	workingDir := filepath.Join(parent, "work")
	if err := os.MkdirAll(workingDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o777); err != nil {
		t.Fatal(err)
	}
	controller := ExecTmuxController{Binary: filepath.Join(dir, "tmux")}
	err := controller.NewSession(context.Background(), TmuxNewSessionOptions{
		SessionName: "comment-reviewer-abc123",
		WorkingDir:  workingDir,
		CommentHome: dir,
		BotletsHome: dir,
		Command:     "'/tmp/comment' session-exec sess_abcdefghijklmnopqrst gen_abcdefghijklmnop",
	})
	if err == nil || !strings.Contains(err.Error(), "working directory parent") {
		t.Fatalf("expected launch parent rejection, got %v", err)
	}
}

func TestExecTmuxControllerResolvesTrustedLaunchDirectorySymlinkParents(t *testing.T) {
	dir := privateTestDir(t, "comment-bus-tmux-")
	realRoot := filepath.Join(dir, "real")
	workingDir := filepath.Join(realRoot, "work")
	if err := os.MkdirAll(workingDir, 0o700); err != nil {
		t.Fatal(err)
	}
	linkRoot := filepath.Join(dir, "link")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	logPath := filepath.Join(dir, "tmux.log")
	tmuxPath := filepath.Join(dir, "tmux")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + shellQuote(logPath) + "\n"
	if err := os.WriteFile(tmuxPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	controller := ExecTmuxController{Binary: tmuxPath}
	if err := controller.NewSession(context.Background(), TmuxNewSessionOptions{
		SessionName: "comment-reviewer-abc123",
		WorkingDir:  filepath.Join(linkRoot, "work"),
		CommentHome: dir,
		BotletsHome: dir,
		Command:     "'/tmp/comment' session-exec sess_abcdefghijklmnopqrst gen_abcdefghijklmnop",
	}); err != nil {
		t.Fatal(err)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(logData)
	if !strings.Contains(log, "-c "+workingDir) || strings.Contains(log, "-c "+filepath.Join(linkRoot, "work")) {
		t.Fatalf("tmux launch dir was not canonicalized:\n%s", log)
	}
}

func TestRunSessionExecUsesAllowlistedRuntimeAndEnv(t *testing.T) {
	paths := testDaemonPaths(t)
	binDir := filepath.Join(paths.Home, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	claudePath := filepath.Join(binDir, "claude")
	if err := os.WriteFile(claudePath, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	record, err := RegisterSession(RegisterSessionOptions{
		Paths:       paths,
		Profile:     "max.reviewer",
		BotName:     "reviewer",
		ScopeType:   "profile",
		ScopeID:     "max.reviewer",
		BotletsHome: filepath.Join(paths.Home, "botlets"),
		SessionName: "comment-reviewer-abc123",
		PaneTarget:  "comment-reviewer-abc123:0.0",
		State:       "starting",
	})
	if err != nil {
		t.Fatal(err)
	}
	// This test exercises the retained "path" (trusted-binary) launch mode: an
	// unpinned path-mode record must be rejected until a trusted binary is pinned.
	record.RuntimeLaunchMode = RuntimeLaunchModePath
	if err := WriteSessionRecord(paths, record); err != nil {
		t.Fatal(err)
	}
	err = RunSessionExec(SessionExecOptions{
		Paths:      paths,
		SessionID:  record.SessionID,
		Generation: record.Generation,
		LookPath:   func(name string) (string, error) { return claudePath, nil },
		Exec:       func(path string, argv []string, env []string) error { return nil },
	})
	if err == nil {
		t.Fatal("expected unpinned runtime command rejection")
	}
	resolvedClaudePath, err := filepath.EvalSymlinks(claudePath)
	if err != nil {
		t.Fatal(err)
	}
	record.RuntimePath = resolvedClaudePath
	record.RuntimeCommandPath = claudePath
	record.RuntimeLaunchMode = RuntimeLaunchModePath
	if err := WriteSessionRecord(paths, record); err != nil {
		t.Fatal(err)
	}
	var execPath string
	var execArgv []string
	var execEnv []string
	sentinel := errors.New("exec sentinel")
	err = RunSessionExec(SessionExecOptions{
		Paths:      paths,
		SessionID:  record.SessionID,
		Generation: record.Generation,
		Environ: []string{
			"COMMENT_IO_PROFILE=max.attacker",
			"COMMENT_IO_SESSION_CAP_FILE=/tmp/wrong",
			"OPENAI_API_KEY=sk-test-secret-value",
			"AXIOM_TOKEN=secret-token-value",
			"PATH=/usr/bin",
			"SHELL=/tmp/evil-shell",
		},
		LookPath: func(name string) (string, error) { return "", errors.New("lookpath should not be called") },
		Exec: func(path string, argv []string, env []string) error {
			execPath = path
			execArgv = append([]string{}, argv...)
			execEnv = append([]string{}, env...)
			return sentinel
		},
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("RunSessionExec error = %v", err)
	}
	if execPath != resolvedClaudePath || len(execArgv) != 4 || execArgv[1] != "--agent" || execArgv[2] != "reviewer" || execArgv[3] != "--dangerously-skip-permissions" {
		t.Fatalf("exec path/argv = %q %#v", execPath, execArgv)
	}
	env := strings.Join(execEnv, "\n")
	for _, want := range []string{
		"COMMENT_IO_HOME=" + paths.Home,
		"COMMENT_IO_PROFILE=max.reviewer",
		"COMMENT_IO_BOT_NAME=reviewer",
		"COMMENT_IO_SESSION_ID=" + record.SessionID,
		"COMMENT_IO_SESSION_GENERATION=" + record.Generation,
		"COMMENT_IO_SESSION_CAP_FILE=" + record.CapabilityFile,
		"BOTLETS_HOME=" + record.BotletsHome,
	} {
		if !strings.Contains(env, want) {
			t.Fatalf("env missing %q:\n%s", want, env)
		}
	}
	if strings.Contains(env, "max.attacker") || strings.Contains(env, "/tmp/wrong") || strings.Contains(env, "OPENAI_API_KEY") || strings.Contains(env, "AXIOM_TOKEN") || strings.Contains(env, "secret-token-value") || strings.Contains(env, "PATH=/usr/bin") || strings.Contains(env, "SHELL=") {
		t.Fatalf("managed env kept caller-controlled session values:\n%s", env)
	}
	if !strings.Contains(env, "PATH="+filepath.Dir(resolvedClaudePath)) {
		t.Fatalf("managed env missing trusted runtime PATH:\n%s", env)
	}
	execPath = ""
	execArgv = nil
	execEnv = nil
	lookPathCalled := false
	err = RunSessionExec(SessionExecOptions{
		Paths:      paths,
		SessionID:  record.SessionID,
		Generation: record.Generation,
		Environ: []string{
			"PATH=/tmp/evil-path",
			"SHELL=/tmp/evil-shell",
		},
		LookPath: func(name string) (string, error) {
			lookPathCalled = true
			return "", errors.New("lookpath should not be called")
		},
		Exec: func(path string, argv []string, env []string) error {
			execPath = path
			execArgv = append([]string{}, argv...)
			execEnv = append([]string{}, env...)
			return sentinel
		},
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("RunSessionExec with absolute runtime error = %v", err)
	}
	if lookPathCalled {
		t.Fatal("absolute runtime command unexpectedly used PATH lookup")
	}
	if execPath != resolvedClaudePath || len(execArgv) != 4 || execArgv[1] != "--agent" || execArgv[2] != "reviewer" || execArgv[3] != "--dangerously-skip-permissions" {
		t.Fatalf("absolute runtime exec path/argv = %q %#v", execPath, execArgv)
	}
	env = strings.Join(execEnv, "\n")
	if strings.Contains(env, "PATH=/tmp/evil-path") || strings.Contains(env, "SHELL=") {
		t.Fatalf("absolute runtime env kept caller-controlled PATH/SHELL:\n%s", env)
	}
	if !strings.Contains(env, "PATH="+filepath.Dir(resolvedClaudePath)) {
		t.Fatalf("absolute runtime env missing trusted runtime PATH:\n%s", env)
	}
	otherRuntimePath := filepath.Join(binDir, "other-runtime")
	if err := os.WriteFile(otherRuntimePath, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	record.RuntimePath = otherRuntimePath
	record.RuntimeCommandPath = claudePath
	record.RuntimeLaunchMode = RuntimeLaunchModePath
	if err := WriteSessionRecord(paths, record); err != nil {
		t.Fatal(err)
	}
	err = RunSessionExec(SessionExecOptions{
		Paths:      paths,
		SessionID:  record.SessionID,
		Generation: record.Generation,
		LookPath:   func(name string) (string, error) { return "", errors.New("lookpath should not be called") },
		Exec:       func(path string, argv []string, env []string) error { return nil },
	})
	if err == nil {
		t.Fatal("expected pinned runtime path mismatch rejection")
	}
	record.RuntimePath = resolvedClaudePath
	record.RuntimeCommandPath = claudePath
	record.RuntimeLaunchMode = RuntimeLaunchModePath
	record.RuntimeCommand = []string{"sh", "-c", "echo bad"}
	if err := WriteSessionRecord(paths, record); err != nil {
		t.Fatal(err)
	}
	err = RunSessionExec(SessionExecOptions{
		Paths:      paths,
		SessionID:  record.SessionID,
		Generation: record.Generation,
		LookPath:   func(name string) (string, error) { return name, nil },
		Exec:       func(path string, argv []string, env []string) error { return nil },
	})
	if err == nil {
		t.Fatal("expected non-allowlisted runtime command rejection")
	}
}

func TestRunSessionExecAcceptsCodexManagedRuntime(t *testing.T) {
	paths := testDaemonPaths(t)
	binDir := filepath.Join(paths.Home, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	codexPath := filepath.Join(binDir, "codex")
	if err := os.WriteFile(codexPath, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	record, err := RegisterSession(RegisterSessionOptions{
		Paths:       paths,
		Profile:     "max.reviewer",
		BotName:     "reviewer",
		ScopeType:   "profile",
		ScopeID:     "max.reviewer",
		BotletsHome: filepath.Join(paths.Home, "botlets"),
		SessionName: "comment-reviewer-abc123",
		PaneTarget:  "comment-reviewer-abc123:0.0",
		Runtime:     "codex",
		State:       "starting",
	})
	if err != nil {
		t.Fatal(err)
	}
	resolvedCodexPath, err := filepath.EvalSymlinks(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	record.RuntimePath = resolvedCodexPath
	record.RuntimeCommandPath = codexPath
	record.RuntimeLaunchMode = RuntimeLaunchModePath
	if err := WriteSessionRecord(paths, record); err != nil {
		t.Fatal(err)
	}
	var execPath string
	var execArgv []string
	sentinel := errors.New("exec sentinel")
	err = RunSessionExec(SessionExecOptions{
		Paths:      paths,
		SessionID:  record.SessionID,
		Generation: record.Generation,
		LookPath:   func(name string) (string, error) { return "", errors.New("lookpath should not be called") },
		Exec: func(path string, argv []string, env []string) error {
			execPath = path
			execArgv = append([]string{}, argv...)
			return sentinel
		},
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("RunSessionExec error = %v", err)
	}
	if execPath != resolvedCodexPath || len(execArgv) != 2 || execArgv[0] != resolvedCodexPath || execArgv[1] != "--yolo" {
		t.Fatalf("codex exec path/argv = %q %#v", execPath, execArgv)
	}
}

func TestRunSessionExecProvidesInteractiveTermForManagedRuntime(t *testing.T) {
	paths := testDaemonPaths(t)
	binDir := filepath.Join(paths.Home, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	codexPath := filepath.Join(binDir, "codex")
	if err := os.WriteFile(codexPath, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	record, err := RegisterSession(RegisterSessionOptions{
		Paths:       paths,
		Profile:     "max.reviewer",
		BotName:     "reviewer",
		ScopeType:   "profile",
		ScopeID:     "max.reviewer",
		BotletsHome: filepath.Join(paths.Home, "botlets"),
		SessionName: "comment-reviewer-abc123",
		PaneTarget:  "comment-reviewer-abc123:0.0",
		Runtime:     "codex",
		State:       "starting",
	})
	if err != nil {
		t.Fatal(err)
	}
	resolvedCodexPath, err := filepath.EvalSymlinks(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	record.RuntimePath = resolvedCodexPath
	record.RuntimeCommandPath = codexPath
	record.RuntimeLaunchMode = RuntimeLaunchModePath
	if err := WriteSessionRecord(paths, record); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name          string
		env           []string
		wantTerm      string
		wantColorTerm string
	}{
		{name: "missing", env: nil, wantTerm: "xterm-256color", wantColorTerm: "truecolor"},
		{name: "dumb", env: []string{"TERM=dumb"}, wantTerm: "xterm-256color", wantColorTerm: "truecolor"},
		{name: "blank", env: []string{"TERM=   "}, wantTerm: "xterm-256color", wantColorTerm: "truecolor"},
		{name: "valid", env: []string{"TERM=xterm-kitty", "COLORTERM=24bit", "NO_COLOR=1", "FORCE_COLOR=0", "CLICOLOR_FORCE=0"}, wantTerm: "xterm-kitty", wantColorTerm: "24bit"},
		{name: "unsafe", env: []string{"TERM=dumb", "COLORTERM=bad\nvalue", "NO_COLOR=1"}, wantTerm: "xterm-256color", wantColorTerm: "truecolor"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var execEnv []string
			sentinel := errors.New("exec sentinel")
			err := RunSessionExec(SessionExecOptions{
				Paths:      paths,
				SessionID:  record.SessionID,
				Generation: record.Generation,
				Environ:    tc.env,
				LookPath:   func(name string) (string, error) { return "", errors.New("lookpath should not be called") },
				Exec: func(path string, argv []string, env []string) error {
					execEnv = append([]string{}, env...)
					return sentinel
				},
			})
			if !errors.Is(err, sentinel) {
				t.Fatalf("RunSessionExec error = %v", err)
			}
			for key, want := range map[string]string{
				"TERM":           tc.wantTerm,
				"COLORTERM":      tc.wantColorTerm,
				"FORCE_COLOR":    "1",
				"CLICOLOR_FORCE": "1",
			} {
				got := envEntryValueForTest(execEnv, key)
				if got != want {
					t.Fatalf("%s = %q, want %q in env:\n%s", key, got, want, strings.Join(execEnv, "\n"))
				}
			}
			if got := envEntryValueForTest(execEnv, "NO_COLOR"); got != "" {
				t.Fatalf("NO_COLOR = %q, want omitted in env:\n%s", got, strings.Join(execEnv, "\n"))
			}
		})
	}
}

func envEntryValueForTest(env []string, key string) string {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}

func TestRunSessionExecAcceptsCanonicalHomeForSymlinkedParent(t *testing.T) {
	linkedPaths, canonicalPaths := symlinkedSessionHomePaths(t)
	binDir := filepath.Join(canonicalPaths.Home, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	claudePath := filepath.Join(binDir, "claude")
	if err := os.WriteFile(claudePath, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	resolvedClaudePath, err := filepath.EvalSymlinks(claudePath)
	if err != nil {
		t.Fatal(err)
	}
	record, err := RegisterSession(RegisterSessionOptions{
		Paths:       linkedPaths,
		Profile:     "max.reviewer",
		BotName:     "reviewer",
		ScopeType:   "profile",
		ScopeID:     "max.reviewer",
		BotletsHome: filepath.Join(canonicalPaths.Home, "botlets"),
		SessionName: "comment-reviewer-abc123",
		PaneTarget:  "%1",
		State:       "starting",
	})
	if err != nil {
		t.Fatal(err)
	}
	record.RuntimePath = resolvedClaudePath
	record.RuntimeCommandPath = claudePath
	record.RuntimeLaunchMode = RuntimeLaunchModePath
	if err := WriteSessionRecord(linkedPaths, record); err != nil {
		t.Fatal(err)
	}
	canonicalCapabilityFile := sessionCapabilityPath(canonicalPaths, record.Profile, record.SessionID, record.Generation)
	if canonicalCapabilityFile == record.CapabilityFile {
		t.Fatal("test did not create distinct linked and canonical capability paths")
	}
	var execEnv []string
	sentinel := errors.New("exec sentinel")
	err = RunSessionExec(SessionExecOptions{
		Paths:      canonicalPaths,
		SessionID:  record.SessionID,
		Generation: record.Generation,
		Environ: []string{
			"COMMENT_IO_HOME=" + linkedPaths.Home,
			"COMMENT_IO_SESSION_CAP_FILE=" + record.CapabilityFile,
		},
		LookPath: func(name string) (string, error) { return "", errors.New("lookpath should not be called") },
		Exec: func(path string, argv []string, env []string) error {
			execEnv = append([]string{}, env...)
			return sentinel
		},
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("RunSessionExec canonical home error = %v", err)
	}
	env := strings.Join(execEnv, "\n")
	for _, want := range []string{
		"COMMENT_IO_HOME=" + canonicalPaths.Home,
		"COMMENT_IO_SESSION_CAP_FILE=" + canonicalCapabilityFile,
	} {
		if !strings.Contains(env, want) {
			t.Fatalf("canonical env missing %q:\n%s", want, env)
		}
	}
	if strings.Contains(env, "COMMENT_IO_HOME="+linkedPaths.Home) || strings.Contains(env, "COMMENT_IO_SESSION_CAP_FILE="+record.CapabilityFile) {
		t.Fatalf("canonical env kept linked home values:\n%s", env)
	}
}

func TestRunSessionExecRejectsUntrustedRuntimePath(t *testing.T) {
	paths := testDaemonPaths(t)
	record, err := RegisterSession(RegisterSessionOptions{
		Paths:       paths,
		Profile:     "max.reviewer",
		BotName:     "reviewer",
		ScopeType:   "profile",
		ScopeID:     "max.reviewer",
		BotletsHome: filepath.Join(paths.Home, "botlets"),
		SessionName: "comment-reviewer-abc123",
		PaneTarget:  "comment-reviewer-abc123:0.0",
		State:       "starting",
	})
	if err != nil {
		t.Fatal(err)
	}
	badDir := filepath.Join(paths.Home, "bad-bin")
	if err := os.MkdirAll(badDir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(badDir, 0o777); err != nil {
		t.Fatal(err)
	}
	claudePath := filepath.Join(badDir, "claude")
	if err := os.WriteFile(claudePath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	record.RuntimePath = claudePath
	record.RuntimeCommandPath = claudePath
	record.RuntimeLaunchMode = RuntimeLaunchModePath
	if err := WriteSessionRecord(paths, record); err != nil {
		t.Fatal(err)
	}
	err = RunSessionExec(SessionExecOptions{
		Paths:      paths,
		SessionID:  record.SessionID,
		Generation: record.Generation,
		LookPath:   func(name string) (string, error) { return claudePath, nil },
		Exec:       func(path string, argv []string, env []string) error { return nil },
	})
	if err == nil {
		t.Fatal("expected untrusted runtime path rejection")
	}
	safeDir := filepath.Join(paths.Home, "safe-bin")
	if err := os.MkdirAll(safeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	groupWritableClaude := filepath.Join(safeDir, "claude")
	if err := os.WriteFile(groupWritableClaude, []byte("#!/bin/sh\nexit 0\n"), 0o770); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(groupWritableClaude, 0o770); err != nil {
		t.Fatal(err)
	}
	record.RuntimePath = groupWritableClaude
	record.RuntimeCommandPath = groupWritableClaude
	record.RuntimeLaunchMode = RuntimeLaunchModePath
	if err := WriteSessionRecord(paths, record); err != nil {
		t.Fatal(err)
	}
	err = RunSessionExec(SessionExecOptions{
		Paths:      paths,
		SessionID:  record.SessionID,
		Generation: record.Generation,
		LookPath:   func(name string) (string, error) { return groupWritableClaude, nil },
		Exec:       func(path string, argv []string, env []string) error { return nil },
	})
	if err == nil {
		t.Fatal("expected group-writable runtime path rejection")
	}
}

func TestResolveRuntimeCommandRejectsSymlinkFromUntrustedParent(t *testing.T) {
	dir := privateTestDir(t, "comment-bus-tmux-")
	safeDir := filepath.Join(dir, "safe-bin")
	if err := os.MkdirAll(safeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(safeDir, "claude-target")
	if err := os.WriteFile(target, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(dir, "shared-tools")
	binDir := filepath.Join(parent, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o775); err != nil {
		t.Fatal(err)
	}
	claudePath := filepath.Join(binDir, "claude")
	if err := os.Symlink(target, claudePath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := resolveRuntimeCommandPath(claudePath); err == nil {
		t.Fatal("expected untrusted symlink parent rejection")
	}
}

func TestResolveRuntimeCommandRejectsSymlinkedParentFromUntrustedParent(t *testing.T) {
	dir := privateTestDir(t, "comment-bus-tmux-")
	safeDir := filepath.Join(dir, "safe-bin")
	if err := os.MkdirAll(safeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(safeDir, "claude")
	if err := os.WriteFile(target, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(dir, "shared-tools")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o775); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(parent, "bin")
	if err := os.Symlink(safeDir, linkDir); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := resolveRuntimeCommandPath(filepath.Join(linkDir, "claude")); err == nil {
		t.Fatal("expected untrusted symlinked parent rejection")
	}
}

func TestTrustedRuntimeSearchPathRejectsArbitraryGroupWritableParent(t *testing.T) {
	dir := privateTestDir(t, "comment-bus-tmux-")
	parent := filepath.Join(dir, "shared-tools")
	binDir := filepath.Join(parent, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := validateTrustedSearchPathDir(binDir, "runtime PATH directory"); err == nil {
		t.Fatal("expected arbitrary group-writable PATH parent rejection")
	}
}

func TestResolveTransientRuntimeCommandWithClientPath(t *testing.T) {
	// Regression: when the client (CLI) supplies an absolute
	// `runtime_command_path`, the daemon must use that path directly and
	// must NOT mutate `command[0]` — that field is the user-typed
	// identifier (e.g. "claude") which downstream code records on the
	// transient runtime and uses for telemetry / pane-current-command
	// matching.
	dir := privateTestDir(t, "comment-bus-runtime-cmdpath-")
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	claudePath := filepath.Join(binDir, "claude")
	if err := os.WriteFile(claudePath, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	resolution, err := resolveTransientRuntimeCommandWithPath([]string{"claude", "--model", "test"}, claudePath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resolution.CommandPath != claudePath {
		t.Fatalf("CommandPath = %q, want %q", resolution.CommandPath, claudePath)
	}
	if resolution.RuntimePath == "" {
		t.Fatal("RuntimePath should be set after resolution")
	}
}

func TestResolveTransientRuntimeCommandWithRelativeClientPathRejected(t *testing.T) {
	_, err := resolveTransientRuntimeCommandWithPath([]string{"claude"}, "./claude")
	if err == nil {
		t.Fatal("expected error for relative runtime_command_path")
	}
}

func TestResolveTransientRuntimeCommandFallsBackToLegacyPathWhenEmpty(t *testing.T) {
	// Sanity: when the client does not supply a `runtime_command_path`
	// (e.g. older CLIs), the daemon falls back to its legacy resolver.
	// We pass an absolute path so this test does not depend on the
	// daemon's PATH lookup quirks (which are exercised by existing
	// integration tests using prependFakeRuntime).
	dir := privateTestDir(t, "comment-bus-runtime-fallback-")
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(binDir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	resolution, err := resolveTransientRuntimeCommandWithPath([]string{bin}, "")
	if err != nil {
		t.Fatalf("legacy fallback should resolve abs path with empty client path; got %v", err)
	}
	if resolution.CommandPath != bin {
		t.Fatalf("CommandPath = %q, want %q", resolution.CommandPath, bin)
	}
}

func TestEnsureCommentCommandShimCreatesCommentAlias(t *testing.T) {
	home := privateTestDir(t, "comment-bus-shim-home-")
	paths, err := ResolvePaths(filepath.Join(home, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureBaseDirs(paths); err != nil {
		t.Fatal(err)
	}
	binaryDir := privateTestDir(t, "comment-bus-shim-binary-")
	commentBinary := filepath.Join(binaryDir, "comment-darwin-arm64")
	if err := os.WriteFile(commentBinary, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	shimPath, err := ensureCommentCommandShim(paths, commentBinary)
	if err != nil {
		t.Fatalf("ensureCommentCommandShim failed: %v", err)
	}
	if filepath.Base(shimPath) != "comment" {
		t.Fatalf("shim basename = %q, want comment", filepath.Base(shimPath))
	}
	if filepath.Dir(shimPath) != filepath.Join(paths.Bus, "bin") {
		t.Fatalf("shim dir = %q, want private bus bin", filepath.Dir(shimPath))
	}
	info, err := os.Lstat(shimPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("shim mode = %v, want symlink", info.Mode())
	}
	resolved, err := resolveTrustedExecutable(shimPath, "comment command shim")
	if err != nil {
		t.Fatalf("shim is not trusted: %v", err)
	}
	if resolved != commentBinary {
		t.Fatalf("shim resolves to %q, want %q", resolved, commentBinary)
	}
}

func TestTransientRuntimeLaunchCommandIncludesCommentShimOnPath(t *testing.T) {
	home := privateTestDir(t, "comment-bus-runtime-path-home-")
	commandDir := filepath.Join(home, "command")
	runtimeDir := filepath.Join(home, "runtime")
	shimDir := filepath.Join(home, "shim")
	for _, dir := range []string{commandDir, runtimeDir, shimDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	runtimePath := filepath.Join(runtimeDir, "claude")
	if err := os.WriteFile(runtimePath, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	command := transientRuntimeLaunchCommand(TransientRuntimeRecord{
		Profile:            "max.reviewer",
		RuntimeCommand:     []string{"claude"},
		RuntimePath:        runtimePath,
		RuntimeCommandPath: filepath.Join(commandDir, "claude"),
		CommentCommandPath: filepath.Join(shimDir, "comment"),
		OutputLogPath:      filepath.Join(home, "runtime.log"),
	})
	if !strings.Contains(command, "PATH='"+shimDir+string(os.PathListSeparator)) {
		t.Fatalf("launch command missing private comment shim dir:\n%s", command)
	}
	if !strings.Contains(command, string(os.PathListSeparator)+commandDir+string(os.PathListSeparator)) {
		t.Fatalf("launch command missing original runtime command dir:\n%s", command)
	}
	if !strings.Contains(command, string(os.PathListSeparator)+runtimeDir) {
		t.Fatalf("launch command missing runtime dir:\n%s", command)
	}
	if strings.Contains(command, "mkfifo") || strings.Contains(command, "__runtime-tail") || strings.Contains(command, "exec >") {
		t.Fatalf("launch command should not redirect runtime stdio away from its TTY:\n%s", command)
	}
	if !strings.Contains(command, " exec ") {
		t.Fatalf("launch command should exec runtime so tmux liveness sees it:\n%s", command)
	}
	pipeCommand := transientRuntimeOutputPipeCommand(filepath.Join(home, "runtime.log"), filepath.Join(shimDir, "comment"))
	if !strings.Contains(pipeCommand, "__runtime-tail") || !strings.Contains(pipeCommand, "--bytes 65536") {
		t.Fatalf("output pipe command should tail runtime output to a bounded log:\n%s", pipeCommand)
	}
}

func TestTransientRuntimeLaunchCommandIncludesLocalSyncEnv(t *testing.T) {
	root := privateTestDir(t, "comment-local-sync-root-")
	docsRoot := filepath.Join(root, "_Comment.io Docs")
	if err := os.MkdirAll(docsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	command := transientRuntimeLaunchCommand(TransientRuntimeRecord{
		Profile:        "max.reviewer",
		RuntimeCommand: []string{"claude"},
		RuntimePath:    "/usr/bin/claude",
		Env: []string{
			"COMMENT_IO_LOCAL_SYNC=1",
			"COMMENT_IO_LOCAL_SYNC_ROOT=" + root,
			"COMMENT_IO_LOCAL_DOCS_ROOT=" + docsRoot,
			"COMMENT_IO_LOCAL_SYNC_MODE=read-only-projection",
			"COMMENT_IO_LOCAL_SYNC_FRESHNESS=live",
			"TERM=xterm-kitty",
			"COLORTERM=24bit",
			"FORCE_COLOR=0",
			"CLICOLOR_FORCE=0",
			"NO_COLOR=1",
		},
	})
	for _, want := range []string{
		"COMMENT_IO_LOCAL_SYNC='1'",
		"COMMENT_IO_LOCAL_SYNC_ROOT='" + root + "'",
		"COMMENT_IO_LOCAL_DOCS_ROOT='" + docsRoot + "'",
		"COMMENT_IO_LOCAL_SYNC_MODE='read-only-projection'",
		"COMMENT_IO_LOCAL_SYNC_FRESHNESS='live'",
		"unset NO_COLOR",
		"TERM='xterm-kitty'",
		"COLORTERM='24bit'",
		"FORCE_COLOR='1'",
		"CLICOLOR_FORCE='1'",
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("launch command missing %q:\n%s", want, command)
		}
	}
	if strings.Contains(command, "NO_COLOR='1'") {
		t.Fatalf("launch command should unset, not export, NO_COLOR:\n%s", command)
	}
}

func TestTransientRuntimeLaunchCommandArmsRewakeForClaudeOnly(t *testing.T) {
	claudeCmd := transientRuntimeLaunchCommand(TransientRuntimeRecord{
		Profile:        "max.reviewer",
		Role:           RuntimeRoleMain,
		Runtime:        "claude",
		RuntimeCommand: []string{"claude"},
		RuntimePath:    "/usr/bin/claude",
	})
	if !strings.Contains(claudeCmd, "COMMENT_IO_LISTEN=1") || !strings.Contains(claudeCmd, "export COMMENT_IO_LISTEN") {
		t.Fatalf("main claude launch command should arm the rewake hook via COMMENT_IO_LISTEN:\n%s", claudeCmd)
	}
	// An absolute claude path (comment run --runtime /opt/homebrew/bin/claude, or a
	// legacy absolute runtime_command[0]) must still arm — matched by basename.
	absClaudeCmd := transientRuntimeLaunchCommand(TransientRuntimeRecord{
		Profile:        "max.reviewer",
		Role:           RuntimeRoleMain,
		Runtime:        "/opt/homebrew/bin/claude",
		RuntimeCommand: []string{"/opt/homebrew/bin/claude"},
		RuntimePath:    "/opt/homebrew/bin/claude",
	})
	if !strings.Contains(absClaudeCmd, "COMMENT_IO_LISTEN=1") {
		t.Fatalf("absolute main claude path should still arm the rewake hook:\n%s", absClaudeCmd)
	}
	// A non-claude runtime has no rewake Stop hook, so it must NOT arm (it would
	// register a waiter the daemon would then forever skip the keystroke for).
	codexCmd := transientRuntimeLaunchCommand(TransientRuntimeRecord{
		Profile:        "max.reviewer",
		Role:           RuntimeRoleMain,
		Runtime:        "codex",
		RuntimeCommand: []string{"codex"},
		RuntimePath:    "/usr/bin/codex",
	})
	if strings.Contains(codexCmd, "COMMENT_IO_LISTEN") {
		t.Fatalf("non-claude launch command must not arm the rewake hook:\n%s", codexCmd)
	}
	// A TASK-role claude runtime must NOT arm: it does not reserve the profile via
	// reserveMainEstablish, so a profile-scoped listener would race the main /
	// impromptu listener for the handle.
	taskClaudeCmd := transientRuntimeLaunchCommand(TransientRuntimeRecord{
		Profile:        "max.reviewer",
		Role:           RuntimeRoleTask,
		Runtime:        "claude",
		RuntimeCommand: []string{"claude"},
		RuntimePath:    "/usr/bin/claude",
	})
	if strings.Contains(taskClaudeCmd, "COMMENT_IO_LISTEN") {
		t.Fatalf("task-role claude launch command must not arm the rewake hook:\n%s", taskClaudeCmd)
	}
}

func TestTransientRuntimeLaunchCommandPreservesPaneTerm(t *testing.T) {
	baseRecord := TransientRuntimeRecord{
		Profile:        "max.reviewer",
		RuntimeCommand: []string{"claude"},
		RuntimePath:    "/usr/bin/claude",
	}
	missing := baseRecord
	missing.Env = []string{"COMMENT_IO_LOCAL_SYNC=1"}
	missingCommand := transientRuntimeLaunchCommand(missing)
	if want := `case "${TERM:-}" in ''|dumb) export TERM='xterm-256color';; esac`; !strings.Contains(missingCommand, want) {
		t.Fatalf("missing TERM should use shell guard %q:\n%s", want, missingCommand)
	}

	valid := baseRecord
	valid.Env = []string{"TERM=tmux-256color"}
	validCommand := transientRuntimeLaunchCommand(valid)
	if !strings.Contains(validCommand, "export TERM='tmux-256color'") {
		t.Fatalf("valid pane TERM should be preserved:\n%s", validCommand)
	}
	if strings.Contains(validCommand, `case "${TERM:-}"`) {
		t.Fatalf("explicit valid TERM should not use missing/dumb guard:\n%s", validCommand)
	}

	dumb := baseRecord
	dumb.Env = []string{"TERM=dumb"}
	dumbCommand := transientRuntimeLaunchCommand(dumb)
	if !strings.Contains(dumbCommand, "export TERM='xterm-256color'") {
		t.Fatalf("explicit dumb TERM should default to color-capable TERM:\n%s", dumbCommand)
	}
	if strings.Contains(dumbCommand, `case "${TERM:-}"`) {
		t.Fatalf("explicit dumb TERM should be handled directly:\n%s", dumbCommand)
	}
}

func TestLocalSyncRuntimeEnvRequiresLocalLLMSDocs(t *testing.T) {
	paths := Paths{Home: privateTestDir(t, "comment-local-sync-home-")}
	root := privateTestDir(t, "comment-local-sync-root-")
	if err := os.MkdirAll(filepath.Join(paths.Home, "sync"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Home, "sync", "config.json"), []byte(`{"root":"`+root+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if env := strings.Join(localSyncRuntimeEnv(paths), "\n"); env == "" || strings.Contains(env, "COMMENT_IO_LOCAL_DOCS_ROOT") || !strings.Contains(env, "COMMENT_IO_LOCAL_SYNC_FRESHNESS=periodic") {
		t.Fatalf("env should expose periodic sync root but not docs root without llms.txt: %s", env)
	}
	if err := os.WriteFile(filepath.Join(paths.Home, "sync", "live-status.json"), []byte(`{"state":"connected","root":"`+root+`","updated_at":"2099-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if env := strings.Join(localSyncRuntimeEnv(paths), "\n"); !strings.Contains(env, "COMMENT_IO_LOCAL_SYNC_FRESHNESS=periodic") {
		t.Fatalf("future-dated live status should not be trusted: %s", env)
	}
	if err := os.WriteFile(filepath.Join(paths.Home, "sync", "config.json"), []byte(`{"root":"`+root+`","live_sync_enabled":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Home, "sync", "live-status.json"), []byte(`{"state":"connected","root":"`+root+`","updated_at":"`+time.Now().UTC().Format(time.RFC3339)+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if env := strings.Join(localSyncRuntimeEnv(paths), "\n"); !strings.Contains(env, "COMMENT_IO_LOCAL_SYNC_FRESHNESS=periodic") {
		t.Fatalf("live status should stay periodic when background sync is disabled: %s", env)
	}
	if err := os.WriteFile(filepath.Join(paths.Home, "sync", "config.json"), []byte(`{"root":"`+root+`","background_sync":true,"live_sync_enabled":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if env := strings.Join(localSyncRuntimeEnv(paths), "\n"); !strings.Contains(env, "COMMENT_IO_LOCAL_SYNC_FRESHNESS=live") {
		t.Fatalf("fresh connected live status should expose live freshness: %s", env)
	}
	docsRoot := filepath.Join(root, "_Comment.io Docs")
	if err := os.MkdirAll(docsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docsRoot, "llms.txt"), []byte("# docs\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docsRoot, localSyncAgentDocsMarkerName), []byte(`{"managed_by":"comment sync","kind":"local-agent-docs"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if env := strings.Join(localSyncRuntimeEnv(paths), "\n"); !strings.Contains(env, "COMMENT_IO_LOCAL_DOCS_ROOT="+docsRoot) {
		t.Fatalf("env should expose docs root when llms.txt exists: %s", env)
	}
}

func TestTransientRuntimeLaunchCommandRunsWithoutRedirectingOutput(t *testing.T) {
	home := privateTestDir(t, "comment-bus-runtime-capture-home-")
	runtimePath := filepath.Join(home, "claude")
	if err := os.WriteFile(runtimePath, []byte("#!/bin/sh\nprintf 'stdout profile=%s run=%s\\n' \"$COMMENT_IO_PROFILE\" \"$COMMENT_IO_RUNTIME_RUN\"\nprintf 'color term=%s colorterm=%s force=%s clicolor=%s nocolor=%s\\n' \"$TERM\" \"$COLORTERM\" \"$FORCE_COLOR\" \"$CLICOLOR_FORCE\" \"${NO_COLOR:-}\"\nprintf 'stderr marker\\n' >&2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	command := transientRuntimeLaunchCommand(TransientRuntimeRecord{
		Profile:        "max.reviewer",
		RuntimeCommand: []string{"claude"},
		RuntimePath:    runtimePath,
		Env:            []string{"TERM=dumb", "COLORTERM=bad\nvalue", "FORCE_COLOR=0", "CLICOLOR_FORCE=0", "NO_COLOR=1"},
		OutputLogPath:  filepath.Join(home, "runtime.log"),
	})
	if syntax, err := exec.Command("/bin/sh", "-n", "-c", command).CombinedOutput(); err != nil {
		t.Fatalf("launch command is not valid shell: %v\n%s\ncommand:\n%s", err, syntax, command)
	}
	output, err := exec.Command("/bin/sh", "-c", command).CombinedOutput()
	if err != nil {
		t.Fatalf("launch command failed: %v\n%s\ncommand:\n%s", err, output, command)
	}
	got := string(output)
	if !strings.Contains(got, "stdout profile=max.reviewer run=1") {
		t.Fatalf("runtime stdout/env not visible:\n%s", got)
	}
	if !strings.Contains(got, "color term=xterm-256color colorterm=truecolor force=1 clicolor=1 nocolor=") {
		t.Fatalf("runtime color env not visible:\n%s", got)
	}
	if !strings.Contains(got, "stderr marker") {
		t.Fatalf("runtime stderr not visible:\n%s", got)
	}
}

func TestTrustedExecutableSearchDirsIncludesUserLocalBin(t *testing.T) {
	dir := privateTestDir(t, "comment-bus-userbin-")
	t.Setenv("HOME", dir)

	localBin := filepath.Join(dir, ".local", "bin")
	homeBin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(localBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(homeBin, 0o755); err != nil {
		t.Fatal(err)
	}

	dirs := trustedExecutableSearchDirs("")
	want := map[string]bool{localBin: false, homeBin: false}
	for _, d := range dirs {
		if _, ok := want[d]; ok {
			want[d] = true
		}
	}
	for path, found := range want {
		if !found {
			t.Fatalf("expected %s in trusted search dirs, got %#v", path, dirs)
		}
	}
}

func TestTrustedExecutableSearchDirsRejectsWorldWritableUserBin(t *testing.T) {
	dir := privateTestDir(t, "comment-bus-userbin-bad-")
	t.Setenv("HOME", dir)

	localBin := filepath.Join(dir, ".local", "bin")
	if err := os.MkdirAll(localBin, 0o755); err != nil {
		t.Fatal(err)
	}
	// world-writable: must be rejected by validateTrustedSearchPathDir
	if err := os.Chmod(localBin, 0o777); err != nil {
		t.Fatal(err)
	}

	for _, d := range trustedExecutableSearchDirs("") {
		if d == localBin {
			t.Fatalf("world-writable %s should not be trusted; got %#v", localBin, trustedExecutableSearchDirs(""))
		}
	}
}

func TestResolveConfiguredTmuxBinaryPrecedence(t *testing.T) {
	t.Setenv(TmuxBinaryEnv, "")
	if got := ResolveConfiguredTmuxBinary(""); got != "tmux" {
		t.Fatalf("default = %q, want tmux", got)
	}
	if got := ResolveConfiguredTmuxBinary("/opt/known/tmux"); got != "/opt/known/tmux" {
		t.Fatalf("explicit = %q, want /opt/known/tmux", got)
	}
	t.Setenv(TmuxBinaryEnv, "/env/tmux")
	if got := ResolveConfiguredTmuxBinary(""); got != "/env/tmux" {
		t.Fatalf("env = %q, want /env/tmux", got)
	}
	// explicit beats env
	if got := ResolveConfiguredTmuxBinary("/explicit/tmux"); got != "/explicit/tmux" {
		t.Fatalf("explicit-over-env = %q, want /explicit/tmux", got)
	}
	// whitespace-only inputs are ignored
	t.Setenv(TmuxBinaryEnv, "   ")
	if got := ResolveConfiguredTmuxBinary("  "); got != "tmux" {
		t.Fatalf("blank inputs = %q, want tmux", got)
	}
}

func TestLooksLikeWrapperScript(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "tmux")
	if err := os.WriteFile(script, []byte("#!/usr/bin/env bash\nexec /opt/homebrew/bin/tmux \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !looksLikeWrapperScript(script) {
		t.Fatalf("expected %s detected as wrapper script", script)
	}
	bin := filepath.Join(dir, "realbin")
	if err := os.WriteFile(bin, []byte{0x7f, 'E', 'L', 'F', 0x02, 0x01, 0x01, 0x00}, 0o755); err != nil {
		t.Fatal(err)
	}
	if looksLikeWrapperScript(bin) {
		t.Fatalf("ELF-like binary %s should not be a wrapper", bin)
	}
	if looksLikeWrapperScript(filepath.Join(dir, "does-not-exist")) {
		t.Fatalf("missing file should not be a wrapper")
	}
}

// Auto-discovery is limited to the trusted dirs and must never consult $PATH.
// When no tmux is in the trusted dirs, the resolver must error out rather than
// LookPath a shim (script OR compiled) that merely happens to be first on PATH.
// (Regression for Codex review of #540.)
func TestResolveTrustedExecutableWithSearchDoesNotConsultPath(t *testing.T) {
	pathDir := privateTestDir(t, "comment-bus-shim-")
	shim := filepath.Join(pathDir, "tmux")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nexec real-tmux \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", pathDir)
	emptyDir := privateTestDir(t, "comment-bus-empty-")
	resolved, err := resolveTrustedExecutableWithSearch("tmux", "tmux binary", []string{emptyDir})
	if err == nil {
		t.Fatalf("expected error when tmux is absent from trusted dirs; got resolved=%q (PATH was consulted)", resolved)
	}
	if resolved == shim {
		t.Fatalf("resolver selected a PATH shim %q; it must not consult $PATH", shim)
	}
	if !strings.Contains(err.Error(), "not found in trusted directories") {
		t.Fatalf("expected a trusted-dir error, got: %v", err)
	}
}

// A FIFO (or other special file) named "tmux" in a trusted dir must not hang
// the resolver: looksLikeWrapperScript stats before opening, so a blocking
// open() never happens. (Regression for Codex review of #540.)
func TestLooksLikeWrapperScriptDoesNotBlockOnFIFO(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "tmux")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}
	done := make(chan bool, 1)
	go func() { done <- looksLikeWrapperScript(fifo) }()
	select {
	case got := <-done:
		if got {
			t.Fatalf("FIFO should not be reported as a wrapper script")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("looksLikeWrapperScript blocked on a FIFO (should stat before open)")
	}
}
