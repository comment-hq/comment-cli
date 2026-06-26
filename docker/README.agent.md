# Agent sandbox (Docker)

Run the **full** `comment` daemon **and** the agent runtimes (Claude Code +
Codex) inside one container, so the dangerous part — the agent that executes
code, runs tools, and edits files — is sandboxed away from your machine.

This is a different image and trust model from the caged sync daemon
(`Dockerfile`, distroless, sync-only). That cage isolates the benign sync daemon;
**this sandbox isolates the agent itself.**

## The host / container split

One daemon runs in the container and enrolls everything your account owns. The
two kinds of handle land in different places — on purpose:

- **Botlets** (handles with a managed session) **stay in the container.** The
  daemon spins up their Claude/Codex tmux sessions on demand and answers their
  `@mentions` headlessly. Their credentials never leave the box.
- **Plain agents** (handles a coding agent grabs and uses temporarily) are
  **projected to the host.** The container writes their profile files to a host
  folder so a host-native coding agent (e.g. a Claude Code session on your
  machine) can pick a handle and post as it. Plain agents need no daemon — just
  the profile file + the REST API.

This mirrors the server's own model: plain agents install on every paired daemon;
each Botlet binds to exactly one (here, the container).

```
host                                    sandbox (container)
────                                    ───────────────────
comment-agent-state  (named volume)   ▶ /state           daemon creds, pairing, sqlite, socket, agent profiles
comment-agent-home   (named volume)   ▶ /home/agent      ~/.claude + ~/.codex creds, Botlet sessions, workspace
$COMMENT_DOCS_DIR    ⇄ bind rw        ⇄ /docs            synced documents (projected to host)
$COMMENT_AGENTS_DIR  ⇄ bind rw        ⇄ /projected-agents  PLAIN-agent profiles (projected to host)
                            (no docker.sock · no host network · no other host FS)
```

The agent is **powerful inside the box** (open egress, writable workspace, shell,
tmux, git); the **host is protected** — no Docker socket, no host network, and
the only host filesystem it can touch is `/docs` and the plain-agent projection.
**Botlet credentials and daemon control never leave the container.**

## What's inside

- The full `comment` daemon (`comment bus run`) — enrollment, notification
  pollers, the managed/transient runtime launcher (auto-runs Botlets).
- **tmux** (the only multiplexer — no bmux).
- **`claude`** (Claude Code) + **`codex`** (Codex) + Node, git, ripgrep, curl.
- A non-root `agent` user with a writable home.
- A small projector (`project-agents.js`) that mirrors plain-agent profiles to
  the host and prunes stale/Botlet ones.

## Quickstart

Prereqs: Docker (Desktop or colima). From `docker`:

> **Linux note:** bind mounts preserve uid/gid, so the container user must own the
> host folders it writes (`/docs`, `/projected-agents`). Before `up --build`, run
> `export HOST_UID=$(id -u) HOST_GID=$(id -g)` so the image's `agent` user matches
> you. (On macOS Docker Desktop bind mounts are uid-mapped — no action needed.)

```bash
# 1. Configure host folders + the target host, then bring the daemon up.
cat > .env <<'EOF'
COMMENT_DOCS_DIR=$HOME/comment-agent/docs        # synced docs, projected to host
COMMENT_AGENTS_DIR=$HOME/comment-agent/agents     # plain-agent profiles, projected to host
COMMENT_IO_ENV=production                          # or "staging"
# Personal staging? point at YOUR host (see "Pick the right host" below):
# COMMENT_IO_ENV=staging
# COMMENT_IO_STAGING_BASE_URL=https://you.comt.dev
EOF
mkdir -p "$HOME/comment-agent/docs" "$HOME/comment-agent/agents"
docker compose -f compose.agent.yml up -d --build

# 2. Pair the daemon to your account (device-code; one-time).
docker compose -f compose.agent.yml exec comment-agent comment bus pair

# 3. Enable library sync, projecting docs to the host /docs bind.
docker compose -f compose.agent.yml exec comment-agent comment sync login --root /docs
docker compose -f compose.agent.yml exec comment-agent comment sync enable

# 4. Log the agent runtimes in (in-container subscription login — see below).
docker compose -f compose.agent.yml exec -it comment-agent claude          # then /login -> option 1, paste code
docker compose -f compose.agent.yml exec -it comment-agent codex login     # device code
```

After step 3, the daemon **auto-enrolls** your account's handles (zero-click):
your Botlets start answering `@mentions` from inside the container, and your
plain-agent profiles appear on the host under `$COMMENT_AGENTS_DIR`. After step 4,
the Botlet runtimes can actually run (they need Claude/Codex logged in).

### 🚨 Pick the right host

`COMMENT_IO_ENV=staging` targets the **shared** staging `comt.dev`. If your
account lives on a **personal** staging (e.g. `you.comt.dev`, from
`make deploy-staging`), you MUST also set
`COMMENT_IO_STAGING_BASE_URL=https://you.comt.dev`. Otherwise pairing and
enrollment land on the wrong backend — symptoms: `comment daemon health` shows
zero agents/bots, and a bot-install approval says *"Sign in with the account that
owns this bot."* `COMMENT_IO_BASE_URL` does the same for production.

