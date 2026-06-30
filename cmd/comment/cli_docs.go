package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

type cliHelpTopic struct {
	Name     string
	Summary  string
	Usage    []string
	When     []string
	Notes    []string
	Examples []string
	Related  []string
}

var cliHelpOrder = []string{
	"docs",
	"run",
	"messages",
	"activity",
	"bus",
	"status",
	"doctor",
	"diagnose",
	"sync",
	"secrets",
	"skill",
	"mcp",
	"botlets",
	"sessions",
	"runtime",
	"plugin",
	"upgrade",
	"uninstall",
}

var cliHelpTopics = map[string]cliHelpTopic{
	"docs": {
		Name:    "docs",
		Summary: "Print the full local Comment.io CLI guide, or create a comm from a local markdown file.",
		Usage: []string{
			"comment docs",
			"comment docs create <file.md> [--profile <handle>] [--anonymous] [--json]",
			"comment docs import <folder> [--profile <handle>] [--dry-run] [--json]",
		},
		When: []string{
			"Use `comment docs` when you need a complete, local, zero-network reference for the CLI.",
			"Use /llms.txt for the Comment.io REST API contract; use comment docs for local runtime, daemon, sync, and troubleshooting flows.",
			"Use `comment docs create` to turn a markdown file on disk into a new comm (`POST /docs`) without hand-writing the JSON request.",
		},
		Notes: []string{
			"`comment help` is the short command map; `comment docs` is the long-form guide.",
			"`comment docs create` uses the single configured non-Botlets agent profile automatically; Botlets bot profiles are never selected implicitly. With multiple non-Botlets profiles pass --profile, or --anonymous to create without an identity.",
			"Pass `-` as the file to read markdown from stdin. The doc title is derived from the first non-empty markdown line.",
			"Anonymous creation prints the per-doc access token and the identify step required before the token can write.",
		},
		Examples: []string{
			"comment docs create notes.md",
			"comment docs create notes.md --profile yourhandle.agent-name",
			"cat notes.md | comment docs create - --anonymous --json",
		},
		Related: []string{"help", "sync"},
	},
	"run": {
		Name:    "run",
		Summary: "Start a local runtime in tmux and attach it to a Comment.io profile.",
		Usage: []string{
			"comment run",
			"comment run <profile-or-bot>",
			"comment run --runtime <binary> --profile <handle> [--cwd <dir>] [--role main|task] [--detach] [runtime args...]",
			"comment --runtime <binary> --profile <handle> [--cwd <dir>] [--role main|task] [--detach] [runtime args...]",
		},
		When: []string{
			"Recommended for agents that should receive mention and review nudges while they run.",
			"Use the root shorthand for shell aliases, for example `alias claude='comment --runtime claude'`.",
			"Use --detach (or COMMENT_IO_SKIP_ATTACH=1) for headless/no-TTY contexts like a container — the daemon keeps servicing the runtime so it still answers @mentions.",
		},
		Notes: []string{
			"`comment run <profile>` uses the runtime saved in `~/.comment-io/agents/<profile>.json`; legacy profiles without a saved runtime default to Claude.",
			"`comment run` starts Guy, your account guide Botlet; `comment run <bot>` starts another Botlets bot by name.",
			"For Botlets bots created by `comment botlets setup`, the CLI uses the bot's saved runtime and safety flags.",
			"When the host CLI was installed by the Docker sandbox flow and no native host daemon is running, `comment run` delegates to the matching `comment-agent-*` container automatically.",
			"The daemon injects receive nudges into the tmux session; the nudge names a local message id, not a secret or message body.",
			"`--role main` is the default. Use `--role task` for short-lived helper runtimes.",
			"`--detach` launches via the daemon but skips the interactive tmux attach. The daemon keeps servicing the detached session, so the agent answers @mentions with no TTY; attach later by re-running the same `comment run`. COMMENT_IO_SKIP_ATTACH=1 does the same for a fixed launch command.",
		},
		Examples: []string{
			"comment run max.reviewer",
			"comment run",
			"comment run reviewer",
			"comment run --runtime codex --profile max.reviewer",
			"comment run --runtime claude --profile max.reviewer -- --dangerously-skip-permissions",
			"comment run --runtime claude --profile max.reviewer --detach",
		},
		Related: []string{"messages", "activity", "runtime", "bus"},
	},
	"messages": {
		Name:    "messages",
		Summary: "Send, wait for, receive, renew, acknowledge, release, list, and repair local daemon messages.",
		Usage: []string{
			"comment messages wait --bot <recipient>|--profile <handle> [--timeout 30m] [--rewake] [--home ~/.comment-io]",
			"comment messages receive|renew|ack|release --bot <recipient>|--profile <handle> <message-id> [--home ~/.comment-io]",
			"comment messages send --bot <sender> --to <bot-or-profile> --body <markdown> [--home ~/.comment-io]",
			"comment messages list|sent --bot <bot> [--home ~/.comment-io]",
			"comment messages repair [--dry-run] [--message-id msg_...] [--op-id op_...] [--home ~/.comment-io]",
		},
		When: []string{
			"Use this for local work-unit delivery once the daemon has claimed cloud notifications or direct messages.",
			"Inside `comment run`, follow the injected receive nudge, do the work, then ack, release, renew, or complete the local id.",
		},
		Notes: []string{
			"`wait` returns an available local id; `receive` reads and claims that id for work.",
			"`wait --rewake` loops until a message arrives, claims it, prints it, and exits 2 — for a Claude Code `async`+`asyncRewake` Stop hook to turn into a model wake-up.",
			"`ack` means handled. `release` means retry later or let another runtime handle it. `renew` extends a long-running lease.",
			"`repair` reconciles local pending operations when a daemon or runtime was interrupted.",
		},
		Examples: []string{
			"comment messages wait --profile max.reviewer --timeout 10s",
			"comment messages receive --profile max.reviewer msg_abc123",
			"comment messages ack --profile max.reviewer msg_abc123",
		},
		Related: []string{"run", "activity", "bus", "diagnose"},
	},
	"listen": {
		Name:    "listen",
		Summary: "Impromptu-attach an interactive Claude Code session to a free (non-daemon-managed) handle, and coordinate the claim with the daemon.",
		Usage: []string{
			"comment listen <handle> [-- claude args...]",
			"comment listen handles [--json] [--home ~/.comment-io]",
			"comment listen claim --profile <handle> [--session <id>] [--home ~/.comment-io]",
			"comment listen release --profile <handle> [--home ~/.comment-io]",
		},
		When: []string{
			"Use `comment listen <handle>` to launch claude preset to listen on a handle the daemon does NOT run as a managed bot.",
			"The plugin uses `listen claim`/`release` to mark a handle busy for the session, and `listen handles` to discover what is configured and free.",
		},
		Notes: []string{
			"`claim` refuses MANAGED_HANDLE for daemon-managed bots (use `comment run <handle>`) and HANDLE_BUSY when another live session already claims the handle (no takeover).",
			"`<handle>` execs `claude` with COMMENT_IO_PROFILE=<handle> and COMMENT_IO_LISTEN=1 set; the headline path is bare `claude`, which the plugin handles on its own.",
		},
		Examples: []string{
			"comment listen handles --json",
			"comment listen claim --profile max.research-reader --session $CLAUDE_CODE_SESSION_ID",
			"comment listen release --profile max.research-reader",
		},
		Related: []string{"run", "messages", "bus"},
	},
	"activity": {
		Name:    "activity",
		Summary: "Mark a received local work item complete when no visible Comment.io reply is needed.",
		Usage: []string{
			"comment activity complete <message-id> [--home ~/.comment-io] [--profile handle]",
		},
		When: []string{
			"Use this after handling a runtime-delivered request that should not produce a visible comment or direct-message reply.",
		},
		Notes: []string{
			"From inside `comment run`, the profile is usually inferred from runtime environment variables.",
			"Completion publishes the activity state and acknowledges the local message.",
		},
		Examples: []string{
			"comment activity complete msg_abc123",
		},
		Related: []string{"run", "messages"},
	},
	"bus": {
		Name:    "bus",
		Summary: "Manage the Comment.io daemon (the background process that keeps agents loaded on this computer) and its embedded SQLite state.",
		Usage: []string{
			"comment bus init [--home ~/.comment-io]",
			"comment bus health [--home ~/.comment-io]",
			"comment bus run [--home ~/.comment-io] [--botlets-home ~/botlets] [--tmux-bin /path/to/tmux]",
			"comment bus install [--home ~/.comment-io] [--botlets-home ~/botlets] [--bin /path/to/comment] [--dry-run]",
			"comment bus start|stop|status|uninstall [--home ~/.comment-io]",
			"comment bus pair [--home ~/.comment-io] [--label \"Max's MacBook\"] [--base-url https://comment.io] [--force] [--cli|--resume]",
			"comment bus unpair [--home ~/.comment-io] [--yes]",
			"comment bus reload-profiles [--home ~/.comment-io] [--botlets-home ~/botlets]",
			"comment bus repair [--dry-run] [--message-id msg_...] [--op-id op_...] [--home ~/.comment-io]",
			"comment daemon health|install|start|stop|status|uninstall [--home ~/.comment-io]",
		},
		When: []string{
			"Use this to install, run, inspect, and repair the local daemon used by `comment run`, `comment messages`, and background sync.",
			"`comment daemon ...` is a compatibility alias for the older command shape.",
		},
		Notes: []string{
			"On macOS, `install` uses launchd. On Linux, it uses systemd --user when available.",
			"`health` reports daemon/store health. `status` reports persistent service status.",
			"`reload-profiles` asks the daemon to re-read `~/.comment-io/agents/*.json`.",
			"`pair` runs a browser-approved device flow and stores this computer's daemon credentials at `~/.comment-io/bus/daemon-auth.json` (0600). In an agent/tool call, use `pair --cli` to print the approval URL/code and exit, then after the human approves run `pair --resume` to finish. `pair --force` replaces the local pairing without revoking the previous daemon or deleting agent profiles; journaled profiles refresh through the new daemon. Revoke the previous computer in the web app after running sessions finish. `unpair` revokes this daemon server-side and, after confirmed revoke, cleans profiles installed by that daemon's enrollments.",
		},
		Examples: []string{
			"comment bus install --bin \"$(command -v comment)\"",
			"comment bus status",
			"comment bus health",
		},
		Related: []string{"doctor", "run", "messages", "sync"},
	},
	"status": {
		Name:    "status",
		Summary: "Show a setup-readiness panel: is this computer paired, and is a coding agent (Claude or Codex) logged in?",
		Usage: []string{
			"comment status [--home ~/.comment-io] [--no-color]",
		},
		When: []string{
			"Run this right after install to see what's left before agents can run here.",
			"The login step glows until you log in to a coding agent — run `claude` (or `claude setup-token`) or `codex login`.",
		},
		Notes: []string{
			"Status is the friendly human view of the same checks `comment doctor` reports as JSON.",
			"Color/formatting auto-disables when output isn't a terminal or NO_COLOR is set.",
		},
		Examples: []string{
			"comment status",
		},
		Related: []string{"doctor", "run", "bus"},
	},
	"doctor": {
		Name:    "doctor",
		Summary: "Check the local CLI environment and optionally apply safe local repairs.",
		Usage: []string{
			"comment doctor [--home ~/.comment-io] [--fix]",
		},
		When: []string{
			"Use this first when local delivery, profile loading, daemon installation, or CLI setup seems broken.",
			"Use `--fix` to create missing directories, tighten file permissions, install/start the daemon when safe, and reload profiles.",
		},
		Notes: []string{
			"Doctor prints JSON and exits 0 when every check is ok or fixed.",
			"Doctor exits 2 when a warning or error remains after checks.",
			"Doctor checks local health; it does not create a support bundle. That is `comment diagnose`.",
		},
		Examples: []string{
			"comment doctor",
			"comment doctor --fix",
		},
		Related: []string{"diagnose", "bus", "plugin"},
	},
	"diagnose": {
		Name:    "diagnose",
		Summary: "Create a diagnostic support bundle and optionally upload it.",
		Usage: []string{
			"comment diagnose [--home ~/.comment-io] [--include-history] [--include-file PATH] [--upload|--no-upload] [--output PATH] [--base-url URL]",
		},
		When: []string{
			"Use this when you need to hand logs and non-sensitive system metadata to Comment.io support.",
			"Use `--no-upload` to keep the bundle local, or `--upload`/`--yes` to upload without an interactive prompt.",
		},
		Notes: []string{
			"Diagnose writes a `.tar.gz` bundle under `~/.comment-io/diagnostics/` unless `--output` is set.",
			"It includes daemon logs and system/profile metadata. It omits message history unless `--include-history` is explicitly provided.",
			"`--include-file PATH` attaches one extra local file, such as a setup transcript, to the bundle.",
			"Diagnose does not repair anything. For local checks and safe repairs, use `comment doctor --fix`.",
		},
		Examples: []string{
			"comment diagnose --no-upload",
			"comment diagnose --include-history --upload",
		},
		Related: []string{"doctor", "bus", "messages"},
	},
	"sync": {
		Name:    "sync",
		Summary: "Mirror Comment.io library documents to read-only local Markdown projections.",
		Usage: []string{
			"comment sync login|once|watch|status|doctor|conflicts|recover|migrate|enable|disable|logout [--home ~/.comment-io]",
			"COMMENT_IO_LIBRARY_SYNC_KEY=... comment sync login --api-key-env [--base-url https://comment.io] [--root ~/Comment\\ Docs]",
		},
		When: []string{
			"Use this when a local agent needs to search or read the `~/Comment Docs` mirror.",
			"Use REST or the web UI to write; synced files are projections, not a writeback surface.",
		},
		Notes: []string{
			"`enable` starts persistent background sync through the daemon. `once` performs a one-shot sync.",
			"`conflicts` and `recover` inspect local edits that were preserved instead of uploaded.",
			"When `COMMENT_IO_LOCAL_DOCS_ROOT` is set, prefer its `llms.txt` over a network fetch for startup docs.",
		},
		Examples: []string{
			"comment sync login",
			"comment sync once",
			"comment sync status --json",
		},
		Related: []string{"bus", "doctor"},
	},
	"secrets": {
		Name:    "secrets",
		Summary: "Store and retrieve computer-local secrets for agents and bots.",
		Usage: []string{
			"comment secrets add <name> <value>|--value-stdin [--home ~/.comment-io]",
			"comment secrets get <name> [--home ~/.comment-io]",
		},
		When: []string{
			"Use this for local credentials that should not be pasted into docs, comments, logs, or messages.",
		},
		Notes: []string{
			"Secrets are stored under `~/.comment-io/.secrets` or `COMMENT_IO_HOME/.secrets` with owner-only permissions.",
			"Prefer `--value-stdin` when shell history may be retained.",
		},
		Examples: []string{
			"printf '%s' \"$OPENAI_API_KEY\" | comment secrets add OPENAI_API_KEY --value-stdin",
			"comment secrets get OPENAI_API_KEY",
		},
		Related: []string{"run", "botlets"},
	},
	"skill": {
		Name:    "skill",
		Summary: "Install the universal Comment.io skill into your coding agent(s).",
		Usage: []string{
			"comment skill install [--base-url https://comment.io]",
		},
		When: []string{
			"Use this to teach a coding agent (Claude Code, Codex, Cursor, Gemini, OpenClaw) the Comment.io API. The native equivalent of `curl -fsSL <base>/skill | sh`.",
		},
		Notes: []string{
			"Detects each agent on this machine and writes `skills/comment/SKILL.md` into its native skills directory.",
			"When no skills directory is found, writes an `~/.agents/AGENTS.md` fallback instead.",
			"The skill is a pointer to `<base>/llms.txt`; it is downloaded text, not executed code.",
		},
		Examples: []string{
			"comment skill install",
		},
		Related: []string{"run", "docs"},
	},
	"mcp": {
		Name:    "mcp",
		Summary: "Launch the Comment.io MCP server for an installed profile.",
		Usage: []string{
			"comment mcp run --profile <handle> [--home ~/.comment-io] [--base-url https://comment.io]",
		},
		When: []string{
			"Use this when connecting a local MCP-capable host to Comment.io tools.",
		},
		Notes: []string{
			"The command loads the selected profile and exports profile, secret, base URL, home, and daemon-availability environment to the MCP process.",
		},
		Examples: []string{
			"comment mcp run --profile max.reviewer",
		},
		Related: []string{"doctor", "run"},
	},
	"botlets": {
		Name:    "botlets",
		Summary: "Install and inspect Botlets bot local runtime state.",
		Usage: []string{
			"comment botlets setup --bot <owner.slug|slug> [--owner handle] [--home ~/.comment-io] [--botlets-home ~/botlets]",
			"comment botlets logs --bot <name-or-handle> [--run blr_...|--message msg_...] [--home ~/.comment-io] [--claude-home ~/.claude] [--codex-home ~/.codex]",
			"comment botlets status [--bot <name-or-handle>] [--home ~/.comment-io] [--botlets-home ~/botlets]",
		},
		When: []string{
			"Use this for Botlets bots that need a Comment.io profile, local brain projection, and runtime wiring.",
		},
		Notes: []string{
			"`setup` writes local bot/profile state. `logs` maps local run history to Claude/Codex JSONL transcripts. `status` reports what is configured and what still needs attention.",
		},
		Examples: []string{
			"comment botlets setup --bot max.reviewer",
			"comment botlets logs --bot max.reviewer --run blr_0123456789abcdef0123456789abcdef",
			"comment botlets status --bot max.reviewer",
		},
		Related: []string{"sync", "run", "secrets"},
	},
	"sessions": {
		Name:    "sessions",
		Summary: "Manage daemon-tracked bot sessions and nudge them with local message ids.",
		Usage: []string{
			"comment sessions start|status|stop [bot] [--home ~/.comment-io]",
			"comment sessions nudge [bot] <message-id> [--home ~/.comment-io]",
		},
		When: []string{
			"Use this for managed bot session lifecycle operations.",
		},
		Notes: []string{
			"`nudge` delivers a specific local message id into the selected managed session.",
		},
		Related: []string{"run", "messages", "botlets"},
	},
	"runtime": {
		Name:    "runtime",
		Summary: "Inspect or stop transient runtimes launched with `comment run`.",
		Usage: []string{
			"comment runtime status|list --profile <handle> [--home ~/.comment-io]",
			"comment runtime stop --profile <handle> <run-id> [--home ~/.comment-io]",
		},
		When: []string{
			"Use this when a runtime was launched through `comment run` and you need its current state or need to stop it.",
		},
		Related: []string{"run", "sessions"},
	},
	"plugin": {
		Name:    "plugin",
		Summary: "Check and repair host plugin installation state.",
		Usage: []string{
			"comment plugin doctor [--claude-home ~/.claude] [--fix]",
		},
		When: []string{
			"Use this for Claude Code plugin cache or installation issues.",
		},
		Notes: []string{
			"This is scoped to host plugin state. Use `comment doctor` for the broader local daemon/profile/CLI health check.",
		},
		Related: []string{"doctor"},
	},
	"upgrade": {
		Name:    "upgrade",
		Summary: "Upgrade the npm CLI package and refresh the local daemon install.",
		Usage: []string{
			"comment upgrade [--home ~/.comment-io] [--botlets-home ~/botlets] [--package @comment-io/cli@latest] [--skip-daemon] [--dry-run]",
		},
		When: []string{
			"Use this when the installed CLI is stale or when setup guidance asks for the latest package.",
		},
		Notes: []string{
			"Set `COMMENT_IO_CLI_PACKAGE` or pass `--package` to pin a staging or custom package.",
		},
		Related: []string{"doctor", "bus"},
	},
	"uninstall": {
		Name:    "uninstall",
		Summary: "Remove local Comment.io CLI state, plugins, daemon service, Docker-mode agent containers, and optionally sync projections.",
		Usage: []string{
			"comment uninstall [--home ~/.comment-io] [--botlets-home ~/botlets] [--yes] [--dry-run] [--skip-cli] [--skip-plugins] [--skip-docker] [--remove-image] [--keep-sync-root]",
		},
		When: []string{
			"Use this when intentionally cleaning this computer's local Comment.io installation.",
		},
		Notes: []string{
			"Prefer `--dry-run` first so you can inspect what will be removed.",
			"If you installed with the Docker option, this also removes the comment-agent containers and their named volumes (which hold the daemon pairing and agent credentials). Pass `--skip-docker` to leave them, or `--remove-image` to also drop the pulled comment-agent image.",
		},
		Related: []string{"doctor", "bus", "sync"},
	},
}

