# M0 connection-model spike — RESULT

**Date:** 2026-06-08
**Verdict:** PASS — the coordinator-forward topology works over tsnet; the
X-Real-IP / `invalid_bearer_token_mismatched_address` risk did NOT materialize.

## What was validated
Two tsnet nodes in two isolated docker networks (real cross-node routing):
- coordinator container: tsnet `<pfx>-coordinator` + `sccache-dist scheduler`
  (:10600) + `sccache` client. tsbridge exposes :10600 over tsnet and forwards
  `127.0.0.1:10501` -> tsnet `<pfx>-worker-1:10501`.
- worker container: tsnet `<pfx>-worker-1` + `sccache-dist server` (docker
  builder). tsbridge exposes :10501 over tsnet and forwards `127.0.0.1:10600`
  -> tsnet `<pfx>-coordinator:10600`.

Server registered with `public_addr = 127.0.0.1:10501` (coordinator-local).

## Evidence
- `sccache --dist-status` -> `num_servers: 1` (server registered over tsnet)
- `sccache gcc -c h.c -o h.o` with `SCCACHE_DIST_FALLBACK=0` -> `COMPILE_OK` (1104-byte ELF)
- `sccache -s` -> `Successful distributed compiles` >=1, `Failed distributed 0`
- NO `invalid_bearer_token_mismatched_address` in the server log.

## DECISION for the full action (locks Milestones 2/3/4)
Use the **coordinator-local `public_addr`** strategy:
- worker's `sccache-dist server` advertises `public_addr = 127.0.0.1:<port>`
  where `<port> = 10501 + (worker_index - 1)`.
- coordinator forwards `127.0.0.1:<port>` -> tsnet `<run>-worker-<i>:10501`
  (each worker's server binds 0.0.0.0:10501 inside its own runner).
- worker reaches the scheduler via a local bridge `127.0.0.1:10600` -> tsnet
  `<run>-coordinator:10600`; server's `scheduler_url = http://127.0.0.1:10600`.
- shared token = the Tailscale OAuth secret (arbitrary shared string).

This matches `internal/coordinator` + `internal/worker` as drafted in the plan.
No scheduler patch needed. Proceed to Milestone 1.
