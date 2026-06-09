// Node 24 JS action wrapper for distcc-action.
//
// Mirrors how actions/setup-go works: the action ships as bundled JS that runs
// on the runner's built-in Node, and at runtime it fetches the real tool — here,
// our prebuilt static Go binary — from this repo's GitHub Releases, cached via
// @actions/tool-cache. The Go binary carries all the tsnet farm logic and reads
// its parameters from INPUT_* env vars; this wrapper only resolves+fetches it,
// wires INPUT_*, and execs it (propagating the exit code).
//
// Version discovery is dynamic: we read GITHUB_ACTION_REF (the ref the caller
// used in `uses:`) and resolve it to a release tag via the releases API, so
// @v1 -> latest v1.*, @v1.0.0 -> exactly v1.0.0. No VERSION file, no constant.
// When GITHUB_ACTION_REF is empty (`uses: ./` — the repo's own CI testing the
// CURRENT checkout) we build the local source instead of downloading a release,
// so self-test always exercises the committed code.

const fs = require('fs');
const path = require('path');
const { spawn } = require('child_process');

const core = require('@actions/core');
const exec = require('@actions/exec');
const io = require('@actions/io');
const tc = require('@actions/tool-cache');
const github = require('@actions/github');
const semver = require('semver');

const OWNER = 'xdqi';
const REPO = 'sccache-dist-action';
const TOOL = 'sccache-dist-action';

// action.yml input id -> INPUT_* env var the Go binary reads. The Go binary's
// contract is INPUT_<UPPER_SNAKE>; @actions/core would expose these too, but we
// set them explicitly so the exec'd subprocess inherits the exact contract
// regardless of how core normalizes names.
const INPUT_IDS = [
  'mode',
  'oauth-client-id',
  'oauth-secret',
  'expected-workers',
  'min-workers',
  'wait-timeout',
  'worker-index',
  'tags',
  'run-prefix',
  'slots',
  'poll-interval',
  'teardown-threshold',
  'dist-fallback',
  'server-log',
  'sccache-ref',
];

function inputEnvName(id) {
  return 'INPUT_' + id.replace(/-/g, '_').toUpperCase();
}

// RUNNER_OS -> GOOS, RUNNER_ARCH -> GOARCH (GitHub's runner env naming).
function goos() {
  const m = { Linux: 'linux', macOS: 'darwin', Windows: 'windows' };
  const v = process.env.RUNNER_OS;
  return m[v] || (process.platform === 'win32' ? 'windows' : process.platform === 'darwin' ? 'darwin' : 'linux');
}

function goarch() {
  const m = { X64: 'amd64', ARM64: 'arm64', ARM: 'arm', X86: '386' };
  const v = process.env.RUNNER_ARCH;
  if (m[v]) return m[v];
  const a = process.arch;
  return a === 'x64' ? 'amd64' : a === 'arm64' ? 'arm64' : a === 'arm' ? 'arm' : a === 'ia32' ? '386' : 'amd64';
}

function assetName(g, a) {
  return `${TOOL}-${g}-${a}` + (g === 'windows' ? '.exe' : '');
}

// Turn a `uses:` ref into a SemVer range, so partial refs resolve to the latest
// matching release: `v1` -> "1.x" (any 1.*), `v1.0` -> "1.0.x", `v1.2.3` -> the
// exact "1.2.3". Returns null if the ref isn't a vMAJOR[.MINOR[.PATCH]] shape
// (a branch name or SHA), in which case the caller falls back to latest.
function refToRange(ref) {
  const v = ref.replace(/^v/i, '');
  if (!/^\d+(\.\d+){0,2}(-[0-9A-Za-z.-]+)?$/.test(v)) return null;
  const core = v.split('-')[0];
  const parts = core.split('.');
  if (parts.length === 1) return `${parts[0]}.x`;
  if (parts.length === 2) return `${parts[0]}.${parts[1]}.x`;
  return v; // full version -> exact match
}

