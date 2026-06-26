#!/usr/bin/env node
//
// Seed the Claude Code runtime config so a Botlet's managed session can
// COLD-START headlessly without hitting an interactive onboarding gate.
//
// Why this exists: the daemon launches a managed Claude Botlet detached, in a
// tmux pane with no human at the keyboard, as `claude --agent <bot>
// --dangerously-skip-permissions`. That flag does NOT suppress Claude's
// first-run interactive gates. On a fresh home Claude stops at, in order:
//   1. theme picker        — part of the onboarding flow, gated by
//                            `hasCompletedOnboarding` in ~/.claude.json (NOT by
//                            the `theme` value in settings.json).
//   2. "trust this folder" — per-DIRECTORY gate; `checkHasTrustDialogAccepted`
//                            walks the cwd and its ANCESTORS for a
//                            `projects[dir].hasTrustDialogAccepted` entry.
//   3. "Bypass Permissions mode — Yes, I accept" — suppressed by
//                            `skipDangerousModePermissionPrompt` in settings.json.
// Gate 3 is the killer: its DEFAULT option is "No, exit", so the daemon's nudge
// keystrokes land on it and Claude quits within ~1s; the daemon then retries
// forever, leaking one tmux socket per attempt.
//
// Gate 2 cannot be seeded per-bot from here: the daemon launches each managed
// session in the bot's BRAIN PROJECTION ROOT (managedSessionWorkingDir in
// internal/commentbus/daemon.go → <sync-root>/<brain-rel-path>), which lives
// under the configured sync root (e.g. /docs or ~/"Comment Docs (staging)") and
// is not known at entrypoint time. Because trust walks ANCESTORS, we accept
// trust once at "/" — the ancestor of every possible launch dir — which is
// appropriate in this explicitly-sandboxed container that already runs in
// bypass-permissions mode. (Verified: with these keys, Claude cold-starts in a
// deep, never-seen directory straight to its main prompt — no theme, trust, or
// bypass gate.)
//
// The seed runs at RUNTIME from the entrypoint, NOT at image build, because
// /home/agent is a named volume that masks anything baked into the image there.
// It is idempotent, merge-only (never clobbers an existing theme, credentials,
// or unrelated project/settings state), writes atomically, and leaves a corrupt
// config file untouched. Codex is unaffected (its managed launch uses --yolo).

const fs = require('fs');
const path = require('path');
const os = require('os');

const HOME = process.env.HOME || os.homedir() || '/home/agent';
const SETTINGS_PATH = path.join(HOME, '.claude', 'settings.json');
const CLAUDE_JSON_PATH = path.join(HOME, '.claude.json');

function readJson(file) {
  // { ok, value, existed } — ok:false means "present but unparseable" → leave
  // it alone. A genuinely-absent file is ok:true, value:{}, existed:false.
  let raw;
  try {
    raw = fs.readFileSync(file, 'utf8');
  } catch (err) {
    if (err && err.code === 'ENOENT') return { ok: true, value: {}, existed: false };
    return { ok: false, value: null, existed: true, error: err };
  }
  if (raw.trim() === '') return { ok: true, value: {}, existed: true };
  try {
    const value = JSON.parse(raw);
    if (value && typeof value === 'object' && !Array.isArray(value)) return { ok: true, value, existed: true };
    return { ok: false, value: null, existed: true, error: new Error('not a JSON object') };
  } catch (err) {
    return { ok: false, value: null, existed: true, error: err };
  }
}

function writeJsonAtomic(file, value, existed) {
  // Create ~/.claude owner-only to match the 0600 file posture (it normally
  // pre-exists from the OAuth login; mode is only applied when we create it).
  fs.mkdirSync(path.dirname(file), { recursive: true, mode: 0o700 });
  // Preserve an existing file's mode; default owner-only for new files. Never
  // widen Claude's config tree.
  let mode = 0o600;
  if (existed) {
    try { mode = fs.statSync(file).mode & 0o777; } catch { /* keep default */ }
  }
  const tmp = `${file}.seed-tmp-${process.pid}`;
  fs.writeFileSync(tmp, JSON.stringify(value, null, 2), { mode });
  fs.chmodSync(tmp, mode); // deterministic final mode regardless of umask
  fs.renameSync(tmp, file); // atomic; symlink-safe (replaces, never writes through)
}

const changed = [];
const warnings = [];

// 1) ~/.claude/settings.json — suppress the bypass-mode accept prompt (gate 3).
{
  const r = readJson(SETTINGS_PATH);
  if (!r.ok) {
    warnings.push(`settings.json unparseable — left untouched (${r.error && r.error.message})`);
  } else {
    const s = r.value;
    let dirty = false;
    if (s.skipDangerousModePermissionPrompt !== true) { s.skipDangerousModePermissionPrompt = true; dirty = true; }
    s.permissions = (s.permissions && typeof s.permissions === 'object') ? s.permissions : {};
    if (s.permissions.defaultMode === undefined) { s.permissions.defaultMode = 'bypassPermissions'; dirty = true; }
    if (s.theme === undefined) { s.theme = 'auto'; dirty = true; } // preference only; gate 1 is suppressed below
    if (dirty) { writeJsonAtomic(SETTINGS_PATH, s, r.existed); changed.push('settings.json (bypass-mode)'); }
  }
}

// 2) ~/.claude.json — complete onboarding (gate 1: theme picker) + trust every
//    directory via the "/" ancestor (gate 2: folder trust).
{
  const r = readJson(CLAUDE_JSON_PATH);
  if (!r.ok) {
    warnings.push(`.claude.json unparseable — left untouched (${r.error && r.error.message})`);
  } else {
    const j = r.value;
    let dirty = false;
    if (j.hasCompletedOnboarding !== true) { j.hasCompletedOnboarding = true; dirty = true; }
    j.projects = (j.projects && typeof j.projects === 'object') ? j.projects : {};
    const root = (j.projects['/'] && typeof j.projects['/'] === 'object') ? j.projects['/'] : {};
    if (root.hasTrustDialogAccepted !== true) { root.hasTrustDialogAccepted = true; j.projects['/'] = root; dirty = true; }
    if (dirty) { writeJsonAtomic(CLAUDE_JSON_PATH, j, r.existed); changed.push('.claude.json (onboarding + folder-trust)'); }
  }
}

// Mirror project-agents.js: log only when something happened; stay silent on an
// idempotent no-op boot.
if (warnings.length) console.error(`seed-runtime-config: ${warnings.join('; ')}`);
if (changed.length) console.log(`seed-runtime-config: seeded ${changed.join(', ')}`);