var cliHelpAliases = map[string]string{
	"daemon":        "bus",
	"daemon alias":  "bus",
	"local daemon":  "bus",
	"message":       "messages",
	"notification":  "messages",
	"notifications": "messages",
	"secret":        "secrets",
	"bot":           "botlets",
	"bots":          "botlets",
	"doc":           "docs",
	"help":          "docs",
}

func runHelp(args []string) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Println(cliUsage())
		return nil
	}
	if len(args) != 1 {
		return errors.New("usage: comment help [command]")
	}
	topic, ok := lookupHelpTopic(args[0])
	if !ok {
		return fmt.Errorf("unknown help topic %q\n\nKnown topics: %s", args[0], strings.Join(knownHelpTopics(), ", "))
	}
	fmt.Println(formatHelpTopic(topic))
	return nil
}

func runDocs(args []string) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		topic, _ := lookupHelpTopic("docs")
		fmt.Println(formatHelpTopic(topic))
		return nil
	}
	if len(args) > 0 && args[0] == "create" {
		return runDocsCreate(args[1:])
	}
	if len(args) > 0 && args[0] == "import" {
		return runDocsImport(args[1:])
	}
	if len(args) > 0 {
		return errors.New("comment docs accepts only the create and import subcommands; run `comment docs create <file.md>`, `comment docs import <folder>`, or `comment help <command>` for a focused topic")
	}
	fmt.Println(cliDocs())
	return nil
}

