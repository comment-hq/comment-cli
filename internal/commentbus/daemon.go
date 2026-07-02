package commentbus

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	maxSocketRequestBytes               = 1 << 20
	socketReadTimeout                   = 5 * time.Second
	socketWriteTimeout                  = 5 * time.Second
	slowSocketRequestDuration           = time.Second
	slowSocketLockWaitDuration          = 250 * time.Millisecond
	slowCloudMutationDuration           = time.Second
	startupProbeTimeout                 = 250 * time.Millisecond
	sessionRuntimeStartupTimeout        = 2 * time.Second
	managedSessionRuntimeStartupTimeout = time.Minute
	sessionRuntimeStartupPoll           = 50 * time.Millisecond
	defaultTmuxSubmitDelay              = 750 * time.Millisecond
	maxTmuxSubmitDelay                  = 5 * time.Second
	claudeTrustPromptMaxWait            = 6 * time.Second
	claudeTrustPromptPoll               = 150 * time.Millisecond
	cloudNotificationTransportSlack     = 100 * time.Millisecond
	cloudNotificationLeaseProbeTimeout  = time.Second
	cloudNotificationPollIdleDelay      = 250 * time.Millisecond
	cloudNotificationPollErrorDelay     = 5 * time.Second
	automaticNudgeInitialBackoff        = 30 * time.Second
	automaticNudgeMaxBackoff            = 5 * time.Minute
	automaticNudgeMaxAttempts           = 3
	automaticNudgeStateLimit            = 256
	dispatchReadySummaryLimit           = 50
	// cloudNotificationAuthRevokedDelay is the back-off when the wake socket
	// closes with WS_AGENT_AUTH_REVOKED_CLOSE_CODE (4431). Reconnecting with
	// the same credentials will be rejected; the operator must re-issue agent
	// credentials. The long delay avoids hammering the server while still
	// allowing automatic recovery if credentials are rotated in-place.
	cloudNotificationAuthRevokedDelay = 5 * time.Minute
)

var cloudNotificationWakeReconcileInterval = 15 * time.Minute

var (
	managedSessionDailyResetPollInterval = time.Minute
	managedSessionDailyResetGracePeriod  = 10 * time.Minute
)

const defaultManagedSessionTimezone = "UTC"

type DaemonOptions struct {
	Paths                     Paths
	Version                   string
	BotletsHome               string
	DefaultBaseURL            string
	ExpectedUID               *uint32
	PeerCredentialFunc        func(*net.UnixConn) (PeerCredential, error)
	NotificationClient        NotificationClient
	EnableNotificationPollers bool
	Tmux                      TmuxController
	TmuxBinary                string
	Bmux                      TmuxController
	BmuxBinary                string
	TmuxPollInterval          time.Duration
	TmuxSubmitDelay           time.Duration
	CommentExecutable         func() (string, error)
	LogWriter                 io.Writer
	Now                       func() time.Time
	// AllowNonLoopbackTCP permits binding the opt-in bus TCP listener
	// (Paths.BusTCPAddr) to a non-loopback address. Default false: a non-loopback
	// bind is refused so the cap-token-only TCP path can't be accidentally exposed
	// to a LAN. Containers that must bind 0.0.0.0 for Docker port publishing set
	// this explicitly and publish the port to host loopback only.
	AllowNonLoopbackTCP bool
	// TCPListenAddr, when non-empty, is the address the daemon binds its opt-in
	// TCP control listener to (e.g. "0.0.0.0:7700" inside a container). This is
	// the DAEMON's bind address, deliberately separate from Paths.BusTCPAddr (the
	// CLIENT's dial address) so a host that exports the dial address to reach a
	// caged daemon doesn't make a native `comment bus run` try to bind it.
	TCPListenAddr string
	// DisableUnixListener skips the Unix-socket listener entirely (TCP-only).
	// Requires TCPListenAddr to be set. Used in containers whose state dir is a
	// bind mount that can't chmod a socket file (macOS virtiofs).
	DisableUnixListener bool
	// MentionAutoStart, when non-nil, launches an already-installed agent's
	// runtime detached (the same path the web "Start your agent" button uses)
	// when a doc @mention arrives for a bot whose "Responds to @mentions" flag is
	// on and nothing is running. nil disables mention auto-launch. It lives in
	// package main (launchAgentRuntimeDetached), so it is injected here rather
	// than imported by package commentbus.
	MentionAutoStart func(ctx context.Context, paths Paths, handle string) error
	// SocketWatchdogInterval is how often the running daemon re-checks that its
	// Unix listening socket still exists and is the file it bound, re-binding it
	// if it vanished. 0 uses defaultSocketWatchdogInterval; a negative value
	// disables the watchdog (tests use this to demonstrate the pre-fix wedge); a
	// positive value sets a fast interval to exercise recovery. Ignored for a
	// TCP-only daemon (no Unix socket to watch).
	SocketWatchdogInterval time.Duration
	// StartupInitTimeout bounds the store/capability init that runs after the
	// singleton lock is acquired but before the socket is bound. <=0 uses
	// defaultStartupInitTimeout. On timeout StartDaemon fails and releases the
	// lock rather than wedging alive-but-socketless.
	StartupInitTimeout time.Duration
	// SocketRebindTimeout bounds a watchdog re-bind (net.ListenUnix/chmod/lstat,
	// none context-cancellable). <=0 uses defaultSocketRebindTimeout. It caps how
	// long Close can wait to join a watchdog stuck re-binding on a wedged
	// filesystem. Tests set a small value.
	SocketRebindTimeout time.Duration
}

type NotificationPollerStatus struct {
	Profile          string  `json:"profile"`
	BotName          string  `json:"bot_name,omitempty"`
	State            string  `json:"state"`
	StartedAt        string  `json:"started_at"`
	StoppedAt        *string `json:"stopped_at,omitempty"`
	LastPollAt       *string `json:"last_poll_at,omitempty"`
	LastLeaseAt      *string `json:"last_lease_at,omitempty"`
	LastErrorAt      *string `json:"last_error_at,omitempty"`
	LastErrorCode    *string `json:"last_error_code,omitempty"`
	LastErrorMessage *string `json:"last_error_message,omitempty"`
}

type notificationPoller struct {
	profile string
	botName string
	cancel  context.CancelFunc
	done    chan struct{}
	status  NotificationPollerStatus
}

type Daemon struct {
	paths                        Paths
	version                      string
	expectedUID                  uint32
	pid                          int
	startedAt                    time.Time
	listener                     *net.UnixListener
	tcpListener                  net.Listener
	lockFile                     *os.File
	store                        *Store
	peerCredentialFunc           func(*net.UnixConn) (PeerCredential, error)
	notificationClient           NotificationClient
	profileMu                    sync.RWMutex
	profileState                 ProfileState
	profileErrors                []ProfileReloadError
	busMu                        sync.Mutex
	sessionMu                    sync.Mutex
	cloudWaitMu                  sync.Mutex
	cloudWaitLocks               map[string]chan struct{}
	notificationPollersEnabled   bool
	notificationPollersActive    bool
	notificationPollerMu         sync.Mutex
	notificationPollers          map[string]*notificationPoller
	transientRuntimeMu           sync.Mutex
	transientRuntimes            map[string]*transientRuntime
	transientRuntimeMainProfiles map[string]string
	transientRuntimeMainIDs      map[string]string
	// listenEstablishMu serializes the "who listens for this handle" decision
	// across the two ways a listener is established — an impromptu listen.claim
	// and a `comment run` main runtime — so they cannot both commit for the same
	// handle in an interleaved check-then-act and double-deliver. It is the
	// outermost of these locks: held only around fast in-memory checks/commits
	// (never across launch I/O), so it introduces no lock-ordering inversion with
	// transientRuntimeMu / listeners.mu / profileMu (acquired under it, never the
	// reverse). establishingMain counts main runtimes mid-launch per handle so a
	// concurrent listen.claim sees an in-flight `comment run` before its
	// reservation lands.
	listenEstablishMu     sync.Mutex
	establishingMain      map[string]int
	tmux                  TmuxController
	bmuxBinary            string
	bmux                  TmuxController
	tmuxPollInterval      time.Duration
	tmuxPollSlowInterval  time.Duration
	tmuxSubmitDelay       time.Duration
	tmuxNudgeLocks        SessionNudgeLocks
	listeners             *listenerRegistry
	writeSessionRecord    func(Paths, SessionRecord) error
	commentExecutablePath func() (string, error)
	now                   func() time.Time
	startupDispatchErrors []MessageDispatchError
	botletsHome           string
	defaultBaseURL        string
	logger                *structuredLogger
	// socketMu guards listener and socketFileInfo, which the socket watchdog
	// swaps under it when it re-binds a vanished socket, and which Close snapshots
	// under it so teardown closes/removes the current listener and socket file.
	socketMu               sync.Mutex
	socketWatchdogInterval time.Duration
	socketRebindTimeout    time.Duration
	// socketWatchdogDone is closed when the socket-watchdog goroutine exits, so
	// Close can join it (like the transient-runtime / notification-poller
	// teardown) before tearing down store/logger/socket — an in-flight re-bind
	// must not touch that state after Close declares cleanup done. nil when no
	// watchdog was started (TCP-only daemon).
	socketWatchdogDone chan struct{}
	socketFileInfo     os.FileInfo
	pidFileInfo        os.FileInfo
	// baseCtx is the daemon's long-lived context (the daemonCtx StartDaemon derives
	// from the caller's ctx). It outlives any per-request/per-ingest context and is
	// cancelled only when the daemon shuts down (shutdownCancel). Detached work that
	// must not be torn down with the request that triggered it — notably the
	// mention-driven auto-launch goroutine — derives from this, NOT from the
	// short-lived per-ingest context the owner WAIT/REWAKE path cancels the instant
	// ingest returns. nil for bare &Daemon{} test fixtures; callers fall back to
	// context.Background() when it's unset.
	baseCtx        context.Context
	shutdownCancel context.CancelFunc
	closeOnce      sync.Once
	closeErr       error
	// mentionAutoStart launches an already-installed agent's runtime detached
	// (the same path the web "Start your agent" button uses) when a doc @mention
	// arrives for a bot whose "Responds to @mentions" flag is on and nothing is
	// running. nil disables mention auto-launch entirely (e.g. in unit tests that
	// never wire it). Injected from cmd/comment because the launch helper lives
	// in package main; package commentbus cannot import it directly.
	mentionAutoStart func(ctx context.Context, paths Paths, handle string) error
	// mentionAutoStartMu guards mentionAutoStartState. Held only for the small
	// check/update of the per-recipient backoff record — never across the launch.
	mentionAutoStartMu sync.Mutex
	// mentionAutoStartState carries per-recipient cooldown + failure backoff +
	// in-flight tracking, keyed by recipient handle, so a flurry of mentions or a
	// bot that fails to launch cannot relaunch-loop.
	mentionAutoStartState map[string]*mentionAutoStartRecord
}

type SocketResponse struct {
	ID     string       `json:"id"`
	OK     bool         `json:"ok"`
	Result any          `json:"result,omitempty"`
	Error  *SocketError `json:"error,omitempty"`
}

type SocketError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func StartDaemon(ctx context.Context, options DaemonOptions) (*Daemon, error) {
	paths := options.Paths
	if paths.Home == "" {
		var err error
		paths, err = ResolvePaths("")
		if err != nil {
			return nil, err
		}
	}
	version := options.Version
	if version == "" {
		version = "dev"
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	startupBotletsHome := options.BotletsHome
	if startupBotletsHome == "" {
		if config, ok, err := ReadBusConfig(paths); err == nil && ok {
			startupBotletsHome = config.BotletsHome
		}
	}
	if startupBotletsHome == "" {
		startupBotletsHome = os.Getenv("BOTLETS_HOME")
	}
	expectedUID := uint32(os.Geteuid())
	if options.ExpectedUID != nil {
		expectedUID = *options.ExpectedUID
	}
	peerCredentialFunc := options.PeerCredentialFunc
	if peerCredentialFunc == nil {
		peerCredentialFunc = PeerCredentialFor
	}
	tmux := options.Tmux
	if tmux == nil {
		tmux = ExecTmuxController{Binary: ResolveConfiguredTmuxBinary(options.TmuxBinary)}
	}
	bmuxBinary := ResolveConfiguredBmuxBinary(options.BmuxBinary)
	bmux := options.Bmux
	if bmux == nil {
		bmux = NewExecBmuxController(paths, bmuxBinary)
	}

	// Acquire the cross-transport singleton lock — and run the Unix-socket guard —
	// BEFORE opening/migrating the store or touching capability state, so a second
	// daemon on the same COMMENT_IO_HOME can't initialize the SQLite store or
	// rotate capabilities before failing to acquire the lock. EnsureBaseDirs first
	// so the home dir exists for the lock file and socket probe (OpenStore also
	// calls it; it's idempotent).
	if err := EnsureBaseDirs(paths); err != nil {
		return nil, err
	}
	// Unix-socket live-daemon guard first, so a unix double-start keeps its
	// socket-specific refusal; it only probes/cleans a stale socket (no chmod, no
	// shared-state mutation — safe on virtiofs and before the lock).
	if err := prepareSocketPath(paths.Socket, paths.PID); err != nil {
		return nil, err
	}
	lockFile, err := acquireDaemonLock(paths)
	if err != nil {
		return nil, err
	}
	// Release the lock on any failure before the daemon struct takes ownership;
	// once owned, daemon.Close() releases it.
	lockOwned := false
	defer func() {
		if !lockOwned {
			_ = lockFile.Close()
		}
	}()

	// Bound the post-lock / pre-listen init (store open + capability ensure) with
	// a startup timeout. These run while the singleton lock is held but before the
	// socket is bound, so a hang here (e.g. a wedged bind-mount filesystem) would
	// otherwise strand the lock with no socket — a live-but-unreachable daemon no
	// client can dial and no fresh StartDaemon can replace. On timeout we return
	// an error and the deferred lockFile.Close() (lockOwned still false) releases
	// the lock for a clean restart.
	startupTimeout := options.StartupInitTimeout
	if startupTimeout <= 0 {
		startupTimeout = defaultStartupInitTimeout
	}
	var store *Store
	var logger *structuredLogger
	if err := runWithStartupTimeout(startupTimeout, func() error {
		runStartupInitProbe()
		s, err := OpenStore(ctx, paths)
		if err != nil {
			return err
		}
		lg, err := newStructuredLogger(paths, options.LogWriter, now)
		if err != nil {
			_ = s.Close()
			return err
		}
		if _, err := EnsureOwnerCapability(paths); err != nil {
			_ = lg.close()
			_ = s.Close()
			return err
		}
		store = s
		logger = lg
		return nil
	}, func() {
		// Late success: the init finished only after the timeout already made
		// StartDaemon return, so nothing will ever use these handles. Close them so
		// a merely-slow (not permanently wedged) init doesn't leak a SQLite/log fd.
		// Safe to read store/logger here: the done-channel receive in
		// runWithStartupTimeout happens-after fn's assignments to them.
		if store != nil {
			_ = store.Close()
		}
		if logger != nil {
			_ = logger.close()
		}
	}); err != nil {
		return nil, err
	}
	// The Unix listener can be disabled (TCP-only). This is required when the
	// state dir is a bind mount whose filesystem can't chmod a socket file —
	// e.g. virtiofs on Docker Desktop / Colima for macOS, where chmod on the
	// socket returns EINVAL — and where the host reaches the daemon over TCP
	// anyway because a bind-mounted Unix socket isn't connectable from the host.
	var listener *net.UnixListener
	var socketFileInfo os.FileInfo
	if !options.DisableUnixListener {
		l, info, err := bindUnixSocket(paths.Socket)
		if err != nil {
			_ = logger.close()
			_ = store.Close()
			return nil, err
		}
		listener = l
		socketFileInfo = info
	}
	if err := WritePrivateFileAtomic(paths.PID, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		_ = logger.close()
		if listener != nil {
			_ = listener.Close()
			_ = os.Remove(paths.Socket)
		}
		_ = store.Close()
		return nil, err
	}
	pidFileInfo, err := os.Lstat(paths.PID)
	if err != nil {
		_ = logger.close()
		if listener != nil {
			_ = listener.Close()
			_ = os.Remove(paths.Socket)
		}
		_ = store.Close()
		_ = os.Remove(paths.PID)
		return nil, err
	}

	daemonCtx, shutdownCancel := context.WithCancel(ctx)
	daemon := &Daemon{
		paths:                        paths,
		version:                      version,
		expectedUID:                  expectedUID,
		pid:                          os.Getpid(),
		startedAt:                    now().UTC(),
		listener:                     listener,
		lockFile:                     lockFile,
		store:                        store,
		peerCredentialFunc:           peerCredentialFunc,
		notificationClient:           options.NotificationClient,
		profileState:                 EmptyProfileState(""),
		cloudWaitLocks:               map[string]chan struct{}{},
		notificationPollersEnabled:   options.EnableNotificationPollers,
		notificationPollers:          map[string]*notificationPoller{},
		transientRuntimes:            map[string]*transientRuntime{},
		transientRuntimeMainProfiles: map[string]string{},
		establishingMain:             map[string]int{},
		transientRuntimeMainIDs:      map[string]string{},
		listeners:                    newListenerRegistry(),
		tmux:                         tmux,
		bmuxBinary:                   bmuxBinary,
		bmux:                         bmux,
		tmuxPollInterval:             normalizeTmuxPollInterval(options.TmuxPollInterval),
		tmuxPollSlowInterval:         normalizeTmuxPollSlowInterval(options.TmuxPollInterval),
		tmuxSubmitDelay:              normalizeTmuxSubmitDelay(options.TmuxSubmitDelay),
		commentExecutablePath:        options.CommentExecutable,
		now:                          now,
		botletsHome:                  startupBotletsHome,
		defaultBaseURL:               options.DefaultBaseURL,
		logger:                       logger,
		socketFileInfo:               socketFileInfo,
		socketWatchdogInterval:       options.SocketWatchdogInterval,
		socketRebindTimeout:          options.SocketRebindTimeout,
		pidFileInfo:                  pidFileInfo,
		baseCtx:                      daemonCtx,
		shutdownCancel:               shutdownCancel,
		mentionAutoStart:             options.MentionAutoStart,
		mentionAutoStartState:        map[string]*mentionAutoStartRecord{},
	}
	lockOwned = true // daemon.Close() now owns releasing the lock
	if tcpAddr := options.TCPListenAddr; tcpAddr != "" {
		if !isLoopbackTCPAddr(tcpAddr) && !options.AllowNonLoopbackTCP {
			_ = daemon.Close()
			return nil, fmt.Errorf("refusing to bind bus TCP listener to non-loopback address %q without AllowNonLoopbackTCP (COMMENT_IO_BUS_TCP_ALLOW_NONLOOPBACK=1)", tcpAddr)
		}
		tcpListener, err := net.Listen("tcp", tcpAddr)
		if err != nil {
			_ = daemon.Close()
			return nil, err
		}
		daemon.tcpListener = tcpListener
	}
	if daemon.listener == nil && daemon.tcpListener == nil {
		_ = daemon.Close()
		return nil, errors.New("no bus listener configured: enable the Unix socket or set COMMENT_IO_BUS_TCP_LISTEN")
	}
	_ = daemon.reloadProfiles(ctx, startupBotletsHome)
	if err := daemon.backfillStoreBotIdentityColumns(ctx); err != nil {
		_ = daemon.Close()
		return nil, err
	}
	startupActions, err := daemon.reconcileStartup(ctx)
	if err != nil {
		_ = daemon.Close()
		return nil, err
	}
	daemon.logger.info("bus.startup_repair", map[string]any{"actions": len(startupActions)})
	if err := daemon.reconcileTransientRuntimes(ctx); err != nil {
		_ = daemon.Close()
		return nil, err
	}
	daemon.startupDispatchErrors = daemon.dispatchReadyQueueHeads(ctx)
	daemon.activateNotificationPollers()
	if daemon.listener != nil {
		go daemon.serveListener(daemon.listener)
		// Watch the Unix socket for disappearance and re-bind it in place, so the
		// daemon can't wedge alive-but-socketless (lock held, no socket) — a state
		// that was previously unrecoverable without killing the process. Close
		// joins socketWatchdogDone so an in-flight re-bind finishes before teardown.
		daemon.socketWatchdogDone = make(chan struct{})
		go daemon.runSocketWatchdog(daemonCtx)
	}
	if daemon.tcpListener != nil {
		go daemon.serveListener(daemon.tcpListener)
	}
	go daemon.runManagedSessionDailyResetLoop(daemonCtx)
	go func() {
		<-daemonCtx.Done()
		_ = daemon.Close()
	}()
	return daemon, nil
}

func (d *Daemon) backfillStoreBotIdentityColumns(ctx context.Context) error {
	d.profileMu.RLock()
	bots := make(map[string]BotRegistryEntry, len(d.profileState.BotRegistry))
	for name, bot := range d.profileState.BotRegistry {
		bots[name] = bot
	}
	d.profileMu.RUnlock()
	return d.store.BackfillBotIdentityColumns(ctx, bots)
}

func (d *Daemon) Close() error {
	d.closeOnce.Do(func() {
		if d.shutdownCancel != nil {
			d.shutdownCancel()
		}
		// Join the socket watchdog before touching store/logger/socket below: an
		// in-flight re-bind (blocked in bindUnixSocket) must not log to a closed
		// logger or install a socket after teardown. Mirrors the join-before-
		// teardown pattern used for transient runtimes and notification pollers.
		if d.socketWatchdogDone != nil {
			<-d.socketWatchdogDone
		}
		d.stopAllTransientRuntimes()
		d.stopAllNotificationPollers()
		// Snapshot the Unix listener + socket identity under socketMu: the watchdog
		// may swap them concurrently when it re-binds. shutdownCancel above has
		// already fired, so any in-flight watchdog re-bind sees ctx cancelled under
		// this same lock and won't install a listener after we snapshot.
		d.socketMu.Lock()
		unixListener := d.listener
		socketFileInfo := d.socketFileInfo
		d.socketMu.Unlock()
		if unixListener != nil {
			d.closeErr = errors.Join(d.closeErr, unixListener.Close())
		}
		if d.tcpListener != nil {
			d.closeErr = errors.Join(d.closeErr, d.tcpListener.Close())
		}
		if d.store != nil {
			d.closeErr = errors.Join(d.closeErr, d.store.Close())
		}
		if d.logger != nil {
			d.closeErr = errors.Join(d.closeErr, d.logger.close())
		}
		d.closeErr = errors.Join(d.closeErr, removeIfSameFile(d.paths.Socket, socketFileInfo))
		d.closeErr = errors.Join(d.closeErr, removeIfSameFile(d.paths.PID, d.pidFileInfo))
		// Release the singleton lock LAST — after the store is closed and the
		// socket/PID are removed — so a concurrent restart can't acquire the lock
		// and open the same home while this daemon's cleanup is still in flight.
		if d.lockFile != nil {
			d.closeErr = errors.Join(d.closeErr, d.lockFile.Close()) // closing the fd releases the flock
		}
	})
	return d.closeErr
}

func (d *Daemon) activateNotificationPollers() {
	if !d.notificationPollersEnabled || d.notificationClient == nil {
		return
	}
	d.notificationPollerMu.Lock()
	d.notificationPollersActive = true
	d.notificationPollerMu.Unlock()
	d.syncNotificationPollersFromProfileState()
}

func (d *Daemon) syncNotificationPollersFromProfileState() {
	if !d.notificationPollersEnabled || d.notificationClient == nil {
		return
	}
	d.profileMu.RLock()
	targets := managedNotificationPollerTargets(d.profileState)
	d.profileMu.RUnlock()
	d.syncNotificationPollers(targets)
}

func managedNotificationPollerTargets(state ProfileState) map[string]string {
	targets := map[string]string{}
	for botName, bot := range state.BotRegistry {
		if !bot.ManagedSession.Enabled || bot.Handle == "" {
			continue
		}
		if _, ok := state.AgentProfiles[bot.Handle]; !ok {
			continue
		}
		targets[bot.Handle] = botName
	}
	return targets
}

func (d *Daemon) syncNotificationPollers(targets map[string]string) {
	d.notificationPollerMu.Lock()
	if !d.notificationPollersActive {
		d.notificationPollerMu.Unlock()
		return
	}
	var toStop []*notificationPoller
	for profile, poller := range d.notificationPollers {
		botName, ok := targets[profile]
		if !ok || poller.botName != botName {
			toStop = append(toStop, poller)
			delete(d.notificationPollers, profile)
		}
	}
	for profile, botName := range targets {
		if _, ok := d.notificationPollers[profile]; ok {
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		now := busTime(time.Now().UTC())
		poller := &notificationPoller{
			profile: profile,
			botName: botName,
			cancel:  cancel,
			done:    make(chan struct{}),
			status: NotificationPollerStatus{
				Profile:   profile,
				BotName:   botName,
				State:     "starting",
				StartedAt: now,
			},
		}
		d.notificationPollers[profile] = poller
		go d.runNotificationPoller(ctx, poller)
	}
	d.notificationPollerMu.Unlock()

	for _, poller := range toStop {
		poller.cancel()
		<-poller.done
	}
}

func (d *Daemon) stopAllNotificationPollers() {
	d.notificationPollerMu.Lock()
	d.notificationPollersActive = false
	pollers := make([]*notificationPoller, 0, len(d.notificationPollers))
	for profile, poller := range d.notificationPollers {
		pollers = append(pollers, poller)
		delete(d.notificationPollers, profile)
	}
	d.notificationPollerMu.Unlock()
	for _, poller := range pollers {
		poller.cancel()
		<-poller.done
	}
}

func (d *Daemon) runNotificationPoller(ctx context.Context, poller *notificationPoller) {
	defer close(poller.done)
	d.updateNotificationPollerStatus(poller, func(status *NotificationPollerStatus) {
		status.State = "polling"
	})
	wakeClient, useWakeHints := d.notificationClient.(NotificationWakeClient)
	recordPollerError := func(err *SocketError) {
		now := busTime(time.Now().UTC())
		d.updateNotificationPollerStatus(poller, func(status *NotificationPollerStatus) {
			status.State = "error"
			status.LastErrorAt = &now
			status.LastErrorCode = &err.Code
			status.LastErrorMessage = &err.Message
		})
		d.logger.warn("notification.poller_error", map[string]any{
			"profile":   poller.profile,
			"code":      err.Code,
			"retryable": err.Retryable,
		})
	}
	recordPollerLease := func() {
		now := busTime(time.Now().UTC())
		d.updateNotificationPollerStatus(poller, func(status *NotificationPollerStatus) {
			status.State = "polling"
			status.LastLeaseAt = &now
			status.LastErrorAt = nil
			status.LastErrorCode = nil
			status.LastErrorMessage = nil
		})
	}
	for {
		if ctx.Err() != nil {
			d.updateNotificationPollerStatus(poller, func(status *NotificationPollerStatus) {
				status.State = "stopped"
				stoppedAt := busTime(time.Now().UTC())
				status.StoppedAt = &stoppedAt
			})
			return
		}
		pollAt := busTime(time.Now().UTC())
		d.updateNotificationPollerStatus(poller, func(status *NotificationPollerStatus) {
			status.State = "polling"
			status.LastPollAt = &pollAt
		})
		acquired, ingestErr := d.ingestCloudNotification(ctx, poller.profile, poller.botName, true, 0, nil)
		if ingestErr != nil {
			recordPollerError(ingestErr)
			if !sleepWithContext(ctx, cloudNotificationPollErrorDelay) {
				continue
			}
			continue
		}
		if acquired {
			recordPollerLease()
		}
		messageID, dispatchErr := d.dispatchReadyQueueHeadWithContext(ctx, poller.profile, poller.botName)
		if dispatchErr != nil {
			now := busTime(time.Now().UTC())
			d.updateNotificationPollerStatus(poller, func(status *NotificationPollerStatus) {
				status.State = "dispatch_error"
				status.LastErrorAt = &now
				status.LastErrorCode = &dispatchErr.Code
				status.LastErrorMessage = &dispatchErr.Message
			})
			d.logger.warn("notification.poller_dispatch_failed", map[string]any{
				"profile":    poller.profile,
				"bot":        poller.botName,
				"message_id": messageID,
				"code":       dispatchErr.Code,
				"retryable":  dispatchErr.Retryable,
			})
			if !sleepWithContext(ctx, cloudNotificationPollErrorDelay) {
				continue
			}
			continue
		}
		if acquired {
			continue
		}
		if !useWakeHints {
			if !sleepWithContext(ctx, cloudNotificationPollIdleDelay) {
				continue
			}
			continue
		}
		profileConfig, _, ok := d.cloudNotificationTarget(poller.profile, poller.botName, true)
		if !ok {
			if !sleepWithContext(ctx, cloudNotificationPollIdleDelay) {
				continue
			}
			continue
		}
		d.updateNotificationPollerStatus(poller, func(status *NotificationPollerStatus) {
			status.State = "waiting_wake"
		})
		wakeTimeout := cloudNotificationWakeReconcileInterval
		nextNudgeAt, nextNudgeErr := d.nextReadyCloudAutomaticNudgeDeadline(poller.profile, poller.botName, time.Now().UTC())
		if nextNudgeErr != nil {
			recordPollerError(nextNudgeErr)
			if !sleepWithContext(ctx, cloudNotificationPollErrorDelay) {
				continue
			}
			continue
		}
		if nextNudgeAt != nil {
			untilNextNudge := time.Until(*nextNudgeAt)
			if untilNextNudge <= 0 {
				continue
			}
			if wakeTimeout <= 0 || untilNextNudge < wakeTimeout {
				wakeTimeout = untilNextNudge
			}
		}
		wakeCtx := ctx
		var cancelWake context.CancelFunc
		if wakeTimeout > 0 {
			wakeCtx, cancelWake = context.WithTimeout(ctx, wakeTimeout)
		}
		wake, wakeErr := wakeClient.WaitNotificationWake(wakeCtx, profileConfig)
		wakeTimedOut := wakeCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil
		if cancelWake != nil {
			cancelWake()
		}
		if ctx.Err() != nil {
			continue
		}
		if wakeTimedOut {
			d.logger.info("notification.poller_wake_reconcile", map[string]any{
				"profile": poller.profile,
			})
			continue
		}
		if wakeErr != nil {
			if errors.Is(wakeErr, ErrAgentAuthRevoked) {
				d.logger.warn("notification.poller_auth_revoked", map[string]any{
					"profile":     poller.profile,
					"remediation": "re-issue agent credentials (e.g. `comment register` or rotate the agent secret)",
				})
				recordPollerError(socketError("AGENT_AUTH_REVOKED", "agent credentials were revoked; re-issue credentials to resume", false))
				d.updateNotificationPollerStatus(poller, func(status *NotificationPollerStatus) {
					status.State = "auth_revoked"
				})
				if !sleepWithContext(ctx, cloudNotificationAuthRevokedDelay) {
					continue
				}
				continue
			}
			fallbackAcquired, fallbackErr := d.ingestCloudNotification(ctx, poller.profile, poller.botName, true, 0, nil)
			if fallbackErr != nil {
				recordPollerError(fallbackErr)
			} else if fallbackAcquired {
				recordPollerLease()
				continue
			} else {
				recordPollerError(socketError("UPSTREAM_ERROR", "notification wake failed", true))
				d.updateNotificationPollerStatus(poller, func(status *NotificationPollerStatus) {
					status.State = "wake_error"
				})
			}
			if !sleepWithContext(ctx, cloudNotificationPollErrorDelay) {
				continue
			}
			continue
		}
		if wake != nil {
			d.logger.info("notification.poller_wake", map[string]any{
				"profile":                poller.profile,
				"unread_count":           wake.UnreadCount,
				"newest_notification_id": wake.NewestNotificationID,
			})
		}
		if wake == nil && !sleepWithContext(ctx, cloudNotificationPollIdleDelay) {
			continue
		}
	}
}

func sleepWithContext(ctx context.Context, delay time.Duration) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	if delay <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func normalizeTmuxSubmitDelay(value time.Duration) time.Duration {
	if value < 0 {
		return 0
	}
	if value == 0 {
		value = DefaultTmuxSubmitDelay()
	}
	return clampTmuxSubmitDelay(value)
}

func DefaultTmuxSubmitDelay() time.Duration {
	value := strings.TrimSpace(os.Getenv("COMMENT_IO_TMUX_SUBMIT_DELAY"))
	if value == "" {
		return defaultTmuxSubmitDelay
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed < 0 {
		return defaultTmuxSubmitDelay
	}
	return clampTmuxSubmitDelay(parsed)
}

func clampTmuxSubmitDelay(value time.Duration) time.Duration {
	if value > maxTmuxSubmitDelay {
		return maxTmuxSubmitDelay
	}
	return value
}

func (d *Daemon) waitForTmuxSubmitSettle(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if d.tmuxSubmitDelay <= 0 {
		return ctx.Err()
	}
	if !sleepWithContext(ctx, d.tmuxSubmitDelay) {
		return ctx.Err()
	}
	return nil
}

func (d *Daemon) updateNotificationPollerStatus(poller *notificationPoller, edit func(*NotificationPollerStatus)) {
	d.notificationPollerMu.Lock()
	defer d.notificationPollerMu.Unlock()
	current := d.notificationPollers[poller.profile]
	if current != poller {
		return
	}
	edit(&current.status)
}

func (d *Daemon) notificationPollerHealth() (bool, []NotificationPollerStatus) {
	d.notificationPollerMu.Lock()
	defer d.notificationPollerMu.Unlock()
	statuses := make([]NotificationPollerStatus, 0, len(d.notificationPollers))
	for _, poller := range d.notificationPollers {
		statuses = append(statuses, poller.status)
	}
	sort.Slice(statuses, func(i, j int) bool {
		return statuses[i].Profile < statuses[j].Profile
	})
	return d.notificationPollersEnabled && d.notificationClient != nil, statuses
}

func (d *Daemon) logLocalBusWrite(message MessageEnvelope, outboxID string, sender messageAuthority, replayed bool) {
	data := messageLogData(message)
	data["outbox_id"] = outboxID
	data["from_profile"] = sender.Profile
	data["from_bot"] = sender.BotName
	if sender.BotID != "" {
		data["from_bot_id"] = sender.BotID
	}
	if sender.BotAgentID != "" {
		data["from_bot_agent_id"] = sender.BotAgentID
	}
	data["replayed"] = replayed
	d.logger.info("bus.local_write", data)
}

func (d *Daemon) logLeaseAcquired(message MessageEnvelope, mode string) {
	data := messageLogData(message)
	data["mode"] = mode
	d.logger.info("lease.acquired", data)
}

func (d *Daemon) logLeaseRenewed(message MessageEnvelope) {
	d.logger.info("lease.renewed", messageLogData(message))
}

func (d *Daemon) logMessageReceived(message MessageEnvelope) {
	data := messageLogData(message)
	data["claim_holder_type"] = claimHolderType(message.Delivery.ClaimHolder)
	d.logger.info("message.received", data)
}

func (d *Daemon) logMessageProxied(message MessageEnvelope, operation string) {
	d.logger.info("message."+operation+"_proxied", messageLogData(message))
}

func shouldLogSocketRequest(op string) bool {
	switch op {
	case "messages.send",
		"messages.receive",
		"messages.renew",
		"messages.ack",
		"messages.release",
		"messages.repair",
		"activity.complete",
		"messages.wait_local_summary",
		"messages.dispatch_ready_queue",
		"messages.cloud_ingest_ready_check",
		"messages.cloud_ingest_cloud_ready_check",
		"messages.list_inbox",
		"messages.list_sent",
		"sessions.start_nudge",
		"sessions.nudge_queue",
		"sessions.nudge_preflight",
		"sessions.nudge_send",
		"sessions.reset-complete",
		"sessions.daily_reset",
		"session.release_claims",
		"repair.startup",
		"cloud.ingest_local",
		"runtime.transient_nudge",
		"cloud.declined_duplicate_release",
		"cloud.stale_notification_release",
		"cloud.stale_botlets_release":
		return true
	default:
		return false
	}
}

func socketRequestLogData(req SocketRequest) map[string]any {
	data := map[string]any{
		"request_id": req.ID,
		"op":         req.Op,
	}
	if req.Auth != nil {
		data["auth_mode"] = req.Auth.Mode
		if req.Auth.Profile != nil {
			data["profile"] = *req.Auth.Profile
		}
		if req.Auth.SessionID != nil {
			data["session_id"] = *req.Auth.SessionID
		}
		if req.Auth.SessionGeneration != nil {
			data["session_generation"] = *req.Auth.SessionGeneration
		}
	}
	for _, key := range []string{"message_id", "op_id", "bot", "profile"} {
		if value, ok := safeSocketStringParam(req.Params, key); ok {
			data[key] = value
		}
	}
	return data
}

func safeSocketStringParam(params map[string]any, key string) (string, bool) {
	value, ok := params[key].(string)
	if !ok || value == "" || containsSecretValue(value) {
		return "", false
	}
	switch key {
	case "message_id":
		if !LocalMessageIDRE.MatchString(value) {
			return "", false
		}
	case "op_id":
		if !LocalOperationIDRE.MatchString(value) {
			return "", false
		}
	case "bot":
		if !isBotName(value) {
			return "", false
		}
	case "profile":
		if !ProfileRE.MatchString(value) {
			return "", false
		}
	}
	return value, true
}

type socketRequestContextKey struct{}

func contextWithSocketRequest(ctx context.Context, req SocketRequest) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, socketRequestContextKey{}, req)
}

func contextWithDiagnosticSocketRequest(ctx context.Context, fallback SocketRequest) context.Context {
	if req, ok := socketRequestFromContext(ctx); ok && shouldLogSocketRequest(req.Op) {
		return ctx
	}
	return contextWithSocketRequest(ctx, fallback)
}

func socketRequestFromContext(ctx context.Context) (SocketRequest, bool) {
	if ctx == nil {
		return SocketRequest{}, false
	}
	req, ok := ctx.Value(socketRequestContextKey{}).(SocketRequest)
	return req, ok
}

func socketRequestWithMessageID(req SocketRequest, messageID string) SocketRequest {
	if messageID == "" {
		return req
	}
	params := make(map[string]any, len(req.Params)+1)
	for key, value := range req.Params {
		params[key] = value
	}
	params["message_id"] = messageID
	req.Params = params
	return req
}

func cloudClaimOperationLogData(req SocketRequest, op CloudNotificationClaimOperation) map[string]any {
	data := socketRequestLogData(req)
	data["operation"] = op.Operation
	data["op_id"] = op.OpID
	data["profile"] = op.Profile
	data["message_id"] = op.LocalMessageID
	data["attempt"] = op.Attempts
	if op.LeaseTTLMS > 0 {
		data["lease_ttl_ms"] = op.LeaseTTLMS
	}
	return data
}

func cloudMutationErrorKind(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, errNotificationMutationDeadline) {
		return "deadline"
	}
	if errors.Is(err, errNotificationMutationAmbiguous) {
		return "ambiguous"
	}
	var statusErr *NotificationHTTPError
	if errors.As(err, &statusErr) {
		return "http_" + strconv.Itoa(statusErr.Status)
	}
	return "error"
}

func (d *Daemon) logCloudMutationStart(req SocketRequest, op CloudNotificationClaimOperation) {
	d.logger.info("notification.cloud_mutation.start", cloudClaimOperationLogData(req, op))
}

func (d *Daemon) logCloudMutationRemoteEnd(req SocketRequest, op CloudNotificationClaimOperation, duration time.Duration, err error) {
	data := cloudClaimOperationLogData(req, op)
	data["duration_ms"] = duration.Milliseconds()
	data["ok"] = err == nil
	if err != nil {
		data["error_kind"] = cloudMutationErrorKind(err)
		d.logger.warn("notification.cloud_mutation.remote_end", data)
		return
	}
	if duration >= slowCloudMutationDuration {
		d.logger.warn("notification.cloud_mutation.remote_end", data)
		return
	}
	d.logger.info("notification.cloud_mutation.remote_end", data)
}

func (d *Daemon) logCloudMutationLocalComplete(req SocketRequest, op CloudNotificationClaimOperation, duration time.Duration) {
	data := cloudClaimOperationLogData(req, op)
	data["duration_ms"] = duration.Milliseconds()
	d.logger.info("notification.cloud_mutation.local_complete", data)
}

func (d *Daemon) logCloudMutationFailed(req SocketRequest, op CloudNotificationClaimOperation, err error) {
	data := cloudClaimOperationLogData(req, op)
	data["error_kind"] = cloudMutationErrorKind(err)
	d.logger.warn("notification.cloud_mutation.failed", data)
}

func (d *Daemon) logSessionStarted(record SessionRecord, restarted bool) {
	data := sessionLogData(record)
	d.logger.info("session.started", data)
	if restarted {
		d.logger.info("session.restarted", data)
	}
}

func (d *Daemon) logSessionStale(record SessionRecord, reason string, releasedMessages []string, err *SocketError) {
	data := sessionLogData(record)
	data["reason"] = reason
	data["released_messages"] = len(releasedMessages)
	if err != nil {
		data["error_code"] = err.Code
		d.logger.warn("session.stale", data)
		return
	}
	d.logger.info("session.stale", data)
}

func (d *Daemon) logSessionDead(record SessionRecord, previousState string, reason string, releasedMessages []string, liveBeforeStop bool) {
	data := sessionLogData(record)
	data["previous_state"] = previousState
	data["reason"] = reason
	data["released_messages"] = len(releasedMessages)
	data["live_before_stop"] = liveBeforeStop
	d.logger.info("session.dead", data)
}

func (d *Daemon) logNudgeAttempted(record SessionRecord, messageID string) {
	data := sessionLogData(record)
	data["message_id"] = messageID
	d.logger.info("tmux.nudge_attempted", data)
}

func (d *Daemon) logNudgeSucceeded(record SessionRecord, messageID string) {
	data := sessionLogData(record)
	data["message_id"] = messageID
	d.logger.info("tmux.nudge_succeeded", data)
}

func (d *Daemon) logNudgeSkippedAsyncRewake(record SessionRecord, messageID string) {
	data := sessionLogData(record)
	data["message_id"] = messageID
	data["reason"] = "async_rewake"
	d.logger.info("tmux.nudge_skipped", data)
}

func (d *Daemon) logNudgeFailed(record SessionRecord, messageID string, stage string, errCode string) {
	data := sessionLogData(record)
	data["message_id"] = messageID
	data["stage"] = stage
	if errCode != "" {
		data["error_code"] = errCode
	}
	d.logger.warn("tmux.nudge_failed", data)
}

func (d *Daemon) logStartupInstructionFailed(record SessionRecord, err *SocketError) {
	data := sessionLogData(record)
	if err != nil && err.Code != "" {
		data["error_code"] = err.Code
	}
	d.logger.warn("tmux.startup_instruction_failed", data)
}

func messageLogData(message MessageEnvelope) map[string]any {
	data := map[string]any{
		"message_id":     message.ID,
		"profile":        message.Profile,
		"bot_name":       message.BotName,
		"source":         message.Source,
		"kind":           message.Kind,
		"delivery_state": message.Delivery.State,
	}
	if message.Delivery.LeaseExpiresAt != nil {
		data["lease_expires_at"] = *message.Delivery.LeaseExpiresAt
	}
	if message.BotID != "" {
		data["bot_id"] = message.BotID
	}
	if message.BotAgentID != "" {
		data["bot_agent_id"] = message.BotAgentID
	}
	if message.Delivery.SessionID != nil {
		data["session_id"] = *message.Delivery.SessionID
	}
	if message.Delivery.SessionGeneration != nil {
		data["session_generation"] = *message.Delivery.SessionGeneration
	}
	if message.Delivery.SessionScope.Type != nil {
		data["session_scope_type"] = *message.Delivery.SessionScope.Type
	}
	if message.Delivery.SessionScope.ID != nil {
		data["session_scope_id"] = *message.Delivery.SessionScope.ID
	}
	return data
}

func sessionLogData(record SessionRecord) map[string]any {
	data := map[string]any{
		"profile":            record.Profile,
		"bot_name":           record.BotName,
		"session_id":         record.SessionID,
		"session_generation": record.Generation,
		"scope_type":         record.ScopeType,
		"scope_id":           record.ScopeID,
		"state":              record.State,
		"host":               normalizeSessionHost(record.Host),
		"session_name":       record.SessionName,
		"pane_target":        record.PaneTarget,
	}
	if record.BotID != "" {
		data["bot_id"] = record.BotID
	}
	if record.BotAgentID != "" {
		data["bot_agent_id"] = record.BotAgentID
	}
	return data
}

func claimHolderType(holder *string) string {
	if holder == nil {
		return ""
	}
	switch {
	case strings.HasPrefix(*holder, "session:"):
		return "session"
	case strings.HasPrefix(*holder, "owner:"):
		return "owner"
	default:
		return "other"
	}
}

// healthTransportTrusted reports whether a caller may receive the full health
// payload. Only Unix-socket callers are trusted (0600 + SO_PEERCRED). The health
// op forbids an auth field (validated upstream), so a TCP caller cannot present a
// capability on health and always receives the reduced public subset.
func (d *Daemon) healthTransportTrusted(conn net.Conn) bool {
	_, ok := conn.(*net.UnixConn)
	return ok
}

// healthFeatures is the capability/feature banner reported by health. Shared by
// the full (trusted) health payload and the public (untrusted-transport) one so
// the two can't drift.
func healthFeatures() map[string]any {
	return map[string]any{
		FeatureBotletsSetupOrientation: FeatureBotletsSetupOrientationVersion,
		FeatureDaemonPairing:           FeatureDaemonPairingVersion,
		FeatureAgentEnrollment:         FeatureAgentEnrollmentVersion,
	}
}

// publicHealthResponse is the reply returned to a health caller on an untrusted
// transport (TCP). It carries liveness plus non-identifying operational fields
// (version, feature banner, profile/bot counts) so diagnostics like `comment
// doctor` degrade gracefully, while withholding the identifying metadata the
// Unix socket protects via 0600 + SO_PEERCRED — handles, daemon id, socket path,
// per-profile queue depths, and poller detail.
func (d *Daemon) publicHealthResponse(id string) SocketResponse {
	profilesLoaded, botsLoaded, _ := d.profileHealth()
	return SocketResponse{ID: id, OK: true, Result: map[string]any{
		"ok":               true,
		"version":          d.version,
		"profiles_loaded":  profilesLoaded,
		"bots_loaded":      botsLoaded,
		"protocol_version": BusProtocolVersion,
		"features":         healthFeatures(),
		// Marks this as the reduced untrusted-transport payload so consumers
		// (e.g. `comment doctor`) know identifying fields like
		// agent_profile_handles were withheld and must not infer "all good" from
		// counts alone.
		"limited": true,
	}}
}

// TCPAddr returns the resolved address of the opt-in bus TCP listener, or "" if
// no TCP listener is configured. Useful when the configured address used a :0
// port and the real port must be discovered.
func (d *Daemon) TCPAddr() string {
	if d.tcpListener == nil {
		return ""
	}
	return d.tcpListener.Addr().String()
}

// isLoopbackTCPAddr reports whether a host:port bind address targets only the
// loopback interface. A bare-port address (":7700") or "0.0.0.0" binds all
// interfaces and is not loopback.
func isLoopbackTCPAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

func (d *Daemon) serveListener(l net.Listener) {
	const (
		acceptBackoffStart = 5 * time.Millisecond
		acceptBackoffMax   = time.Second
	)
	ctx := d.baseCtx
	if ctx == nil {
		ctx = context.Background()
	}
	var backoff time.Duration
	for {
		conn, err := l.Accept()
		if err != nil {
			// A transient accept error (e.g. EMFILE/ENFILE under fd exhaustion)
			// must not kill the accept loop: a dead loop leaves the socket file on
			// disk but unserved — a wedge the file-identity watchdog can't see. Back
			// off and keep serving. A permanent error means the listener was closed
			// (shutdown or watchdog re-bind), so we exit.
			if isTransientAcceptError(err) {
				if backoff == 0 {
					backoff = acceptBackoffStart
				} else if backoff < acceptBackoffMax {
					backoff *= 2
					if backoff > acceptBackoffMax {
						backoff = acceptBackoffMax
					}
				}
				d.logger.warn("bus.accept_transient_error", map[string]any{
					"error":      err.Error(),
					"backoff_ms": backoff.Milliseconds(),
				})
				timer := time.NewTimer(backoff)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
				continue
			}
			return
		}
		backoff = 0
		go d.handleConn(conn)
	}
}

func (d *Daemon) handleConn(conn net.Conn) {
	defer conn.Close()
	line, err := readSocketRequestLine(conn)
	if err != nil {
		_ = writeSocketResponse(conn, SocketResponse{OK: false, Error: socketError("VALIDATION_ERROR", err.Error(), false)})
		return
	}
	// A TCP client opens with a server-auth handshake so it can verify this is the
	// real daemon before sending its capability. Answer it, then read the real
	// request on the same connection. (Unix clients never send one.)
	if hs, ok := isHandshakeLine(line); ok {
		if err := d.respondHandshake(conn, hs); err != nil {
			return
		}
		line, err = readSocketRequestLine(conn)
		if err != nil {
			_ = writeSocketResponse(conn, SocketResponse{OK: false, Error: socketError("VALIDATION_ERROR", err.Error(), false)})
			return
		}
	}
	// Watch the peer for an early disconnect so a long-running handler (an
	// asyncRewake `messages.wait`) can abort instead of claiming a message for a
	// caller that has already gone away. The request line is fully read above and
	// this is a one-shot request/response socket, so the only further bytes are a
	// close: a blocking Read therefore returns (error) exactly when the peer
	// disconnects. conn.Close() (deferred) unblocks the goroutine, so it never
	// leaks past this connection. Started AFTER the handshake read so it never
	// competes with readSocketRequestLine for the connection's bytes.
	connCtx, cancelConn := context.WithCancel(context.Background())
	defer cancelConn()
	go func() {
		var buf [1]byte
		_, readErr := conn.Read(buf[:])
		if readErr != nil {
			cancelConn()
		}
	}()
	response := d.HandleRequest(connCtx, conn, line)
	writeStartedAt := time.Now()
	stopWatchdog := d.startSocketResponseWriteWatchdog(line, response, writeStartedAt)
	writeErr := writeSocketResponse(conn, response)
	stopWatchdog()
	d.logSocketResponseWrite(line, response, time.Since(writeStartedAt), writeErr)
}

func (d *Daemon) HandleRequest(ctx context.Context, conn net.Conn, raw []byte) SocketResponse {
	var req SocketRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return SocketResponse{OK: false, Error: socketError("VALIDATION_ERROR", "invalid json", false)}
	}
	if err := validateSocketEnvelopeForAuth(req); err != nil {
		return SocketResponse{ID: safeSocketResponseID(req.ID), OK: false, Error: classifySocketValidationError(err)}
	}
	logReq := req
	if decoded, decodeErr := decodeSocketParams(req); decodeErr == nil {
		logReq = decoded
	}
	startedAt := time.Now()
	logRequest := shouldLogSocketRequest(req.Op)
	if logRequest {
		d.logger.info("socket.request.start", socketRequestLogData(logReq))
	}
	if req.Op != "health" {
		authReq := logReq
		var authErr *SocketError
		if logRequest {
			stage := d.startSocketStage(logReq, "socket.authorize")
			authErr = d.authorize(conn, authReq)
			if authErr != nil {
				stage.failed("auth_error")
			} else {
				stage.done()
			}
		} else {
			authErr = d.authorize(conn, authReq)
		}
		if authErr != nil {
			response := SocketResponse{ID: req.ID, OK: false, Error: authErr}
			if logRequest {
				d.logSocketRequestEnd(logReq, startedAt, response)
			}
			return response
		}
	}
	validated, err := validateAndDecodeSocketRequest(req)
	if err != nil {
		response := SocketResponse{ID: req.ID, OK: false, Error: classifySocketValidationError(err)}
		if logRequest {
			d.logSocketRequestEnd(logReq, startedAt, response)
		}
		return response
	}
	// health is an unauthenticated liveness probe, but its full payload exposes
	// identifying daemon metadata (id, profile handles, queue depths, paths). On
	// the Unix socket that's protected by 0600 + SO_PEERCRED; the opt-in TCP
	// transport has no such gate and health can't carry a capability, so a
	// non-unix caller always gets the reduced public subset.
	if validated.Op == "health" && !d.healthTransportTrusted(conn) {
		return d.publicHealthResponse(validated.ID)
	}
	response := d.dispatch(ctx, validated)
	if shouldLogSocketRequest(validated.Op) {
		d.logSocketRequestEnd(validated, startedAt, response)
	}
	return response
}

func (d *Daemon) logSocketRequestEnd(req SocketRequest, startedAt time.Time, response SocketResponse) {
	data := socketRequestLogData(req)
	duration := time.Since(startedAt)
	data["duration_ms"] = duration.Milliseconds()
	data["ok"] = response.OK
	if response.Error != nil {
		data["error_code"] = response.Error.Code
		data["retryable"] = response.Error.Retryable
		d.logger.warn("socket.request.end", data)
	} else if duration >= slowSocketRequestDuration {
		d.logger.warn("socket.request.end", data)
	} else {
		d.logger.info("socket.request.end", data)
	}
}

func (d *Daemon) logSocketResponseWrite(raw []byte, response SocketResponse, duration time.Duration, err error) {
	data, tracked := socketResponseWriteLogData(raw, response)
	if !tracked && err == nil && duration < slowSocketRequestDuration {
		return
	}
	data["phase"] = "complete"
	data["duration_ms"] = duration.Milliseconds()
	data["ok"] = err == nil
	if err != nil {
		data["error_kind"] = "write_failed"
		d.logger.warn("socket.response.write", data)
		return
	}
	if duration >= slowSocketRequestDuration {
		d.logger.warn("socket.response.write", data)
		return
	}
	d.logger.info("socket.response.write", data)
}

func (d *Daemon) startSocketResponseWriteWatchdog(raw []byte, response SocketResponse, startedAt time.Time) func() {
	data, tracked := socketResponseWriteLogData(raw, response)
	if !tracked {
		return func() {}
	}
	timer := time.AfterFunc(slowSocketRequestDuration, func() {
		data["phase"] = "waiting"
		data["duration_ms"] = time.Since(startedAt).Milliseconds()
		d.logger.warn("socket.response.write", data)
	})
	return func() {
		timer.Stop()
	}
}

func socketResponseWriteLogData(raw []byte, response SocketResponse) (map[string]any, bool) {
	var req SocketRequest
	if err := json.Unmarshal(raw, &req); err == nil {
		if validated, validateErr := validateAndDecodeSocketRequest(req); validateErr == nil && shouldLogSocketRequest(validated.Op) {
			data := socketRequestLogData(validated)
			if data["request_id"] == "" && response.ID != "" {
				data["request_id"] = response.ID
			}
			return data, true
		}
	}
	data := map[string]any{}
	if response.ID != "" {
		data["request_id"] = response.ID
	} else if isSafeSocketRequestID(req.ID) {
		data["request_id"] = req.ID
	}
	if _, ok := socketOperations[req.Op]; ok {
		data["op"] = req.Op
	} else if req.Op != "" {
		data["op"] = "invalid"
	} else {
		data["op"] = "unknown"
	}
	return data, false
}

func (d *Daemon) logSocketLockWait(req SocketRequest, lockName string, duration time.Duration) {
	if !shouldLogSocketRequest(req.Op) || duration < slowSocketLockWaitDuration {
		return
	}
	data := socketRequestLogData(req)
	data["lock"] = lockName
	data["phase"] = "acquired"
	data["duration_ms"] = duration.Milliseconds()
	d.logger.warn("socket.lock_wait_slow", data)
}

func (d *Daemon) startSocketLockWaitWatchdog(req SocketRequest, lockName string, startedAt time.Time) func() {
	if !shouldLogSocketRequest(req.Op) {
		return func() {}
	}
	timer := time.AfterFunc(slowSocketLockWaitDuration, func() {
		data := socketRequestLogData(req)
		data["lock"] = lockName
		data["phase"] = "waiting"
		data["duration_ms"] = time.Since(startedAt).Milliseconds()
		d.logger.warn("socket.lock_wait_slow", data)
	})
	return func() {
		timer.Stop()
	}
}

func (d *Daemon) lockBusForSocketRequest(req SocketRequest) {
	startedAt := time.Now()
	stopWatchdog := d.startSocketLockWaitWatchdog(req, "bus", startedAt)
	d.busMu.Lock()
	stopWatchdog()
	d.logSocketLockWait(req, "bus", time.Since(startedAt))
}

func (d *Daemon) lockBusForContext(ctx context.Context) {
	if req, ok := socketRequestFromContext(ctx); ok {
		d.lockBusForSocketRequest(req)
		return
	}
	d.busMu.Lock()
}

func (d *Daemon) lockSessionForSocketRequest(req SocketRequest) {
	startedAt := time.Now()
	stopWatchdog := d.startSocketLockWaitWatchdog(req, "session", startedAt)
	d.sessionMu.Lock()
	stopWatchdog()
	d.logSocketLockWait(req, "session", time.Since(startedAt))
}

func (d *Daemon) lockSessionForContext(ctx context.Context) {
	if req, ok := socketRequestFromContext(ctx); ok {
		d.lockSessionForSocketRequest(req)
		return
	}
	d.sessionMu.Lock()
}

type socketStageTracker struct {
	daemon    *Daemon
	req       SocketRequest
	stage     string
	startedAt time.Time
	stop      func()
}

func (d *Daemon) startSocketStage(req SocketRequest, stage string) socketStageTracker {
	startedAt := time.Now()
	return socketStageTracker{
		daemon:    d,
		req:       req,
		stage:     stage,
		startedAt: startedAt,
		stop:      d.startSocketStageWatchdog(req, stage, startedAt),
	}
}

func (d *Daemon) startSocketStageWatchdog(req SocketRequest, stage string, startedAt time.Time) func() {
	if !shouldLogSocketRequest(req.Op) {
		return func() {}
	}
	timer := time.AfterFunc(slowSocketLockWaitDuration, func() {
		data := socketRequestLogData(req)
		data["stage"] = stage
		data["phase"] = "waiting"
		data["duration_ms"] = time.Since(startedAt).Milliseconds()
		d.logger.warn("socket.stage_slow", data)
	})
	return func() {
		timer.Stop()
	}
}

func (stage socketStageTracker) done() {
	stage.stop()
	stage.daemon.logSocketStage(stage.req, stage.stage, time.Since(stage.startedAt), "", true)
}

func (stage socketStageTracker) failed(errorKind string) {
	stage.stop()
	stage.daemon.logSocketStage(stage.req, stage.stage, time.Since(stage.startedAt), errorKind, false)
}

func (d *Daemon) logSocketStage(req SocketRequest, stage string, duration time.Duration, errorKind string, ok bool) {
	if !shouldLogSocketRequest(req.Op) {
		return
	}
	if ok && duration < slowSocketLockWaitDuration {
		return
	}
	data := socketRequestLogData(req)
	data["stage"] = stage
	data["phase"] = "complete"
	data["duration_ms"] = duration.Milliseconds()
	data["ok"] = ok
	if errorKind != "" {
		data["error_kind"] = errorKind
	}
	d.logger.warn("socket.stage_slow", data)
}

func (d *Daemon) getInboxMessageForSocketRequest(req SocketRequest, profile string, messageID string, stageName string) (MessageEnvelope, error) {
	stage := d.startSocketStage(req, stageName)
	message, err := d.store.GetInboxMessage(context.Background(), profile, messageID)
	if err != nil {
		stage.failed("store_error")
		return MessageEnvelope{}, err
	}
	stage.done()
	return message, nil
}

func (d *Daemon) getInboxMessageForAuthority(req SocketRequest, authority messageAuthority, messageID string, stageName string) (MessageEnvelope, error) {
	if !messageAuthorityAllowsIdentityProfileDrift(req, authority) {
		return d.getInboxMessageForSocketRequest(req, authority.Profile, messageID, stageName)
	}
	stage := d.startSocketStage(req, stageName)
	message, err := d.store.GetInboxMessage(context.Background(), authority.Profile, messageID)
	if errors.Is(err, ErrMessageNotFound) {
		message, err = d.store.GetInboxMessageByID(context.Background(), messageID)
		if err == nil && message.Profile != authority.Profile && message.Source == "comment.io" && !messageMatchesAuthorityBot(message, authority) {
			err = ErrMessageNotFound
		}
	}
	if err != nil {
		stage.failed("store_error")
		return MessageEnvelope{}, err
	}
	stage.done()
	return message, nil
}

func (d *Daemon) getInboxMessageForContext(ctx context.Context, profile string, messageID string, stageName string) (MessageEnvelope, error) {
	if req, ok := socketRequestFromContext(ctx); ok {
		return d.getInboxMessageForSocketRequest(req, profile, messageID, stageName)
	}
	return d.store.GetInboxMessage(context.Background(), profile, messageID)
}

func (d *Daemon) runSocketStage(req SocketRequest, stageName string, fn func() error) error {
	stage := d.startSocketStage(req, stageName)
	if err := fn(); err != nil {
		stage.failed("store_error")
		return err
	}
	stage.done()
	return nil
}

func (d *Daemon) runSocketStageForContext(ctx context.Context, stageName string, fn func() error) error {
	if req, ok := socketRequestFromContext(ctx); ok {
		return d.runSocketStage(req, stageName, fn)
	}
	return fn()
}

func (d *Daemon) recordCloudNotificationClaimOperationAttempt(req SocketRequest, op CloudNotificationClaimOperation, now time.Time, errorMessage string) (CloudNotificationClaimOperation, *SocketError) {
	var attempted CloudNotificationClaimOperation
	if err := d.runSocketStage(req, "cloud.operation_attempt_record", func() error {
		var recordErr error
		attempted, recordErr = RecordCloudNotificationClaimOperationAttempt(d.paths, op, now)
		return recordErr
	}); err != nil {
		return CloudNotificationClaimOperation{}, socketError("UPSTREAM_ERROR", errorMessage, true)
	}
	return attempted, nil
}

func (d *Daemon) readPendingCloudNotificationClaimOperationForSocketRequest(req SocketRequest, opID string, stageName string) (CloudNotificationClaimOperation, bool, error) {
	var op CloudNotificationClaimOperation
	var ok bool
	err := d.runSocketStage(req, stageName, func() error {
		var readErr error
		op, ok, readErr = ReadPendingCloudNotificationClaimOperationWithRetry(d.paths, opID)
		return readErr
	})
	return op, ok, err
}

func (d *Daemon) readDoneCloudNotificationClaimOperationForSocketRequest(req SocketRequest, opID string, stageName string) (CloudNotificationClaimOperation, bool, error) {
	var op CloudNotificationClaimOperation
	var ok bool
	err := d.runSocketStage(req, stageName, func() error {
		var readErr error
		op, ok, readErr = ReadDoneCloudNotificationClaimOperation(d.paths, opID)
		return readErr
	})
	return op, ok, err
}

func (d *Daemon) completeCloudNotificationClaimOperationForSocketRequest(req SocketRequest, op CloudNotificationClaimOperation, now time.Time, stageName string) error {
	return d.runSocketStage(req, stageName, func() error {
		return CompleteCloudNotificationClaimOperation(d.paths, op, now)
	})
}

func (d *Daemon) releaseNotificationForSocketRequest(req SocketRequest, profileConfig AgentProfile, claimID string, opID string, stageName string) (*CloudNotificationClaimMutation, error) {
	var mutation *CloudNotificationClaimMutation
	err := d.runSocketStage(req, stageName, func() error {
		var releaseErr error
		mutation, releaseErr = d.notificationClient.ReleaseNotification(context.Background(), profileConfig, claimID, opID)
		return releaseErr
	})
	return mutation, err
}

// respondHandshake proves to a TCP client that this daemon holds the capability
// the client is about to use, without the capability ever crossing the wire. If
// the daemon can't resolve the expected capability for this auth (bad/unknown
// session, missing owner.cap), it returns an error and sends nothing — the client
// then fails verification and aborts without disclosing its capability.
func (d *Daemon) respondHandshake(conn net.Conn, hs handshakeRequest) error {
	capability, err := d.expectedCapabilityForAuth(hs.Auth)
	if err != nil {
		return err
	}
	return writeHandshakeResponse(conn, handshakeResponse{HSProof: handshakeProof(capability, hs.HSNonce)})
}

// expectedCapabilityForAuth returns the capability value the daemon expects for a
// given auth envelope (owner.cap for owner mode; the stored session capability
// for session mode), so it can prove knowledge of it during the handshake.
func (d *Daemon) expectedCapabilityForAuth(auth *SocketAuth) (string, error) {
	if auth == nil {
		return "", errors.New("handshake requires auth")
	}
	if auth.Mode == "owner" {
		expected, err := ReadPrivateCapability(d.paths.Home, d.paths.OwnerCapability, "owner capability file")
		if err != nil {
			return "", err
		}
		if !CapabilityTokenRE.MatchString(expected) {
			return "", errors.New("invalid owner capability")
		}
		return expected, nil
	}
	if auth.Profile == nil || auth.SessionID == nil || auth.SessionGeneration == nil {
		return "", errors.New("invalid session auth")
	}
	record, err := ReadSessionRecord(d.paths, *auth.SessionID)
	if err != nil {
		return "", err
	}
	if record.SessionID != *auth.SessionID || record.Profile != *auth.Profile || record.Generation != *auth.SessionGeneration {
		return "", errors.New("session mismatch")
	}
	path, err := sessionCapabilityPathForRecord(d.paths, record)
	if err != nil {
		return "", err
	}
	expected, err := ReadPrivateCapability(d.paths.Home, path, "capability file")
	if err != nil {
		return "", err
	}
	if !CapabilityTokenRE.MatchString(expected) {
		return "", errors.New("invalid session capability")
	}
	return expected, nil
}

func (d *Daemon) authorize(conn net.Conn, req SocketRequest) *SocketError {
	// Unix-socket callers are additionally gated by SO_PEERCRED UID matching
	// (defense-in-depth on top of the capability token). TCP callers — the opt-in
	// transport — have no peer credentials, so the capability token below is the
	// sole gate; the listener is loopback by default and the cap is required.
	if uc, ok := conn.(*net.UnixConn); ok {
		cred, err := d.peerCredentialFunc(uc)
		if err != nil {
			return socketError("FORBIDDEN", "could not verify local peer", false)
		}
		if cred.UID != d.expectedUID {
			return socketError("FORBIDDEN", "local peer uid mismatch", false)
		}
		// agents.mint-ephemeral is auth-OPTIONAL on the Unix socket: the
		// SO_PEERCRED UID gate above is the sole enforcer (a fresh bootstrap
		// session has no owner capability on disk), and the daemon mints with its
		// OWN pairing token — granting no authority beyond its existing
		// agent_enrollment. TCP callers (no peer creds) fall through to the
		// capability requirement below, so the auth-free path is Unix-only.
		if req.Op == "agents.mint-ephemeral" && req.Auth == nil {
			return nil
		}
	}
	if req.Auth == nil {
		return socketError("UNAUTHORIZED", "missing auth", false)
	}
	if req.Auth.Mode == "owner" {
		expected, err := ReadPrivateCapability(d.paths.Home, d.paths.OwnerCapability, "owner capability file")
		if err != nil {
			return socketError("FORBIDDEN", "owner capability unavailable", false)
		}
		if !CapabilityTokenRE.MatchString(expected) {
			return socketError("FORBIDDEN", "invalid owner capability", false)
		}
		// Constant-time compare: the owner capability is now reachable over the
		// opt-in TCP transport, so avoid leaking match length via timing.
		if subtle.ConstantTimeCompare([]byte(req.Auth.Capability), []byte(expected)) != 1 {
			return socketError("FORBIDDEN", "invalid owner capability", false)
		}
		return nil
	}
	if !sessionAuthAllowedForOperation(req.Op) {
		return socketError("FORBIDDEN", "session auth is not allowed for this operation", false)
	}
	verify := VerifySessionCapability
	if req.Op == "sessions.reset-complete" {
		verify = VerifySessionCapabilityForResetComplete
	}
	record, err := verify(d.paths, *req.Auth)
	if err != nil {
		return socketError("FORBIDDEN", "invalid session capability", false)
	}
	if req.Op == "reload-profiles" {
		if err := authorizeSessionProfileReload(req, record); err != nil {
			return socketError("FORBIDDEN", err.Error(), false)
		}
	}
	return nil
}

func authorizeSessionProfileReload(req SocketRequest, record SessionRecord) error {
	requested, ok := req.Params["botlets_home"].(string)
	if !ok || strings.TrimSpace(requested) == "" {
		return errors.New("session reload requires botlets home")
	}
	requestedHome, err := ResolveBotletsHome(requested)
	if err != nil {
		return errors.New("invalid botlets home")
	}
	sessionHome, err := ResolveBotletsHome(record.BotletsHome)
	if err != nil {
		return errors.New("invalid session botlets home")
	}
	if requestedHome != sessionHome {
		return errors.New("session auth is not allowed to reload this botlets home")
	}
	return nil
}

func (d *Daemon) dispatch(ctx context.Context, req SocketRequest) SocketResponse {
	switch req.Op {
	case "health":
		version, _ := d.store.SchemaVersion(context.Background())
		profilesLoaded, botsLoaded, profileErrors := d.profileHealth()
		notificationPollersEnabled, notificationPollers := d.notificationPollerHealth()
		startupDispatchErrors := d.startupDispatchErrors
		if startupDispatchErrors == nil {
			startupDispatchErrors = []MessageDispatchError{}
		}
		// Per-profile queued (unclaimed) message counts. Lets an operator see
		// a backlog piling up on a profile even when the daemon reports
		// "connected" — the silent-queue case from bug #95. Best-effort: a
		// query failure must not fail the whole health check.
		queuedMentions, queuedErr := d.store.UnclaimedCountsByProfile(context.Background())
		if queuedErr != nil {
			d.logger.warn("health.queued_mentions_failed", map[string]any{"error": queuedErr.Error()})
			queuedMentions = map[string]int{}
		}
		// Pairing state is re-read from daemon-auth.json on every health call
		// (it is one small file) so `comment bus pair`/`unpair` are reflected
		// without a daemon restart. Only the public daemon id is exposed —
		// never the token.
		daemonPaired := false
		var daemonID any
		if auth, ok, authErr := LoadDaemonAuth(d.paths); authErr != nil {
			d.logger.warn("health.daemon_auth_unreadable", map[string]any{"error": authErr.Error()})
		} else if ok {
			daemonPaired = true
			daemonID = auth.DaemonID
		}
		return SocketResponse{ID: req.ID, OK: true, Result: map[string]any{
			"ok":                           true,
			"version":                      d.version,
			"pid":                          d.pid,
			"started_at":                   d.startedAt.Format(time.RFC3339),
			"socket_path":                  d.paths.Socket,
			"bus_tcp_addr":                 d.TCPAddr(),
			"profiles_loaded":              profilesLoaded,
			"agent_profile_handles":        d.agentProfileHandlesLoaded(),
			"profile_load_errors":          d.profileLoadErrorsSnapshot(),
			"bots_loaded":                  botsLoaded,
			"profile_errors":               profileErrors,
			"notification_pollers_enabled": notificationPollersEnabled,
			"notification_pollers":         notificationPollers,
			"queued_mentions":              queuedMentions,
			"startup_dispatch_errors":      startupDispatchErrors,
			"schema_version":               version,
			"protocol_version":             BusProtocolVersion,
			"daemon_paired":                daemonPaired,
			"daemon_id":                    daemonID,
			"auto_update":                  AutoUpdateHealth(d.paths, d.version),
			// Only advertise features that actually work. FeatureAgentEnrollment
			// ships with the Phase 3 enrollment worker.
			"features": healthFeatures(),
		}}
	case "reload-profiles":
		botletsHome := ""
		if value, ok := req.Params["botlets_home"].(string); ok {
			botletsHome = value
		}
		return SocketResponse{ID: req.ID, OK: true, Result: d.reloadProfiles(context.Background(), botletsHome)}
	case "messages.repair":
		dryRun, _ := req.Params["dry_run"].(bool)
		filter := RepairFilter{}
		if messageID, ok := req.Params["message_id"].(string); ok {
			filter.MessageID = messageID
		}
		if opID, ok := req.Params["op_id"].(string); ok {
			filter.OpID = opID
		}
		actions, err := d.repairBus(contextWithSocketRequest(context.Background(), req), dryRun, filter)
		if err != nil {
			return SocketResponse{ID: req.ID, OK: false, Error: socketError("UPSTREAM_ERROR", "repair failed", true)}
		}
		return SocketResponse{ID: req.ID, OK: true, Result: map[string]any{"dry_run": dryRun, "actions": actions}}
	case "messages.send":
		send, err := d.sendMessages(req)
		if err != nil {
			return SocketResponse{ID: req.ID, OK: false, Error: err}
		}
		result := map[string]any{"outbox_id": send.OutboxID, "messages": send.Messages}
		if len(send.DispatchErrors) > 0 {
			result["dispatch_errors"] = send.DispatchErrors
		}
		return SocketResponse{ID: req.ID, OK: true, Result: result}
	case "messages.wait":
		rewakeWait, _ := req.Params["rewake"].(bool)
		// For an UNFILTERED rewake wait, register the pull-waiter at the handler
		// scope so it spans both the wait below AND the atomic receive — there is no
		// window where the waiter is gone while the message is still unclaimed, which
		// would otherwise let a competing dispatch bmux-nudge a message we are about
		// to claim (a managed-path double-wake). A kind-filtered wait registers no
		// waiter (it can't receive other kinds, so it must not suppress their nudges).
		var deregisterWaiter func()
		if rewakeWait {
			if kindsRaw, _ := req.Params["kinds"].([]any); len(kindsRaw) == 0 {
				if waitFilter, ferr := d.messageListFilter(req); ferr == nil {
					p, sid, gen := pullWaiterIdentity(req, waitFilter)
					deregisterWaiter = d.listeners.registerPullWaiter(p, sid, gen)
					defer deregisterWaiter()
				}
			}
		}
		// claimLost reports a lost rewake claim AND recovers the ready message: it
		// deregisters this waiter first (so the re-dispatch does not just re-skip on
		// our now-dead waiter) then re-dispatches the ready queue head, so a managed
		// session is re-nudged (bmux) or the rightful listener/cold-start picks it up
		// promptly instead of waiting for the next poll. deregisterWaiter is a Once,
		// so the deferred call above stays a safe no-op.
		claimLost := func() SocketResponse {
			if deregisterWaiter != nil {
				deregisterWaiter()
			}
			d.reDispatchAfterRewakeLoss(req)
			return SocketResponse{ID: req.ID, OK: true, Result: map[string]any{"claim_lost": true}}
		}
		listenSession, _ := req.Params["listen_session"].(string)
		// Validate the listen claim BEFORE blocking: on a daemon restart the in-memory
		// claims are gone, so a reconnecting scoped waiter would otherwise block in
		// waitMessage for the full interval before the post-wait check notices — leaving
		// the handle advertised as unclaimed all that time. Fast-fail to claim_lost so
		// the CLI re-claims (binding/launcher still live) or disarms promptly.
		if listenSession != "" {
			waitProfile, _ := req.Params["profile"].(string)
			if claim, ok := d.listeners.claimFor(waitProfile); !ok || claim.ClaimedBy != listenSession {
				return claimLost()
			}
		}
		summary, err := d.waitMessage(ctx, req)
		if err != nil {
			return SocketResponse{ID: req.ID, OK: false, Error: err}
		}
		// A rewake waiter that has gone away mid-wait (the Claude Code session/hook
		// was killed, so the socket peer disconnected) must NOT have a message
		// claimed on its behalf — that would strand it on a dead lease until expiry
		// instead of re-dispatching/cold-starting promptly. Treat a cancelled
		// connection like a lost claim: leave any ready message unclaimed.
		if rewakeWait && ctx.Err() != nil {
			return claimLost()
		}
		// Impromptu listen waits are claim-scoped: if the listen claim is no longer
		// held by this listen session (detached, force-released, or reassigned), do
		// NOT deliver to this now-stale waiter — report claim_lost so it exits and
		// frees the handle/lock. Any pending message stays unclaimed and is
		// re-dispatched to the rightful holder (or cold-started).
		if listenSession != "" {
			waitProfile, _ := req.Params["profile"].(string)
			if claim, ok := d.listeners.claimFor(waitProfile); !ok || claim.ClaimedBy != listenSession {
				return claimLost()
			}
		}
		if summary == nil {
			return SocketResponse{ID: req.ID, OK: true, Result: map[string]any{"timeout": true}}
		}
		if rewakeWait {
			// Claim the message in the same request, while the pull-waiter above is
			// still registered, and return the full message so the CLI does not make a
			// separate (gap-prone) receive call.
			recvReq := req
			recvReq.Op = "messages.receive"
			recvReq.Params = receiveParamsFromWait(req, summary.MessageID)
			message, recvErr := d.receiveMessage(recvReq)
			if recvErr != nil {
				return SocketResponse{ID: req.ID, OK: false, Error: recvErr}
			}
			// receiveMessage acquires the bus lock and does store I/O, so ownership can
			// change DURING the claim: the connection can drop, or the listen claim can
			// be force-released/reassigned, in the window between the pre-receive checks
			// above and the claim landing. Re-verify now that we hold the message; if we
			// are no longer the rightful listener, release it back so it re-dispatches to
			// the new owner (or cold-starts) instead of sitting on this dead lease.
			if d.rewakeOwnershipLost(ctx, req, listenSession) {
				d.releaseRewakeMessage(req, message)
				return claimLost()
			}
			rewakeResult := d.messageReceiveResult(message)
			if updated, outcome, skipped, skipErr := d.skipTerminalCloudReplay(recvReq, message); skipErr != nil {
				return SocketResponse{ID: req.ID, OK: false, Error: skipErr}
			} else if skipped {
				rewakeResult = terminalCloudReplaySkippedResult(updated, outcome)
			}
			return SocketResponse{ID: req.ID, OK: true, Result: rewakeResult}
		}
		return SocketResponse{ID: req.ID, OK: true, Result: summary}
	case "messages.receive":
		message, err := d.receiveMessage(req)
		if err != nil {
			return SocketResponse{ID: req.ID, OK: false, Error: err}
		}
		result := d.messageReceiveResult(message)
		if updated, outcome, skipped, skipErr := d.skipTerminalCloudReplay(req, message); skipErr != nil {
			return SocketResponse{ID: req.ID, OK: false, Error: skipErr}
		} else if skipped {
			result = terminalCloudReplaySkippedResult(updated, outcome)
		}
		return SocketResponse{ID: req.ID, OK: true, Result: result}
	case "messages.renew":
		message, err := d.renewMessage(req)
		if err != nil {
			return SocketResponse{ID: req.ID, OK: false, Error: err}
		}
		return SocketResponse{ID: req.ID, OK: true, Result: map[string]any{"message_id": message.ID, "lease_expires_at": message.Delivery.LeaseExpiresAt, "delivery": message.Delivery}}
	case "messages.ack":
		message, err := d.ackMessage(req)
		if err != nil {
			return SocketResponse{ID: req.ID, OK: false, Error: err}
		}
		return SocketResponse{ID: req.ID, OK: true, Result: map[string]any{"ok": true, "message_id": message.ID, "state": message.Delivery.State, "delivery": message.Delivery}}
	case "messages.release":
		message, err := d.releaseMessage(req)
		if err != nil {
			return SocketResponse{ID: req.ID, OK: false, Error: err}
		}
		return SocketResponse{ID: req.ID, OK: true, Result: map[string]any{"ok": true, "message_id": message.ID, "state": message.Delivery.State, "delivery": message.Delivery}}
	case "activity.complete":
		message, err := d.completeActivity(req)
		if err != nil {
			return SocketResponse{ID: req.ID, OK: false, Error: err}
		}
		return SocketResponse{ID: req.ID, OK: true, Result: map[string]any{"ok": true, "message_id": message.ID, "state": message.Delivery.State, "delivery": message.Delivery}}
	case "messages.list":
		messages, err := d.listMessages(req)
		if err != nil {
			return SocketResponse{ID: req.ID, OK: false, Error: err}
		}
		return SocketResponse{ID: req.ID, OK: true, Result: map[string]any{"messages": messages, "next_cursor": nextMessageCursor(messages, limitFromParams(req.Params))}}
	case "messages.sent":
		messages, err := d.sentMessages(req)
		if err != nil {
			return SocketResponse{ID: req.ID, OK: false, Error: err}
		}
		return SocketResponse{ID: req.ID, OK: true, Result: map[string]any{"messages": messages, "next_cursor": nextMessageCursor(messages, limitFromParams(req.Params))}}
	case "sessions.register":
		if req.Auth == nil || req.Auth.Mode != "owner" {
			return SocketResponse{ID: req.ID, OK: false, Error: socketError("FORBIDDEN", "sessions.register requires owner auth", false)}
		}
		record, err := d.registerSession(req)
		if err != nil {
			return SocketResponse{ID: req.ID, OK: false, Error: err}
		}
		return SocketResponse{ID: req.ID, OK: true, Result: map[string]any{"session": record}}
	case "sessions.start":
		if req.Auth == nil || req.Auth.Mode != "owner" {
			return SocketResponse{ID: req.ID, OK: false, Error: socketError("FORBIDDEN", "sessions.start requires owner auth", false)}
		}
		record, err := d.startSession(req)
		if err != nil {
			return SocketResponse{ID: req.ID, OK: false, Error: err}
		}
		return SocketResponse{ID: req.ID, OK: true, Result: map[string]any{"session": record}}
	case "sessions.stop":
		result, err := d.stopSession(req)
		if err != nil {
			return SocketResponse{ID: req.ID, OK: false, Error: err}
		}
		return SocketResponse{ID: req.ID, OK: true, Result: result}
	case "sessions.reset-complete":
		result, err := d.completeSessionDailyReset(req)
		if err != nil {
			return SocketResponse{ID: req.ID, OK: false, Error: err}
		}
		return SocketResponse{ID: req.ID, OK: true, Result: result}
	case "sessions.status":
		sessions, err := d.sessionStatus(req)
		if err != nil {
			return SocketResponse{ID: req.ID, OK: false, Error: err}
		}
		return SocketResponse{ID: req.ID, OK: true, Result: map[string]any{"sessions": sessions}}
	case "sessions.nudge":
		result, err := d.nudgeSession(req)
		if err != nil {
			return SocketResponse{ID: req.ID, OK: false, Error: err}
		}
		return SocketResponse{ID: req.ID, OK: true, Result: result}
	case "runtime.start":
		if req.Auth == nil || req.Auth.Mode != "owner" {
			return SocketResponse{ID: req.ID, OK: false, Error: socketError("FORBIDDEN", "runtime.start requires owner auth", false)}
		}
		startResult, err := d.startTransientRuntime(req)
		if err != nil {
			return SocketResponse{ID: req.ID, OK: false, Error: err}
		}
		return SocketResponse{ID: req.ID, OK: true, Result: map[string]any{"runtime": startResult.Record, "existing": startResult.Existing}}
	case "runtime.stop":
		if req.Auth == nil || req.Auth.Mode != "owner" {
			return SocketResponse{ID: req.ID, OK: false, Error: socketError("FORBIDDEN", "runtime.stop requires owner auth", false)}
		}
		result, err := d.stopTransientRuntimeForRequest(req)
		if err != nil {
			return SocketResponse{ID: req.ID, OK: false, Error: err}
		}
		return SocketResponse{ID: req.ID, OK: true, Result: result}
	case "runtime.status":
		if req.Auth == nil || req.Auth.Mode != "owner" {
			return SocketResponse{ID: req.ID, OK: false, Error: socketError("FORBIDDEN", "runtime.status requires owner auth", false)}
		}
		result, err := d.transientRuntimeStatusForRequest(req)
		if err != nil {
			return SocketResponse{ID: req.ID, OK: false, Error: err}
		}
		return SocketResponse{ID: req.ID, OK: true, Result: result}
	case "runtime.list":
		if req.Auth == nil || req.Auth.Mode != "owner" {
			return SocketResponse{ID: req.ID, OK: false, Error: socketError("FORBIDDEN", "runtime.list requires owner auth", false)}
		}
		result, err := d.listTransientRuntimesForRequest(req)
		if err != nil {
			return SocketResponse{ID: req.ID, OK: false, Error: err}
		}
		return SocketResponse{ID: req.ID, OK: true, Result: result}
	case "listen.handles":
		if req.Auth == nil || req.Auth.Mode != "owner" {
			return SocketResponse{ID: req.ID, OK: false, Error: socketError("FORBIDDEN", "listen.handles requires owner auth", false)}
		}
		return SocketResponse{ID: req.ID, OK: true, Result: d.listenHandles()}
	case "listen.claim":
		if req.Auth == nil || req.Auth.Mode != "owner" {
			return SocketResponse{ID: req.ID, OK: false, Error: socketError("FORBIDDEN", "listen.claim requires owner auth", false)}
		}
		result, err := d.claimListenHandle(req)
		if err != nil {
			return SocketResponse{ID: req.ID, OK: false, Error: err}
		}
		return SocketResponse{ID: req.ID, OK: true, Result: result}
	case "listen.release":
		if req.Auth == nil || req.Auth.Mode != "owner" {
			return SocketResponse{ID: req.ID, OK: false, Error: socketError("FORBIDDEN", "listen.release requires owner auth", false)}
		}
		result, err := d.releaseListenHandle(req)
		if err != nil {
			return SocketResponse{ID: req.ID, OK: false, Error: err}
		}
		return SocketResponse{ID: req.ID, OK: true, Result: result}
	case "agents.mint-ephemeral":
		// No owner-auth gate: the socket is already owner-only (0600), the mint
		// is owner-scoped + server-rate-limited, and this path must work during
		// a no-credential bootstrap (a fresh session with no ark_ key). The
		// daemon uses its OWN pairing token to mint; secrets never cross back.
		result, err := d.mintEphemeralViaPairing(ctx, req)
		if err != nil {
			return SocketResponse{ID: req.ID, OK: false, Error: err}
		}
		return SocketResponse{ID: req.ID, OK: true, Result: result}
	default:
		return SocketResponse{ID: req.ID, OK: false, Error: socketError("VALIDATION_ERROR", "operation not implemented yet", false)}
	}
}

func (d *Daemon) registerSession(req SocketRequest) (SessionRecord, *SocketError) {
	return SessionRecord{}, socketError("CONFLICT", "sessions.register is not available for managed sessions; use sessions.start", false)
}

// listenHandleInfo describes whether a configured profile is daemon-managed and
// whether an impromptu listen claim is currently held on it.
func (d *Daemon) configuredHandles() (all map[string]struct{}, managed map[string]struct{}) {
	all = map[string]struct{}{}
	managed = map[string]struct{}{}
	d.profileMu.RLock()
	for handle := range d.profileState.AgentProfiles {
		all[handle] = struct{}{}
	}
	for _, bot := range d.profileState.BotRegistry {
		if bot.Handle == "" {
			continue
		}
		all[bot.Handle] = struct{}{}
		if bot.ManagedSession.Enabled {
			managed[bot.Handle] = struct{}{}
		}
	}
	d.profileMu.RUnlock()
	// A handle currently driven by an active main transient runtime
	// (`comment run --profile H`) is also daemon-delivered, so it is busy for
	// impromptu listen purposes: surface it and refuse claims like a managed
	// handle. Taken in a separate lock section (not nested under profileMu) to
	// keep lock ordering uni-directional.
	d.transientRuntimeMu.Lock()
	for profile := range d.transientRuntimeMainProfiles {
		if profile != "" {
			all[profile] = struct{}{}
			managed[profile] = struct{}{}
		}
	}
	d.transientRuntimeMu.Unlock()
	return all, managed
}

// reserveMainEstablish marks a `comment run` main runtime as starting for
// profile, blocking if an impromptu listen claim already holds it. It returns an
// idempotent release for the in-flight marker (call with defer once the launch
// has either reserved the runtime or failed), the conflicting claim, and whether
// it was blocked. Held under listenEstablishMu so it is mutually exclusive with
// claimListenHandle: the two cannot both pass their checks and commit for the
// same handle.
func (d *Daemon) reserveMainEstablish(profile string) (release func(), conflict listenClaim, blocked bool) {
	d.listenEstablishMu.Lock()
	defer d.listenEstablishMu.Unlock()
	if claim, ok := d.listeners.claimFor(profile); ok {
		return nil, claim, true
	}
	d.establishingMain[profile]++
	var once sync.Once
	return func() {
		once.Do(func() {
			d.listenEstablishMu.Lock()
			if d.establishingMain[profile] <= 1 {
				delete(d.establishingMain, profile)
			} else {
				d.establishingMain[profile]--
			}
			d.listenEstablishMu.Unlock()
		})
	}, listenClaim{}, false
}

// listenHandles answers listen.handles: every configured profile with its
// managed flag and current impromptu listen-claim state.
func (d *Daemon) listenHandles() map[string]any {
	all, managed := d.configuredHandles()
	handles := make([]string, 0, len(all))
	for handle := range all {
		handles = append(handles, handle)
	}
	sort.Strings(handles)
	out := make([]map[string]any, 0, len(handles))
	for _, handle := range handles {
		_, isManaged := managed[handle]
		entry := map[string]any{
			"handle":     handle,
			"managed":    isManaged,
			"claimed":    false,
			"claimed_by": nil,
		}
		if claim, ok := d.listeners.claimFor(handle); ok {
			entry["claimed"] = true
			entry["claimed_by"] = listenClaimedByValue(claim)
		}
		out = append(out, entry)
	}
	return map[string]any{"handles": out}
}

func listenClaimedByValue(claim listenClaim) any {
	if claim.ClaimedBy == "" {
		return nil
	}
	return claim.ClaimedBy
}

// claimListenHandle answers listen.claim: register an impromptu attach to a free
// (non-daemon-managed) handle. Refuses MANAGED_HANDLE for daemon-managed bots
// and HANDLE_BUSY when another live claim already holds the handle.
func (d *Daemon) claimListenHandle(req SocketRequest) (map[string]any, *SocketError) {
	handle, _ := req.Params["profile"].(string)
	if !ProfileRE.MatchString(handle) {
		return nil, socketError("VALIDATION_ERROR", "invalid profile", false)
	}
	if req.Auth != nil && req.Auth.Profile != nil && *req.Auth.Profile != handle {
		return nil, socketError("FORBIDDEN", "owner profile does not match handle", false)
	}
	// Held across the configured/managed/establishing checks AND the claim commit
	// so a `comment run` for this handle cannot pass reserveMainEstablish in
	// between (which would let both a listen claim and a main runtime own it).
	d.listenEstablishMu.Lock()
	defer d.listenEstablishMu.Unlock()
	all, managed := d.configuredHandles()
	if _, ok := all[handle]; !ok {
		return nil, socketError("NOT_FOUND", "handle is not configured on this computer", false)
	}
	if _, ok := managed[handle]; ok {
		return nil, socketError("MANAGED_HANDLE", "handle "+handle+" is daemon-managed; use comment run "+handle, false)
	}
	if d.establishingMain[handle] > 0 {
		return nil, socketError("HANDLE_BUSY", "handle "+handle+" is starting a `comment run` runtime; detach is not needed, just run it there", false)
	}
	session, _ := req.Params["session"].(string)
	claim, ok := d.listeners.claimListen(handle, session, d.currentTime())
	if !ok {
		// Idempotent own-session re-claim: the handle is already claimed by THIS
		// session (e.g. a re-claim racing its own prior claim that has not yet been
		// evicted). Fall through to the managed re-check + success so the listener
		// resumes without violating the single-listener invariant. A different owner
		// still gets HANDLE_BUSY.
		if !(session != "" && claim.ClaimedBy == session) {
			message := "handle " + handle + " is already being listened to"
			if claim.ClaimedBy != "" {
				message += " by " + claim.ClaimedBy
			}
			return nil, socketError("HANDLE_BUSY", message, false)
		}
	}
	// Re-check managed status AFTER committing the claim: a concurrent
	// reloadProfiles can promote this handle to daemon-managed between the
	// configuredHandles() snapshot above and this commit, because profileState is
	// updated under profileMu (not listenEstablishMu). If it became managed, drop
	// the claim we just recorded and refuse — otherwise the impromptu listener would
	// deliver alongside the managed owner path. (reload's own dropClaimsForManaged
	// covers claims that predate the reload; this covers one committed during it.)
	_, managedNow := d.configuredHandles()
	if _, isManaged := managedNow[handle]; isManaged {
		d.listeners.dropClaimsForManaged(map[string]struct{}{handle: {}})
		return nil, socketError("MANAGED_HANDLE", "handle "+handle+" is daemon-managed; use comment run "+handle, false)
	}
	return map[string]any{
		"handle":     handle,
		"managed":    false,
		"claimed":    true,
		"claimed_by": listenClaimedByValue(claim),
	}, nil
}

// releaseListenHandle answers listen.release: drop any impromptu listen claim on
// the handle. Releasing an unclaimed handle is a no-op success.
func (d *Daemon) releaseListenHandle(req SocketRequest) (map[string]any, *SocketError) {
	handle, _ := req.Params["profile"].(string)
	if !ProfileRE.MatchString(handle) {
		return nil, socketError("VALIDATION_ERROR", "invalid profile", false)
	}
	if req.Auth != nil && req.Auth.Profile != nil && *req.Auth.Profile != handle {
		return nil, socketError("FORBIDDEN", "owner profile does not match handle", false)
	}
	session, _ := req.Params["session"].(string)
	force, _ := req.Params["force"].(bool)
	claim, released, mismatch := d.listeners.releaseListenScoped(handle, session, force)
	if mismatch {
		message := "handle " + handle + " is claimed by another session"
		if claim.ClaimedBy != "" {
			message += " (" + claim.ClaimedBy + ")"
		}
		message += "; pass the matching --session or --force"
		return nil, socketError("HANDLE_BUSY", message, false)
	}
	result := map[string]any{
		"handle":     handle,
		"released":   released,
		"claimed_by": nil,
	}
	if released {
		result["claimed_by"] = listenClaimedByValue(claim)
	}
	return result, nil
}

func (d *Daemon) controllerForHost(host string) TmuxController {
	if normalizeSessionHost(host) == SessionHostBmux {
		if d.bmux != nil {
			return d.bmux
		}
		return NewExecBmuxController(d.paths, "")
	}
	return d.tmux
}

func (d *Daemon) controllerForSession(record SessionRecord) TmuxController {
	return d.controllerForHost(record.Host)
}

func (d *Daemon) startSession(req SocketRequest) (SessionRecord, *SocketError) {
	authority, err := d.ownerSelectedAuthority(req.Auth, req.Params, true)
	if err != nil {
		return SessionRecord{}, err
	}
	scopeType, _ := req.Params["scope_type"].(string)
	scopeID, _ := req.Params["scope_id"].(string)
	if scopeType == "" {
		scopeType = "profile"
		scopeID = authority.Profile
	}
	bot, botletsHome, botErr := d.sessionBot(authority.Profile, authority.BotName)
	if botErr != nil {
		return SessionRecord{}, botErr
	}
	if requestedModelSet, _ := req.Params["requested_model_set"].(bool); requestedModelSet {
		requestedModel, _ := req.Params["requested_model"].(string)
		bot.ManagedSession.Model = requestedModel
	}
	if expectedRuntime, _ := req.Params["expected_runtime"].(string); expectedRuntime != "" && bot.ManagedSession.Runtime != expectedRuntime {
		return SessionRecord{}, socketError("CONFLICT", "managed session for "+authority.Profile+" uses runtime "+strconv.Quote(bot.ManagedSession.Runtime), false)
	}
	if expectedModel, _ := req.Params["expected_model"].(string); expectedModel != "" && bot.ManagedSession.Model != expectedModel {
		return SessionRecord{}, socketError("CONFLICT", "managed session for "+authority.Profile+" uses model "+strconv.Quote(bot.ManagedSession.Model), false)
	}
	d.sessionMu.Lock()
	defer d.sessionMu.Unlock()
	nudgeCtx := contextWithSocketRequest(context.Background(), SocketRequest{
		ID:     req.ID,
		Op:     "sessions.start_nudge",
		Auth:   req.Auth,
		Params: req.Params,
	})
	existing, ok, hadStale, staleReleasedMessages, existingErr := d.reconcileLiveSessionForScopeLocked(authority.Profile, authority.BotName, bot.BotID, botAgentID(bot), scopeType, scopeID, bot.ManagedSession.Runtime, bot.ManagedSession.Model)
	if existingErr != nil {
		return SessionRecord{}, existingErr
	}
	if ok {
		if dailyResetPending(existing) {
			return existing, nil
		}
		if _, nudgeErr := d.nudgeStaleReadyMessagesLockedWithContext(nudgeCtx, existing, hadStale, staleReleasedMessages); nudgeErr != nil {
			return SessionRecord{}, nudgeErr
		}
		return existing, nil
	}
	record, createErr := d.createAndLaunchSessionLocked(authority.Profile, authority.BotName, scopeType, scopeID, bot, botletsHome, hadStale)
	if createErr != nil {
		return SessionRecord{}, createErr
	}
	if _, nudgeErr := d.nudgeReadyQueueHeadIfIdleLockedWithContext(nudgeCtx, record, false); nudgeErr != nil {
		return SessionRecord{}, nudgeErr
	}
	return record, nil
}

func (d *Daemon) sessionBot(profile string, botName string) (BotRegistryEntry, string, *SocketError) {
	bot, botletsHome, managed, err := d.managedSessionBot(profile, botName)
	if err != nil {
		return BotRegistryEntry{}, "", err
	}
	if !managed {
		return BotRegistryEntry{}, "", socketError("CONFLICT", "bot is not configured for managed sessions", false)
	}
	return bot, botletsHome, nil
}

func (d *Daemon) managedSessionBot(profile string, botName string) (BotRegistryEntry, string, bool, *SocketError) {
	d.profileMu.RLock()
	bot, botOK := d.profileState.BotRegistry[botName]
	botletsHome := d.profileState.BotletsHome
	d.profileMu.RUnlock()
	if !botOK || bot.Handle != profile {
		return BotRegistryEntry{}, "", false, socketError("NOT_FOUND", "bot profile is not loaded", false)
	}
	if !bot.ManagedSession.Enabled {
		return bot, botletsHome, false, nil
	}
	return bot, botletsHome, true, nil
}

func (d *Daemon) createAndLaunchSessionLocked(profile string, botName string, scopeType string, scopeID string, bot BotRegistryEntry, botletsHome string, restarted bool) (SessionRecord, *SocketError) {
	return d.createAndLaunchSessionLockedWithContext(context.Background(), profile, botName, scopeType, scopeID, bot, botletsHome, restarted)
}

func (d *Daemon) createAndLaunchSessionLockedWithContext(ctx context.Context, profile string, botName string, scopeType string, scopeID string, bot BotRegistryEntry, botletsHome string, restarted bool) (SessionRecord, *SocketError) {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return SessionRecord{}, nil
	}
	sessionID, idErr := GenerateLocalID("sess", 0)
	if idErr != nil {
		return SessionRecord{}, socketError("UPSTREAM_ERROR", "could not allocate session id", true)
	}
	generation, genErr := GenerateLocalID("gen", 0)
	if genErr != nil {
		return SessionRecord{}, socketError("UPSTREAM_ERROR", "could not allocate session generation", true)
	}
	sessionName, nameErr := tmuxSessionName(botName, sessionID)
	if nameErr != nil {
		return SessionRecord{}, socketError("VALIDATION_ERROR", "could not build tmux session name", false)
	}
	record, err := RegisterSession(RegisterSessionOptions{
		Paths:       d.paths,
		Host:        normalizeNewManagedSessionHost(bot.ManagedSession.Host),
		Profile:     profile,
		BotName:     botName,
		BotID:       bot.BotID,
		BotAgentID:  botAgentID(bot),
		ScopeType:   scopeType,
		ScopeID:     scopeID,
		SessionID:   sessionID,
		Generation:  generation,
		BotletsHome: botletsHome,
		SessionName: sessionName,
		Runtime:     bot.ManagedSession.Runtime,
		Model:       bot.ManagedSession.Model,
		State:       "starting",
		Now:         d.currentTime(),
	})
	if err != nil {
		if errors.Is(err, ErrInvalidSession) {
			return SessionRecord{}, socketError("VALIDATION_ERROR", "could not register session", false)
		}
		return SessionRecord{}, socketError("UPSTREAM_ERROR", "could not register session", true)
	}
	if ctx.Err() != nil {
		record.State = "dead"
		_ = d.writeSessionRecordLocked(record)
		return SessionRecord{}, nil
	}
	launched, launchErr := d.launchTmuxSessionLockedWithContext(ctx, record)
	record = launched
	if launchErr != nil {
		record.State = "dead"
		_ = d.writeSessionRecordLocked(record)
		d.logSessionDead(record, "starting", "launch_failed", nil, false)
		return SessionRecord{}, launchErr
	}
	if ctx.Err() != nil {
		_ = d.controllerForSession(record).KillSession(context.Background(), record.SessionName)
		record.State = "dead"
		_ = d.writeSessionRecordLocked(record)
		return SessionRecord{}, nil
	}
	controller := d.controllerForSession(record)
	paneTarget, paneErr := controller.PaneTarget(ctx, record.SessionName)
	if paneErr != nil {
		_ = controller.KillSession(context.Background(), record.SessionName)
		record.State = "dead"
		_ = d.writeSessionRecordLocked(record)
		d.logSessionDead(record, "starting", "pane_target_unavailable", nil, false)
		return SessionRecord{}, socketError("UPSTREAM_ERROR", "could not resolve session target", true)
	}
	record.PaneTarget = paneTarget
	if err := d.writeSessionRecordLocked(record); err != nil {
		_ = controller.KillSession(context.Background(), record.SessionName)
		return SessionRecord{}, socketError("UPSTREAM_ERROR", "could not record session pane target", true)
	}
	if runtimeErr := d.waitForSessionRuntimeLockedWithContext(ctx, record, managedSessionRuntimeStartupTimeout); runtimeErr != nil {
		_ = controller.KillSession(context.Background(), record.SessionName)
		record.State = "dead"
		_ = d.writeSessionRecordLocked(record)
		d.logSessionDead(record, "starting", "runtime_start_failed", nil, true)
		return SessionRecord{}, runtimeErr
	}
	if profileConfig, bot, ok := d.cloudNotificationTarget(profile, record.BotName, true); ok {
		var startupErr *SocketError
		record, startupErr = d.sendSessionStartupInstruction(ctx, record, profileConfig.BaseURL, profileConfig.Handle, bot)
		if startupErr != nil {
			d.logStartupInstructionFailed(record, startupErr)
			if normalizeSessionHost(record.Host) == SessionHostBmux {
				_ = controller.KillSession(context.Background(), record.SessionName)
				record.State = "dead"
				_ = d.writeSessionRecordLocked(record)
				d.logSessionDead(record, "starting", "startup_instruction_failed", nil, true)
				return SessionRecord{}, startupErr
			}
		}
	} else {
		d.logger.warn("runtime.startup_instruction.profile_not_loaded", map[string]any{
			"session_name": record.SessionName,
			"profile":      profile,
			"bot_name":     record.BotName,
		})
	}
	record.State = "alive"
	record.StartupStartedAt = ""
	if err := d.writeSessionRecordLocked(record); err != nil {
		_ = controller.KillSession(context.Background(), record.SessionName)
		return SessionRecord{}, socketError("UPSTREAM_ERROR", "could not activate session", true)
	}
	d.logSessionStarted(record, restarted)
	return record, nil
}

func (d *Daemon) launchTmuxSessionLocked(record SessionRecord) (SessionRecord, *SocketError) {
	return d.launchTmuxSessionLockedWithContext(context.Background(), record)
}

func (d *Daemon) launchTmuxSessionLockedWithContext(ctx context.Context, record SessionRecord) (SessionRecord, *SocketError) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateSessionNameForHost(record.Host, record.SessionName); err != nil {
		return record, socketError("VALIDATION_ERROR", "invalid session name", false)
	}
	if ctx.Err() != nil {
		return record, nil
	}
	workingDir := d.managedSessionWorkingDir(record)
	if normalizeRuntimeLaunchMode(record.RuntimeLaunchMode) == RuntimeLaunchModeShell {
		// Shell mode: the runtime name is resolved through the user's login shell
		// at exec time (RunSessionExec), so there is no binary path to pin here.
		// Only the working directory may need persisting.
		if record.WorkingDir != workingDir {
			record.WorkingDir = workingDir
			if err := d.writeSessionRecordLocked(record); err != nil {
				return record, socketError("UPSTREAM_ERROR", "could not prepare session runtime", true)
			}
		}
	} else {
		runtimeResolution, err := resolveRuntimeCommandReference(record, nil)
		if err != nil {
			d.logger.warn("runtime.resolve_failed", map[string]any{
				"session_name": record.SessionName,
				"profile":      record.Profile,
				"bot_name":     record.BotName,
				"runtime":      record.Runtime,
				"reason":       err.Error(),
			})
			return record, socketError("UPSTREAM_ERROR", "could not trust runtime binary: "+err.Error(), true)
		}
		if record.RuntimePath != runtimeResolution.RuntimePath || record.RuntimeCommandPath != runtimeResolution.CommandPath || record.WorkingDir != workingDir {
			record.RuntimePath = runtimeResolution.RuntimePath
			record.RuntimeCommandPath = runtimeResolution.CommandPath
			record.WorkingDir = workingDir
			if err := d.writeSessionRecordLocked(record); err != nil {
				return record, socketError("UPSTREAM_ERROR", "could not prepare session runtime", true)
			}
		}
	}
	executablePath := d.commentExecutablePath
	if executablePath == nil {
		executablePath = commentExecutablePath
	}
	commentBinary, err := executablePath()
	if err != nil {
		return record, socketError("UPSTREAM_ERROR", "could not resolve comment binary", true)
	}
	commentBinary, err = resolveTrustedExecutable(commentBinary, "comment binary")
	if err != nil {
		return record, socketError("UPSTREAM_ERROR", "could not trust comment binary", true)
	}
	command, err := sessionExecCommand(commentBinary, record.SessionID, record.Generation)
	if err != nil {
		return record, socketError("VALIDATION_ERROR", "invalid session launcher", false)
	}
	outputPipeCommand := ""
	if normalizeSessionHost(record.Host) == SessionHostBmux {
		outputPipeCommand = transientRuntimeOutputPipeCommand(record.OutputLogPath, commentBinary)
	}
	if err := d.controllerForSession(record).NewSession(ctx, TmuxNewSessionOptions{
		SessionName:       record.SessionName,
		WorkingDir:        workingDir,
		CommentHome:       d.paths.Home,
		BotletsHome:       record.BotletsHome,
		Command:           command,
		OutputPipeCommand: outputPipeCommand,
	}); err != nil {
		return record, launchSessionSocketError(record, err)
	}
	return record, nil
}

func launchSessionSocketError(record SessionRecord, err error) *SocketError {
	// tmux missing entirely -> same distinct, human-readable error + install
	// hint the transient runtime path returns, so managed/auto-started sessions
	// do not fall back to an opaque launch failure.
	if errors.Is(err, ErrTmuxNotInstalled) {
		return socketError(SocketErrorCodeTmuxNotInstalled, TmuxNotInstalledMessage(), true)
	}
	// bmux missing entirely -> a distinct, human-readable error + install hint,
	// matching the tmux path, so managed/auto-started sessions don't fall back to
	// an opaque launch failure.
	if errors.Is(err, ErrBmuxNotInstalled) {
		return socketError(SocketErrorCodeBmuxNotInstalled, BmuxNotInstalledMessage(), true)
	}
	message := "could not launch session"
	if normalizeSessionHost(record.Host) == SessionHostBmux {
		detail := strings.TrimSpace(err.Error())
		if strings.Contains(detail, BmuxBinaryEnv) && !containsSecretValue(detail) && !strings.ContainsAny(detail, "\r\n\x00") {
			message += ": " + detail
		}
	}
	return socketError("UPSTREAM_ERROR", message, true)
}

func (d *Daemon) managedSessionWorkingDir(record SessionRecord) string {
	fallback := record.BotletsHome
	if fallback == "" {
		fallback = d.paths.Home
	}
	d.profileMu.RLock()
	bot, ok := d.profileState.BotRegistry[record.BotName]
	d.profileMu.RUnlock()
	if ok && bot.Handle == record.Profile && bot.BrainRef != nil {
		brainRoot, err := ValidateBotletsBrainProjection(d.paths, bot)
		if err != nil || brainRoot == "" {
			return fallback
		}
		return brainRoot
	}
	return fallback
}

func (d *Daemon) writeSessionRecordLocked(record SessionRecord) error {
	if d.writeSessionRecord != nil {
		return d.writeSessionRecord(d.paths, record)
	}
	return WriteSessionRecord(d.paths, record)
}

// listSessionRecords wraps the lenient package reader, logging any individual
// records it had to skip (malformed/invalid) so a poisoned record is visible in
// the structured logs instead of silently disappearing — and, crucially, never
// fails the whole read for one bad record (issue #1420).
func (d *Daemon) listSessionRecords() ([]SessionRecord, error) {
	records, skipped, err := ListSessionRecordsLenient(d.paths)
	if err != nil {
		return nil, err
	}
	if len(skipped) > 0 {
		sample := skipped
		if len(sample) > 10 {
			sample = sample[:10]
		}
		d.logger.warn("session.records_skipped", map[string]any{
			"count":       len(skipped),
			"session_ids": sample,
			"sampled":     len(sample) < len(skipped),
		})
	}
	return records, nil
}

func (d *Daemon) aliveSessionForScopeLocked(profile string, botName string, scopeType string, scopeID string) (SessionRecord, bool, *SocketError) {
	records, err := d.listSessionRecords()
	if err != nil {
		return SessionRecord{}, false, socketError("UPSTREAM_ERROR", "could not read sessions", false)
	}
	var matches []SessionRecord
	for _, record := range records {
		if record.Profile == profile && record.BotName == botName && record.ScopeType == scopeType && record.ScopeID == scopeID && record.State == "alive" {
			matches = append(matches, record)
		}
	}
	if len(matches) > 1 {
		return SessionRecord{}, false, socketError("CONFLICT", "multiple alive sessions match scope", false)
	}
	if len(matches) == 1 {
		return matches[0], true, nil
	}
	return SessionRecord{}, false, nil
}

func (d *Daemon) reconcileLiveSessionForScopeLocked(profile string, botName string, botID string, botAgentID string, scopeType string, scopeID string, expectedRuntime string, expectedModel string) (SessionRecord, bool, bool, []string, *SocketError) {
	records, err := d.listSessionRecords()
	if err != nil {
		return SessionRecord{}, false, false, nil, socketError("UPSTREAM_ERROR", "could not read sessions", false)
	}
	var liveMatches []SessionRecord
	var releasedMessages []string
	hadStale := false
	for _, record := range records {
		if !sessionRecordMatchesTargetScope(record, profile, botName, botID, botAgentID, scopeType, scopeID) {
			continue
		}
		switch record.State {
		case "stale":
			if cleanupErr := d.cleanupStaleBmuxControlSessionLocked(record, "stale_session_reconciliation"); cleanupErr != nil {
				return SessionRecord{}, false, false, nil, cleanupErr
			}
			released, releaseErr := d.releaseSessionClaimsLocked(record, "stale_session_reconciliation")
			if releaseErr != nil {
				return SessionRecord{}, false, false, nil, releaseErr
			}
			releasedMessages = append(releasedMessages, released...)
			hadStale = true
		case "alive", "starting":
			record, live, liveErr := d.recoverLiveTmuxSessionLocked(record)
			if liveErr != nil {
				return SessionRecord{}, false, false, nil, liveErr
			}
			if live {
				if managedSessionConfigChanged(record, expectedRuntime, expectedModel) {
					released, staleErr := d.markSessionStaleLocked(record, "managed_session_config_changed")
					if staleErr != nil {
						return SessionRecord{}, false, false, nil, staleErr
					}
					releasedMessages = append(releasedMessages, released...)
					hadStale = true
					continue
				}
				runtimeReason, runtimeErr := d.sessionRuntimeIssueLocked(record, record.State == "starting")
				if runtimeErr != nil {
					return SessionRecord{}, false, false, nil, runtimeErr
				}
				if runtimeReason != "" {
					released, staleErr := d.markSessionStaleLocked(record, runtimeReason)
					if staleErr != nil {
						return SessionRecord{}, false, false, nil, staleErr
					}
					releasedMessages = append(releasedMessages, released...)
					hadStale = true
					continue
				}
				if record.State == "starting" {
					record.State = "alive"
					if writeErr := d.writeSessionRecordLocked(record); writeErr != nil {
						return SessionRecord{}, false, false, nil, socketError("UPSTREAM_ERROR", "could not update session status", true)
					}
				}
				liveMatches = append(liveMatches, record)
				continue
			}
			released, staleErr := d.markSessionStaleLocked(record, "tmux_session_missing")
			if staleErr != nil {
				return SessionRecord{}, false, false, nil, staleErr
			}
			releasedMessages = append(releasedMessages, released...)
			hadStale = true
		}
	}
	if len(liveMatches) > 1 {
		record, duplicateReleasedMessages, duplicateErr := d.fenceDuplicateLiveSessionsLocked(liveMatches)
		if duplicateErr != nil {
			return SessionRecord{}, false, false, nil, duplicateErr
		}
		releasedMessages = append(releasedMessages, duplicateReleasedMessages...)
		return record, true, true, releasedMessages, nil
	}
	if len(liveMatches) == 1 {
		return liveMatches[0], true, hadStale, releasedMessages, nil
	}
	return SessionRecord{}, false, hadStale, releasedMessages, nil
}

func sessionRecordMatchesTargetScope(record SessionRecord, profile string, botName string, botID string, botAgentID string, scopeType string, scopeID string) bool {
	if record.ScopeType != scopeType {
		return false
	}
	labelsMatch := record.Profile == profile && record.BotName == botName
	stableMatch := sameStableBotIdentity(record.BotID, record.BotAgentID, botID, botAgentID)
	if scopeType == "profile" {
		return (labelsMatch && record.ScopeID == scopeID) || stableMatch
	}
	return record.ScopeID == scopeID && (labelsMatch || stableMatch)
}

func managedSessionConfigChanged(record SessionRecord, expectedRuntime string, expectedModel string) bool {
	if expectedRuntime != "" && record.Runtime != expectedRuntime {
		return true
	}
	return strings.TrimSpace(record.Model) != strings.TrimSpace(expectedModel)
}

func (d *Daemon) fenceDuplicateLiveSessionsLocked(matches []SessionRecord) (SessionRecord, []string, *SocketError) {
	ordered := append([]SessionRecord(nil), matches...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return sessionRecordPreferredOver(ordered[i], ordered[j])
	})
	kept := ordered[0]
	releasedMessages := []string{}
	for _, duplicate := range ordered[1:] {
		released, err := d.fenceDuplicateLiveSessionLocked(duplicate)
		if err != nil {
			return SessionRecord{}, nil, err
		}
		releasedMessages = append(releasedMessages, released...)
	}
	data := sessionLogData(kept)
	data["duplicates_fenced"] = len(ordered) - 1
	data["released_messages"] = len(releasedMessages)
	d.logger.warn("session.duplicates_fenced", data)
	return kept, releasedMessages, nil
}

func sessionRecordPreferredOver(a SessionRecord, b SessionRecord) bool {
	aTime, aErr := time.Parse(time.RFC3339Nano, a.CreatedAt)
	bTime, bErr := time.Parse(time.RFC3339Nano, b.CreatedAt)
	if aErr == nil && bErr == nil && !aTime.Equal(bTime) {
		return aTime.After(bTime)
	}
	if aErr == nil && bErr != nil {
		return true
	}
	if aErr != nil && bErr == nil {
		return false
	}
	if a.SessionID != b.SessionID {
		return a.SessionID > b.SessionID
	}
	return a.Generation > b.Generation
}

func (d *Daemon) fenceDuplicateLiveSessionLocked(record SessionRecord) ([]string, *SocketError) {
	previousState := record.State
	live := false
	if record.SessionName != "" {
		var liveErr error
		live, liveErr = d.controllerForSession(record).HasSession(context.Background(), record.SessionName)
		if liveErr != nil {
			return nil, socketError("UPSTREAM_ERROR", "could not inspect duplicate session", true)
		}
	}
	record.State = "stale"
	if err := d.writeSessionRecordLocked(record); err != nil {
		return nil, socketError("UPSTREAM_ERROR", "could not fence duplicate session", true)
	}
	if normalizeSessionHost(record.Host) == SessionHostBmux {
		if cleanupErr := d.cleanupStaleBmuxControlSessionLocked(record, "duplicate_live_session_fenced"); cleanupErr != nil {
			record.State = previousState
			_ = d.writeSessionRecordLocked(record)
			return nil, cleanupErr
		}
	} else if live {
		if killErr := d.controllerForSession(record).KillSession(context.Background(), record.SessionName); killErr != nil && !errors.Is(killErr, ErrTmuxSessionMissing) {
			data := sessionLogData(record)
			data["previous_state"] = previousState
			data["error"] = killErr.Error()
			d.logger.warn("session.duplicate_kill_failed", data)
			record.State = previousState
			_ = d.writeSessionRecordLocked(record)
			return nil, socketError("UPSTREAM_ERROR", "could not stop duplicate session", true)
		}
	}
	released, releaseErr := d.releaseSessionClaimsLocked(record, "duplicate_live_session_fenced")
	d.logSessionStale(record, "duplicate_live_session_fenced", released, releaseErr)
	return released, releaseErr
}

func (d *Daemon) hasStaleSessionForScopeLocked(profile string, botName string, scopeType string, scopeID string) (bool, *SocketError) {
	records, err := d.listSessionRecords()
	if err != nil {
		return false, socketError("UPSTREAM_ERROR", "could not read sessions", false)
	}
	for _, record := range records {
		if record.Profile == profile && record.BotName == botName && record.ScopeType == scopeType && record.ScopeID == scopeID && record.State == "stale" {
			return true, nil
		}
	}
	return false, nil
}

func (d *Daemon) ensureManagedSessionLocked(profile string, botName string, primaryMessageID string) (SessionRecord, bool, bool, *SocketError) {
	return d.ensureManagedSessionLockedWithContext(context.Background(), profile, botName, primaryMessageID)
}

func (d *Daemon) ensureManagedSessionLockedWithContext(ctx context.Context, profile string, botName string, primaryMessageID string) (SessionRecord, bool, bool, *SocketError) {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return SessionRecord{}, false, false, nil
	}
	bot, botletsHome, managed, err := d.managedSessionBot(profile, botName)
	if err != nil {
		return SessionRecord{}, false, false, err
	}
	if !managed {
		return SessionRecord{}, false, false, nil
	}
	existing, ok, hadStale, staleReleasedMessages, existingErr := d.reconcileLiveSessionForScopeLocked(profile, botName, bot.BotID, botAgentID(bot), "profile", profile, bot.ManagedSession.Runtime, bot.ManagedSession.Model)
	if existingErr != nil {
		return SessionRecord{}, false, false, existingErr
	}
	if ok {
		if dailyResetPending(existing) {
			return existing, true, false, nil
		}
		if ctx.Err() != nil {
			return existing, true, false, nil
		}
		readyNudged, nudgeErr := d.nudgeStaleReadyMessagesLockedWithContext(ctx, existing, hadStale, staleReleasedMessages)
		if nudgeErr != nil {
			return SessionRecord{}, false, false, nudgeErr
		}
		return existing, true, readyNudged, nil
	}
	if ctx.Err() != nil {
		return SessionRecord{}, true, false, nil
	}
	record, createErr := d.createAndLaunchSessionLockedWithContext(ctx, profile, botName, "profile", profile, bot, botletsHome, hadStale)
	if createErr != nil {
		return SessionRecord{}, false, false, createErr
	}
	if ctx.Err() != nil {
		return record, true, false, nil
	}
	readyNudged, nudgeErr := d.nudgeReadyQueueHeadIfIdleLockedWithContext(ctx, record, false)
	if nudgeErr != nil {
		return SessionRecord{}, false, false, nudgeErr
	}
	return record, true, readyNudged, nil
}

func (d *Daemon) recoverLiveTmuxSessionLocked(record SessionRecord) (SessionRecord, bool, *SocketError) {
	return d.recoverLiveTmuxSessionLockedWithOptions(record, bmuxRecoveryOptions{releaseClaimsOnRelaunchFailure: true})
}

func (d *Daemon) recoverLiveTmuxSessionLockedForRepair(record SessionRecord) (SessionRecord, bool, *SocketError) {
	return d.recoverLiveTmuxSessionLockedWithOptions(record, bmuxRecoveryOptions{releaseClaimsOnRelaunchFailure: false})
}

func (d *Daemon) recoverLiveTmuxSessionLockedForClaimReopen(record SessionRecord) (SessionRecord, bool, *SocketError) {
	return d.recoverLiveTmuxSessionLockedWithOptions(record, bmuxRecoveryOptions{releaseClaimsOnRelaunchFailure: false})
}

func (d *Daemon) recoverLiveTmuxSessionLockedWithOptions(record SessionRecord, options bmuxRecoveryOptions) (SessionRecord, bool, *SocketError) {
	if normalizeSessionHost(record.Host) == SessionHostBmux {
		return d.recoverLiveBmuxSessionLocked(record, options)
	}
	return d.liveTmuxSessionLocked(record, true)
}

func (d *Daemon) liveTmuxSessionLocked(record SessionRecord, recoverPane bool) (SessionRecord, bool, *SocketError) {
	if record.SessionName == "" {
		return record, false, nil
	}
	if err := validateSessionNameForHost(record.Host, record.SessionName); err != nil {
		return SessionRecord{}, false, socketError("VALIDATION_ERROR", "invalid session name", false)
	}
	if record.PaneTarget != "" {
		if err := validatePaneTargetForHost(record.Host, record.PaneTarget); err != nil {
			return SessionRecord{}, false, socketError("VALIDATION_ERROR", "invalid session target", false)
		}
	}
	live, err := d.controllerForSession(record).HasSession(context.Background(), record.SessionName)
	if err != nil {
		return SessionRecord{}, false, socketError("UPSTREAM_ERROR", "could not inspect session", true)
	}
	if !live || !recoverPane {
		return record, live, nil
	}
	return record, true, nil
}

type bmuxStatusReader interface {
	BmuxStatus(ctx context.Context, sessionName string) (bmuxStatus, error)
}

type bmuxRecoveryOptions struct {
	releaseClaimsOnRelaunchFailure bool
}

func (d *Daemon) recoverLiveBmuxSessionLocked(record SessionRecord, options bmuxRecoveryOptions) (SessionRecord, bool, *SocketError) {
	if record.SessionName == "" {
		return record, false, nil
	}
	if err := validateSessionNameForHost(record.Host, record.SessionName); err != nil {
		return SessionRecord{}, false, socketError("VALIDATION_ERROR", "invalid session name", false)
	}
	if record.PaneTarget != "" {
		if err := validatePaneTargetForHost(record.Host, record.PaneTarget); err != nil {
			return SessionRecord{}, false, socketError("VALIDATION_ERROR", "invalid session target", false)
		}
	}
	controller := d.controllerForSession(record)
	reader, ok := controller.(bmuxStatusReader)
	if !ok {
		return d.liveTmuxSessionLocked(record, true)
	}
	status, err := reader.BmuxStatus(context.Background(), record.SessionName)
	if err == nil && status.childAlive {
		return record, true, nil
	}
	if err != nil && !errors.Is(err, ErrTmuxSessionMissing) {
		return SessionRecord{}, false, socketError("UPSTREAM_ERROR", "could not inspect session", true)
	}
	if killErr := controller.KillSession(context.Background(), record.SessionName); killErr != nil && !errors.Is(killErr, ErrTmuxSessionMissing) {
		data := sessionLogData(record)
		data["error"] = killErr.Error()
		d.logger.warn("session.bmux_recovery_cleanup_failed", data)
		return SessionRecord{}, false, socketError("UPSTREAM_ERROR", "could not stop stale bmux session", true)
	}
	relaunched, recovered, relaunchErr := d.relaunchManagedBmuxSessionLocked(context.Background(), record, options)
	if relaunchErr != nil {
		return SessionRecord{}, false, relaunchErr
	}
	return relaunched, recovered, nil
}

func (d *Daemon) relaunchManagedBmuxSessionLocked(ctx context.Context, record SessionRecord, options bmuxRecoveryOptions) (SessionRecord, bool, *SocketError) {
	if ctx == nil {
		ctx = context.Background()
	}
	_, botletsHome, managed, botErr := d.managedSessionBot(record.Profile, record.BotName)
	if botErr != nil || !managed {
		return record, false, nil
	}
	if normalizeSessionHost(record.Host) != SessionHostBmux {
		return record, false, nil
	}
	if !isManagedSessionRuntime(record.Runtime) {
		return d.failManagedBmuxRelaunchLockedWithOptions(record, "runtime_untrusted", socketError("VALIDATION_ERROR", "invalid session runtime", false), options)
	}
	if record.RuntimeSessionRef != "" && !isRuntimeSessionRef(record.Runtime, record.RuntimeSessionRef) {
		return d.failManagedBmuxRelaunchLockedWithOptions(record, "runtime_untrusted", socketError("VALIDATION_ERROR", "invalid runtime session ref", false), options)
	}
	launchMode := managedSessionLaunchFresh
	if record.RuntimeSessionRef != "" {
		launchMode = managedSessionLaunchResume
	}
	if launchMode == managedSessionLaunchFresh && record.Runtime == "claude" {
		runtimeSessionRef, err := GenerateUUIDv4()
		if err != nil {
			return SessionRecord{}, false, socketError("UPSTREAM_ERROR", "could not allocate runtime session ref", true)
		}
		record.RuntimeSessionRef = runtimeSessionRef
	}
	record.BotletsHome = botletsHome
	record.RuntimeCommand = managedSessionRuntimeCommandForLaunch(record.Runtime, record.BotName, record.RuntimeSessionRef, launchMode, record.Model)
	record.PaneTarget = ""
	record.StartupStartedAt = busTime(d.currentTime())
	record.State = "starting"
	if err := d.writeSessionRecordLocked(record); err != nil {
		return SessionRecord{}, false, socketError("UPSTREAM_ERROR", "could not prepare session restart", true)
	}
	launched, launchErr := d.launchTmuxSessionLockedWithContext(ctx, record)
	record = launched
	if launchErr != nil {
		return d.failManagedBmuxRelaunchLockedWithOptions(record, "launch_failed", launchErr, options)
	}
	controller := d.controllerForSession(record)
	paneTarget, paneErr := controller.PaneTarget(ctx, record.SessionName)
	if paneErr != nil {
		_ = controller.KillSession(context.Background(), record.SessionName)
		return d.failManagedBmuxRelaunchLockedWithOptions(record, "pane_target_unavailable", socketError("UPSTREAM_ERROR", "could not resolve session target", true), options)
	}
	record.PaneTarget = paneTarget
	if err := d.writeSessionRecordLocked(record); err != nil {
		_ = controller.KillSession(context.Background(), record.SessionName)
		return d.failManagedBmuxRelaunchLockedWithOptions(record, "pane_target_unavailable", socketError("UPSTREAM_ERROR", "could not record session pane target", true), options)
	}
	if runtimeErr := d.waitForSessionRuntimeLockedWithContext(ctx, record, managedSessionRuntimeStartupTimeout); runtimeErr != nil {
		_ = controller.KillSession(context.Background(), record.SessionName)
		return d.failManagedBmuxRelaunchLockedWithOptions(record, "runtime_start_failed", runtimeErr, options)
	}
	if profileConfig, bot, ok := d.cloudNotificationTarget(record.Profile, record.BotName, true); ok {
		var startupErr *SocketError
		record, startupErr = d.sendSessionStartupInstruction(ctx, record, profileConfig.BaseURL, profileConfig.Handle, bot)
		if startupErr != nil {
			d.logStartupInstructionFailed(record, startupErr)
			_ = controller.KillSession(context.Background(), record.SessionName)
			return d.failManagedBmuxRelaunchLockedWithOptions(record, "startup_instruction_failed", startupErr, options)
		}
	} else {
		d.logger.warn("runtime.startup_instruction.profile_not_loaded", map[string]any{
			"session_name": record.SessionName,
			"profile":      record.Profile,
			"bot_name":     record.BotName,
		})
	}
	record.State = "alive"
	if err := d.writeSessionRecordLocked(record); err != nil {
		_ = controller.KillSession(context.Background(), record.SessionName)
		return d.failManagedBmuxRelaunchLockedWithOptions(record, "activate_failed", socketError("UPSTREAM_ERROR", "could not activate session", true), options)
	}
	d.logSessionStarted(record, true)
	return record, true, nil
}

func (d *Daemon) failManagedBmuxRelaunchLocked(record SessionRecord, reason string, failure *SocketError) (SessionRecord, bool, *SocketError) {
	return d.failManagedBmuxRelaunchLockedWithOptions(record, reason, failure, bmuxRecoveryOptions{releaseClaimsOnRelaunchFailure: true})
}

func (d *Daemon) failManagedBmuxRelaunchLockedWithOptions(record SessionRecord, reason string, failure *SocketError, options bmuxRecoveryOptions) (SessionRecord, bool, *SocketError) {
	var staleErr *SocketError
	if options.releaseClaimsOnRelaunchFailure {
		_, staleErr = d.markSessionStaleLocked(record, reason)
	} else {
		staleErr = d.markSessionStaleLockedWithoutClaimRelease(record, reason)
	}
	if staleErr != nil {
		return SessionRecord{}, false, staleErr
	}
	record.State = "stale"
	if options.releaseClaimsOnRelaunchFailure {
		return record, false, failure
	}
	return record, false, nil
}

func (d *Daemon) markSessionStaleLocked(record SessionRecord, reason string) ([]string, *SocketError) {
	return d.markSessionStaleLockedWithOptions(record, reason, true)
}

func (d *Daemon) markSessionStaleLockedWithoutClaimRelease(record SessionRecord, reason string) *SocketError {
	_, err := d.markSessionStaleLockedWithOptions(record, reason, false)
	return err
}

func (d *Daemon) markSessionStaleLockedWithOptions(record SessionRecord, reason string, releaseClaims bool) ([]string, *SocketError) {
	record.State = "stale"
	if err := d.writeSessionRecordLocked(record); err != nil {
		return nil, socketError("UPSTREAM_ERROR", "could not mark session stale", true)
	}
	if cleanupErr := d.cleanupStaleBmuxControlSessionLocked(record, reason); cleanupErr != nil {
		return nil, cleanupErr
	}
	if !releaseClaims {
		d.logSessionStale(record, reason, nil, nil)
		return nil, nil
	}
	released, releaseErr := d.releaseSessionClaimsLocked(record, reason)
	d.logSessionStale(record, reason, released, releaseErr)
	return released, releaseErr
}

func (d *Daemon) cleanupStaleBmuxControlSessionLocked(record SessionRecord, reason string) *SocketError {
	if normalizeSessionHost(record.Host) == SessionHostBmux && record.SessionName != "" {
		if killErr := d.controllerForSession(record).KillSession(context.Background(), record.SessionName); killErr != nil && !errors.Is(killErr, ErrTmuxSessionMissing) {
			data := sessionLogData(record)
			data["reason"] = reason
			data["error"] = killErr.Error()
			d.logger.warn("session.stale_cleanup_failed", data)
			return socketError("UPSTREAM_ERROR", "could not stop stale bmux session", true)
		}
	}
	return nil
}

func (d *Daemon) releaseSessionClaimsLocked(record SessionRecord, reason string) ([]string, *SocketError) {
	claimHolder := "session:" + record.SessionID + ":" + record.Generation
	now := time.Now().UTC()
	req := internalCloudReleaseSocketRequest("session.release_claims", record.Profile, "")
	d.lockBusForSocketRequest(req)
	defer d.busMu.Unlock()
	var releasedMessages []string
	if err := d.runSocketStage(req, "session.release_claims.local_release", func() error {
		var storeErr error
		releasedMessages, storeErr = d.store.ReleaseClaimsForHolder(context.Background(), claimHolder, reason, now)
		return storeErr
	}); err != nil {
		return nil, classifyMessageStoreError(err)
	}
	var cloudMessages []MessageEnvelope
	if err := d.runSocketStage(req, "session.release_claims.cloud_list", func() error {
		var storeErr error
		cloudMessages, storeErr = d.store.ListCloudClaimsForHolder(context.Background(), claimHolder)
		return storeErr
	}); err != nil {
		return nil, classifyMessageStoreError(err)
	}
	for _, message := range cloudMessages {
		messageReq := socketRequestWithMessageID(req, message.ID)
		var metadata PrivateCloudMessageMetadata
		metadataErr := d.runSocketStage(messageReq, "session.release_claims.metadata_read", func() error {
			var err error
			metadata, err = ReadPrivateCloudMessageMetadata(d.paths, message.Profile, message.ID)
			return err
		})
		var pendingOps []CloudNotificationClaimOperation
		if metadataErr != nil {
			if err := d.runSocketStage(messageReq, "session.release_claims.pending_ops_read", func() error {
				var pendingErr error
				pendingOps, pendingErr = ListPendingCloudNotificationClaimOperationsForLocalMessage(d.paths, message.Profile, message.ID)
				return pendingErr
			}); err != nil {
				return nil, socketError("UPSTREAM_ERROR", "could not inspect pending notification operations", true)
			}
		} else {
			if err := d.runSocketStage(messageReq, "session.release_claims.pending_ops_read", func() error {
				var pendingErr error
				pendingOps, pendingErr = ListPendingCloudNotificationClaimOperationsForMessage(d.paths, message.Profile, message.ID, metadata.ClaimID, metadata.NotificationID)
				return pendingErr
			}); err != nil {
				return nil, socketError("UPSTREAM_ERROR", "could not inspect pending notification operations", true)
			}
		}
		for _, pendingOp := range pendingOps {
			if pendingOp.Operation == "renew" {
				if err := d.runSocketStage(messageReq, "session.release_claims.abandon_renew", func() error {
					return AbandonCloudNotificationClaimOperation(d.paths, pendingOp)
				}); err != nil {
					return nil, socketError("UPSTREAM_ERROR", "could not abandon pending notification renew", true)
				}
			}
		}
		if HasPendingTerminalCloudNotificationClaimOperation(pendingOps) {
			continue
		}
		if metadataErr != nil {
			updated, err := d.quarantineCloudMessageForMissingMetadata(contextWithSocketRequest(context.Background(), messageReq), message.Profile, message.ID, now)
			if err != nil {
				return nil, classifyMessageStoreError(err)
			}
			releasedMessages = append(releasedMessages, updated.ID)
			continue
		}
		if leaseExpired(message.Delivery.LeaseExpiresAt, now) {
			if err := d.runSocketStage(messageReq, "session.release_claims.expired_cloud_release", func() error {
				_, err := d.releaseAbandonedCloudClaimLocked(context.Background(), message, "lease_expired", now)
				return err
			}); err != nil {
				return nil, socketError("UPSTREAM_ERROR", "could not release expired cloud session claim", true)
			}
			var updated MessageEnvelope
			if err := d.runSocketStage(messageReq, "session.release_claims.refetch_message", func() error {
				var storeErr error
				updated, storeErr = d.store.GetInboxMessage(context.Background(), message.Profile, message.ID)
				return storeErr
			}); err != nil {
				return nil, classifyMessageStoreError(err)
			}
			if updated.Delivery.State == "released" && updated.Delivery.ClaimHolder == nil && updated.Delivery.LeaseExpiresAt == nil {
				releasedMessages = append(releasedMessages, message.ID)
			}
			continue
		}
		var updated MessageEnvelope
		if err := d.runSocketStage(messageReq, "session.release_claims.requeue_cloud_message", func() error {
			var storeErr error
			updated, storeErr = d.store.RequeueCloudMessageLocally(context.Background(), message.Profile, message.ID, claimHolder, reason, now)
			return storeErr
		}); err != nil {
			return nil, classifyMessageStoreError(err)
		}
		releasedMessages = append(releasedMessages, updated.ID)
	}
	return releasedMessages, nil
}

type managedSessionResetTarget struct {
	Profile     string
	BotName     string
	Bot         BotRegistryEntry
	BotletsHome string
}

func (d *Daemon) currentTime() time.Time {
	if d.now != nil {
		return d.now().UTC()
	}
	return time.Now().UTC()
}

func (d *Daemon) runManagedSessionDailyResetLoop(ctx context.Context) {
	if managedSessionDailyResetPollInterval <= 0 {
		return
	}
	for {
		if !sleepWithContext(ctx, managedSessionDailyResetPollInterval) {
			return
		}
		for _, resetErr := range d.runManagedSessionDailyResetOnce(ctx) {
			d.logger.warn("session.daily_reset_reconcile_failed", map[string]any{
				"profile":   resetErr.Profile,
				"code":      resetErr.Code,
				"message":   resetErr.Message,
				"retryable": resetErr.Retryable,
			})
		}
	}
}

func (d *Daemon) runManagedSessionDailyResetOnce(ctx context.Context) []MessageDispatchError {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return nil
	}
	now := d.currentTime()
	targets := d.managedSessionResetTargets()
	var out []MessageDispatchError
	for _, target := range targets {
		if ctx.Err() != nil {
			return out
		}
		if err := d.reconcileManagedSessionDailyReset(ctx, target, now); err != nil {
			out = append(out, MessageDispatchError{
				Profile:   target.Profile,
				Code:      err.Code,
				Message:   err.Message,
				Retryable: err.Retryable,
			})
		}
	}
	return out
}

func (d *Daemon) managedSessionResetTargets() []managedSessionResetTarget {
	d.profileMu.RLock()
	defer d.profileMu.RUnlock()
	targets := make([]managedSessionResetTarget, 0, len(d.profileState.BotRegistry))
	for botName, bot := range d.profileState.BotRegistry {
		if !bot.ManagedSession.Enabled || bot.Handle == "" {
			continue
		}
		targets = append(targets, managedSessionResetTarget{
			Profile:     bot.Handle,
			BotName:     botName,
			Bot:         bot,
			BotletsHome: d.profileState.BotletsHome,
		})
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].Profile != targets[j].Profile {
			return targets[i].Profile < targets[j].Profile
		}
		return targets[i].BotName < targets[j].BotName
	})
	return targets
}

func (d *Daemon) reconcileManagedSessionDailyReset(ctx context.Context, target managedSessionResetTarget, now time.Time) *SocketError {
	date, resetAt, timezone, due, locErr := managedSessionDailyResetWindow(target.Bot, now)
	if locErr != nil {
		return socketError("VALIDATION_ERROR", "invalid managed session reset timezone", false)
	}
	if !due {
		return nil
	}
	d.sessionMu.Lock()
	defer d.sessionMu.Unlock()
	completed, pending, hasPending, err := d.dailyResetStateForTargetLocked(target.Profile, target.BotName, date)
	if err != nil {
		return err
	}
	if completed {
		return nil
	}
	if hasPending {
		return d.continuePendingDailyResetLocked(ctx, target, pending, timezone, now)
	}
	record, ok, _, _, reconcileErr := d.reconcileLiveSessionForScopeLocked(target.Profile, target.BotName, target.Bot.BotID, botAgentID(target.Bot), "profile", target.Profile, target.Bot.ManagedSession.Runtime, target.Bot.ManagedSession.Model)
	if reconcileErr != nil {
		return reconcileErr
	}
	if !ok || !sessionCreatedBefore(record, resetAt) {
		return nil
	}
	return d.requestDailyResetLocked(ctx, target, record, date, timezone, now)
}

func managedSessionDailyResetWindow(bot BotRegistryEntry, now time.Time) (string, time.Time, string, bool, error) {
	location, timezone, err := managedSessionTimezone(bot)
	if err != nil {
		return "", time.Time{}, "", false, err
	}
	localNow := now.In(location)
	resetAt := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 2, 0, 0, 0, location)
	return localNow.Format("2006-01-02"), resetAt, timezone, !localNow.Before(resetAt), nil
}

func managedSessionTimezone(bot BotRegistryEntry) (*time.Location, string, error) {
	timezone := strings.TrimSpace(bot.ManagedSession.Timezone)
	if timezone == "" {
		timezone = defaultManagedSessionTimezone
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, "", err
	}
	return location, timezone, nil
}

func sessionCreatedBefore(record SessionRecord, resetAt time.Time) bool {
	createdAt, err := time.Parse(time.RFC3339Nano, record.CreatedAt)
	if err != nil {
		return true
	}
	return createdAt.Before(resetAt)
}

func (d *Daemon) dailyResetStateForTargetLocked(profile string, botName string, date string) (bool, SessionRecord, bool, *SocketError) {
	records, err := d.listSessionRecords()
	if err != nil {
		return false, SessionRecord{}, false, socketError("UPSTREAM_ERROR", "could not read sessions", false)
	}
	var pending []SessionRecord
	for _, record := range records {
		if record.Profile != profile || record.BotName != botName || record.ScopeType != "profile" || record.ScopeID != profile || record.DailyReset == nil || record.DailyReset.Date != date {
			continue
		}
		switch record.DailyReset.State {
		case "completed":
			return true, SessionRecord{}, false, nil
		case "requested", "replacing":
			pending = append(pending, record)
		}
	}
	if len(pending) == 0 {
		return false, SessionRecord{}, false, nil
	}
	sort.SliceStable(pending, func(i, j int) bool {
		return sessionRecordPreferredOver(pending[i], pending[j])
	})
	return false, pending[0], true, nil
}

func (d *Daemon) continuePendingDailyResetLocked(ctx context.Context, target managedSessionResetTarget, record SessionRecord, timezone string, now time.Time) *SocketError {
	if record.DailyReset != nil && record.DailyReset.State == "replacing" {
		_, finishErr := d.finishDailyResetLocked(ctx, target, record, "daily_reset_replacement_retry", false)
		return finishErr
	}
	live := false
	if record.State == "alive" || record.State == "starting" {
		var liveErr *SocketError
		record, live, liveErr = d.recoverLiveTmuxSessionLocked(record)
		if liveErr != nil {
			_, finishErr := d.finishDailyResetLocked(ctx, target, record, "daily_reset_tmux_unhealthy", true)
			return finishErr
		}
	}
	if !live || dailyResetDeadlineExceeded(record, now) {
		_, finishErr := d.finishDailyResetLocked(ctx, target, record, "daily_reset_fallback", true)
		return finishErr
	}
	if record.DailyReset != nil && record.DailyReset.PromptedAt == "" {
		updated, missing, promptErr := d.sendDailyResetPromptLocked(ctx, target, record, *record.DailyReset, timezone, now)
		if missing {
			_, finishErr := d.finishDailyResetLocked(ctx, target, updated, "daily_reset_tmux_missing", true)
			return finishErr
		}
		return promptErr
	}
	return nil
}

func dailyResetDeadlineExceeded(record SessionRecord, now time.Time) bool {
	if record.DailyReset == nil {
		return false
	}
	deadline, err := time.Parse(time.RFC3339Nano, record.DailyReset.DeadlineAt)
	if err != nil {
		return true
	}
	return !now.Before(deadline)
}

func (d *Daemon) requestDailyResetLocked(ctx context.Context, target managedSessionResetTarget, record SessionRecord, date string, timezone string, now time.Time) *SocketError {
	logPath, pathErr := d.dailyResetLogPath(target.Bot, target.BotletsHome, date)
	if pathErr != nil {
		return socketError("UPSTREAM_ERROR", "could not resolve daily reset log path", true)
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return socketError("UPSTREAM_ERROR", "could not prepare daily reset log directory", true)
	}
	if err := os.Chmod(filepath.Dir(logPath), 0o700); err != nil {
		return socketError("UPSTREAM_ERROR", "could not secure daily reset log directory", true)
	}
	reset := DailyResetRecord{
		Date:        date,
		State:       "requested",
		Reason:      "daily_reset",
		RequestedAt: busTime(now),
		DeadlineAt:  busTime(now.Add(managedSessionDailyResetGracePeriod)),
		LogPath:     logPath,
	}
	record.DailyReset = &reset
	if err := d.writeSessionRecordLocked(record); err != nil {
		return socketError("UPSTREAM_ERROR", "could not record daily reset", true)
	}
	updated, missing, promptErr := d.sendDailyResetPromptLocked(ctx, target, record, reset, timezone, now)
	if missing {
		_, finishErr := d.finishDailyResetLocked(ctx, target, updated, "daily_reset_tmux_missing", true)
		return finishErr
	}
	if promptErr != nil {
		return promptErr
	}
	data := sessionLogData(updated)
	data["date"] = date
	data["timezone"] = timezone
	data["log_path"] = logPath
	d.logger.info("session.daily_reset_requested", data)
	return nil
}

func (d *Daemon) sendDailyResetPromptLocked(ctx context.Context, target managedSessionResetTarget, record SessionRecord, reset DailyResetRecord, timezone string, now time.Time) (SessionRecord, bool, *SocketError) {
	prompt := formatDailyResetPrompt(target.Bot, record, reset, timezone)
	unlock := d.tmuxNudgeLocks.lock(record.SessionName)
	defer unlock()
	controller := d.controllerForSession(record)
	if missing, readyErr := d.verifySessionRuntimeReadyForPrompt(ctx, record); readyErr != nil || missing {
		return record, missing, readyErr
	}
	submissionBefore, submissionEnabled, submissionErr := d.beginRuntimeSubmissionCheck(record)
	if submissionErr != nil {
		return record, false, submissionErr
	}
	if _, err := d.sendPrompt(ctx, controller, record.PaneTarget, prompt); err != nil {
		if errors.Is(err, ErrTmuxSessionMissing) {
			return record, true, nil
		}
		return record, false, socketError("UPSTREAM_ERROR", "could not request daily reset handoff", true)
	}
	if ctx.Err() != nil {
		return record, false, nil
	}
	if err := d.waitForTmuxSubmitSettle(ctx); err != nil {
		return record, false, nil
	}
	if err := controller.SendEnter(ctx, record.PaneTarget); err != nil {
		if errors.Is(err, ErrTmuxSessionMissing) {
			return record, true, nil
		}
		return record, false, socketError("UPSTREAM_ERROR", "could not submit daily reset handoff", true)
	}
	if submitErr := d.verifyRuntimeSubmission(ctx, record, submissionBefore, submissionEnabled, "daily reset handoff"); submitErr != nil {
		return record, false, submitErr
	}
	reset.PromptedAt = busTime(now)
	record.DailyReset = &reset
	if err := d.writeSessionRecordLocked(record); err != nil {
		return record, false, socketError("UPSTREAM_ERROR", "could not record daily reset handoff", true)
	}
	return record, false, nil
}

func (d *Daemon) dailyResetLogPath(bot BotRegistryEntry, botletsHome string, date string) (string, error) {
	if bot.BrainRef != nil {
		if brainRoot, err := ValidateBotletsBrainProjection(d.paths, bot); err == nil && brainRoot != "" {
			return filepath.Join(brainRoot, "logs", date+".md"), nil
		}
		if brainRoot, err := ResolveBotletsBrainProjectionHint(d.paths, bot); err == nil && brainRoot != "" {
			return filepath.Join(brainRoot, "logs", date+".md"), nil
		}
	}
	if botletsHome == "" {
		botletsHome = d.botletsHome
	}
	if botletsHome == "" {
		return "", errors.New("missing botlets home")
	}
	botletsHome, err := ResolveBotletsHome(botletsHome)
	if err != nil {
		return "", err
	}
	return filepath.Join(botletsHome, "logs", strings.ReplaceAll(bot.Handle, ".", "_"), bot.Name, date+".md"), nil
}

func formatDailyResetPrompt(bot BotRegistryEntry, record SessionRecord, reset DailyResetRecord, timezone string) string {
	return "Daily reset handoff: write a complete Markdown summary of today's work to " + shellQuote(reset.LogPath) +
		". Include bot handle " + bot.Handle + ", bot name " + bot.Name + ", date " + reset.Date + ", timezone " + timezone +
		", runtime " + record.Runtime + ", session id " + record.SessionID + ", and reset reason " + reset.Reason +
		". After the file is written, run exactly: comment botlets session reset --log-path " + shellQuote(reset.LogPath)
}

func dailyResetPending(record SessionRecord) bool {
	return record.DailyReset != nil && record.DailyReset.State == "requested"
}

func (d *Daemon) sessionAuthDailyResetPendingLocked(req SocketRequest) bool {
	if req.Auth == nil || req.Auth.Mode != "session" || req.Auth.SessionID == nil || req.Auth.SessionGeneration == nil {
		return false
	}
	record, err := ReadSessionRecord(d.paths, *req.Auth.SessionID)
	return err == nil && record.Generation == *req.Auth.SessionGeneration && dailyResetPending(record)
}

func (d *Daemon) completeSessionDailyReset(req SocketRequest) (map[string]any, *SocketError) {
	if req.Auth == nil || req.Auth.Mode != "session" {
		return nil, socketError("FORBIDDEN", "botlets session reset requires managed session auth", false)
	}
	d.sessionMu.Lock()
	defer d.sessionMu.Unlock()
	record, err := VerifySessionCapabilityForResetComplete(d.paths, *req.Auth)
	if err != nil {
		return nil, socketError("FORBIDDEN", "invalid session capability", false)
	}
	if !paramsMatchSession(req.Params, record) {
		return nil, socketError("FORBIDDEN", "session profile mismatch", false)
	}
	if record.DailyReset == nil {
		return nil, socketError("CONFLICT", "no daily reset is pending for this session", false)
	}
	if logPath, ok := req.Params["log_path"].(string); ok && logPath != record.DailyReset.LogPath {
		return nil, socketError("FORBIDDEN", "daily reset log path does not match pending reset", false)
	}
	resetCtx := contextWithSocketRequest(context.Background(), req)
	if record.DailyReset.State == "completed" {
		return d.finishDailyResetLocked(resetCtx, managedSessionResetTarget{}, record, "daily_reset_complete", false)
	}
	if record.DailyReset.State != "requested" && record.DailyReset.State != "replacing" {
		return nil, socketError("CONFLICT", "no daily reset is pending for this session", false)
	}
	if record.DailyReset.State == "requested" {
		if err := verifyDailyResetLog(record.DailyReset.LogPath); err != nil {
			return nil, socketError("CONFLICT", "daily reset log is not ready", false)
		}
	}
	bot, botletsHome, botErr := d.sessionBot(record.Profile, record.BotName)
	if botErr != nil {
		return nil, botErr
	}
	target := managedSessionResetTarget{Profile: record.Profile, BotName: record.BotName, Bot: bot, BotletsHome: botletsHome}
	return d.finishDailyResetLocked(resetCtx, target, record, "daily_reset_complete", false)
}

func verifyDailyResetLog(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return errors.New("daily reset log is empty")
	}
	return nil
}

func (d *Daemon) finishDailyResetLocked(ctx context.Context, target managedSessionResetTarget, record SessionRecord, reason string, fallback bool) (map[string]any, *SocketError) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = contextWithDiagnosticSocketRequest(ctx, internalCloudReleaseSocketRequest("sessions.daily_reset", record.Profile, ""))
	if record.DailyReset == nil {
		return nil, socketError("CONFLICT", "no daily reset is pending for this session", false)
	}
	if record.DailyReset.State == "completed" {
		return map[string]any{
			"ok":                     true,
			"session_id":             record.SessionID,
			"state":                  record.State,
			"log_path":               record.DailyReset.LogPath,
			"replacement_session_id": record.DailyReset.ReplacementSessionID,
			"released_messages":      []string{},
		}, nil
	}
	if record.DailyReset.State != "requested" && record.DailyReset.State != "replacing" {
		return nil, socketError("CONFLICT", "no daily reset is pending for this session", false)
	}
	if record.DailyReset.State == "requested" {
		if fallback {
			if err := writeDailyResetFallbackLog(target.Bot, record, *record.DailyReset, reason, d.currentTime()); err != nil {
				return nil, socketError("UPSTREAM_ERROR", "could not write daily reset fallback log", true)
			}
		} else if err := verifyDailyResetLog(record.DailyReset.LogPath); err != nil {
			return nil, socketError("CONFLICT", "daily reset log is not ready", false)
		}
		previousState := record.State
		live := false
		if record.SessionName != "" {
			var liveErr error
			live, liveErr = d.controllerForSession(record).HasSession(context.Background(), record.SessionName)
			if liveErr != nil {
				live = false
				d.logger.warn("session.daily_reset_host_inspect_failed", sessionLogData(record))
			}
		}
		replacing := *record.DailyReset
		replacing.State = "replacing"
		replacing.CompletedAt = ""
		replacing.ReplacementSessionID = ""
		record.State = "dead"
		record.DailyReset = &replacing
		if err := d.writeSessionRecordLocked(record); err != nil {
			return nil, socketError("UPSTREAM_ERROR", "could not stop resetting session", true)
		}
		if live || (normalizeSessionHost(record.Host) == SessionHostBmux && record.SessionName != "") {
			if killErr := d.controllerForSession(record).KillSession(context.Background(), record.SessionName); killErr != nil {
				if errors.Is(killErr, ErrTmuxSessionMissing) {
					live = false
				} else {
					data := sessionLogData(record)
					data["error"] = killErr.Error()
					d.logger.warn("session.daily_reset_kill_failed", data)
					restore := record
					requested := replacing
					requested.State = "requested"
					requested.CompletedAt = ""
					requested.ReplacementSessionID = ""
					restore.State = previousState
					restore.DailyReset = &requested
					_ = d.writeSessionRecordLocked(restore)
					return nil, socketError("UPSTREAM_ERROR", "could not stop session for daily reset", true)
				}
			}
		}
		releasedMessages, releaseErr := d.releaseSessionClaimsLocked(record, reason)
		if releaseErr != nil {
			return nil, releaseErr
		}
		d.logSessionDead(record, previousState, reason, releasedMessages, live)
		return d.completeDailyResetReplacementLocked(ctx, target, record, reason, fallback, releasedMessages, true)
	}
	return d.completeDailyResetReplacementLocked(ctx, target, record, reason, fallback, nil, false)
}

func (d *Daemon) completeDailyResetReplacementLocked(ctx context.Context, target managedSessionResetTarget, record SessionRecord, reason string, fallback bool, releasedMessages []string, claimsReleased bool) (map[string]any, *SocketError) {
	if !claimsReleased {
		released, releaseErr := d.releaseSessionClaimsLocked(record, reason)
		if releaseErr != nil {
			return nil, releaseErr
		}
		releasedMessages = append(releasedMessages, released...)
	}
	replacement, ok, _, staleReleasedMessages, reconcileErr := d.reconcileLiveSessionForScopeLocked(target.Profile, target.BotName, target.Bot.BotID, botAgentID(target.Bot), "profile", target.Profile, target.Bot.ManagedSession.Runtime, target.Bot.ManagedSession.Model)
	if reconcileErr != nil {
		return nil, reconcileErr
	}
	releasedMessages = append(releasedMessages, staleReleasedMessages...)
	if !ok {
		var createErr *SocketError
		replacement, createErr = d.createAndLaunchSessionLockedWithContext(ctx, target.Profile, target.BotName, "profile", target.Profile, target.Bot, target.BotletsHome, true)
		if createErr != nil {
			return nil, createErr
		}
	}
	if replacement.SessionID == "" {
		return nil, nil
	}
	completed := *record.DailyReset
	completed.State = "completed"
	completed.CompletedAt = busTime(d.currentTime())
	completed.ReplacementSessionID = replacement.SessionID
	record.State = "dead"
	record.DailyReset = &completed
	if err := d.writeSessionRecordLocked(record); err != nil {
		return nil, socketError("UPSTREAM_ERROR", "could not record daily reset replacement", true)
	}
	_, nudgeErr := d.nudgeReadyQueueHeadIfIdleLockedWithContext(ctx, replacement, false)
	if nudgeErr != nil {
		return nil, nudgeErr
	}
	data := sessionLogData(record)
	data["replacement_session_id"] = replacement.SessionID
	data["released_messages"] = len(releasedMessages)
	data["fallback"] = fallback
	data["log_path"] = completed.LogPath
	d.logger.info("session.daily_reset_completed", data)
	return map[string]any{
		"ok":                     true,
		"session_id":             record.SessionID,
		"state":                  record.State,
		"log_path":               completed.LogPath,
		"replacement_session_id": replacement.SessionID,
		"released_messages":      releasedMessages,
	}, nil
}

func writeDailyResetFallbackLog(bot BotRegistryEntry, record SessionRecord, reset DailyResetRecord, reason string, now time.Time) error {
	if info, err := os.Stat(reset.LogPath); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Daily Reset Fallback: %s\n\n", reset.Date)
	fmt.Fprintf(&b, "- Bot handle: %s\n", bot.Handle)
	fmt.Fprintf(&b, "- Bot name: %s\n", bot.Name)
	fmt.Fprintf(&b, "- Date: %s\n", reset.Date)
	fmt.Fprintf(&b, "- Runtime: %s\n", record.Runtime)
	fmt.Fprintf(&b, "- Session ID: %s\n", record.SessionID)
	fmt.Fprintf(&b, "- Reset reason: %s\n", reason)
	fmt.Fprintf(&b, "- Requested at: %s\n", reset.RequestedAt)
	fmt.Fprintf(&b, "- Fallback written at: %s\n\n", busTime(now))
	fmt.Fprintf(&b, "The runtime did not complete the daily reset handoff before the daemon fallback. Existing message claims were released or requeued before the replacement session was launched.\n")
	return WritePrivateFileAtomic(reset.LogPath, []byte(b.String()), 0o600)
}

func (d *Daemon) stopSession(req SocketRequest) (map[string]any, *SocketError) {
	d.sessionMu.Lock()
	defer d.sessionMu.Unlock()
	record, err := d.selectSessionForMutation(req)
	if err != nil {
		return nil, err
	}
	reason, _ := req.Params["reason"].(string)
	if reason == "" {
		reason = "session_stopped"
	}
	live := false
	if record.SessionName != "" {
		var liveErr error
		live, liveErr = d.controllerForSession(record).HasSession(context.Background(), record.SessionName)
		if liveErr != nil {
			return nil, socketError("UPSTREAM_ERROR", "could not inspect session", true)
		}
	}
	previousState := record.State
	record.State = "dead"
	if err := d.writeSessionRecordLocked(record); err != nil {
		return nil, socketError("UPSTREAM_ERROR", "could not stop session", true)
	}
	if live || (normalizeSessionHost(record.Host) == SessionHostBmux && record.SessionName != "") {
		if killErr := d.controllerForSession(record).KillSession(context.Background(), record.SessionName); killErr != nil {
			if errors.Is(killErr, ErrTmuxSessionMissing) {
				live = false
			} else {
				record.State = previousState
				_ = d.writeSessionRecordLocked(record)
				return nil, socketError("UPSTREAM_ERROR", "could not stop session", true)
			}
		}
	}
	releasedMessages, releaseErr := d.releaseSessionClaimsLocked(record, reason)
	if releaseErr != nil {
		return nil, releaseErr
	}
	d.logSessionDead(record, previousState, reason, releasedMessages, live)
	return map[string]any{"ok": true, "session_id": record.SessionID, "state": record.State, "released_messages": releasedMessages}, nil
}

func (d *Daemon) nudgeSession(req SocketRequest) (map[string]any, *SocketError) {
	messageID := req.Params["message_id"].(string)
	d.sessionMu.Lock()
	defer d.sessionMu.Unlock()
	record, err := d.selectSessionForMutation(req)
	if err != nil {
		return nil, err
	}
	if record.State != "alive" {
		return nil, socketError("CONFLICT", "session is not alive", false)
	}
	if dailyResetPending(record) {
		return nil, socketError("CONFLICT", "session is resetting", false)
	}
	now := time.Now().UTC()
	holder := "session:" + record.SessionID + ":" + record.Generation
	authority := authorityForSessionRecord(record)
	stageReq := socketRequestWithMessageID(req, messageID)
	stageReq.Op = "sessions.nudge_preflight"
	d.lockBusForSocketRequest(stageReq)
	message, storeErr := d.getInboxMessageForAuthority(stageReq, authority, messageID, "session.nudge_get_message")
	var activeOther bool
	activeErr := d.runSocketStage(stageReq, "session.nudge_has_active_other", func() error {
		var err error
		activeOther, err = d.store.HasActiveSessionClaimForBotExcluding(context.Background(), holder, record.BotName, record.BotID, record.BotAgentID, messageID, now)
		return err
	})
	d.busMu.Unlock()
	if storeErr != nil {
		return nil, classifyMessageStoreError(storeErr)
	}
	if activeErr != nil {
		return nil, classifyMessageStoreError(activeErr)
	}
	if !messageMatchesAuthorityBot(message, authority) {
		return nil, socketError("FORBIDDEN", "message bot does not match session", false)
	}
	if !messageReceivableForSessionNudge(message, record, activeOther, now) {
		return nil, socketError("CONFLICT", "message is not receivable by session", false)
	}
	updated, nudgeErr := d.sendSessionNudgeLockedWithContext(contextWithSocketRequest(context.Background(), stageReq), record, message)
	if nudgeErr != nil {
		return nil, nudgeErr
	}
	result := map[string]any{
		"ok":           true,
		"message_id":   messageID,
		"session_id":   updated.SessionID,
		"attempted_at": *updated.LastNudge.AttemptedAt,
	}
	if updated.LastNudge.SucceededAt != nil {
		result["succeeded_at"] = *updated.LastNudge.SucceededAt
	}
	return result, nil
}

func messageReceivableForSessionNudge(message MessageEnvelope, record SessionRecord, activeOther bool, now time.Time) bool {
	if activeOther {
		return false
	}
	if shouldWriteMessageSpool(message, now) {
		return true
	}
	if message.Source == "comment.io" {
		return false
	}
	if message.Delivery.State != "claimed" {
		return false
	}
	holder := "session:" + record.SessionID + ":" + record.Generation
	return stringValue(message.Delivery.ClaimHolder) == holder && !leaseExpired(message.Delivery.LeaseExpiresAt, now)
}

func (d *Daemon) currentReceivableMessageForSessionNudge(ctx context.Context, record SessionRecord, messageID string) (MessageEnvelope, *SocketError) {
	d.lockBusForContext(ctx)
	message, storeErr := d.getInboxMessageForContext(ctx, record.Profile, messageID, "nudge.current_get_message")
	if storeErr != nil {
		d.busMu.Unlock()
		return MessageEnvelope{}, classifyMessageStoreError(storeErr)
	}
	if message.Profile != record.Profile || message.BotName != record.BotName {
		d.busMu.Unlock()
		return MessageEnvelope{}, socketError("FORBIDDEN", "message bot does not match session", false)
	}
	now := time.Now().UTC()
	if !messageReceivableForSessionNudge(message, record, false, now) {
		d.busMu.Unlock()
		d.syncMessageSpoolForDelivery(message, "nudge_skip")
		return MessageEnvelope{}, socketError("CONFLICT", "message is not receivable by session", false)
	}
	if message.Source == "comment.io" {
		if metadataErr := d.runSocketStageForContext(ctx, "nudge.current_metadata_read", func() error {
			_, err := ReadPrivateCloudMessageMetadata(d.paths, record.Profile, messageID)
			return err
		}); metadataErr != nil {
			_, err := d.quarantineCloudMessageForMissingMetadata(ctx, record.Profile, messageID, now)
			d.busMu.Unlock()
			if err != nil {
				return MessageEnvelope{}, classifyMessageStoreError(err)
			}
			return MessageEnvelope{}, socketError("UPSTREAM_ERROR", "could not read notification metadata", true)
		}
		if message.Kind == "botlets.task" {
			summary := &MessageWaitSummary{
				MessageID: message.ID,
				Profile:   message.Profile,
				BotName:   message.BotName,
				Kind:      message.Kind,
				Source:    message.Source,
				Refs:      message.Refs,
			}
			if !d.botletsTaskSummaryMatchesCurrentTarget(summary) {
				d.busMu.Unlock()
				d.sessionMu.Unlock()
				_, err := d.quarantineCloudMessageForStaleBotletsTaskTarget(ctx, record.Profile, messageID, now)
				d.lockSessionForContext(ctx)
				if err != nil {
					return MessageEnvelope{}, classifyMessageStoreError(err)
				}
				return MessageEnvelope{}, socketError("CONFLICT", "botlets task target is stale", false)
			}
		}
	}
	d.busMu.Unlock()
	return message, nil
}

func (d *Daemon) refreshMessageSpoolNudgeAfterSend(ctx context.Context, record SessionRecord, messageID string) {
	d.lockBusForContext(ctx)
	defer d.busMu.Unlock()
	message, storeErr := d.getInboxMessageForContext(ctx, record.Profile, messageID, "nudge.refresh_spool_get_message")
	if storeErr != nil || message.Profile != record.Profile || message.BotName != record.BotName {
		return
	}
	if !messageReceivableForSessionNudge(message, record, false, time.Now().UTC()) {
		_ = d.runSocketStageForContext(ctx, "nudge.refresh_spool_remove", func() error {
			return RemoveMessageSpool(d.paths, message.Profile, message.ID)
		})
		return
	}
	if message.Source == "comment.io" {
		if metadataErr := d.runSocketStageForContext(ctx, "nudge.refresh_spool_metadata_read", func() error {
			_, err := ReadPrivateCloudMessageMetadata(d.paths, record.Profile, messageID)
			return err
		}); metadataErr != nil {
			_ = d.runSocketStageForContext(ctx, "nudge.refresh_spool_remove", func() error {
				return RemoveMessageSpool(d.paths, message.Profile, message.ID)
			})
			return
		}
	}
	_ = d.runSocketStageForContext(ctx, "nudge.refresh_spool_update", func() error {
		return UpdateMessageSpoolNudge(d.paths, record, message)
	})
}

func (d *Daemon) sessionStatus(req SocketRequest) ([]SessionRecord, *SocketError) {
	d.sessionMu.Lock()
	defer d.sessionMu.Unlock()
	if req.Auth != nil && req.Auth.Mode == "session" {
		record, err := VerifySessionCapability(d.paths, *req.Auth)
		if err != nil {
			return nil, socketError("FORBIDDEN", "invalid session capability", false)
		}
		record, syncErr := d.syncSessionStatusLivenessLocked(record)
		if syncErr != nil {
			return nil, syncErr
		}
		return filterSessions([]SessionRecord{record}, req.Params), nil
	}
	records, err := d.listSessionRecords()
	if err != nil {
		return nil, socketError("UPSTREAM_ERROR", "could not read sessions", false)
	}
	filtered := filterSessions(records, req.Params)
	for i := range filtered {
		record, syncErr := d.syncSessionStatusLivenessLocked(filtered[i])
		if syncErr != nil {
			return nil, syncErr
		}
		filtered[i] = record
	}
	return filtered, nil
}

func (d *Daemon) syncSessionLivenessLocked(record SessionRecord) (SessionRecord, *SocketError) {
	return d.syncSessionLivenessLockedWithOptions(record, true)
}

func (d *Daemon) syncSessionStatusLivenessLocked(record SessionRecord) (SessionRecord, *SocketError) {
	return d.syncSessionLivenessLockedWithOptions(record, false)
}

func (d *Daemon) syncSessionLivenessLockedWithOptions(record SessionRecord, waitForStartingRuntime bool) (SessionRecord, *SocketError) {
	if record.State != "alive" && record.State != "starting" {
		return record, nil
	}
	var (
		live bool
		err  *SocketError
	)
	if waitForStartingRuntime {
		record, live, err = d.recoverLiveTmuxSessionLocked(record)
	} else {
		record, live, err = d.inspectLiveTmuxSessionLocked(record, true)
	}
	if err != nil {
		return SessionRecord{}, err
	}
	if live {
		if record.State == "starting" && !waitForStartingRuntime {
			running, runtimeReason, runtimeErr := d.sessionRuntimeStatusLocked(record)
			if runtimeErr != nil {
				return SessionRecord{}, runtimeErr
			}
			if runtimeReason != "" {
				_, staleErr := d.markSessionStaleLocked(record, runtimeReason)
				if staleErr != nil {
					return SessionRecord{}, staleErr
				}
				record.State = "stale"
				if runtimeReason == "runtime_untrusted" {
					return record, socketError("UPSTREAM_ERROR", "could not verify session runtime", true)
				}
				return record, nil
			}
			if !running {
				if managedSessionStartupRemaining(record, time.Now()) <= 0 {
					_, staleErr := d.markSessionStaleLocked(record, "runtime_not_running")
					if staleErr != nil {
						return SessionRecord{}, staleErr
					}
					record.State = "stale"
					return record, nil
				}
				return record, nil
			}
		} else {
			runtimeReason, runtimeErr := d.sessionRuntimeIssueLocked(record, record.State == "starting")
			if runtimeErr != nil {
				return SessionRecord{}, runtimeErr
			}
			if runtimeReason != "" {
				_, staleErr := d.markSessionStaleLocked(record, runtimeReason)
				if staleErr != nil {
					return SessionRecord{}, staleErr
				}
				record.State = "stale"
				if runtimeReason == "runtime_untrusted" {
					return record, socketError("UPSTREAM_ERROR", "could not verify session runtime", true)
				}
				return record, nil
			}
		}
		if record.State == "starting" {
			record.State = "alive"
			if err := d.writeSessionRecordLocked(record); err != nil {
				return SessionRecord{}, socketError("UPSTREAM_ERROR", "could not update session status", true)
			}
		}
		return record, nil
	}
	_, staleErr := d.markSessionStaleLocked(record, "tmux_session_missing")
	if staleErr != nil {
		return SessionRecord{}, staleErr
	}
	record.State = "stale"
	return record, nil
}

func (d *Daemon) nudgeStaleReadyMessagesLocked(record SessionRecord, hadStale bool, staleReleasedMessages []string) (bool, *SocketError) {
	ctx := contextWithSocketRequest(context.Background(), internalCloudReleaseSocketRequest("sessions.start_nudge", record.Profile, ""))
	return d.nudgeStaleReadyMessagesLockedWithContext(ctx, record, hadStale, staleReleasedMessages)
}

func (d *Daemon) nudgeStaleReadyMessagesLockedWithContext(ctx context.Context, record SessionRecord, hadStale bool, staleReleasedMessages []string) (bool, *SocketError) {
	_ = hadStale
	if len(staleReleasedMessages) == 0 {
		return false, nil
	}
	return d.nudgeReadyQueueHeadIfIdleLockedWithContext(ctx, record, false)
}

func (d *Daemon) nudgeReadyQueueHeadLocked(record SessionRecord, suppressAlreadyNudged bool) (bool, *SocketError) {
	ctx := contextWithSocketRequest(context.Background(), internalCloudReleaseSocketRequest("sessions.nudge_queue", record.Profile, ""))
	return d.nudgeReadyQueueHeadLockedWithContext(ctx, record, suppressAlreadyNudged)
}

func (d *Daemon) nudgeReadyQueueHeadLockedWithContext(ctx context.Context, record SessionRecord, suppressAlreadyNudged bool) (bool, *SocketError) {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return false, nil
	}
	if dailyResetPending(record) {
		return false, nil
	}
	var summary *MessageWaitSummary
	for {
		nudgeRecords := d.automaticNudgeBlockRecordsLocked()
		d.lockBusForContext(ctx)
		var storeErr error
		summary, storeErr = d.waitReadyMessageSummaryForDispatchLocked(ctx, MessageListFilter{Profile: record.Profile, BotName: record.BotName}, time.Now().UTC(), nudgeRecords)
		d.busMu.Unlock()
		if stale, ok := staleBotletsTaskTargetFromError(storeErr); ok {
			d.sessionMu.Unlock()
			if _, err := d.quarantineCloudMessageForStaleBotletsTaskTarget(ctx, stale.Profile, stale.MessageID, time.Now().UTC()); err != nil {
				d.lockSessionForContext(ctx)
				return false, classifyMessageStoreError(err)
			}
			d.lockSessionForContext(ctx)
			refreshed, ok, refreshErr := d.refreshNudgeSessionRecordLocked(record)
			if refreshErr != nil {
				return false, refreshErr
			}
			if !ok || ctx.Err() != nil || dailyResetPending(refreshed) {
				return false, nil
			}
			record = refreshed
			continue
		}
		if storeErr != nil {
			return false, classifyMessageStoreError(storeErr)
		}
		if summary == nil {
			return false, nil
		}
		break
	}
	if ctx.Err() != nil {
		return false, nil
	}
	if suppressAlreadyNudged && maybeLastSuccessfulNudgeForMessage(record, summary.MessageID) {
		liveRecord, live, liveErr := d.recoverLiveTmuxSessionLocked(record)
		if liveErr != nil {
			return false, liveErr
		}
		if live {
			record = liveRecord
			if lastSuccessfulNudgeMatches(record, summary.MessageID) {
				if runtimeErr := d.verifySessionRuntimeForNudgeLocked(record); runtimeErr != nil {
					return false, runtimeErr
				}
				return true, nil
			}
		}
	}
	d.lockBusForContext(ctx)
	message, storeErr := d.getInboxMessageForContext(ctx, record.Profile, summary.MessageID, "nudge.queue_get_message")
	d.busMu.Unlock()
	if storeErr != nil {
		return false, classifyMessageStoreError(storeErr)
	}
	if ctx.Err() != nil {
		return false, nil
	}
	if _, nudgeErr := d.sendSessionNudgeLockedWithContext(ctx, record, message); nudgeErr != nil {
		return false, nudgeErr
	}
	return true, nil
}

func (d *Daemon) nudgeReadyQueueHeadIfIdleLocked(record SessionRecord, suppressAlreadyNudged bool) (bool, *SocketError) {
	ctx := contextWithSocketRequest(context.Background(), internalCloudReleaseSocketRequest("sessions.nudge_queue", record.Profile, ""))
	return d.nudgeReadyQueueHeadIfIdleLockedWithContext(ctx, record, suppressAlreadyNudged)
}

func (d *Daemon) nudgeReadyQueueHeadIfIdleLockedWithContext(ctx context.Context, record SessionRecord, suppressAlreadyNudged bool) (bool, *SocketError) {
	active, err := d.sessionHasActiveClaimLocked(ctx, record)
	if err != nil {
		return false, err
	}
	if active {
		return false, nil
	}
	if dailyResetPending(record) {
		return false, nil
	}
	nudged, nudgeErr := d.nudgeReadyQueueHeadLockedWithContext(ctx, record, suppressAlreadyNudged)
	if isSessionIdleConflict(nudgeErr) {
		return false, nil
	}
	return nudged, nudgeErr
}

func isSessionIdleConflict(err *SocketError) bool {
	return err != nil &&
		err.Code == "CONFLICT" &&
		(err.Message == "session pane is busy" || err.Message == "runtime session file is not ready")
}

func (d *Daemon) sessionHasActiveClaimLocked(ctx context.Context, record SessionRecord) (bool, *SocketError) {
	holder := "session:" + record.SessionID + ":" + record.Generation
	d.lockBusForContext(ctx)
	var active bool
	stage := socketStageTracker{}
	if req, ok := socketRequestFromContext(ctx); ok {
		stage = d.startSocketStage(req, "nudge.has_active_claim")
	}
	active, storeErr := d.store.HasActiveSessionClaimForBot(context.Background(), holder, record.BotName, record.BotID, record.BotAgentID, time.Now().UTC())
	d.busMu.Unlock()
	if storeErr != nil {
		if stage.stop != nil {
			stage.failed("store_error")
		}
		return false, classifyMessageStoreError(storeErr)
	}
	if stage.stop != nil {
		stage.done()
	}
	return active, nil
}

func (d *Daemon) refreshNudgeSessionRecordLocked(record SessionRecord) (SessionRecord, bool, *SocketError) {
	refreshed, err := ReadSessionRecord(d.paths, record.SessionID)
	if err != nil {
		return SessionRecord{}, false, socketError("UPSTREAM_ERROR", "could not read session", true)
	}
	if refreshed.State != "alive" || refreshed.Generation != record.Generation {
		return refreshed, false, nil
	}
	if dailyResetPending(refreshed) {
		return refreshed, false, nil
	}
	return refreshed, true, nil
}

func (d *Daemon) nudgeNextReadyForSessionAuthority(req SocketRequest, authority messageAuthority) *SocketError {
	if authority.SessionID == nil || authority.SessionGeneration == nil {
		return nil
	}
	ctx := contextWithSocketRequest(context.Background(), req)
	d.lockSessionForContext(ctx)
	defer d.sessionMu.Unlock()
	record, err := ReadSessionRecord(d.paths, *authority.SessionID)
	if err != nil {
		return nil
	}
	if record.State != "alive" || record.Generation != *authority.SessionGeneration {
		return nil
	}
	if dailyResetPending(record) {
		return nil
	}
	_, nudgeErr := d.nudgeReadyQueueHeadIfIdleLockedWithContext(ctx, record, false)
	return nudgeErr
}

func (d *Daemon) sendSessionNudgeLocked(record SessionRecord, message MessageEnvelope) (SessionRecord, *SocketError) {
	ctx := contextWithSocketRequest(context.Background(), internalCloudReleaseSocketRequest("sessions.nudge_send", record.Profile, message.ID))
	return d.sendSessionNudgeLockedWithContext(ctx, record, message)
}

func (d *Daemon) sendSessionNudgeLockedWithContext(ctx context.Context, record SessionRecord, message MessageEnvelope) (SessionRecord, *SocketError) {
	if ctx == nil {
		ctx = context.Background()
	}
	messageID := message.ID
	automatic := automaticNudgeContext(ctx)
	if ctx.Err() != nil {
		return record, nil
	}
	if record.State != "alive" {
		d.logNudgeFailed(record, messageID, "preflight", "CONFLICT")
		return SessionRecord{}, socketError("CONFLICT", "session is not alive", false)
	}
	if dailyResetPending(record) {
		d.logNudgeFailed(record, messageID, "preflight", "CONFLICT")
		return SessionRecord{}, socketError("CONFLICT", "session is resetting", false)
	}
	if messageID == "" {
		return SessionRecord{}, socketError("VALIDATION_ERROR", "invalid message id", false)
	}
	message, currentErr := d.currentReceivableMessageForSessionNudge(ctx, record, messageID)
	if currentErr != nil {
		d.logNudgeFailed(record, messageID, "preflight", currentErr.Code)
		return SessionRecord{}, currentErr
	}
	record, live, liveErr := d.recoverLiveTmuxSessionLocked(record)
	if liveErr != nil {
		d.logNudgeFailed(record, messageID, "liveness", liveErr.Code)
		return SessionRecord{}, liveErr
	}
	if !live {
		if _, staleErr := d.markSessionStaleLocked(record, "tmux_session_missing"); staleErr != nil {
			d.logNudgeFailed(record, messageID, "liveness", staleErr.Code)
			return SessionRecord{}, staleErr
		}
		d.logNudgeFailed(record, messageID, "liveness", "CONFLICT")
		return SessionRecord{}, socketError("CONFLICT", "session is not running", false)
	}
	if runtimeErr := d.verifySessionRuntimeForNudgeLocked(record); runtimeErr != nil {
		d.logNudgeFailed(record, messageID, "runtime", runtimeErr.Code)
		return SessionRecord{}, runtimeErr
	}
	message, currentErr = d.currentReceivableMessageForSessionNudge(ctx, record, messageID)
	if currentErr != nil {
		d.logNudgeFailed(record, messageID, "pre_send", currentErr.Code)
		return SessionRecord{}, currentErr
	}
	// asyncRewake: a Claude Code session pulls its own messages via
	// `comment messages wait --rewake` instead of being typed into by bmux. When
	// such a session has a live pull-waiter registered, skip the bmux keystroke
	// entirely: record a clean nudge success and leave the ready message in the
	// local queue for the waiter to claim. We deliberately do NOT enter the
	// automatic backoff/stuck branch and do NOT mark the message failed. Codex
	// and other runtimes, and Claude sessions with no waiter, fall through to the
	// normal bmux path below.
	if record.Runtime == "claude" && d.listeners.hasPullWaiter(record.Profile, record.SessionID, record.Generation) {
		var skipClaimGeneration *string
		if message.Source == "comment.io" {
			if metadata, metadataErr := ReadPrivateCloudMessageMetadata(d.paths, message.Profile, messageID); metadataErr == nil {
				skipClaimGeneration = cloudHandlingClaimGeneration(metadata)
			}
		}
		nudgedAt := busTime(time.Now().UTC())
		record.LastNudge = LastNudgeRecord{
			MessageID:       &messageID,
			PaneTarget:      &record.PaneTarget,
			AttemptedAt:     &nudgedAt,
			SucceededAt:     &nudgedAt,
			ClaimGeneration: skipClaimGeneration,
		}
		clearAutomaticNudgeState(&record, messageID, skipClaimGeneration)
		if writeErr := d.writeSessionRecordLocked(record); writeErr != nil {
			d.logNudgeFailed(record, messageID, "record_async_rewake", "UPSTREAM_ERROR")
			return SessionRecord{}, socketError("UPSTREAM_ERROR", "could not record nudge", true)
		}
		d.logNudgeSkippedAsyncRewake(record, messageID)
		return record, nil
	}
	nudgeText, err := formatTmuxNudge(record.BotName, messageID)
	if err != nil {
		d.logNudgeFailed(record, messageID, "format", "VALIDATION_ERROR")
		return SessionRecord{}, socketError("VALIDATION_ERROR", "invalid nudge text", false)
	}
	var claimGeneration *string
	if message.Source == "comment.io" {
		if metadata, metadataErr := ReadPrivateCloudMessageMetadata(d.paths, message.Profile, messageID); metadataErr == nil {
			claimGeneration = cloudHandlingClaimGeneration(metadata)
		}
	}
	previousAutomaticNudge := automaticNudgeState(record, messageID, claimGeneration)
	attemptedAt := busTime(time.Now().UTC())
	record.LastNudge = LastNudgeRecord{
		MessageID:       &messageID,
		PaneTarget:      &record.PaneTarget,
		AttemptedAt:     &attemptedAt,
		ClaimGeneration: claimGeneration,
	}
	if writeErr := d.writeSessionRecordLocked(record); writeErr != nil {
		d.logNudgeFailed(record, messageID, "record_attempt", "UPSTREAM_ERROR")
		return SessionRecord{}, socketError("UPSTREAM_ERROR", "could not record nudge", true)
	}
	if ctx.Err() != nil {
		return record, nil
	}
	d.logNudgeAttempted(record, messageID)
	unlock := d.tmuxNudgeLocks.lock(record.SessionName)
	defer unlock()
	controller := d.controllerForSession(record)
	submissionBefore, submissionEnabled, submissionErr := d.beginRuntimeSubmissionCheck(record)
	if submissionErr != nil {
		d.logNudgeFailed(record, messageID, "session_file", submissionErr.Code)
		return SessionRecord{}, submissionErr
	}
	sendStage, sendErr := d.sendPrompt(ctx, controller, record.PaneTarget, nudgeText)
	if sendErr != nil {
		if errors.Is(sendErr, ErrTmuxSessionMissing) {
			if _, staleErr := d.markSessionStaleLocked(record, "tmux_session_missing"); staleErr != nil {
				d.logNudgeFailed(record, messageID, sendStage, staleErr.Code)
				return SessionRecord{}, staleErr
			}
			d.logNudgeFailed(record, messageID, sendStage, "CONFLICT")
			return SessionRecord{}, socketError("CONFLICT", "session is not running", false)
		}
		d.logNudgeFailed(record, messageID, sendStage, "UPSTREAM_ERROR")
		return SessionRecord{}, socketError("UPSTREAM_ERROR", "could not nudge session", true)
	}
	if ctx.Err() != nil {
		return record, nil
	}
	if err := d.waitForTmuxSubmitSettle(ctx); err != nil {
		return record, nil
	}
	if err := controller.SendEnter(ctx, record.PaneTarget); err != nil {
		if errors.Is(err, ErrTmuxSessionMissing) {
			if _, staleErr := d.markSessionStaleLocked(record, "tmux_session_missing"); staleErr != nil {
				d.logNudgeFailed(record, messageID, "send_enter", staleErr.Code)
				return SessionRecord{}, staleErr
			}
			d.logNudgeFailed(record, messageID, "send_enter", "CONFLICT")
			return SessionRecord{}, socketError("CONFLICT", "session is not running", false)
		}
		d.logNudgeFailed(record, messageID, "send_enter", "UPSTREAM_ERROR")
		return SessionRecord{}, socketError("UPSTREAM_ERROR", "could not submit session nudge", true)
	}
	if submitErr := d.verifyRuntimeSubmission(ctx, record, submissionBefore, submissionEnabled, "session nudge"); submitErr != nil {
		d.logNudgeFailed(record, messageID, "session_file", submitErr.Code)
		return SessionRecord{}, submitErr
	}
	succeededAt := busTime(time.Now().UTC())
	record.LastNudge.SucceededAt = &succeededAt
	if automatic {
		record.LastNudge.AttemptCount = nextAutomaticNudgeAttempt(previousAutomaticNudge, messageID, claimGeneration)
		nextEligibleAt, stuck := nextAutomaticNudgeDeadline(record.LastNudge.AttemptCount, time.Now().UTC())
		if stuck {
			record.LastNudge.Stuck = true
			record.LastNudge.FailureReason = "automatic_nudge_attempt_cap"
			record.LastNudge.NextEligibleAt = nil
		} else {
			nextEligible := busTime(nextEligibleAt)
			record.LastNudge.NextEligibleAt = &nextEligible
		}
		setAutomaticNudgeState(&record, record.LastNudge)
	} else {
		record.LastNudge.AttemptCount = 0
		record.LastNudge.NextEligibleAt = nil
		record.LastNudge.FailureReason = ""
		record.LastNudge.Stuck = false
		clearAutomaticNudgeState(&record, messageID, claimGeneration)
	}
	if writeErr := d.writeSessionRecordLocked(record); writeErr != nil {
		d.logNudgeFailed(record, messageID, "record_success", "UPSTREAM_ERROR")
		return SessionRecord{}, socketError("UPSTREAM_ERROR", "could not record nudge", true)
	}
	d.refreshMessageSpoolNudgeAfterSend(ctx, record, messageID)
	d.logNudgeSucceeded(record, messageID)
	if message.Source == "comment.io" {
		if record.LastNudge.Stuck {
			d.publishCloudHandlingClearBestEffortAsyncForProfiles(record.Profile, message.Profile, messageID, "failed")
		} else if !automatic || !sameSuccessfulNudgeGeneration(previousAutomaticNudge, record.LastNudge, messageID) {
			d.publishCloudHandlingStartBestEffortAsyncForProfiles(record.Profile, message.Profile, messageID)
		}
	}
	return record, nil
}

func (d *Daemon) sendTmuxPrompt(ctx context.Context, paneTarget string, text string) (string, error) {
	return d.sendPrompt(ctx, d.tmux, paneTarget, text)
}

func (d *Daemon) sendPrompt(ctx context.Context, controller TmuxController, paneTarget string, text string) (string, error) {
	if strings.ContainsAny(text, "\r\n") {
		if ctx.Err() != nil {
			return "paste_text", nil
		}
		if err := controller.PasteText(ctx, paneTarget, text); err != nil {
			return "paste_text", err
		}
		return "paste_text", nil
	}
	for _, chunk := range chunkTmuxText(text, tmuxNudgeChunkSize) {
		if ctx.Err() != nil {
			return "send_literal", nil
		}
		if err := controller.SendLiteral(ctx, paneTarget, chunk); err != nil {
			return "send_literal", err
		}
	}
	return "send_literal", nil
}

// claudeFolderTrustPromptPresent reports whether the captured pane text shows
// Claude Code's "Do you trust this folder?" gate. Claude shows this modal
// before the composer on first launch in a directory it has not trusted yet.
func claudeFolderTrustPromptPresent(text string) bool {
	lower := strings.ToLower(text)
	if strings.Contains(lower, "trust this folder") {
		return true
	}
	// Fallback on the menu choices in case the heading wording changes.
	return strings.Contains(lower, "yes, i trust") && strings.Contains(lower, "no, exit")
}

// acceptClaudeFolderTrustPrompt dismisses Claude Code's folder-trust gate if it
// is on screen before we send the startup instruction. If the gate is up when
// we paste the orientation and press Enter, that Enter is consumed accepting
// the dialog (option 1, "Yes, I trust this folder", is preselected) and the
// orientation is never submitted. We accept the gate once, then wait for the
// composer to replace it — pressing Enter exactly once so we never submit a
// stray empty prompt. Best-effort: if the pane can't be read we proceed and let
// the normal send path surface any real session errors.
func (d *Daemon) acceptClaudeFolderTrustPrompt(ctx context.Context, paneTarget string) {
	if ctx == nil {
		ctx = context.Background()
	}
	text, err := d.tmux.CapturePane(ctx, paneTarget, 80)
	if err != nil || !claudeFolderTrustPromptPresent(text) {
		return
	}
	if err := d.tmux.SendEnter(ctx, paneTarget); err != nil {
		return
	}
	deadline := time.Now().Add(claudeTrustPromptMaxWait)
	for time.Now().Before(deadline) {
		if !sleepWithContext(ctx, claudeTrustPromptPoll) {
			return
		}
		text, err := d.tmux.CapturePane(ctx, paneTarget, 80)
		if err != nil {
			continue
		}
		if !claudeFolderTrustPromptPresent(text) {
			return
		}
	}
}

func (d *Daemon) sendTmuxStartupInstruction(ctx context.Context, sessionName string, paneTarget string, baseURL string, handle string, bot BotRegistryEntry) *SocketError {
	record := SessionRecord{Host: SessionHostTmux, SessionName: sessionName, PaneTarget: paneTarget}
	_, err := d.sendSessionStartupInstruction(ctx, record, baseURL, handle, bot)
	return err
}

func (d *Daemon) sendSessionStartupInstruction(ctx context.Context, record SessionRecord, baseURL string, handle string, bot BotRegistryEntry) (SessionRecord, *SocketError) {
	unlock := d.tmuxNudgeLocks.lock(record.SessionName)
	defer unlock()
	controller := d.controllerForSession(record)
	// Claude Code shows a "Do you trust this folder?" gate before the composer on
	// first launch. tmux can inspect and clear it from rendered screen text; bmux
	// deliberately has no emulator, so the bmux path uses a small blind Enter
	// sequence before pasting orientation. Claude's empty composer ignores these
	// Enters after the gate is gone.
	if normalizeSessionHost(record.Host) == SessionHostTmux {
		d.acceptClaudeFolderTrustPrompt(ctx, record.PaneTarget)
	} else {
		if readyErr := d.waitForBmuxStartupInputReady(ctx, record); readyErr != nil {
			return record, readyErr
		}
		d.clearClaudeStartupGateBlind(ctx, controller, record.PaneTarget)
	}
	if ctx.Err() != nil {
		return record, socketError("CANCELED", "startup instruction canceled before submit", false)
	}
	codexCorrelation, codexCorrelationEnabled, codexCorrelationErr := d.beginCodexRolloutCorrelation(record)
	if codexCorrelationErr != nil {
		return record, codexCorrelationErr
	}
	submissionBefore, submissionEnabled, submissionErr := d.beginRuntimeSubmissionCheck(record)
	if submissionErr != nil {
		return record, submissionErr
	}
	// Prefer the richer multi-line Botlets orientation when the bot has a
	// validated brain projection. PasteText commits the text into the pane
	// before we send Enter; if PasteText errors out we fall back cleanly to
	// the single-line builder. After a successful Paste we MUST NOT fall
	// through — orphan text would mix with the fallback prompt — so the
	// Enter-step error is propagated as-is.
	if bot.BrainRef != nil {
		if sent, sendErr := d.sendBotletsMultilineOrientationWithController(ctx, controller, record.PaneTarget, handle, bot, codexCorrelation.Marker()); sent {
			if sendErr != nil {
				return record, sendErr
			}
			return d.verifyStartupSubmission(ctx, record, submissionBefore, submissionEnabled, codexCorrelation, codexCorrelationEnabled)
		}
	}
	instruction, err := d.buildStartupInstructionForBot(baseURL, handle, bot)
	if err != nil {
		d.logger.warn("runtime.startup_instruction.skipped", map[string]any{
			"session_name": record.SessionName,
			"handle":       handle,
			"bot_name":     bot.Name,
			"error":        err.Error(),
		})
		return record, socketError("VALIDATION_ERROR", "invalid startup instruction", false)
	}
	instruction = appendCodexRolloutMarker(instruction, codexCorrelation.Marker())
	if _, err := d.sendPrompt(ctx, controller, record.PaneTarget, instruction); err != nil {
		if errors.Is(err, ErrTmuxSessionMissing) {
			return record, socketError("CONFLICT", "session is not running", false)
		}
		return record, socketError("UPSTREAM_ERROR", "could not send startup instruction", true)
	}
	if ctx.Err() != nil {
		// Prompt was typed but Enter wasn't sent — mirror the multiline
		// orientation path: returning nil here would claim "started
		// successfully" when in reality the bot never saw the prompt
		// submitted. Surface the cancel.
		return record, socketError("CANCELED", "startup instruction canceled before submit", false)
	}
	if err := d.waitForTmuxSubmitSettle(ctx); err != nil {
		return record, socketError("CANCELED", "startup instruction canceled before submit", false)
	}
	if err := controller.SendEnter(ctx, record.PaneTarget); err != nil {
		if errors.Is(err, ErrTmuxSessionMissing) {
			return record, socketError("CONFLICT", "session is not running", false)
		}
		return record, socketError("UPSTREAM_ERROR", "could not submit startup instruction", true)
	}
	return d.verifyStartupSubmission(ctx, record, submissionBefore, submissionEnabled, codexCorrelation, codexCorrelationEnabled)
}

func (d *Daemon) clearClaudeStartupGateBlind(ctx context.Context, controller TmuxController, paneTarget string) {
	for i := 0; i < 3; i++ {
		if ctx.Err() != nil {
			return
		}
		_ = controller.SendEnter(ctx, paneTarget)
		if !sleepWithContext(ctx, 300*time.Millisecond) {
			return
		}
	}
}

type bmuxOutputWaiter interface {
	WaitForOutput(ctx context.Context, sessionName string, needle []byte, timeout time.Duration) (bool, error)
}

func (d *Daemon) waitForBmuxStartupInputReady(ctx context.Context, record SessionRecord) *SocketError {
	waiter, ok := d.controllerForSession(record).(bmuxOutputWaiter)
	data := sessionLogData(record)
	if !ok {
		d.logger.warn("runtime.startup_ready_marker_unavailable", data)
		return nil
	}
	ready, err := waiter.WaitForOutput(ctx, record.SessionName, []byte("\x1b[?2004h"), transientRuntimeStartupInputReadyWait)
	if err != nil {
		data["error"] = err.Error()
		d.logger.warn("runtime.startup_ready_marker_failed", data)
		if errors.Is(err, ErrTmuxSessionMissing) {
			return socketError("CONFLICT", "session is not running", false)
		}
		if errors.Is(err, context.Canceled) {
			return socketError("CANCELED", "startup instruction canceled before submit", false)
		}
		return nil
	}
	if ready {
		d.logger.info("runtime.startup_ready_marker_observed", data)
		return nil
	}
	d.logger.warn("runtime.startup_ready_marker_timeout", data)
	return nil
}

type codexRolloutCorrelation struct {
	Nonce      string
	UserHome   string
	WorkingDir string
}

func (c codexRolloutCorrelation) Marker() string {
	if c.Nonce == "" {
		return ""
	}
	return "comment_io_codex_rollout_nonce=" + c.Nonce
}

func appendCodexRolloutMarker(text string, marker string) string {
	text = strings.TrimRight(text, "\r\n")
	if marker == "" {
		return text
	}
	return text + "\n\n" + marker
}

func (d *Daemon) beginCodexRolloutCorrelation(record SessionRecord) (codexRolloutCorrelation, bool, *SocketError) {
	if !isCodexBmuxSession(record) {
		return codexRolloutCorrelation{}, false, nil
	}
	nonce, err := GenerateLocalID("op", 20)
	if err != nil {
		return codexRolloutCorrelation{}, false, socketError("UPSTREAM_ERROR", "could not allocate Codex rollout nonce", true)
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return codexRolloutCorrelation{}, false, socketError("UPSTREAM_ERROR", "could not inspect Codex home", true)
	}
	workingDir := record.WorkingDir
	if workingDir == "" {
		workingDir = d.managedSessionWorkingDir(record)
	}
	return codexRolloutCorrelation{
		Nonce:      nonce,
		UserHome:   userHome,
		WorkingDir: workingDir,
	}, true, nil
}

func (d *Daemon) beginRuntimeSubmissionCheck(record SessionRecord) (SessionFileSnapshot, bool, *SocketError) {
	if !isClaudeBmuxSession(record) {
		return SessionFileSnapshot{}, false, nil
	}
	snapshot, err := d.claudeSessionFileSnapshot(record)
	if err != nil {
		data := sessionLogData(record)
		data["error"] = err.Error()
		d.logger.warn("runtime.session_file_snapshot_failed", data)
		return SessionFileSnapshot{}, false, socketError("UPSTREAM_ERROR", "could not inspect runtime session file", true)
	}
	return snapshot, true, nil
}

func (d *Daemon) verifyStartupSubmission(ctx context.Context, record SessionRecord, before SessionFileSnapshot, submissionEnabled bool, codexCorrelation codexRolloutCorrelation, codexCorrelationEnabled bool) (SessionRecord, *SocketError) {
	if submitErr := d.verifyRuntimeSubmission(ctx, record, before, submissionEnabled, "startup instruction"); submitErr != nil {
		return record, submitErr
	}
	if !codexCorrelationEnabled {
		return record, nil
	}
	return d.verifyCodexRolloutCorrelation(ctx, record, codexCorrelation, "startup instruction")
}

func (d *Daemon) verifyRuntimeSubmission(ctx context.Context, record SessionRecord, before SessionFileSnapshot, enabled bool, action string) *SocketError {
	if !enabled {
		return nil
	}
	ok, err := WaitForSessionFileGrowth(ctx, before, transientRuntimeStartupInputReadyWait, transientRuntimeStartupInputReadyPoll)
	data := sessionLogData(record)
	data["session_file"] = before.Path
	data["action"] = action
	if err != nil {
		data["error"] = err.Error()
		d.logger.warn("runtime.session_file_submit_failed", data)
		return socketError("UPSTREAM_ERROR", "could not verify runtime received "+action, true)
	}
	if !ok {
		d.logger.warn("runtime.session_file_submit_timeout", data)
		return socketError("UPSTREAM_ERROR", "runtime did not record "+action, true)
	}
	d.logger.info("runtime.session_file_submit_observed", data)
	return nil
}

func (d *Daemon) verifyCodexRolloutCorrelation(ctx context.Context, record SessionRecord, correlation codexRolloutCorrelation, action string) (SessionRecord, *SocketError) {
	match, ok, err := WaitForCodexRolloutCorrelation(ctx, correlation.UserHome, correlation.WorkingDir, correlation.Nonce, transientRuntimeStartupInputReadyWait, transientRuntimeStartupInputReadyPoll)
	data := sessionLogData(record)
	data["action"] = action
	data["working_dir"] = correlation.WorkingDir
	if err != nil {
		data["error"] = err.Error()
		d.logger.warn("runtime.codex_rollout_correlation_failed", data)
		return record, socketError("UPSTREAM_ERROR", "could not inspect Codex rollout", true)
	}
	if !ok {
		d.logger.warn("runtime.codex_rollout_correlation_timeout", data)
		return record, nil
	}
	record.RuntimeSessionRef = match.SessionID
	if writeErr := d.writeSessionRecordLocked(record); writeErr != nil {
		return record, socketError("UPSTREAM_ERROR", "could not record Codex session id", true)
	}
	data["runtime_session_ref"] = match.SessionID
	data["rollout_path"] = match.Path
	d.logger.info("runtime.codex_rollout_correlated", data)
	return record, nil
}

func (d *Daemon) verifyRuntimeSessionFileIdle(record SessionRecord) *SocketError {
	if !isClaudeBmuxSession(record) {
		return nil
	}
	snapshot, err := d.claudeSessionFileSnapshot(record)
	if err != nil {
		data := sessionLogData(record)
		data["error"] = err.Error()
		d.logger.warn("runtime.session_file_idle_failed", data)
		return socketError("UPSTREAM_ERROR", "could not inspect runtime session file", true)
	}
	if !snapshot.Exists {
		return socketError("CONFLICT", "runtime session file is not ready", false)
	}
	if time.Since(snapshot.ModTime) < transientRuntimeStartupInputQuietDelay {
		return socketError("CONFLICT", "session pane is busy", false)
	}
	return nil
}

func (d *Daemon) claudeSessionFileSnapshot(record SessionRecord) (SessionFileSnapshot, error) {
	userHome, err := os.UserHomeDir()
	if err != nil {
		return SessionFileSnapshot{}, err
	}
	workingDir := record.WorkingDir
	if workingDir == "" {
		workingDir = d.managedSessionWorkingDir(record)
	}
	return ReadClaudeSessionFileSnapshot(userHome, workingDir, record.RuntimeSessionRef)
}

func isClaudeBmuxSession(record SessionRecord) bool {
	return normalizeSessionHost(record.Host) == SessionHostBmux && record.Runtime == "claude" && record.RuntimeSessionRef != ""
}

func isCodexBmuxSession(record SessionRecord) bool {
	return normalizeSessionHost(record.Host) == SessionHostBmux && record.Runtime == "codex"
}

// sendBotletsMultilineOrientation pastes the multi-line
// BuildBotletsSetupOrientation prompt into the pane via tmux paste-buffer
// and sends Enter to submit. Returns:
//   - sent=false: nothing landed in the pane; caller must fall back to the
//     single-line prompt path.
//   - sent=true, err=nil: orientation was pasted and submitted successfully.
//   - sent=true, err non-nil: orientation was pasted but submitting Enter
//     failed; caller must NOT fall back (orphan text already in the pane).
func (d *Daemon) sendBotletsMultilineOrientation(ctx context.Context, paneTarget string, handle string, bot BotRegistryEntry) (bool, *SocketError) {
	return d.sendBotletsMultilineOrientationWithController(ctx, d.tmux, paneTarget, handle, bot, "")
}

func (d *Daemon) sendBotletsMultilineOrientationWithController(ctx context.Context, controller TmuxController, paneTarget string, handle string, bot BotRegistryEntry, extraMarker string) (bool, *SocketError) {
	// Escape hatch: a terminal that does NOT advertise bracketed-paste
	// support will see tmux replace newlines with CR (Enter), which would
	// submit each line as a separate prompt. If that's ever observed in
	// the wild, set COMMENT_BUS_BOTLETS_MULTILINE_ORIENTATION=0 to drop
	// back to the single-line builder without redeploying.
	if os.Getenv("COMMENT_BUS_BOTLETS_MULTILINE_ORIENTATION") == "0" {
		return false, nil
	}
	brainRoot, err := ValidateBotletsBrainProjection(d.paths, bot)
	bootstrapProbeError := ""
	if err != nil || brainRoot == "" {
		if err != nil {
			bootstrapProbeError = err.Error()
		} else {
			bootstrapProbeError = "brain projection is not currently readable"
		}
		hintRoot, hintErr := ResolveBotletsBrainProjectionHint(d.paths, bot)
		if hintErr != nil || hintRoot == "" {
			return false, nil
		}
		brainRoot = hintRoot
	}
	hasBootstrap := false
	if bootstrapProbeError == "" {
		var bootstrapErr error
		hasBootstrap, bootstrapErr = BotletsBootstrapPresent(brainRoot)
		if bootstrapErr != nil {
			bootstrapProbeError = bootstrapErr.Error()
		}
	}
	docsRoot, _ := localSyncOrientationPaths(d.paths)
	baseURL := ""
	if profile, ok := d.cloudNotificationProfile(handle); ok {
		baseURL = profile.BaseURL
	}
	body, err := BuildBotletsSetupOrientation(BotletsSetupOrientationInput{
		BotName:             bot.Name,
		BotDisplayName:      bot.DisplayName,
		BotHandle:           handle,
		BrainRoot:           brainRoot,
		BaseURL:             baseURL,
		DocsRoot:            docsRoot,
		HasBootstrap:        hasBootstrap,
		BootstrapProbeError: bootstrapProbeError,
	})
	if err != nil {
		d.logger.warn("runtime.startup_instruction.botlets_multiline_build_failed", map[string]any{
			"handle":   handle,
			"bot_name": bot.Name,
			"error":    err.Error(),
		})
		return false, nil
	}
	body = appendCodexRolloutMarker(body, extraMarker)
	if err := controller.PasteText(ctx, paneTarget, body); err != nil {
		d.logger.warn("runtime.startup_instruction.botlets_multiline_paste_failed", map[string]any{
			"handle":   handle,
			"bot_name": bot.Name,
			"error":    err.Error(),
		})
		return false, nil
	}
	if err := d.waitForTmuxSubmitSettle(ctx); err != nil {
		return true, socketError("CANCELED", "botlets orientation canceled before submit", false)
	}
	if ctx.Err() != nil {
		// Text was pasted but ctx was canceled before we sent Enter. Per the
		// godoc contract above, sent=true,nil means "submitted successfully"
		// — that would be a lie. Surface the cancel so callers don't mark
		// the run as oriented.
		return true, socketError("CANCELED", "botlets orientation canceled before submit", false)
	}
	if err := controller.SendEnter(ctx, paneTarget); err != nil {
		if errors.Is(err, ErrTmuxSessionMissing) {
			return true, socketError("CONFLICT", "session is not running", false)
		}
		return true, socketError("UPSTREAM_ERROR", "could not submit botlets orientation", true)
	}
	return true, nil
}

// buildStartupInstructionForBot picks the correct single-line tmux startup
// instruction for the bot. Botlets bots (with a BrainRef that matches local
// sync placement metadata) get a botlets-tailored prompt that names the brain
// root and startup files, even if the final brain directory has not materialized
// yet; everything else gets the generic Comment.io prompt. Both inline resolved
// COMMENT_IO_LOCAL_SYNC_ROOT and COMMENT_IO_LOCAL_DOCS_ROOT values when they
// are available so the agent sees absolute paths instead of literal env-var
// names.
func (d *Daemon) buildStartupInstructionForBot(baseURL string, handle string, bot BotRegistryEntry) (string, error) {
	docsRoot, syncRoot := localSyncOrientationPaths(d.paths)
	paths := startupOrientationPaths{DocsRoot: docsRoot, SyncRoot: syncRoot}
	if bot.BrainRef != nil {
		if brainRoot, err := ResolveBotletsBrainProjectionHint(d.paths, bot); err == nil && brainRoot != "" {
			botletsPaths := paths
			botletsPaths.BrainRoot = brainRoot
			text, err := formatBotletsTmuxStartupInstruction(baseURL, bot.Name, bot.DisplayName, handle, botletsPaths)
			if err == nil {
				return text, nil
			}
			// Botlets prompt failed validation (most likely the extra
			// brain-root + startup-file list pushed past the 512-byte cap
			// even with the docs-root URL fallback). Don't drop orientation
			// entirely — fall through to the generic Comment.io prompt,
			// which has its own paths→URL fallback chain.
			d.logger.warn("runtime.startup_instruction.botlets_fallback", map[string]any{
				"handle":   handle,
				"bot_name": bot.Name,
				"error":    err.Error(),
			})
		}
	}
	return formatTmuxStartupInstruction(baseURL, handle, paths)
}

func (d *Daemon) verifySessionRuntimeForNudgeLocked(record SessionRecord) *SocketError {
	if record.PaneTarget == "" {
		return socketError("CONFLICT", "session pane is not available", false)
	}
	if runtimeErr := d.verifySessionRuntimeTrustLocked(record); runtimeErr != nil {
		return runtimeErr
	}
	currentCommand, err := d.controllerForSession(record).PaneCurrentCommand(context.Background(), record.PaneTarget)
	if err != nil {
		if errors.Is(err, ErrTmuxSessionMissing) {
			if _, staleErr := d.markSessionStaleLocked(record, "tmux_session_missing"); staleErr != nil {
				return staleErr
			}
			return socketError("CONFLICT", "session is not running", false)
		}
		return socketError("UPSTREAM_ERROR", "could not inspect session foreground command", true)
	}
	if sessionPaneRunsExpectedRuntime(record, currentCommand) {
		return d.verifyRuntimeSessionFileIdle(record)
	}
	return socketError("CONFLICT", "session pane is busy", false)
}

func (d *Daemon) verifySessionRuntimeReadyForPrompt(ctx context.Context, record SessionRecord) (bool, *SocketError) {
	if record.PaneTarget == "" {
		return true, nil
	}
	if err := sessionRuntimeResolvable(record); err != nil {
		return false, socketError("UPSTREAM_ERROR", "could not verify session runtime", true)
	}
	currentCommand, err := d.controllerForSession(record).PaneCurrentCommand(ctx, record.PaneTarget)
	if err != nil {
		if errors.Is(err, ErrTmuxSessionMissing) {
			return true, nil
		}
		return false, socketError("UPSTREAM_ERROR", "could not inspect session foreground command", true)
	}
	if !sessionPaneRunsExpectedRuntime(record, currentCommand) {
		return false, socketError("CONFLICT", "session pane is busy", false)
	}
	if idleErr := d.verifyRuntimeSessionFileIdle(record); idleErr != nil {
		return false, idleErr
	}
	return false, nil
}

func (d *Daemon) verifySessionRuntimeTrustLocked(record SessionRecord) *SocketError {
	if err := sessionRuntimeResolvable(record); err != nil {
		if _, staleErr := d.markSessionStaleLocked(record, "runtime_untrusted"); staleErr != nil {
			return staleErr
		}
		return socketError("UPSTREAM_ERROR", "could not verify session runtime", true)
	}
	return nil
}

func (d *Daemon) sessionRuntimeStatusLocked(record SessionRecord) (bool, string, *SocketError) {
	if record.PaneTarget == "" {
		return false, "tmux_session_missing", nil
	}
	if err := sessionRuntimeResolvable(record); err != nil {
		return false, "runtime_untrusted", nil
	}
	currentCommand, err := d.controllerForSession(record).PaneCurrentCommand(context.Background(), record.PaneTarget)
	if err != nil {
		if errors.Is(err, ErrTmuxSessionMissing) {
			return false, "tmux_session_missing", nil
		}
		return false, "", socketError("UPSTREAM_ERROR", "could not inspect session foreground command", true)
	}
	return sessionPaneRunsExpectedRuntime(record, currentCommand), "", nil
}

func (d *Daemon) sessionRuntimeIssueLocked(record SessionRecord, waitForRuntime bool) (string, *SocketError) {
	timeout := sessionRuntimeStartupTimeout
	now := time.Now()
	if waitForRuntime {
		timeout = managedSessionStartupRemaining(record, now)
	}
	deadline := now.Add(timeout)
	for {
		running, runtimeReason, runtimeErr := d.sessionRuntimeStatusLocked(record)
		if runtimeErr != nil {
			return "", runtimeErr
		}
		if runtimeReason != "" {
			return runtimeReason, nil
		}
		if running {
			return "", nil
		}
		if !waitForRuntime {
			return "", nil
		}
		if !time.Now().Before(deadline) {
			return "runtime_not_running", nil
		}
		time.Sleep(sessionRuntimeStartupPoll)
	}
}

func managedSessionStartupRemaining(record SessionRecord, now time.Time) time.Duration {
	if now.IsZero() {
		now = time.Now()
	}
	startedAtText := record.StartupStartedAt
	if startedAtText == "" {
		startedAtText = record.CreatedAt
	}
	startedAt, err := time.Parse(time.RFC3339Nano, startedAtText)
	if err != nil {
		return managedSessionRuntimeStartupTimeout
	}
	elapsed := now.Sub(startedAt)
	if elapsed <= 0 {
		return managedSessionRuntimeStartupTimeout
	}
	if elapsed >= managedSessionRuntimeStartupTimeout {
		return 0
	}
	return managedSessionRuntimeStartupTimeout - elapsed
}

func (d *Daemon) waitForSessionRuntimeLocked(record SessionRecord, timeout time.Duration) *SocketError {
	return d.waitForSessionRuntimeLockedWithContext(context.Background(), record, timeout)
}

func (d *Daemon) waitForSessionRuntimeLockedWithContext(ctx context.Context, record SessionRecord, timeout time.Duration) *SocketError {
	if ctx == nil {
		ctx = context.Background()
	}
	if record.PaneTarget == "" {
		return socketError("CONFLICT", "session pane is not available", false)
	}
	if err := sessionRuntimeResolvable(record); err != nil {
		return socketError("UPSTREAM_ERROR", "could not verify session runtime", true)
	}
	deadline := time.Now().Add(timeout)
	inspectFailed := false
	for {
		if ctx.Err() != nil {
			return socketError("CONFLICT", "session startup canceled", false)
		}
		currentCommand, err := d.controllerForSession(record).PaneCurrentCommand(ctx, record.PaneTarget)
		if err != nil {
			if errors.Is(err, ErrTmuxSessionMissing) {
				return socketError("CONFLICT", "session is not running", false)
			}
			inspectFailed = true
		} else {
			inspectFailed = false
			if sessionPaneRunsExpectedRuntime(record, currentCommand) {
				return nil
			}
		}
		if !time.Now().Before(deadline) {
			if inspectFailed {
				return socketError("UPSTREAM_ERROR", "could not inspect session foreground command", true)
			}
			return socketError("UPSTREAM_ERROR", "session runtime did not start", true)
		}
		if !sleepWithContext(ctx, sessionRuntimeStartupPoll) {
			return socketError("CONFLICT", "session startup canceled", false)
		}
	}
}

func sessionPaneRunsExpectedRuntime(record SessionRecord, currentCommand string) bool {
	if currentCommand == "" {
		return false
	}
	if normalizeRuntimeLaunchMode(record.RuntimeLaunchMode) == RuntimeLaunchModeShell {
		// The pane runs ONLY what our `<shell> -ilc` script launched: the runtime
		// name, or the binary/alias/function it resolves to. tmux reports the
		// foreground process, and the `-ilc` shell exits when the runtime exits,
		// so a non-shell foreground means the runtime is running; the login shell
		// is foreground only momentarily while rc loads (treated as not-yet).
		return !isLoginShellCommandName(currentCommand)
	}
	expected := expectedSessionRuntimeCommandNames(record)
	_, ok := expected[currentCommand]
	return ok
}

func maybeLastSuccessfulNudgeForMessage(record SessionRecord, messageID string) bool {
	return record.LastNudge.MessageID != nil &&
		*record.LastNudge.MessageID == messageID &&
		record.LastNudge.SucceededAt != nil
}

func lastSuccessfulNudgeMatches(record SessionRecord, messageID string) bool {
	return maybeLastSuccessfulNudgeForMessage(record, messageID) &&
		record.LastNudge.PaneTarget != nil &&
		record.PaneTarget != "" &&
		*record.LastNudge.PaneTarget == record.PaneTarget
}

func automaticNudgeContext(ctx context.Context) bool {
	if req, ok := socketRequestFromContext(ctx); ok {
		return req.Op != "sessions.nudge" && req.Op != "sessions.nudge_preflight"
	}
	return true
}

func automaticNudgeStateKey(messageID string, claimGeneration *string) string {
	if claimGeneration == nil || *claimGeneration == "" {
		return messageID
	}
	return messageID + "|" + *claimGeneration
}

func automaticNudgeState(record SessionRecord, messageID string, claimGeneration *string) LastNudgeRecord {
	if record.AutomaticNudges != nil {
		if state, ok := record.AutomaticNudges[automaticNudgeStateKey(messageID, claimGeneration)]; ok {
			return state
		}
	}
	if record.LastNudge.MessageID != nil &&
		*record.LastNudge.MessageID == messageID &&
		sameClaimGeneration(record.LastNudge.ClaimGeneration, claimGeneration) &&
		(record.LastNudge.AttemptCount > 0 || record.LastNudge.NextEligibleAt != nil || record.LastNudge.Stuck) {
		return record.LastNudge
	}
	return LastNudgeRecord{}
}

func setAutomaticNudgeState(record *SessionRecord, nudge LastNudgeRecord) {
	if record == nil || nudge.MessageID == nil || *nudge.MessageID == "" {
		return
	}
	if record.AutomaticNudges == nil {
		record.AutomaticNudges = map[string]LastNudgeRecord{}
	}
	record.AutomaticNudges[automaticNudgeStateKey(*nudge.MessageID, nudge.ClaimGeneration)] = nudge
	pruneAutomaticNudgeStates(record)
}

func clearAutomaticNudgeState(record *SessionRecord, messageID string, claimGeneration *string) {
	if record == nil || record.AutomaticNudges == nil {
		return
	}
	delete(record.AutomaticNudges, automaticNudgeStateKey(messageID, claimGeneration))
	if len(record.AutomaticNudges) == 0 {
		record.AutomaticNudges = nil
	}
}

func pruneAutomaticNudgeStates(record *SessionRecord) {
	if record == nil || len(record.AutomaticNudges) <= automaticNudgeStateLimit {
		return
	}
	type keyedState struct {
		key       string
		timestamp string
	}
	states := make([]keyedState, 0, len(record.AutomaticNudges))
	for key, state := range record.AutomaticNudges {
		timestamp := ""
		if state.AttemptedAt != nil {
			timestamp = *state.AttemptedAt
		}
		if state.SucceededAt != nil && *state.SucceededAt > timestamp {
			timestamp = *state.SucceededAt
		}
		states = append(states, keyedState{key: key, timestamp: timestamp})
	}
	sort.Slice(states, func(i, j int) bool {
		if states[i].timestamp == states[j].timestamp {
			return states[i].key < states[j].key
		}
		return states[i].timestamp < states[j].timestamp
	})
	for len(record.AutomaticNudges) > automaticNudgeStateLimit && len(states) > 0 {
		delete(record.AutomaticNudges, states[0].key)
		states = states[1:]
	}
}

func nextAutomaticNudgeAttempt(previous LastNudgeRecord, messageID string, claimGeneration *string) int {
	if previous.MessageID == nil || *previous.MessageID != messageID {
		return 1
	}
	if !sameClaimGeneration(previous.ClaimGeneration, claimGeneration) {
		return 1
	}
	return previous.AttemptCount + 1
}

func nextAutomaticNudgeDeadline(attempt int, now time.Time) (time.Time, bool) {
	if attempt >= automaticNudgeMaxAttempts {
		return time.Time{}, true
	}
	if attempt <= 0 {
		attempt = 1
	}
	backoff := automaticNudgeInitialBackoff
	for i := 1; i < attempt; i++ {
		backoff *= 2
		if backoff >= automaticNudgeMaxBackoff {
			backoff = automaticNudgeMaxBackoff
			break
		}
	}
	return now.UTC().Add(backoff), false
}

func sameSuccessfulNudgeGeneration(previous LastNudgeRecord, current LastNudgeRecord, messageID string) bool {
	if previous.MessageID == nil || *previous.MessageID != messageID || previous.SucceededAt == nil {
		return false
	}
	return sameClaimGeneration(previous.ClaimGeneration, current.ClaimGeneration)
}

func sameClaimGeneration(a *string, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func cloudHandlingClaimGeneration(metadata PrivateCloudMessageMetadata) *string {
	if metadata.ClaimedAt != "" {
		return &metadata.ClaimedAt
	}
	if metadata.ClaimID != "" {
		value := "claim:" + metadata.ClaimID
		return &value
	}
	return nil
}

func expectedSessionRuntimeCommandNames(record SessionRecord) map[string]struct{} {
	expected := map[string]struct{}{}
	add := func(name string) {
		if isSafePaneCommandName(name) {
			expected[name] = struct{}{}
		}
	}
	if len(record.RuntimeCommand) > 0 && record.RuntimeCommand[0] != "" {
		add(filepath.Base(record.RuntimeCommand[0]))
	}
	if record.RuntimeCommandPath != "" {
		add(filepath.Base(record.RuntimeCommandPath))
	}
	if record.RuntimePath != "" {
		add(filepath.Base(record.RuntimePath))
		for _, name := range runtimeScriptCommandNames(record.RuntimePath) {
			add(name)
		}
	}
	return expected
}

func runtimeScriptCommandNames(runtimePath string) []string {
	file, err := os.Open(runtimePath)
	if err != nil {
		return nil
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 16*1024))
	if err != nil {
		return nil
	}
	text := string(data)
	line := text
	if newline := strings.IndexByte(line, '\n'); newline >= 0 {
		line = line[:newline]
	}
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "#!") {
		return nil
	}
	fields := strings.Fields(strings.TrimSpace(strings.TrimPrefix(line, "#!")))
	if len(fields) == 0 {
		return nil
	}
	interpreter := filepath.Base(fields[0])
	if interpreter == "env" {
		names := []string{interpreter}
		target := envCommandName(fields[1:])
		if target != "" {
			if isShellInterpreter(target) {
				if execNames := shellWrapperExecCommandNames(text); len(execNames) > 0 {
					return execNames
				}
			}
			names = append(names, target)
		}
		return names
	}
	if isShellInterpreter(interpreter) {
		if names := shellWrapperExecCommandNames(text); len(names) > 0 {
			return names
		}
	}
	return []string{interpreter}
}

func shellWrapperExecCommandNames(text string) []string {
	seen := map[string]struct{}{}
	names := []string{}
	scanner := bufio.NewScanner(strings.NewReader(text))
	scanner.Buffer(make([]byte, 0, 1024), 64*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		tokens := splitShellFields(line)
		for i, token := range tokens {
			if trimShellToken(token) != "exec" {
				continue
			}
			if name := shellExecCommandName(tokens[i+1:]); isSafePaneCommandName(name) {
				if _, ok := seen[name]; !ok {
					seen[name] = struct{}{}
					names = append(names, name)
				}
			}
			break
		}
	}
	return names
}

func shellExecCommandName(tokens []string) string {
	for i := 0; i < len(tokens); i++ {
		token := tokens[i]
		token = trimShellToken(token)
		if token == "" || strings.Contains(token, "=") {
			continue
		}
		if strings.HasPrefix(token, "-") {
			if token == "-a" && i+1 < len(tokens) {
				i++
			}
			continue
		}
		if strings.HasPrefix(token, "$") && !strings.ContainsRune(token, filepath.Separator) {
			continue
		}
		name := filepath.Base(token)
		if name == "env" {
			if target := envCommandName(tokens[i+1:]); target != "" {
				return target
			}
		}
		return name
	}
	return ""
}

func envCommandName(tokens []string) string {
	for i := 0; i < len(tokens); i++ {
		token := trimShellToken(tokens[i])
		if token == "" {
			continue
		}
		if strings.Contains(token, "=") && !strings.HasPrefix(token, "-") {
			continue
		}
		switch {
		case token == "-a" || token == "--argv0" || token == "-C" || token == "--chdir":
			if i+1 < len(tokens) {
				i++
			}
			continue
		case strings.HasPrefix(token, "-a") || strings.HasPrefix(token, "--argv0=") || strings.HasPrefix(token, "-C") || strings.HasPrefix(token, "--chdir="):
			continue
		case token == "-u" || token == "--unset":
			if i+1 < len(tokens) {
				i++
			}
			continue
		case strings.HasPrefix(token, "-u") || strings.HasPrefix(token, "--unset="):
			continue
		case token == "-S" || token == "--split-string":
			if i+1 < len(tokens) {
				splitTokens := splitShellFields(trimShellToken(tokens[i+1]))
				if target := envCommandName(splitTokens); target != "" {
					return target
				}
				if target := envCommandName(tokens[i+1:]); target != "" {
					return target
				}
			}
			continue
		case strings.HasPrefix(token, "--split-string="):
			splitTokens := splitShellFields(trimShellToken(strings.TrimPrefix(token, "--split-string=")))
			if target := envCommandName(splitTokens); target != "" {
				return target
			}
			continue
		case strings.HasPrefix(token, "-S"):
			splitSource := strings.TrimSpace(strings.TrimPrefix(token, "-S") + " " + strings.Join(tokens[i+1:], " "))
			splitTokens := splitShellFields(trimShellToken(splitSource))
			if target := envCommandName(splitTokens); target != "" {
				return target
			}
			continue
		case strings.HasPrefix(token, "-"):
			continue
		}
		if strings.HasPrefix(token, "$") && !strings.ContainsRune(token, filepath.Separator) {
			continue
		}
		return filepath.Base(token)
	}
	return ""
}

func trimShellToken(token string) string {
	return strings.Trim(token, " \t'\"`;(){}")
}

func splitShellFields(line string) []string {
	var fields []string
	var b strings.Builder
	var quote rune
	escaped := false
	for _, r := range line {
		if escaped {
			b.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
				continue
			}
			b.WriteRune(r)
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			continue
		}
		if r == ' ' || r == '\t' {
			if b.Len() > 0 {
				fields = append(fields, b.String())
				b.Reset()
			}
			continue
		}
		b.WriteRune(r)
	}
	if escaped {
		b.WriteRune('\\')
	}
	if b.Len() > 0 {
		fields = append(fields, b.String())
	}
	return fields
}

func isShellInterpreter(name string) bool {
	switch name {
	case "sh", "bash", "zsh", "dash", "ksh":
		return true
	default:
		return false
	}
}

func isSafePaneCommandName(name string) bool {
	return name != "" && len(name) <= 128 && !strings.ContainsAny(name, "/\\\r\n\x00") && !containsSecretValue(name)
}

func (d *Daemon) selectSessionForMutation(req SocketRequest) (SessionRecord, *SocketError) {
	if req.Auth != nil && req.Auth.Mode == "session" {
		record, err := VerifySessionCapability(d.paths, *req.Auth)
		if err != nil {
			return SessionRecord{}, socketError("FORBIDDEN", "invalid session capability", false)
		}
		if !paramsMatchSession(req.Params, record) {
			return SessionRecord{}, socketError("FORBIDDEN", "session profile mismatch", false)
		}
		return record, nil
	}
	if !hasSessionSelector(req.Params) {
		return SessionRecord{}, socketError("VALIDATION_ERROR", "session operation requires a session selector", false)
	}
	// Mutating session operations must fail closed: lenient reads are for
	// status/liveness paths only, because dropping one malformed record can hide
	// an ambiguous or unauthorized mutation target.
	records, err := ListSessionRecords(d.paths)
	if err != nil {
		return SessionRecord{}, socketError("UPSTREAM_ERROR", "could not read sessions", false)
	}
	matches := filterSessions(records, req.Params)
	if len(matches) == 0 {
		return SessionRecord{}, socketError("NOT_FOUND", "session not found", false)
	}
	if req.Auth != nil && req.Auth.Mode == "owner" && req.Auth.Profile != nil {
		profileMatches := filterSessionsByProfile(matches, *req.Auth.Profile)
		if len(profileMatches) == 0 {
			return SessionRecord{}, socketError("FORBIDDEN", "owner profile does not match session", false)
		}
		matches = profileMatches
	}
	if _, hasSessionID := req.Params["session_id"].(string); !hasSessionID {
		alive := filterAliveSessions(matches)
		if len(alive) > 0 {
			matches = alive
		}
	}
	if len(matches) > 1 {
		return SessionRecord{}, socketError("CONFLICT", "session selector is ambiguous", false)
	}
	return matches[0], nil
}

func filterSessions(records []SessionRecord, params map[string]any) []SessionRecord {
	bot, hasBot := params["bot"].(string)
	profile, hasProfile := params["profile"].(string)
	sessionID, hasSessionID := params["session_id"].(string)
	out := make([]SessionRecord, 0, len(records))
	for _, record := range records {
		if hasBot && record.BotName != bot {
			continue
		}
		if hasProfile && record.Profile != profile {
			continue
		}
		if hasSessionID && record.SessionID != sessionID {
			continue
		}
		out = append(out, record)
	}
	return out
}

func filterSessionsByProfile(records []SessionRecord, profile string) []SessionRecord {
	out := make([]SessionRecord, 0, len(records))
	for _, record := range records {
		if record.Profile == profile {
			out = append(out, record)
		}
	}
	return out
}

func filterAliveSessions(records []SessionRecord) []SessionRecord {
	out := make([]SessionRecord, 0, len(records))
	for _, record := range records {
		if record.State == "alive" {
			out = append(out, record)
		}
	}
	return out
}

func paramsMatchSession(params map[string]any, record SessionRecord) bool {
	if bot, ok := params["bot"].(string); ok && bot != record.BotName {
		return false
	}
	if profile, ok := params["profile"].(string); ok && profile != record.Profile {
		return false
	}
	if sessionID, ok := params["session_id"].(string); ok && sessionID != record.SessionID {
		return false
	}
	return true
}

func hasSessionSelector(params map[string]any) bool {
	for _, key := range []string{"bot", "profile", "session_id"} {
		if _, ok := params[key].(string); ok {
			return true
		}
	}
	return false
}

type messageAuthority struct {
	Profile           string
	BotName           string
	BotID             string
	BotAgentID        string
	ClaimHolder       string
	Holder            string
	SessionID         *string
	SessionScopeType  *string
	SessionScopeID    *string
	SessionGeneration *string
}

func (d *Daemon) sendMessages(req SocketRequest) (LocalMessageSendResult, *SocketError) {
	unlock, lockErr := d.lockSessionAuthIfNeeded(req)
	if lockErr != nil {
		return LocalMessageSendResult{}, lockErr
	}
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()
	sender, err := d.resolveSenderAuthority(req)
	if err != nil {
		return LocalMessageSendResult{}, err
	}
	recipients, err := d.resolveRecipients(req.Params["to"].([]any))
	if err != nil {
		return LocalMessageSendResult{}, err
	}
	body := req.Params["body"].(map[string]any)
	content := body["content"].(string)
	refs := map[string]any{}
	if raw, ok := req.Params["refs"].(map[string]any); ok {
		refs = raw
	}
	var threadID *string
	if raw, ok := req.Params["thread_id"].(string); ok && raw != "" {
		threadID = &raw
	}
	idempotencyKey, _ := req.Params["idempotency_key"].(string)
	if idempotencyKey == "" {
		var idErr error
		idempotencyKey, idErr = GenerateLocalID("op", 0)
		if idErr != nil {
			return LocalMessageSendResult{}, socketError("UPSTREAM_ERROR", "could not allocate outbox id", true)
		}
	}
	d.lockBusForSocketRequest(req)
	var result LocalMessageSendResult
	storeErr := d.runSocketStage(req, "local.send_messages", func() error {
		var err error
		result, err = d.store.InsertLocalMessages(context.Background(), LocalMessageSend{
			SenderProfile:    sender.Profile,
			SenderBotName:    sender.BotName,
			SenderBotID:      sender.BotID,
			SenderBotAgentID: sender.BotAgentID,
			Recipients:       recipients,
			Body:             MessageBody{Format: "markdown", Content: content},
			Refs:             refs,
			ThreadID:         threadID,
			IdempotencyKey:   idempotencyKey,
			Now:              time.Now().UTC(),
		})
		return err
	})
	d.busMu.Unlock()
	if storeErr != nil {
		return LocalMessageSendResult{}, classifyMessageStoreError(storeErr)
	}
	for _, message := range result.Messages {
		d.logLocalBusWrite(message, result.OutboxID, sender, result.Replayed)
	}
	unlock()
	unlock = nil
	result.DispatchErrors = append(result.DispatchErrors, d.writeMessageSpoolForSendResult(req, result.Messages)...)
	result.DispatchErrors = append(result.DispatchErrors, d.dispatchLocalBusMessages(contextWithSocketRequest(context.Background(), req), result.Messages, result.Replayed)...)
	return result, nil
}

func (d *Daemon) writeMessageSpoolForSendResult(req SocketRequest, messages []MessageEnvelope) []MessageDispatchError {
	var out []MessageDispatchError
	for _, message := range messages {
		spoolReq := socketRequestWithMessageID(req, message.ID)
		d.lockBusForSocketRequest(spoolReq)
		inbox, storeErr := d.getInboxMessageForSocketRequest(spoolReq, message.Profile, message.ID, "local.send_spool_get_message")
		if storeErr != nil {
			d.busMu.Unlock()
			out = append(out, MessageDispatchError{
				MessageID: message.ID,
				Profile:   message.Profile,
				Code:      "SPOOL_ERROR",
				Message:   "could not read message for spool",
				Retryable: true,
			})
			continue
		}
		stageName := "local.send_spool_remove"
		spoolFn := func() error {
			return RemoveMessageSpool(d.paths, inbox.Profile, inbox.ID)
		}
		if shouldWriteMessageSpool(inbox, time.Now().UTC()) {
			stageName = "local.send_spool_write"
			spoolFn = func() error {
				return WriteMessageSpool(d.paths, inbox)
			}
		}
		spoolErr := d.runSocketStage(spoolReq, stageName, spoolFn)
		d.busMu.Unlock()
		if spoolErr != nil {
			out = append(out, MessageDispatchError{
				MessageID: message.ID,
				Profile:   message.Profile,
				Code:      "SPOOL_ERROR",
				Message:   "could not write message spool",
				Retryable: true,
			})
		}
	}
	return out
}

func (d *Daemon) writeMessageSpoolForSendResultWithoutRequest(messages []MessageEnvelope) []MessageDispatchError {
	req := internalCloudReleaseSocketRequest("messages.send", "", "")
	return d.writeMessageSpoolForSendResult(req, messages)
}

type managedBotTarget struct {
	Profile string
	BotName string
}

const staleBotletsTaskTargetReleaseReason = "botlets_task_target_stale"

var errStaleBotletsTaskReleasePending = errors.New("stale botlets task release pending")

type staleBotletsTaskTargetError struct {
	Profile   string
	MessageID string
}

func (e staleBotletsTaskTargetError) Error() string {
	return "stale botlets task target"
}

func staleBotletsTaskTargetFromError(err error) (staleBotletsTaskTargetError, bool) {
	var stale staleBotletsTaskTargetError
	if errors.As(err, &stale) {
		return stale, true
	}
	return staleBotletsTaskTargetError{}, false
}

func (d *Daemon) dispatchReadyQueueHeads(ctx context.Context) []MessageDispatchError {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return nil
	}
	d.profileMu.RLock()
	targets := make([]managedBotTarget, 0, len(d.profileState.BotRegistry))
	for botName, bot := range d.profileState.BotRegistry {
		if bot.ManagedSession.Enabled && bot.Handle != "" {
			targets = append(targets, managedBotTarget{Profile: bot.Handle, BotName: botName})
		}
	}
	d.profileMu.RUnlock()

	var out []MessageDispatchError
	for _, target := range targets {
		if ctx.Err() != nil {
			return out
		}
		messageID, err := d.dispatchReadyQueueHeadWithContext(ctx, target.Profile, target.BotName)
		if err != nil {
			out = append(out, MessageDispatchError{
				MessageID: messageID,
				Profile:   target.Profile,
				Code:      err.Code,
				Message:   err.Message,
				Retryable: err.Retryable,
			})
		}
	}
	return out
}

func (d *Daemon) dispatchReadyQueueHead(profile string, botName string) (string, *SocketError) {
	return d.dispatchReadyQueueHeadWithContext(context.Background(), profile, botName)
}

func (d *Daemon) dispatchReadyQueueHeadWithContext(ctx context.Context, profile string, botName string) (string, *SocketError) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = contextWithDiagnosticSocketRequest(ctx, internalCloudReleaseSocketRequest("messages.dispatch_ready_queue", profile, ""))
	if ctx.Err() != nil {
		return "", nil
	}
	var summary *MessageWaitSummary
	for {
		nudgeRecords := d.automaticNudgeBlockRecords()
		d.lockBusForContext(ctx)
		var storeErr error
		summary, storeErr = d.waitReadyMessageSummaryForDispatchLocked(ctx, MessageListFilter{Profile: profile, BotName: botName}, time.Now().UTC(), nudgeRecords)
		d.busMu.Unlock()
		if stale, ok := staleBotletsTaskTargetFromError(storeErr); ok {
			if _, err := d.quarantineCloudMessageForStaleBotletsTaskTarget(ctx, stale.Profile, stale.MessageID, time.Now().UTC()); err != nil {
				return "", classifyMessageStoreError(err)
			}
			continue
		}
		if storeErr != nil {
			return "", classifyMessageStoreError(storeErr)
		}
		if summary == nil {
			return "", nil
		}
		break
	}
	if ctx.Err() != nil {
		return summary.MessageID, nil
	}
	d.sessionMu.Lock()
	defer d.sessionMu.Unlock()
	if ctx.Err() != nil {
		return summary.MessageID, nil
	}
	record, managed, readyWorkNudged, err := d.ensureManagedSessionLockedWithContext(ctx, profile, botName, "")
	if err != nil {
		return summary.MessageID, err
	}
	if !managed || readyWorkNudged {
		return summary.MessageID, nil
	}
	if ctx.Err() != nil {
		return summary.MessageID, nil
	}
	if runtimeErr := d.verifySessionRuntimeTrustLocked(record); runtimeErr != nil {
		return summary.MessageID, runtimeErr
	}
	if ctx.Err() != nil {
		return summary.MessageID, nil
	}
	_, nudgeErr := d.nudgeReadyQueueHeadIfIdleLockedWithContext(ctx, record, false)
	return summary.MessageID, nudgeErr
}

func (d *Daemon) dispatchLocalBusMessages(ctx context.Context, messages []MessageEnvelope, suppressAlreadyNudged bool) []MessageDispatchError {
	var out []MessageDispatchError
	for _, message := range messages {
		messageCtx := ctx
		if req, ok := socketRequestFromContext(ctx); ok {
			messageCtx = contextWithSocketRequest(ctx, socketRequestWithMessageID(req, message.ID))
		}
		if err := d.dispatchLocalBusMessage(messageCtx, message, suppressAlreadyNudged); err != nil {
			out = append(out, MessageDispatchError{
				MessageID: message.ID,
				Profile:   message.Profile,
				Code:      err.Code,
				Message:   err.Message,
				Retryable: err.Retryable,
			})
		}
	}
	return out
}

func (d *Daemon) dispatchLocalBusMessage(ctx context.Context, message MessageEnvelope, suppressAlreadyNudged bool) *SocketError {
	ctx = contextWithDiagnosticSocketRequest(ctx, internalCloudReleaseSocketRequest("messages.dispatch_ready_queue", message.Profile, message.ID))
	d.lockSessionForContext(ctx)
	defer d.sessionMu.Unlock()
	record, managed, readyWorkNudged, err := d.ensureManagedSessionLockedWithContext(ctx, message.Profile, message.BotName, message.ID)
	if err != nil {
		return err
	}
	if !managed {
		return nil
	}
	if runtimeErr := d.verifySessionRuntimeTrustLocked(record); runtimeErr != nil {
		return runtimeErr
	}
	active, activeErr := d.sessionHasActiveClaimLocked(ctx, record)
	if activeErr != nil {
		return activeErr
	}
	if active {
		return nil
	}
	if readyWorkNudged {
		return nil
	}
	_, nudgeErr := d.nudgeReadyQueueHeadIfIdleLockedWithContext(ctx, record, suppressAlreadyNudged)
	return nudgeErr
}

// pullWaiterIdentity derives the (profile, session_id, generation) identity to
// register a rewake pull-waiter under. A session-auth caller's identity comes
// from its verified auth triple (so it cannot spoof another session via params);
// an owner-auth caller (impromptu free-handle listen) supplies the optional
// triple through params and otherwise registers a profile-scoped waiter.
func pullWaiterIdentity(req SocketRequest, filter MessageListFilter) (profile, sessionID, generation string) {
	profile = filter.Profile
	if req.Auth != nil && req.Auth.Mode == "session" {
		if req.Auth.Profile != nil && profile == "" {
			profile = *req.Auth.Profile
		}
		if req.Auth.SessionID != nil {
			sessionID = *req.Auth.SessionID
		}
		if req.Auth.SessionGeneration != nil {
			generation = *req.Auth.SessionGeneration
		}
	}
	if sessionID == "" {
		sessionID, _ = req.Params["session_id"].(string)
	}
	if generation == "" {
		generation, _ = req.Params["session_generation"].(string)
	}
	return profile, sessionID, generation
}

// receiveParamsFromWait builds the messages.receive params for the atomic claim a
// rewake messages.wait performs in the same request: the message id plus the
// caller's bot/profile (the auth triple is carried on the cloned request).
func receiveParamsFromWait(req SocketRequest, messageID string) map[string]any {
	params := map[string]any{"message_id": messageID}
	if bot, ok := req.Params["bot"].(string); ok && bot != "" {
		params["bot"] = bot
	}
	if profile, ok := req.Params["profile"].(string); ok && profile != "" {
		params["profile"] = profile
	}
	return params
}

// rewakeOwnershipLost reports whether a rewake waiter is no longer entitled to a
// message it just claimed: either the requesting connection dropped, or (for an
// impromptu listen) the listen claim is no longer held by this listen session.
// Used to release a message claimed in the brief window during which the
// concurrent receive ran.
func (d *Daemon) rewakeOwnershipLost(ctx context.Context, req SocketRequest, listenSession string) bool {
	if ctx != nil && ctx.Err() != nil {
		return true
	}
	if listenSession != "" {
		waitProfile, _ := req.Params["profile"].(string)
		if claim, ok := d.listeners.claimFor(waitProfile); !ok || claim.ClaimedBy != listenSession {
			return true
		}
	}
	return false
}

// releaseRewakeMessage returns a message claimed by an atomic rewake wait back to
// the queue when ownership was lost mid-claim, so it re-dispatches to the
// rightful listener (or cold-starts) instead of sitting on a dead lease until it
// expires. Best-effort: a release failure just falls back to lease expiry.
func (d *Daemon) releaseRewakeMessage(req SocketRequest, message MessageEnvelope) {
	releaseReq := req
	releaseReq.Op = "messages.release"
	params := receiveParamsFromWait(req, message.ID)
	params["reason"] = "rewake_claim_lost"
	releaseReq.Params = params
	if _, relErr := d.releaseMessage(releaseReq); relErr != nil {
		d.logger.warn("rewake.release_after_claim_lost_failed", map[string]any{
			"message_id": message.ID,
			"profile":    message.Profile,
			"error_code": relErr.Code,
		})
	}
}

// reDispatchAfterRewakeLoss re-dispatches a profile's ready queue head after a
// rewake waiter lost its claim, so a managed session is re-nudged (the waiter is
// already deregistered, so this no longer hits the async-rewake skip) or a free
// handle's rightful listener/cold-start picks the message up — instead of the
// message sitting ready until the next poll. A fresh context is used because the
// request's connection context is often cancelled (the reason the claim was lost).
func (d *Daemon) reDispatchAfterRewakeLoss(req SocketRequest) {
	filter, err := d.messageListFilter(req)
	if err != nil {
		return
	}
	if filter.Profile == "" && filter.BotName == "" {
		return
	}
	_, _ = d.dispatchReadyQueueHeadWithContext(context.Background(), filter.Profile, filter.BotName)
}

func (d *Daemon) waitMessage(ctx context.Context, req SocketRequest) (*MessageWaitSummary, *SocketError) {
	filter, err := d.messageListFilter(req)
	if err != nil {
		return nil, err
	}
	// Note: the asyncRewake pull-waiter is registered by the messages.wait handler
	// (not here) so it spans both this wait and the handler's atomic receive.
	if kinds, ok := req.Params["kinds"].([]any); ok {
		for _, kind := range kinds {
			filter.Kinds = append(filter.Kinds, kind.(string))
		}
	}
	var sessionClaimHolder string
	if req.Auth != nil && req.Auth.Mode == "session" {
		sessionClaimHolder = "session:" + *req.Auth.SessionID + ":" + *req.Auth.SessionGeneration
	}
	timeout := time.Duration(numberParam(req.Params["timeout_ms"])) * time.Millisecond
	deadline := time.Now().Add(timeout)
	for {
		// A dropped connection (the rewake waiter went away) ends the wait promptly
		// with no message, so the handler claims nothing on a departed caller's
		// behalf and we don't poll for the full timeout against a dead peer.
		if ctx != nil && ctx.Err() != nil {
			return nil, nil
		}
		summary, sessionActive, waitErr := d.waitLocalMessageSummary(req, filter, sessionClaimHolder)
		if waitErr != nil {
			return nil, waitErr
		}
		if summary != nil {
			if req.Auth != nil && req.Auth.Mode == "owner" {
				_, _ = d.dispatchReadyQueueHead(summary.Profile, summary.BotName)
			}
			return summary, nil
		}
		if !sessionActive && req.Auth != nil && req.Auth.Mode == "owner" && shouldIngestCloudMessageKinds(filter.Kinds) {
			allowProfileOnlyCloudIngest := filter.BotName == ""
			if cloudContextTimeout, ok := cloudNotificationWaitContextTimeout(timeout, deadline); ok {
				// Derive the cloud-wait context from the connection ctx so a disconnect
				// (the rewake hook/socket going away) cancels this blocking wait
				// immediately, letting the loop return and the pull-waiter deregister —
				// otherwise hasPullWaiter would stay true until the per-wait timeout and the
				// transient nudge path would skip bmux for messages arriving in between.
				cloudBase := ctx
				if cloudBase == nil {
					cloudBase = context.Background()
				}
				cloudCtx, cancel := context.WithTimeout(cloudBase, cloudContextTimeout)
				var ingestErr *SocketError
				if timeout <= 0 {
					_, ingestErr = d.ingestCloudNotification(cloudCtx, filter.Profile, filter.BotName, allowProfileOnlyCloudIngest, normalizedCloudNotificationWaitOperationTimeout(timeout), filter.Kinds)
				} else {
					_, ingestErr = d.waitForCloudNotificationWake(cloudCtx, filter.Profile, filter.BotName, allowProfileOnlyCloudIngest, normalizedCloudNotificationWaitOperationTimeout(timeout), filter.Kinds)
				}
				cancel()
				if ingestErr != nil {
					return nil, ingestErr
				}
				summary, _, waitErr = d.waitLocalMessageSummary(req, filter, sessionClaimHolder)
				if waitErr != nil {
					return nil, waitErr
				}
				if summary != nil {
					if req.Auth != nil && req.Auth.Mode == "owner" {
						_, _ = d.dispatchReadyQueueHead(summary.Profile, summary.BotName)
					}
					return summary, nil
				}
			}
		}
		if timeout <= 0 {
			return nil, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, nil
		}
		sleep := 100 * time.Millisecond
		if remaining < sleep {
			sleep = remaining
		}
		if ctx != nil {
			timer := time.NewTimer(sleep)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, nil
			case <-timer.C:
			}
		} else {
			time.Sleep(sleep)
		}
	}
}

func cloudNotificationWaitContextTimeout(timeout time.Duration, deadline time.Time) (time.Duration, bool) {
	if timeout <= 0 {
		return startupProbeTimeout, true
	}
	remaining := time.Until(deadline)
	if remaining <= cloudNotificationTransportSlack {
		return 0, false
	}
	contextTimeout := remaining - cloudNotificationTransportSlack
	if contextTimeout > cloudNotificationLeaseProbeTimeout {
		contextTimeout = cloudNotificationLeaseProbeTimeout
	}
	return contextTimeout, true
}

func normalizedCloudNotificationWaitOperationTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return 0
	}
	maxTimeout := 65 * time.Second
	if timeout > maxTimeout {
		return maxTimeout
	}
	return timeout
}

func requireProfileScopedOwnerAuth(req SocketRequest, profile string) *SocketError {
	if req.Auth == nil || req.Auth.Mode != "owner" || req.Auth.Profile == nil {
		return socketError("FORBIDDEN", "operation requires profile-scoped owner auth", false)
	}
	if *req.Auth.Profile != profile {
		return socketError("FORBIDDEN", "owner profile does not match requested profile", false)
	}
	return nil
}

func (d *Daemon) waitLocalMessageSummary(req SocketRequest, filter MessageListFilter, sessionClaimHolder string) (*MessageWaitSummary, bool, *SocketError) {
	unlock, lockErr := d.lockSessionAuthIfNeeded(req)
	if lockErr != nil {
		return nil, false, lockErr
	}
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()
	if sessionClaimHolder != "" && d.sessionAuthDailyResetPendingLocked(req) {
		return nil, true, nil
	}
	waitCtx := contextWithSocketRequest(context.Background(), req)
	if !shouldLogSocketRequest(req.Op) {
		waitCtx = contextWithSocketRequest(context.Background(), SocketRequest{
			ID:     req.ID,
			Op:     "messages.wait_local_summary",
			Auth:   req.Auth,
			Params: req.Params,
		})
	}
	for {
		d.lockBusForContext(waitCtx)
		if sessionClaimHolder != "" {
			var active bool
			if err := d.runSocketStageForContext(waitCtx, "wait.has_active_claim", func() error {
				var storeErr error
				active, storeErr = d.store.HasActiveSessionClaimForBot(context.Background(), sessionClaimHolder, filter.BotName, filter.BotID, filter.BotAgentID, time.Now().UTC())
				return storeErr
			}); err != nil {
				d.busMu.Unlock()
				return nil, false, classifyMessageStoreError(err)
			}
			if active {
				d.busMu.Unlock()
				return nil, true, nil
			}
		}
		summary, storeErr := d.waitCurrentReadyMessageSummaryLocked(waitCtx, filter, time.Now().UTC())
		d.busMu.Unlock()
		if stale, ok := staleBotletsTaskTargetFromError(storeErr); ok {
			unlock()
			unlock = nil
			reqCtx := contextWithSocketRequest(context.Background(), req)
			if _, err := d.quarantineCloudMessageForStaleBotletsTaskTarget(reqCtx, stale.Profile, stale.MessageID, time.Now().UTC()); err != nil {
				return nil, false, classifyMessageStoreError(err)
			}
			relock, relockErr := d.lockSessionAuthIfNeeded(req)
			if relockErr != nil {
				return nil, false, relockErr
			}
			unlock = relock
			if sessionClaimHolder != "" && d.sessionAuthDailyResetPendingLocked(req) {
				return nil, true, nil
			}
			continue
		}
		if storeErr != nil {
			return nil, false, classifyMessageStoreError(storeErr)
		}
		return summary, false, nil
	}
}

func (d *Daemon) waitCurrentReadyMessageSummaryLocked(ctx context.Context, filter MessageListFilter, now time.Time) (*MessageWaitSummary, error) {
	for {
		var summary *MessageWaitSummary
		storeErr := d.runSocketStageForContext(ctx, "ready.wait_summary", func() error {
			var err error
			summary, err = d.store.WaitMessageSummary(ctx, filter)
			return err
		})
		if storeErr != nil {
			return nil, storeErr
		}
		if summary == nil || summary.Source != "comment.io" {
			return summary, nil
		}
		summaryCtx := ctx
		if req, ok := socketRequestFromContext(ctx); ok {
			summaryCtx = contextWithSocketRequest(ctx, socketRequestWithMessageID(req, summary.MessageID))
		}
		if err := d.runSocketStageForContext(summaryCtx, "ready.metadata_read", func() error {
			_, err := ReadPrivateCloudMessageMetadata(d.paths, summary.Profile, summary.MessageID)
			return err
		}); err != nil {
			if _, err := d.quarantineCloudMessageForMissingMetadata(summaryCtx, summary.Profile, summary.MessageID, now); err != nil {
				return nil, err
			}
			continue
		}
		if summary.Kind == "botlets.task" && !d.botletsTaskSummaryMatchesCurrentTarget(summary) {
			return nil, staleBotletsTaskTargetError{Profile: summary.Profile, MessageID: summary.MessageID}
		}
		return summary, nil
	}
}

func (d *Daemon) waitReadyMessageSummaryForDispatchLocked(ctx context.Context, filter MessageListFilter, now time.Time, nudgeRecords []SessionRecord) (*MessageWaitSummary, error) {
	cursor := filter.Cursor
	for {
		pageFilter := filter
		pageFilter.Cursor = cursor
		var summaries []MessageWaitSummary
		storeErr := d.runSocketStageForContext(ctx, "nudge.ready_summary", func() error {
			var err error
			summaries, err = d.store.WaitMessageSummaries(ctx, pageFilter, dispatchReadySummaryLimit)
			return err
		})
		if storeErr != nil {
			return nil, storeErr
		}
		if len(summaries) == 0 {
			return nil, nil
		}
		for i := range summaries {
			summary := summaries[i]
			if summary.Source != "comment.io" {
				return &summary, nil
			}
			if summary.Kind == "botlets.task" && !d.botletsTaskSummaryMatchesCurrentTarget(&summary) {
				return nil, staleBotletsTaskTargetError{Profile: summary.Profile, MessageID: summary.MessageID}
			}
			if d.cloudMessageAutoNudgeBlocked(summary, now, nudgeRecords) {
				continue
			}
			return &summary, nil
		}
		if len(summaries) < dispatchReadySummaryLimit {
			return nil, nil
		}
		cursor = summaries[len(summaries)-1].MessageID
	}
}

func (d *Daemon) automaticNudgeBlockRecords() []SessionRecord {
	d.sessionMu.Lock()
	defer d.sessionMu.Unlock()
	return d.automaticNudgeBlockRecordsLocked()
}

func (d *Daemon) automaticNudgeBlockRecordsLocked() []SessionRecord {
	records, err := d.listSessionRecords()
	if err != nil {
		return nil
	}
	return records
}

func (d *Daemon) cloudMessageAutoNudgeBlocked(summary MessageWaitSummary, now time.Time, records []SessionRecord) bool {
	blocked, _ := d.cloudMessageAutoNudgeBlockDeadline(summary, now, records)
	return blocked
}

func (d *Daemon) cloudMessageAutoNudgeBlockDeadline(summary MessageWaitSummary, now time.Time, records []SessionRecord) (bool, *time.Time) {
	if summary.Source != "comment.io" {
		return false, nil
	}
	claimGeneration := d.cloudMessageClaimGeneration(summary)
	for _, record := range records {
		if record.State != "alive" || record.Profile != summary.Profile || record.BotName != summary.BotName {
			continue
		}
		if summary.BotID != "" && record.BotID != "" && record.BotID != summary.BotID {
			continue
		}
		if summary.BotAgentID != "" && record.BotAgentID != "" && record.BotAgentID != summary.BotAgentID {
			continue
		}
		if blocked, deadline := automaticNudgeBlockDeadline(record, summary.MessageID, claimGeneration, now); blocked {
			return true, deadline
		}
	}
	return false, nil
}

func (d *Daemon) cloudMessageClaimGeneration(summary MessageWaitSummary) *string {
	if summary.Source != "comment.io" {
		return nil
	}
	metadata, err := ReadPrivateCloudMessageMetadata(d.paths, summary.Profile, summary.MessageID)
	if err != nil {
		return nil
	}
	return cloudHandlingClaimGeneration(metadata)
}

func automaticNudgeBlocked(record SessionRecord, messageID string, claimGeneration *string, now time.Time) bool {
	blocked, _ := automaticNudgeBlockDeadline(record, messageID, claimGeneration, now)
	return blocked
}

func automaticNudgeBlockDeadline(record SessionRecord, messageID string, claimGeneration *string, now time.Time) (bool, *time.Time) {
	state := automaticNudgeState(record, messageID, claimGeneration)
	if state.MessageID == nil || *state.MessageID != messageID {
		return false, nil
	}
	if !sameClaimGeneration(state.ClaimGeneration, claimGeneration) {
		return false, nil
	}
	if state.Stuck {
		return true, nil
	}
	if state.NextEligibleAt == nil {
		return false, nil
	}
	nextEligibleAt, err := time.Parse(time.RFC3339Nano, *state.NextEligibleAt)
	if err != nil {
		return false, nil
	}
	if nextEligibleAt.After(now.UTC()) {
		return true, &nextEligibleAt
	}
	return false, nil
}

func shouldIngestCloudMessageKinds(kinds []string) bool {
	if len(kinds) == 0 {
		return true
	}
	for _, kind := range kinds {
		switch kind {
		case "botlets.task":
			return true
		}
	}
	return false
}

func cloudMessageKindAllowedByFilter(kind string, filterKinds []string) bool {
	if len(filterKinds) == 0 {
		return true
	}
	for _, filterKind := range filterKinds {
		if filterKind == kind {
			return true
		}
	}
	return false
}

func (d *Daemon) waitForCloudNotificationWake(ctx context.Context, profile string, botName string, cloudOnly bool, operationTimeout time.Duration, filterKinds []string) (bool, *SocketError) {
	for {
		acquired, ingestErr := d.ingestCloudNotification(ctx, profile, botName, cloudOnly, operationTimeout, filterKinds)
		if acquired || ingestErr != nil || ctx.Err() != nil {
			return acquired, ingestErr
		}
		if d.notificationClient == nil {
			return false, nil
		}
		wakeClient, ok := d.notificationClient.(NotificationWakeClient)
		if !ok {
			if !sleepWithContext(ctx, cloudNotificationPollIdleDelay) {
				return false, nil
			}
			return false, nil
		}
		profileConfig, _, targetOK := d.cloudNotificationTarget(profile, botName, cloudOnly)
		if !targetOK {
			return false, nil
		}
		wakeCtx := ctx
		var cancelWake context.CancelFunc
		if deadline, ok := ctx.Deadline(); ok {
			wakeTimeout := time.Until(deadline)
			if wakeTimeout > cloudNotificationPollIdleDelay {
				wakeTimeout = cloudNotificationPollIdleDelay
			}
			if wakeTimeout <= 0 {
				return false, nil
			}
			wakeCtx, cancelWake = context.WithTimeout(ctx, wakeTimeout)
		}
		_, wakeErr := wakeClient.WaitNotificationWake(wakeCtx, profileConfig)
		if cancelWake != nil {
			cancelWake()
		}
		if wakeErr != nil {
			if ctx.Err() != nil || wakeCtx.Err() != nil || errors.Is(wakeErr, errNotificationWakeDeadline) {
				return false, nil
			}
			if errors.Is(wakeErr, ErrAgentAuthRevoked) {
				return false, socketError("AGENT_AUTH_REVOKED", "agent credentials were revoked; re-issue credentials to resume", false)
			}
			fallbackAcquired, fallbackErr := d.ingestCloudNotification(ctx, profile, botName, cloudOnly, operationTimeout, filterKinds)
			if fallbackAcquired || fallbackErr != nil || ctx.Err() != nil {
				return fallbackAcquired, fallbackErr
			}
			if !sleepWithContext(ctx, cloudNotificationPollIdleDelay) {
				return false, nil
			}
			return false, nil
		}
	}
}

func (d *Daemon) ingestCloudNotification(ctx context.Context, profile string, botName string, cloudOnly bool, operationTimeout time.Duration, filterKinds []string) (bool, *SocketError) {
	if d.notificationClient == nil {
		return false, nil
	}
	profileConfig, bot, ok := d.cloudNotificationTarget(profile, botName, cloudOnly)
	if !ok {
		return false, nil
	}
	unlockCloudWait, locked := d.lockCloudNotificationWait(ctx, profile)
	if !locked {
		return false, nil
	}
	defer unlockCloudWait()
	if ctx.Err() != nil {
		return false, nil
	}
	var (
		ready    bool
		readyErr *SocketError
	)
	if cloudOnly {
		ready, readyErr = d.hasReadyCloudNotificationMessage(profile, botName)
	} else {
		ready, readyErr = d.hasReadyLocalMessageForCloudIngest(profile, botName)
	}
	if readyErr != nil {
		return false, readyErr
	}
	if ready {
		return false, nil
	}
	profileConfig, bot, ok = d.cloudNotificationTarget(profile, botName, cloudOnly)
	if !ok {
		return false, nil
	}
	leaseHolder := "comment-bus:" + profile
	leaseKinds := canonicalCloudNotificationKinds(filterKinds)
	op, err := BeginCloudNotificationWaitOperationForBotAndKinds(d.paths, profile, botName, operationTimeout, defaultLocalLeaseTTL, leaseHolder, leaseKinds, time.Now().UTC())
	if err != nil {
		return false, socketError("UPSTREAM_ERROR", "could not prepare notification lease", true)
	}
	op, err = RecordCloudNotificationWaitOperationAttempt(d.paths, op, time.Now().UTC())
	if err != nil {
		return false, socketError("UPSTREAM_ERROR", "could not prepare notification lease", true)
	}
	opLeaseTTL := time.Duration(op.LeaseTTLMS) * time.Millisecond
	lease, err := d.notificationClient.LeaseNotification(ctx, profileConfig, opLeaseTTL, op.LeaseHolder, op.OpID, leaseKinds...)
	if err != nil {
		if !errors.Is(err, errNotificationLeaseDeadline) && !errors.Is(err, errNotificationLeaseAmbiguous) {
			_ = CompleteCloudNotificationWaitOperation(d.paths, op, time.Now().UTC())
		}
		if errors.Is(err, errNotificationLeaseDeadline) {
			return false, socketError("UPSTREAM_ERROR", "notification lease timed out", true)
		}
		return false, socketError("UPSTREAM_ERROR", "notification lease failed", true)
	}
	if lease == nil {
		if err := CompleteCloudNotificationWaitOperation(d.paths, op, time.Now().UTC()); err != nil {
			return false, socketError("UPSTREAM_ERROR", "could not complete notification lease", true)
		}
		return false, nil
	}
	localReq := internalCloudReleaseSocketRequest("cloud.ingest_local", profile, "")
	localCtx := contextWithSocketRequest(context.Background(), localReq)
	notificationID := lease.NotificationID
	if notificationID == "" {
		notificationID = lease.Notification.ID
	}
	if !isSafeCloudID("notification", notificationID) {
		_ = CompleteCloudNotificationWaitOperation(d.paths, op, time.Now().UTC())
		return false, socketError("UPSTREAM_ERROR", "invalid notification lease", true)
	}
	if lease.Notification.Type == "botlets_task" {
		repairedProfile, repairedBot, repaired, repairErr := d.repairCloudBotletsTaskRegistryIfRenamed(ctx, profileConfig, bot, *lease)
		if repairErr != nil {
			d.logger.warn("botlets.task_rename_repair_failed", map[string]any{
				"profile":         profile,
				"bot":             bot.Name,
				"notification_id": notificationID,
				"error":           repairErr.Error(),
			})
			return false, socketError("UPSTREAM_ERROR", "could not repair Botlets profile rename", true)
		}
		if repaired {
			profile = repairedProfile.Handle
			botName = repairedBot.Name
			profileConfig = repairedProfile
			bot = repairedBot
			localReq = internalCloudReleaseSocketRequest("cloud.ingest_local", profile, "")
			localCtx = contextWithSocketRequest(context.Background(), localReq)
		}
	}
	localStage := d.startSocketStage(localReq, "cloud.ingest_local")
	localStageDone := false
	finishLocalStage := func() {
		if localStageDone {
			return
		}
		localStageDone = true
		localStage.done()
	}
	d.sessionMu.Lock()
	d.lockBusForSocketRequest(localReq)
	d.profileMu.RLock()
	currentProfileConfig, currentBot, currentOK := d.cloudNotificationTargetLocked(profile, botName, cloudOnly)
	if errors.Is(ctx.Err(), context.Canceled) || !currentOK || !sameCloudNotificationTarget(profileConfig, bot, currentProfileConfig, currentBot) {
		d.profileMu.RUnlock()
		d.busMu.Unlock()
		d.sessionMu.Unlock()
		finishLocalStage()
		now := time.Now().UTC()
		if releaseErr := d.releaseStaleCloudNotificationLease(profile, profileConfig, *lease, notificationID, now); releaseErr != nil {
			return false, releaseErr
		}
		if err := CompleteCloudNotificationWaitOperation(d.paths, op, now); err != nil {
			return false, socketError("UPSTREAM_ERROR", "could not complete notification lease", true)
		}
		return false, nil
	}
	profileConfig = currentProfileConfig
	bot = currentBot
	ingestLocksHeld := true
	unlockIngestLocks := func() {
		if !ingestLocksHeld {
			return
		}
		d.sessionMu.Unlock()
		d.busMu.Unlock()
		d.profileMu.RUnlock()
		ingestLocksHeld = false
		finishLocalStage()
	}
	// Auto-start (mention -> launch the recipient bot) MUST run OUTSIDE the ingest
	// locks (sessionMu/busMu/profileMu). mentionAutoStartTargetIsLive's stale-record
	// verify-and-forget waits on a transient runtime goroutine (forgetInactiveTransientRuntime
	// blocks on <-runtime.done); that goroutine can be parked acquiring busMu inside
	// nudgeTransientReadyQueueHead. Holding busMu here while waiting on it = deadlock,
	// hanging the whole daemon on a new @mention. So we DON'T call it inline (where it
	// would run before the deferred unlock). Instead the genuinely-new-message success
	// path below records the trigger args, and this defer fires the auto-start only
	// after unlockIngestLocks has released the ingest locks.
	//
	// Deferred LIFO ordering is load-bearing: this defer is registered BEFORE
	// `defer unlockIngestLocks()`, so it runs AFTER the unlock when the function
	// returns. The in-flight/cooldown reservation (reserveMentionAutoStart, guarded
	// by the separate mentionAutoStartMu) still serializes concurrent mentions into a
	// single launch even though the liveness check now runs unlocked.
	var (
		autoStartTrigger bool
		autoStartProfile string
		autoStartBot     BotRegistryEntry
		autoStartRawKind string
	)
	defer func() {
		if !autoStartTrigger {
			return
		}
		d.maybeAutoStartRuntimeForMention(ctx, autoStartProfile, autoStartBot, autoStartRawKind)
	}()
	defer unlockIngestLocks()
	releaseDeclined := func(metadataProfile string, messageID string, profileConfig AgentProfile, lease CloudNotificationLease, notificationID string, now time.Time) *SocketError {
		unlockIngestLocks()
		return d.releaseDeclinedCloudNotificationLease(metadataProfile, messageID, profileConfig, lease, notificationID, now)
	}
	replayPendingAck := func(pendingOps []CloudNotificationClaimOperation, lease CloudNotificationLease, now time.Time) (bool, *SocketError) {
		pendingOp, ok := firstReplayablePendingAckCloudOperation(pendingOps)
		if !ok {
			return false, nil
		}
		unlockIngestLocks()
		return d.replayPendingAckCloudOperationForDuplicateLease(profileConfig, pendingOp, lease, now)
	}
	if lease.Notification.Type == "botlets_task" && !d.botletsTaskNotificationMatchesCurrentTarget(lease.Notification.BotletsTask, bot, profileConfig) {
		now := time.Now().UTC()
		declinedID, idErr := GenerateLocalID("msg", 0)
		if idErr != nil {
			_ = CompleteCloudNotificationWaitOperation(d.paths, op, now)
			return false, socketError("UPSTREAM_ERROR", "could not allocate message id", true)
		}
		if releaseErr := releaseDeclined(profile, declinedID, profileConfig, *lease, notificationID, now); releaseErr != nil {
			return false, releaseErr
		}
		if err := CompleteCloudNotificationWaitOperation(d.paths, op, now); err != nil {
			return false, socketError("UPSTREAM_ERROR", "could not complete notification lease", true)
		}
		return false, nil
	}
	if existingProfile, existingID, ok, err := findPrivateCloudMessageByNotificationIDForBot(d.paths, profile, bot, notificationID); err != nil {
		return false, socketError("UPSTREAM_ERROR", "could not inspect notification metadata", true)
	} else if ok {
		existingCtx := localCtx
		if req, ok := socketRequestFromContext(localCtx); ok {
			existingCtx = contextWithSocketRequest(localCtx, socketRequestWithMessageID(req, existingID))
		}
		existing, storeErr := d.getInboxMessageForContext(existingCtx, existingProfile, existingID, "cloud.ingest_existing_get_message")
		if storeErr == nil {
			now := time.Now().UTC()
			if !cloudMessageKindAllowedByFilter(existing.Kind, filterKinds) {
				if releaseErr := releaseDeclined(existingProfile, existingID, profileConfig, *lease, notificationID, now); releaseErr != nil {
					return false, releaseErr
				}
				if err := CompleteCloudNotificationWaitOperation(d.paths, op, now); err != nil {
					return false, socketError("UPSTREAM_ERROR", "could not complete notification lease", true)
				}
				return false, nil
			}
			if (botName != "" || existingProfile != profile) && !existingCloudNotificationMessageMatchesTarget(existing, bot) {
				if releaseErr := releaseDeclined(existingProfile, existingID, profileConfig, *lease, notificationID, now); releaseErr != nil {
					return false, releaseErr
				}
				if err := CompleteCloudNotificationWaitOperation(d.paths, op, now); err != nil {
					return false, socketError("UPSTREAM_ERROR", "could not complete notification lease", true)
				}
				return false, nil
			}
			reopenClaimed, adoptLease, reopenErr := d.shouldAdoptCloudNotificationLease(existing, now)
			if reopenErr != nil {
				return false, reopenErr
			}
			if !adoptLease {
				metadataAvailable := true
				currentMetadata, metadataErr := ReadPrivateCloudMessageMetadata(d.paths, existingProfile, existingID)
				if metadataErr != nil {
					metadataAvailable = false
					pendingOps, pendingErr := ListPendingCloudNotificationClaimOperationsForLocalMessage(d.paths, existingProfile, existingID)
					if pendingErr != nil {
						return false, socketError("UPSTREAM_ERROR", "could not inspect pending notification operations", true)
					}
					if err := AbandonPendingCloudNotificationRenewOperations(d.paths, pendingOps); err != nil {
						return false, socketError("UPSTREAM_ERROR", "could not abandon stale notification renew", true)
					}
					if HasPendingTerminalCloudNotificationClaimOperation(pendingOps) {
						replayed, replayErr := replayPendingAck(pendingOps, *lease, now)
						if replayErr != nil {
							return false, replayErr
						}
						if replayed {
							if err := CompleteCloudNotificationWaitOperation(d.paths, op, now); err != nil {
								return false, socketError("UPSTREAM_ERROR", "could not complete notification lease", true)
							}
							return false, nil
						}
						if releaseErr := releaseDeclined(existingProfile, existingID, profileConfig, *lease, notificationID, now); releaseErr != nil {
							return false, releaseErr
						}
						if err := CompleteCloudNotificationWaitOperation(d.paths, op, now); err != nil {
							return false, socketError("UPSTREAM_ERROR", "could not complete notification lease", true)
						}
						return false, nil
					}
					if existing.Source == "comment.io" {
						if _, quarantineErr := d.quarantineCloudMessageForMissingMetadata(existingCtx, existingProfile, existingID, now); quarantineErr != nil && !errors.Is(quarantineErr, ErrMessageConflict) {
							return false, classifyMessageStoreError(quarantineErr)
						}
					}
				} else {
					pendingOps, pendingErr := ListPendingCloudNotificationClaimOperationsForMessage(d.paths, existingProfile, existingID, currentMetadata.ClaimID, currentMetadata.NotificationID)
					if pendingErr != nil {
						return false, socketError("UPSTREAM_ERROR", "could not inspect pending notification operations", true)
					}
					if HasPendingTerminalCloudNotificationClaimOperation(pendingOps) {
						if err := AbandonPendingCloudNotificationRenewOperations(d.paths, pendingOps); err != nil {
							return false, socketError("UPSTREAM_ERROR", "could not abandon stale notification renew", true)
						}
						replayed, replayErr := replayPendingAck(pendingOps, *lease, now)
						if replayErr != nil {
							return false, replayErr
						}
						if replayed {
							if err := CompleteCloudNotificationWaitOperation(d.paths, op, now); err != nil {
								return false, socketError("UPSTREAM_ERROR", "could not complete notification lease", true)
							}
							return false, nil
						}
					}
				}
				if shouldReleaseDeclinedCloudNotificationLease(existing, currentMetadata, metadataAvailable, *lease, now) {
					if releaseErr := releaseDeclined(existingProfile, existingID, profileConfig, *lease, notificationID, now); releaseErr != nil {
						return false, releaseErr
					}
				}
				if err := CompleteCloudNotificationWaitOperation(d.paths, op, now); err != nil {
					return false, socketError("UPSTREAM_ERROR", "could not complete notification lease", true)
				}
				return false, nil
			}
			currentMetadata, metadataErr := ReadPrivateCloudMessageMetadata(d.paths, existingProfile, existingID)
			if metadataErr != nil {
				pendingOps, pendingErr := ListPendingCloudNotificationClaimOperationsForLocalMessage(d.paths, existingProfile, existingID)
				if pendingErr != nil {
					return false, socketError("UPSTREAM_ERROR", "could not inspect pending notification operations", true)
				}
				if err := AbandonPendingCloudNotificationRenewOperations(d.paths, pendingOps); err != nil {
					return false, socketError("UPSTREAM_ERROR", "could not abandon stale notification renew", true)
				}
				if HasPendingTerminalCloudNotificationClaimOperation(pendingOps) {
					replayed, replayErr := replayPendingAck(pendingOps, *lease, now)
					if replayErr != nil {
						return false, replayErr
					}
					if replayed {
						if err := CompleteCloudNotificationWaitOperation(d.paths, op, now); err != nil {
							return false, socketError("UPSTREAM_ERROR", "could not complete notification lease", true)
						}
						return false, nil
					}
					if releaseErr := releaseDeclined(existingProfile, existingID, profileConfig, *lease, notificationID, now); releaseErr != nil {
						return false, releaseErr
					}
					if err := CompleteCloudNotificationWaitOperation(d.paths, op, now); err != nil {
						return false, socketError("UPSTREAM_ERROR", "could not complete notification lease", true)
					}
					return false, nil
				}
				if existing.Source == "comment.io" {
					if _, quarantineErr := d.quarantineCloudMessageForMissingMetadata(existingCtx, existingProfile, existingID, now); quarantineErr != nil && !errors.Is(quarantineErr, ErrMessageConflict) {
						return false, classifyMessageStoreError(quarantineErr)
					}
				}
				if releaseErr := releaseDeclined(existingProfile, existingID, profileConfig, *lease, notificationID, now); releaseErr != nil {
					return false, releaseErr
				}
				if err := CompleteCloudNotificationWaitOperation(d.paths, op, now); err != nil {
					return false, socketError("UPSTREAM_ERROR", "could not complete notification lease", true)
				}
				return false, nil
			}
			pendingOps, pendingErr := ListPendingCloudNotificationClaimOperationsForMessage(d.paths, existingProfile, existingID, currentMetadata.ClaimID, currentMetadata.NotificationID)
			if pendingErr != nil {
				return false, socketError("UPSTREAM_ERROR", "could not inspect pending notification operations", true)
			}
			if err := AbandonPendingCloudNotificationRenewOperations(d.paths, pendingOps); err != nil {
				return false, socketError("UPSTREAM_ERROR", "could not abandon stale notification renew", true)
			}
			if HasPendingTerminalCloudNotificationClaimOperation(pendingOps) {
				replayed, replayErr := replayPendingAck(pendingOps, *lease, now)
				if replayErr != nil {
					return false, replayErr
				}
				if replayed {
					if err := CompleteCloudNotificationWaitOperation(d.paths, op, now); err != nil {
						return false, socketError("UPSTREAM_ERROR", "could not complete notification lease", true)
					}
					return false, nil
				}
				if releaseErr := releaseDeclined(existingProfile, existingID, profileConfig, *lease, notificationID, now); releaseErr != nil {
					return false, releaseErr
				}
				if err := CompleteCloudNotificationWaitOperation(d.paths, op, now); err != nil {
					return false, socketError("UPSTREAM_ERROR", "could not complete notification lease", true)
				}
				return false, nil
			}
			_, metadata, metadataErr := CloudMessageFromLease(existingID, profile, bot, profileConfig, *lease, now)
			if metadataErr != nil {
				_ = CompleteCloudNotificationWaitOperation(d.paths, op, now)
				return false, socketError("UPSTREAM_ERROR", "invalid notification lease", true)
			}
			metadata.Profile = existingProfile
			if err := WritePrivateCloudMessageMetadata(d.paths, metadata); err != nil {
				return false, socketError("UPSTREAM_ERROR", "could not write notification metadata", true)
			}
			quarantinedForRedelivery, refreshErr := d.store.RefreshCloudNotificationLease(localCtx, existingProfile, existingID, metadata.LeaseExpiresAt, now, reopenClaimed)
			if refreshErr != nil {
				return false, classifyMessageStoreError(refreshErr)
			}
			if quarantinedForRedelivery {
				// Surface the stuck redelivery loop in `make logs-errors` (#301)
				// — it was previously silent while the bot burned cycles on the
				// same message.
				d.logger.warn("message.redelivery_warning", map[string]any{
					"message_id": existingID,
					"profile":    profile,
					"cap":        cloudRedeliveryCap,
					"reason":     "redelivery_cap_exceeded",
				})
			}
			updated, storeErr := d.getInboxMessageForContext(existingCtx, existingProfile, existingID, "cloud.ingest_updated_get_message")
			if storeErr != nil {
				return false, classifyMessageStoreError(storeErr)
			}
			// A terminally-acked cloud notification is not real work to hand to
			// the runtime — either it was just quarantined for exceeding the
			// redelivery cap (#301), or it was already acked and is only being
			// re-delivered because the cloud-side ack never landed. Decline the
			// remote lease and report not-acquired so the poller idles instead of
			// tight-looping on the same notification (reporting acquired==true
			// here would make the poller immediately continue and re-lease it).
			if updated.Delivery.State == "acked" {
				if releaseErr := releaseDeclined(existingProfile, existingID, profileConfig, *lease, notificationID, now); releaseErr != nil {
					return false, releaseErr
				}
				if err := CompleteCloudNotificationWaitOperation(d.paths, op, now); err != nil {
					return false, socketError("UPSTREAM_ERROR", "could not complete notification lease", true)
				}
				return false, nil
			}
			_ = WriteMessageSpool(d.paths, updated)
			if err := CompleteCloudNotificationWaitOperation(d.paths, op, now); err != nil {
				return false, socketError("UPSTREAM_ERROR", "could not complete notification lease", true)
			}
			d.logLeaseAcquired(updated, "refresh")
			return true, nil
		}
		if !errors.Is(storeErr, ErrMessageNotFound) {
			return false, classifyMessageStoreError(storeErr)
		}
	}
	messageID, err := GenerateLocalID("msg", 0)
	if err != nil {
		return false, socketError("UPSTREAM_ERROR", "could not allocate message id", true)
	}
	now := time.Now().UTC()
	message, metadata, err := CloudMessageFromLease(messageID, profile, bot, profileConfig, *lease, now)
	if err != nil {
		_ = releaseDeclined(profile, messageID, profileConfig, *lease, notificationID, now)
		_ = CompleteCloudNotificationWaitOperation(d.paths, op, now)
		return false, socketError("UPSTREAM_ERROR", "invalid notification lease", true)
	}
	if message.Kind == "botlets.task" {
		expanded, expandErr := ExpandBotletsTaskMessageBody(d.paths, bot, message, lease.Notification)
		if expandErr != nil {
			d.logger.warn("botlets.task_orientation_skipped", map[string]any{
				"profile":         profile,
				"bot":             bot.Name,
				"notification_id": notificationID,
				"error":           expandErr.Error(),
			})
		} else {
			message = expanded
		}
	}
	if !cloudMessageKindAllowedByFilter(message.Kind, filterKinds) {
		if releaseErr := releaseDeclined(profile, messageID, profileConfig, *lease, notificationID, now); releaseErr != nil {
			return false, releaseErr
		}
		if err := CompleteCloudNotificationWaitOperation(d.paths, op, now); err != nil {
			return false, socketError("UPSTREAM_ERROR", "could not complete notification lease", true)
		}
		return false, nil
	}
	if err := WritePrivateCloudMessageMetadata(d.paths, metadata); err != nil {
		return false, socketError("UPSTREAM_ERROR", "could not write notification metadata", true)
	}
	inserted, err := d.store.InsertCloudNotificationMessage(localCtx, message)
	if err != nil {
		return false, classifyMessageStoreError(err)
	}
	_ = WriteMessageSpool(d.paths, inserted)
	// A genuinely NEW incoming doc notification just landed and is now DURABLY
	// stored. If the recipient bot opted into "Responds to @mentions" and nothing
	// is running for it, launch its runtime detached (async; never blocks this
	// poller). All gates + cooldown/in-flight backoff live in
	// maybeAutoStartRuntimeForMention. This is the new-message path only — the
	// existing-message refresh/re-delivery paths above don't auto-launch, so a
	// re-leased message can't re-trigger a launch.
	//
	// Gate on the RAW notification kind (lease.Notification.Type), NOT the stored
	// message kind: NormalizeNotificationKind collapses mention / reply / comment
	// / suggestion all into "doc.mention", so the stored kind cannot distinguish a
	// real @mention from a passive comment/reply/suggestion on a followed doc.
	// Only a real "mention" or "review_requested" should auto-start the bot
	// (enforced by mentionAutoStartRawKindEligible inside the deferred fire).
	//
	// Arm the trigger HERE — immediately after the durable InsertCloudNotificationMessage
	// that represents a genuinely-new message of an eligible kind, and BEFORE the
	// CompleteCloudNotificationWaitOperation completion write below. The completion is a
	// purely-local lease-bookkeeping write; if it FAILS after the mention is already
	// durably stored, the path returns an error before reaching this point on a later
	// poller pass (subsequent passes see an already-stored message, not a new insert,
	// so they never auto-start). Arming before the completion makes the auto-start
	// depend only on the mention being durably stored, not on the completion succeeding,
	// so a completion-write failure can no longer drop the launch.
	//
	// This is the ONLY genuinely-new-message success path; every other return above is
	// a decline/refresh/re-lease/error that must NOT auto-start, and none of them arm
	// the trigger. The actual fire is deferred: the defer registered above runs AFTER
	// unlockIngestLocks releases the ingest locks (sessionMu/busMu/profileMu). Invoking
	// it inline would run it while those locks are still held and can deadlock against a
	// transient runtime goroutine parked on busMu (see the auto-start defer above). The
	// final profile/bot/raw-kind values are captured here so the deferred call sees
	// exactly what this path resolved.
	//
	// Arm with currentBot — the post-lease record re-read under profileMu above (the
	// same value bot was reassigned to). sameCloudNotificationTarget does NOT compare
	// RespondsToMentions, so a user who toggles the "Responds to @mentions" opt-in
	// DURING the lease window is reflected only in this refreshed record, not the
	// pre-lease bot. Capturing currentBot guarantees the deferred auto-start arms on
	// the latest opt-in (belt-and-suspenders: maybeAutoStartRuntimeForMention also
	// re-checks RespondsToMentions at fire time). It targets the same handle — only
	// the freshness of the record differs.
	autoStartTrigger = true
	autoStartProfile = profile
	autoStartBot = currentBot
	autoStartRawKind = lease.Notification.Type
	if err := CompleteCloudNotificationWaitOperation(d.paths, op, time.Now().UTC()); err != nil {
		return false, socketError("UPSTREAM_ERROR", "could not complete notification lease", true)
	}
	d.logLeaseAcquired(inserted, "new")
	return true, nil
}

func (d *Daemon) repairCloudBotletsTaskRegistryIfRenamed(ctx context.Context, profileConfig AgentProfile, bot BotRegistryEntry, lease CloudNotificationLease) (AgentProfile, BotRegistryEntry, bool, error) {
	task := lease.Notification.BotletsTask
	repairedEntry, repaired, err := repairBotletsRegistryFromCloudTask(ctx, d.paths, d.botletsHome, bot, task)
	if err != nil || !repaired {
		return AgentProfile{}, BotRegistryEntry{}, false, err
	}
	reload := d.reloadProfiles(ctx, d.botletsHome)
	if HasFatalProfileReloadError(reload.Errors) {
		return AgentProfile{}, BotRegistryEntry{}, false, fmt.Errorf("profile reload after Botlets rename repair failed: %+v", reload.Errors)
	}
	d.profileMu.RLock()
	repairedProfile, profileOK := d.profileState.AgentProfiles[repairedEntry.Handle]
	repairedBot, botOK := d.profileState.BotRegistry[repairedEntry.Name]
	d.profileMu.RUnlock()
	if !profileOK || !botOK {
		return AgentProfile{}, BotRegistryEntry{}, false, errors.New("profile reload after Botlets rename repair did not load repaired bot")
	}
	if !validateCloudBotletsTaskTarget(task, repairedBot, repairedProfile) {
		return AgentProfile{}, BotRegistryEntry{}, false, errors.New("repaired Botlets registry does not match cloud task")
	}
	d.logger.info("botlets.task_rename_repaired", map[string]any{
		"previous_profile": profileConfig.Handle,
		"profile":          repairedProfile.Handle,
		"previous_bot":     bot.Name,
		"bot":              repairedBot.Name,
		"bot_id":           repairedBot.BotID,
		"bot_agent_id":     botAgentID(repairedBot),
	})
	return repairedProfile, repairedBot, true, nil
}

func shouldReleaseDeclinedCloudNotificationLease(message MessageEnvelope, metadata PrivateCloudMessageMetadata, metadataAvailable bool, lease CloudNotificationLease, now time.Time) bool {
	if message.Source != "comment.io" {
		return true
	}
	if message.Delivery.State == "claimed" && !leaseExpired(message.Delivery.LeaseExpiresAt, now) && metadataAvailable && metadata.ClaimID == lease.ClaimID {
		return false
	}
	return true
}

func existingCloudNotificationMessageMatchesTarget(message MessageEnvelope, bot BotRegistryEntry) bool {
	return message.BotName == bot.Name || bot.MatchesStableIdentity(message.BotID, message.BotAgentID)
}

func firstReplayablePendingAckCloudOperation(ops []CloudNotificationClaimOperation) (CloudNotificationClaimOperation, bool) {
	for _, op := range ops {
		if op.Operation == "ack" {
			return op, true
		}
	}
	return CloudNotificationClaimOperation{}, false
}

func (d *Daemon) replayPendingAckCloudOperationForDuplicateLease(profileConfig AgentProfile, op CloudNotificationClaimOperation, duplicateLease CloudNotificationLease, now time.Time) (bool, *SocketError) {
	if op.Operation != "ack" {
		return false, nil
	}
	duplicateNotificationID := duplicateLease.NotificationID
	if duplicateNotificationID == "" {
		duplicateNotificationID = duplicateLease.Notification.ID
	}
	if duplicateLease.ClaimID == "" || duplicateNotificationID != op.NotificationID {
		return false, nil
	}
	if op.ClaimHolder == "" {
		return false, nil
	}
	if d.notificationClient == nil {
		return false, socketError("UPSTREAM_ERROR", "notification client is not configured", true)
	}
	req := internalCloudReleaseSocketRequest("cloud.pending_terminal_replay", op.Profile, op.LocalMessageID)
	var (
		pending CloudNotificationClaimOperation
		ok      bool
		err     error
	)
	if readErr := d.runSocketStage(req, "cloud.pending_terminal_replay_read", func() error {
		pending, ok, err = ReadPendingCloudNotificationClaimOperationWithRetry(d.paths, op.OpID)
		return err
	}); readErr != nil {
		return false, socketError("UPSTREAM_ERROR", "could not inspect pending notification operation", true)
	}
	pendingAlreadyDone := false
	if !ok || !sameCloudNotificationClaimOperation(pending, op) {
		done, doneOK, doneErr := d.readDoneCloudNotificationClaimOperationForSocketRequest(req, op.OpID, "cloud.pending_terminal_replay_done_read")
		if doneErr != nil {
			return false, socketError("UPSTREAM_ERROR", "could not inspect notification operation", true)
		}
		if !doneOK || !sameCloudNotificationClaimOperation(done, op) {
			return false, nil
		}
		pending = done
		pendingAlreadyDone = true
	}

	var metadata PrivateCloudMessageMetadata
	if metadataErr := d.runSocketStage(req, "cloud.pending_terminal_metadata_read", func() error {
		var readErr error
		metadata, readErr = ReadPrivateCloudMessageMetadata(d.paths, op.Profile, op.LocalMessageID)
		return readErr
	}); metadataErr != nil {
		return false, nil
	} else if metadata.ClaimID != op.ClaimID || metadata.NotificationID != op.NotificationID {
		return false, nil
	}

	var message MessageEnvelope
	if storeErr := d.runSocketStage(req, "cloud.pending_terminal_message_read", func() error {
		var readErr error
		message, readErr = d.store.GetInboxMessage(context.Background(), op.Profile, op.LocalMessageID)
		return readErr
	}); storeErr != nil {
		if errors.Is(storeErr, ErrMessageNotFound) {
			return false, nil
		}
		return false, classifyMessageStoreError(storeErr)
	}
	if cloudClaimOperationCompletedHolderMismatch(message, op) {
		return false, nil
	}
	localAlreadyComplete := replayedCloudOperationMatchesLocalState(message, op)
	if !localAlreadyComplete {
		if message.Source != "comment.io" || message.Delivery.State != "claimed" || message.Delivery.ClaimHolder == nil || cloudClaimOperationHolderMismatch(message, op) {
			return false, nil
		}
	}

	attempted := pending
	if !pendingAlreadyDone {
		var attemptErr *SocketError
		attempted, attemptErr = d.recordCloudNotificationClaimOperationAttempt(req, pending, now, "could not prepare notification operation")
		if attemptErr != nil {
			done, doneOK, doneErr := d.readDoneCloudNotificationClaimOperationForSocketRequest(req, pending.OpID, "cloud.pending_terminal_replay_attempt_done_read")
			if doneErr != nil {
				return false, socketError("UPSTREAM_ERROR", "could not inspect notification operation", true)
			}
			if !doneOK || !sameCloudNotificationClaimOperation(done, pending) {
				return false, attemptErr
			}
			attempted = done
			pendingAlreadyDone = true
		}
	}
	var duplicateOp CloudNotificationClaimOperation
	if prepErr := d.runSocketStage(req, "cloud.pending_terminal_duplicate_ack_prepare", func() error {
		var done bool
		var err error
		duplicateOp, done, err = BeginCloudNotificationClaimOperationForHolder(d.paths, "ack", op.Profile, op.LocalMessageID, duplicateLease.ClaimID, op.NotificationID, op.ClaimHolder, "", 0, now)
		if done {
			return errors.New("duplicate notification ack operation unexpectedly done")
		}
		duplicateOp, err = EnsureCloudNotificationClaimOperationCredentialProfile(d.paths, duplicateOp, profileConfig.Handle, now)
		return err
	}); prepErr != nil {
		return false, socketError("UPSTREAM_ERROR", "could not prepare duplicate notification ack", true)
	}
	duplicateAttempt, attemptErr := d.recordCloudNotificationClaimOperationAttempt(req, duplicateOp, now, "could not prepare duplicate notification ack")
	if attemptErr != nil {
		return false, attemptErr
	}
	d.logCloudMutationStart(req, duplicateAttempt)

	remoteStartedAt := time.Now()
	var mutation *CloudNotificationClaimMutation
	if remoteErr := d.runSocketStage(req, "cloud.remote_ack", func() error {
		var mutationErr error
		mutation, mutationErr = d.notificationClient.AckNotification(context.Background(), profileConfig, duplicateAttempt.ClaimID, duplicateAttempt.OpID)
		return mutationErr
	}); remoteErr != nil {
		d.logCloudMutationRemoteEnd(req, duplicateAttempt, time.Since(remoteStartedAt), remoteErr)
		d.logCloudMutationFailed(req, duplicateAttempt, remoteErr)
		if errors.Is(remoteErr, errNotificationMutationDeadline) || errors.Is(remoteErr, errNotificationMutationAmbiguous) {
			_ = RecordCloudNotificationClaimOperationFailure(d.paths, duplicateAttempt, remoteErr, now)
			return true, nil
		}
		_ = AbandonCloudNotificationClaimOperation(d.paths, duplicateAttempt)
		return false, nil
	}
	d.logCloudMutationRemoteEnd(req, duplicateAttempt, time.Since(remoteStartedAt), nil)
	if mutation == nil || mutation.ClaimID != duplicateAttempt.ClaimID || mutation.NotificationID != attempted.NotificationID {
		failure := errors.New("notification claim response mismatch")
		_ = RecordCloudNotificationClaimOperationFailure(d.paths, duplicateAttempt, failure, now)
		d.logCloudMutationFailed(req, duplicateAttempt, failure)
		return false, socketError("UPSTREAM_ERROR", "notification claim response mismatch", true)
	}

	d.lockBusForSocketRequest(req)
	defer d.busMu.Unlock()
	localStartedAt := time.Now()
	pendingAfterMutation, pendingOK, pendingReadErr := d.readPendingCloudNotificationClaimOperationForSocketRequest(req, attempted.OpID, "cloud.pending_terminal_completion.pending_operation_read")
	if pendingReadErr != nil {
		_ = RecordCloudNotificationClaimOperationFailure(d.paths, duplicateAttempt, pendingReadErr, now)
		d.logCloudMutationFailed(req, duplicateAttempt, pendingReadErr)
		return false, socketError("UPSTREAM_ERROR", "could not inspect notification operation", true)
	}
	if !pendingOK {
		done, doneOK, doneErr := d.readDoneCloudNotificationClaimOperationForSocketRequest(req, attempted.OpID, "cloud.pending_terminal_completion.done_operation_read")
		if doneErr != nil {
			return false, socketError("UPSTREAM_ERROR", "could not inspect notification operation", true)
		}
		if doneOK && sameCloudNotificationClaimOperation(done, attempted) {
			var latestMetadata PrivateCloudMessageMetadata
			if metadataErr := d.runSocketStage(req, "cloud.pending_terminal_completion.replay_metadata_read", func() error {
				var readErr error
				latestMetadata, readErr = ReadPrivateCloudMessageMetadata(d.paths, op.Profile, op.LocalMessageID)
				return readErr
			}); metadataErr != nil {
				return false, socketError("UPSTREAM_ERROR", "could not read notification metadata", true)
			}
			var latestMessage MessageEnvelope
			if storeErr := d.runSocketStage(req, "cloud.pending_terminal_completion.replay_message_read", func() error {
				var readErr error
				latestMessage, readErr = d.store.GetInboxMessage(context.Background(), op.Profile, op.LocalMessageID)
				return readErr
			}); storeErr != nil {
				return false, classifyMessageStoreError(storeErr)
			}
			replayed, replayErr := d.replayCompletedCloudMessageMutation(latestMessage, latestMetadata, done, attempted.ClaimHolder)
			if replayErr != nil {
				return false, replayErr
			}
			if err := d.completeCloudNotificationClaimOperationForSocketRequest(req, duplicateAttempt, now, "cloud.pending_terminal_completion.duplicate_operation_complete"); err != nil {
				d.logCloudMutationFailed(req, duplicateAttempt, err)
				return false, socketError("UPSTREAM_ERROR", "could not complete duplicate notification operation", true)
			}
			d.logCloudMutationLocalComplete(req, attempted, time.Since(localStartedAt))
			d.syncMessageSpoolForDelivery(replayed, op.Operation)
			return true, nil
		}
		failure := errors.New("notification operation disappeared before local completion")
		_ = RecordCloudNotificationClaimOperationFailure(d.paths, duplicateAttempt, failure, now)
		d.logCloudMutationFailed(req, duplicateAttempt, failure)
		return false, socketError("UPSTREAM_ERROR", "notification operation result is uncertain", true)
	}
	if !sameCloudNotificationClaimOperation(pendingAfterMutation, attempted) {
		failure := errors.New("notification operation changed before local completion")
		_ = RecordCloudNotificationClaimOperationFailure(d.paths, duplicateAttempt, failure, now)
		d.logCloudMutationFailed(req, duplicateAttempt, failure)
		return false, socketError("CONFLICT", "notification operation id conflict", false)
	}
	if metadataErr := d.runSocketStage(req, "cloud.pending_terminal_completion.metadata_read", func() error {
		var readErr error
		metadata, readErr = ReadPrivateCloudMessageMetadata(d.paths, op.Profile, op.LocalMessageID)
		return readErr
	}); metadataErr != nil {
		_ = RecordCloudNotificationClaimOperationFailure(d.paths, duplicateAttempt, metadataErr, now)
		d.logCloudMutationFailed(req, duplicateAttempt, metadataErr)
		return false, socketError("UPSTREAM_ERROR", "could not read notification metadata", true)
	}
	if metadata.ClaimID != attempted.ClaimID || metadata.NotificationID != attempted.NotificationID {
		failure := errors.New("notification metadata changed before local completion")
		_ = RecordCloudNotificationClaimOperationFailure(d.paths, duplicateAttempt, failure, now)
		d.logCloudMutationFailed(req, duplicateAttempt, failure)
		return false, socketError("CONFLICT", "notification operation id conflict", false)
	}
	if storeErr := d.runSocketStage(req, "cloud.pending_terminal_completion.message_read", func() error {
		var readErr error
		message, readErr = d.store.GetInboxMessage(context.Background(), op.Profile, op.LocalMessageID)
		return readErr
	}); storeErr != nil {
		_ = RecordCloudNotificationClaimOperationFailure(d.paths, duplicateAttempt, storeErr, now)
		d.logCloudMutationFailed(req, duplicateAttempt, storeErr)
		return false, classifyMessageStoreError(storeErr)
	}
	if replayedCloudOperationMatchesLocalState(message, attempted) {
		if err := d.completeCloudNotificationClaimOperationForSocketRequest(req, attempted, now, "cloud.pending_terminal_completion.operation_complete"); err != nil {
			d.logCloudMutationFailed(req, attempted, err)
			return false, socketError("UPSTREAM_ERROR", "could not complete notification operation", true)
		}
		if err := d.completeCloudNotificationClaimOperationForSocketRequest(req, duplicateAttempt, now, "cloud.pending_terminal_completion.duplicate_operation_complete"); err != nil {
			d.logCloudMutationFailed(req, duplicateAttempt, err)
			return false, socketError("UPSTREAM_ERROR", "could not complete duplicate notification operation", true)
		}
		d.logCloudMutationLocalComplete(req, attempted, time.Since(localStartedAt))
		d.syncMessageSpoolForDelivery(MessageEnvelope{ID: op.LocalMessageID, Profile: op.Profile}, op.Operation)
		return true, nil
	}
	if message.Source != "comment.io" || message.Delivery.State != "claimed" || message.Delivery.ClaimHolder == nil || cloudClaimOperationHolderMismatch(message, attempted) {
		failure := errors.New("local delivery cannot safely replay operation")
		_ = RecordCloudNotificationClaimOperationFailure(d.paths, duplicateAttempt, failure, now)
		d.logCloudMutationFailed(req, duplicateAttempt, failure)
		return false, socketError("CONFLICT", "notification operation id conflict", false)
	}
	var updated MessageEnvelope
	if storeErr := d.runSocketStage(req, "cloud.pending_terminal_completion.ack_message", func() error {
		var err error
		updated, err = d.store.AckCloudMessage(context.Background(), op.Profile, op.LocalMessageID, stringValue(message.Delivery.ClaimHolder), now)
		return err
	}); storeErr != nil {
		_ = RecordCloudNotificationClaimOperationFailure(d.paths, duplicateAttempt, storeErr, now)
		d.logCloudMutationFailed(req, duplicateAttempt, storeErr)
		return false, classifyMessageStoreError(storeErr)
	}
	if err := d.completeCloudNotificationClaimOperationForSocketRequest(req, attempted, now, "cloud.pending_terminal_completion.operation_complete"); err != nil {
		d.logCloudMutationFailed(req, attempted, err)
		return false, socketError("UPSTREAM_ERROR", "could not complete notification operation", true)
	}
	if err := d.completeCloudNotificationClaimOperationForSocketRequest(req, duplicateAttempt, now, "cloud.pending_terminal_completion.duplicate_operation_complete"); err != nil {
		d.logCloudMutationFailed(req, duplicateAttempt, err)
		return false, socketError("UPSTREAM_ERROR", "could not complete duplicate notification operation", true)
	}
	d.logCloudMutationLocalComplete(req, attempted, time.Since(localStartedAt))
	d.syncMessageSpoolForDelivery(updated, op.Operation)
	return true, nil
}

func (d *Daemon) releaseDeclinedCloudNotificationLease(profile string, messageID string, profileConfig AgentProfile, lease CloudNotificationLease, notificationID string, now time.Time) *SocketError {
	if d.notificationClient == nil {
		return socketError("UPSTREAM_ERROR", "notification client is not configured", true)
	}
	req := internalCloudReleaseSocketRequest("cloud.declined_duplicate_release", profile, messageID)
	var op CloudNotificationClaimOperation
	var done bool
	if err := d.runSocketStage(req, "cloud.declined_release_operation_prepare", func() error {
		var prepareErr error
		op, done, prepareErr = BeginDeclinedDuplicateCloudNotificationReleaseOperation(d.paths, profile, messageID, lease.ClaimID, notificationID, now)
		return prepareErr
	}); err != nil {
		return socketError("UPSTREAM_ERROR", "could not prepare duplicate notification release", true)
	}
	if done {
		return nil
	}
	attempted, attemptErr := d.recordCloudNotificationClaimOperationAttempt(req, op, now, "could not prepare duplicate notification release")
	if attemptErr != nil {
		return attemptErr
	}
	d.logCloudMutationStart(req, attempted)
	remoteStartedAt := time.Now()
	mutation, err := d.releaseNotificationForSocketRequest(req, profileConfig, attempted.ClaimID, attempted.OpID, "cloud.remote_release")
	if err != nil {
		d.logCloudMutationRemoteEnd(req, attempted, time.Since(remoteStartedAt), err)
		d.logCloudMutationFailed(req, attempted, err)
		if errors.Is(err, errNotificationMutationDeadline) || errors.Is(err, errNotificationMutationAmbiguous) {
			_ = RecordCloudNotificationClaimOperationFailure(d.paths, attempted, err, now)
			return nil
		}
		_ = AbandonCloudNotificationClaimOperation(d.paths, attempted)
		var statusErr *NotificationHTTPError
		if errors.As(err, &statusErr) && (statusErr.Status == 404 || statusErr.Status == 409) {
			return nil
		}
		return socketError("UPSTREAM_ERROR", "could not release duplicate notification lease", true)
	}
	d.logCloudMutationRemoteEnd(req, attempted, time.Since(remoteStartedAt), nil)
	if mutation == nil || mutation.ClaimID != attempted.ClaimID || mutation.NotificationID != attempted.NotificationID {
		failure := errors.New("notification duplicate release response mismatch")
		_ = RecordCloudNotificationClaimOperationFailure(d.paths, attempted, failure, now)
		d.logCloudMutationFailed(req, attempted, failure)
		return socketError("UPSTREAM_ERROR", "notification duplicate release response mismatch", true)
	}
	localStartedAt := time.Now()
	if err := d.completeCloudNotificationClaimOperationForSocketRequest(req, attempted, now, "cloud.declined_release_local_complete"); err != nil {
		d.logCloudMutationFailed(req, attempted, err)
		return socketError("UPSTREAM_ERROR", "could not complete duplicate notification release", true)
	}
	d.logCloudMutationLocalComplete(req, attempted, time.Since(localStartedAt))
	return nil
}

func sameCloudNotificationTarget(aProfile AgentProfile, aBot BotRegistryEntry, bProfile AgentProfile, bBot BotRegistryEntry) bool {
	return aProfile.Handle == bProfile.Handle &&
		aProfile.AgentSecret == bProfile.AgentSecret &&
		aProfile.BaseURL == bProfile.BaseURL &&
		aProfile.Path == bProfile.Path &&
		aBot.Name == bBot.Name &&
		aBot.Handle == bBot.Handle &&
		aBot.CredentialPath == bBot.CredentialPath &&
		sameBotBrainRef(aBot.BrainRef, bBot.BrainRef) &&
		aBot.ManagedSession == bBot.ManagedSession
}

func sameBotBrainRef(a *BotBrainRef, b *BotBrainRef) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.WorkspaceID == b.WorkspaceID &&
		a.OwnerAgentID == b.OwnerAgentID &&
		a.BotAgentID == b.BotAgentID &&
		a.ContainerID == b.ContainerID &&
		a.RootFolderID == b.RootFolderID &&
		a.RelativePath == b.RelativePath &&
		a.SetupGeneration == b.SetupGeneration
}

func (d *Daemon) releaseStaleCloudNotificationLease(profile string, profileConfig AgentProfile, lease CloudNotificationLease, notificationID string, now time.Time) *SocketError {
	if d.notificationClient == nil {
		return socketError("UPSTREAM_ERROR", "notification client is not configured", true)
	}
	if !isSafeCloudID("claim", lease.ClaimID) {
		return socketError("UPSTREAM_ERROR", "invalid notification lease", true)
	}
	messageID, err := GenerateLocalID("msg", 0)
	if err != nil {
		return socketError("UPSTREAM_ERROR", "could not allocate stale notification release", true)
	}
	req := internalCloudReleaseSocketRequest("cloud.stale_notification_release", profile, messageID)
	var op CloudNotificationClaimOperation
	var done bool
	if err := d.runSocketStage(req, "cloud.stale_notification_operation_prepare", func() error {
		var prepareErr error
		op, done, prepareErr = BeginDeclinedDuplicateCloudNotificationReleaseOperation(d.paths, profile, messageID, lease.ClaimID, notificationID, now)
		return prepareErr
	}); err != nil {
		return socketError("UPSTREAM_ERROR", "could not prepare stale notification release", true)
	}
	if done {
		return nil
	}
	attempted, attemptErr := d.recordCloudNotificationClaimOperationAttempt(req, op, now, "could not prepare stale notification release")
	if attemptErr != nil {
		return attemptErr
	}
	d.logCloudMutationStart(req, attempted)
	remoteStartedAt := time.Now()
	mutation, err := d.releaseNotificationForSocketRequest(req, profileConfig, attempted.ClaimID, attempted.OpID, "cloud.remote_release")
	if err != nil {
		d.logCloudMutationRemoteEnd(req, attempted, time.Since(remoteStartedAt), err)
		d.logCloudMutationFailed(req, attempted, err)
		if errors.Is(err, errNotificationMutationDeadline) || errors.Is(err, errNotificationMutationAmbiguous) {
			_ = RecordCloudNotificationClaimOperationFailure(d.paths, attempted, err, now)
			return nil
		}
		_ = AbandonCloudNotificationClaimOperation(d.paths, attempted)
		var statusErr *NotificationHTTPError
		if errors.As(err, &statusErr) && (statusErr.Status == 404 || statusErr.Status == 409) {
			return nil
		}
		d.logger.warn("notification.stale_target_release_failed", map[string]any{
			"profile": profileConfig.Handle,
			"reason":  "release_failed",
		})
		return socketError("UPSTREAM_ERROR", "could not release stale notification lease", true)
	}
	d.logCloudMutationRemoteEnd(req, attempted, time.Since(remoteStartedAt), nil)
	if mutation == nil || mutation.ClaimID != attempted.ClaimID || mutation.NotificationID != attempted.NotificationID {
		failure := errors.New("notification stale release response mismatch")
		_ = RecordCloudNotificationClaimOperationFailure(d.paths, attempted, failure, now)
		d.logCloudMutationFailed(req, attempted, failure)
		d.logger.warn("notification.stale_target_release_failed", map[string]any{
			"profile": profileConfig.Handle,
			"reason":  "response_mismatch",
		})
		return socketError("UPSTREAM_ERROR", "notification stale release response mismatch", true)
	}
	localStartedAt := time.Now()
	if err := d.completeCloudNotificationClaimOperationForSocketRequest(req, attempted, now, "cloud.stale_notification_local_complete"); err != nil {
		d.logCloudMutationFailed(req, attempted, err)
		return socketError("UPSTREAM_ERROR", "could not complete stale notification release", true)
	}
	d.logCloudMutationLocalComplete(req, attempted, time.Since(localStartedAt))
	d.logger.info("notification.stale_target_released", map[string]any{
		"profile":     profileConfig.Handle,
		"released_at": busTime(now.UTC()),
	})
	return nil
}

func (d *Daemon) hasReadyLocalMessageForCloudIngest(profile string, botName string) (bool, *SocketError) {
	ctx := contextWithSocketRequest(context.Background(), internalCloudReleaseSocketRequest("messages.cloud_ingest_ready_check", profile, ""))
	for {
		d.lockBusForContext(ctx)
		summary, storeErr := d.waitCurrentReadyMessageSummaryLocked(ctx, MessageListFilter{Profile: profile, BotName: botName}, time.Now().UTC())
		d.busMu.Unlock()
		if stale, ok := staleBotletsTaskTargetFromError(storeErr); ok {
			if _, err := d.quarantineCloudMessageForStaleBotletsTaskTarget(ctx, stale.Profile, stale.MessageID, time.Now().UTC()); err != nil {
				return false, classifyMessageStoreError(err)
			}
			continue
		}
		if storeErr != nil {
			return false, classifyMessageStoreError(storeErr)
		}
		return summary != nil, nil
	}
}

func (d *Daemon) hasReadyCloudNotificationMessage(profile string, botName string) (bool, *SocketError) {
	req := internalCloudReleaseSocketRequest("messages.cloud_ingest_cloud_ready_check", profile, "")
	nudgeRecords := d.automaticNudgeBlockRecords()
	ready, _, err := d.scanReadyCloudNotificationMessages(contextWithSocketRequest(context.Background(), req), MessageListFilter{Profile: profile, BotName: botName, Source: "comment.io"}, time.Now().UTC(), nudgeRecords)
	if err != nil {
		return false, classifyMessageStoreError(err)
	}
	return ready, nil
}

func (d *Daemon) nextReadyCloudAutomaticNudgeDeadline(profile string, botName string, now time.Time) (*time.Time, *SocketError) {
	req := internalCloudReleaseSocketRequest("messages.cloud_nudge_deadline", profile, "")
	nudgeRecords := d.automaticNudgeBlockRecords()
	_, nextEligibleAt, err := d.scanReadyCloudNotificationMessages(contextWithSocketRequest(context.Background(), req), MessageListFilter{Profile: profile, BotName: botName, Source: "comment.io"}, now, nudgeRecords)
	if err != nil {
		return nil, classifyMessageStoreError(err)
	}
	return nextEligibleAt, nil
}

func (d *Daemon) scanReadyCloudNotificationMessages(ctx context.Context, filter MessageListFilter, now time.Time, nudgeRecords []SessionRecord) (bool, *time.Time, error) {
	cursor := filter.Cursor
	var nextEligibleAt *time.Time
	for {
		pageFilter := filter
		pageFilter.Cursor = cursor
		var summaries []MessageWaitSummary
		if err := d.runSocketStageForContext(ctx, "cloud_ingest.cloud_ready_message", func() error {
			var storeErr error
			summaries, storeErr = d.store.WaitMessageSummaries(ctx, pageFilter, dispatchReadySummaryLimit)
			return storeErr
		}); err != nil {
			return false, nil, err
		}
		if len(summaries) == 0 {
			return false, nextEligibleAt, nil
		}
		for i := range summaries {
			summary := summaries[i]
			if summary.Source != "comment.io" {
				continue
			}
			blocked, deadline := d.cloudMessageAutoNudgeBlockDeadline(summary, now, nudgeRecords)
			if blocked {
				if deadline != nil && deadline.After(now.UTC()) && (nextEligibleAt == nil || deadline.Before(*nextEligibleAt)) {
					value := *deadline
					nextEligibleAt = &value
				}
				continue
			}
			return true, nextEligibleAt, nil
		}
		if len(summaries) < dispatchReadySummaryLimit {
			return false, nextEligibleAt, nil
		}
		cursor = summaries[len(summaries)-1].MessageID
	}
}

func (d *Daemon) lockCloudNotificationWait(ctx context.Context, profile string) (func(), bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	d.cloudWaitMu.Lock()
	lock := d.cloudWaitLocks[profile]
	if lock == nil {
		lock = make(chan struct{}, 1)
		lock <- struct{}{}
		d.cloudWaitLocks[profile] = lock
	}
	d.cloudWaitMu.Unlock()
	select {
	case <-lock:
		return func() { lock <- struct{}{} }, true
	case <-ctx.Done():
		return func() {}, false
	}
}

func (d *Daemon) shouldAdoptCloudNotificationLease(message MessageEnvelope, now time.Time) (bool, bool, *SocketError) {
	if message.Source != "comment.io" {
		return false, true, nil
	}
	if message.Delivery.State != "claimed" {
		if message.Delivery.State == "unclaimed" || message.Delivery.State == "released" {
			return false, true, nil
		}
		return false, false, nil
	}
	if message.Delivery.SessionID == nil {
		if leaseExpired(message.Delivery.LeaseExpiresAt, now) {
			return true, true, nil
		}
		return false, false, nil
	}
	record, err := ReadSessionRecord(d.paths, *message.Delivery.SessionID)
	if err != nil {
		if sessionRecordMissing(d.paths, *message.Delivery.SessionID) {
			return true, true, nil
		}
		return false, false, socketError("UPSTREAM_ERROR", "could not read session", true)
	}
	if shouldRetryStaleBmuxCleanupForClaim(record, message) {
		if cleanupErr := d.cleanupStaleBmuxControlSessionLocked(record, "stale_cloud_claim_reopen"); cleanupErr != nil {
			return false, false, cleanupErr
		}
	}
	if leaseExpired(message.Delivery.LeaseExpiresAt, now) {
		return true, true, nil
	}
	if record.Profile != message.Profile {
		return true, true, nil
	}
	if record.State != "alive" {
		return true, true, nil
	}
	if record.BotName != message.BotName {
		return true, true, nil
	}
	if message.Delivery.SessionGeneration == nil || record.Generation != *message.Delivery.SessionGeneration {
		return true, true, nil
	}
	staleReason := "tmux_session_missing"
	record, live, liveErr := d.recoverLiveTmuxSessionLockedForClaimReopen(record)
	if liveErr != nil {
		return false, false, socketError("UPSTREAM_ERROR", "could not inspect tmux session", true)
	}
	if live {
		runtimeReason, runtimeErr := d.sessionRuntimeIssueLocked(record, false)
		if runtimeErr != nil {
			return false, false, runtimeErr
		}
		if runtimeReason == "" {
			return false, false, nil
		}
		staleReason = runtimeReason
	}
	record.State = "stale"
	if err := d.writeSessionRecordLocked(record); err != nil {
		return false, false, socketError("UPSTREAM_ERROR", "could not mark session stale", true)
	}
	if cleanupErr := d.cleanupStaleBmuxControlSessionLocked(record, staleReason); cleanupErr != nil {
		return false, false, cleanupErr
	}
	return true, true, nil
}

func shouldRetryStaleBmuxCleanupForClaim(record SessionRecord, message MessageEnvelope) bool {
	if normalizeSessionHost(record.Host) != SessionHostBmux {
		return false
	}
	if record.State != "stale" || record.BotName != message.BotName {
		return false
	}
	return message.Delivery.SessionGeneration == nil || record.Generation == *message.Delivery.SessionGeneration
}

func sessionRecordMissing(paths Paths, sessionID string) bool {
	if !LocalSessionIDRE.MatchString(sessionID) {
		return false
	}
	_, err := os.Stat(sessionRecordPath(paths, sessionID))
	return errors.Is(err, os.ErrNotExist)
}

func readyCloudMessageForManagedDispatch(message MessageEnvelope, now time.Time) bool {
	return message.Source == "comment.io" && message.Delivery.State == "unclaimed" && message.Delivery.LeaseExpiresAt != nil && !leaseExpired(message.Delivery.LeaseExpiresAt, now)
}

func (d *Daemon) cloudNotificationTarget(profile string, botName string, allowProfileOnly bool) (AgentProfile, BotRegistryEntry, bool) {
	d.profileMu.RLock()
	defer d.profileMu.RUnlock()
	return d.cloudNotificationTargetLocked(profile, botName, allowProfileOnly)
}

func (d *Daemon) cloudNotificationTargetLocked(profile string, botName string, allowProfileOnly bool) (AgentProfile, BotRegistryEntry, bool) {
	profileConfig, profileOK := d.profileState.AgentProfiles[profile]
	if !profileOK {
		return AgentProfile{}, BotRegistryEntry{}, false
	}
	if botName != "" {
		bot, ok := d.profileState.BotRegistry[botName]
		if !ok {
			for _, candidate := range d.profileState.BotRegistry {
				if candidate.MatchesDaemonSelector(botName) {
					bot = candidate
					ok = true
					break
				}
			}
		}
		if !ok || bot.Handle != profile {
			if allowProfileOnly {
				bot = profileOnlyNotificationBot(profile)
				if bot.Name == botName {
					return profileConfig, bot, true
				}
			}
			return AgentProfile{}, BotRegistryEntry{}, false
		}
		return profileConfig, bot, true
	}
	names := make([]string, 0, len(d.profileState.BotRegistry))
	for name := range d.profileState.BotRegistry {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		bot := d.profileState.BotRegistry[name]
		if bot.Handle == profile {
			return profileConfig, bot, true
		}
	}
	if allowProfileOnly {
		return profileConfig, profileOnlyNotificationBot(profile), true
	}
	return AgentProfile{}, BotRegistryEntry{}, false
}

func profileOnlyNotificationBot(profile string) BotRegistryEntry {
	name := "agent"
	if dot := strings.LastIndex(profile, "."); dot >= 0 && dot < len(profile)-1 {
		suffix := profile[dot+1:]
		if isBotName(suffix) {
			name = suffix
		}
	}
	return BotRegistryEntry{Name: name, Handle: profile}
}

func (d *Daemon) botletsTaskSummaryMatchesCurrentTarget(summary *MessageWaitSummary) bool {
	task, ok := botletsTaskFromMessageRefs(summary.Refs)
	if !ok || !validateCloudBotletsTaskNotification(task) {
		return false
	}
	d.profileMu.RLock()
	profileConfig, bot, targetOK := d.cloudNotificationTargetLocked(summary.Profile, summary.BotName, false)
	if !targetOK && (task.BotID != "" || task.BotAgentID != "") {
		for _, candidate := range d.profileState.BotRegistry {
			if !candidate.MatchesStableIdentity(task.BotID, task.BotAgentID) {
				continue
			}
			candidateProfileConfig, profileOK := d.profileState.AgentProfiles[candidate.Handle]
			if !profileOK {
				continue
			}
			profileConfig = candidateProfileConfig
			bot = candidate
			targetOK = true
			break
		}
	}
	d.profileMu.RUnlock()
	if !targetOK {
		return false
	}
	return d.botletsTaskNotificationMatchesCurrentTarget(task, bot, profileConfig)
}

func (d *Daemon) botletsTaskNotificationMatchesCurrentTarget(task *CloudBotletsTaskNotification, bot BotRegistryEntry, profileConfig AgentProfile) bool {
	if !validateCloudBotletsTaskTarget(task, bot, profileConfig) {
		return false
	}
	_, err := ValidateBotletsBrainProjection(d.paths, bot)
	return err == nil
}

func botletsTaskFromMessageRefs(refs map[string]any) (*CloudBotletsTaskNotification, bool) {
	if refs == nil || refString(refs, "notification_type") != "botlets_task" {
		return nil, false
	}
	task := &CloudBotletsTaskNotification{
		RunID:               refString(refs, "run_id"),
		Kind:                refString(refs, "task_kind"),
		OwnerAgentID:        refString(refs, "owner_agent_id"),
		BotID:               refString(refs, "bot_id"),
		BotAgentID:          refString(refs, "bot_agent_id"),
		BotSlug:             refString(refs, "bot_slug"),
		BotName:             refString(refs, "bot_name"),
		BotHandle:           refString(refs, "bot_handle"),
		ScheduledFor:        refString(refs, "scheduled_for"),
		EnqueuedAt:          refString(refs, "enqueued_at"),
		ScheduleVersion:     refInt(refs, "schedule_version"),
		ExecutionGeneration: refInt(refs, "execution_generation"),
		SetupGeneration:     refInt(refs, "setup_generation"),
		Cron:                refString(refs, "cron"),
		Timezone:            refString(refs, "timezone"),
	}
	return task, true
}

func refString(refs map[string]any, key string) string {
	value, _ := refs[key].(string)
	return value
}

func refInt(refs map[string]any, key string) int {
	switch value := refs[key].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		if value == float64(int(value)) {
			return int(value)
		}
	}
	return 0
}

func (d *Daemon) receiveMessage(req SocketRequest) (MessageEnvelope, *SocketError) {
	unlock, lockErr := d.lockSessionAuthIfNeeded(req)
	if lockErr != nil {
		return MessageEnvelope{}, lockErr
	}
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()
	if req.Auth != nil && req.Auth.Mode == "session" && d.sessionAuthDailyResetPendingLocked(req) {
		return MessageEnvelope{}, socketError("CONFLICT", "session is resetting", false)
	}
	authority, err := d.messageMutationAuthority(req)
	if err != nil {
		return MessageEnvelope{}, err
	}
	messageID := req.Params["message_id"].(string)
	d.lockBusForSocketRequest(req)
	existing, storeErr := d.getInboxMessageForAuthority(req, authority, messageID, "local.receive_get_message")
	if storeErr != nil {
		d.busMu.Unlock()
		return MessageEnvelope{}, classifyMessageStoreError(storeErr)
	}
	if matchErr := requireMessageBotScope(req, authority, existing); matchErr != nil {
		d.busMu.Unlock()
		return MessageEnvelope{}, matchErr
	}
	now := time.Now().UTC()
	if existing.Source == "comment.io" {
		if existing.Delivery.State != "unclaimed" || existing.Delivery.LeaseExpiresAt == nil || leaseExpired(existing.Delivery.LeaseExpiresAt, now) {
			d.busMu.Unlock()
			return MessageEnvelope{}, classifyMessageStoreError(ErrMessageConflict)
		}
		if metadataErr := d.runSocketStage(req, "cloud.receive_metadata_read", func() error {
			_, err := ReadPrivateCloudMessageMetadata(d.paths, existing.Profile, messageID)
			return err
		}); metadataErr != nil {
			reqCtx := contextWithSocketRequest(context.Background(), req)
			_, err := d.quarantineCloudMessageForMissingMetadata(reqCtx, existing.Profile, messageID, now)
			if err != nil {
				d.busMu.Unlock()
				return MessageEnvelope{}, classifyMessageStoreError(err)
			}
			d.busMu.Unlock()
			return MessageEnvelope{}, socketError("UPSTREAM_ERROR", "could not read notification metadata", true)
		}
		if existing.Kind == "botlets.task" {
			summary := &MessageWaitSummary{
				MessageID: existing.ID,
				Profile:   existing.Profile,
				BotName:   existing.BotName,
				Kind:      existing.Kind,
				Source:    existing.Source,
				Refs:      existing.Refs,
			}
			if !d.botletsTaskSummaryMatchesCurrentTarget(summary) {
				d.busMu.Unlock()
				unlock()
				unlock = nil
				reqCtx := contextWithSocketRequest(context.Background(), req)
				if _, err := d.quarantineCloudMessageForStaleBotletsTaskTarget(reqCtx, existing.Profile, messageID, now); err != nil {
					return MessageEnvelope{}, classifyMessageStoreError(err)
				}
				return MessageEnvelope{}, classifyMessageStoreError(ErrMessageConflict)
			}
		}
	}
	var message MessageEnvelope
	storeErr = d.runSocketStage(req, "local.receive_claim_message", func() error {
		var err error
		message, err = d.store.ClaimMessage(context.Background(), MessageClaimOptions{
			Profile:           existing.Profile,
			MessageID:         messageID,
			ClaimHolder:       authority.ClaimHolder,
			SessionID:         authority.SessionID,
			SessionScopeType:  authority.SessionScopeType,
			SessionScopeID:    authority.SessionScopeID,
			SessionGeneration: authority.SessionGeneration,
			LeaseTTL:          defaultLocalLeaseTTL,
			Now:               now,
		})
		return err
	})
	d.busMu.Unlock()
	if storeErr != nil {
		return MessageEnvelope{}, classifyMessageStoreError(storeErr)
	}
	d.syncMessageSpoolForDelivery(message, "receive")
	d.logMessageReceived(message)
	return message, nil
}

func (d *Daemon) cloudReplayProtectionForMessage(message MessageEnvelope) map[string]any {
	if message.Source != "comment.io" {
		return nil
	}
	metadata, err := ReadPrivateCloudMessageMetadata(d.paths, message.Profile, message.ID)
	if err != nil || metadata.NotificationID == "" {
		return nil
	}
	targetType := "document"
	targetID := stringRefValue(message.Refs, "doc_slug")
	if commentID := stringRefValue(message.Refs, "comment_id"); commentID != "" {
		targetType = "comment"
		targetID = commentID
	} else if suggestionID := stringRefValue(message.Refs, "suggestion_id"); suggestionID != "" {
		targetType = "suggestion"
		targetID = suggestionID
	} else if message.ThreadID != nil && *message.ThreadID != "" {
		targetType = "thread"
		targetID = *message.ThreadID
	}
	intent := "visible_response"
	raw := strings.Join([]string{metadata.NotificationID, targetType, targetID, intent}, "\n")
	sum := sha256.Sum256([]byte(raw))
	return map[string]any{
		"key":         "nrp_" + hex.EncodeToString(sum[:])[:32],
		"target_type": targetType,
		"target_id":   targetID,
		"intent":      intent,
	}
}

func (d *Daemon) messageReceiveResult(message MessageEnvelope) map[string]any {
	result := map[string]any{"message": message}
	if replay := d.cloudReplayProtectionForMessage(message); replay != nil {
		result["replay_protection"] = replay
	}
	return result
}

func terminalCloudReplaySkippedResult(message MessageEnvelope, outcome string) map[string]any {
	return map[string]any{
		"message_id":      message.ID,
		"delivery":        message.Delivery,
		"replay_skipped":  true,
		"skip_outcome":    outcome,
		"settled_locally": true,
	}
}

func terminalCloudReplayOutcomeSkippable(outcome string) bool {
	switch outcome {
	case "responded", "no_response", "cancelled", "superseded":
		return true
	default:
		return false
	}
}

func terminalCloudReplayAlreadyAcked(err error) bool {
	var statusErr *NotificationHTTPError
	return errors.As(err, &statusErr) && statusErr.Status == 409 && statusErr.Code == "LEASE_ALREADY_ACKED"
}

func terminalCloudReplayAckRequest(req SocketRequest, messageID string) SocketRequest {
	ackReq := req
	ackReq.Params = map[string]any{"message_id": messageID}
	if bot, ok := req.Params["bot"].(string); ok && bot != "" {
		ackReq.Params["bot"] = bot
	}
	if profile, ok := req.Params["profile"].(string); ok && profile != "" {
		ackReq.Params["profile"] = profile
	}
	return ackReq
}

func (d *Daemon) skipTerminalCloudReplay(req SocketRequest, message MessageEnvelope) (MessageEnvelope, string, bool, *SocketError) {
	if message.Source != "comment.io" {
		return MessageEnvelope{}, "", false, nil
	}
	authority, authErr := d.messageMutationAuthority(req)
	if authErr != nil {
		return MessageEnvelope{}, "", false, authErr
	}
	credentialProfile := d.cloudMutationCredentialProfile(authority, message)
	ctx, cancel := context.WithTimeout(contextWithSocketRequest(context.Background(), req), 2*time.Second)
	defer cancel()
	result, err := d.publishCloudMessageHandlingActivityResultForProfiles(ctx, credentialProfile, message.Profile, message.ID, "start", "")
	if terminalCloudReplayAlreadyAcked(err) {
		ackReq := terminalCloudReplayAckRequest(req, message.ID)
		updated, ackErr := d.ackMessage(ackReq)
		if ackErr != nil {
			return MessageEnvelope{}, "already_acked", false, ackErr
		}
		d.logger.info("notification.replay_skipped", map[string]any{
			"profile":    message.Profile,
			"message_id": message.ID,
			"outcome":    "already_acked",
		})
		return updated, "already_acked", true, nil
	}
	if err != nil || result == nil || !result.Ignored || !terminalCloudReplayOutcomeSkippable(result.TerminalOutcome) {
		return MessageEnvelope{}, "", false, nil
	}
	ackReq := terminalCloudReplayAckRequest(req, message.ID)
	updated, ackErr := d.ackMessage(ackReq)
	if ackErr != nil {
		return MessageEnvelope{}, result.TerminalOutcome, false, ackErr
	}
	d.logger.info("notification.replay_skipped", map[string]any{
		"profile":    message.Profile,
		"message_id": message.ID,
		"outcome":    result.TerminalOutcome,
	})
	return updated, result.TerminalOutcome, true, nil
}

func stringRefValue(refs map[string]any, key string) string {
	if refs == nil {
		return ""
	}
	value, _ := refs[key].(string)
	return value
}

func (d *Daemon) renewMessage(req SocketRequest) (MessageEnvelope, *SocketError) {
	unlock, lockErr := d.lockSessionAuthIfNeeded(req)
	if lockErr != nil {
		return MessageEnvelope{}, lockErr
	}
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()
	authority, err := d.messageMutationAuthority(req)
	if err != nil {
		return MessageEnvelope{}, err
	}
	ttl := defaultLocalLeaseTTL
	if raw, ok := req.Params["lease_ttl_ms"]; ok {
		ttl = time.Duration(numberParam(raw)) * time.Millisecond
	}
	d.lockBusForSocketRequest(req)
	preflightMessage, storeErr := d.getInboxMessageForAuthority(req, authority, req.Params["message_id"].(string), "local.preflight_get_message")
	if storeErr != nil {
		d.busMu.Unlock()
		return MessageEnvelope{}, classifyMessageStoreError(storeErr)
	}
	if matchErr := requireMessageBotScope(req, authority, preflightMessage); matchErr != nil {
		d.busMu.Unlock()
		return MessageEnvelope{}, matchErr
	}
	if preflightMessage.Source == "comment.io" {
		unlock()
		unlock = nil
		return d.renewCloudMessageLocked(req, authority, preflightMessage, ttl)
	}
	if _, ok := req.Params["op_id"]; ok {
		d.busMu.Unlock()
		return MessageEnvelope{}, socketError("VALIDATION_ERROR", "op_id is only supported for cloud-backed messages", false)
	}
	var message MessageEnvelope
	storeErr = d.runSocketStage(req, "local.renew_message", func() error {
		var err error
		message, err = d.store.RenewMessage(context.Background(), preflightMessage.Profile, req.Params["message_id"].(string), authority.ClaimHolder, ttl, time.Now().UTC())
		return err
	})
	d.busMu.Unlock()
	if storeErr != nil {
		return MessageEnvelope{}, classifyMessageStoreError(storeErr)
	}
	unlock()
	unlock = nil
	d.logLeaseRenewed(message)
	return message, nil
}

func (d *Daemon) ackMessage(req SocketRequest) (MessageEnvelope, *SocketError) {
	unlock, lockErr := d.lockSessionAuthIfNeeded(req)
	if lockErr != nil {
		return MessageEnvelope{}, lockErr
	}
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()
	authority, err := d.messageMutationAuthority(req)
	if err != nil {
		return MessageEnvelope{}, err
	}
	d.lockBusForSocketRequest(req)
	preflightMessage, storeErr := d.getInboxMessageForAuthority(req, authority, req.Params["message_id"].(string), "local.preflight_get_message")
	if storeErr != nil {
		d.busMu.Unlock()
		return MessageEnvelope{}, classifyMessageStoreError(storeErr)
	}
	if matchErr := requireMessageBotScope(req, authority, preflightMessage); matchErr != nil {
		d.busMu.Unlock()
		return MessageEnvelope{}, matchErr
	}
	if preflightMessage.Source == "comment.io" {
		unlock()
		unlock = nil
		updated, finishErr := d.finishCloudMessageLocked(req, authority, preflightMessage, "ack")
		if finishErr != nil {
			return MessageEnvelope{}, finishErr
		}
		d.syncMessageSpoolForDelivery(updated, "ack")
		_ = d.nudgeNextReadyForSessionAuthority(req, authority)
		return updated, nil
	}
	if _, ok := req.Params["op_id"]; ok {
		d.busMu.Unlock()
		return MessageEnvelope{}, socketError("VALIDATION_ERROR", "op_id is only supported for cloud-backed messages", false)
	}
	var message MessageEnvelope
	storeErr = d.runSocketStage(req, "local.ack_message", func() error {
		var err error
		message, err = d.store.AckMessage(context.Background(), preflightMessage.Profile, req.Params["message_id"].(string), authority.ClaimHolder, time.Now().UTC())
		return err
	})
	d.busMu.Unlock()
	if storeErr != nil {
		return MessageEnvelope{}, classifyMessageStoreError(storeErr)
	}
	unlock()
	unlock = nil
	d.syncMessageSpoolForDelivery(message, "ack")
	_ = d.nudgeNextReadyForSessionAuthority(req, authority)
	return message, nil
}

func (d *Daemon) releaseMessage(req SocketRequest) (MessageEnvelope, *SocketError) {
	unlock, lockErr := d.lockSessionAuthIfNeeded(req)
	if lockErr != nil {
		return MessageEnvelope{}, lockErr
	}
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()
	authority, err := d.messageMutationAuthority(req)
	if err != nil {
		return MessageEnvelope{}, err
	}
	reason, _ := req.Params["reason"].(string)
	d.lockBusForSocketRequest(req)
	preflightMessage, storeErr := d.getInboxMessageForAuthority(req, authority, req.Params["message_id"].(string), "local.preflight_get_message")
	if storeErr != nil {
		d.busMu.Unlock()
		return MessageEnvelope{}, classifyMessageStoreError(storeErr)
	}
	if matchErr := requireMessageBotScope(req, authority, preflightMessage); matchErr != nil {
		d.busMu.Unlock()
		return MessageEnvelope{}, matchErr
	}
	if preflightMessage.Source == "comment.io" {
		unlock()
		unlock = nil
		updated, finishErr := d.finishCloudMessageLocked(req, authority, preflightMessage, "release")
		if finishErr != nil {
			return MessageEnvelope{}, finishErr
		}
		d.syncMessageSpoolForDelivery(updated, "release")
		_ = d.nudgeNextReadyForSessionAuthority(req, authority)
		return updated, nil
	}
	if _, ok := req.Params["op_id"]; ok {
		d.busMu.Unlock()
		return MessageEnvelope{}, socketError("VALIDATION_ERROR", "op_id is only supported for cloud-backed messages", false)
	}
	var message MessageEnvelope
	storeErr = d.runSocketStage(req, "local.release_message", func() error {
		var err error
		message, err = d.store.ReleaseMessage(context.Background(), preflightMessage.Profile, req.Params["message_id"].(string), authority.ClaimHolder, reason, time.Now().UTC())
		return err
	})
	d.busMu.Unlock()
	if storeErr != nil {
		return MessageEnvelope{}, classifyMessageStoreError(storeErr)
	}
	unlock()
	unlock = nil
	d.syncMessageSpoolForDelivery(message, "release")
	_ = d.nudgeNextReadyForSessionAuthority(req, authority)
	return message, nil
}

func (d *Daemon) completeActivity(req SocketRequest) (MessageEnvelope, *SocketError) {
	unlock, lockErr := d.lockSessionAuthIfNeeded(req)
	if lockErr != nil {
		return MessageEnvelope{}, lockErr
	}
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()
	if req.Auth == nil || (req.Auth.Mode != "session" && req.Auth.Mode != "owner") {
		return MessageEnvelope{}, socketError("FORBIDDEN", "activity complete requires a managed session or runtime profile", false)
	}
	authority, err := d.messageMutationAuthority(req)
	if err != nil {
		return MessageEnvelope{}, err
	}
	messageID := req.Params["message_id"].(string)
	d.lockBusForSocketRequest(req)
	message, storeErr := d.getInboxMessageForAuthority(req, authority, messageID, "local.preflight_get_message")
	if storeErr != nil {
		d.busMu.Unlock()
		return MessageEnvelope{}, classifyMessageStoreError(storeErr)
	}
	if matchErr := requireMessageBotScope(req, authority, message); matchErr != nil {
		d.busMu.Unlock()
		return MessageEnvelope{}, matchErr
	}
	if message.Source != "comment.io" {
		if _, ok := req.Params["op_id"]; ok {
			d.busMu.Unlock()
			return MessageEnvelope{}, socketError("VALIDATION_ERROR", "op_id is only supported for cloud-backed messages", false)
		}
		var updated MessageEnvelope
		storeErr := d.runSocketStage(req, "local.activity_ack_message", func() error {
			var err error
			updated, err = d.store.AckMessage(context.Background(), message.Profile, messageID, authority.ClaimHolder, time.Now().UTC())
			return err
		})
		d.busMu.Unlock()
		if storeErr != nil {
			return MessageEnvelope{}, classifyMessageStoreError(storeErr)
		}
		unlock()
		unlock = nil
		d.syncMessageSpoolForDelivery(updated, "ack")
		_ = d.nudgeNextReadyForSessionAuthority(req, authority)
		return updated, nil
	}
	if _, ok := req.Params["op_id"].(string); !ok {
		opID, opErr := GenerateLocalID("op", 0)
		if opErr != nil {
			d.busMu.Unlock()
			return MessageEnvelope{}, socketError("UPSTREAM_ERROR", "could not allocate notification ack operation", true)
		}
		req.Params["op_id"] = opID
	}
	completeOpID := req.Params["op_id"].(string)

	if message.Delivery.State == "acked" && stringValue(message.Delivery.ClaimHolder) == authority.ClaimHolder {
		credentialProfile := d.cloudMutationCredentialProfile(authority, message)
		d.busMu.Unlock()
		unlock()
		unlock = nil
		reqCtx := contextWithSocketRequest(context.Background(), req)
		if publishErr := d.publishCloudMessageHandlingActivityForProfiles(reqCtx, credentialProfile, message.Profile, messageID, "complete", "no_response", completeOpID); publishErr != nil {
			return MessageEnvelope{}, socketError("UPSTREAM_ERROR", "could not publish handling completion", true)
		}
		d.syncMessageSpoolForDelivery(message, "ack")
		_ = d.nudgeNextReadyForSessionAuthority(req, authority)
		return message, nil
	}
	_, hasPendingAck, pendingErr := d.pendingCloudMessageMutationLocked(req, message.Profile, message.ID, "ack", 0)
	if pendingErr != nil {
		d.busMu.Unlock()
		return MessageEnvelope{}, pendingErr
	}
	if !hasPendingAck {
		latest, _, staleErr := d.rejectStaleCloudBotletsTaskMutationWithoutHoldingLocks(req, authority, message, time.Now().UTC(), func() {
			d.busMu.Unlock()
			if unlock != nil {
				unlock()
				unlock = nil
			}
		}, func() {
			d.lockBusForSocketRequest(req)
		})
		if staleErr != nil {
			d.busMu.Unlock()
			return MessageEnvelope{}, staleErr
		}
		message = latest
	}
	if err := validateCloudMessageMutation(message, authority.ClaimHolder, time.Now().UTC(), hasPendingAck); err != nil {
		d.busMu.Unlock()
		return MessageEnvelope{}, classifyMessageStoreError(err)
	}
	d.busMu.Unlock()

	d.lockBusForSocketRequest(req)
	latest, storeErr := d.getInboxMessageForAuthority(req, authority, messageID, "local.preflight_get_message")
	if storeErr != nil {
		d.busMu.Unlock()
		return MessageEnvelope{}, classifyMessageStoreError(storeErr)
	}
	if matchErr := requireMessageBotScope(req, authority, latest); matchErr != nil {
		d.busMu.Unlock()
		return MessageEnvelope{}, matchErr
	}
	if unlock != nil {
		unlock()
		unlock = nil
	}
	credentialProfile := d.cloudMutationCredentialProfile(authority, latest)
	updated, finishErr := d.finishCloudMessageLocked(req, authority, latest, "ack")
	if finishErr != nil {
		return MessageEnvelope{}, finishErr
	}
	reqCtx := contextWithSocketRequest(context.Background(), req)
	if publishErr := d.publishCloudMessageHandlingActivityForProfiles(reqCtx, credentialProfile, latest.Profile, messageID, "complete", "no_response", completeOpID); publishErr != nil {
		return MessageEnvelope{}, socketError("UPSTREAM_ERROR", "could not publish handling completion", true)
	}
	d.syncMessageSpoolForDelivery(updated, "ack")
	_ = d.nudgeNextReadyForSessionAuthority(req, authority)
	return updated, nil
}

func (d *Daemon) syncMessageSpoolForDelivery(message MessageEnvelope, operation string) {
	d.syncMessageSpoolForDeliveryWithContext(context.Background(), message, operation)
}

func (d *Daemon) syncMessageSpoolForDeliveryWithContext(ctx context.Context, message MessageEnvelope, operation string) {
	if operation == "receive" || operation == "ack" || operation == "release" {
		_ = d.runSocketStageForContext(ctx, "spool.delivery_remove", func() error {
			return RemoveMessageSpool(d.paths, message.Profile, message.ID)
		})
		return
	}
	if shouldWriteMessageSpool(message, time.Now().UTC()) {
		_ = d.runSocketStageForContext(ctx, "spool.delivery_write", func() error {
			return WriteMessageSpool(d.paths, message)
		})
		return
	}
	_ = d.runSocketStageForContext(ctx, "spool.delivery_remove", func() error {
		return RemoveMessageSpool(d.paths, message.Profile, message.ID)
	})
}

func (d *Daemon) quarantineCloudMessageForMissingMetadata(ctx context.Context, profile string, messageID string, now time.Time) (MessageEnvelope, error) {
	var updated MessageEnvelope
	if err := d.runSocketStageForContext(ctx, "cloud.missing_metadata_quarantine", func() error {
		var storeErr error
		updated, storeErr = d.store.QuarantineCloudMessage(ctx, profile, messageID, "private_metadata_unavailable", now)
		return storeErr
	}); err != nil {
		return MessageEnvelope{}, err
	}
	d.syncMessageSpoolForDeliveryWithContext(ctx, updated, "quarantine")
	return updated, nil
}

func (d *Daemon) quarantineCloudMessageForStaleBotletsTaskTarget(ctx context.Context, profile string, messageID string, now time.Time) (MessageEnvelope, error) {
	ctx = contextWithDiagnosticSocketRequest(ctx, internalCloudReleaseSocketRequest("cloud.stale_botlets_release", profile, messageID))
	var requests []SocketRequest
	if req, ok := socketRequestFromContext(ctx); ok {
		requests = []SocketRequest{req}
	}
	released, err := d.releaseStaleBotletsTaskCloudClaim(profile, messageID, now, requests...)
	if err != nil {
		return MessageEnvelope{}, err
	}
	if !released {
		return MessageEnvelope{}, errStaleBotletsTaskReleasePending
	}
	d.lockBusForContext(ctx)
	defer d.busMu.Unlock()
	latest, err := d.getInboxMessageForContext(ctx, profile, messageID, "stale.quarantine_get_message")
	if err != nil {
		return MessageEnvelope{}, err
	}
	if !d.cloudBotletsTaskMessageTargetStale(latest) {
		return latest, nil
	}
	var updated MessageEnvelope
	if err := d.runSocketStageForContext(ctx, "stale.quarantine_message", func() error {
		var quarantineErr error
		updated, quarantineErr = d.store.QuarantineCloudMessage(ctx, profile, messageID, staleBotletsTaskTargetReleaseReason, now)
		return quarantineErr
	}); err != nil {
		return MessageEnvelope{}, err
	}
	d.syncMessageSpoolForDeliveryWithContext(ctx, updated, "quarantine")
	return updated, nil
}

func (d *Daemon) cloudBotletsTaskMessageTargetStale(message MessageEnvelope) bool {
	if message.Source != "comment.io" || message.Kind != "botlets.task" {
		return false
	}
	summary := &MessageWaitSummary{
		MessageID: message.ID,
		Profile:   message.Profile,
		BotName:   message.BotName,
		Kind:      message.Kind,
		Source:    message.Source,
		Refs:      message.Refs,
	}
	return !d.botletsTaskSummaryMatchesCurrentTarget(summary)
}

func (d *Daemon) rejectStaleCloudBotletsTaskMutationWithoutHoldingLocks(
	req SocketRequest,
	authority messageAuthority,
	message MessageEnvelope,
	now time.Time,
	beforeRemote func(),
	afterRemote func(),
) (MessageEnvelope, bool, *SocketError) {
	if !d.cloudBotletsTaskMessageTargetStale(message) {
		return message, false, nil
	}
	beforeRemote()
	released, releaseErr := d.releaseStaleBotletsTaskCloudClaim(authority.Profile, message.ID, now, req)
	afterRemote()
	latest, storeErr := d.getInboxMessageForSocketRequest(req, authority.Profile, message.ID, "stale.mutation_get_message")
	if storeErr != nil {
		return MessageEnvelope{}, true, classifyMessageStoreError(storeErr)
	}
	if !d.cloudBotletsTaskMessageTargetStale(latest) {
		return latest, false, nil
	}
	if releaseErr != nil {
		return MessageEnvelope{}, true, classifyMessageStoreError(releaseErr)
	}
	if !released {
		return MessageEnvelope{}, true, classifyMessageStoreError(errStaleBotletsTaskReleasePending)
	}
	var updated MessageEnvelope
	quarantineErr := d.runSocketStage(req, "stale.mutation_quarantine_message", func() error {
		var err error
		updated, err = d.store.QuarantineCloudMessage(context.Background(), authority.Profile, message.ID, staleBotletsTaskTargetReleaseReason, now)
		return err
	})
	if quarantineErr != nil {
		return MessageEnvelope{}, true, classifyMessageStoreError(quarantineErr)
	}
	d.syncMessageSpoolForDeliveryWithContext(contextWithSocketRequest(context.Background(), req), updated, "quarantine")
	return MessageEnvelope{}, true, classifyMessageStoreError(ErrMessageConflict)
}

func (d *Daemon) releaseStaleBotletsTaskCloudClaim(profile string, messageID string, now time.Time, requests ...SocketRequest) (bool, error) {
	if d.notificationClient == nil {
		return true, nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	req := staleBotletsReleaseSocketRequest(profile, messageID, requests...)
	var metadata PrivateCloudMessageMetadata
	if err := d.runSocketStage(req, "cloud.stale_release_metadata_read", func() error {
		var metadataErr error
		metadata, metadataErr = ReadPrivateCloudMessageMetadata(d.paths, profile, messageID)
		return metadataErr
	}); err != nil {
		return true, nil
	}
	profileConfig, ok := d.cloudNotificationProfile(profile)
	if !ok {
		return true, nil
	}
	var op CloudNotificationClaimOperation
	var done bool
	if err := d.runSocketStage(req, "cloud.stale_release_operation_prepare", func() error {
		var prepareErr error
		op, done, prepareErr = BeginCloudNotificationClaimOperation(
			d.paths,
			"release",
			profile,
			messageID,
			metadata.ClaimID,
			metadata.NotificationID,
			"",
			0,
			now,
			staleBotletsTaskTargetReleaseReason,
		)
		return prepareErr
	}); err != nil {
		return false, err
	}
	if done {
		return true, nil
	}
	attempted, attemptErr := d.recordCloudNotificationClaimOperationAttempt(req, op, now, "could not prepare notification operation")
	if attemptErr != nil {
		return false, errors.New(attemptErr.Message)
	}
	d.logCloudMutationStart(req, attempted)
	remoteStartedAt := time.Now()
	mutation, err := d.releaseNotificationForSocketRequest(req, profileConfig, attempted.ClaimID, attempted.OpID, "cloud.remote_release")
	if err != nil {
		d.logCloudMutationRemoteEnd(req, attempted, time.Since(remoteStartedAt), err)
		d.logCloudMutationFailed(req, attempted, err)
		if errors.Is(err, errNotificationMutationDeadline) || errors.Is(err, errNotificationMutationAmbiguous) {
			_ = RecordCloudNotificationClaimOperationFailure(d.paths, attempted, err, now)
			return false, nil
		}
		_ = AbandonCloudNotificationClaimOperation(d.paths, attempted)
		var statusErr *NotificationHTTPError
		if errors.As(err, &statusErr) && (statusErr.Status == 404 || statusErr.Status == 409) {
			return true, nil
		}
		return false, err
	}
	d.logCloudMutationRemoteEnd(req, attempted, time.Since(remoteStartedAt), nil)
	if mutation == nil || mutation.ClaimID != attempted.ClaimID || mutation.NotificationID != attempted.NotificationID {
		failure := errors.New("notification stale botlets release response mismatch")
		_ = RecordCloudNotificationClaimOperationFailure(d.paths, attempted, failure, now)
		d.logCloudMutationFailed(req, attempted, failure)
		return false, failure
	}
	localStartedAt := time.Now()
	if err := d.completeCloudNotificationClaimOperationForSocketRequest(req, attempted, now, "cloud.stale_release_local_complete"); err != nil {
		d.logCloudMutationFailed(req, attempted, err)
		return false, err
	}
	d.logCloudMutationLocalComplete(req, attempted, time.Since(localStartedAt))
	return true, nil
}

func staleBotletsReleaseSocketRequest(profile string, messageID string, requests ...SocketRequest) SocketRequest {
	if len(requests) > 0 && shouldLogSocketRequest(requests[0].Op) {
		return requests[0]
	}
	return internalCloudReleaseSocketRequest("cloud.stale_botlets_release", profile, messageID)
}

func internalCloudReleaseSocketRequest(op string, profile string, messageID string) SocketRequest {
	return SocketRequest{
		ID: "internal_" + strings.ReplaceAll(op, ".", "_"),
		Op: op,
		Auth: &SocketAuth{
			Mode:    "internal",
			Profile: &profile,
		},
		Params: map[string]any{"message_id": messageID},
	}
}

func (d *Daemon) listMessages(req SocketRequest) ([]MessageEnvelope, *SocketError) {
	unlock, lockErr := d.lockSessionAuthIfNeeded(req)
	if lockErr != nil {
		return nil, lockErr
	}
	defer unlock()
	filter, err := d.messageListFilter(req)
	if err != nil {
		return nil, err
	}
	if state, ok := req.Params["state"].(string); ok {
		filter.State = state
	}
	if req.Auth != nil && req.Auth.Mode == "session" {
		filter.Holder = "session:" + *req.Auth.SessionID + ":" + *req.Auth.SessionGeneration
		filter.State = "claimed"
		filter.ActiveOnly = true
	}
	stageReq := req
	stageReq.Op = "messages.list_inbox"
	d.lockBusForSocketRequest(stageReq)
	var messages []MessageEnvelope
	storeErr := d.runSocketStage(stageReq, "local.list_inbox_messages", func() error {
		var err error
		messages, err = d.store.ListInboxMessages(context.Background(), filter)
		return err
	})
	d.busMu.Unlock()
	if storeErr != nil {
		return nil, classifyMessageStoreError(storeErr)
	}
	return messages, nil
}

func (d *Daemon) sentMessages(req SocketRequest) ([]MessageEnvelope, *SocketError) {
	unlock, lockErr := d.lockSessionAuthIfNeeded(req)
	if lockErr != nil {
		return nil, lockErr
	}
	defer unlock()
	authority, err := d.messageListAuthority(req)
	if err != nil {
		return nil, err
	}
	limit := limitFromParams(req.Params)
	cursor, _ := req.Params["cursor"].(string)
	stageReq := req
	stageReq.Op = "messages.list_sent"
	d.lockBusForSocketRequest(stageReq)
	var messages []MessageEnvelope
	storeErr := d.runSocketStage(stageReq, "local.list_sent_messages", func() error {
		var err error
		messages, err = d.store.ListSentMessages(context.Background(), authority.Profile, limit, cursor)
		return err
	})
	d.busMu.Unlock()
	if storeErr != nil {
		return nil, classifyMessageStoreError(storeErr)
	}
	return messages, nil
}

func (d *Daemon) renewCloudMessageLocked(req SocketRequest, authority messageAuthority, message MessageEnvelope, ttl time.Duration) (MessageEnvelope, *SocketError) {
	locked := true
	defer func() {
		if locked {
			d.busMu.Unlock()
		}
	}()
	localProfile := message.Profile
	if replay, ok, replayErr := d.replayDoneCloudMessageMutationLocked(req, localProfile, authority.ClaimHolder, message, "renew", ttl); replayErr != nil {
		return MessageEnvelope{}, replayErr
	} else if ok {
		return replay, nil
	}
	pending, hasPending, pendingErr := d.pendingCloudMessageMutationLocked(req, localProfile, message.ID, "renew", ttl)
	if pendingErr != nil {
		return MessageEnvelope{}, pendingErr
	}
	if recovered, ok, recoverErr := d.recoverLocallyCompletedCloudMutation(req, message, pending, hasPending, authority.ClaimHolder); recoverErr != nil {
		return MessageEnvelope{}, recoverErr
	} else if ok {
		return recovered, nil
	}
	if !hasPending {
		latest, _, staleErr := d.rejectStaleCloudBotletsTaskMutationWithoutHoldingLocks(req, authority, message, time.Now().UTC(), func() {
			d.busMu.Unlock()
			locked = false
		}, func() {
			d.lockBusForSocketRequest(req)
			locked = true
		})
		if staleErr != nil {
			return MessageEnvelope{}, staleErr
		}
		message = latest
	}
	if err := validateCloudMessageMutation(message, authority.ClaimHolder, time.Now().UTC(), hasPending); err != nil {
		return MessageEnvelope{}, classifyMessageStoreError(err)
	}
	credentialProfile := d.cloudMutationCredentialProfile(authority, message)
	profileConfig, metadata, op, alreadyDone, prepErr := d.prepareCloudMessageMutationLocked(req, credentialProfile, localProfile, message.ID, authority.ClaimHolder, "renew", ttl)
	if prepErr != nil {
		return MessageEnvelope{}, prepErr
	}
	if alreadyDone {
		return d.replayCompletedCloudMessageMutation(message, metadata, op, authority.ClaimHolder)
	}
	attempted, attemptErr := d.recordCloudNotificationClaimOperationAttempt(req, op, time.Now().UTC(), "could not prepare notification operation")
	if attemptErr != nil {
		return MessageEnvelope{}, attemptErr
	}
	d.logCloudMutationStart(req, attempted)

	d.busMu.Unlock()
	locked = false

	remoteStartedAt := time.Now()
	var lease *CloudNotificationLease
	err := d.runSocketStage(req, "cloud.remote_renew", func() error {
		var renewErr error
		lease, renewErr = d.notificationClient.RenewNotification(context.Background(), profileConfig, attempted.ClaimID, ttl, attempted.OpID)
		return renewErr
	})
	if err != nil {
		d.logCloudMutationRemoteEnd(req, attempted, time.Since(remoteStartedAt), err)
		d.logCloudMutationFailed(req, attempted, err)
		return MessageEnvelope{}, d.handleCloudMessageMutationError(attempted, err)
	}
	d.logCloudMutationRemoteEnd(req, attempted, time.Since(remoteStartedAt), nil)
	if lease == nil {
		failure := errors.New("empty notification renew response")
		_ = RecordCloudNotificationClaimOperationFailure(d.paths, attempted, failure, time.Now().UTC())
		d.logCloudMutationFailed(req, attempted, failure)
		return MessageEnvelope{}, socketError("UPSTREAM_ERROR", "notification renew response mismatch", true)
	}
	if lease.ClaimID != attempted.ClaimID || lease.NotificationID != attempted.NotificationID {
		failure := errors.New("notification renew response mismatch")
		_ = RecordCloudNotificationClaimOperationFailure(d.paths, attempted, failure, time.Now().UTC())
		d.logCloudMutationFailed(req, attempted, failure)
		return MessageEnvelope{}, socketError("UPSTREAM_ERROR", "notification renew response mismatch", true)
	}

	d.lockBusForSocketRequest(req)
	locked = true
	localStartedAt := time.Now()
	localStage := d.startSocketStage(req, "cloud.local_completion")
	localStageOpen := true
	defer func() {
		if localStageOpen {
			localStage.failed("incomplete")
		}
	}()

	latest, storeErr := d.getInboxMessageForSocketRequest(req, localProfile, message.ID, "cloud.local_completion.get_message")
	if storeErr != nil {
		_ = RecordCloudNotificationClaimOperationFailure(d.paths, attempted, storeErr, time.Now().UTC())
		d.logCloudMutationFailed(req, attempted, storeErr)
		return MessageEnvelope{}, classifyMessageStoreError(storeErr)
	}
	if matchErr := requireMessageBotScope(req, authority, latest); matchErr != nil {
		_ = RecordCloudNotificationClaimOperationFailure(d.paths, attempted, errors.New(matchErr.Message), time.Now().UTC())
		d.logCloudMutationFailed(req, attempted, errors.New(matchErr.Code))
		return MessageEnvelope{}, matchErr
	}
	pendingAfterMutation, pendingOK, pendingReadErr := d.readPendingCloudNotificationClaimOperationForSocketRequest(req, attempted.OpID, "cloud.local_completion.pending_operation_read")
	if pendingReadErr != nil {
		_ = RecordCloudNotificationClaimOperationFailure(d.paths, attempted, pendingReadErr, time.Now().UTC())
		d.logCloudMutationFailed(req, attempted, pendingReadErr)
		return MessageEnvelope{}, socketError("UPSTREAM_ERROR", "could not inspect notification operation", true)
	}
	if !pendingOK {
		done, doneOK, doneErr := d.readDoneCloudNotificationClaimOperationForSocketRequest(req, attempted.OpID, "cloud.local_completion.done_operation_read")
		if doneErr != nil {
			return MessageEnvelope{}, socketError("UPSTREAM_ERROR", "could not inspect notification operation", true)
		}
		if doneOK && sameCloudNotificationClaimOperation(done, attempted) {
			var latestMetadata PrivateCloudMessageMetadata
			if metadataErr := d.runSocketStage(req, "cloud.local_completion.replay_metadata_read", func() error {
				var err error
				latestMetadata, err = ReadPrivateCloudMessageMetadata(d.paths, localProfile, message.ID)
				return err
			}); metadataErr != nil {
				return MessageEnvelope{}, socketError("UPSTREAM_ERROR", "could not read notification metadata", true)
			}
			replayed, replayErr := d.replayCompletedCloudMessageMutation(latest, latestMetadata, done, authority.ClaimHolder)
			if replayErr != nil {
				return MessageEnvelope{}, replayErr
			}
			localStage.done()
			localStageOpen = false
			return replayed, nil
		}
		failure := errors.New("notification operation disappeared before local completion")
		_ = RecordCloudNotificationClaimOperationFailure(d.paths, attempted, failure, time.Now().UTC())
		d.logCloudMutationFailed(req, attempted, failure)
		return MessageEnvelope{}, socketError("UPSTREAM_ERROR", "notification operation result is uncertain", true)
	}
	if !sameCloudNotificationClaimOperation(pendingAfterMutation, attempted) {
		failure := errors.New("notification operation changed before local completion")
		_ = RecordCloudNotificationClaimOperationFailure(d.paths, attempted, failure, time.Now().UTC())
		d.logCloudMutationFailed(req, attempted, failure)
		return MessageEnvelope{}, socketError("CONFLICT", "notification operation id conflict", false)
	}
	var latestMetadata PrivateCloudMessageMetadata
	if metadataErr := d.runSocketStage(req, "cloud.local_completion.metadata_read", func() error {
		var err error
		latestMetadata, err = ReadPrivateCloudMessageMetadata(d.paths, localProfile, message.ID)
		return err
	}); metadataErr != nil {
		_ = RecordCloudNotificationClaimOperationFailure(d.paths, attempted, metadataErr, time.Now().UTC())
		d.logCloudMutationFailed(req, attempted, metadataErr)
		return MessageEnvelope{}, socketError("UPSTREAM_ERROR", "could not read notification metadata", true)
	}
	if latestMetadata.ClaimID != attempted.ClaimID || latestMetadata.NotificationID != attempted.NotificationID {
		failure := errors.New("notification metadata changed before local completion")
		_ = RecordCloudNotificationClaimOperationFailure(d.paths, attempted, failure, time.Now().UTC())
		d.logCloudMutationFailed(req, attempted, failure)
		return MessageEnvelope{}, socketError("CONFLICT", "notification operation id conflict", false)
	}
	latestMetadata.LeaseExpiresAt = lease.LeaseExpiresAt
	if lease.ClaimedAt != "" {
		latestMetadata.ClaimedAt = lease.ClaimedAt
	}
	if err := d.runSocketStage(req, "cloud.local_completion.metadata_write", func() error {
		return WritePrivateCloudMessageMetadata(d.paths, latestMetadata)
	}); err != nil {
		_ = RecordCloudNotificationClaimOperationFailure(d.paths, attempted, err, time.Now().UTC())
		d.logCloudMutationFailed(req, attempted, err)
		return MessageEnvelope{}, socketError("UPSTREAM_ERROR", "could not write notification metadata", true)
	}
	var updated MessageEnvelope
	if storeErr := d.runSocketStage(req, "cloud.local_completion.renew_message", func() error {
		var err error
		updated, err = d.store.RenewCloudMessage(context.Background(), localProfile, latest.ID, authority.ClaimHolder, lease.LeaseExpiresAt, time.Now().UTC())
		return err
	}); storeErr != nil {
		_ = RecordCloudNotificationClaimOperationFailure(d.paths, attempted, storeErr, time.Now().UTC())
		d.logCloudMutationFailed(req, attempted, storeErr)
		return MessageEnvelope{}, classifyMessageStoreError(storeErr)
	}
	if err := d.completeCloudNotificationClaimOperationForSocketRequest(req, attempted, time.Now().UTC(), "cloud.local_completion.operation_complete"); err != nil {
		d.logCloudMutationFailed(req, attempted, err)
		return MessageEnvelope{}, socketError("UPSTREAM_ERROR", "could not complete notification operation", true)
	}
	localStage.done()
	localStageOpen = false
	d.logCloudMutationLocalComplete(req, attempted, time.Since(localStartedAt))
	d.logLeaseRenewed(updated)
	return updated, nil
}

func (d *Daemon) finishCloudMessageLocked(req SocketRequest, authority messageAuthority, message MessageEnvelope, operation string) (MessageEnvelope, *SocketError) {
	locked := true
	defer func() {
		if locked {
			d.busMu.Unlock()
		}
	}()
	localProfile := message.Profile
	if replay, ok, replayErr := d.replayDoneCloudMessageMutationLocked(req, localProfile, authority.ClaimHolder, message, operation, 0); replayErr != nil {
		return MessageEnvelope{}, replayErr
	} else if ok {
		return replay, nil
	}
	pending, hasPending, pendingErr := d.pendingCloudMessageMutationLocked(req, localProfile, message.ID, operation, 0)
	if pendingErr != nil {
		return MessageEnvelope{}, pendingErr
	}
	if recovered, ok, recoverErr := d.recoverLocallyCompletedCloudMutation(req, message, pending, hasPending, authority.ClaimHolder); recoverErr != nil {
		return MessageEnvelope{}, recoverErr
	} else if ok {
		return recovered, nil
	}
	if operation == "ack" && !hasPending {
		latest, _, staleErr := d.rejectStaleCloudBotletsTaskMutationWithoutHoldingLocks(req, authority, message, time.Now().UTC(), func() {
			d.busMu.Unlock()
			locked = false
		}, func() {
			d.lockBusForSocketRequest(req)
			locked = true
		})
		if staleErr != nil {
			return MessageEnvelope{}, staleErr
		}
		message = latest
	}
	if err := validateCloudMessageMutation(message, authority.ClaimHolder, time.Now().UTC(), hasPending); err != nil {
		return MessageEnvelope{}, classifyMessageStoreError(err)
	}
	credentialProfile := d.cloudMutationCredentialProfile(authority, message)
	profileConfig, metadata, op, alreadyDone, prepErr := d.prepareCloudMessageMutationLocked(req, credentialProfile, localProfile, message.ID, authority.ClaimHolder, operation, 0)
	if prepErr != nil {
		return MessageEnvelope{}, prepErr
	}
	if alreadyDone {
		return d.replayCompletedCloudMessageMutation(message, metadata, op, authority.ClaimHolder)
	}
	attempted, attemptErr := d.recordCloudNotificationClaimOperationAttempt(req, op, time.Now().UTC(), "could not prepare notification operation")
	if attemptErr != nil {
		return MessageEnvelope{}, attemptErr
	}
	d.logCloudMutationStart(req, attempted)

	d.busMu.Unlock()
	locked = false

	var (
		mutation *CloudNotificationClaimMutation
		err      error
	)
	remoteStartedAt := time.Now()
	remoteStage := "cloud.remote_" + operation
	err = d.runSocketStage(req, remoteStage, func() error {
		var mutationErr error
		if operation == "ack" {
			mutation, mutationErr = d.notificationClient.AckNotification(context.Background(), profileConfig, attempted.ClaimID, attempted.OpID)
		} else {
			mutation, mutationErr = d.notificationClient.ReleaseNotification(context.Background(), profileConfig, attempted.ClaimID, attempted.OpID)
		}
		return mutationErr
	})
	if err != nil {
		d.logCloudMutationRemoteEnd(req, attempted, time.Since(remoteStartedAt), err)
		d.logCloudMutationFailed(req, attempted, err)
		return MessageEnvelope{}, d.handleCloudMessageMutationError(attempted, err)
	}
	d.logCloudMutationRemoteEnd(req, attempted, time.Since(remoteStartedAt), nil)
	if mutation == nil {
		failure := errors.New("empty notification claim response")
		_ = RecordCloudNotificationClaimOperationFailure(d.paths, attempted, failure, time.Now().UTC())
		d.logCloudMutationFailed(req, attempted, failure)
		return MessageEnvelope{}, socketError("UPSTREAM_ERROR", "notification claim response mismatch", true)
	}
	if mutation.ClaimID != attempted.ClaimID || mutation.NotificationID != attempted.NotificationID {
		failure := errors.New("notification claim response mismatch")
		_ = RecordCloudNotificationClaimOperationFailure(d.paths, attempted, failure, time.Now().UTC())
		d.logCloudMutationFailed(req, attempted, failure)
		return MessageEnvelope{}, socketError("UPSTREAM_ERROR", "notification claim response mismatch", true)
	}

	d.lockBusForSocketRequest(req)
	locked = true
	localStartedAt := time.Now()
	localStage := d.startSocketStage(req, "cloud.local_completion")
	localStageOpen := true
	defer func() {
		if localStageOpen {
			localStage.failed("incomplete")
		}
	}()

	latest, storeErr := d.getInboxMessageForSocketRequest(req, localProfile, message.ID, "cloud.local_completion.get_message")
	if storeErr != nil {
		_ = RecordCloudNotificationClaimOperationFailure(d.paths, attempted, storeErr, time.Now().UTC())
		d.logCloudMutationFailed(req, attempted, storeErr)
		return MessageEnvelope{}, classifyMessageStoreError(storeErr)
	}
	if matchErr := requireMessageBotScope(req, authority, latest); matchErr != nil {
		_ = RecordCloudNotificationClaimOperationFailure(d.paths, attempted, errors.New(matchErr.Message), time.Now().UTC())
		d.logCloudMutationFailed(req, attempted, errors.New(matchErr.Code))
		return MessageEnvelope{}, matchErr
	}
	pendingAfterMutation, pendingOK, pendingReadErr := d.readPendingCloudNotificationClaimOperationForSocketRequest(req, attempted.OpID, "cloud.local_completion.pending_operation_read")
	if pendingReadErr != nil {
		_ = RecordCloudNotificationClaimOperationFailure(d.paths, attempted, pendingReadErr, time.Now().UTC())
		d.logCloudMutationFailed(req, attempted, pendingReadErr)
		return MessageEnvelope{}, socketError("UPSTREAM_ERROR", "could not inspect notification operation", true)
	}
	if !pendingOK {
		done, doneOK, doneErr := d.readDoneCloudNotificationClaimOperationForSocketRequest(req, attempted.OpID, "cloud.local_completion.done_operation_read")
		if doneErr != nil {
			return MessageEnvelope{}, socketError("UPSTREAM_ERROR", "could not inspect notification operation", true)
		}
		if doneOK && sameCloudNotificationClaimOperation(done, attempted) {
			var latestMetadata PrivateCloudMessageMetadata
			if metadataErr := d.runSocketStage(req, "cloud.local_completion.replay_metadata_read", func() error {
				var err error
				latestMetadata, err = ReadPrivateCloudMessageMetadata(d.paths, localProfile, message.ID)
				return err
			}); metadataErr != nil {
				return MessageEnvelope{}, socketError("UPSTREAM_ERROR", "could not read notification metadata", true)
			}
			replayed, replayErr := d.replayCompletedCloudMessageMutation(latest, latestMetadata, done, authority.ClaimHolder)
			if replayErr != nil {
				return MessageEnvelope{}, replayErr
			}
			localStage.done()
			localStageOpen = false
			return replayed, nil
		}
		failure := errors.New("notification operation disappeared before local completion")
		_ = RecordCloudNotificationClaimOperationFailure(d.paths, attempted, failure, time.Now().UTC())
		d.logCloudMutationFailed(req, attempted, failure)
		return MessageEnvelope{}, socketError("UPSTREAM_ERROR", "notification operation result is uncertain", true)
	}
	if !sameCloudNotificationClaimOperation(pendingAfterMutation, attempted) {
		failure := errors.New("notification operation changed before local completion")
		_ = RecordCloudNotificationClaimOperationFailure(d.paths, attempted, failure, time.Now().UTC())
		d.logCloudMutationFailed(req, attempted, failure)
		return MessageEnvelope{}, socketError("CONFLICT", "notification operation id conflict", false)
	}
	var latestMetadata PrivateCloudMessageMetadata
	if metadataErr := d.runSocketStage(req, "cloud.local_completion.metadata_read", func() error {
		var err error
		latestMetadata, err = ReadPrivateCloudMessageMetadata(d.paths, localProfile, message.ID)
		return err
	}); metadataErr != nil {
		_ = RecordCloudNotificationClaimOperationFailure(d.paths, attempted, metadataErr, time.Now().UTC())
		d.logCloudMutationFailed(req, attempted, metadataErr)
		return MessageEnvelope{}, socketError("UPSTREAM_ERROR", "could not read notification metadata", true)
	}
	if latestMetadata.ClaimID != attempted.ClaimID || latestMetadata.NotificationID != attempted.NotificationID {
		failure := errors.New("notification metadata changed before local completion")
		_ = RecordCloudNotificationClaimOperationFailure(d.paths, attempted, failure, time.Now().UTC())
		d.logCloudMutationFailed(req, attempted, failure)
		return MessageEnvelope{}, socketError("CONFLICT", "notification operation id conflict", false)
	}

	var updated MessageEnvelope
	storeStage := "cloud.local_completion." + operation + "_message"
	if storeErr := d.runSocketStage(req, storeStage, func() error {
		var err error
		if operation == "ack" {
			updated, err = d.store.AckCloudMessage(context.Background(), localProfile, message.ID, authority.ClaimHolder, time.Now().UTC())
		} else {
			updated, err = d.store.ReleaseCloudMessage(context.Background(), localProfile, message.ID, authority.ClaimHolder, attempted.ReleaseReason, time.Now().UTC())
		}
		return err
	}); storeErr != nil {
		_ = RecordCloudNotificationClaimOperationFailure(d.paths, attempted, storeErr, time.Now().UTC())
		d.logCloudMutationFailed(req, attempted, storeErr)
		return MessageEnvelope{}, classifyMessageStoreError(storeErr)
	}
	if err := d.completeCloudNotificationClaimOperationForSocketRequest(req, attempted, time.Now().UTC(), "cloud.local_completion.operation_complete"); err != nil {
		d.logCloudMutationFailed(req, attempted, err)
		return MessageEnvelope{}, socketError("UPSTREAM_ERROR", "could not complete notification operation", true)
	}
	localStage.done()
	localStageOpen = false
	d.logCloudMutationLocalComplete(req, attempted, time.Since(localStartedAt))
	d.logMessageProxied(updated, operation)
	return updated, nil
}

func (d *Daemon) pendingCloudMessageMutationLocked(req SocketRequest, profile string, messageID string, operation string, ttl time.Duration) (CloudNotificationClaimOperation, bool, *SocketError) {
	opID, _ := req.Params["op_id"].(string)
	var metadata PrivateCloudMessageMetadata
	if metadataErr := d.runSocketStage(req, "cloud.pending_metadata_read", func() error {
		var err error
		metadata, err = ReadPrivateCloudMessageMetadata(d.paths, profile, messageID)
		return err
	}); metadataErr != nil {
		return CloudNotificationClaimOperation{}, false, socketError("UPSTREAM_ERROR", "could not read notification metadata", true)
	}
	var (
		op  CloudNotificationClaimOperation
		ok  bool
		err error
	)
	releaseReason := ""
	if operation == "release" {
		releaseReason, _ = req.Params["reason"].(string)
	}
	if readErr := d.runSocketStage(req, "cloud.pending_operation_read", func() error {
		if opID != "" {
			op, ok, err = ReadPendingCloudNotificationClaimOperationWithRetry(d.paths, opID)
		} else {
			op, ok, err = findPendingCloudNotificationClaimOperation(d.paths, operation, profile, messageID, metadata.ClaimID, metadata.NotificationID, ttl, releaseReason)
		}
		return err
	}); readErr != nil {
		return CloudNotificationClaimOperation{}, false, socketError("CONFLICT", "notification operation id conflict", false)
	}
	if !ok {
		return CloudNotificationClaimOperation{}, false, nil
	}
	if op.Operation != operation || op.Profile != profile || op.LocalMessageID != messageID || op.PrivateMetadataRef != privateCloudMessageRef(profile, messageID) || op.LeaseTTLMS != durationMilliseconds(ttl) {
		return CloudNotificationClaimOperation{}, false, socketError("CONFLICT", "notification operation id conflict", false)
	}
	if op.ClaimID != metadata.ClaimID || op.NotificationID != metadata.NotificationID {
		return CloudNotificationClaimOperation{}, false, socketError("CONFLICT", "notification operation id conflict", false)
	}
	if opID != "" && !releaseReasonMatchesExactly(op, releaseReason) {
		return CloudNotificationClaimOperation{}, false, socketError("CONFLICT", "notification operation id conflict", false)
	}
	if opID == "" && !releaseReasonLookupCompatible(op, releaseReason) {
		return CloudNotificationClaimOperation{}, false, socketError("CONFLICT", "notification operation id conflict", false)
	}
	return op, true, nil
}

func (d *Daemon) replayDoneCloudMessageMutationLocked(req SocketRequest, profile string, claimHolder string, message MessageEnvelope, operation string, ttl time.Duration) (MessageEnvelope, bool, *SocketError) {
	opID, hasOpID := req.Params["op_id"].(string)
	var metadata PrivateCloudMessageMetadata
	if metadataErr := d.runSocketStage(req, "cloud.replay_metadata_read", func() error {
		var err error
		metadata, err = ReadPrivateCloudMessageMetadata(d.paths, profile, message.ID)
		return err
	}); metadataErr != nil {
		return MessageEnvelope{}, false, socketError("UPSTREAM_ERROR", "could not read notification metadata", true)
	}
	var done CloudNotificationClaimOperation
	var err error
	releaseReason := ""
	if operation == "release" {
		releaseReason, _ = req.Params["reason"].(string)
	}
	if hasOpID && opID != "" {
		var doneOK bool
		if readErr := d.runSocketStage(req, "cloud.replay_done_operation_read", func() error {
			done, doneOK, err = ReadDoneCloudNotificationClaimOperation(d.paths, opID)
			return err
		}); readErr != nil {
			return MessageEnvelope{}, false, socketError("CONFLICT", "notification operation id conflict", false)
		}
		if !doneOK {
			return MessageEnvelope{}, false, nil
		}
		if done.Operation != operation || done.Profile != profile || done.LocalMessageID != message.ID || done.PrivateMetadataRef != privateCloudMessageRef(profile, message.ID) || done.ClaimID != metadata.ClaimID || done.NotificationID != metadata.NotificationID || done.LeaseTTLMS != durationMilliseconds(ttl) || !releaseReasonMatchesExactly(done, releaseReason) {
			return MessageEnvelope{}, false, socketError("CONFLICT", "notification operation id conflict", false)
		}
	} else {
		if operation == "renew" {
			return MessageEnvelope{}, false, nil
		}
		var doneOK bool
		if readErr := d.runSocketStage(req, "cloud.replay_done_operation_read", func() error {
			done, doneOK, err = findDoneCloudNotificationClaimOperation(d.paths, operation, profile, message.ID, metadata.ClaimID, metadata.NotificationID, ttl, releaseReason)
			return err
		}); readErr != nil {
			return MessageEnvelope{}, false, socketError("UPSTREAM_ERROR", "could not inspect notification operation", true)
		}
		if !doneOK {
			return MessageEnvelope{}, false, nil
		}
	}
	replay, replayErr := d.replayCompletedCloudMessageMutation(message, metadata, done, claimHolder)
	if replayErr != nil {
		if opID != "" {
			return MessageEnvelope{}, false, replayErr
		}
		return MessageEnvelope{}, false, nil
	}
	return replay, true, nil
}

func (d *Daemon) prepareCloudMessageMutationLocked(req SocketRequest, credentialProfile string, metadataProfile string, messageID string, claimHolder string, operation string, ttl time.Duration) (AgentProfile, PrivateCloudMessageMetadata, CloudNotificationClaimOperation, bool, *SocketError) {
	if d.notificationClient == nil {
		return AgentProfile{}, PrivateCloudMessageMetadata{}, CloudNotificationClaimOperation{}, false, socketError("UPSTREAM_ERROR", "notification client is not configured", true)
	}
	profileConfig, ok := d.cloudNotificationProfile(credentialProfile)
	if !ok {
		return AgentProfile{}, PrivateCloudMessageMetadata{}, CloudNotificationClaimOperation{}, false, socketError("UPSTREAM_ERROR", "notification profile is not loaded", true)
	}
	var metadata PrivateCloudMessageMetadata
	if metadataErr := d.runSocketStage(req, "cloud.prepare_metadata_read", func() error {
		var err error
		metadata, err = ReadPrivateCloudMessageMetadata(d.paths, metadataProfile, messageID)
		return err
	}); metadataErr != nil {
		return AgentProfile{}, PrivateCloudMessageMetadata{}, CloudNotificationClaimOperation{}, false, socketError("UPSTREAM_ERROR", "could not read notification metadata", true)
	}
	opID, _ := req.Params["op_id"].(string)
	releaseReason := ""
	if operation == "release" {
		releaseReason, _ = req.Params["reason"].(string)
	}
	if conflictErr := d.rejectConflictingTerminalCloudMutation(req, metadataProfile, messageID, metadata, claimHolder, operation, ttl, releaseReason, opID); conflictErr != nil {
		return AgentProfile{}, PrivateCloudMessageMetadata{}, CloudNotificationClaimOperation{}, false, conflictErr
	}
	var op CloudNotificationClaimOperation
	var done bool
	if prepErr := d.runSocketStage(req, "cloud.operation_prepare", func() error {
		var err error
		op, done, err = BeginCloudNotificationClaimOperationForHolder(d.paths, operation, metadataProfile, messageID, metadata.ClaimID, metadata.NotificationID, claimHolder, opID, ttl, time.Now().UTC(), releaseReason)
		return err
	}); prepErr != nil {
		return AgentProfile{}, PrivateCloudMessageMetadata{}, CloudNotificationClaimOperation{}, false, socketError("CONFLICT", "notification operation id conflict", false)
	}
	if op.ClaimID != metadata.ClaimID || op.NotificationID != metadata.NotificationID {
		return AgentProfile{}, PrivateCloudMessageMetadata{}, CloudNotificationClaimOperation{}, false, socketError("CONFLICT", "notification operation id conflict", false)
	}
	return profileConfig, metadata, op, done, nil
}

func (d *Daemon) rejectConflictingTerminalCloudMutation(req SocketRequest, profile string, messageID string, metadata PrivateCloudMessageMetadata, claimHolder string, operation string, ttl time.Duration, releaseReason string, requestedOpID string) *SocketError {
	if operation != "ack" && operation != "release" {
		return nil
	}
	var pendingOps []CloudNotificationClaimOperation
	if err := d.runSocketStage(req, "cloud.terminal_operation_preflight", func() error {
		var pendingErr error
		pendingOps, pendingErr = ListPendingCloudNotificationClaimOperationsForMessage(d.paths, profile, messageID, metadata.ClaimID, metadata.NotificationID)
		return pendingErr
	}); err != nil {
		return socketError("UPSTREAM_ERROR", "could not inspect pending notification operations", true)
	}
	for _, pending := range pendingOps {
		if pending.Operation != "ack" && (pending.Operation != "release" || isDeclinedDuplicateReleaseOperation(pending)) {
			continue
		}
		if sameCloudNotificationClaimOperationSelector(pending, operation, profile, messageID, metadata.ClaimID, metadata.NotificationID, claimHolder, ttl, releaseReason) &&
			(requestedOpID == "" || requestedOpID == pending.OpID) {
			continue
		}
		return socketError("CONFLICT", "terminal notification operation already in progress", false)
	}
	return nil
}

func (d *Daemon) handleCloudMessageMutationError(op CloudNotificationClaimOperation, err error) *SocketError {
	if errors.Is(err, errNotificationMutationDeadline) || errors.Is(err, errNotificationMutationAmbiguous) {
		_ = RecordCloudNotificationClaimOperationFailure(d.paths, op, err, time.Now().UTC())
		return socketError("UPSTREAM_ERROR", "notification operation result is uncertain", true)
	}
	_ = AbandonCloudNotificationClaimOperation(d.paths, op)
	var statusErr *NotificationHTTPError
	if errors.As(err, &statusErr) {
		switch statusErr.Status {
		case 400:
			return socketError("VALIDATION_ERROR", "notification operation was rejected", false)
		case 404:
			return socketError("NOT_FOUND", "notification claim was not found", false)
		case 409:
			return socketError("CONFLICT", "notification claim conflict", false)
		case 401, 403:
			return socketError("FORBIDDEN", "notification profile was rejected", false)
		}
	}
	return socketError("UPSTREAM_ERROR", "notification operation failed", false)
}

func validateCloudMessageMutation(message MessageEnvelope, claimHolder string, now time.Time, allowExpired bool) error {
	if message.Source != "comment.io" || message.Delivery.State != "claimed" || stringValue(message.Delivery.ClaimHolder) != claimHolder {
		return ErrMessageConflict
	}
	if !allowExpired && leaseExpired(message.Delivery.LeaseExpiresAt, now) {
		return ErrMessageConflict
	}
	return nil
}

func (d *Daemon) replayCompletedCloudMessageMutation(message MessageEnvelope, metadata PrivateCloudMessageMetadata, op CloudNotificationClaimOperation, claimHolder string) (MessageEnvelope, *SocketError) {
	if message.Source != "comment.io" ||
		message.ID != op.LocalMessageID ||
		message.Profile != op.Profile ||
		metadata.Source != "comment.io" ||
		metadata.Profile != op.Profile ||
		metadata.LocalMessageID != op.LocalMessageID ||
		metadata.ClaimID != op.ClaimID ||
		metadata.NotificationID != op.NotificationID {
		return MessageEnvelope{}, socketError("CONFLICT", "notification operation id conflict", false)
	}
	if op.ClaimHolder != "" && op.ClaimHolder != claimHolder {
		return MessageEnvelope{}, socketError("CONFLICT", "notification operation id conflict", false)
	}
	expectedClaimHolder := claimHolder
	if op.ClaimHolder != "" {
		expectedClaimHolder = op.ClaimHolder
	}
	switch op.Operation {
	case "ack":
		if message.Delivery.State == "acked" && stringValue(message.Delivery.ClaimHolder) == expectedClaimHolder {
			return message, nil
		}
	case "release":
		if cloudReleaseCompletedLocalState(message) {
			return message, nil
		}
	case "renew":
		if message.Delivery.State == "claimed" && message.Delivery.LeaseExpiresAt != nil && stringValue(message.Delivery.ClaimHolder) == expectedClaimHolder {
			return message, nil
		}
	}
	return MessageEnvelope{}, socketError("CONFLICT", "notification operation id conflict", false)
}

func (d *Daemon) recoverLocallyCompletedCloudMutation(req SocketRequest, message MessageEnvelope, op CloudNotificationClaimOperation, hasPending bool, claimHolder string) (MessageEnvelope, bool, *SocketError) {
	if !hasPending || op.Operation == "renew" {
		return MessageEnvelope{}, false, nil
	}
	var metadata PrivateCloudMessageMetadata
	if metadataErr := d.runSocketStage(req, "cloud.recover_metadata_read", func() error {
		var err error
		metadata, err = ReadPrivateCloudMessageMetadata(d.paths, op.Profile, op.LocalMessageID)
		return err
	}); metadataErr != nil {
		return MessageEnvelope{}, false, socketError("UPSTREAM_ERROR", "could not read notification metadata", true)
	}
	replayed, replayErr := d.replayCompletedCloudMessageMutation(message, metadata, op, claimHolder)
	if replayErr != nil {
		return MessageEnvelope{}, false, nil
	}
	if err := d.runSocketStage(req, "cloud.recover_local_complete", func() error {
		return CompleteCloudNotificationClaimOperation(d.paths, op, time.Now().UTC())
	}); err != nil {
		return MessageEnvelope{}, false, socketError("UPSTREAM_ERROR", "could not complete notification operation", true)
	}
	return replayed, true, nil
}

func (d *Daemon) cloudNotificationProfile(profile string) (AgentProfile, bool) {
	d.profileMu.RLock()
	defer d.profileMu.RUnlock()
	profileConfig, ok := d.profileState.AgentProfiles[profile]
	return profileConfig, ok
}

func (d *Daemon) cloudMutationCredentialProfile(authority messageAuthority, message MessageEnvelope) string {
	d.profileMu.RLock()
	defer d.profileMu.RUnlock()
	if _, ok := d.profileState.AgentProfiles[authority.Profile]; ok {
		return authority.Profile
	}
	if !messageMatchesAuthorityBot(message, authority) || (authority.BotID == "" && authority.BotAgentID == "") {
		return authority.Profile
	}
	messageHasStableIdentity := message.BotID != "" || message.BotAgentID != ""
	for _, bot := range d.profileState.BotRegistry {
		if !bot.MatchesStableIdentity(authority.BotID, authority.BotAgentID) {
			continue
		}
		if messageHasStableIdentity && !bot.MatchesStableIdentity(message.BotID, message.BotAgentID) {
			continue
		}
		if _, ok := d.profileState.AgentProfiles[bot.Handle]; ok {
			return bot.Handle
		}
	}
	return authority.Profile
}

func (d *Daemon) publishCloudMessageHandlingActivity(ctx context.Context, profile string, messageID string, action string, outcome string, idempotencyKeys ...string) error {
	return d.publishCloudMessageHandlingActivityForProfiles(ctx, profile, profile, messageID, action, outcome, idempotencyKeys...)
}

func (d *Daemon) publishCloudMessageHandlingActivityForProfiles(ctx context.Context, credentialProfile string, metadataProfile string, messageID string, action string, outcome string, idempotencyKeys ...string) error {
	_, err := d.publishCloudMessageHandlingActivityResultForProfiles(ctx, credentialProfile, metadataProfile, messageID, action, outcome, idempotencyKeys...)
	return err
}

func (d *Daemon) publishCloudMessageHandlingActivityResultForProfiles(ctx context.Context, credentialProfile string, metadataProfile string, messageID string, action string, outcome string, idempotencyKeys ...string) (*CloudNotificationHandlingResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if d.notificationClient == nil {
		return nil, errors.New("notification client is not configured")
	}
	if !ProfileRE.MatchString(credentialProfile) || !ProfileRE.MatchString(metadataProfile) || !LocalMessageIDRE.MatchString(messageID) {
		return nil, errors.New("invalid notification handling target")
	}
	if action != "start" && action != "complete" && action != "clear" {
		return nil, errors.New("invalid notification handling action")
	}
	profileConfig, ok := d.cloudNotificationProfile(credentialProfile)
	if !ok {
		return nil, errors.New("notification profile is not loaded")
	}
	var metadata PrivateCloudMessageMetadata
	if err := d.runSocketStageForContext(ctx, "notification.handling_activity.metadata_read", func() error {
		var metadataErr error
		metadata, metadataErr = ReadPrivateCloudMessageMetadata(d.paths, metadataProfile, messageID)
		return metadataErr
	}); err != nil {
		return nil, err
	}
	if metadata.BaseURL != "" && profileConfig.BaseURL != metadata.BaseURL {
		return nil, errors.New("notification profile no longer matches private metadata")
	}
	opID := ""
	if len(idempotencyKeys) > 0 {
		opID = idempotencyKeys[0]
	}
	if opID == "" {
		generatedOpID, err := GenerateLocalID("op", 0)
		if err != nil {
			return nil, err
		}
		opID = generatedOpID
	}
	request := CloudNotificationHandlingRequest{Action: action}
	if outcome != "" {
		request.Outcome = outcome
	}
	if generation := cloudHandlingClaimGeneration(metadata); generation != nil {
		request.ClaimGeneration = *generation
	}
	request.ProgressAt = busTime(time.Now().UTC())
	logData := map[string]any{
		"profile":          credentialProfile,
		"metadata_profile": metadataProfile,
		"message_id":       messageID,
		"action":           action,
		"op_id":            opID,
	}
	if req, ok := socketRequestFromContext(ctx); ok {
		for key, value := range socketRequestLogData(req) {
			logData[key] = value
		}
		logData["action"] = action
		logData["op_id"] = opID
	}
	if outcome != "" {
		logData["outcome"] = outcome
	}
	d.logger.info("notification.handling_activity.start", logData)
	startedAt := time.Now()
	var result *CloudNotificationHandlingResult
	err := d.runSocketStageForContext(ctx, "notification.handling_activity.remote_publish", func() error {
		var publishErr error
		result, publishErr = d.notificationClient.PublishNotificationHandlingActivity(ctx, profileConfig, metadata.ClaimID, request, opID)
		return publishErr
	})
	logData["duration_ms"] = time.Since(startedAt).Milliseconds()
	logData["ok"] = err == nil
	if err != nil {
		logData["error_kind"] = cloudMutationErrorKind(err)
		d.logger.warn("notification.handling_activity.end", logData)
		return nil, err
	}
	if time.Since(startedAt) >= slowCloudMutationDuration {
		d.logger.warn("notification.handling_activity.end", logData)
	} else {
		d.logger.info("notification.handling_activity.end", logData)
	}
	return result, nil
}

func (d *Daemon) publishCloudHandlingStartBestEffort(ctx context.Context, profile string, messageID string) {
	d.publishCloudHandlingStartBestEffortForProfiles(ctx, profile, profile, messageID)
}

func (d *Daemon) publishCloudHandlingStartBestEffortForProfiles(ctx context.Context, credentialProfile string, metadataProfile string, messageID string) {
	if err := d.publishCloudMessageHandlingActivityForProfiles(ctx, credentialProfile, metadataProfile, messageID, "start", ""); err != nil {
		d.logger.warn("notification.handling_start_failed", map[string]any{
			"profile":          credentialProfile,
			"metadata_profile": metadataProfile,
			"message_id":       messageID,
			"error":            err.Error(),
		})
	}
}

func (d *Daemon) publishCloudHandlingClearBestEffortForProfiles(ctx context.Context, credentialProfile string, metadataProfile string, messageID string, outcome string) {
	if err := d.publishCloudMessageHandlingActivityForProfiles(ctx, credentialProfile, metadataProfile, messageID, "clear", outcome); err != nil {
		d.logger.warn("notification.handling_clear_failed", map[string]any{
			"profile":          credentialProfile,
			"metadata_profile": metadataProfile,
			"message_id":       messageID,
			"outcome":          outcome,
			"error":            err.Error(),
		})
	}
}

func (d *Daemon) publishCloudHandlingStartBestEffortAsync(profile string, messageID string) {
	d.publishCloudHandlingStartBestEffortAsyncForProfiles(profile, profile, messageID)
}

func (d *Daemon) publishCloudHandlingStartBestEffortAsyncForProfiles(credentialProfile string, metadataProfile string, messageID string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), notificationMutationTimeout)
		defer cancel()
		d.publishCloudHandlingStartBestEffortForProfiles(ctx, credentialProfile, metadataProfile, messageID)
	}()
}

func (d *Daemon) publishCloudHandlingClearBestEffortAsyncForProfiles(credentialProfile string, metadataProfile string, messageID string, outcome string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), notificationMutationTimeout)
		defer cancel()
		d.publishCloudHandlingClearBestEffortForProfiles(ctx, credentialProfile, metadataProfile, messageID, outcome)
	}()
}

func (d *Daemon) lockSessionAuthIfNeeded(req SocketRequest) (func(), *SocketError) {
	if req.Auth == nil || req.Auth.Mode != "session" {
		return func() {}, nil
	}
	d.lockSessionForSocketRequest(req)
	var record SessionRecord
	if err := d.runSocketStage(req, "session.verify_capability", func() error {
		var verifyErr error
		record, verifyErr = VerifySessionCapability(d.paths, *req.Auth)
		return verifyErr
	}); err != nil {
		d.sessionMu.Unlock()
		return nil, socketError("FORBIDDEN", "invalid session capability", false)
	}
	var syncSocketErr *SocketError
	if err := d.runSocketStage(req, "session.sync_liveness", func() error {
		record, syncSocketErr = d.syncSessionLivenessLocked(record)
		if syncSocketErr != nil {
			return errors.New(syncSocketErr.Code)
		}
		return nil
	}); err != nil {
		d.sessionMu.Unlock()
		return nil, syncSocketErr
	}
	if record.State != "alive" {
		d.sessionMu.Unlock()
		return nil, socketError("FORBIDDEN", "invalid session capability", false)
	}
	var runtimeSocketErr *SocketError
	if err := d.runSocketStage(req, "session.verify_runtime_trust", func() error {
		runtimeSocketErr = d.verifySessionRuntimeTrustLocked(record)
		if runtimeSocketErr != nil {
			return errors.New(runtimeSocketErr.Code)
		}
		return nil
	}); err != nil {
		d.sessionMu.Unlock()
		return nil, runtimeSocketErr
	}
	return d.sessionMu.Unlock, nil
}

func (d *Daemon) messageListFilter(req SocketRequest) (MessageListFilter, *SocketError) {
	authority, err := d.messageListAuthority(req)
	if err != nil {
		return MessageListFilter{}, err
	}
	cursor, _ := req.Params["cursor"].(string)
	filter := MessageListFilter{
		Profile: authority.Profile,
		BotName: messageListBotFilter(req, authority),
		Limit:   limitFromParams(req.Params),
		Cursor:  cursor,
	}
	if filter.BotName != "" && messageAuthorityAllowsIdentityProfileDrift(req, authority) {
		filter.BotName = ""
		filter.BotID = authority.BotID
		filter.BotAgentID = authority.BotAgentID
		filter.AllowIdentityProfileDrift = true
	}
	return filter, nil
}

func (d *Daemon) resolveSenderAuthority(req SocketRequest) (messageAuthority, *SocketError) {
	if req.Auth != nil && req.Auth.Mode == "session" {
		record, err := VerifySessionCapability(d.paths, *req.Auth)
		if err != nil {
			return messageAuthority{}, socketError("FORBIDDEN", "invalid session capability", false)
		}
		if fromBot, ok := req.Params["from_bot"].(string); ok && fromBot != record.BotName {
			return messageAuthority{}, socketError("FORBIDDEN", "sender bot does not match session", false)
		}
		return authorityForSessionRecord(record), nil
	}
	if fromBot, ok := req.Params["from_bot"].(string); ok {
		bot, err := d.botByName(fromBot)
		if err != nil {
			return messageAuthority{}, err
		}
		if req.Auth != nil && req.Auth.Profile != nil && *req.Auth.Profile != bot.Handle {
			return messageAuthority{}, socketError("FORBIDDEN", "owner profile does not match sender", false)
		}
		return authorityForBot(bot), nil
	}
	return d.ownerSelectedAuthority(req.Auth, req.Params, true)
}

func (d *Daemon) messageListAuthority(req SocketRequest) (messageAuthority, *SocketError) {
	if req.Auth != nil && req.Auth.Mode == "session" {
		record, err := VerifySessionCapability(d.paths, *req.Auth)
		if err != nil {
			return messageAuthority{}, socketError("FORBIDDEN", "invalid session capability", false)
		}
		authority := authorityForSessionRecord(record)
		if !paramsMatchAuthority(req.Params, authority) {
			return messageAuthority{}, socketError("FORBIDDEN", "session profile mismatch", false)
		}
		return authority, nil
	}
	return d.ownerSelectedAuthority(req.Auth, req.Params, true)
}

func (d *Daemon) messageMutationAuthority(req SocketRequest) (messageAuthority, *SocketError) {
	if req.Auth != nil && req.Auth.Mode == "session" {
		record, err := VerifySessionCapability(d.paths, *req.Auth)
		if err != nil {
			return messageAuthority{}, socketError("FORBIDDEN", "invalid session capability", false)
		}
		authority := authorityForSessionRecord(record)
		if !paramsMatchAuthority(req.Params, authority) {
			return messageAuthority{}, socketError("FORBIDDEN", "session profile mismatch", false)
		}
		return authority, nil
	}
	return d.ownerSelectedAuthority(req.Auth, req.Params, true)
}

func requireMessageBotScope(req SocketRequest, authority messageAuthority, message MessageEnvelope) *SocketError {
	if req.Auth != nil && req.Auth.Mode == "session" && !messageMatchesAuthorityBot(message, authority) {
		return socketError("FORBIDDEN", "message bot does not match session", false)
	}
	if req.Auth != nil && req.Auth.Mode == "owner" {
		if _, hasBot := req.Params["bot"].(string); hasBot && !messageMatchesAuthorityBot(message, authority) {
			return socketError("FORBIDDEN", "message bot does not match selected bot", false)
		}
	}
	return nil
}

func messageListBotFilter(req SocketRequest, authority messageAuthority) string {
	if req.Auth != nil && req.Auth.Mode == "session" {
		return authority.BotName
	}
	if _, ok := req.Params["bot"].(string); ok {
		return authority.BotName
	}
	return ""
}

func (d *Daemon) ownerSelectedAuthority(auth *SocketAuth, params map[string]any, required bool) (messageAuthority, *SocketError) {
	if auth == nil || auth.Mode != "owner" {
		return messageAuthority{}, socketError("FORBIDDEN", "owner auth required", false)
	}
	var selectedProfile string
	var selectedBot *BotRegistryEntry
	if params != nil {
		if botName, ok := params["bot"].(string); ok {
			bot, err := d.botByName(botName)
			if err != nil {
				return messageAuthority{}, err
			}
			selectedProfile = bot.Handle
			selectedBot = &bot
		}
		if profile, ok := params["profile"].(string); ok {
			if selectedProfile != "" && selectedProfile != profile {
				return messageAuthority{}, socketError("VALIDATION_ERROR", "bot and profile do not match", false)
			}
			selectedProfile = profile
		}
	}
	if selectedProfile == "" && auth.Profile != nil {
		selectedProfile = *auth.Profile
	}
	if selectedProfile == "" {
		if required {
			return messageAuthority{}, socketError("VALIDATION_ERROR", "message operation requires a profile or bot", false)
		}
		return messageAuthority{}, nil
	}
	if auth.Profile != nil && *auth.Profile != selectedProfile {
		return messageAuthority{}, socketError("FORBIDDEN", "owner profile does not match operation profile", false)
	}
	if selectedBot != nil {
		return authorityForBot(*selectedBot), nil
	}
	bot, err := d.botByProfile(selectedProfile)
	if err != nil {
		return messageAuthority{}, err
	}
	return authorityForBot(bot), nil
}

func (d *Daemon) resolveRecipients(rawTargets []any) ([]LocalMessageRecipient, *SocketError) {
	out := make([]LocalMessageRecipient, 0, len(rawTargets))
	seen := map[string]struct{}{}
	for _, raw := range rawTargets {
		target := strings.TrimPrefix(raw.(string), "@")
		var bot BotRegistryEntry
		var err *SocketError
		if isBotName(target) {
			bot, err = d.botByName(target)
		} else {
			bot, err = d.botByProfile(target)
		}
		if err != nil {
			return nil, err
		}
		if _, ok := seen[bot.Handle]; ok {
			continue
		}
		seen[bot.Handle] = struct{}{}
		out = append(out, LocalMessageRecipient{
			Profile:    bot.Handle,
			BotName:    bot.Name,
			BotID:      bot.BotID,
			BotAgentID: botAgentID(bot),
		})
	}
	return out, nil
}

func (d *Daemon) botByName(name string) (BotRegistryEntry, *SocketError) {
	d.profileMu.RLock()
	defer d.profileMu.RUnlock()
	names := make([]string, 0, len(d.profileState.BotRegistry))
	for candidate := range d.profileState.BotRegistry {
		names = append(names, candidate)
	}
	sort.Strings(names)
	for _, candidate := range names {
		bot := d.profileState.BotRegistry[candidate]
		if bot.MatchesDaemonSelector(name) {
			return bot, nil
		}
	}
	return BotRegistryEntry{}, socketError("NOT_FOUND", "bot profile is not loaded", false)
}

func (d *Daemon) botByProfile(profile string) (BotRegistryEntry, *SocketError) {
	d.profileMu.RLock()
	defer d.profileMu.RUnlock()
	for _, bot := range d.profileState.BotRegistry {
		if bot.Handle == profile {
			return bot, nil
		}
	}
	if _, ok := d.profileState.AgentProfiles[profile]; ok {
		return profileOnlyNotificationBot(profile), nil
	}
	return BotRegistryEntry{}, socketError("NOT_FOUND", "bot profile is not loaded", false)
}

func authorityForBot(bot BotRegistryEntry) messageAuthority {
	return messageAuthority{
		Profile:     bot.Handle,
		BotName:     bot.Name,
		BotID:       bot.BotID,
		BotAgentID:  botAgentID(bot),
		ClaimHolder: "owner:" + bot.Handle,
	}
}

func authorityForSessionRecord(record SessionRecord) messageAuthority {
	holder := "session:" + record.SessionID + ":" + record.Generation
	return messageAuthority{
		Profile:           record.Profile,
		BotName:           record.BotName,
		BotID:             record.BotID,
		BotAgentID:        record.BotAgentID,
		ClaimHolder:       holder,
		Holder:            holder,
		SessionID:         &record.SessionID,
		SessionScopeType:  &record.ScopeType,
		SessionScopeID:    &record.ScopeID,
		SessionGeneration: &record.Generation,
	}
}

func messageMatchesAuthorityBot(message MessageEnvelope, authority messageAuthority) bool {
	return message.BotName == authority.BotName ||
		sameStableBotIdentity(message.BotID, message.BotAgentID, authority.BotID, authority.BotAgentID)
}

func messageAuthorityAllowsIdentityProfileDrift(req SocketRequest, authority messageAuthority) bool {
	if authority.BotID == "" && authority.BotAgentID == "" {
		return false
	}
	if req.Auth != nil && req.Auth.Mode == "session" {
		return true
	}
	_, hasBotSelector := req.Params["bot"].(string)
	return hasBotSelector
}

func paramsMatchAuthority(params map[string]any, authority messageAuthority) bool {
	if bot, ok := params["bot"].(string); ok && bot != authority.BotName {
		return false
	}
	if profile, ok := params["profile"].(string); ok && profile != authority.Profile {
		return false
	}
	return true
}

func classifyMessageStoreError(err error) *SocketError {
	if errors.Is(err, ErrMessageNotFound) {
		return socketError("NOT_FOUND", "message not found", false)
	}
	if errors.Is(err, ErrMessageConflict) {
		return socketError("CONFLICT", "message state conflict", false)
	}
	return socketError("UPSTREAM_ERROR", "message operation failed", true)
}

func limitFromParams(params map[string]any) int {
	if raw, ok := params["limit"]; ok {
		return numberParam(raw)
	}
	return 50
}

func nextMessageCursor(messages []MessageEnvelope, limit int) *string {
	if limit <= 0 {
		limit = 50
	}
	if len(messages) < limit || len(messages) == 0 {
		return nil
	}
	cursor := messages[len(messages)-1].ID
	return &cursor
}

func numberParam(raw any) int {
	switch value := raw.(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return 0
	}
}

func (d *Daemon) profileHealth() (int, int, int) {
	d.profileMu.RLock()
	defer d.profileMu.RUnlock()
	return len(d.profileState.AgentProfiles), len(d.profileState.BotRegistry), len(d.profileErrors)
}

// agentProfileHandlesLoaded returns the sorted list of agent profile handles
// currently loaded in the daemon. Used by `comment doctor` to diff against
// the profiles that exist on disk.
func (d *Daemon) agentProfileHandlesLoaded() []string {
	d.profileMu.RLock()
	defer d.profileMu.RUnlock()
	handles := make([]string, 0, len(d.profileState.AgentProfiles))
	for handle := range d.profileState.AgentProfiles {
		handles = append(handles, handle)
	}
	sort.Strings(handles)
	return handles
}

// profileLoadErrorsSnapshot returns a copy of the daemon's most recent
// profile-load errors so `comment doctor` can surface them.
func (d *Daemon) profileLoadErrorsSnapshot() []ProfileReloadError {
	d.profileMu.RLock()
	defer d.profileMu.RUnlock()
	out := make([]ProfileReloadError, len(d.profileErrors))
	copy(out, d.profileErrors)
	return out
}

func (d *Daemon) reloadProfiles(ctx context.Context, botletsHome string) ProfileReloadResult {
	result := ProfileReloadResult{
		Added:     []string{},
		Removed:   []string{},
		Restarted: []string{},
		Errors:    []ProfileReloadError{},
	}
	d.profileMu.Lock()
	if botletsHome == "" {
		botletsHome = d.botletsHome
	}
	state, errorsOut := LoadProfileState(ctx, ProfileLoadOptions{
		Paths:          d.paths,
		BotletsHome:    botletsHome,
		DefaultBaseURL: d.defaultBaseURL,
	})

	if HasFatalProfileReloadError(errorsOut) {
		// Directory- or registry-level problem (UNTRUSTED_AGENTS_DIR, etc.).
		// Keep the previously-loaded state so a transient access failure does
		// not unload working profiles.
		result.Errors = append(result.Errors, errorsOut...)
		d.profileErrors = append([]ProfileReloadError{}, errorsOut...)
		result.ProfilesLoaded = len(d.profileState.AgentProfiles)
		result.BotsLoaded = len(d.profileState.BotRegistry)
		d.profileMu.Unlock()
		d.syncNotificationPollers(map[string]string{})
		return result
	}

	// Per-entry errors (e.g. INVALID_AGENT_PROFILE on one file) are reported
	// but should not block valid profiles from loading.
	result.Errors = append(result.Errors, errorsOut...)
	result.Added, result.Removed = diffProfileHandles(d.profileState.AgentProfiles, state.AgentProfiles)
	d.profileState = state
	// Invalidate any impromptu listen claim on a handle that this reload made
	// daemon-managed, so the impromptu listener disarms (its next rewake wait gets
	// claim_lost) instead of double-delivering alongside the managed owner path.
	managedNow := map[string]struct{}{}
	for _, bot := range state.BotRegistry {
		if bot.Handle != "" && bot.ManagedSession.Enabled {
			managedNow[bot.Handle] = struct{}{}
		}
	}
	if dropped := d.listeners.dropClaimsForManaged(managedNow); len(dropped) > 0 {
		d.logger.info("listen.claims_dropped_now_managed", map[string]any{"handles": dropped})
	}
	d.profileErrors = append([]ProfileReloadError{}, errorsOut...)
	d.botletsHome = state.BotletsHome
	result.ProfilesLoaded = len(d.profileState.AgentProfiles)
	result.BotsLoaded = len(d.profileState.BotRegistry)
	if err := WriteBusConfig(d.paths, BusConfig{BotletsHome: state.BotletsHome}); err != nil {
		configErr := ProfileReloadError{Code: "WRITE_BUS_CONFIG_FAILED", Message: "could not persist daemon profile config"}
		result.Errors = append(result.Errors, configErr)
		d.profileErrors = append(d.profileErrors, configErr)
	}
	bots := make(map[string]BotRegistryEntry, len(d.profileState.BotRegistry))
	for name, bot := range d.profileState.BotRegistry {
		bots[name] = bot
	}
	d.profileMu.Unlock()
	if d.store != nil {
		if err := d.store.BackfillBotIdentityColumns(ctx, bots); err != nil {
			backfillErr := ProfileReloadError{Code: "BACKFILL_STORE_IDENTITY_FAILED", Message: "could not backfill message history bot identity"}
			result.Errors = append(result.Errors, backfillErr)
			d.profileMu.Lock()
			d.profileErrors = append(d.profileErrors, backfillErr)
			d.profileMu.Unlock()
		}
	}
	d.syncNotificationPollersFromProfileState()
	return result
}

func diffProfileHandles(oldProfiles, newProfiles map[string]AgentProfile) ([]string, []string) {
	var added []string
	var removed []string
	for handle := range newProfiles {
		if _, ok := oldProfiles[handle]; !ok {
			added = append(added, handle)
		}
	}
	for handle := range oldProfiles {
		if _, ok := newProfiles[handle]; !ok {
			removed = append(removed, handle)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	if added == nil {
		added = []string{}
	}
	if removed == nil {
		removed = []string{}
	}
	return added, removed
}

func prepareSocketPath(socketPath string, pidPath string) error {
	info, err := os.Lstat(socketPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	pidInfo, err := os.Lstat(pidPath)
	if errors.Is(err, os.ErrNotExist) {
		pidInfo = nil
	} else if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("daemon socket path exists and is not a socket: %s", socketPath)
	}
	switch probeDaemonSocket(socketPath) {
	case socketProbeLive:
		return fmt.Errorf("daemon socket already exists: %s", socketPath)
	case socketProbeUnknown:
		return fmt.Errorf("daemon socket exists but could not confirm it is stale: %s", socketPath)
	}
	removed, err := removeIfSameFileStatus(socketPath, info)
	if err != nil {
		return err
	}
	if !removed {
		return fmt.Errorf("daemon socket changed while preparing: %s", socketPath)
	}
	if _, err := removeIfSameFileStatus(pidPath, pidInfo); err != nil {
		return err
	}
	return nil
}

type socketProbeState int

const (
	socketProbeLive socketProbeState = iota
	socketProbeStale
	socketProbeUnknown
)

func probeDaemonSocket(socketPath string) socketProbeState {
	dialer := net.Dialer{Timeout: startupProbeTimeout}
	conn, err := dialer.Dial("unix", socketPath)
	if err != nil {
		if isKnownStaleSocketDialError(err) {
			return socketProbeStale
		}
		return socketProbeUnknown
	}
	defer conn.Close()
	deadline := time.Now().Add(startupProbeTimeout)
	_ = conn.SetDeadline(deadline)
	_, _ = conn.Write([]byte(`{"id":"req_startupprobe","op":"health","params":{}}` + "\n"))
	reader := bufio.NewReader(io.LimitReader(conn, 4096))
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return socketProbeLive
	}
	var response SocketResponse
	if err := json.Unmarshal(line, &response); err != nil {
		return socketProbeLive
	}
	if response.OK {
		return socketProbeLive
	}
	return socketProbeUnknown
}

func isKnownStaleSocketDialError(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT)
}

func readSocketRequestLine(conn net.Conn) ([]byte, error) {
	if err := conn.SetReadDeadline(time.Now().Add(socketReadTimeout)); err != nil {
		return nil, errors.New("invalid request")
	}
	reader := bufio.NewReader(io.LimitReader(conn, maxSocketRequestBytes+1))
	line, err := reader.ReadBytes('\n')
	_ = conn.SetReadDeadline(time.Time{})
	if len(line) > maxSocketRequestBytes {
		return nil, errors.New("request too large")
	}
	if err != nil {
		return nil, errors.New("invalid request")
	}
	return line, nil
}

func removeIfSameFile(path string, expected os.FileInfo) error {
	_, err := removeIfSameFileStatus(path, expected)
	return err
}

func removeIfSameFileStatus(path string, expected os.FileInfo) (bool, error) {
	if expected == nil {
		return false, nil
	}
	current, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !sameFileUnchanged(current, expected) {
		return false, nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return true, nil
}

func sameFileUnchanged(current os.FileInfo, expected os.FileInfo) bool {
	if !os.SameFile(current, expected) {
		return false
	}
	return current.Mode() == expected.Mode() &&
		current.Size() == expected.Size() &&
		current.ModTime().Equal(expected.ModTime())
}

func writeSocketResponse(conn net.Conn, response SocketResponse) error {
	encoded, err := json.Marshal(response)
	if err != nil {
		return err
	}
	if err := conn.SetWriteDeadline(time.Now().Add(socketWriteTimeout)); err != nil {
		return err
	}
	defer func() {
		_ = conn.SetWriteDeadline(time.Time{})
	}()
	_, err = conn.Write(append(encoded, '\n'))
	return err
}

func classifySocketValidationError(err error) *SocketError {
	message := err.Error()
	switch message {
	case "missing auth":
		return socketError("UNAUTHORIZED", message, false)
	default:
		return socketError("VALIDATION_ERROR", message, false)
	}
}

func safeSocketResponseID(id string) string {
	if isSafeSocketRequestID(id) {
		return id
	}
	return ""
}

func socketError(code, message string, retryable bool) *SocketError {
	return &SocketError{Code: code, Message: message, Retryable: retryable}
}
