package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

const maxCLIMessageBodyBytes = 1_000_000

// rewakeDaemonUnavailableExit is returned by `messages wait --rewake` when no
// daemon socket is reachable, signalling the plugin Stop hook to fall back to its
// direct WebSocket listener instead of giving up.
const rewakeDaemonUnavailableExit = 3

type stringListFlag []string

func (f *stringListFlag) String() string {
	return strings.Join(*f, ",")
}

func (f *stringListFlag) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("empty value")
	}
	*f = append(*f, value)
	return nil
}

type cliSocketError struct {
	Code    string
	Message string
}

func (err cliSocketError) Error() string {
	if err.Code == "" {
		return err.Message
	}
	return fmt.Sprintf("%s: %s", err.Code, err.Message)
}

func resolveCLIPaths(home string) (commentbus.Paths, error) {
	envHome := strings.TrimSpace(os.Getenv("COMMENT_IO_HOME"))
	if home == "" {
		home = envHome
	}
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		return paths, err
	}
	// Inject the opt-in TCP transport ONLY when this command targets the
	// env-configured home (COMMENT_IO_HOME) — i.e. the caged daemon the dial
	// address belongs to. Never when --home points at a different/native daemon
	// (it must dial that home's Unix socket), and never for tests using temp
	// homes. The generic ResolvePaths (library/tests) stays free of the env.
	// Compare CLEANED homes (paths.Home is already expanded) so a trailing-slash /
	// relative / ~ --home that resolves to COMMENT_IO_HOME still matches.
	if envHome != "" {
		if cleanedEnvHome, expErr := commentbus.ExpandHome(envHome); expErr == nil && paths.Home == cleanedEnvHome {
			if tcp := strings.TrimSpace(os.Getenv("COMMENT_IO_BUS_TCP_ADDR")); tcp != "" {
				paths.BusTCPAddr = tcp
			}
		}
	}
	return paths, nil
}

func parseInterspersedFlags(fs *flag.FlagSet, args []string) error {
	valueFlags := map[string]struct{}{}
	fs.VisitAll(func(f *flag.Flag) {
		if !isBoolFlag(f) {
			valueFlags[f.Name] = struct{}{}
		}
	})
	normalized, err := interspersedFlagArgs(args, valueFlags)
	if err != nil {
		return err
	}
	return fs.Parse(normalized)
}

func isBoolFlag(f *flag.Flag) bool {
	type boolFlag interface {
		IsBoolFlag() bool
	}
	if value, ok := f.Value.(boolFlag); ok {
		return value.IsBoolFlag()
	}
	return false
}

func runMessages(args []string) error {
	if len(args) == 0 {
		return errors.New(usage())
	}
	switch args[0] {
	case "send":
		return runMessagesSend(args[1:])
	case "wait":
		return runMessagesWait(args[1:])
	case "receive":
		return runMessagesMutation("receive", args[1:])
	case "renew":
		return runMessagesMutation("renew", args[1:])
	case "ack":
		return runMessagesMutation("ack", args[1:])
	case "release":
		return runMessagesMutation("release", args[1:])
	case "list":
		return runMessagesList("list", args[1:])
	case "sent":
		return runMessagesList("sent", args[1:])
	case "repair":
		return repair(args[1:])
	default:
		return fmt.Errorf("unknown messages command %q", args[0])
	}
}

func runActivity(args []string) error {
	if len(args) == 0 {
		return errors.New(usage())
	}
	switch args[0] {
	case "complete":
		return runActivityComplete(args[1:])
	default:
		return fmt.Errorf("unknown activity command %q", args[0])
	}
}

