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
//   2) We FAIL CLOSED: if the Botlets registry can't be trusted, or if it is
//      missing after local Botlets state has existed, we skip the whole
//      projection cycle rather than treat it as "no Botlets".
// If the projection mount is absent, projection is disabled (no-op).

const fs = require('fs');
const path = require('path');

const HOME = process.env.COMMENT_IO_HOME || '/state';
const AGENTS_DIR = path.join(HOME, 'agents');
const PROJECTION_DIR = process.env.COMMENT_AGENTS_PROJECTION_DIR || '/projected-agents';
const BUS_CONFIG_VERSION = 2;
const BOT_NAME_RE = /^[a-z0-9][a-z0-9-]{0,62}$/;
const PROFILE_HANDLE_RE = /^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]\.[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$/;
const SECRET_VALUE_RE =
  /(^|[^A-Za-z0-9])(as_[A-Za-z0-9_-]+|ark_[A-Za-z0-9_.-]+_[A-Za-z0-9_-]+|(cap|clm|ntf|mbx)_[A-Za-z0-9_-]+)($|[^A-Za-z0-9])/;
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

function nonBlank(value) {
  return typeof value === 'string' && value.trim() !== '';
}

function isHomePath(value) {
  return value === '~' || value.startsWith('~/');
}

function isSafePathString(value) {
  return typeof value === 'string' && value !== '' && value.length <= 4096 && !/[\r\n\0]/.test(value) && !SECRET_VALUE_RE.test(value);
}

function isSafeHomeOrAbsolutePath(value) {
  return isSafePathString(value) && (path.isAbsolute(value) || isHomePath(value));
}

function resolveBotletsHomeValue(value) {
  if (!isSafeHomeOrAbsolutePath(value)) throw new Error('invalid botlets home');
  return path.normalize(expandTilde(value));
}

function readPersistedBotletsHome() {
  const cfgPath = path.join(HOME, 'bus', 'config.json');
  let raw;
  try {
    raw = fs.readFileSync(cfgPath, 'utf8');
  } catch (err) {
    if (err && err.code === 'ENOENT') return '';
    throw new Error('bus config is unreadable');
  }
  let cfg;
  try {
    cfg = JSON.parse(raw);
  } catch {
    throw new Error('bus config is malformed');
  }
  if (!cfg || typeof cfg !== 'object' || Array.isArray(cfg) || cfg.version !== BUS_CONFIG_VERSION) {
    throw new Error('bus config has an unsupported version');
  }
  if (cfg.botlets_home == null || cfg.botlets_home === '') return '';
  if (typeof cfg.botlets_home !== 'string') throw new Error('bus config botlets_home is invalid');
  return resolveBotletsHomeValue(cfg.botlets_home);
}

// Resolve the Botlets home exactly as the daemon does (resolveDaemonBotletsHome):
// the daemon-selected home forwarded by the entrypoint (covers an explicit
// `comment bus run --botlets-home /custom` before it's persisted) ->
// persisted bus config (<COMMENT_IO_HOME>/bus/config.json `botlets_home`) ->
// BOTLETS_HOME env -> default <agent-home>/botlets, expanding ~ at each step.
// Matching the daemon is essential: a wrong home yields an empty exclusion set
// and would project Botlet secrets as if they were plain agents.
function resolveBotletsHome() {
  if (nonBlank(process.env.COMMENT_PROJECTOR_BOTLETS_HOME)) {
    return resolveBotletsHomeValue(process.env.COMMENT_PROJECTOR_BOTLETS_HOME);
  }
  const persisted = readPersistedBotletsHome();
  if (persisted) return persisted;
  if (nonBlank(process.env.BOTLETS_HOME)) {
    return resolveBotletsHomeValue(process.env.BOTLETS_HOME);
  }
  return resolveBotletsHomeValue(path.join(process.env.HOME || '/home/agent', 'botlets'));
}

function addHandle(set, value) {
  if (typeof value !== 'string') return;
  const trimmed = value.trim();
  if (trimmed) set.add(trimmed);
}

function addRegistryHandle(set, value) {
  if (typeof value !== 'string') return false;
  const trimmed = value.trim();
  if (!PROFILE_HANDLE_RE.test(trimmed)) return false;
  set.add(trimmed);
  return true;
}

function handleFromCredentialProfile(value) {
  if (!isSafeRegistryCredentialPath(value)) return '';
  const file = path.basename(value);
  if (!file.endsWith('.json')) return '';
  const handle = file.slice(0, -'.json'.length);
  if (!PROFILE_HANDLE_RE.test(handle)) return '';
  return handle;
}

function isSafeRegistryCredentialPath(value) {
  if (typeof value !== 'string') return false;
  if (!value || value.length > 4096 || /[\r\n\0]/.test(value) || value.includes('://') || SECRET_VALUE_RE.test(value)) {
    return false;
  }
  if (path.isAbsolute(value) || value === '~' || value.startsWith('~/')) return true;
  const cleaned = path.normalize(value);
  return cleaned !== '.' && cleaned !== '..' && !cleaned.startsWith(`..${path.sep}`) && !path.isAbsolute(cleaned);
}

function botletProfileHints(options = {}) {
  const handles = new Set();
  let found = false;
  const listed = readAgentProfileFiles(options);
  if (!listed.ok) {
    if (listed.missing && options.allowMissingAgentsDir) return { handles, found, unsafe: false };
    return { handles, found, unsafe: true };
  }
  for (const file of listed.files) {
    const read = readAgentProfileJSON(file);
    if (read.unsafe) return { handles, found, unsafe: true };
    const prof = read.profile;
    if (!prof) {
      continue;
    }
    if (!prof || typeof prof !== 'object') continue;
    const botletish =
      prof.profile_kind === 'alias' ||
      typeof prof.bot_id === 'string' ||
      typeof prof.bot_agent_id === 'string' ||
      prof.disabled_for_polling === true;
    if (!botletish) continue;
    found = true;
    addHandle(handles, prof.alias_of);
    addHandle(handles, prof.handle);
    addHandle(handles, file.replace(/\.json$/, ''));
  }
  return { handles, found, unsafe: false };
}

function journalBotletHints() {
  const handles = new Set();
  let unsafe = false;
  let raw;
  try {
    raw = fs.readFileSync(path.join(HOME, 'bus', 'enroll-journal.json'), 'utf8');
  } catch (err) {
    if (err && err.code === 'ENOENT') return { handles, unsafe: false };
    return { handles, unsafe: true }; // unreadable journal means we cannot safely prove no Botlets.
  }
  let journal;
  try {
    journal = JSON.parse(raw);
  } catch {
    return { handles, unsafe: true };
  }
  if (!journal || typeof journal !== 'object' || Array.isArray(journal)) return { handles, unsafe: true };
  for (const entry of Object.values(journal)) {
    if (!entry || typeof entry !== 'object') continue;
    if (!entry.botlets_handle && !entry.botlets_home) continue;
    const before = handles.size;
    addHandle(handles, entry.botlets_handle);
    addHandle(handles, entry.handle);
    if (handles.size === before) unsafe = true;
  }
  return { handles, unsafe };
}

function teamRuntimeBotletHints(botletsHome) {
  const handles = new Set();
  let raw;
  try {
    raw = fs.readFileSync(path.join(botletsHome, 'team-runtime.json'), 'utf8');
  } catch (err) {
    if (err && err.code === 'ENOENT') return { handles, unsafe: false };
    return { handles, unsafe: true };
  }
  let cfg;
  try {
    cfg = JSON.parse(raw);
  } catch {
    return { handles, unsafe: true };
  }
  if (!cfg || typeof cfg !== 'object' || Array.isArray(cfg)) return { handles, unsafe: true };
  if (!Object.prototype.hasOwnProperty.call(cfg, 'last_manifest_agents')) return { handles, unsafe: true };
  if (!Array.isArray(cfg.last_manifest_agents)) return { handles, unsafe: true };
  for (const handle of cfg.last_manifest_agents) addHandle(handles, handle);
  return { handles, unsafe: false };
}

function missingRegistryUnsafe(botletsHome, options = {}) {
  const profiles = botletProfileHints(options);
  if (profiles.unsafe) return true;
  if (profiles.found) return true;
  const journal = journalBotletHints();
  if (journal.unsafe || journal.handles.size > 0) return true;
  const team = teamRuntimeBotletHints(botletsHome);
  return team.unsafe || team.handles.size > 0;
}

function registryPath(botletsHome) {
  return path.join(botletsHome, 'registry.json');
}

function ownedByCurrentUser(info) {
  if (typeof process.getuid !== 'function' || typeof info.uid !== 'number') return true;
  return info.uid === process.getuid();
}

function ownedByRootOrCurrentUser(info) {
  if (typeof process.getuid !== 'function' || typeof info.uid !== 'number') return true;
  return info.uid === 0 || info.uid === process.getuid();
}

function normalizeTrustedBotletsParentPath(value) {
  const clean = path.normalize(value);
  if (process.platform === 'darwin' && (clean === '/var' || clean.startsWith('/var/'))) {
    return path.normalize('/private/var' + clean.slice('/var'.length));
  }
  return clean;
}

function allowedGroupWritableHomebrewDir(value, allowBin) {
  const clean = path.normalize(value);
  if (clean === '/opt/homebrew/Cellar') return true;
  if (process.platform === 'darwin' && clean === '/opt/homebrew/lib') return true;
  if (allowBin && clean === '/opt/homebrew/bin') return true;
  if (clean === '/usr/local/Cellar') return true;
  if (process.platform === 'darwin' && clean === '/usr/local/lib') return true;
  if (allowBin && clean === '/usr/local/bin') return true;
  return false;
}

function unsafeTrustedDirMode(value, info, allowHomebrewBin, allowHomebrew) {
  if ((info.mode & 0o002) !== 0) return (info.mode & 0o1000) === 0;
  if ((info.mode & 0o020) !== 0) return !allowHomebrew || !allowedGroupWritableHomebrewDir(value, allowHomebrewBin);
  return false;
}

function trustedSearchPathDir(value) {
  if (!isSafePathString(value) || !path.isAbsolute(value)) return false;
  let dir = path.normalize(value);
  for (;;) {
    let info;
    try {
      info = fs.lstatSync(dir);
    } catch {
      return false;
    }
    if (info.isSymbolicLink() || !info.isDirectory() || !ownedByRootOrCurrentUser(info)) return false;
    if (unsafeTrustedDirMode(dir, info, true, true)) return false;
    const parent = path.dirname(dir);
    if (parent === dir) return true;
    dir = parent;
  }
}

function trustedBotletsHomeParent(botletsHome) {
  return trustedSearchPathDir(normalizeTrustedBotletsParentPath(path.dirname(botletsHome)));
}

function trustedPrivateDir(dir, options = {}) {
  let info;
  try {
    info = fs.lstatSync(dir);
  } catch (err) {
    return Boolean(options.allowMissing && err && err.code === 'ENOENT');
  }
  return !info.isSymbolicLink() && info.isDirectory() && ownedByCurrentUser(info) && (info.mode & 0o022) === 0;
}

function trustedAgentProfileFile(file) {
  let info;
  try {
    info = fs.lstatSync(file);
  } catch {
    return false;
  }
  return !info.isSymbolicLink() && info.isFile() && ownedByCurrentUser(info) && (info.mode & 0o077) === 0;
}

function trustedRegistryFile(file, options = {}) {
  let info;
  try {
    info = fs.lstatSync(file);
  } catch (err) {
    return Boolean(options.allowMissing && err && err.code === 'ENOENT');
  }
  return !info.isSymbolicLink() && info.isFile() && ownedByCurrentUser(info) && (info.mode & 0o022) === 0;
}

function fsyncDirBestEffort(dir) {
  let fd;
  try {
    fd = fs.openSync(dir, fs.constants.O_RDONLY);
    fs.fsyncSync(fd);
  } catch {
    /* best effort */
  } finally {
    if (fd !== undefined) {
      try {
        fs.closeSync(fd);
      } catch {
        /* best effort */
      }
    }
  }
}

function writeFileNoReplaceAtomic(dest, payload, mode) {
  const dir = path.dirname(dest);
  const base = path.basename(dest);
  const suffix = `${process.pid}.${Date.now()}.${Math.random().toString(16).slice(2)}`;
  const tmp = path.join(dir, `.${base}.${suffix}.tmp`);
  let fd;
  let publishing = false;
  try {
    fd = fs.openSync(tmp, fs.constants.O_CREAT | fs.constants.O_EXCL | fs.constants.O_WRONLY, mode);
    fs.fchmodSync(fd, mode);
    fs.writeFileSync(fd, payload);
    fs.fsyncSync(fd);
    fs.closeSync(fd);
    fd = undefined;
    publishing = true;
    fs.linkSync(tmp, dest); // no-replace: fails with EEXIST if a real writer won the race
    fsyncDirBestEffort(dir);
    return true;
  } catch (err) {
    if (publishing && err && err.code === 'EEXIST') return true;
    return false;
  } finally {
    if (fd !== undefined) {
      try {
        fs.closeSync(fd);
      } catch {
        /* best effort */
      }
    }
    try {
      fs.unlinkSync(tmp);
    } catch {
      /* best effort */
    }
  }
}

function ensureFreshEmptyRegistry(botletsHome, options = {}) {
  const regPath = registryPath(botletsHome);
  if (!trustedBotletsHomeParent(botletsHome)) return false;
  if (!trustedPrivateDir(botletsHome, { allowMissing: true })) return false;
  if (!trustedRegistryFile(regPath, { allowMissing: true })) return false;
  try {
    fs.accessSync(regPath, fs.constants.R_OK);
    return true;
  } catch (err) {
    if (!err || err.code !== 'ENOENT') return false;
  }
  if (missingRegistryUnsafe(botletsHome, options)) return false;
  try {
    fs.mkdirSync(botletsHome, { recursive: true, mode: 0o700 });
    return writeFileNoReplaceAtomic(regPath, JSON.stringify({ bots: [] }, null, 2) + '\n', 0o600);
  } catch {
    return false;
  }
}

// Returns { handles, ok }. ok === false means we could NOT determine the Botlet
// set → callers must fail closed. A genuinely fresh sandbox initializes a valid
// empty registry before profiles exist; once usable profiles exist, a missing
// registry is unsafe because canonical Botlet credentials look like plain-agent
// profiles without registry/journal/team-runtime context.
function botletHandles(botletsHome, options = {}) {
  const regPath = registryPath(botletsHome);
  if (!trustedBotletsHomeParent(botletsHome)) return { handles: null, ok: false };
  if (!trustedPrivateDir(botletsHome)) return { handles: null, ok: false };
  if (!trustedRegistryFile(regPath, { allowMissing: true })) return { handles: null, ok: false };
  let raw;
  try {
    raw = fs.readFileSync(regPath, 'utf8');
  } catch (e) {
    if (e && e.code === 'ENOENT') {
      if (missingRegistryUnsafe(botletsHome)) return { handles: null, ok: false, missing: true };
      if (!options.allowMissingRegistry) return { handles: null, ok: false, missing: true };
      return { handles: new Set(), ok: true, missing: true };
    }
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
    if (!b || typeof b !== 'object' || Array.isArray(b)) return { handles: null, ok: false };
    if (typeof b.name !== 'string' || !BOT_NAME_RE.test(b.name)) return { handles: null, ok: false };
    if (!addRegistryHandle(set, b.handle)) return { handles: null, ok: false };
    if (b.handle_aliases != null) {
      if (!Array.isArray(b.handle_aliases)) return { handles: null, ok: false };
      for (const a of b.handle_aliases) {
        if (!addRegistryHandle(set, a)) return { handles: null, ok: false };
      }
    }
    const credentialHandle = handleFromCredentialProfile(b.credential_profile);
    if (!credentialHandle) return { handles: null, ok: false };
    set.add(credentialHandle);
  }
  const profiles = botletProfileHints();
  if (profiles.unsafe) return { handles: null, ok: false };
  for (const handle of profiles.handles) set.add(handle);
  const journal = journalBotletHints();
  if (journal.unsafe) return { handles: null, ok: false };
  for (const handle of journal.handles) set.add(handle);
  const team = teamRuntimeBotletHints(botletsHome);
  if (team.unsafe) return { handles: null, ok: false };
  for (const handle of team.handles) set.add(handle);
  return { handles: set, ok: true };
}

function readJsonFiles(dir) {
  try {
    return { files: fs.readdirSync(dir).filter((f) => f.endsWith('.json') && !f.endsWith('.tmp')), ok: true };
  } catch (err) {
    return { files: [], ok: false, missing: err && err.code === 'ENOENT' };
  }
}

function directoryStatus(dir) {
  try {
    const info = fs.lstatSync(dir);
    const trusted = !info.isSymbolicLink() && info.isDirectory() && ownedByCurrentUser(info) && (info.mode & 0o022) === 0;
    return { ok: trusted, missing: false, unsafe: !trusted };
  } catch (err) {
    if (err && err.code === 'ENOENT') return { ok: false, missing: true, unsafe: false };
    return { ok: false, missing: false, unsafe: true };
  }
}

function readAgentProfileFiles(options = {}) {
  const dir = directoryStatus(AGENTS_DIR);
  if (!dir.ok) {
    if (dir.missing && options.allowMissingAgentsDir) return { files: [], ok: true, missing: true };
    return { files: [], ok: false, missing: dir.missing, unsafe: true };
  }
  const listed = readJsonFiles(AGENTS_DIR);
  if (!listed.ok) return listed;
  for (const file of listed.files) {
    if (!trustedAgentProfileFile(path.join(AGENTS_DIR, file))) {
      return { files: [], ok: false, unsafe: true };
    }
  }
  return listed;
}

function readAgentProfileJSON(file) {
  const profilePath = path.join(AGENTS_DIR, file);
  if (!trustedAgentProfileFile(profilePath)) return { unsafe: true };
  try {
    return { profile: JSON.parse(fs.readFileSync(profilePath, 'utf8')) };
  } catch (err) {
    if (err && err.code === 'ENOENT') return {};
    if (err instanceof SyntaxError) return {};
    return { unsafe: true };
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

  let botletsHome;
  try {
    botletsHome = resolveBotletsHome();
  } catch (err) {
    return { failClosed: true, botletsHome: '<unresolved>', reason: err && err.message ? err.message : 'Botlets home is invalid' };
  }
  const priorManifest = loadManifest();
  const agentsDir = directoryStatus(AGENTS_DIR);
  if (!agentsDir.ok) {
    if (agentsDir.missing && priorManifest.size === 0 && !ensureFreshEmptyRegistry(botletsHome, { allowMissingAgentsDir: true })) {
      return { failClosed: true, botletsHome, reason: `Botlets registry under ${botletsHome} is unavailable or unsafe` };
    }
    if (agentsDir.unsafe || priorManifest.size > 0) {
      return { failClosed: true, botletsHome, reason: `agent profiles directory ${AGENTS_DIR} is unavailable or unsafe` };
    }
    return { projected: [], pruned: [], disabled: false };
  }
  const agentFiles = readAgentProfileFiles();
  if (!agentFiles.ok) {
    if (!agentFiles.missing || priorManifest.size > 0) {
      return { failClosed: true, botletsHome, reason: `agent profiles directory ${AGENTS_DIR} is unavailable or unsafe` };
    }
    return { projected: [], pruned: [], disabled: false };
  }
  if (agentFiles.files.length === 0 && priorManifest.size > 0) {
    return { failClosed: true, botletsHome, reason: `agent profiles directory ${AGENTS_DIR} is empty while prior projections exist` };
  }
  if (agentFiles.files.length === 0 && !ensureFreshEmptyRegistry(botletsHome)) {
    return { failClosed: true, botletsHome, reason: `Botlets registry under ${botletsHome} is unavailable or unsafe` };
  }
  // Early fail-closed if the registry can't be read at all.
  if (!botletHandles(botletsHome).ok) return { failClosed: true, botletsHome };

  const wanted = new Set();
  const projected = [];
  for (const file of agentFiles.files) {
    const read = readAgentProfileJSON(file);
    if (read.unsafe) {
      return { failClosed: true, botletsHome, reason: `agent profile ${file} is unavailable or unsafe` };
    }
    const prof = read.profile;
    if (!prof) {
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
  for (const file of priorManifest) {
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

function main() {
  const r = projectOnce();
  if (r.failClosed) {
    const reason = r.reason || `Botlets registry under ${r.botletsHome} is unavailable or unsafe`;
    console.error(
      `project-agents: ${reason} — skipping projection (fail closed, no plain agents projected this cycle)`,
    );
  } else if (!r.disabled && (r.projected.length || r.pruned.length)) {
    const parts = [];
    if (r.projected.length) parts.push(`projected ${r.projected.join(', ')}`);
    if (r.pruned.length) parts.push(`pruned ${r.pruned.join(', ')}`);
    console.log(`project-agents: ${parts.join('; ')}`);
  }
}

if (require.main === module) {
  main();
} else {
  module.exports = {
    ensureFreshEmptyRegistry,
    projectOnce,
    writeFileNoReplaceAtomic,
  };
}