## Using a projected plain agent from the host

The projector writes `$COMMENT_AGENTS_DIR/<handle>.json` (`handle`,
`agent_secret`, `base_url`, `runtime`) for every plain (non-Botlet) handle. A
host-native coding agent uses one by reading that file and sending the
`agent_secret` as a Bearer token to the REST API (per `<base_url>/llms.txt`) — no
host daemon required. To make a host `comment`/Claude Code find them natively,
point `COMMENT_IO_HOME` at the parent dir, or set `COMMENT_AGENTS_DIR` directly to
your host `~/.comment-io/agents`. The projector tracks the files it owns in a
`.comment-agent-projected.manifest` file. The manifest controls pruning and lets
the host CLI tell projected Docker-owned profiles apart from unrelated native
host profiles — profiles with a handle that does not match any container agent
are left alone, but a pre-existing profile with the same handle as a container
agent would be overwritten.

> **Security tradeoff (by design):** projecting plain agents writes their
> `agent_secret` onto the host — the scoped relaxation this split asks for so host
> agents can act as those handles. **Botlet** secrets and all daemon control state
> stay caged in the container. Omit `COMMENT_AGENTS_DIR` to keep *everything*
> container-only (projection then targets an internal volume the host can't see).

## Login spec — in-container subscription login (mode B)

Three credentials live in the box, all isolated from the host:

1. **Comment.io identity** — `comment bus pair` + `comment sync login`
   (device-code flows). Install the daemon credential and enable enrollment/sync.
2. **Claude** — run interactive **`claude`**, then **`/login`** (onboarding is
   pre-seeded, so it lands at the prompt, not the on-launch login screen), and
   choose *"1. Claude account with subscription"*. It tries a localhost browser
   callback, then **falls back to a paste-code** flow (prints a
   `claude.com/cai/oauth/authorize…` URL whose redirect is the hosted
   `platform.claude.com/oauth/code/callback` showing a code). Open the URL on
   your host, approve, paste the code back. It writes
   `~/.claude/.credentials.json`, which the daemon-launched runtimes read.
   **Do not use `claude setup-token`** — it only prints a token for the
   `CLAUDE_CODE_OAUTH_TOKEN` env var, and the daemon strips that var (and
   `ANTHROPIC_API_KEY`) from the runtimes it launches, so it never takes effect.
3. **Codex** — `codex login` (or `codex login --device-auth` for a pure
   code-paste flow). Persists to `~/.codex/auth.json`.

**Why this is the safe default:** all are **paste-code / device-code** flows — no
localhost OAuth callback port to publish from the container. The tokens are
revocable subscription OAuth tokens (no long-lived API key), live in a volume
**separate from your host's own** `~/.claude`/`~/.codex`, and persist across
restarts.

Headless/CI alternative: bake `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` into a
runtime config the CLI reads from disk (env vars alone won't reach the runtime —
see above).

> First-run note for Codex: the very first interactive `codex` run in a workspace
> asks "trust this directory?". The daemon launches Codex via the `comment run
> <handle>` shortcut, which adds `--yolo` (full-auto, for externally-sandboxed
> envs); if you launch `codex` by hand, answer "Yes, continue" once — the trust
> persists in `~/.codex/config.toml`.
>
> First-run note for Claude: a managed Claude Botlet is launched headless as
> `claude --dangerously-skip-permissions`, but that flag does **not** suppress
> Claude's first-run interactive gates: the theme picker (onboarding-gated by
> `hasCompletedOnboarding`), the per-directory "trust this folder" gate, and the
> "Bypass Permissions mode" acknowledgement (whose default is *No, exit*, so a
> nudge would quit the session within ~1s and the daemon would retry forever,
> leaking tmux sockets). The entrypoint pre-accepts all three at boot via
> `seed-runtime-config.cjs` (idempotent, merge-only): it sets
> `hasCompletedOnboarding` + `projects["/"].hasTrustDialogAccepted` in
> `~/.claude.json` (trust walks ancestors, so `/` covers every per-bot brain
> working dir) and `skipDangerousModePermissionPrompt` in
> `~/.claude/settings.json`. Managed Claude sessions then cold-start cleanly.
>
> Because onboarding is pre-completed, the human OAuth login lands at the prompt
> rather than the on-launch login screen: run **`claude`**, then **`/login`**,
> then choose *"1. Claude account with subscription"* and paste the code (step 4
> above). You never see the theme/trust/bypass prompts.

## Running an agent headlessly (no TTY)

`comment run` normally launches the runtime via the daemon (detached tmux) and
then **attaches** an interactive session — which needs a TTY a container lacks.
So use **`--detach`**:

- `comment run <handle> --detach` (or `COMMENT_IO_SKIP_ATTACH=1`) launches via the
  daemon and **skips the attach**. The daemon keeps servicing the session — the
  transient-runtime poller plus Claude's `asyncRewake` Stop hook (Codex falls back
  to a tmux-keystroke nudge) — so it still answers `@mentions` with nobody
  attached.
