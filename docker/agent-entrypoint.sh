#!/usr/bin/env bash
#
# Agent-sandbox entrypoint. Runs the full `comment` daemon, which enrolls the
# account's handles and auto-runs Botlets as managed tmux sessions IN this
# container. Two background helpers run alongside it (only under `bus run`):
#
#   - project_loop: mirrors PLAIN (non-Botlet) agent profiles to a host bind
#     mount ($COMMENT_AGENTS_PROJECTION_DIR) so host-native coding agents can grab
#     and use those handles. Botlet credentials stay container-only. No-op if the
#     projection mount is absent. See docker/project-agents.js.
#   - autostart_loop (OPTIONAL): if COMMENT_AGENT_PROFILE is set, keeps that one
#     handle's runtime launched headless IN the container. Off by default — in the
#     clean split, plain agents run on the HOST and Botlets auto-run via the
#     daemon, so the container usually needs neither.
#
# Onboarding (pair, claude/codex login, sync login) is run via `docker compose
# exec`, which bypasses this entrypoint and talks to the running daemon.
#
# Pass-through: any non-`bus run` argv is exec'd straight through to `comment`
# (so `docker run <img> version`, `... bus pair`, `... run --detach`, etc. all
# work and never trigger the background loops).
set -euo pipefail

COMMENT_BIN=/usr/local/bin/comment
PROJECT_AGENTS_JS=/usr/local/bin/project-agents.js
SEED_RUNTIME_CONFIG_JS=/usr/local/bin/seed-runtime-config.cjs

# seed_runtime_config pre-accepts Claude Code's first-run interactive gates
# (theme/onboarding, folder-trust, and the "Bypass Permissions mode"
# acknowledgement) so a Botlet's managed Claude session can cold-start headlessly.
# Without it the daemon-launched `claude --dangerously-skip-permissions` pane
# stops at the bypass-mode prompt — whose default option is "No, exit" — and the
# daemon's nudge quits it within ~1s, retrying forever and leaking tmux sockets.
# Idempotent and merge-only; see docker/seed-runtime-config.cjs. Codex needs no
# seed (--yolo).
seed_runtime_config() {
  node "$SEED_RUNTIME_CONFIG_JS" || true
}

# project_loop keeps the host-projected plain-agent profiles in sync with what
# the daemon has enrolled. project-agents.js is idempotent and self-disables when
# the projection mount is absent.
project_loop() {
  while true; do
    node "$PROJECT_AGENTS_JS" || true
    sleep 20
  done
}

# autostart_loop launches (and, if it dies, relaunches) the configured agent
# runtime detached, via the `comment run <profile>` SHORTCUT form so the daemon
# applies the profile's saved runtime + safe-auto flags. Idempotent: success when
# already up, non-zero while the profile isn't ready — so it tolerates the
# one-time human pair/login after first boot and survives restarts.
autostart_loop() {
  [ -n "${COMMENT_AGENT_PROFILE:-}" ] || return 0
  local up=0
  while true; do
    if COMMENT_IO_SKIP_ATTACH=1 "$COMMENT_BIN" run \
        "$COMMENT_AGENT_PROFILE" --detach >/dev/null 2>&1; then
      if [ "$up" -eq 0 ]; then
        echo "agent-entrypoint: @${COMMENT_AGENT_PROFILE} runtime is up (detached)"
        up=1
      fi
    elif [ "$up" -eq 1 ]; then
      echo "agent-entrypoint: @${COMMENT_AGENT_PROFILE} runtime went away; will relaunch" >&2
      up=0
    fi
    sleep 30
  done
}

# Extract an explicit `--botlets-home` from the daemon args so the projector uses
# the SAME Botlets home the daemon will, even before it's persisted to bus config
# (otherwise the projector could read an empty default registry at startup and
# project Botlet credentials).
detect_botlets_home() {
  local prev="" a
  for a in "$@"; do
    case "$a" in
      --botlets-home=*) printf '%s' "${a#*=}"; return 0 ;;
    esac
    if [ "$prev" = "--botlets-home" ]; then printf '%s' "$a"; return 0; fi
    prev="$a"
  done
}

if [ "${1:-}" = "bus" ] && [ "${2:-}" = "run" ]; then
  COMMENT_PROJECTOR_BOTLETS_HOME="$(detect_botlets_home "$@")"
  export COMMENT_PROJECTOR_BOTLETS_HOME
  # Seed Claude config BEFORE the daemon starts so the first Botlet cold-start
  # already finds the onboarding gates pre-accepted.
  seed_runtime_config
  project_loop &
  autostart_loop &
fi

exec "$COMMENT_BIN" "$@"
