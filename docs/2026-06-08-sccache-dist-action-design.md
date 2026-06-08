# sccache-dist-action — Design

**Date:** 2026-06-08
**Status:** Design approved; ready for implementation planning.

## Summary

A reusable GitHub Action that turns several GitHub-hosted runners into an
**ephemeral sccache-dist compilation farm** connected by a temporary Tailscale
mesh — the same shape as [`distcc-action`](https://github.com/xdqi/distcc-action),
but the compile engine is **sccache-dist** instead of distcc. A coordinator
runner runs the build (and hosts the scheduler), N worker runners lend cores as
sccache-dist build servers, and the coordinator's ordinary `make -j CC="sccache gcc"`
fans compilation out across them. Adds two things distcc-action cannot:
**a real content-addressed compile cache** (persisted across runs via
`actions/cache`) and **cross-toolchain distribution** (e.g. our forked `zig cc`).

This is a sibling project to distcc-action and **copies its architecture
wholesale** (Node JS wrapper that resolves a prebuilt Go binary; a single
`CGO_ENABLED=0` Go binary embedding Tailscale via `tsnet`; `mode:
coordinator|worker`; per-run hostname namespacing; coordinator-goes-offline
teardown). Only the compile-engine layer changes.

## Background / what's reused vs new

distcc-action's structure is the template (see its full map in the chat / its
`docs/`). We keep:

- **Node 24 JS wrapper** (`src/index.js` → bundled `dist/index.js`): downloads
  the prebuilt Go binary from this repo's Releases (or builds from source on
  `uses: ./`), installs deps, sets `INPUT_*` env, execs the binary with inherited
  stdio.
- **Go binary** (`cmd/sccache-dist-action`, `internal/{config,tsmesh}`): the
  tsnet mesh layer (`tsmesh.Up`/`Peers`/`WaitForWorkers`/`Listen`/`Dial`),
  per-run hostname namespacing (`<run_id>-coordinator` / `<run_id>-worker-<i>`),
  ephemeral nodes, OAuth-secret-as-authkey, the detached-forwarder pattern on the
  coordinator, the worker guard loop (exit when coordinator peer goes offline).
- **Lifecycle**: coordinator's detached forwarder stays alive holding the tsnet
  bridges + scheduler until job end; workers poll and exit when the coordinator
  drops.

What changes (the sccache-dist-specific layer, replacing `internal/distccrun`
with `internal/sccachedist`):

- The **engine binaries**: `sccache` + `sccache-dist`, built from our fork
  **`xdqi/sccache` branch `sccache-dist-poc-tweaks`** (the 4 dist fixes + native
  `zig cc`). Not distcc.
- Three roles instead of two: coordinator additionally runs the **scheduler**;
  workers run **`sccache-dist server`** instead of `distccd`.
- The coordinator exports a **different env contract** (sccache config +
  `SCCACHE_*`), not `DISTCC_HOSTS`.

## Architecture / topology

```
workflow
├── job: workers   matrix [1..N]                          (parallel)
│     uses: <repo>@v1  mode: worker
│        - tsnet join (hostname=<run_id>-worker-<i>, tag)
│        - run `sccache-dist server` (docker builder; spins a job container
│          per compile on the runner's own docker)
│        - expose the server's HTTP port over tsnet
│        - register with the scheduler (reached over tsnet)
│        - guard loop: exit when <run_id>-coordinator goes offline
│
└── job: build   (coordinator)                            (parallel)
      uses: <repo>@v1  mode: coordinator  expected-workers: N
        main process: spawn detached forwarder, wait for env export, return
        detached forwarder (lives until job end):
          - tsnet join (hostname=<run_id>-coordinator, tag)
          - start `sccache-dist scheduler` (listens on a local port)
          - wait for <run_id>-worker-* peers Online (up to wait-timeout)
          - for each worker: tsnet-forward a local 127.0.0.1:105xx -> worker
            server over tsnet  (same bridge as distcc-action's 37xx->distccd)
          - write the sccache client config (scheduler_url=local scheduler,
            token auth) + export SCCACHE_* into $GITHUB_ENV
          - start the sccache client server (`sccache --start-server`)
          - block (select{}) until job end
        (user build runs here: make -j  CC="sccache gcc")
        post: forwarder killed at job end -> coordinator node drops ->
              scheduler dies with it -> workers see coordinator offline -> exit
```

### Three roles, on two runner kinds