func cliUsage() string {
	return strings.Join([]string{
		"Comment.io CLI",
		"",
		"Usage:",
		"  comment help [command]",
		"  comment docs",
		"  comment <command> [options]",
		"",
		"Start here:",
		"  comment docs                         Full local CLI guide for agents",
		"  comment docs create <file.md>        Create a comm from a local markdown file",
		"  comment docs import <folder>         Import a folder of markdown (e.g. an Obsidian vault) into your Library",
		"  comment doctor [--fix]               Check local health and apply safe repairs",
		"  comment diagnose [--no-upload]       Build a support bundle; does not repair",
		"  comment run <profile-or-bot>        Start a saved agent profile or Botlets bot",
		"  comment run                         Start Guy, your account guide Botlet",
		"  comment run --runtime <binary> --profile <handle> [runtime args...]",
		"                                       Start a local runtime for daemon nudges",
		"",
		"Daemon and local delivery:",
		"  comment bus install [--bin /path/to/comment]",
		"                                       Install or refresh the background daemon service",
		"  comment bus pair                     Pair this computer with your Comment.io account (one time)",
		"  comment bus unpair [--yes]           Revoke and remove this computer's daemon credentials",
		"  comment bus status                   Check persistent service status",
		"  comment messages wait --bot <recipient>|--profile <handle> [--timeout 30m] [--rewake]",
		"                                       Wait for a local daemon work item",
		"  comment listen <handle>              Launch claude listening on a free (unmanaged) handle",
		"  comment mcp run --profile <handle>   Launch the MCP server for one installed profile",
		"",
		"Common agent workflow:",
		"  1. comment doctor --fix",
		"  2. comment bus status",
		"  3. comment run yourhandle.agent-name",
		"  4. On a nudge: comment messages receive --profile yourhandle.agent-name msg_...",
		"  5. After work: comment messages ack --profile yourhandle.agent-name msg_...",
		"",
		"Commands:",
		"  run          Launch a runtime in tmux and bind it to a profile",
		"  messages     Local daemon work-unit wait/receive/renew/ack/release/send/list",
		"  listen       Launch/claim/release impromptu listening on a free handle",
		"  activity     Mark a local work item complete when no visible reply is needed",
		"  bus          Init, install, run, inspect, and repair the local daemon",
		"  sync         Read-only local Markdown projections under ~/Comment Docs",
		"  secrets      Computer-local secret storage for agents and bots",
		"  mcp          Launch the Comment.io MCP server for a profile",
		"  botlets    Install and inspect Botlets bot local state",
		"  sessions     Manage daemon-tracked bot sessions",
		"  runtime      Inspect or stop runtimes launched by comment run",
		"  plugin       Check host plugin installation state",
		"  doctor       Local environment checks and safe repairs",
		"  diagnose     Diagnostic bundle creation/upload for support",
		"  upgrade      Upgrade the CLI package and refresh daemon install",
		"  uninstall    Remove local CLI state, daemon service, plugins, or sync files",
		"",
		"Doctor vs diagnose:",
		"  comment doctor   checks this computer and can fix safe local setup issues.",
		"  comment diagnose creates a tar.gz support bundle and optionally uploads it; it never repairs.",
		"",
		"More help:",
		"  comment help doctor",
		"  comment help diagnose",
		"  comment help messages",
		"  comment docs",
		"",
		"Staging:",
		"  --staging (or the comment-staging command, or COMMENT_IO_ENV=staging) runs against a fully separate set:",
		"  ~/.comment-io-staging state, https://comt.dev endpoint, ~/Comment Docs (staging) sync folder, and its own daemon.",
		"  --production forces production for a single invocation. Production is the default.",
		"",
		"Environment:",
		"  COMMENT_IO_ENV selects the deployment (production or staging); --staging/--production and comment-staging set it.",
		"  COMMENT_IO_HOME overrides the state root (default ~/.comment-io, or ~/.comment-io-staging under staging).",
		"  COMMENT_IO_BASE_URL overrides the default endpoint (https://comment.io, or https://comt.dev under staging).",
		"  COMMENT_IO_STAGING_BASE_URL overrides the staging endpoint only.",
		"  COMMENT_IO_LOCAL_SYNC_ROOT and COMMENT_IO_LOCAL_DOCS_ROOT are set inside managed runtimes when local sync is enabled.",
	}, "\n")
}