// Normalize a release tag to a clean SemVer string, or null if it can't be
// coerced (skip non-version tags like "nightly"). semver.coerce accepts loose
// shapes — strips a leading `v`, fills missing minor/patch (so `v1.2` -> 1.2.0)
// — which is what we want for range matching.
function tagToSemver(tag) {
  const c = semver.coerce(tag);
  return c ? c.version : null;
}

// Pick the release matching the `uses:` ref. An exact tag match wins; otherwise
// the highest release whose SemVer satisfies the ref's range (so `v1` -> latest
// 1.*, never 10.*; pre-releases excluded). Drafts are never selected (their
// assets aren't downloadable). Delegates all version comparison to the `semver`
// library. Returns the chosen release object, or null for no match.
function pickRelease(releases, ref) {
  const usable = releases.filter((r) => !r.draft);
  const exact = usable.find((r) => r.tag_name === ref);
  if (exact) return exact;
  const range = refToRange(ref);
  if (!range) return null;
  const matched = usable
    .map((r) => ({ r, v: tagToSemver(r.tag_name) }))
    .filter((x) => x.v && semver.satisfies(x.v, range, { includePrerelease: false }));
  if (matched.length === 0) return null;
  matched.sort((x, y) => semver.rcompare(x.v, y.v));
  return matched[0].r;
}

async function resolveRelease(octokit, ref) {
  if (ref) {
    const releases = await octokit.paginate(octokit.rest.repos.listReleases, {
      owner: OWNER,
      repo: REPO,
      per_page: 100,
    });
    const hit = pickRelease(releases, ref);
    if (hit) return hit;
    core.info(`No release matching ref "${ref}"; falling back to latest release.`);
  }
  const { data } = await octokit.rest.repos.getLatestRelease({ owner: OWNER, repo: REPO });
  return data;
}

// Download the matching asset from a resolved release and cache it. Returns the
// path to the executable, or throws.
async function downloadBinary(octokit, ref, token) {
  const release = await resolveRelease(octokit, ref);
  const tag = release.tag_name;
  const g = goos();
  const a = goarch();
  const wanted = assetName(g, a);

  // tool-cache versions must be a stable, explicit SemVer (tc.find treats a
  // non-explicit version as a range and may miss). Use the coerced SemVer of the
  // tag when possible, else the raw tag — keep find and cacheDir on the same key.
  const cacheVersion = tagToSemver(tag) || tag;

  const cached = tc.find(TOOL, cacheVersion, a);
  if (cached) {
    core.info(`Using cached ${TOOL} ${tag} (${g}/${a}) from ${cached}`);
    return path.join(cached, wanted);
  }

  const asset = (release.assets || []).find((x) => x.name === wanted);
  if (!asset) {
    const have = (release.assets || []).map((x) => x.name).join(', ') || '(none)';
    throw new Error(`release ${tag} has no asset "${wanted}" (assets: ${have})`);
  }

  core.info(`Downloading ${wanted} from release ${tag}...`);
  // Authenticate the download so private/rate-limited fetches work; the public
  // browser_download_url also works token-less, but passing the token is harmless.
  const dl = await tc.downloadTool(asset.browser_download_url, undefined, token ? `token ${token}` : undefined);

  // Stage under the canonical asset name so the cached layout is predictable.
  const staged = path.join(path.dirname(dl), wanted);
  if (staged !== dl) fs.renameSync(dl, staged);
  if (g !== 'windows') fs.chmodSync(staged, 0o755);

  // cacheDir is async — await it (an unawaited Promise here silently breaks the
  // returned path AND leaves a half-copied cache for the next run).
  const dir = await tc.cacheDir(path.dirname(staged), TOOL, cacheVersion, a);
  const exe = path.join(dir, wanted);
  if (g !== 'windows') fs.chmodSync(exe, 0o755);
  core.info(`Cached ${TOOL} ${tag} at ${exe}`);
  return exe;
}

