package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

// runListen powers `comment listen`. It has two shapes:
//
//   - claim/release/handles subcommands talk to the daemon over the socket to
//     coordinate impromptu free-handle listen claims (an interactive Claude Code
//     session attaching to a handle the daemon does NOT manage as a bot).
//   - `comment listen <handle>` is a launcher shortcut: it execs `claude` with
//     COMMENT_IO_PROFILE=<handle> and COMMENT_IO_LISTEN=1 set so the plugin
//     inside Claude listens on that handle. The headline path is bare `claude`,
//     which the plugin handles on its own; this shortcut just preselects the
//     handle.
func runListen(args []string) error {
	if len(args) == 0 {
		return errors.New("comment listen requires a handle or one of: handles, claim, release")
	}
	switch args[0] {
	case "handles":
		return runListenHandles(args[1:])
	case "claim":
		return runListenClaim(args[1:])
	case "release":
		return runListenRelease(args[1:])
	default:
		handle := args[0]
		if strings.HasPrefix(handle, "-") {
			return fmt.Errorf("comment listen: unknown flag or subcommand %q", handle)
		}
		if !commentbus.ProfileRE.MatchString(handle) {
			return fmt.Errorf("comment listen: %q is not a valid handle", handle)
		}
		// Drop one leading `--` separator (`comment listen <handle> -- <claude args>`)
		// so a literal `--` isn't passed through to claude, which would stop its own
		// option parsing and treat the intended flags as positional input.
		extraArgs := args[1:]
		if len(extraArgs) > 0 && extraArgs[0] == "--" {
			extraArgs = extraArgs[1:]
		}
		return runListenLaunch(handle, extraArgs)
	}
}

func runListenHandles(args []string) error {
	fs := flag.NewFlagSet("comment listen handles", flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) > 0 {
		return errors.New("listen handles does not accept positional arguments")
	}
	paths, err := resolveCLIPaths(*home)
	if err != nil {
		return err
	}
	auth, err := listenAuth(paths, "")
	if err != nil {
		return err
	}
	if *jsonOut {
		return callSocketAndPrint(context.Background(), paths, "listen.handles", auth, map[string]any{}, 10*time.Second)
	}
	response, err := callSocket(context.Background(), paths, "listen.handles", auth, map[string]any{}, 10*time.Second)
	if err != nil {
		return err
	}
	if !response.OK {
		if err := printJSON(response); err != nil {
			return err
		}
		return socketResponseError(response)
	}
	printListenHandlesTable(response.Result)
	return nil
}

func printListenHandlesTable(result any) {
	resultMap, _ := result.(map[string]any)
	rows, _ := resultMap["handles"].([]any)
	if len(rows) == 0 {
		fmt.Println("No configured handles.")
		return
	}
	type line struct{ handle, kind, claim string }
	lines := make([]line, 0, len(rows))
	width := 0
	for _, raw := range rows {
		entry, _ := raw.(map[string]any)
		handle, _ := entry["handle"].(string)
		kind := "free"
		if managed, _ := entry["managed"].(bool); managed {
			kind = "managed"
		}
		claim := ""
		if claimed, _ := entry["claimed"].(bool); claimed {
			claim = "claimed"
			if by, ok := entry["claimed_by"].(string); ok && by != "" {
				claim = "claimed by " + by
			}
		}
		if len(handle) > width {
			width = len(handle)
		}
		lines = append(lines, line{handle: handle, kind: kind, claim: claim})
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i].handle < lines[j].handle })
	for _, l := range lines {
		text := fmt.Sprintf("%-*s  %-7s", width, l.handle, l.kind)
		if l.claim != "" {
			text += "  " + l.claim
		}
		fmt.Println(strings.TrimRight(text, " "))
	}
}

func runListenClaim(args []string) error {
	return runListenClaimOp("listen.claim", "claim", args)
}

func runListenRelease(args []string) error {
	return runListenClaimOp("listen.release", "release", args)
}