func cliDocs() string {
	var b strings.Builder
	b.WriteString("# Comment.io CLI Docs\n\n")
	b.WriteString("This is the local CLI reference. Use it when you need to understand `comment` without fetching remote docs or burning context on general API material. For REST endpoints, edit conflict behavior, and per-doc tokens, fetch `/llms.txt` from the target Comment.io host.\n\n")
	b.WriteString("## Mental Model\n\n")
	b.WriteString("- The CLI owns local behavior on this computer: the daemon, tmux runtime delivery, local messages, local sync projections, local secrets, diagnostics, and setup checks.\n")
	b.WriteString("- The REST API owns remote document behavior: creating comms, reading markdown, editing, commenting, suggestions, access, and agent profile management.\n")
	b.WriteString("- `~/.comment-io/agents/*.json` contains installed agent profiles. Treat `agent_secret` values as bearer credentials.\n")
	b.WriteString("- `COMMENT_IO_HOME` changes the local state root. By default it is `~/.comment-io`.\n")
	b.WriteString("- Running as `comment-staging`, with `--staging`, or with `COMMENT_IO_ENV=staging` targets a separate staging set: `~/.comment-io-staging` state, the `https://comt.dev` endpoint, a `~/Comment Docs (staging)` sync folder, and its own daemon. Production is the default and is untouched.\n\n")
	b.WriteString("## Fast Path For Running Agents\n\n")
	b.WriteString("```bash\n")
	b.WriteString("comment doctor --fix\n")
	b.WriteString("comment bus status\n")
	b.WriteString("comment run yourhandle.agent-name\n")
	b.WriteString("# When the daemon nudges the runtime:\n")
	b.WriteString("comment messages receive --profile yourhandle.agent-name msg_...\n")
	b.WriteString("# Do the requested work through Comment.io REST/API or web UI, then:\n")
	b.WriteString("comment messages ack --profile yourhandle.agent-name msg_...\n")
	b.WriteString("```\n\n")
	b.WriteString("If `comment messages receive` returns `replay_skipped: true`, the notification was already settled; do not respond, ack, release, or complete it. If the work was handled but should not create a visible Comment.io reply, run `comment activity complete msg_...` instead of acking manually. If you cannot handle it, run `comment messages release --profile yourhandle.agent-name msg_...`.\n\n")
	b.WriteString("## Doctor vs Diagnose\n\n")
	b.WriteString("- `comment doctor` is the local health check. It prints structured JSON, verifies setup, and with `--fix` applies safe local repairs such as directory creation, permission tightening, daemon install/start, and profile reload.\n")
	b.WriteString("- `comment diagnose` is the support bundle command. It gathers daemon logs plus system/profile metadata into a `.tar.gz`, can upload it to support, and never changes local setup. It excludes message history unless `--include-history` is explicit.\n\n")
	b.WriteString("## Work Unit Lifecycle\n\n")
	b.WriteString("1. `comment messages wait` finds an available local message id outside `comment run`; managed runtimes usually receive a fixed nudge instead.\n")
	b.WriteString("2. `comment messages receive` claims and prints that message, or returns `replay_skipped: true` when the cloud notification was already settled.\n")
	b.WriteString("3. Do the work. For document work, use the Comment.io REST API or web UI.\n")
	b.WriteString("4. `comment messages renew` before a long lease expires.\n")
	b.WriteString("5. `comment messages ack` after a visible response, `comment activity complete` when no visible response is needed, or `comment messages release` when the work should be retried.\n\n")
	b.WriteString("## Command Reference\n")
	for _, name := range cliHelpOrder {
		topic, ok := lookupHelpTopic(name)
		if !ok {
			continue
		}
		b.WriteString("\n")
		b.WriteString(formatMarkdownTopic(topic))
	}
	b.WriteString("\n## Environment Variables\n\n")
	b.WriteString("- `COMMENT_IO_ENV`: select the deployment, `production` (default) or `staging`. The `--staging`/`--production` flags and the `comment-staging` command set this for you. Staging uses a fully separate state root, endpoint, sync folder, and daemon so it can coexist with production on one computer.\n")
	b.WriteString("- `COMMENT_IO_HOME`: override the local state root, default `~/.comment-io` (`~/.comment-io-staging` under staging).\n")
	b.WriteString("- `COMMENT_IO_BASE_URL`: override the default Comment.io host for profile loading and diagnostics upload when a command supports it. Defaults to `https://comment.io` (`https://comt.dev` under staging).\n")
	b.WriteString("- `COMMENT_IO_STAGING_BASE_URL`: override the staging endpoint only, leaving production untouched.\n")
	b.WriteString("- `COMMENT_IO_PROFILE`: set inside `comment run` runtimes so profile-scoped commands can infer the handle.\n")
	b.WriteString("- `COMMENT_IO_RUNTIME_RUN`: set inside `comment run` runtimes to mark the process as daemon-managed.\n")
	b.WriteString("- `COMMENT_IO_LOCAL_SYNC_ROOT`: set inside managed runtimes when local sync projections are available.\n")
	b.WriteString("- `COMMENT_IO_LOCAL_DOCS_ROOT`: set inside managed runtimes when local synced agent docs are available; prefer its `llms.txt` over a network fetch.\n")
	b.WriteString("- `COMMENT_IO_LIBRARY_SYNC_KEY`: optional scoped key consumed by `comment sync login --api-key-env`.\n")
	b.WriteString("- `COMMENT_IO_CLI_PACKAGE`: optional package override for `comment upgrade`.\n\n")
	b.WriteString("## Local File Safety\n\n")
	b.WriteString("- Local sync files are read-only projections. Do not edit them expecting remote writes.\n")
	b.WriteString("- Secrets stored through `comment secrets` are local credentials. Do not paste them into comms, logs, diagnostics, or direct messages.\n")
	b.WriteString("- Diagnostics bundles may include logs and, with `--include-history`, local message history. Review before sharing when needed.\n")
	return strings.TrimSpace(b.String())
}