- **Coordinator runner**: scheduler + sccache client (+ the user's build). The
  scheduler is near-stateless and lives only for this run — perfectly ephemeral,
  matching distcc-action's "nothing lingers" model.
- **Worker runner**: one `sccache-dist server` each (docker builder — spins a
  busybox job container per compile via the runner's docker; no bwrap/root needed,
  validated in the PoC).

### The connection model (the hard part — must spike first)

sccache-dist's protocol has TWO connection axes that both must traverse tsnet:

1. **server → scheduler** (registration/heartbeat): each worker's
   `sccache-dist server` must reach the scheduler on the coordinator. Worker dials
   the coordinator over tsnet.
2. **client → server** (submit_toolchain / run_job): the sccache client (on the
   coordinator) connects **directly** to each server at the `public_addr` the
   scheduler handed back. We make that reachable by having the coordinator
   **tsnet-forward a local `127.0.0.1:105xx` to each worker's server** (exactly
   distcc-action's port-forward bridge), and registering the server with
   `public_addr = 127.0.0.1:105xx` so the scheduler hands the client a
   coordinator-local address.

**RISK — `X-Real-IP` / `invalid_bearer_token_mismatched_address`:** the scheduler
verifies a registering server's source IP against the address it claims, and the
job token is per-IP. In the PoC, host-as-client only worked once the server's
real connection IP matched its `public_addr` (we used host networking). In the
tsnet-forward model the server registers from its tailnet IP but must advertise a
coordinator-local `127.0.0.1:105xx`. **This mismatch is the #1 thing to spike
before building the full action** — options if it rejects:
  - run the scheduler with `server_auth`/`client_auth` such that the address
    check is satisfied (token mode + ensure the forwarded path preserves the
    expected source), or
  - forward the server→scheduler heartbeat through the same tsnet bridge so the
    scheduler sees a consistent address, or
  - patch the fork's scheduler to relax/redirect the IP check for this topology
    (last resort; we already maintain the fork).
This risk is sccache-dist-specific and has no distcc analogue.

## Engine binaries — from the fork

- Source: **`github.com/xdqi/sccache` @ `sccache-dist-poc-tweaks`** (already
  pushed). Carries: never-distribute-assembly, `/dev/null` regular-file packing,
  docker-builder robustness (cp-deadlock/start-stop/kill-PID1), and native
  `zig cc`.
- Built as **static musl** (`cargo build --release --target
  x86_64-unknown-linux-musl --no-default-features --features="dist-client
  dist-server vendored-openssl"`) so the binaries run on any runner image. The
  same Stage-0 recipe as `distcc-action/spike/sccache-dist`.
- The JS wrapper (or a release workflow) obtains them. Decision: **publish the
  prebuilt sccache/sccache-dist binaries as Release assets of THIS action repo**
  (built in CI from the fork), so the action downloads them like distcc-action
  downloads its Go binary. `install-distcc`'s analogue (`install-sccache`) is not
  needed — we always ship our forked binaries.

## Inputs (action.yml) — mirrors distcc-action, renamed for sccache

Required: `mode` (coordinator|worker), `oauth-secret` (Tailscale).
Coordinator: `expected-workers`, `min-workers`, `wait-timeout`.
Worker: `worker-index`.
Common: `tags`, `run-prefix`, `slots` (per-worker concurrent jobs), `poll-interval`,
`teardown-threshold`.
New/sccache-specific:
- `sccache-version` / `sccache-ref`: which fork build to use (default: pinned).
- `cache` (bool, default true): enable `actions/cache` persistence of the
  coordinator's `~/.cache/sccache` across runs.
- `cache-key` / `cache-prefix`: actions/cache key (default derived from
  `runner.os` + a compiler hash).
- `dist-fallback` (bool, default true): `SCCACHE_DIST_FALLBACK` — let a
  non-distributable compile fall back to local rather than fail the build.

## Outputs / exported env (the contract the build step uses)

Instead of `DISTCC_HOSTS`/`DISTCC_CC_PREFIX`, the coordinator exports:
- writes `~/.config/sccache/config` (`[dist] scheduler_url=http://127.0.0.1:<sched>`,
  `[dist.auth] type=token token=<shared>`).
- `$GITHUB_ENV`: `SCCACHE_DIST_FALLBACK`, optionally `SCCACHE_DIR`,
  `RUSTC_WRAPPER`/`CC` hints, and a `SCCACHE_WORKERS_ONLINE` count.
- The user build step uses **`make -j<J> CC="sccache gcc"`** (gcc auto-packaged
  to the servers on Linux). `<J>` = sum of worker slots (exported as
  `SCCACHE_J`, analogous to `DISTCC_J`).
- Step outputs: `workers-online`, `scheduler-url`, `sccache-j`.

## Cache persistence (actions/cache)

The coordinator's compile cache lives at `~/.cache/sccache` (client-side disk
cache; the worker servers hold only the toolchain cache, not objects — confirmed
in the PoC). When `cache: true`, the example workflow wraps the coordinator job
with `actions/cache` keyed on OS + compiler version, so the content-addressed
`.o` cache survives across runs. This is the concrete extra value over distcc
(which has no cache layer at all). The action documents the key; persistence is
done in the user's workflow via `actions/cache` (the action exports `SCCACHE_DIR`
so the cache path is known), keeping the action itself stateless.

