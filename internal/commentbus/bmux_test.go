//go:build darwin || linux

package commentbus

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

var errUnexpectedBmuxTestFrame = errors.New("unexpected bmux test frame")

func stubBmuxProcessIdentity(t *testing.T) {
	t.Helper()
	old := bmuxProcessIdentityFn
	bmuxProcessIdentityFn = func(pid uint32) (string, error) {
		if pid == 0 {
			return "", ErrTmuxSessionMissing
		}
		return "test-pid-" + strconv.FormatUint(uint64(pid), 10), nil
	}
	t.Cleanup(func() { bmuxProcessIdentityFn = old })
}

func TestExecBmuxControllerWaitForOutputObservesSubscribeMarker(t *testing.T) {
	paths := testDaemonPaths(t)
	sessionName := "comment-reviewer-abc123"
	socketPath, err := BmuxSocketPathForSession(paths, sessionName)
	if err != nil {
		t.Fatal(err)
	}
	tokenFile, err := BmuxTokenFileForSession(paths, sessionName)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WritePrivateFileAtomic(tokenFile, []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	serverErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		hello, err := readBmuxFrame(conn)
		if err != nil {
			serverErr <- err
			return
		}
		if len(hello) != 34 || hello[0] != bmuxMsgHello || hello[1] != bmuxProtocolVersion {
			serverErr <- errUnexpectedBmuxTestFrame
			return
		}
		if err := writeBmuxFrame(conn, []byte{bmuxMsgOK}); err != nil {
			serverErr <- err
			return
		}
		subscribe, err := readBmuxFrame(conn)
		if err != nil {
			serverErr <- err
			return
		}
		if len(subscribe) != 1 || subscribe[0] != bmuxMsgSubscribe {
			serverErr <- errUnexpectedBmuxTestFrame
			return
		}
		if err := writeBmuxFrame(conn, []byte{bmuxMsgOK}); err != nil {
			serverErr <- err
			return
		}
		if err := writeBmuxFrame(conn, append([]byte{bmuxMsgOutput}, []byte("prefix \x1b[?20")...)); err != nil {
			serverErr <- err
			return
		}
		if err := writeBmuxFrame(conn, append([]byte{bmuxMsgOutput}, []byte("04h suffix")...)); err != nil {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	controller := ExecBmuxController{Paths: paths}
	ready, err := controller.WaitForOutput(context.Background(), sessionName, []byte("\x1b[?2004h"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !ready {
		t.Fatal("WaitForOutput() did not observe readiness marker")
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestReadBmuxFrameUntilPreservesPartialFrameAcrossTimeouts(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	payload := append([]byte{bmuxMsgOutput}, []byte("ready")...)
	done := make(chan error, 1)
	go func() {
		var prefix [4]byte
		binary.LittleEndian.PutUint32(prefix[:], uint32(len(payload)))
		if _, err := server.Write(prefix[:2]); err != nil {
			done <- err
			return
		}
		time.Sleep(50 * time.Millisecond)
		if _, err := server.Write(prefix[2:]); err != nil {
			done <- err
			return
		}
		time.Sleep(50 * time.Millisecond)
		if _, err := server.Write(payload[:2]); err != nil {
			done <- err
			return
		}
		time.Sleep(50 * time.Millisecond)
		_, err := server.Write(payload[2:])
		done <- err
	}()

	frame, err := readBmuxFrameUntil(client, 10*time.Millisecond, time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(frame, payload) {
		t.Fatalf("frame = %#v, want %#v", frame, payload)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestExecBmuxControllerCommandPlacesSubcommandBeforeSocket(t *testing.T) {
	paths := testDaemonPaths(t)
	sessionName := "comment-reviewer-abc123"
	controller := ExecBmuxController{Binary: "/bin/echo", Paths: paths}
	cmd, _, cancel, err := controller.command(context.Background(), sessionName, "status")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	socketPath, err := BmuxSocketPathForSession(paths, sessionName)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"status", "-S", socketPath}
	if !reflect.DeepEqual(cmd.Args[1:], want) {
		t.Fatalf("bmux args = %#v, want %#v", cmd.Args[1:], want)
	}
	cmd, _, cancel, err = controller.command(context.Background(), sessionName, "send-keys", "-l", "hello")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	want = []string{"send-keys", "-S", socketPath, "-l", "hello"}
	if !reflect.DeepEqual(cmd.Args[1:], want) {
		t.Fatalf("bmux send-keys args = %#v, want %#v", cmd.Args[1:], want)
	}
}

func TestExecBmuxControllerTreatsStoppedChildAsKillableSession(t *testing.T) {
	paths := testDaemonPaths(t)
	tempDir := t.TempDir()
	binary := filepath.Join(tempDir, "bmux")
	killFile := filepath.Join(tempDir, "killed.txt")
	script := strings.Join([]string{
		"#!/bin/sh",
		"case \"$1\" in",
		"  status)",
		"    printf 'pid 4242\\nchild_pgid 4242\\nchild_alive false\\nforeground \\n'",
		"    exit 0",
		"    ;;",
		"  kill)",
		"    printf '%s\\n' \"$@\" > " + shellQuote(killFile),
		"    exit 0",
		"    ;;",
		"esac",
		"exit 2",
		"",
	}, "\n")
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	sessionName := "comment-reviewer-abc123"
	controller := ExecBmuxController{Binary: binary, Paths: paths}
	live, err := controller.HasSession(context.Background(), sessionName)
	if err != nil {
		t.Fatal(err)
	}
	if !live {
		t.Fatal("HasSession returned false for a live bmux control server with an exited child")
	}
	paneTarget, err := BmuxSocketPathForSession(paths, sessionName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.PaneCurrentCommand(context.Background(), paneTarget); !errors.Is(err, ErrTmuxSessionMissing) {
		t.Fatalf("PaneCurrentCommand err = %v, want ErrTmuxSessionMissing", err)
	}
	if err := controller.KillSession(context.Background(), sessionName); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(killFile)
	if err != nil {
		t.Fatal(err)
	}
	socketPath, err := BmuxSocketPathForSession(paths, sessionName)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(args), "kill\n-S\n"+socketPath+"\n"; got != want {
		t.Fatalf("kill args = %q, want %q", got, want)
	}
}

func TestResolveTrustedBmuxBinaryPathMentionsBmuxEnv(t *testing.T) {
	emptyDir := privateTestDir(t, "comment-bus-empty-bmux-")
	_, err := resolveTrustedExecutableWithSearchAndEnv("bmux", "bmux binary", []string{emptyDir}, BmuxBinaryEnv)
	if err == nil {
		t.Fatal("expected error when bmux is absent from trusted dirs")
	}
	if !strings.Contains(err.Error(), BmuxBinaryEnv) {
		t.Fatalf("bmux resolver error = %v, want %s", err, BmuxBinaryEnv)
	}
	if strings.Contains(err.Error(), TmuxBinaryEnv) {
		t.Fatalf("bmux resolver error mentioned tmux env: %v", err)
	}
}

func TestBmuxMissingSessionOutputDoesNotHideTokenFailures(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   bool
	}{
		{
			name:   "socket connect failure",
			output: "failed to connect to /tmp/comment.sock: no such file or directory",
			want:   true,
		},
		{
			name:   "connection refused",
			output: "connection refused",
			want:   true,
		},
		{
			name:   "missing token file",
			output: "open /tmp/comment.token: no such file or directory",
			want:   false,
		},
		{
			name:   "bad token closes connection",
			output: "server closed connection",
			want:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := bmuxMissingSessionOutput(tc.output); got != tc.want {
				t.Fatalf("bmuxMissingSessionOutput(%q) = %v, want %v", tc.output, got, tc.want)
			}
		})
	}
}

func TestExecBmuxControllerNewSessionReapsLauncher(t *testing.T) {
	stubBmuxProcessIdentity(t)
	paths := testDaemonPaths(t)
	tempDir := t.TempDir()
	binary := filepath.Join(tempDir, "bmux")
	argsFile := filepath.Join(tempDir, "args.txt")
	script := "#!/bin/sh\nif [ \"$1\" = \"status\" ]; then printf 'pid 12345\\nchild_pgid 12345\\nchild_alive true\\nforeground claude\\n'; exit 0; fi\nprintf '%s\\n' \"$@\" > " + shellQuote(argsFile) + "\nsocket=\nwhile [ $# -gt 0 ]; do\n  if [ \"$1\" = \"-S\" ]; then shift; socket=\"$1\"; fi\n  shift || true\ndone\n: > \"$socket\"\nexit 0\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	controller := ExecBmuxController{Binary: binary, Paths: paths}
	err := controller.NewSession(context.Background(), TmuxNewSessionOptions{
		SessionName: "comment-reviewer-abc123",
		WorkingDir:  paths.Home,
		CommentHome: paths.Home,
		BotletsHome: paths.Home,
		Command:     "true",
	})
	if err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "\n--\n"+bmuxChildShell+"\n-lc\n") {
		t.Fatalf("bmux child shell args = %q, want absolute shell after --", string(args))
	}
	credential, err := readBmuxSessionCredential(paths, "comment-reviewer-abc123")
	if err != nil {
		t.Fatal(err)
	}
	if credential.SessionName != "comment-reviewer-abc123" || credential.ChildPID != 12345 || credential.ChildPGID != 12345 {
		t.Fatalf("bmux credential = %+v, want session + child pid/pgid", credential)
	}
	if credential.ChildIdentity != "test-pid-12345" {
		t.Fatalf("bmux child identity = %q, want test-pid-12345", credential.ChildIdentity)
	}
	if token, err := readBmuxToken(paths, "comment-reviewer-abc123"); err != nil || token == [32]byte{} {
		t.Fatalf("readBmuxToken with metadata = %x, %v", token, err)
	}
	time.Sleep(50 * time.Millisecond)
	// A second Wait on an already reaped child would return os.ErrProcessDone;
	// this smoke assertion mainly prevents the test binary from leaving an
	// unreaped helper process behind when NewSession uses a shell pipeline.
	if out, err := exec.Command(binary, "-S", filepath.Join(t.TempDir(), "noop.sock")).CombinedOutput(); err != nil {
		t.Fatalf("helper sanity failed: %v (%s)", err, out)
	}
}

func TestExecBmuxControllerNewSessionForwardsRuntimeEnvironment(t *testing.T) {
	stubBmuxProcessIdentity(t)
	t.Setenv(EnvVar, EnvStaging)
	t.Setenv(StagingBaseURLEnvVar, "https://staging.example")
	t.Setenv(BaseURLEnvVar, "https://generic.example")

	paths := testDaemonPaths(t)
	tempDir := t.TempDir()
	binary := filepath.Join(tempDir, "bmux")
	envFile := filepath.Join(tempDir, "env.txt")
	script := "#!/bin/sh\nif [ \"$1\" = \"status\" ]; then printf 'pid 12345\\nchild_pgid 12345\\nchild_alive true\\nforeground claude\\n'; exit 0; fi\n{ printf 'COMMENT_IO_ENV=%s\\n' \"$COMMENT_IO_ENV\"; printf 'COMMENT_IO_STAGING_BASE_URL=%s\\n' \"$COMMENT_IO_STAGING_BASE_URL\"; printf 'COMMENT_IO_BASE_URL=%s\\n' \"$COMMENT_IO_BASE_URL\"; } > " + shellQuote(envFile) + "\nsocket=\nwhile [ $# -gt 0 ]; do\n  if [ \"$1\" = \"-S\" ]; then shift; socket=\"$1\"; fi\n  shift || true\ndone\n: > \"$socket\"\nexit 0\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	controller := ExecBmuxController{Binary: binary, Paths: paths}
	if err := controller.NewSession(context.Background(), TmuxNewSessionOptions{
		SessionName: "comment-reviewer-abc123",
		WorkingDir:  paths.Home,
		CommentHome: paths.Home,
		BotletsHome: paths.Home,
		Command:     "true",
	}); err != nil {
		t.Fatal(err)
	}
	envData, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	env := string(envData)
	for _, want := range []string{
		EnvVar + "=" + EnvStaging,
		StagingBaseURLEnvVar + "=https://staging.example",
		BaseURLEnvVar + "=https://generic.example",
	} {
		if !strings.Contains(env, want) {
			t.Fatalf("bmux launch env missing %q:\n%s", want, env)
		}
	}
}

func TestExecBmuxControllerNewSessionPreservesLiveSocketAndToken(t *testing.T) {
	paths := testDaemonPaths(t)
	sessionName := "comment-reviewer-abc123"
	socketPath, err := BmuxSocketPathForSession(paths, sessionName)
	if err != nil {
		t.Fatal(err)
	}
	tokenFile, err := BmuxTokenFileForSession(paths, sessionName)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		t.Fatal(err)
	}
	originalToken := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n"
	if err := WritePrivateFileAtomic(tokenFile, []byte(originalToken), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan struct{})
	go func() {
		conn, _ := listener.Accept()
		if conn != nil {
			_ = conn.Close()
		}
		close(accepted)
	}()

	tempDir := t.TempDir()
	binary := filepath.Join(tempDir, "bmux")
	calledFile := filepath.Join(tempDir, "called")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n: > "+shellQuote(calledFile)+"\nexit 2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	controller := ExecBmuxController{Binary: binary, Paths: paths}
	err = controller.NewSession(context.Background(), TmuxNewSessionOptions{
		SessionName: sessionName,
		WorkingDir:  paths.Home,
		CommentHome: paths.Home,
		BotletsHome: paths.Home,
		Command:     "true",
	})
	if err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("NewSession err = %v, want live socket collision", err)
	}
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("bmux live-socket probe did not connect")
	}
	data, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != originalToken {
		t.Fatalf("token file was overwritten: %q", data)
	}
	if _, err := os.Stat(calledFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bmux binary should not have been invoked, stat err=%v", err)
	}
}

func TestExecBmuxControllerNewSessionLetsBmuxReplaceStaleSocket(t *testing.T) {
	stubBmuxProcessIdentity(t)
	paths := testDaemonPaths(t)
	sessionName := "comment-reviewer-abc123"
	socketPath, err := BmuxSocketPathForSession(paths, sessionName)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(socketPath, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	tempDir := t.TempDir()
	binary := filepath.Join(tempDir, "bmux")
	launchedFile := filepath.Join(tempDir, "launched")
	script := strings.Join([]string{
		"#!/bin/sh",
		"if [ \"$1\" = \"status\" ]; then",
		"  if [ -f " + shellQuote(launchedFile) + " ]; then printf 'pid 12345\\nchild_pgid 12345\\nchild_alive true\\nforeground claude\\n'; exit 0; fi",
		"  printf 'failed to connect\\n' >&2; exit 1",
		"fi",
		"socket=",
		"while [ $# -gt 0 ]; do",
		"  if [ \"$1\" = \"-S\" ]; then shift; socket=\"$1\"; fi",
		"  shift || true",
		"done",
		"if [ \"$(cat \"$socket\")\" != stale ]; then printf 'socket was removed before launch\\n' >&2; exit 9; fi",
		": > " + shellQuote(launchedFile),
		"rm -f \"$socket\"",
		": > \"$socket\"",
		"exit 0",
		"",
	}, "\n")
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	controller := ExecBmuxController{Binary: binary, Paths: paths}
	if err := controller.NewSession(context.Background(), TmuxNewSessionOptions{
		SessionName: sessionName,
		WorkingDir:  paths.Home,
		CommentHome: paths.Home,
		BotletsHome: paths.Home,
		Command:     "true",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(launchedFile); err != nil {
		t.Fatalf("bmux launcher did not run: %v", err)
	}
}

func TestExecBmuxControllerNewSessionCleansUpAfterReadyTimeout(t *testing.T) {
	oldWait := bmuxSocketWaitLimit
	bmuxSocketWaitLimit = 50 * time.Millisecond
	t.Cleanup(func() { bmuxSocketWaitLimit = oldWait })

	paths := testDaemonPaths(t)
	tempDir := t.TempDir()
	binary := filepath.Join(tempDir, "bmux")
	killedFile := filepath.Join(tempDir, "killed")
	script := strings.Join([]string{
		"#!/bin/sh",
		"if [ \"$1\" = \"status\" ]; then printf 'failed to connect\\n' >&2; exit 1; fi",
		"if [ \"$1\" = \"kill\" ]; then : > " + shellQuote(killedFile) + "; exit 0; fi",
		"exit 0",
		"",
	}, "\n")
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	controller := ExecBmuxController{Binary: binary, Paths: paths}
	err := controller.NewSession(context.Background(), TmuxNewSessionOptions{
		SessionName: "comment-reviewer-abc123",
		WorkingDir:  paths.Home,
		CommentHome: paths.Home,
		BotletsHome: paths.Home,
		Command:     "true",
	})
	if err == nil {
		t.Fatal("expected NewSession to fail when bmux never becomes ready")
	}
	if _, err := os.Stat(killedFile); err != nil {
		t.Fatalf("failed launch did not call bmux kill: %v", err)
	}
}

func TestExecBmuxControllerKillSessionFallsBackToPersistedChildProcessGroup(t *testing.T) {
	paths := testDaemonPaths(t)
	sessionName := "comment-reviewer-abc123"
	tokenFile, err := BmuxTokenFileForSession(paths, sessionName)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(tokenFile), 0o700); err != nil {
		t.Fatal(err)
	}
	credential := bmuxSessionCredential{
		TokenHex:      "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SessionName:   sessionName,
		ChildPID:      4242,
		ChildPGID:     4242,
		ChildIdentity: "test-pid-4242",
	}
	if err := writeBmuxSessionCredential(paths, sessionName, credential); err != nil {
		t.Fatal(err)
	}
	oldIdentity := bmuxProcessIdentityFn
	bmuxProcessIdentityFn = func(pid uint32) (string, error) {
		if pid != 4242 {
			t.Fatalf("process identity pid = %d, want 4242", pid)
		}
		return "test-pid-4242", nil
	}
	oldTerminate := terminatePersistedBmuxChildProcessGroupFn
	var killed []uint32
	terminatePersistedBmuxChildProcessGroupFn = func(pid uint32, pgid uint32, identity string) error {
		if pid != 4242 {
			t.Fatalf("persisted child pid = %d, want 4242", pid)
		}
		if identity != "test-pid-4242" {
			t.Fatalf("persisted child identity = %q, want test-pid-4242", identity)
		}
		killed = append(killed, pgid)
		return nil
	}
	t.Cleanup(func() {
		bmuxProcessIdentityFn = oldIdentity
		terminatePersistedBmuxChildProcessGroupFn = oldTerminate
	})
	tempDir := t.TempDir()
	binary := filepath.Join(tempDir, "bmux")
	script := "#!/bin/sh\nif [ \"$1\" = \"kill\" ]; then printf 'failed to connect\\n' >&2; exit 1; fi\nexit 2\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	controller := ExecBmuxController{Binary: binary, Paths: paths}
	if err := controller.KillSession(context.Background(), sessionName); err != nil {
		t.Fatal(err)
	}
	if len(killed) != 1 || killed[0] != 4242 {
		t.Fatalf("persisted child process group kills = %+v, want [4242]", killed)
	}
	if _, err := os.Stat(tokenFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bmux credential should be removed after fallback kill, stat err=%v", err)
	}
}

func TestExecBmuxControllerKillSessionClearsCredentialWhenPersistedChildMissing(t *testing.T) {
	paths := testDaemonPaths(t)
	sessionName := "comment-reviewer-abc123"
	tokenFile, err := BmuxTokenFileForSession(paths, sessionName)
	if err != nil {
		t.Fatal(err)
	}
	if err := WritePrivateFileAtomic(tokenFile, []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\nsession_name "+sessionName+"\nchild_pgid 4242\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldTerminate := terminatePersistedBmuxChildProcessGroupFn
	terminatePersistedBmuxChildProcessGroupFn = func(uint32, uint32, string) error {
		t.Fatal("missing persisted child pid should not signal any process group")
		return nil
	}
	t.Cleanup(func() { terminatePersistedBmuxChildProcessGroupFn = oldTerminate })
	tempDir := t.TempDir()
	binary := filepath.Join(tempDir, "bmux")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nif [ \"$1\" = \"kill\" ]; then printf 'failed to connect\\n' >&2; exit 1; fi\nexit 2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	controller := ExecBmuxController{Binary: binary, Paths: paths}
	if err := controller.KillSession(context.Background(), sessionName); err != nil {
		t.Fatalf("KillSession err = %v, want nil", err)
	}
	if _, err := os.Stat(tokenFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale bmux credential should be removed after missing child, stat err=%v", err)
	}
}

func TestExecBmuxControllerKillSessionClearsCredentialWhenProcessIdentityDiffers(t *testing.T) {
	paths := testDaemonPaths(t)
	sessionName := "comment-reviewer-abc123"
	tokenFile, err := BmuxTokenFileForSession(paths, sessionName)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeBmuxSessionCredential(paths, sessionName, bmuxSessionCredential{
		TokenHex:      "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SessionName:   sessionName,
		ChildPID:      4242,
		ChildPGID:     4242,
		ChildIdentity: "linux:old-boot:123",
	}); err != nil {
		t.Fatal(err)
	}
	oldIdentity := bmuxProcessIdentityFn
	bmuxProcessIdentityFn = func(pid uint32) (string, error) {
		if pid != 4242 {
			t.Fatalf("process identity pid = %d, want 4242", pid)
		}
		return "linux:new-boot:123", nil
	}
	oldTerminate := terminatePersistedBmuxChildProcessGroupFn
	terminatePersistedBmuxChildProcessGroupFn = func(uint32, uint32, string) error {
		t.Fatal("mismatched process identity should not signal any process group")
		return nil
	}
	t.Cleanup(func() {
		bmuxProcessIdentityFn = oldIdentity
		terminatePersistedBmuxChildProcessGroupFn = oldTerminate
	})
	tempDir := t.TempDir()
	binary := filepath.Join(tempDir, "bmux")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nif [ \"$1\" = \"kill\" ]; then printf 'failed to connect\\n' >&2; exit 1; fi\nexit 2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	controller := ExecBmuxController{Binary: binary, Paths: paths}
	if err := controller.KillSession(context.Background(), sessionName); err != nil {
		t.Fatalf("KillSession err = %v, want nil", err)
	}
	if _, err := os.Stat(tokenFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mismatched bmux credential should be removed after missing child, stat err=%v", err)
	}
}

func TestExecBmuxControllerKillSessionMissingCredentialAndSocketIsIdempotent(t *testing.T) {
	paths := testDaemonPaths(t)
	sessionName := "comment-reviewer-abc123"
	tokenFile, err := BmuxTokenFileForSession(paths, sessionName)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(tokenFile), 0o700); err != nil {
		t.Fatal(err)
	}
	oldTerminate := terminatePersistedBmuxChildProcessGroupFn
	terminatePersistedBmuxChildProcessGroupFn = func(uint32, uint32, string) error {
		t.Fatal("missing bmux credential should not signal any process group")
		return nil
	}
	t.Cleanup(func() { terminatePersistedBmuxChildProcessGroupFn = oldTerminate })
	tempDir := t.TempDir()
	binary := filepath.Join(tempDir, "bmux")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nif [ \"$1\" = \"kill\" ]; then printf 'open %s: no such file or directory\\n' \"$BMUX_TOKEN_FILE\" >&2; exit 1; fi\nexit 2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	controller := ExecBmuxController{Binary: binary, Paths: paths}
	if err := controller.KillSession(context.Background(), sessionName); err != nil {
		t.Fatalf("KillSession err = %v, want nil", err)
	}
}

func TestExecBmuxControllerKillSessionMissingCredentialWithLiveSocketIsNotMissing(t *testing.T) {
	paths := testDaemonPaths(t)
	sessionName := "comment-reviewer-abc123"
	socketPath, err := BmuxSocketPathForSession(paths, sessionName)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan struct{})
	go func() {
		conn, _ := listener.Accept()
		if conn != nil {
			_ = conn.Close()
		}
		close(accepted)
	}()
	tempDir := t.TempDir()
	binary := filepath.Join(tempDir, "bmux")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nif [ \"$1\" = \"kill\" ]; then printf 'open %s: no such file or directory\\n' \"$BMUX_TOKEN_FILE\" >&2; exit 1; fi\nexit 2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	controller := ExecBmuxController{Binary: binary, Paths: paths}
	if err := controller.KillSession(context.Background(), sessionName); err == nil || errors.Is(err, ErrTmuxSessionMissing) {
		t.Fatalf("KillSession err = %v, want non-missing token failure", err)
	}
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("bmux live-socket probe did not connect")
	}
}

func TestExecBmuxControllerKillSessionRemovesCredentialAfterLiveKill(t *testing.T) {
	paths := testDaemonPaths(t)
	sessionName := "comment-reviewer-abc123"
	credential := bmuxSessionCredential{
		TokenHex:      "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SessionName:   sessionName,
		ChildPID:      4242,
		ChildPGID:     4242,
		ChildIdentity: "test-pid-4242",
	}
	if err := writeBmuxSessionCredential(paths, sessionName, credential); err != nil {
		t.Fatal(err)
	}
	tokenFile, err := BmuxTokenFileForSession(paths, sessionName)
	if err != nil {
		t.Fatal(err)
	}
	tempDir := t.TempDir()
	binary := filepath.Join(tempDir, "bmux")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nif [ \"$1\" = \"kill\" ]; then exit 0; fi\nexit 2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	controller := ExecBmuxController{Binary: binary, Paths: paths}
	if err := controller.KillSession(context.Background(), sessionName); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tokenFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bmux credential should be removed after live kill, stat err=%v", err)
	}
}
