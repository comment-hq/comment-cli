# comment-cli

The local daemon and CLI for [Comment.io](https://comment.io) — the
agent-native document editor where humans and AI write together.

`comment-cli` provides the `comment` command: a local message bus, a file-sync
engine that mirrors your Comment.io docs to a local folder, and a managed-runtime
launcher for agents — all talking to the Comment.io API.

> This repository is a read-only mirror of Comment.io's CLI/daemon module, synced
> from the upstream monorepo. Issues and discussion are welcome; code changes are
> made upstream and synced here.

## Install

```bash
# Homebrew / prebuilt binaries: see Releases.
# Or build from source (Go 1.26+):
go install github.com/comment-hq/comment-cli/cmd/comment@latest
```

`comment-cli` is a pure-Go module (`modernc.org/sqlite`, no cgo), so
`CGO_ENABLED=0` produces a fully static binary.

> **Source builds are unversioned.** `go install`/`go build` here omit the
> release version stamp, so the binary reports `version=dev` and the daemon's
> auto-update + minimum-version checks treat it as a dev build (it won't
> auto-upgrade itself when production raises the minimum CLI version). That's fine
> for development or trying it out; for a **long-running daemon** that should track
> the published minimum, use the prebuilt release binaries or the `@comment-io/cli`
> npm package — or stamp a version yourself:
> `go build -ldflags "-X main.version=X.Y.Z" ./cmd/comment`.

## Quick start

```bash
comment --help          # all commands
comment version
comment bus install     # install + start the always-on background daemon (launchd/systemd)
comment sync login      # link this machine
comment sync enable     # turn on background sync (login alone leaves it off)
comment daemon health   # verify the daemon is up
```

(`comment bus install` sets up the daemon as a user service so its background
workers actually run; `go install` builds only the binary.)

See `comment docs` for the full local CLI reference, and
<https://comment.io/llms.txt> for the agent-facing HTTP API.

## Build the agent sandbox image

Comment.io publishes a prebuilt **agent sandbox** image — the full daemon plus the
Claude Code and Codex runtimes, running *inside* a container so the agent (which
executes code, runs tools, and edits files) is isolated from your host. The
`/setup` "Run in a Docker Image" one-liner pulls it from GHCR.

The build recipe lives in this repo, so you can inspect, customize, and build it
yourself instead of trusting the prebuilt image. From the repo root:

```bash
# Build context is the module root; the Dockerfile builds the comment daemon
# from this source, then layers the agent runtimes on top.
docker build -f docker/Dockerfile.agent -t comment-agent .
```

To run it via Compose (handles host UID/GID + volumes for you), first set the
required host paths — `compose.agent.yml` refuses to start until `COMMENT_DOCS_DIR`
points at an absolute directory — then bring it up:

```bash
export COMMENT_DOCS_DIR="$HOME/comment-agent/docs"      # synced docs (required)
export COMMENT_AGENTS_DIR="$HOME/comment-agent/agents"  # plain-agent profiles (optional)
mkdir -p "$COMMENT_DOCS_DIR" "$COMMENT_AGENTS_DIR"
docker compose -f docker/compose.agent.yml up -d --build
```

See [`docker/README.agent.md`](docker/README.agent.md) for the full environment,
host/container trust model, pairing, and runtime login — read it before running
Compose. The prebuilt image and a
build-it-yourself image are interchangeable — pick whichever you prefer.

## Not included in this repo

`comment mcp run` (the Model Context Protocol server) needs the separate
Comment.io MCP bundle, which ships with the `@comment-io/cli` npm package and is
**not** part of this source repo. Without it the command reports "could not locate
Comment.io MCP entrypoint" — install the npm CLI, or pass an explicit
`--entrypoint` to your own MCP build.

## Security

`comment-cli` runs a local control daemon and holds local credentials. See
[`SECURITY.md`](SECURITY.md) for the trust model and how to report issues.

## License

[MIT](LICENSE) © Every.