## Example workflows (gcc, redis + linux)

- **selftest.yml**: 2 workers + coordinator; build **redis** with
  `make -j$SCCACHE_J CC="sccache gcc"`; assert distribution via `sccache -s`
  (non-zero "successful distributed compiles", 0 failed). Wrap coordinator with
  `actions/cache` to demonstrate cross-run cache hits on re-run.
- **smoke.yml**: 3 workers + coordinator; build the **Linux kernel** tinyconfig
  `bzImage` with `CC="sccache gcc"`; assembly compiles locally (our fork's gate),
  C distributes; boot the bzImage in QEMU and assert it reaches init (the same
  success criterion distcc-action's smoke uses).
- Both use plain system **gcc** (auto-packaged), per the chosen example compiler.

## Files (new repo ~/sccache-dist-action/)

| path | role |
|---|---|
| `action.yml` | inputs/outputs, `node24` → `dist/index.js` |
| `src/index.js` → `dist/index.js` | JS wrapper (port of distcc-action's), downloads the Go binary + the sccache/sccache-dist engine binaries |
| `cmd/sccache-dist-action/{main,procctl}.go` | entrypoint, mode dispatch, detached forwarder |
| `internal/config` | `INPUT_*` parsing (sccache-flavored) |
| `internal/tsmesh` | tsnet mesh (ported verbatim from distcc-action) |
| `internal/coordinator` | scheduler launch + worker wait + per-server tsnet forward + write sccache config + export env |
| `internal/worker` | `sccache-dist server` launch + expose over tsnet + register + guard loop |
| `internal/sccachedist` | replaces `distccrun`: render scheduler.conf/server.conf, start scheduler/server, compute J, token mgmt |
| `.github/workflows/{release,selftest,smoke}.yml` | build engine binaries from the fork + Go binary; the two example builds |
| `docs/` | this design |
| `go.mod` | `github.com/xdqi/sccache-dist-action` |

## Verification

1. **Spike the connection model FIRST** (the X-Real-IP risk) on real GitHub
   Actions or a local 2-container tsnet mock before building the full action:
   1 coordinator (scheduler+client) + 1 worker (server), confirm the worker
   registers and a single `sccache gcc -c` distributes over tsnet.
2. selftest: redis builds, `sccache -s` shows distributed compiles, 0 failed;
   re-run shows cache hits (actions/cache working).
3. smoke: kernel bzImage builds distributed, boots in QEMU to the init/VFS panic.
4. Released-binary path: a second repo's workflow `uses: xdqi/sccache-dist-action@vX`
   downloads prebuilt assets and builds redis.

## Risks (ordered)

1. **server↔scheduler↔client over tsnet + X-Real-IP** (above) — the load-bearing
   unknown; spike before committing to the full build.
2. **Engine binary size / startup**: shipping sccache+sccache-dist (~30 MB) as
   release assets; cache via tool-cache like the Go binary.
3. **Scheduler ephemerality**: confirmed near-stateless in the PoC; rebuilt from
   heartbeats — fine for per-run lifecycle.
4. **gcc version skew client vs auto-packaged**: client auto-packages its own gcc
   and ships it, so server needs no gcc — but keep all runners on the same image
   (same gcc) as distcc-action already requires.

## Non-goals (v1)

- No multi-client shared farm (single coordinator, per the user's scope).
- No Windows/macOS workers (sccache-dist servers are Linux-only; same as
  distcc-action's POSIX-only stance).
- Example uses gcc; `zig cc` cross-distribution is supported by the engine but
  not the headline example.
