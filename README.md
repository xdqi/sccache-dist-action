# sccache-dist-action

A reusable GitHub Action that turns several GitHub-hosted runners into an
ephemeral [**sccache-dist**](https://github.com/mozilla/sccache) compile farm,
wired together by a temporary [Tailscale](https://tailscale.com/) mesh.

One **coordinator** runner runs your build *and* hosts the sccache-dist
scheduler plus the sccache client. N **worker** runners each run an
`sccache-dist server`. When the coordinator invokes something like
`make -j CC="sccache gcc"`, compilation fans out across the workers over the
tailnet, while object files flow back into a content-addressed cache on the
coordinator.

It is a sibling of [`xdqi/distcc-action`](https://github.com/xdqi/distcc-action),
but with two things plain distcc cannot offer: a real **content-addressed
compile cache** (`.o` results dedup and persist across runs) and
**cross-toolchain support** (the engine can package and ship arbitrary
toolchains to the workers).

## How it works

- A single static **Go binary** embeds Tailscale via
  [`tsnet`](https://pkg.go.dev/tailscale.com/tsnet) — no system `tailscaled`,
  no root, nothing to install on the runner. Each invocation joins the tailnet
  as an ephemeral, tagged node.
- The action's `mode` input selects behaviour: `coordinator` or `worker`.
- Every run namespaces its tailnet hostnames by `github.run_id` (the
  `run-prefix` input), so concurrent CI jobs never collide on the tailnet.
- The **coordinator** brings up the sccache-dist scheduler, waits for the
  expected workers to come online, then detaches a background **forwarder**.
  The forwarder holds the scheduler and per-worker `tsnet` port-forwards open
  for the lifetime of the job, so the build step can reach every worker over
  plain loopback. It is torn down when the job ends.
- Each **worker** registers with the scheduler and serves compile jobs. A
  worker watches the coordinator's tailnet node and **exits when the
  coordinator goes offline** (after `teardown-threshold` consecutive offline
  reads), so worker jobs never hang past the build they were serving.
- The engine binaries (`sccache` + `sccache-dist`) are built from the fork
  [`github.com/xdqi/sccache`](https://github.com/xdqi/sccache) (branch
  `sccache-dist-poc-tweaks`) and **downloaded at runtime** by the JS wrapper
  from a release asset; nothing is compiled on the critical path.

## Quick start

Two jobs: a `workers` matrix that brings up the farm, and a `build`
coordinator that runs the actual compile. Both authenticate to the same
tailnet with `oauth-secret`.

```yaml
jobs:
  workers:
    strategy:
      matrix: { idx: [1, 2, 3] }
    runs-on: ubuntu-latest
    steps:
      - uses: xdqi/sccache-dist-action@v1
        with: { mode: worker, worker-index: '${{ matrix.idx }}', oauth-secret: '${{ secrets.TS_OAUTH_SECRET }}' }
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: xdqi/sccache-dist-action@v1
        with: { mode: coordinator, expected-workers: 3, oauth-secret: '${{ secrets.TS_OAUTH_SECRET }}' }
      - run: |
          git clone --depth 1 https://github.com/redis/redis && cd redis
          make -j"${SCCACHE_J}" CC="sccache gcc"
```

The `workers` and `build` jobs run concurrently. The coordinator blocks in its
setup step until `expected-workers` servers have registered (or `wait-timeout`
elapses), then exports `SCCACHE_J` and friends for the build step. The worker
jobs stay alive serving compiles and shut themselves down once the coordinator
finishes.

## Tailscale setup (one-time)

You need a tagged, OAuth-authed tailnet so that every CI run can mint ephemeral
nodes without manual approval:

1. In your tailnet policy (ACL) file, declare an owner for the tag the action
   uses, e.g.:

   ```jsonc
   "tagOwners": {
     "tag:ci-sccache": ["autogroup:admin"]
   }
   ```

2. Create an **OAuth client** with the `auth_keys` **write** scope and attach
   the same tag (`tag:ci-sccache`). The action uses the client secret directly
   as an ephemeral auth key.
3. Store the client secret as the repository secret **`TS_OAUTH_SECRET`**.

All nodes the action creates are **ephemeral** (they disappear from the tailnet
shortly after the job ends) and **tagged**, so they inherit the tag's ACL
grants rather than a user identity.

## The cache — the extra value over distcc

distcc only farms out compilation; every run recompiles everything. sccache
adds a **content-addressed compile cache**: identical preprocessed inputs map
to the same cache key, so an unchanged `.o` is fetched instead of recompiled.

The action points the cache at `~/.cache/sccache` on the coordinator and
exports `SCCACHE_DIR` accordingly. Wrap the coordinator job's cache directory
with [`actions/cache`](https://github.com/actions/cache), keyed on OS and
compiler, to persist results across CI runs:

```yaml
      - uses: actions/cache@v4
        with:
          path: ~/.cache/sccache
          key: sccache-${{ runner.os }}-gcc-${{ github.sha }}
          restore-keys: |
            sccache-${{ runner.os }}-gcc-
```

Place this step before the build. On a warm cache the farm is mostly fetching
prebuilt objects; on a cold or partial cache it distributes the misses across
the workers. Plain distcc has no equivalent — there is nothing to cache and
nothing to restore.

## Inputs

| Input                | Required | Default                | Description |
| -------------------- | -------- | ---------------------- | ----------- |
| `mode`               | yes      | —                      | `coordinator` or `worker`. |
| `oauth-secret`       | yes      | —                      | Tailscale OAuth client secret (used directly as the ephemeral auth key). |
| `oauth-client-id`    | no       | —                      | Reserved; currently unused. |
| `expected-workers`   | no       | —                      | Number of workers the coordinator waits for. |
| `min-workers`        | no       | `expected-workers`     | Minimum workers online before the coordinator proceeds. |
| `wait-timeout`       | no       | `300s`                 | Maximum time the coordinator waits for workers. |
| `worker-index`       | no       | —                      | Unique per worker (typically the matrix index). |
| `tags`               | no       | `tag:ci-sccache`       | Tailnet tag(s) applied to the ephemeral nodes. |
| `run-prefix`         | no       | `${{ github.run_id }}` | Hostname namespace, isolating concurrent runs. |
| `slots`              | no       | `0`                    | Per-worker concurrent compile jobs (`0` = `nproc`). |
| `poll-interval`      | no       | `1s`                   | Worker teardown poll interval. |
| `teardown-threshold` | no       | `5`                    | Consecutive offline reads before a worker exits. |
| `dist-fallback`      | no       | `true`                 | Sets `SCCACHE_DIST_FALLBACK` — fall back to local compile when a job is not distributable. |
| `sccache-ref`        | no       | `sccache-dist-poc-tweaks` | Forked sccache build/branch to download and use. |
| `github-token`       | no       | `${{ github.token }}`  | Token for downloading the engine release asset. |

## Outputs

| Output           | Description |
| ---------------- | ----------- |
| `workers-online` | Number of workers that actually registered and are participating. |
| `sccache-j`      | Suggested `-j` value (sum of worker slots). |
| `scheduler-url`  | The coordinator's scheduler URL. |

## Exported environment for the build step

The coordinator setup step exports the following into `GITHUB_ENV`, ready for
your build command:

| Variable                  | Meaning |
| ------------------------- | ------- |
| `SCCACHE_J`               | Suggested `-j` (sum of online worker slots). |
| `SCCACHE_WORKERS_ONLINE`  | Number of participating workers. |
| `SCCACHE_DIR`             | Cache directory (`~/.cache/sccache`). |
| `SCCACHE_DIST_FALLBACK`   | Whether non-distributable jobs fall back to local compile. |

The coordinator also writes `~/.config/sccache/config` so the sccache client
knows how to reach the scheduler over the tailnet.

## Scope & limitations

- **Single coordinator.** There is one build client per farm; this is not a
  shared, multi-client cluster.
- **Linux workers only.** `sccache-dist server` is Linux-only, so worker
  runners must be Linux.
- **System gcc in the example.** The quick start uses the runner's system gcc;
  the toolchain is auto-packaged and shipped to the servers.
- **Cross-toolchain is supported but not the headline.** The engine can
  distribute cross compilers such as `zig cc`; the simple example sticks to
  native gcc for clarity.
- **Assembly compiles locally.** The engine forces `.S`/assembly inputs to
  compile on the coordinator; C/C++ translation units are what get
  distributed.