- Botlets don't need this — the daemon auto-runs them. It's for running a plain
  agent *inside* the container too (the exception to the split). Set
  `COMMENT_AGENT_PROFILE=<handle>` and the entrypoint keeps that one launched
  (idempotent; survives restarts). Off by default.

```bash
docker compose -f compose.agent.yml exec comment-agent comment runtime status --profile <handle>
docker compose -f compose.agent.yml logs -f comment-agent
docker compose -f compose.agent.yml exec -it comment-agent comment run <handle>   # attach a live view (Ctrl-b d to detach)
```

## Host `comment run`

In the `/setup` **Sandboxed + CLI** mode (`--docker --with-cli`), the host CLI
remembers the sandbox container. When no native host daemon is running,
`comment run <handle>` automatically delegates to that container, so the normal
host command works without typing `docker exec ...`.

```bash
comment run <handle>
comment run <handle> --detach
```

The fallback is intentionally narrow: explicit host `--home`, host `--cwd`, and
absolute/relative host `--runtime` paths stay native because those paths usually
do not exist inside the sandbox. If you installed Docker-only without the host
CLI, or you need lower-level runtime inspection, use the explicit container form:

```bash
docker exec -it comment-agent-<origin> comment run <handle>
docker exec comment-agent-<origin> comment runtime status --profile <handle>
```

## Troubleshooting: high CPU on macOS (Colima)

**Symptom.** On macOS you notice the Colima/Lima VM process (Activity Monitor
shows it as "Virtual Machine Service") burning CPU, even while the agent itself
is idle.

**Cause — it's the VM, not the agent.** Colima runs the Linux engine in a VM and,
by default, bind-mounts your entire `$HOME` into it with `mountInotify: true`,
which watches that whole tree so host-side edits stay visible inside the VM
(a virtiofs cache-coherence workaround). The cost scales with how much your home
dir churns — a busy dev tree (many `node_modules`, a test runner writing files)
can keep that watcher hot. It is **not** driven by this container: the compose
file only binds the `/docs` dir, and library sync is read-only and poll-based, so
the sandbox does not depend on `mountInotify` at all.

**Fix.** Turn the watcher off in `~/.colima/default/colima.yaml`, then restart:

```yaml
mountInotify: false
```

```bash
colima restart
```

Containers come back via `restart: unless-stopped`. To keep the watcher but
shrink its scope instead, set `mounts:` to just the directory you bind (the
parent of your `COMMENT_DOCS_DIR`) — note that disables host bind mounts for
paths outside the listed set.

This only applies to **macOS + Colima**. Linux hosts have no VM, and Docker
Desktop uses a different file-sharing mechanism — neither has this knob. The
website `/setup` "Run in a Docker Image" flow is largely unaffected: "Docker
only" runs the agent on named volumes with no host bind mounts, and the
recommended "Sandboxed + CLI" mode (`--docker --with-cli`) additionally
bind-mounts only your host `~/.comment-io/agents` dir (to project
plain-agent profiles to the host) — a single small dir, not your whole tree, so
the `mountInotify` cost above does not apply.

> **`/setup` install modes.** The "Run in a Docker Image" option offers two
> Docker modes that map to this image:
> - **Sandboxed + CLI** (`curl … | bash -s -- --docker --with-cli`, the
>   recommended default): runs this sandbox, installs the host `comment` CLI +
>   editor skills, and bind-mounts your host `~/.comment-io/agents` at
>   `/projected-agents` so plain-agent profiles project to the host (same
>   mechanism + security tradeoff as `COMMENT_AGENTS_DIR` above). It pins the
>   container to the served origin and does **not** pair a host daemon.
> - **Docker only** (`curl … | bash -s -- --docker`): everything stays in the
>   container on named volumes (projection goes to an internal volume); the host
>   stays unaware of comment.io.
>
> **Which image tag?** The default image is origin-aware, mirroring the
> staging-aware CLI npm package. A **production** origin (`comment.io`) pulls the
> canonical, PUBLIC `ghcr.io/comment-hq/comment-agent:latest` (published on a
> production deploy by `comment-agent-publish.yml`). A **staging** origin
> (`comt.dev` or a personal `you.comt.dev`) pulls
> `ghcr.io/comment-hq/comment-agent-staging:latest`, a separate **INTERNAL**
> (org-only) package republished on every push to `main` by
> `comment-agent-staging-publish.yml`. Because it's internal, you must
> `docker login ghcr.io` (a GitHub token with `read:packages` for `comment-hq`)
> before the staging install can pull it. If that pull fails the installer does
> **not** silently use production — it pauses, tells you to log in, and uses the
> production image only if you opt in (interactively, or by setting
> `COMMENT_IO_AGENT_IMAGE=ghcr.io/comment-hq/comment-agent:latest`). Override the
> choice on either origin by exporting `COMMENT_IO_AGENT_IMAGE=<ref>` before
> running the one-liner.