// Build the Go binary from the local action source. Used for the repo's own CI
// (`uses: ./`, empty GITHUB_ACTION_REF) so tests exercise the committed code,
// and as a last-resort fallback if the release download fails but `go` is present.
async function buildFromSource(actionPath) {
  const g = goos();
  const out = path.join(actionPath, TOOL + (g === 'windows' ? '.exe' : ''));
  core.info(`Building ${TOOL} from source at ${actionPath} (CGO_ENABLED=0)...`);
  await exec.exec('go', ['build', '-o', out, './cmd/sccache-dist-action'], {
    cwd: actionPath,
    env: { ...process.env, CGO_ENABLED: '0' },
  });
  if (g !== 'windows') fs.chmodSync(out, 0o755);
  return out;
}

// Is the `go` toolchain on PATH? io.which(tool, false) returns '' when absent
// (no throw), so this just tests for a non-empty resolved path.
async function goAvailable() {
  return (await io.which('go', false)) !== '';
}

// Download the prebuilt forked sccache + sccache-dist engine binaries (built in
// CI from github.com/xdqi/sccache) from this repo's releases and put them on
// PATH so the Go binary (and the user's `sccache` build calls) find them. The
// engine assets ride the same release as the Go binary, named
// `sccache-<ref>-linux-<arch>` and `sccache-dist-<ref>-linux-<arch>`.
async function ensureEngine(octokit, ref, token) {
  const a = goarch();
  const sref = core.getInput('sccache-ref') || 'sccache-dist-poc-tweaks';
  const binDir = path.join(process.env.RUNNER_TEMP || '/tmp', 'sccache-engine');
  await io.mkdirP(binDir);
  const release = await resolveRelease(octokit, ref);
  const cacheVersion = tagToSemver(release.tag_name) || release.tag_name;
  for (const name of ['sccache', 'sccache-dist']) {
    const wanted = `${name}-${sref}-linux-${a}`;
    const cacheKey = `${name}-${sref}`;
    let exe;
    const cached = tc.find(cacheKey, cacheVersion, a);
    if (cached) {
      exe = path.join(cached, name);
    } else {
      const asset = (release.assets || []).find((x) => x.name === wanted);
      if (!asset) {
        const have = (release.assets || []).map((x) => x.name).join(', ') || '(none)';
        throw new Error(`release ${release.tag_name} has no engine asset "${wanted}" (assets: ${have})`);
      }
      core.info(`Downloading engine ${wanted} from release ${release.tag_name}...`);
      const dl = await tc.downloadTool(asset.browser_download_url, undefined, token ? `token ${token}` : undefined);
      const staged = path.join(path.dirname(dl), name);
      if (staged !== dl) fs.renameSync(dl, staged);
      fs.chmodSync(staged, 0o755);
      const dir = await tc.cacheDir(path.dirname(staged), cacheKey, cacheVersion, a);
      exe = path.join(dir, name);
      fs.chmodSync(exe, 0o755);
    }
    fs.copyFileSync(exe, path.join(binDir, name));
    fs.chmodSync(path.join(binDir, name), 0o755);
  }
  core.addPath(binDir);
  core.info(`sccache engine (${sref}) on PATH at ${binDir}`);
}

