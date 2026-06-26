#!/usr/bin/env node
// Agent-sandbox credential projector.
//
// The single daemon in this container enrolls EVERYTHING the account owns
// (the server installs plain agents on every daemon, and Botlets on exactly
// one — this one). We keep that split clean on the host:
//
//   - Botlets stay container-only. Their managed tmux sessions and credentials
//     never leave the box.
//   - Plain (non-Botlet) agent handles are PROJECTED to a host folder so a
//     host-native coding agent (e.g. a Claude Code session on the host) can grab
//     a handle and post as it — plain agents need only their profile file
//     (handle + agent_secret + base_url) and the REST API, no daemon.
//
// This mirrors <COMMENT_IO_HOME>/agents/<handle>.json ->
// $COMMENT_AGENTS_PROJECTION_DIR for every handle that is NOT a Botlet, and
// prunes projected files that are no longer plain agents. It is idempotent and
// safe to run on a loop.
//
// SECURITY: this deliberately writes plain-agent agent_secret files onto a host
// bind mount — the scoped trust relaxation the split asks for. Botlet secrets and
// all daemon control state stay inside the container. Two safeguards keep Botlet
// secrets from ever leaking:
//   1) We resolve the SAME Botlets home the daemon uses, so the exclusion set is
//      correct even with a non-default home.
//   2) We FAIL CLOSED: if the Botlets registry exists but can't be read/parsed,
//      we skip the whole projection cycle rather than treat it as "no Botlets".
// If the projection mount is absent, projection is disabled (no-op).

const fs = require('fs');
const path = require('path');

const HOME = process.env.COMMENT_IO_HOME || '/state';
const AGENTS_DIR = path.join(HOME, 'agents');
const PROJECTION_DIR = process.env.COMMENT_AGENTS_PROJECTION_DIR || '/projected-agents';
// Manifest of files THIS projector owns, so we only ever prune our own copies and
// never touch unrelated agent profiles already in the projection dir (it may be a
// shared host agents dir, e.g. ~/.comment-io/agents). Deliberately NOT a *.json
// name so the host's agent-profile loader (which scans `*.json`) ignores it.
const MANIFEST = path.join(PROJECTION_DIR, '.comment-agent-projected.manifest');

// Expand a leading ~ like the daemon's ExpandHome, so a `~/botlets` value (which
// the daemon accepts) isn't read as a literal path that ENOENTs and silently
// disables the exclusion set.
function expandTilde(p) {
  if (!p) return p;
  const home = process.env.HOME || '/home/agent';
  if (p === '~') return home;
  if (p.startsWith('~/')) return path.join(home, p.slice(2));
  return p;
}

// Resolve the Botlets home exactly as the daemon does (resolveDaemonBotletsHome):
// the daemon-selected home forwarded by the entrypoint (covers an explicit
// `comment bus run --botlets-home /custom` before it's persisted) ->
// persisted bus config (<COMMENT_IO_HOME>/bus/config.json `botlets_home`) ->
// BOTLETS_HOME env -> default <agent-home>/botlets, expanding ~ at each step.
// Matching the daemon is essential: a wrong home yields an empty exclusion set
// and would project Botlet secrets as if they were plain agents.
function resolveBotletsHome() {
  if (process.env.COMMENT_PROJECTOR_BOTLETS_HOME && process.env.COMMENT_PROJECTOR_BOTLETS_HOME.trim()) {
    return expandTilde(process.env.COMMENT_PROJECTOR_BOTLETS_HOME.trim());
  }
  try {
    const cfg = JSON.parse(fs.readFileSync(path.join(HOME, 'bus', 'config.json'), 'utf8'));
    if (cfg && typeof cfg.botlets_home === 'string' && cfg.botlets_home.trim()) return expandTilde(cfg.botlets_home.trim());
  } catch {
    /* no/invalid bus config → fall through to env/default */
  }
  if (process.env.BOTLETS_HOME && process.env.BOTLETS_HOME.trim()) return expandTilde(process.env.BOTLETS_HOME.trim());
  return path.join(process.env.HOME || '/home/agent', 'botlets');
}

// Returns { handles, ok }. ok === false means we could NOT determine the Botlet
// set (registry present but unreadable/corrupt) → callers must fail closed.
// A genuinely absent registry (ENOENT) is ok with an empty set: no Botlets
// enrolled, safe to project plain agents.
function botletHandles(botletsHome) {
  const regPath = path.join(botletsHome, 'registry.json');
  let raw;
  try {
    raw = fs.readFileSync(regPath, 'utf8');
  } catch (e) {
    if (e && e.code === 'ENOENT') return { handles: new Set(), ok: true };
    return { handles: null, ok: false }; // exists but unreadable → fail closed
  }
  let reg;
  try {
    reg = JSON.parse(raw);
  } catch {
    return { handles: null, ok: false }; // corrupt → fail closed
  }
  // Mirror the daemon: `bots` MUST be an array (profiles.go treats a nil/missing
  // `bots` as INVALID_BOTLETS_REGISTRY). A valid empty registry is `{"bots":[]}`.
  // Anything else (e.g. `{}` or `{"bots":null}`) → fail closed, so we never treat
  // a malformed registry as "no botlets" and project Botlet credentials.
  if (!reg || !Array.isArray(reg.bots)) return { handles: null, ok: false };
  const set = new Set();
  for (const b of reg.bots) {
    if (!b) continue;
    if (b.handle) set.add(b.handle);
    if (Array.isArray(b.handle_aliases)) for (const a of b.handle_aliases) set.add(a);
  }
  return { handles: set, ok: true };
}