func runActivityComplete(args []string) error {
	fs := flag.NewFlagSet("comment activity complete", flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	profile := fs.String("profile", "", "profile handle")
	messageIDFlag := fs.String("message-id", "", "local message id")
	opID := fs.String("op-id", "", "operation idempotency id")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	messageID := *messageIDFlag
	positionals := fs.Args()
	if messageID == "" && len(positionals) > 0 {
		messageID = positionals[0]
		positionals = positionals[1:]
	}
	if len(positionals) > 0 {
		return errors.New("activity complete accepts at most one positional message id")
	}
	if messageID == "" {
		return errors.New("activity complete requires a message id")
	}
	paths, err := resolveCLIPaths(*home)
	if err != nil {
		return err
	}
	auth, _, ok, err := sessionAuthFromEnv(paths)
	if err != nil {
		return err
	}
	params := map[string]any{"message_id": messageID}
	if !ok {
		selectedProfile := *profile
		if selectedProfile == "" && os.Getenv("COMMENT_IO_RUNTIME_RUN") != "" {
			selectedProfile = os.Getenv("COMMENT_IO_PROFILE")
		}
		if selectedProfile == "" {
			return errors.New("activity complete requires a managed comment run session or runtime profile")
		}
		auth, err = ownerAuth(paths, selectedProfile)
		if err != nil {
			return err
		}
		params["profile"] = selectedProfile
	}
	if *opID != "" {
		params["op_id"] = *opID
	}
	return callSocketAndPrint(context.Background(), paths, "activity.complete", auth, params, 10*time.Second)
}

func runSessions(args []string) error {
	if len(args) == 0 {
		return errors.New(usage())
	}
	switch args[0] {
	case "start":
		return runSessionsStart(args[1:])
	case "status":
		return runSessionsStatus(args[1:])
	case "stop":
		return runSessionsStop(args[1:])
	case "nudge":
		return runSessionsNudge(args[1:])
	default:
		return fmt.Errorf("unknown sessions command %q", args[0])
	}
}

func reloadProfiles(args []string) error {
	fs := flag.NewFlagSet("comment bus reload-profiles", flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	botletsHome := fs.String("botlets-home", "", "Botlets home directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	paths, err := resolveCLIPaths(*home)
	if err != nil {
		return err
	}
	auth, err := ownerOnlyAuth(paths, "")
	if err != nil {
		return err
	}
	params := map[string]any{}
	if *botletsHome != "" {
		params["botlets_home"] = *botletsHome
	}
	return callSocketAndPrint(context.Background(), paths, "reload-profiles", auth, params, 10*time.Second)
}

func runMessagesSend(args []string) error {
	fs := flag.NewFlagSet("comment messages send", flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	botletsHome := fs.String("botlets-home", "", "Botlets home directory")
	bot := fs.String("bot", "", "sender bot name")
	fromBot := fs.String("from-bot", "", "sender bot name")
	profile := fs.String("profile", "", "sender profile handle")
	body := fs.String("body", "", "markdown message body")
	bodyFile := fs.String("body-file", "", "read markdown body from file, or - for stdin")
	threadID := fs.String("thread-id", "", "message thread id")
	idempotencyKey := fs.String("idempotency-key", "", "operation idempotency key")
	var to stringListFlag
	var refs stringListFlag
	fs.Var(&to, "to", "recipient bot name or profile; repeatable")
	fs.Var(&refs, "ref", "safe message ref as key=value; repeatable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	senderBot := firstNonEmpty(*fromBot, *bot)
	content, err := readMessageBody(*body, *bodyFile, fs.Args())
	if err != nil {
		return err
	}
	if len(to) == 0 {
		return errors.New("messages send requires at least one --to recipient")
	}
	paths, err := resolveCLIPaths(*home)
	if err != nil {
		return err
	}
	auth, err := socketAuth(context.Background(), paths, socketAuthOptions{
		Profile:      *profile,
		Bot:          senderBot,
		BotletsHome:  *botletsHome,
		NeedProfile:  senderBot == "" && *profile != "",
		AllowSession: true,
	})
	if err != nil {
		return err
	}
	params := map[string]any{
		"to": to,
		"body": map[string]any{
			"format":  "markdown",
			"content": content,
		},
	}
	if senderBot != "" {
		params["from_bot"] = senderBot
	}
	if parsedRefs, err := parseRefs(refs); err != nil {
		return err
	} else if len(parsedRefs) > 0 {
		params["refs"] = parsedRefs
	}
	if *threadID != "" {
		params["thread_id"] = *threadID
	}
	if *idempotencyKey != "" {
		params["idempotency_key"] = *idempotencyKey
	}
	return callSocketAndPrint(context.Background(), paths, "messages.send", auth, params, messagesSendResponseTimeout())
}

func messagesSendResponseTimeout() time.Duration {
	return managedSessionStartWait
}

func runMessagesWait(args []string) error {
	fs := flag.NewFlagSet("comment messages wait", flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	botletsHome := fs.String("botlets-home", "", "Botlets home directory")
	bot := fs.String("bot", "", "recipient bot name")
	profile := fs.String("profile", "", "recipient profile handle")
	timeout := fs.String("timeout", "", "wait timeout, such as 30m or 10s")
	timeoutMS := fs.Int("timeout-ms", 0, "wait timeout in milliseconds")
	rewake := fs.Bool("rewake", false, "loop until a message arrives, claim it, print it, and exit 2 (for asyncRewake Stop hooks)")
	var kinds stringListFlag
	fs.Var(&kinds, "kind", "message kind filter; repeatable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	paths, err := resolveCLIPaths(*home)
	if err != nil {
		return err
	}
	auth, err := socketAuth(context.Background(), paths, socketAuthOptions{
		Profile:      *profile,
		Bot:          *bot,
		BotletsHome:  *botletsHome,
		AllowSession: true,
	})
	if err != nil {
		// Route to the WebSocket fallback (rc 3) ONLY for a non-managed rewake listener
		// (impromptu `/comment listen` or launcher) — e.g. a raw-profile setup with a
		// stale daemon.sock and no owner capability. The fallback authenticates with the
		// profile's agent secret, so it works without the owner capability. A MANAGED
		// session (COMMENT_IO_SESSION_ID set) must NOT fall back: the daemon is
		// authoritative for it, and a raw-WS fallback would double-deliver alongside the
		// daemon — so a bad/managed cap surfaces as a normal error (hook exits 0), not rc 3.
		if *rewake && os.Getenv("COMMENT_IO_SESSION_ID") == "" {
			return cliExitError{Code: rewakeDaemonUnavailableExit}
		}
		return err
	}
	waitMS, err := timeoutMillis(*timeout, *timeoutMS)
	if err != nil {
		return err
	}
	if *rewake {
		return runMessagesRewake(context.Background(), paths, auth, *bot, *profile, kinds, waitMS)
	}
	params := botProfileParams(*bot, *profile)
	if waitMS > 0 {
		params["timeout_ms"] = waitMS
	}
	if len(kinds) > 0 {
		params["kinds"] = kinds
	}
	responseTimeout := 10 * time.Second
	if waitMS > 0 {
		responseTimeout = time.Duration(waitMS)*time.Millisecond + 10*time.Second
	}
	return callSocketAndPrint(context.Background(), paths, "messages.wait", auth, params, responseTimeout)
}

// runMessagesRewake powers `comment messages wait --rewake`. It blocks (looping
// across wait timeouts) until a local message is available, claims it via
// messages.receive, prints the received message JSON to stdout, and returns an
// exit-2 sentinel. That exit code is what an `async`+`asyncRewake` Claude Code
// Stop hook turns into a model wake-up, with the printed JSON delivered as the
// wake message. A daemon/socket error returns normally (exit 1) so a caller can
// fall back to a direct WebSocket listener when no daemon is running.
func runMessagesRewake(ctx context.Context, paths commentbus.Paths, auth *commentbus.SocketAuth, bot, profile string, kinds stringListFlag, waitMS int) error {
	if waitMS <= 0 {
		// Loop re-waits on each timeout, so the listener is effectively indefinite;
		// a bounded per-wait timeout just keeps the socket call from blocking forever.
		if os.Getenv("COMMENT_IO_LISTEN_SESSION") != "" {
			// Impromptu listen wait: re-check the listen claim on a shorter cadence so
			// a detached/reassigned listener clears promptly rather than lingering.
			waitMS = 120000
		} else {
			waitMS = 600000
		}
	}
	responseTimeout := time.Duration(waitMS)*time.Millisecond + 10*time.Second
	for {
		waitParams := rewakeWaitParams(bot, profile, kinds, waitMS)
		waitResp, err := callSocket(ctx, paths, "messages.wait", auth, waitParams, responseTimeout)
		if err != nil {
			if isDaemonUnavailableError(err) {
				// No reachable daemon to pull from — exit with a distinct code so the
				// Stop hook falls back to its direct WebSocket listener (`comment bus
				// health` can report healthy even when the daemon socket is down).
				return cliExitError{Code: rewakeDaemonUnavailableExit}
			}
			if isConnectionDroppedError(err) {
				// The daemon likely restarted while we were blocked (the connection was
				// already open, so this is EOF/reset rather than refused). Pause briefly
				// and re-wait so the idle session keeps a listener across a restart; the
				// next connect distinguishes a restart (reconnects) from a gone daemon
				// (refused/missing -> exit 3 -> the hook's WebSocket fallback).
				time.Sleep(time.Second)
				continue
			}
			return err
		}
		if !waitResp.OK {
			if waitResp.Error != nil && waitResp.Error.Retryable {
				// Transient daemon error (e.g. an UPSTREAM_ERROR from a notification
				// lease/network blip) — re-wait rather than disarming the idle listener
				// (a bare `comment listen` session has no bmux fallback to recover it).
				time.Sleep(time.Second)
				continue
			}
			return socketResponseError(waitResp)
		}
		result, _ := waitResp.Result.(map[string]any)
		if result == nil {
			continue
		}
		if lost, _ := result["claim_lost"].(bool); lost {
			// claim_lost means our scoped listen claim is no longer held. Two causes
			// that the response can't tell apart: the daemon restarted and dropped its
			// in-memory claims (the handle is now free — we should re-claim and keep
			// listening), or the user explicitly detached (binding removed / launcher
			// exited — we should disarm). reclaimAfterClaimLost disambiguates via the
			// still-wants-to-listen signal and re-claims only in the restart case.
			if listenSession := os.Getenv("COMMENT_IO_LISTEN_SESSION"); listenSession != "" {
				switch err := reclaimAfterClaimLost(ctx, paths, auth, profile, listenSession); {
				case err == nil:
					continue // re-claimed (daemon restart) — keep listening
				case errors.Is(err, errStopListening):
					return nil // detached or owned elsewhere — disarm
				default:
					// Daemon went away again mid-reclaim; re-wait (the next wait routes to
					// the WS fallback via exit 3, or retries) rather than disarming.
					time.Sleep(time.Second)
					continue
				}
			}
			// Unscoped wait (no listen session) — nothing to re-claim; exit so the
			// handle and the per-handle singleton lock free for a re-attach.
			return nil
		}
		if timedOut, _ := result["timeout"].(bool); timedOut {
			continue
		}
		if skipped, _ := result["replay_skipped"].(bool); skipped {
			continue
		}
		// A rewake wait claims the message atomically in the same request and
		// returns it as `message`, so there is no separate (gap-prone) receive
		// call. Print the claimed message and exit 2 to wake the model.
		if _, ok := result["message"]; !ok {
			continue
		}
		// Final detach re-check: a mention can be claimed in the brief window between
		// the detach removing the binding and `listen.release` clearing the claim (the
		// daemon still sees the claim held, so it delivered). If the listener has
		// detached by now, do NOT wake — release the just-claimed message so it
		// re-dispatches to the rightful owner, and disarm.
		if listenSession := os.Getenv("COMMENT_IO_LISTEN_SESSION"); listenSession != "" && !stillWantsToListen(paths, profile, listenSession) {
			if msg, ok := result["message"].(map[string]any); ok {
				if id, ok := msg["id"].(string); ok && id != "" {
					releaseParams := botProfileParams(bot, profile)
					releaseParams["message_id"] = id
					releaseParams["reason"] = "listener_detached"
					_, _ = callSocket(ctx, paths, "messages.release", auth, releaseParams, 10*time.Second)
				}
			}
			return nil
		}
		if err := printJSON(result); err != nil {
			return err
		}
		return cliExitError{Code: 2}
	}
}

// errStopListening signals that a rewake claim_lost is final: this session no
// longer wants to listen (binding removed / launcher exited) or is no longer
// entitled to the handle (claimed elsewhere). The caller exits 0 (disarm) rather
// than re-claiming.
var errStopListening = errors.New("stop listening")

// stillWantsToListen reports whether the session behind listenSession still
// intends to listen on THIS profile — the signal used to tell a daemon restart
// (re-claim) apart from an explicit user detach (disarm). For a `comment listen
// <handle>` launcher (token launch-<pid>) the signal is the launcher process
// still being alive (a launcher process is bound to one handle for its lifetime).
// For an impromptu `/comment listen` session it is the binding file still present
// AND still naming this profile: detach removes the binding BEFORE releasing the
// claim, and re-attaching to a different handle rewrites the binding to that
// handle — so a binding that is gone or now names another handle means this
// handle's listener should disarm, not re-claim.
func stillWantsToListen(paths commentbus.Paths, profile, listenSession string) bool {
	if listenSession == "" {
		return false
	}
	if pidStr, ok := strings.CutPrefix(listenSession, "launch-"); ok {
		pid, err := strconv.Atoi(pidStr)
		if err != nil {
			return false
		}
		return processIsAlive(pid)
	}
	bound, err := os.ReadFile(filepath.Join(paths.Home, "rewake", "bind-"+listenSession))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(bound)) == profile
}

// reclaimAfterClaimLost handles a rewake claim_lost for a scoped (impromptu or
// launcher) listener. It re-claims the handle for the same listen session when
// the session still wants to listen — recovering from a daemon restart that wiped
// the in-memory claim — and returns errStopListening when the session has
// detached or the handle is owned by someone else. A transport error (the daemon
// went away again mid-reclaim) is returned so the caller re-waits rather than
// disarming. Returns nil when the re-claim succeeded and the wait loop should
// continue.
func reclaimAfterClaimLost(ctx context.Context, paths commentbus.Paths, auth *commentbus.SocketAuth, profile, listenSession string) error {
	if !stillWantsToListen(paths, profile, listenSession) {
		return errStopListening
	}
	resp, err := callSocket(ctx, paths, "listen.claim", auth, map[string]any{"profile": profile, "session": listenSession}, 10*time.Second)
	if err != nil {
		return err // transport/daemon-gone: let the caller re-wait
	}
	if !resp.OK {
		// HANDLE_BUSY / MANAGED_HANDLE / FORBIDDEN / NOT_FOUND — not ours to listen.
		return errStopListening
	}
	return nil
}

// rewakeWaitParams builds the params for a `messages.wait --rewake` socket call.
// It always sets rewake:true so the daemon registers a pull-waiter (and skips
// the bmux keystroke for a Claude session that is pulling its own messages), and
// forwards the managed-session triple from the environment so the daemon can key
// the waiter to this exact session. Malformed env values are dropped rather than
// sent, so a bad COMMENT_IO_SESSION_* value never fails the wait outright.
func rewakeWaitParams(bot, profile string, kinds stringListFlag, waitMS int) map[string]any {
	params := botProfileParams(bot, profile)
	params["rewake"] = true
	if waitMS > 0 {
		params["timeout_ms"] = waitMS
	}
	if len(kinds) > 0 {
		params["kinds"] = kinds
	}
	if sessionID := os.Getenv("COMMENT_IO_SESSION_ID"); commentbus.LocalSessionIDRE.MatchString(sessionID) {
		params["session_id"] = sessionID
	}
	if generation := os.Getenv("COMMENT_IO_SESSION_GENERATION"); commentbus.LocalSessionGenerationIDRE.MatchString(generation) {
		params["session_generation"] = generation
	}
	// Impromptu listen: carry the listen claim's session so the daemon can keep the
	// wait claim-scoped (it returns claim_lost if the claim is no longer held by us).
	if ls := os.Getenv("COMMENT_IO_LISTEN_SESSION"); commentbus.ListenSessionTokenRE.MatchString(ls) {
		params["listen_session"] = ls
	}
	return params
}

func runMessagesMutation(op string, args []string) error {
	fs := flag.NewFlagSet("comment messages "+op, flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	botletsHome := fs.String("botlets-home", "", "Botlets home directory")
	bot := fs.String("bot", "", "recipient bot name")
	profile := fs.String("profile", "", "recipient profile handle")
	messageIDFlag := fs.String("message-id", "", "local message id")
	leaseTTL := fs.String("lease", "", "lease TTL, such as 10m")
	leaseTTLMS := fs.Int("lease-ttl-ms", 0, "lease TTL in milliseconds")
	opID := fs.String("op-id", "", "operation idempotency id")
	reason := fs.String("reason", "", "release reason")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	messageID := *messageIDFlag
	positionals := fs.Args()
	if messageID == "" && len(positionals) > 0 {
		messageID = positionals[0]
		positionals = positionals[1:]
	}
	if len(positionals) > 0 {
		return fmt.Errorf("messages %s accepts at most one positional message id", op)
	}
	if messageID == "" {
		return fmt.Errorf("messages %s requires a message id", op)
	}
	paths, err := resolveCLIPaths(*home)
	if err != nil {
		return err
	}
	auth, err := socketAuth(context.Background(), paths, socketAuthOptions{
		Profile:      *profile,
		Bot:          *bot,
		BotletsHome:  *botletsHome,
		NeedProfile:  true,
		AllowSession: true,
	})
	if err != nil {
		return err
	}
	params := map[string]any{"message_id": messageID}
	if *bot != "" {
		params["bot"] = *bot
	}
	if *profile != "" {
		params["profile"] = *profile
	}
	if op == "renew" {
		ttlMS, err := timeoutMillis(*leaseTTL, *leaseTTLMS)
		if err != nil {
			return err
		}
		if ttlMS > 0 {
			params["lease_ttl_ms"] = ttlMS
		}
	}
	if op == "release" && *reason != "" {
		params["reason"] = *reason
	}
	if *opID != "" {
		params["op_id"] = *opID
	}
	return callSocketAndPrint(context.Background(), paths, "messages."+op, auth, params, 10*time.Second)
}

func runMessagesList(command string, args []string) error {
	fs := flag.NewFlagSet("comment messages "+command, flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	botletsHome := fs.String("botlets-home", "", "Botlets home directory")
	bot := fs.String("bot", "", "bot name")
	profile := fs.String("profile", "", "profile handle")
	state := fs.String("state", "", "delivery state")
	limit := fs.Int("limit", 0, "maximum messages to return")
	cursor := fs.String("cursor", "", "pagination cursor")
	if err := fs.Parse(args); err != nil {
		return err
	}
	paths, err := resolveCLIPaths(*home)
	if err != nil {
		return err
	}
	auth, err := socketAuth(context.Background(), paths, socketAuthOptions{
		Profile:      *profile,
		Bot:          *bot,
		BotletsHome:  *botletsHome,
		AllowSession: true,
	})
	if err != nil {
		return err
	}
	params := botProfileParams(*bot, *profile)
	if command == "list" && *state != "" {
		params["state"] = *state
	}
	if *limit > 0 {
		params["limit"] = *limit
	}
	if *cursor != "" {
		params["cursor"] = *cursor
	}
	return callSocketAndPrint(context.Background(), paths, "messages."+command, auth, params, 10*time.Second)
}

func runSessionsStart(args []string) error {
	fs := flag.NewFlagSet("comment sessions start", flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	bot := fs.String("bot", "", "bot name")
	profile := fs.String("profile", "", "profile handle")
	scopeType := fs.String("scope-type", "", "session scope type")
	scopeID := fs.String("scope-id", "", "session scope id")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	positionals := fs.Args()
	if *bot == "" && len(positionals) > 0 {
		*bot = positionals[0]
		positionals = positionals[1:]
	}
	if len(positionals) > 0 {
		return errors.New("sessions start accepts at most one positional bot")
	}
	paths, err := resolveCLIPaths(*home)
	if err != nil {
		return err
	}
	auth, err := ownerOnlyAuth(paths, *profile)
	if err != nil {
		return err
	}
	params := botProfileParams(*bot, *profile)
	if *scopeType != "" {
		params["scope_type"] = *scopeType
	}
	if *scopeID != "" {
		params["scope_id"] = *scopeID
	}
	return callSocketAndPrint(context.Background(), paths, "sessions.start", auth, params, managedSessionStartWait)
}

func runSessionsStatus(args []string) error {
	fs := flag.NewFlagSet("comment sessions status", flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	bot := fs.String("bot", "", "bot name")
	profile := fs.String("profile", "", "profile handle")
	sessionID := fs.String("session-id", "", "session id")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	positionals := fs.Args()
	if *bot == "" && len(positionals) > 0 {
		*bot = positionals[0]
		positionals = positionals[1:]
	}
	if len(positionals) > 0 {
		return errors.New("sessions status accepts at most one positional bot")
	}
	paths, err := resolveCLIPaths(*home)
	if err != nil {
		return err
	}
	auth, err := socketAuth(context.Background(), paths, socketAuthOptions{
		Profile:      *profile,
		Bot:          *bot,
		AllowSession: true,
	})
	if err != nil {
		return err
	}
	params := botProfileParams(*bot, *profile)
	if *sessionID != "" {
		params["session_id"] = *sessionID
	}
	return callSocketAndPrint(context.Background(), paths, "sessions.status", auth, params, 10*time.Second)
}

func runSessionsStop(args []string) error {
	fs := flag.NewFlagSet("comment sessions stop", flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	bot := fs.String("bot", "", "bot name")
	profile := fs.String("profile", "", "profile handle")
	sessionID := fs.String("session-id", "", "session id")
	reason := fs.String("reason", "", "stop reason")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	positionals := fs.Args()
	if *bot == "" && len(positionals) > 0 {
		*bot = positionals[0]
		positionals = positionals[1:]
	}
	if len(positionals) > 0 {
		return errors.New("sessions stop accepts at most one positional bot")
	}
	paths, err := resolveCLIPaths(*home)
	if err != nil {
		return err
	}
	auth, err := socketAuth(context.Background(), paths, socketAuthOptions{
		Profile:      *profile,
		Bot:          *bot,
		AllowSession: true,
	})
	if err != nil {
		return err
	}
	params := botProfileParams(*bot, *profile)
	if *sessionID != "" {
		params["session_id"] = *sessionID
	}
	if *reason != "" {
		params["reason"] = *reason
	}
	return callSocketAndPrint(context.Background(), paths, "sessions.stop", auth, params, 10*time.Second)
}

func runSessionsNudge(args []string) error {
	fs := flag.NewFlagSet("comment sessions nudge", flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	bot := fs.String("bot", "", "bot name")
	profile := fs.String("profile", "", "profile handle")
	sessionID := fs.String("session-id", "", "session id")
	messageID := fs.String("message-id", "", "local message id")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	positionals := fs.Args()
	if *bot == "" && len(positionals) > 0 && !commentbus.LocalMessageIDRE.MatchString(positionals[0]) {
		*bot = positionals[0]
		positionals = positionals[1:]
	}
	if *messageID == "" && len(positionals) > 0 {
		*messageID = positionals[0]
		positionals = positionals[1:]
	}
	if *messageID == "" {
		return errors.New("sessions nudge requires a message id")
	}
	if len(positionals) > 0 {
		return errors.New("sessions nudge accepts at most bot and message id positionals")
	}
	paths, err := resolveCLIPaths(*home)
	if err != nil {
		return err
	}
	auth, err := socketAuth(context.Background(), paths, socketAuthOptions{
		Profile:      *profile,
		Bot:          *bot,
		AllowSession: true,
	})
	if err != nil {
		return err
	}
	params := botProfileParams(*bot, *profile)
	if *sessionID != "" {
		params["session_id"] = *sessionID
	}
	params["message_id"] = *messageID
	return callSocketAndPrint(context.Background(), paths, "sessions.nudge", auth, params, 10*time.Second)
}

type socketAuthOptions struct {
	Profile      string
	Bot          string
	BotletsHome  string
	NeedProfile  bool
	AllowSession bool
}

func socketAuth(ctx context.Context, paths commentbus.Paths, options socketAuthOptions) (*commentbus.SocketAuth, error) {
	if options.AllowSession {
		auth, botName, ok, err := sessionAuthFromEnv(paths)
		if err != nil {
			if !shouldIgnoreProfileOnlySessionEnv(options, err) {
				return nil, err
			}
		} else if ok {
			if options.Profile != "" && auth.Profile != nil && options.Profile != *auth.Profile {
				return nil, errors.New("selected profile does not match managed session")
			}
			if options.Bot != "" && options.Bot != botName {
				return nil, errors.New("selected bot does not match managed session")
			}
			return auth, nil
		}
	}
	profile := options.Profile
	if options.NeedProfile || profile != "" {
		resolved, err := resolveCLIProfile(ctx, paths, profile, options.Bot, options.BotletsHome)
		if err != nil {
			return nil, err
		}
		profile = resolved
	}
	return ownerAuth(paths, profile)
}

var errIncompleteManagedSessionEnv = errors.New("incomplete managed-session environment")

func shouldIgnoreProfileOnlySessionEnv(options socketAuthOptions, err error) bool {
	if !errors.Is(err, errIncompleteManagedSessionEnv) || options.Profile == "" || options.Bot != "" {
		return false
	}
	profile := os.Getenv("COMMENT_IO_PROFILE")
	return profile == options.Profile &&
		os.Getenv("COMMENT_IO_BOT_NAME") == "" &&
		os.Getenv("COMMENT_IO_SESSION_ID") == "" &&
		os.Getenv("COMMENT_IO_SESSION_GENERATION") == "" &&
		os.Getenv("COMMENT_IO_SESSION_CAP_FILE") == ""
}

func ownerAuth(paths commentbus.Paths, profile string) (*commentbus.SocketAuth, error) {
	file, err := commentbus.OpenPrivateFile(paths.Home, paths.OwnerCapability, "owner capability file")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	capability, err := commentbus.ReadCapabilityFromReader(file)
	if err != nil {
		return nil, fmt.Errorf("owner capability unavailable; run comment bus init first: %w", err)
	}
	if !commentbus.CapabilityTokenRE.MatchString(capability) {
		return nil, errors.New("invalid owner capability file")
	}
	auth := &commentbus.SocketAuth{Mode: "owner", Capability: capability}
	if profile != "" {
		auth.Profile = &profile
	}
	return auth, nil
}

func ownerOnlyAuth(paths commentbus.Paths, profile string) (*commentbus.SocketAuth, error) {
	if _, _, ok, err := sessionAuthFromEnv(paths); ok || err != nil {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("owner-only command is not allowed from a managed session")
	}
	return ownerAuth(paths, profile)
}

func sessionAuthFromEnv(paths commentbus.Paths) (*commentbus.SocketAuth, string, bool, error) {
	profile := os.Getenv("COMMENT_IO_PROFILE")
	botName := os.Getenv("COMMENT_IO_BOT_NAME")
	sessionID := os.Getenv("COMMENT_IO_SESSION_ID")
	generation := os.Getenv("COMMENT_IO_SESSION_GENERATION")
	capFile := os.Getenv("COMMENT_IO_SESSION_CAP_FILE")
	// A transient runtime (started by `comment run --runtime <bin>
	// --profile <handle>`) sets COMMENT_IO_PROFILE and COMMENT_IO_RUNTIME_RUN
	// but deliberately does NOT supply the bot/session capability env vars.
	// Treat only that exact profile-only shape as "not a managed session" so
	// callers fall through to owner auth scoped to the profile, without letting
	// a managed session opt out of least-privilege auth by setting the runtime
	// marker.
	if os.Getenv("COMMENT_IO_RUNTIME_RUN") != "" && profile != "" && botName == "" && sessionID == "" && generation == "" && capFile == "" {
		return nil, "", false, nil
	}
	if profile == "" && botName == "" && sessionID == "" && generation == "" && capFile == "" {
		return nil, "", false, nil
	}
	if profile == "" || botName == "" || sessionID == "" || generation == "" || capFile == "" {
		return nil, "", true, errIncompleteManagedSessionEnv
	}
	if !commentbus.ProfileRE.MatchString(profile) || !commentbus.BotNameRE.MatchString(botName) || !commentbus.LocalSessionIDRE.MatchString(sessionID) || !commentbus.LocalSessionGenerationIDRE.MatchString(generation) {
		return nil, "", true, errors.New("invalid managed-session environment")
	}
	expectedCapFile := filepath.Clean(filepath.Join(paths.Capabilities, profile, sessionID, generation+".cap"))
	if capFile != filepath.Clean(capFile) || capFile != expectedCapFile {
		return nil, "", true, errors.New("managed-session capability file does not match selected home")
	}
	file, err := commentbus.OpenPrivateFile(paths.Home, expectedCapFile, "managed-session capability file")
	if err != nil {
		return nil, "", true, err
	}
	defer file.Close()
	capability, err := commentbus.ReadCapabilityFromReader(file)
	if err != nil {
		return nil, "", true, fmt.Errorf("could not read session capability file: %w", err)
	}
	if !commentbus.CapabilityTokenRE.MatchString(capability) {
		return nil, "", true, errors.New("invalid managed-session capability file")
	}
	return &commentbus.SocketAuth{
		Mode:              "session",
		Capability:        capability,
		Profile:           &profile,
		SessionID:         &sessionID,
		SessionGeneration: &generation,
	}, botName, true, nil
}

func resolveCLIProfile(ctx context.Context, paths commentbus.Paths, profile string, bot string, botletsHome string) (string, error) {
	if profile == "" && bot == "" {
		return "", errors.New("this command requires --bot or --profile outside a managed session")
	}
	if bot == "" {
		return profile, nil
	}
	botletsHome = persistedCLIBotletsHome(paths, botletsHome)
	state, loadErrors := commentbus.LoadProfileState(ctx, commentbus.ProfileLoadOptions{
		Paths:       paths,
		BotletsHome: botletsHome,
	})
	entry, ok, selectErr := selectBotletsEntryBySelector(state.BotRegistry, bot)
	if selectErr != nil {
		return "", selectErr
	}
	if !ok {
		if len(loadErrors) > 0 {
			return "", fmt.Errorf("could not resolve bot %q: %s", bot, loadErrors[0].Message)
		}
		return "", fmt.Errorf("bot %q is not loaded", bot)
	}
	if profile != "" && !entry.MatchesProfile(profile) {
		return "", errors.New("bot and profile do not match")
	}
	return entry.Handle, nil
}

func persistedCLIBotletsHome(paths commentbus.Paths, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if config, ok, err := commentbus.ReadBusConfig(paths); err == nil && ok && config.BotletsHome != "" {
		return config.BotletsHome
	}
	return ""
}

// resolveDaemonBotletsHome resolves the Botlets home for the daemon's
// background workers (agent enrollment, team resync) using the SAME order
// commentbus.StartDaemon applies at boot: the explicit --botlets-home value
// (hint), then the persisted bus config, then the BOTLETS_HOME environment
// variable, then the default home. Workers must match the daemon, or an
// env/flag-selected home would have the daemon loading profiles from one home
// while its workers install into another.
func resolveDaemonBotletsHome(paths commentbus.Paths, hint string) (string, error) {
	return commentbus.ResolveBotletsHome(firstNonEmpty(hint, persistedCLIBotletsHome(paths, ""), os.Getenv("BOTLETS_HOME")))
}

func callSocketAndPrint(ctx context.Context, paths commentbus.Paths, op string, auth *commentbus.SocketAuth, params map[string]any, responseTimeout time.Duration) error {
	response, err := callSocket(ctx, paths, op, auth, params, responseTimeout)
	if err != nil {
		return err
	}
	if err := printJSON(response); err != nil {
		return err
	}
	return socketResponseError(response)
}

func callSocketResultAndPrint(ctx context.Context, paths commentbus.Paths, op string, auth *commentbus.SocketAuth, params map[string]any, responseTimeout time.Duration) error {
	response, err := callSocket(ctx, paths, op, auth, params, responseTimeout)
	if err != nil {
		return err
	}
	if !response.OK {
		if err := printJSON(response); err != nil {
			return err
		}
		return socketResponseError(response)
	}
	return printJSON(response.Result)
}

func interspersedFlagArgs(args []string, valueFlags map[string]struct{}) ([]string, error) {
	flags := make([]string, 0, len(args))
	positionals := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positionals = append(positionals, args[i+1:]...)
			break
		}
		name, inlineValue, ok := flagArgName(arg)
		if !ok {
			positionals = append(positionals, arg)
			continue
		}
		if _, valueFlag := valueFlags[name]; valueFlag {
			flags = append(flags, arg)
			if !inlineValue && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			} else if !inlineValue {
				return nil, fmt.Errorf("flag needs an argument: -%s", name)
			}
			continue
		}
		flags = append(flags, arg)
	}
	return append(flags, positionals...), nil
}

func flagArgName(arg string) (string, bool, bool) {
	if !strings.HasPrefix(arg, "-") || arg == "-" {
		return "", false, false
	}
	trimmed := strings.TrimLeft(arg, "-")
	if trimmed == "" {
		return "", false, false
	}
	name, _, hasInlineValue := strings.Cut(trimmed, "=")
	return name, hasInlineValue, name != ""
}

func callSocket(ctx context.Context, paths commentbus.Paths, op string, auth *commentbus.SocketAuth, params map[string]any, responseTimeout time.Duration) (commentbus.SocketResponse, error) {
	id, err := commentbus.GenerateSocketRequestID()
	if err != nil {
		return commentbus.SocketResponse{}, err
	}
	if params == nil {
		params = map[string]any{}
	}
	return commentbus.CallSocket(ctx, paths, commentbus.SocketRequest{
		ID:     id,
		Op:     op,
		Auth:   auth,
		Params: params,
	}, responseTimeout)
}

func socketResponseError(response commentbus.SocketResponse) error {
	if !response.OK {
		if response.Error == nil {
			return cliSocketError{Message: "socket operation failed"}
		}
		return cliSocketError{Code: response.Error.Code, Message: response.Error.Message}
	}
	return nil
}

func isDaemonUnavailableError(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)
}

// isConnectionDroppedError reports whether a socket call failed because an
// already-open connection was closed/reset mid-request — e.g. the daemon
// restarted while a `messages.wait` was blocked. These surface as EOF/ECONNRESET/
// EPIPE rather than the not-found/refused errors isDaemonUnavailableError covers.
func isConnectionDroppedError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "connection reset") || strings.Contains(msg, "broken pipe") || strings.Contains(msg, "EOF")
}

func botProfileParams(bot string, profile string) map[string]any {
	params := map[string]any{}
	if bot != "" {
		params["bot"] = bot
	}
	if profile != "" {
		params["profile"] = profile
	}
	return params
}

func timeoutMillis(durationValue string, explicitMillis int) (int, error) {
	if explicitMillis < 0 {
		return 0, errors.New("timeout must be non-negative")
	}
	if explicitMillis > 0 {
		return explicitMillis, nil
	}
	if durationValue == "" {
		return 0, nil
	}
	if millis, err := strconv.Atoi(durationValue); err == nil {
		if millis < 0 {
			return 0, errors.New("timeout must be non-negative")
		}
		return millis, nil
	}
	duration, err := time.ParseDuration(durationValue)
	if err != nil {
		return 0, err
	}
	if duration < 0 {
		return 0, errors.New("timeout must be non-negative")
	}
	return int(duration / time.Millisecond), nil
}

func readMessageBody(body string, bodyFile string, positionals []string) (string, error) {
	if body != "" && bodyFile != "" {
		return "", errors.New("use --body or --body-file, not both")
	}
	if bodyFile != "" {
		var reader io.Reader
		var file *os.File
		if bodyFile == "-" {
			reader = os.Stdin
		} else {
			var err error
			file, err = os.Open(bodyFile)
			if err != nil {
				return "", err
			}
			defer file.Close()
			reader = file
		}
		data, err := io.ReadAll(io.LimitReader(reader, maxCLIMessageBodyBytes+1))
		if err != nil {
			return "", err
		}
		if len(data) > maxCLIMessageBodyBytes {
			return "", errors.New("message body is too large")
		}
		return string(data), nil
	}
	if body != "" {
		return body, nil
	}
	if len(positionals) > 0 {
		return strings.Join(positionals, " "), nil
	}
	return "", errors.New("messages send requires --body, --body-file, or a positional body")
}

func parseRefs(values []string) (map[string]any, error) {
	refs := map[string]any{}
	for _, value := range values {
		key, raw, ok := strings.Cut(value, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("invalid --ref %q", value)
		}
		refs[key] = raw
	}
	return refs, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