async function resolveBinary() {
  const ref = process.env.GITHUB_ACTION_REF || '';
  const actionPath = process.env.GITHUB_ACTION_PATH || process.cwd();
  const token = core.getInput('github-token');

  // `uses: ./` (empty ref) means "test the CURRENT checkout": prefer building
  // the local source over downloading a released binary, so the repo's own CI
  // is honest about what it validates. External callers always pass a real ref.
  if (!ref) {
    if (await goAvailable()) {
      core.info('GITHUB_ACTION_REF is empty (local `uses: ./`); building from source.');
      return buildFromSource(actionPath);
    }
    core.info('GITHUB_ACTION_REF is empty but `go` is unavailable; downloading latest release.');
  }

  if (!token) {
    // Unauthenticated API calls share a 60 req/hr IP pool and download without
    // an auth header — easy to hit a 403 on a busy runner. Warn so the eventual
    // failure is legible instead of a mysterious rate-limit + go-build fallback.
    core.warning('github-token is empty; release API calls will be unauthenticated and may be rate-limited.');
  }
  const octokit = github.getOctokit(token);
  try {
    return await downloadBinary(octokit, ref, token);
  } catch (err) {
    core.warning(`Release download failed: ${err.message}`);
    if (await goAvailable()) {
      core.warning('Falling back to `go build` from source.');
      return buildFromSource(actionPath);
    }
    throw new Error(`could not obtain ${TOOL} binary (download failed and \`go\` is not on PATH): ${err.message}`);
  }
}

async function run() {
  // Windows is unsupported: the binary's detached-forwarder/teardown relies on
  // POSIX syscall.Setsid/Kill (so no windows asset is published), and distcc
  // itself is a POSIX tool. Fail fast with a clear message instead of a
  // confusing "no asset" + go-build-fallback chain.
  if (goos() === 'windows') {
    core.setFailed('sccache-dist-action does not support Windows runners (the farm is Linux only).');
    return;
  }

  const ref = process.env.GITHUB_ACTION_REF || '';
  const token = core.getInput('github-token');
  // Fetch the forked sccache engine binaries (skip on local `uses: ./` with no
  // ref — the engine is expected to be on PATH already in that dev path).
  if (ref) {
    try {
      const octokit = github.getOctokit(token);
      await ensureEngine(octokit, ref, token);
    } catch (err) {
      core.warning(`engine download failed: ${err.message}`);
    }
  } else {
    core.info('local `uses: ./` (no ref): expecting sccache/sccache-dist already on PATH');
  }
  const bin = await resolveBinary();

  // Wire INPUT_* for the Go binary. Pass an empty string through (the binary's
  // config layer already applies its own defaults for unset/empty values).
  const env = { ...process.env };
  for (const id of INPUT_IDS) {
    const v = core.getInput(id);
    env[inputEnvName(id)] = v;
  }

  // Run the binary with INHERITED stdio (not @actions/exec's pipes). The
  // coordinator detaches a Setsid forwarder that sets cmd.Stdout/Stderr to its
  // own (inherited) stdout/stderr and lives until job end. If we used
  // @actions/exec (which spawns with piped stdio and resolves on the 'close'
  // event), that long-lived grandchild would keep the pipe write-ends open, so
  // 'close' would never fire — forcing a 10s "STDIO did not close" timeout on
  // every coordinator run, and risking a stall if the pipe buffer fills.
  // Inheriting the runner's real fds avoids the pipes entirely and matches what
  // the old composite `run:` step did. The binary writes DISTCC_HOSTS/distcc-*
  // to $GITHUB_OUTPUT/$GITHUB_ENV itself; we resolve on 'exit' and propagate the
  // code, doing NO further I/O afterwards.
  const code = await new Promise((resolve, reject) => {
    const child = spawn(bin, [], { env, stdio: 'inherit' });
    child.on('error', reject);
    child.on('exit', (code, signal) => resolve(signal ? 128 : code === null ? 1 : code));
  });
  if (code !== 0) {
    core.setFailed(`${TOOL} exited with code ${code}`);
  }
}

// Run only as the action entrypoint; importing for tests must not execute it.
if (require.main === module) {
  run().catch((err) => {
    core.setFailed(err && err.message ? err.message : String(err));
  });
}

// Exported for unit-level reasoning/testing; harmless under the bundled action.
module.exports = { refToRange, tagToSemver, pickRelease, assetName, goos, goarch, inputEnvName, run };