func formatHelpTopic(topic cliHelpTopic) string {
	var b strings.Builder
	b.WriteString("comment ")
	b.WriteString(topic.Name)
	b.WriteString("\n\n")
	b.WriteString(topic.Summary)
	b.WriteString("\n\n")
	writeTextSection(&b, "Usage", topic.Usage)
	writeTextSection(&b, "Use when", topic.When)
	writeTextSection(&b, "Notes", topic.Notes)
	writeTextSection(&b, "Examples", topic.Examples)
	if len(topic.Related) > 0 {
		b.WriteString("Related: ")
		b.WriteString(strings.Join(topic.Related, ", "))
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

func formatMarkdownTopic(topic cliHelpTopic) string {
	var b strings.Builder
	b.WriteString("### comment ")
	b.WriteString(topic.Name)
	b.WriteString("\n\n")
	b.WriteString(topic.Summary)
	b.WriteString("\n\n")
	writeMarkdownSection(&b, "Usage", topic.Usage)
	writeMarkdownSection(&b, "Use when", topic.When)
	writeMarkdownSection(&b, "Notes", topic.Notes)
	writeMarkdownSection(&b, "Examples", topic.Examples)
	if len(topic.Related) > 0 {
		b.WriteString("Related: ")
		b.WriteString("`")
		b.WriteString(strings.Join(topic.Related, "`, `"))
		b.WriteString("`.\n")
	}
	return b.String()
}

func writeTextSection(b *strings.Builder, title string, values []string) {
	if len(values) == 0 {
		return
	}
	b.WriteString(title)
	b.WriteString(":\n")
	for _, value := range values {
		b.WriteString("  ")
		b.WriteString(value)
		b.WriteString("\n")
	}
	b.WriteString("\n")
}

func writeMarkdownSection(b *strings.Builder, title string, values []string) {
	if len(values) == 0 {
		return
	}
	b.WriteString("**")
	b.WriteString(title)
	b.WriteString("**\n\n")
	for _, value := range values {
		b.WriteString("- ")
		if title == "Usage" || title == "Examples" {
			b.WriteString("`")
			b.WriteString(value)
			b.WriteString("`")
		} else {
			b.WriteString(value)
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
}

func lookupHelpTopic(value string) (cliHelpTopic, bool) {
	topic := canonicalHelpTopic(value)
	if alias, ok := cliHelpAliases[topic]; ok {
		topic = alias
	}
	found, ok := cliHelpTopics[topic]
	return found, ok
}

func canonicalHelpTopic(value string) string {
	topic := strings.ToLower(strings.TrimSpace(value))
	topic = strings.TrimPrefix(topic, "comment ")
	topic = strings.TrimPrefix(topic, "comment-")
	topic = strings.TrimSpace(topic)
	if strings.Contains(topic, " ") {
		topic = strings.Fields(topic)[0]
	}
	return topic
}

func knownHelpTopics() []string {
	topics := make([]string, 0, len(cliHelpTopics))
	for topic := range cliHelpTopics {
		topics = append(topics, topic)
	}
	sort.Strings(topics)
	return topics
}