function listJson(dir) {
  try {
    return fs.readdirSync(dir).filter((f) => f.endsWith('.json') && !f.endsWith('.tmp'));
  } catch {
    return [];
  }
}

// loadManifest returns the set of projection filenames this projector created on
// a prior cycle. Only these are eligible for pruning.
function loadManifest() {
  try {
    const m = JSON.parse(fs.readFileSync(MANIFEST, 'utf8'));
    return new Set(Array.isArray(m.files) ? m.files : []);
  } catch {
    return new Set();
  }
}

function saveManifest(files) {
  const tmp = MANIFEST + '.tmp';
  fs.writeFileSync(tmp, JSON.stringify({ files: [...files].sort() }, null, 2) + '\n', { mode: 0o600 });
  fs.renameSync(tmp, MANIFEST);
}

function projectOnce() {
  // Projection mount absent → feature disabled.
  if (!fs.existsSync(PROJECTION_DIR)) return { disabled: true };

  const botletsHome = resolveBotletsHome();
  // Early fail-closed if the registry can't be read at all.
  if (!botletHandles(botletsHome).ok) return { failClosed: true, botletsHome };

  const wanted = new Set();
  const projected = [];
  for (const file of listJson(AGENTS_DIR)) {
    let prof;
    try {
      prof = JSON.parse(fs.readFileSync(path.join(AGENTS_DIR, file), 'utf8'));
    } catch {
      continue;
    }
    const handle = prof.handle;
    if (!handle || !prof.agent_secret) continue; // not a usable profile

    // Re-read the Botlets set just-in-time, per file. A Botlet enrolling mid-cycle
    // writes its profile and registry entry non-atomically; checking the freshest
    // registry right before we decide on THIS handle closes the window where a new
    // Botlet's profile exists but its registry entry hasn't been read yet. If the
    // registry goes unreadable mid-cycle, fail closed (stop, don't prune).
    const fresh = botletHandles(botletsHome);
    if (!fresh.ok) {
      // Registry went unreadable mid-cycle. Persist ownership of everything we've
      // projected so far (merged with the prior manifest) before bailing, so a
      // file written earlier this cycle is never orphaned — it stays prunable on a
      // later cycle if its handle turns out to be a Botlet.
      try {
        saveManifest(new Set([...loadManifest(), ...wanted]));
      } catch {
        /* best effort */
      }
      return { failClosed: true, botletsHome };
    }
    if (fresh.handles.has(handle)) continue; // Botlet → container-only, never project

    wanted.add(file);
    const payload =
      JSON.stringify(
        { handle: prof.handle, agent_secret: prof.agent_secret, base_url: prof.base_url, runtime: prof.runtime || '' },
        null,
        2,
      ) + '\n';
    const dest = path.join(PROJECTION_DIR, file);
    try {
      if (fs.readFileSync(dest, 'utf8') === payload) {
        // Unchanged content, but enforce private perms — a pre-existing copy
        // (restored/older) could be group/world-readable, exposing the secret
        // and making the host CLI reject the profile as not private.
        try {
          fs.chmodSync(dest, 0o600);
        } catch {
          /* best effort */
        }
        continue;
      }
    } catch {
      /* dest missing → write it */
    }
    const tmp = dest + '.tmp';
    fs.writeFileSync(tmp, payload, { mode: 0o600 });
    fs.renameSync(tmp, dest);
    try {
      fs.chmodSync(dest, 0o600); // guarantee 0600 regardless of umask
    } catch {
      /* best effort */
    }
    projected.push(handle);
  }

  // Prune ONLY files this projector created on a prior cycle that are no longer
  // plain agents (deleted, or became Botlets). Files we don't own — e.g. the
  // host's own agent profiles when COMMENT_AGENTS_DIR is a shared agents dir — are
  // never touched.
  const pruned = [];
  for (const file of loadManifest()) {
    if (!wanted.has(file)) {
      try {
        fs.unlinkSync(path.join(PROJECTION_DIR, file));
      } catch {
        /* already gone / not removable */
      }
      pruned.push(file.replace(/\.json$/, ''));
    }
  }
  saveManifest(wanted);
  return { projected, pruned, disabled: false };
}

const r = projectOnce();
if (r.failClosed) {
  console.error(
    `project-agents: Botlets registry under ${r.botletsHome} is unreadable — skipping projection (fail closed, no plain agents projected this cycle)`,
  );
} else if (!r.disabled && (r.projected.length || r.pruned.length)) {
  const parts = [];
  if (r.projected.length) parts.push(`projected ${r.projected.join(', ')}`);
  if (r.pruned.length) parts.push(`pruned ${r.pruned.join(', ')}`);
  console.log(`project-agents: ${parts.join('; ')}`);
}