func runListenClaimOp(op string, name string, args []string) error {
	fs := flag.NewFlagSet("comment listen "+name, flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	profile := fs.String("profile", "", "handle to "+name)
	session := fs.String("session", "", "opaque listening-session id recorded as claimed_by")
	force := fs.Bool("force", false, "release even when held by another session (recovery/cleanup); release only")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	handle := *profile
	positionals := fs.Args()
	if handle == "" && len(positionals) > 0 {
		handle = positionals[0]
		positionals = positionals[1:]
	}
	if len(positionals) > 0 {
		return fmt.Errorf("listen %s accepts at most one positional handle", name)
	}
	if handle == "" {
		return fmt.Errorf("listen %s requires --profile <handle>", name)
	}
	if !commentbus.ProfileRE.MatchString(handle) {
		return errors.New("invalid profile")
	}
	// Impromptu `/comment listen` attach is for a bare `claude` session. Inside any
	// `comment run` (COMMENT_IO_RUNTIME_RUN set) the runtime is already serviced by
	// the daemon and the Stop hook ignores impromptu bindings there, so a claim
	// would just strand the handle. Refuse it (release/handles stay allowed).
	if name == "claim" && os.Getenv("COMMENT_IO_RUNTIME_RUN") != "" {
		return errors.New("comment listen claim is for a bare `claude` session; inside `comment run` the daemon already delivers — use the managed session, or detach first")
	}
	paths, err := resolveCLIPaths(*home)
	if err != nil {
		return err
	}
	auth, err := listenAuth(paths, handle)
	if err != nil {
		return err
	}
	params := map[string]any{"profile": handle}
	if *session != "" {
		params["session"] = *session
	}
	if *force {
		params["force"] = true
	}
	return callSocketAndPrint(context.Background(), paths, op, auth, params, 10*time.Second)
}

// listenAuth resolves owner auth for `comment listen`. An impromptu listen runs
// bare `claude` with COMMENT_IO_PROFILE set but no managed-session triple;
// tolerate that profile-only environment and fall through to profile-scoped
// owner auth, while refusing a real managed session (which must use
// `comment run <handle>`).
var errListenFromManagedSession = errors.New("comment listen is not allowed from a managed session; use comment run")

func listenAuth(paths commentbus.Paths, profile string) (*commentbus.SocketAuth, error) {
	_, _, ok, err := sessionAuthFromEnv(paths)
	if err != nil && !errors.Is(err, errIncompleteManagedSessionEnv) {
		return nil, err
	}
	if ok && err == nil {
		return nil, errListenFromManagedSession
	}
	return ownerAuth(paths, profile)
}

// runListenLaunchChild is the indirection used to exec the launched runtime so
// it can be overridden in tests.
var runListenLaunchChild = defaultRunListenLaunchChild

func runListenLaunch(handle string, extraArgs []string) error {
	token := fmt.Sprintf("launch-%d", os.Getpid())
	release, _, refusal := claimForLaunch(handle, token)
	if refusal != nil {
		return refusal
	}
	if release != nil {
		defer release()
	}
	// Keep this launcher alive across terminal signals for EVERY launch-token
	// session, whether or not an initial claim was acquired. Two reasons: the
	// deferred release (when we did claim) must run rather than being skipped by a
	// signal-killed parent; and the child always carries
	// COMMENT_IO_LISTEN_SESSION=launch-<our pid>, so the rewake loop's liveness
	// check re-claims only while THIS process is alive — if Ctrl-C killed the parent
	// while Claude kept running (the no-daemon-at-launch case), the listener would
	// disarm and never re-claim when the daemon appears. Ctrl-C / SIGTERM / SIGHUP go
	// to the whole foreground group, so the child `claude` receives them directly and
	// decides whether to exit; we only absorb them here so the parent outlives the
	// child and runs its cleanup when the child returns.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigCh)
	go func() {
		for range sigCh {
		}
	}()
	set := map[string]string{
		"COMMENT_IO_PROFILE": handle,
		"COMMENT_IO_LISTEN":  "1",
		// Always scope the launched session's rewake wait to this launcher's token —
		// even when we could not claim up front (daemon down at launch). An unscoped
		// wait would let a daemon that appears later deliver this handle without
		// enforcing a listen claim (a handle that may be managed/claimed elsewhere).
		// With the token always set, the wait is claim-scoped: if we hold the claim it
		// proceeds; otherwise the rewake loop re-claims for this token while the
		// launcher is alive (covering both daemon-restart and daemon-started-late) or
		// disarms if the handle is owned elsewhere.
		"COMMENT_IO_LISTEN_SESSION": token,
	}
	env := mcpChildEnv(os.Environ(), set, []string{
		"COMMENT_IO_BOT_NAME",
		"COMMENT_IO_SESSION_ID",
		"COMMENT_IO_SESSION_GENERATION",
		"COMMENT_IO_SESSION_CAP_FILE",
		"COMMENT_IO_RUNTIME_RUN",
	})
	return runListenLaunchChild(extraArgs, env)
}

// claimForLaunch best-effort claims handle with the daemon before arming a
// launched listener, so `comment listen <handle>` cannot listen on a managed or
// already-claimed handle. It returns a release func when a claim was acquired
// (call after the child exits), and a non-nil refusal ONLY when a reachable
// daemon refused (MANAGED_HANDLE/HANDLE_BUSY) — the caller must not launch then.
// When no daemon is reachable (or owner auth is unavailable) it returns
// (nil, nil) so the bare-claude / raw-WS path still works; a managed-session
// caller is refused (it should use `comment run`).
func claimForLaunch(handle, token string) (release func(), claimed bool, refusal error) {
	paths, err := resolveCLIPaths("")
	if err != nil {
		return nil, false, nil
	}
	auth, err := listenAuth(paths, handle)
	if err != nil {
		if errors.Is(err, errListenFromManagedSession) {
			return nil, false, err
		}
		return nil, false, nil // no owner auth to coordinate; proceed best-effort
	}
	ctx := context.Background()
	// Best-effort release of THIS launch token on exit. Returned whenever owner auth
	// is available — even if the initial claim below fails because the daemon is down
	// at launch — because the rewake loop can listen.claim the same token once the
	// daemon appears, and the SessionEnd hook is inert for launcher sessions. Without
	// this, that late-acquired claim would stay until `--force` / a daemon restart.
	// Releasing a handle we do not hold is a no-op.
	releaseToken := func() {
		_, _ = callSocket(ctx, paths, "listen.release", auth, map[string]any{"profile": handle, "session": token}, 10*time.Second)
	}
	resp, err := callSocket(ctx, paths, "listen.claim", auth, map[string]any{"profile": handle, "session": token}, 10*time.Second)
	if err != nil {
		// Daemon unreachable now; still release any claim the rewake loop acquires later.
		return releaseToken, false, nil
	}
	if !resp.OK {
		_ = printJSON(resp)
		return nil, false, socketResponseError(resp)
	}
	return releaseToken, true, nil
}

func defaultRunListenLaunchChild(extraArgs []string, env []string) error {
	resolved, err := exec.LookPath("claude")
	if err != nil {
		return errors.New("comment listen: cannot find claude on PATH; install it or run claude directly")
	}
	cmd := exec.Command(resolved, extraArgs...)
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return cliExitError{Code: exitErr.ExitCode()}
		}
		return err
	}
	return nil
}
