# sccache-dist-action Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A reusable GitHub Action that stands up an ephemeral sccache-dist compile farm over a Tailscale tsnet mesh (coordinator hosts scheduler + sccache client; worker runners run sccache-dist servers), so a coordinator's `make -j CC="sccache gcc"` distributes across workers with a content-addressed cache persisted via actions/cache.

**Architecture:** Port distcc-action's structure verbatim — Node JS wrapper resolving a prebuilt Go binary, a single `CGO_ENABLED=0` Go binary embedding Tailscale via `tsnet`, `mode: coordinator|worker`, per-run hostname namespacing, detached-forwarder coordinator, coordinator-offline teardown. Swap the `distccrun` engine layer for an `sccachedist` layer; add a third role (scheduler) co-located on the coordinator. Engine binaries (`sccache`+`sccache-dist`) are built from the fork `github.com/xdqi/sccache @ sccache-dist-poc-tweaks`.

**Tech Stack:** Go 1.26 (`tailscale.com/tsnet`), Node 24 (`@actions/*`, `semver`, `@vercel/ncc`), Rust (engine binaries, built in CI), Docker (sccache-dist docker builder), GitHub Actions.

**CRITICAL ORDERING:** Milestone 0 is a make-or-break spike of the
server↔scheduler↔client-over-tsnet connection model and the
`invalid_bearer_token_mismatched_address` (X-Real-IP) risk. **Do not start
Milestone 1+ until Milestone 0 passes or its fix is identified.** Everything
downstream assumes the connection model works.

**Reference template:** `/home/kosaka/distcc-action` (the structure being ported).
**PoC reference:** `/home/kosaka/distcc-action/spike/sccache-dist/` (working
scheduler/server/client configs + launch commands).
**Design:** `~/sccache-dist-action/docs/2026-06-08-sccache-dist-action-design.md`.

---

## File Structure

```
~/sccache-dist-action/
├── action.yml                          # inputs/outputs, node24 -> dist/index.js
├── package.json                        # @actions/*, semver; "build": ncc
├── src/index.js                        # JS wrapper (port of distcc-action)
├── dist/index.js                       # ncc-bundled wrapper (committed)
├── go.mod                              # github.com/xdqi/sccache-dist-action
├── cmd/sccache-dist-action/
│   ├── main.go                         # mode dispatch + detached forwarder
│   └── procctl.go                      # Setsid spawn, PID file, waitForEnvExport
├── internal/
│   ├── config/config.go                # INPUT_* parsing (sccache-flavored)
│   ├── tsmesh/tsmesh.go                # tsnet mesh (verbatim port)
│   ├── sccachedist/sccachedist.go      # render confs, start scheduler/server, J, token
│   ├── coordinator/coordinator.go      # scheduler + wait + forward + write config + export
│   └── worker/worker.go                # sccache-dist server + expose + guard
├── spike/                              # Milestone 0 only (kept for repro)
│   └── tsnet-conn/                     # the connection-model spike
├── .github/workflows/
│   ├── engine.yml                      # build sccache/sccache-dist from the fork -> release assets
│   ├── release.yml                     # build the Go binary matrix -> release assets
│   ├── selftest.yml                    # 2 workers + coordinator, build redis (gcc), assert dist + cache
│   └── smoke.yml                       # 3 workers + coordinator, build linux kernel (gcc), boot QEMU
└── docs/...
```

---

## Milestone 0 — Connection-model spike (THE GATE)

Validate, with the **real forked binaries** over a **real tsnet mesh**, that a
worker's `sccache-dist server` registers with a scheduler on the coordinator and
a single `sccache gcc -c` distributes — specifically clearing the X-Real-IP /
`invalid_bearer_token_mismatched_address` check in the tsnet-forward topology.

The spike uses **two local tsnet nodes in two containers on isolated docker
networks** (the faithful cross-host model distcc-action's spike used), NOT host
networking (host networking is what masked the IP problem in the PoC).

### Task 0.1: Obtain the forked engine binaries (static musl)

**Files:**
- Create: `spike/tsnet-conn/get-binaries.sh`

- [ ] **Step 1: Write the binary-fetch script**

```bash
#!/usr/bin/env bash
# Build sccache + sccache-dist (static musl) from the fork and drop into ./bin.
set -euo pipefail
cd "$(dirname "$0")"
FORK_URL="https://github.com/xdqi/sccache.git"
FORK_REF="sccache-dist-poc-tweaks"
rm -rf src-clone && git clone --depth 1 --branch "$FORK_REF" "$FORK_URL" src-clone
docker run --rm -v "$PWD/src-clone":/src -v "$PWD/bin-out":/out -w /src \
  rust:1-bookworm bash -c '
    apt-get update >/dev/null 2>&1
    apt-get install -y --no-install-recommends pkg-config perl make musl-tools musl-dev >/dev/null 2>&1
    rustup target add x86_64-unknown-linux-musl >/dev/null 2>&1
    cargo build --release --target x86_64-unknown-linux-musl \
      --no-default-features --features="dist-client dist-server vendored-openssl"
    mkdir -p /out
    cp target/x86_64-unknown-linux-musl/release/sccache /out/
    cp target/x86_64-unknown-linux-musl/release/sccache-dist /out/
  '
mkdir -p bin && cp bin-out/sccache bin-out/sccache-dist bin/
file bin/sccache-dist
```

- [ ] **Step 2: Run it, verify static musl binaries**

Run: `mkdir -p ~/sccache-dist-action/spike/tsnet-conn && cd ~/sccache-dist-action/spike/tsnet-conn && bash get-binaries.sh`
Expected: `bin/sccache-dist: ELF 64-bit ... statically linked`

> Note: locally you may instead copy the already-built binaries from
> `/home/kosaka/distcc-action/spike/sccache-dist/bin/` to save the Rust build.

- [ ] **Step 3: Commit**

```bash
cd ~/sccache-dist-action
git add spike/tsnet-conn/get-binaries.sh
git commit -m "spike(conn): script to build forked sccache/sccache-dist (static musl)"
```

### Task 0.2: A tiny tsnet bridge program for the spike

We need to prove the bridge: the coordinator's sccache client connects to a
local port that tsnet-forwards to the worker's server, AND the worker's server
reaches the scheduler over tsnet. Write a minimal Go `tsbridge` that joins the
mesh and forwards a local port to a peer (mirrors coordinator's bridge), reused
by the spike on both ends.

**Files:**
- Create: `spike/tsnet-conn/tsbridge/main.go`
- Create: `spike/tsnet-conn/tsbridge/go.mod`

- [ ] **Step 1: Write tsbridge (join mesh; forward localPort -> peerHost:peerPort)**

```go
// tsbridge: join a tailnet and forward a local TCP port to peer:port over tsnet.
// Usage: tsbridge -hostname H -authkey K -tags T -listen 127.0.0.1:LP -target PEER:PP
package main

import (
	"context"
	"flag"
	"io"
	"log"
	"net"
	"strings"
	"tailscale.com/tsnet"
)

func main() {
	hostname := flag.String("hostname", "", "tsnet hostname")
	authkey := flag.String("authkey", "", "tailscale authkey")
	tags := flag.String("tags", "", "comma tags")
	listen := flag.String("listen", "", "local addr to listen on (empty = none)")
	target := flag.String("target", "", "peer host:port to forward to (over tsnet)")
	expose := flag.String("expose", "", "tsnet addr to expose -> localTarget (empty = none)")
	localTarget := flag.String("local-target", "", "local host:port that -expose forwards to")
	flag.Parse()

	var at []string
	for _, t := range strings.Split(*tags, ",") {
		if t = strings.TrimSpace(t); t != "" {
			at = append(at, t)
		}
	}
	srv := &tsnet.Server{Hostname: *hostname, AuthKey: *authkey, Ephemeral: true,
		Dir: "/tmp/tsnet-" + *hostname, AdvertiseTags: at}
	if err := srv.Start(); err != nil {
		log.Fatalf("start: %v", err)
	}
	if _, err := srv.Up(context.Background()); err != nil {
		log.Fatalf("up: %v", err)
	}
	log.Printf("tsbridge %s joined", *hostname)

	// Forward a LOCAL listener to a tsnet peer (client side).
	if *listen != "" && *target != "" {
		ln, err := net.Listen("tcp", *listen)
		if err != nil {
			log.Fatalf("listen %s: %v", *listen, err)
		}
		go acceptLoop(ln, func(c net.Conn) (net.Conn, error) {
			return srv.Dial(context.Background(), "tcp", *target)
		})
		log.Printf("forward %s -> tsnet:%s", *listen, *target)
	}
	// Expose a tsnet listener forwarding to a LOCAL target (server side).
	if *expose != "" && *localTarget != "" {
		ln, err := srv.Listen("tcp", *expose)
		if err != nil {
			log.Fatalf("tsnet listen %s: %v", *expose, err)
		}
		go acceptLoop(ln, func(c net.Conn) (net.Conn, error) {
			return net.Dial("tcp", *localTarget)
		})
		log.Printf("expose tsnet:%s -> %s", *expose, *localTarget)
	}
	select {}
}

func acceptLoop(ln net.Listener, dial func(net.Conn) (net.Conn, error)) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			r, err := dial(c)
			if err != nil {
				return
			}
			defer r.Close()
			go io.Copy(r, c)
			io.Copy(c, r)
		}()
	}
}
```

- [ ] **Step 2: go.mod for tsbridge**

```
module tsbridge

go 1.26

require tailscale.com v1.100.0
```

- [ ] **Step 3: Build it static**

Run: `cd ~/sccache-dist-action/spike/tsnet-conn/tsbridge && go mod tidy && CGO_ENABLED=0 go build -o ../bin/tsbridge .`
Expected: `../bin/tsbridge` produced, no errors.

- [ ] **Step 4: Commit**

```bash
cd ~/sccache-dist-action
git add spike/tsnet-conn/tsbridge
git commit -m "spike(conn): tsbridge — minimal tsnet port-forward both directions"
```

### Task 0.3: The spike — coordinator(scheduler+client) + worker(server) over tsnet

**Files:**
- Create: `spike/tsnet-conn/run.sh`
- Create: `spike/tsnet-conn/scheduler.conf`, `server.conf`, `client.config`
- Create: `spike/tsnet-conn/Dockerfile.node`

- [ ] **Step 1: Write the three sccache-dist configs (token auth, placeholders)**

`scheduler.conf`:
```
public_addr = "0.0.0.0:10600"
[client_auth]
type = "token"
token = "__TOKEN__"
[server_auth]
type = "token"
token = "__TOKEN__"
```

`server.conf`:
```
cache_dir = "/var/lib/sccache-dist/cache"
public_addr = "__SERVER_PUBLIC_ADDR__"
bind_address = "0.0.0.0:10501"
scheduler_url = "__SCHEDULER_URL__"
[builder]
type = "docker"
[scheduler_auth]
type = "token"
token = "__TOKEN__"
```

`client.config`:
```
[dist]
scheduler_url = "__SCHEDULER_URL__"
toolchain_cache_size = 2147483648
[dist.auth]
type = "token"
token = "__TOKEN__"
```

- [ ] **Step 2: Dockerfile.node (runtime image carrying the binaries + tsbridge + gcc + docker CLI)**

```dockerfile
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      gcc g++ libc6-dev make git ca-certificates docker.io \
    && rm -rf /var/lib/apt/lists/*
COPY bin/sccache /usr/local/bin/sccache
COPY bin/sccache-dist /usr/local/bin/sccache-dist
COPY bin/tsbridge /usr/local/bin/tsbridge
RUN chmod +x /usr/local/bin/sccache /usr/local/bin/sccache-dist /usr/local/bin/tsbridge
```

- [ ] **Step 3: Write run.sh — the spike orchestration**

```bash
#!/usr/bin/env bash
# Spike: prove sccache-dist distributes over a tsnet mesh with the
# coordinator-forward topology, clearing the X-Real-IP check.
#
# Topology (two isolated docker nets => real DERP, like distcc-action's spike):
#   coord container: tsnet "coordinator" + sccache-dist scheduler (:10600)
#                    + sccache client; runs a tsbridge forwarding
#                    127.0.0.1:10501 -> tsnet "spk-worker-1":10501
#   worker container: tsnet "spk-worker-1" + sccache-dist server (docker builder)
#                    exposing its :10501 over tsnet; reaches scheduler over a
#                    tsbridge forwarding 127.0.0.1:10600 -> tsnet "coordinator":10600
#
# KEY TEST: server registers public_addr=127.0.0.1:10501 (coordinator-local), the
# client reaches it through the coordinator's bridge; confirm no
# invalid_bearer_token_mismatched_address and a distributed compile.
set -euo pipefail
cd "$(dirname "$0")"
: "${TS_AUTHKEY:?set TS_AUTHKEY to a tailscale authkey (OAuth client secret w/ tag)}"
TOKEN="${TOKEN:-spike-shared-token}"
TAG="${TAG:-tag:ci}"
PFX="spk$$"
IMG=sccache-spike:latest

docker build -f Dockerfile.node -t "$IMG" .

# isolated networks (force real cross-node routing)
docker network create "${PFX}-cn" >/dev/null
docker network create "${PFX}-wn" >/dev/null
cleanup(){ docker rm -f ${PFX}-coord ${PFX}-worker >/dev/null 2>&1 || true
           docker network rm ${PFX}-cn ${PFX}-wn >/dev/null 2>&1 || true; }
trap cleanup EXIT

# --- coordinator container ---
docker run -d --name ${PFX}-coord --network ${PFX}-cn \
  -v /var/run/docker.sock:/var/run/docker.sock "$IMG" sleep infinity >/dev/null
# --- worker container (own docker.sock for its server's docker builder) ---
docker run -d --name ${PFX}-worker --network ${PFX}-wn \
  -v /var/run/docker.sock:/var/run/docker.sock "$IMG" sleep infinity >/dev/null

# coordinator: join mesh as "coordinator", start scheduler, expose scheduler over tsnet
docker exec ${PFX}-coord bash -lc "
  sed 's/__TOKEN__/$TOKEN/g' /dev/stdin > /run/scheduler.conf <<'EOF'
$(cat scheduler.conf)
EOF
  SCCACHE_NO_DAEMON=1 sccache-dist scheduler --config /run/scheduler.conf &
  sleep 1
  tsbridge -hostname ${PFX}-coordinator -authkey '$TS_AUTHKEY' -tags '$TAG' \
    -expose ':10600' -local-target 127.0.0.1:10600 \
    -listen 127.0.0.1:10501 -target ${PFX}-worker-1:10501 &
  sleep 8
"
# worker: join mesh as worker-1, bridge to scheduler, expose its server over tsnet, start server
docker exec ${PFX}-worker bash -lc "
  tsbridge -hostname ${PFX}-worker-1 -authkey '$TS_AUTHKEY' -tags '$TAG' \
    -expose ':10501' -local-target 127.0.0.1:10501 \
    -listen 127.0.0.1:10600 -target ${PFX}-coordinator:10600 &
  sleep 8
  cat > /run/server.conf <<EOF
$(sed -e "s|__TOKEN__|$TOKEN|g" -e 's|__SERVER_PUBLIC_ADDR__|127.0.0.1:10501|g' -e 's|__SCHEDULER_URL__|http://127.0.0.1:10600|g' server.conf)
EOF
  SCCACHE_NO_DAEMON=1 sccache-dist server --config /run/server.conf > /tmp/server.log 2>&1 &
  sleep 6
  echo '--- server log (look for register success vs mismatched_address) ---'
  tail -8 /tmp/server.log
"
# coordinator: configure client, check dist-status, do one distributed compile
docker exec ${PFX}-coord bash -lc "
  mkdir -p /root/.config/sccache
  cat > /root/.config/sccache/config <<EOF
$(sed -e "s|__TOKEN__|$TOKEN|g" -e 's|__SCHEDULER_URL__|http://127.0.0.1:10600|g' client.config)
EOF
  export SCCACHE_DIST_FALLBACK=0
  sccache --stop-server >/dev/null 2>&1 || true
  sccache --start-server
  echo '--- dist-status (num_servers should be 1) ---'
  sccache --dist-status
  echo 'int main(void){return 0;}' > /tmp/h.c
  sccache --zero-stats >/dev/null
  echo '--- compile (no fallback) ---'
  sccache gcc -c /tmp/h.c -o /tmp/h.o && echo COMPILE_OK
  sccache -s | grep -iE 'successful distributed|failed distributed'
"
```

- [ ] **Step 2: Run the spike**

Run: `cd ~/sccache-dist-action/spike/tsnet-conn && TS_AUTHKEY=<key> TAG=tag:ci bash run.sh 2>&1 | tail -30`
Expected (PASS): `num_servers: 1`, `COMPILE_OK`, `successful distributed compiles ... 1`, `failed distributed ... 0`, and NO `invalid_bearer_token_mismatched_address` in the server log.

- [ ] **Step 3: GATE — analyze result**

- If PASS: record in `spike/tsnet-conn/RESULT.md` ("connection model validated; public_addr=coordinator-local works over tsnet bridge"). Proceed to Milestone 1.
- If FAIL with `invalid_bearer_token_mismatched_address`: this is the anticipated risk. Try, in order: (a) set the server's `public_addr` to its **tailnet IP** and have the coordinator forward to that instead of using a coordinator-local addr; (b) front the scheduler so `X-Real-IP` matches; (c) patch the fork's scheduler IP check (we own the fork). Document which fix worked in `RESULT.md` before proceeding — the chosen fix changes Milestone 4's coordinator forwarding.

- [ ] **Step 4: Commit the spike + result**

```bash
cd ~/sccache-dist-action
git add spike/tsnet-conn/
git commit -m "spike(conn): validate sccache-dist over tsnet (X-Real-IP gate) + RESULT"
```

---

## Milestone 1 — Repo scaffold + Go module + tsmesh (verbatim port)

### Task 1.1: go.mod + tsmesh port

**Files:**
- Create: `go.mod`
- Create: `internal/tsmesh/tsmesh.go`
- Create: `internal/tsmesh/tsmesh_test.go`

- [ ] **Step 1: go.mod**

```
module github.com/xdqi/sccache-dist-action

go 1.26
```

- [ ] **Step 2: Copy tsmesh.go verbatim from distcc-action**

Run: `cp /home/kosaka/distcc-action/internal/tsmesh/tsmesh.go ~/sccache-dist-action/internal/tsmesh/tsmesh.go`
(It has no distcc-specific logic — pure mesh. No edits needed.)

- [ ] **Step 3: Write a test for the pure helpers (FilterOnline/PresentOnline/workersReady)**

```go
package tsmesh

import "testing"

func TestFilterOnline(t *testing.T) {
	peers := []Peer{
		{Host: "r1-worker-1", Online: true},
		{Host: "r1-worker-2", Online: false},
		{Host: "r1-coordinator", Online: true},
	}
	got := FilterOnline(peers, "r1-worker-")
	if len(got) != 1 || got[0].Host != "r1-worker-1" {
		t.Fatalf("got %+v", got)
	}
}

func TestWorkersReady(t *testing.T) {
	if workersReady(1, 2, 1, false) { t.Fatal("before timeout needs full expected") }
	if !workersReady(2, 2, 1, false) { t.Fatal("full expected ready") }
	if !workersReady(1, 2, 1, true) { t.Fatal("after timeout min suffices") }
	if workersReady(0, 2, 1, true) { t.Fatal("below min not ready") }
}
```

- [ ] **Step 4: Run + tidy + test**

Run: `cd ~/sccache-dist-action && go mod tidy && go test ./internal/tsmesh/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum internal/tsmesh
git commit -m "scaffold: go module + tsmesh (ported from distcc-action)"
```

### Task 1.2: config (sccache-flavored)

**Files:**
- Create: `internal/config/config.go`
- Create: `internal/config/config_test.go`

- [ ] **Step 1: Write config.go**

```go
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Mode            string
	OAuthSecret     string
	Tags            string
	RunPrefix       string
	ExpectedWorkers int
	MinWorkers      int
	WaitTimeout     time.Duration
	Slots           int
	PollInterval    time.Duration
	TeardownThresh  int
	DistFallback    bool
}

func Load() (*Config, error) { return loadFrom(os.Getenv) }

func loadFrom(get func(string) string) (*Config, error) {
	c := &Config{
		Mode:           get("INPUT_MODE"),
		OAuthSecret:    get("INPUT_OAUTH_SECRET"),
		Tags:           orDefault(get("INPUT_TAGS"), "tag:ci-sccache"),
		RunPrefix:      orDefault(get("INPUT_RUN_PREFIX"), get("GITHUB_RUN_ID")),
		Slots:          atoiOr(get("INPUT_SLOTS"), 0),
		PollInterval:   durOr(get("INPUT_POLL_INTERVAL"), time.Second),
		TeardownThresh: atoiOr(get("INPUT_TEARDOWN_THRESHOLD"), 5),
		WaitTimeout:    durOr(get("INPUT_WAIT_TIMEOUT"), 300*time.Second),
		DistFallback:   boolOr(get("INPUT_DIST_FALLBACK"), true),
	}
	if c.Mode != "coordinator" && c.Mode != "worker" {
		return nil, fmt.Errorf("mode must be coordinator|worker, got %q", c.Mode)
	}
	if c.OAuthSecret == "" {
		return nil, fmt.Errorf("oauth-secret is required")
	}
	if c.RunPrefix == "" {
		return nil, fmt.Errorf("run-prefix empty and GITHUB_RUN_ID unset")
	}
	if c.Mode == "coordinator" {
		c.ExpectedWorkers = atoiOr(get("INPUT_EXPECTED_WORKERS"), 0)
		if c.ExpectedWorkers < 1 {
			return nil, fmt.Errorf("expected-workers must be >= 1 for coordinator")
		}
		c.MinWorkers = atoiOr(get("INPUT_MIN_WORKERS"), c.ExpectedWorkers)
		if c.MinWorkers < 1 || c.MinWorkers > c.ExpectedWorkers {
			return nil, fmt.Errorf("min-workers must be in [1, expected-workers]")
		}
	}
	return c, nil
}

func orDefault(v, d string) string { if v == "" { return d }; return v }
func atoiOr(v string, d int) int { if v == "" { return d }; if n, err := strconv.Atoi(v); err == nil { return n }; return d }
func boolOr(v string, d bool) bool { if v == "" { return d }; if b, err := strconv.ParseBool(v); err == nil { return b }; return d }
func durOr(v string, d time.Duration) time.Duration {
	if v == "" { return d }
	if dur, err := time.ParseDuration(v); err == nil { return dur }
	if n, err := strconv.Atoi(v); err == nil { return time.Duration(n) * time.Second }
	return d
}
```

- [ ] **Step 2: Write config_test.go**

```go
package config

import "testing"

func TestLoadCoordinator(t *testing.T) {
	env := map[string]string{
		"INPUT_MODE": "coordinator", "INPUT_OAUTH_SECRET": "k",
		"INPUT_RUN_PREFIX": "r1", "INPUT_EXPECTED_WORKERS": "3",
	}
	c, err := loadFrom(func(k string) string { return env[k] })
	if err != nil { t.Fatal(err) }
	if c.ExpectedWorkers != 3 || c.MinWorkers != 3 || !c.DistFallback {
		t.Fatalf("got %+v", c)
	}
}

func TestLoadRejectsBadMode(t *testing.T) {
	_, err := loadFrom(func(k string) string { if k == "INPUT_MODE" { return "x" }; return "" })
	if err == nil { t.Fatal("expected error") }
}
```

- [ ] **Step 3: Run test**

Run: `cd ~/sccache-dist-action && go test ./internal/config/`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/config
git commit -m "config: INPUT_* parsing for sccache-dist-action"
```

---

## Milestone 2 — sccachedist engine layer

Replaces distcc-action's `distccrun`. Renders the scheduler/server/client confs,
starts the scheduler and server processes, computes J. The exact conf content
and the public_addr/forward strategy come from **Milestone 0's RESULT.md**.

### Task 2.1: sccachedist package

**Files:**
- Create: `internal/sccachedist/sccachedist.go`
- Create: `internal/sccachedist/sccachedist_test.go`

- [ ] **Step 1: Write sccachedist.go**

```go
package sccachedist

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

// Nproc is the default per-worker slot count.
func Nproc() int { return runtime.NumCPU() }

// TotalJ is the suggested -j for the coordinator build (sum of worker slots).
func TotalJ(numWorkers, slots int) int { return numWorkers * slots }

// SchedulerConf renders the scheduler config with the shared token.
func SchedulerConf(token string) string {
	return fmt.Sprintf(`public_addr = "0.0.0.0:10600"
[client_auth]
type = "token"
token = %q
[server_auth]
type = "token"
token = %q
`, token, token)
}

// ServerConf renders the server config. publicAddr is the address the CLIENT
// uses to reach this server (per Milestone 0 RESULT: coordinator-local
// 127.0.0.1:<port>); schedulerURL is reachable from the worker over tsnet.
func ServerConf(token, publicAddr, schedulerURL, cacheDir string) string {
	return fmt.Sprintf(`cache_dir = %q
public_addr = %q
bind_address = "0.0.0.0:10501"
scheduler_url = %q
[builder]
type = "docker"
[scheduler_auth]
type = "token"
token = %q
`, cacheDir, publicAddr, schedulerURL, token)
}

// ClientConfig renders ~/.config/sccache/config for the coordinator's client.
func ClientConfig(token, schedulerURL string) string {
	return fmt.Sprintf(`[dist]
scheduler_url = %q
toolchain_cache_size = 5368709120
[dist.auth]
type = "token"
token = %q
`, schedulerURL, token)
}

// StartScheduler launches `sccache-dist scheduler` in the foreground (caller
// keeps the *exec.Cmd to kill on teardown). Logs to stderr/stdout.
func StartScheduler(confPath string) (*exec.Cmd, error) {
	cmd := exec.Command("sccache-dist", "scheduler", "--config", confPath)
	cmd.Env = append(os.Environ(), "SCCACHE_NO_DAEMON=1")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start scheduler: %w", err)
	}
	return cmd, nil
}

// StartServer launches `sccache-dist server`.
func StartServer(confPath string) (*exec.Cmd, error) {
	cmd := exec.Command("sccache-dist", "server", "--config", confPath)
	cmd.Env = append(os.Environ(), "SCCACHE_NO_DAEMON=1")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start server: %w", err)
	}
	return cmd, nil
}

// WriteFile writes content to path (0644), creating parent dirs.
func WriteFile(path, content string) error {
	if err := os.MkdirAll(dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

func dir(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return "."
}
```

- [ ] **Step 2: Write sccachedist_test.go (pure renderers)**

```go
package sccachedist

import "strings"
import "testing"

func TestSchedulerConf(t *testing.T) {
	c := SchedulerConf("tok")
	if !strings.Contains(c, `token = "tok"`) || !strings.Contains(c, "0.0.0.0:10600") {
		t.Fatalf("bad conf:\n%s", c)
	}
}

func TestServerConf(t *testing.T) {
	c := ServerConf("tok", "127.0.0.1:10501", "http://127.0.0.1:10600", "/c")
	for _, want := range []string{`public_addr = "127.0.0.1:10501"`, `type = "docker"`, `scheduler_url = "http://127.0.0.1:10600"`} {
		if !strings.Contains(c, want) {
			t.Fatalf("missing %q in:\n%s", want, c)
		}
	}
}

func TestTotalJ(t *testing.T) {
	if TotalJ(3, 4) != 12 { t.Fatal("3*4=12") }
}
```

- [ ] **Step 3: Run test**

Run: `cd ~/sccache-dist-action && go test ./internal/sccachedist/`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/sccachedist
git commit -m "sccachedist: conf renderers + scheduler/server launchers + J"
```

> **NOTE:** If Milestone 0's RESULT.md chose the tailnet-IP `public_addr` variant
> instead of coordinator-local, adjust `ServerConf`'s `publicAddr` usage and the
> Task 4 forwarding accordingly. Keep the renderers faithful to the validated
> topology.

---

## Milestone 3 — worker role

### Task 3.1: worker.go (start server, expose over tsnet, bridge to scheduler, guard)

**Files:**
- Create: `internal/worker/worker.go`
- Create: `internal/worker/worker_test.go`

- [ ] **Step 1: Write worker.go**

```go
package worker

import (
	"context"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/xdqi/sccache-dist-action/internal/config"
	"github.com/xdqi/sccache-dist-action/internal/sccachedist"
	"github.com/xdqi/sccache-dist-action/internal/tsmesh"
)

type guard struct {
	threshold int
	misses    int
}

func (g *guard) observe(present bool) bool {
	if present {
		g.misses = 0
		return false
	}
	g.misses++
	return g.misses >= g.threshold
}

// Run executes the worker: join mesh, bridge to the coordinator's scheduler,
// start sccache-dist server, expose it over tsnet, then guard.
func Run(ctx context.Context, c *config.Config, hostname string) error {
	mesh, err := tsmesh.Up(ctx, hostname, c.OAuthSecret, c.Tags)
	if err != nil {
		return err
	}
	defer mesh.Close()

	coordHost := c.RunPrefix + "-coordinator"

	// Bridge local 127.0.0.1:10600 -> coordinator scheduler over tsnet, so the
	// server's scheduler_url=http://127.0.0.1:10600 reaches the real scheduler.
	schedLn, err := listenLocal("127.0.0.1:10600")
	if err != nil {
		return err
	}
	go forward(schedLn, func() (closer, error) {
		return mesh.Dial(context.Background(), coordHost+":10600")
	})

	// Expose the local server (:10501) over tsnet so the coordinator's forward
	// bridge can reach it.
	exposeLn, err := mesh.Listen(":10501")
	if err != nil {
		return err
	}
	go forward(exposeLn, func() (closer, error) {
		return dialLocal("127.0.0.1:10501")
	})

	// Wait until the coordinator (hence scheduler) is online before starting the
	// server, so the server's first heartbeat lands.
	for {
		peers, _ := mesh.Peers(ctx)
		if tsmesh.PresentOnline(peers, coordHost) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.PollInterval):
		}
	}

	// public_addr per Milestone 0 RESULT (coordinator-local 127.0.0.1:<port>).
	// The coordinator forwards a unique local port per worker; the worker is told
	// its assigned port via INPUT (worker-index -> port). Default: 10501 + idx.
	conf := sccachedist.ServerConf(token(c), serverPublicAddr(c), "http://127.0.0.1:10600", "/var/lib/sccache-dist/cache")
	if err := sccachedist.WriteFile("/run/server.conf", conf); err != nil {
		return err
	}
	srv, err := sccachedist.StartServer("/run/server.conf")
	if err != nil {
		return err
	}
	defer srv.Process.Kill()
	log.Printf("[worker] %s server up, guarding coordinator %s", hostname, coordHost)

	g := &guard{threshold: c.TeardownThresh}
	for {
		peers, _ := mesh.Peers(ctx)
		if g.observe(tsmesh.PresentOnline(peers, coordHost)) {
			log.Printf("[worker] coordinator gone %dx -> exiting", c.TeardownThresh)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.PollInterval):
		}
	}
}

// token derives the shared sccache-dist token from the OAuth secret (reuse it,
// per the design: the token is an arbitrary shared string and the tailnet is
// already authenticated).
func token(c *config.Config) string { return c.OAuthSecret }

// serverPublicAddr returns the coordinator-local address the client uses to
// reach this worker's server. MUST match the coordinator's per-worker forward
// port. Both sides derive it from worker-index: 10501 + (idx-1).
func serverPublicAddr(c *config.Config) string {
	return fmt.Sprintf("127.0.0.1:%d", 10501) // single-port baseline; see Task 4 note
}
```

> NOTE: `listenLocal`, `dialLocal`, `forward`, `closer` are tiny net helpers.
> Define them in `internal/worker/net.go` (next step). The per-worker unique
> port mapping (10501+idx) is finalized in Task 4 where the coordinator side is
> written; keep both sides reading the same formula.

- [ ] **Step 2: Write internal/worker/net.go (local forward helpers)**

```go
package worker

import (
	"io"
	"net"
)

type closer interface {
	io.ReadWriteCloser
}

func listenLocal(addr string) (net.Listener, error) { return net.Listen("tcp", addr) }
func dialLocal(addr string) (closer, error)         { return net.Dial("tcp", addr) }

func forward(ln net.Listener, dial func() (closer, error)) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			r, err := dial()
			if err != nil {
				return
			}
			defer r.Close()
			go io.Copy(r, c)
			io.Copy(c, r)
		}()
	}
}
```

- [ ] **Step 3: Write worker_test.go (guard logic)**

```go
package worker

import "testing"

func TestGuard(t *testing.T) {
	g := &guard{threshold: 3}
	if g.observe(true) { t.Fatal("present: no exit") }
	if g.observe(false) || g.observe(false) { t.Fatal("2 misses < 3") }
	if !g.observe(false) { t.Fatal("3rd miss -> exit") }
	g.observe(true) // reset
	if g.observe(false) { t.Fatal("reset then 1 miss < 3") }
}
```

- [ ] **Step 4: Run test (compile + guard)**

Run: `cd ~/sccache-dist-action && go test ./internal/worker/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/worker
git commit -m "worker: sccache-dist server + tsnet bridges + guard loop"
```

---

## Milestone 4 — coordinator role (scheduler + wait + forward + export)

### Task 4.1: coordinator.go

**Files:**
- Create: `internal/coordinator/coordinator.go`

- [ ] **Step 1: Write coordinator.go**

```go
package coordinator

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/xdqi/sccache-dist-action/internal/config"
	"github.com/xdqi/sccache-dist-action/internal/sccachedist"
	"github.com/xdqi/sccache-dist-action/internal/tsmesh"
)

// Run executes the coordinator: join mesh, start scheduler, wait for workers,
// forward a local port to each worker's server over tsnet, write the sccache
// client config, export env, then block until job end.
func Run(ctx context.Context, c *config.Config, hostname string) error {
	mesh, err := tsmesh.Up(ctx, hostname, c.OAuthSecret, c.Tags)
	if err != nil {
		return err
	}
	// Do NOT close mesh: the forwards + scheduler stay alive until job end.

	token := c.OAuthSecret

	// Start the scheduler locally (listens 0.0.0.0:10600; workers reach it via
	// their tsnet bridge to <coord>:10600).
	if err := sccachedist.WriteFile("/run/scheduler.conf", sccachedist.SchedulerConf(token)); err != nil {
		return err
	}
	// Expose the scheduler over tsnet so workers' bridges can dial it.
	schedExpose, err := mesh.Listen(":10600")
	if err != nil {
		return err
	}
	go acceptForward(schedExpose, func() (net.Conn, error) { return net.Dial("tcp", "127.0.0.1:10600") })
	if _, err := sccachedist.StartScheduler("/run/scheduler.conf"); err != nil {
		return err
	}

	// Wait for workers.
	prefix := c.RunPrefix + "-worker-"
	waitCtx, cancel := context.WithTimeout(ctx, c.WaitTimeout)
	defer cancel()
	online, _ := mesh.WaitForWorkers(waitCtx, prefix, c.ExpectedWorkers, c.MinWorkers, c.PollInterval)
	if len(online) < c.MinWorkers {
		return fmt.Errorf("only %d/%d workers online", len(online), c.MinWorkers)
	}
	log.Printf("[coord] %d/%d workers online", len(online), c.ExpectedWorkers)

	slots := c.Slots
	if slots == 0 {
		slots = sccachedist.Nproc()
	}

	// Forward a local port per worker -> worker server over tsnet. The port MUST
	// match the worker's serverPublicAddr formula. Baseline single-worker uses
	// 10501; multi-worker uses 10501+i and each worker is told its index.
	for i, w := range online {
		lp := 10501 + i
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", lp))
		if err != nil {
			return err
		}
		target := fmt.Sprintf("%s:10501", w.Host) // dial worker by hostname over tsnet
		go acceptForward(ln, func() (net.Conn, error) {
			return mesh.Dial(context.Background(), target)
		})
		log.Printf("[coord] forward 127.0.0.1:%d -> tsnet %s", lp, target)
	}

	// Write the client config (scheduler is local).
	home, _ := os.UserHomeDir()
	cfgPath := filepath.Join(home, ".config", "sccache", "config")
	if err := sccachedist.WriteFile(cfgPath, sccachedist.ClientConfig(token, "http://127.0.0.1:10600")); err != nil {
		return err
	}

	// Export env for the build step.
	j := sccachedist.TotalJ(len(online), slots)
	if ge := os.Getenv("GITHUB_ENV"); ge != "" {
		f, err := os.OpenFile(ge, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "SCCACHE_J=%d\n", j)
			fmt.Fprintf(f, "SCCACHE_WORKERS_ONLINE=%d\n", len(online))
			fmt.Fprintf(f, "SCCACHE_DIR=%s\n", filepath.Join(home, ".cache", "sccache"))
			fmt.Fprintf(f, "SCCACHE_DIST_FALLBACK=%s\n", boolStr(c.DistFallback))
			f.Close()
		}
	}
	if go_ := os.Getenv("GITHUB_OUTPUT"); go_ != "" {
		f, err := os.OpenFile(go_, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "workers-online=%d\n", len(online))
			fmt.Fprintf(f, "sccache-j=%d\n", j)
			fmt.Fprintf(f, "scheduler-url=http://127.0.0.1:10600\n")
			f.Close()
		}
	}
	log.Printf("[coord] exported SCCACHE_J=%d, %d workers; client config at %s", j, len(online), cfgPath)
	_ = strconv.Itoa // keep import if unused after edits
	_ = time.Second
	return nil
}

func acceptForward(ln net.Listener, dial func() (net.Conn, error)) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			r, err := dial()
			if err != nil {
				return
			}
			defer r.Close()
			go io.Copy(r, c)
			io.Copy(c, r)
		}()
	}
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
```

- [ ] **Step 2: Build (compile check)**

Run: `cd ~/sccache-dist-action && go build ./...`
Expected: builds clean. (Remove the `_ = strconv.Itoa`/`_ = time.Second` keepalives if those imports end up used; otherwise drop the imports.)

- [ ] **Step 3: Commit**

```bash
git add internal/coordinator
git commit -m "coordinator: scheduler + worker wait + per-worker tsnet forward + env export"
```

> **NOTE (multi-worker port mapping):** worker `serverPublicAddr` must equal the
> coordinator's per-worker forward port. Pass the index to the worker (it already
> gets `INPUT_WORKER_INDEX`) and compute `10501 + (idx-1)` on BOTH sides. Update
> `internal/worker/worker.go:serverPublicAddr` to read `INPUT_WORKER_INDEX`
> accordingly when moving past the single-worker baseline.

---

## Milestone 5 — main entrypoint + procctl (detached forwarder)

### Task 5.1: main.go + procctl.go (ported, renamed)

**Files:**
- Create: `cmd/sccache-dist-action/main.go`
- Create: `cmd/sccache-dist-action/procctl.go`

- [ ] **Step 1: Write main.go**

```go
package main

import (
	"context"
	"log"
	"os"

	"github.com/xdqi/sccache-dist-action/internal/config"
	"github.com/xdqi/sccache-dist-action/internal/coordinator"
	"github.com/xdqi/sccache-dist-action/internal/worker"
)

func main() {
	if phaseFromArgs(os.Args) == "teardown" {
		if pid := readPid(); pid > 0 {
			_ = killPid(pid)
			log.Printf("[teardown] signaled forwarder pid=%d", pid)
		}
		return
	}

	c, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	ctx := context.Background()

	hostname := c.RunPrefix + "-" + c.Mode
	if c.Mode == "worker" {
		idx := os.Getenv("INPUT_WORKER_INDEX")
		hostname = c.RunPrefix + "-worker-" + idx
	}

	if c.Mode == "worker" {
		if err := worker.Run(ctx, c, hostname); err != nil {
			log.Fatalf("worker: %v", err)
		}
		return
	}

	// Coordinator: detached-forwarder pattern (so scheduler + forwards survive
	// the step returning, until job end).
	if os.Getenv("SCCACHE_ACTION_FORWARDER") == "1" {
		if err := coordinator.Run(ctx, c, hostname); err != nil {
			log.Fatalf("coordinator: %v", err)
		}
		select {} // block until the runner kills us at job end
	}

	pid, err := spawnForwarder()
	if err != nil {
		log.Fatalf("spawn forwarder: %v", err)
	}
	writePid(pid)
	if !waitForEnvExport() {
		log.Fatalf("forwarder did not export SCCACHE_J")
	}
	log.Printf("[coord] forwarder detached pid=%d; env exported", pid)
}

func phaseFromArgs(args []string) string {
	for _, a := range args[1:] {
		if a == "--teardown" {
			return "teardown"
		}
	}
	return "main"
}
```

- [ ] **Step 2: Write procctl.go (ported; rename DISTCC_HOSTS check -> SCCACHE_J, env var name)**

```go
package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const pidFile = "/tmp/sccache-action-forwarder.pid"

func spawnForwarder() (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	var env []string
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "RUNNER_TRACKING_ID=") {
			continue // let it outlive the step
		}
		env = append(env, e)
	}
	env = append(env, "SCCACHE_ACTION_FORWARDER=1")
	cmd := exec.Command(exe)
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	return cmd.Process.Pid, nil
}

func writePid(pid int) { _ = os.WriteFile(pidFile, []byte(strconv.Itoa(pid)), 0o644) }
func readPid() int {
	b, err := os.ReadFile(pidFile)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}
func killPid(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }

func waitForEnvExport() bool {
	ge := os.Getenv("GITHUB_ENV")
	if ge == "" {
		return true
	}
	deadline := time.Now().Add(6 * time.Minute)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(ge); err == nil && strings.Contains(string(b), "SCCACHE_J=") {
			return true
		}
		time.Sleep(time.Second)
	}
	return false
}
```

- [ ] **Step 3: Build the whole binary**

Run: `cd ~/sccache-dist-action && CGO_ENABLED=0 go build -o /tmp/sda ./cmd/sccache-dist-action && echo BUILT`
Expected: `BUILT`.

- [ ] **Step 4: Commit**

```bash
git add cmd/sccache-dist-action
git commit -m "cmd: main mode-dispatch + detached forwarder (ported from distcc-action)"
```

---

## Milestone 6 — action.yml + JS wrapper

### Task 6.1: action.yml

**Files:**
- Create: `action.yml`

- [ ] **Step 1: Write action.yml**

```yaml
name: 'sccache-dist Tailnet Farm'
description: 'Ephemeral sccache-dist compile farm over a Tailscale mesh on GitHub runners'
inputs:
  mode:           { description: 'coordinator | worker', required: true }
  oauth-secret:   { description: 'Tailscale OAuth client secret (used as authkey)', required: true }
  oauth-client-id:{ description: 'reserved; unused', required: false }
  expected-workers:{ description: 'workers to wait for (coordinator)', required: false }
  min-workers:    { description: 'min online to proceed (default expected)', required: false }
  wait-timeout:   { description: 'max wait for workers', required: false, default: '300s' }
  worker-index:   { description: 'unique per worker (matrix idx)', required: false }
  tags:           { description: 'tailnet tags', required: false, default: 'tag:ci-sccache' }
  run-prefix:     { description: 'hostname namespacing', required: false, default: '${{ github.run_id }}' }
  slots:          { description: 'per-worker concurrent jobs (0=nproc)', required: false, default: '0' }
  poll-interval:  { description: 'teardown poll interval', required: false, default: '1s' }
  teardown-threshold:{ description: 'offline reads before worker exits', required: false, default: '5' }
  dist-fallback:  { description: 'SCCACHE_DIST_FALLBACK (fall back to local on non-distributable)', required: false, default: 'true' }
  sccache-ref:    { description: 'forked sccache build to use', required: false, default: 'sccache-dist-poc-tweaks' }
  github-token:   { description: 'token for release download', required: false, default: '${{ github.token }}' }
outputs:
  workers-online: { description: 'participating workers' }
  sccache-j:      { description: 'suggested -j (sum of worker slots)' }
  scheduler-url:  { description: 'coordinator scheduler URL' }
runs:
  using: 'node24'
  main: 'dist/index.js'
```

- [ ] **Step 2: Commit**

```bash
git add action.yml
git commit -m "action.yml: inputs/outputs for sccache-dist-action"
```

### Task 6.2: JS wrapper (port distcc-action's src/index.js)

**Files:**
- Create: `package.json`
- Create: `src/index.js`

- [ ] **Step 1: package.json**

```json
{
  "name": "sccache-dist-action",
  "private": true,
  "scripts": { "build": "ncc build src/index.js -o dist --license licenses.txt" },
  "dependencies": {
    "@actions/core": "^1.10.1",
    "@actions/exec": "^1.1.1",
    "@actions/github": "^6.0.0",
    "@actions/tool-cache": "^2.0.1",
    "semver": "^7.6.0"
  },
  "devDependencies": { "@vercel/ncc": "^0.38.1" }
}
```

- [ ] **Step 2: Port src/index.js**

Run: `cp /home/kosaka/distcc-action/src/index.js ~/sccache-dist-action/src/index.js`

Then edit these specifics (the resolution logic is identical; only names/engine change):
- `TOOL`/asset name: `sccache-dist-action-${g}-${a}`.
- `INPUT_IDS`: replace distcc list with `['mode','oauth-client-id','oauth-secret','expected-workers','min-workers','wait-timeout','worker-index','tags','run-prefix','slots','poll-interval','teardown-threshold','dist-fallback','sccache-ref']`.
- Remove `ensureDistcc()` and its call (no apt install). ADD `ensureEngine()` that
  downloads the prebuilt `sccache` + `sccache-dist` release assets (named
  `sccache-${ref}-linux-amd64`, `sccache-dist-${ref}-linux-amd64`) into a dir on
  PATH (tool-cache), so the Go binary finds them. Call it before `resolveBinary`.
- Keep `buildFromSource` for `uses: ./` (build the Go binary; for the engine in
  local mode, expect the binaries already on PATH or build via the spike script).
- Keep the Windows guard (`core.setFailed` — POSIX only).

Concretely, `ensureEngine` (add near `ensureDistcc`'s old spot):
```javascript
async function ensureEngine(octokit, ref, token) {
  const a = goarch();
  const binDir = path.join(process.env.RUNNER_TEMP || '/tmp', 'sccache-engine');
  await io.mkdirP(binDir);
  for (const name of ['sccache', 'sccache-dist']) {
    const asset = `${name}-${ref}-linux-${a}`;
    const cached = tc.find(asset, ref);
    let exe;
    if (cached) { exe = path.join(cached, name); }
    else {
      const rel = await resolveRelease(octokit, ref); // engine assets ride the action release
      const found = rel.assets.find((x) => x.name === asset);
      const dl = await tc.downloadTool(found.browser_download_url, undefined, token ? `token ${token}` : undefined);
      const dir = await tc.cacheFile(dl, name, asset, ref);
      exe = path.join(dir, name);
    }
    await exec.exec('chmod', ['+x', exe]);
    await io.cp(exe, path.join(binDir, name));
  }
  core.addPath(binDir);
}
```
(Use whatever `resolveRelease`/`tc`/`io` the ported file already imports; add
`const io = require('@actions/io');` if missing.)

- [ ] **Step 3: Build the bundle**

Run: `cd ~/sccache-dist-action && npm install && npm run build`
Expected: `dist/index.js` produced.

- [ ] **Step 4: Commit**

```bash
git add package.json package-lock.json src/index.js dist/index.js
git commit -m "js-wrapper: resolve Go binary + download forked sccache engine (port)"
```

---

## Milestone 7 — CI: build engine binaries + Go binary + examples

### Task 7.1: engine.yml (build sccache/sccache-dist from the fork)

**Files:**
- Create: `.github/workflows/engine.yml`

- [ ] **Step 1: Write engine.yml**

```yaml
name: engine
on:
  workflow_dispatch:
    inputs:
      ref: { description: 'fork ref', default: 'sccache-dist-poc-tweaks' }
jobs:
  build-engine:
    runs-on: ubuntu-latest
    steps:
      - name: Build sccache + sccache-dist (static musl) from the fork
        run: |
          REF='${{ github.event.inputs.ref }}'
          git clone --depth 1 --branch "$REF" https://github.com/xdqi/sccache.git src
          docker run --rm -v "$PWD/src":/src -v "$PWD/out":/out -w /src rust:1-bookworm bash -c '
            apt-get update >/dev/null && apt-get install -y --no-install-recommends pkg-config perl make musl-tools musl-dev >/dev/null
            rustup target add x86_64-unknown-linux-musl
            cargo build --release --target x86_64-unknown-linux-musl --no-default-features --features="dist-client dist-server vendored-openssl"
            mkdir -p /out
            cp target/x86_64-unknown-linux-musl/release/sccache      /out/sccache-'"$REF"'-linux-amd64
            cp target/x86_64-unknown-linux-musl/release/sccache-dist /out/sccache-dist-'"$REF"'-linux-amd64
          '
      - uses: actions/upload-artifact@v4
        with: { name: engine, path: out/* }
```

(The release workflow attaches these `out/*` assets to the action's release. For
v1, manual `workflow_dispatch` + `gh release upload` is acceptable; document it.)

- [ ] **Step 2: Commit**

```bash
git add .github/workflows/engine.yml
git commit -m "ci(engine): build forked sccache/sccache-dist static musl"
```

### Task 7.2: release.yml (Go binary matrix) — port distcc-action's

**Files:**
- Create: `.github/workflows/release.yml`

- [ ] **Step 1: Copy + adapt distcc-action's release.yml**

Run: `cp /home/kosaka/distcc-action/.github/workflows/release.yml ~/sccache-dist-action/.github/workflows/release.yml`

Edit: build path `./cmd/sccache-dist-action`, asset name `sccache-dist-action-${GOOS}-${GOARCH}`, and add a step (before release upload) that downloads the `engine` artifact and uploads `sccache-*`/`sccache-dist-*` assets alongside the Go binaries.

- [ ] **Step 2: Commit**

```bash
git add .github/workflows/release.yml
git commit -m "ci(release): Go binary matrix + engine assets (port)"
```

### Task 7.3: selftest.yml (2 workers + redis, gcc, cache)

**Files:**
- Create: `.github/workflows/selftest.yml`

- [ ] **Step 1: Write selftest.yml**

```yaml
name: selftest
on: [workflow_dispatch]
jobs:
  workers:
    strategy:
      matrix: { idx: [1, 2] }
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
      - uses: actions/setup-go@v6
        with: { go-version: '1.26' }
      - uses: ./
        with:
          mode: worker
          worker-index: ${{ matrix.idx }}
          oauth-secret: ${{ secrets.TS_OAUTH_SECRET }}
          tags: tag:ci
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
      - uses: actions/setup-go@v6
        with: { go-version: '1.26' }
      - name: Cache sccache objects
        uses: actions/cache@v4
        with:
          path: ~/.cache/sccache
          key: sccache-${{ runner.os }}-redis-${{ github.run_id }}
          restore-keys: sccache-${{ runner.os }}-redis-
      - id: farm
        uses: ./
        with:
          mode: coordinator
          expected-workers: 2
          oauth-secret: ${{ secrets.TS_OAUTH_SECRET }}
          tags: tag:ci
      - name: Build redis via the farm
        run: |
          echo "SCCACHE_J=$SCCACHE_J workers=$SCCACHE_WORKERS_ONLINE"
          sccache --zero-stats || true
          git clone --depth 1 https://github.com/redis/redis
          cd redis
          make -j"${SCCACHE_J:-4}" CC="sccache gcc" MALLOC=libc CFLAGS="-Wno-error" || true
          echo "=== sccache -s ==="
          sccache -s
          # assert distribution happened
          sccache -s | grep -A1 "Successful distributed compiles" | grep -qE '[1-9]' \
            && echo "DISTRIBUTION CONFIRMED" || { echo "no distribution"; exit 1; }
```

- [ ] **Step 2: Commit**

```bash
git add .github/workflows/selftest.yml
git commit -m "ci(selftest): 2 workers + distributed redis (gcc) + actions/cache"
```

### Task 7.4: smoke.yml (3 workers + linux kernel, gcc, QEMU boot)

**Files:**
- Create: `.github/workflows/smoke.yml`

- [ ] **Step 1: Write smoke.yml**

```yaml
name: smoke
on: [workflow_dispatch]
jobs:
  workers:
    strategy:
      matrix: { idx: [1, 2, 3] }
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
      - uses: actions/setup-go@v6
        with: { go-version: '1.26' }
      - uses: ./
        with:
          mode: worker
          worker-index: ${{ matrix.idx }}
          oauth-secret: ${{ secrets.TS_OAUTH_SECRET }}
          tags: tag:ci
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
      - uses: actions/setup-go@v6
        with: { go-version: '1.26' }
      - name: Kernel build deps + qemu
        run: sudo apt-get update && sudo apt-get install -y flex bison libelf-dev bc libssl-dev qemu-system-x86
      - id: farm
        uses: ./
        with:
          mode: coordinator
          expected-workers: 3
          min-workers: 1
          oauth-secret: ${{ secrets.TS_OAUTH_SECRET }}
          tags: tag:ci
      - name: Build kernel via the farm
        run: |
          git clone --depth 1 https://github.com/torvalds/linux.git linux
          cd linux
          make tinyconfig
          ./scripts/config --enable CONFIG_PRINTK --enable CONFIG_TTY \
            --enable CONFIG_SERIAL_8250 --enable CONFIG_SERIAL_8250_CONSOLE --enable CONFIG_BINFMT_ELF
          make olddefconfig
          # C distributes; .S compiles locally (engine forces assembly local)
          make -j"${SCCACHE_J:-6}" CC="sccache gcc" bzImage
          test -f arch/x86/boot/bzImage
          sccache -s
      - name: Boot in QEMU
        working-directory: linux
        run: |
          set +e
          timeout 90 qemu-system-x86_64 -nographic -no-reboot \
            -kernel arch/x86/boot/bzImage -append "console=ttyS0 panic=-1" 2>&1 | tee boot.log
          set -e
          grep -q "Linux version" boot.log || { echo "no banner"; exit 1; }
          grep -Eq "VFS: Unable to mount root fs|No working init|Kernel panic" boot.log \
            && echo "SMOKE boot = PASS" || { echo "did not reach init"; exit 1; }
```

- [ ] **Step 2: Commit**

```bash
git add .github/workflows/smoke.yml
git commit -m "ci(smoke): 3 workers + distributed linux kernel (gcc) + QEMU boot"
```

---

## Milestone 8 — README + push

### Task 8.1: README + push to GitHub

**Files:**
- Create: `README.md`

- [ ] **Step 1: Write README** (mirror distcc-action's: quick start, how it works, Tailscale setup, inputs/outputs, the cache note, scope/limitations — single-coordinator, Linux workers only, gcc auto-package; cross-toolchain/zig cc available).

- [ ] **Step 2: Commit + create the GitHub repo + push**

```bash
cd ~/sccache-dist-action
git add README.md && git commit -m "docs: README"
gh repo create xdqi/sccache-dist-action --public --source=. --remote=origin --push
```

- [ ] **Step 3: Trigger engine.yml + release once, then run selftest against `uses: ./`**

Run (manual, on GitHub): dispatch `engine` to build the binaries, attach to a `v0.0.1` release; then dispatch `selftest`.
Expected: redis builds, `DISTRIBUTION CONFIRMED`, cache hits on a second run.

---

## Self-review notes (gaps to watch during execution)

- **Milestone 0 is load-bearing.** If the X-Real-IP check rejects the
  coordinator-local `public_addr`, Tasks 2.1/3.1/4.1's `public_addr`/forward
  details change per RESULT.md. Do not hardcode before the spike passes.
- **Multi-worker port mapping**: worker `serverPublicAddr` and coordinator forward
  port must use the identical `10501 + (idx-1)` formula reading `INPUT_WORKER_INDEX`.
  The single-worker baseline (10501) must be generalized in Task 4's NOTE before
  selftest's 2-worker run.
- **Engine on `uses: ./`** (local selftest): the JS wrapper's `ensureEngine`
  downloads release assets; for `uses: ./` with no release yet, the workflow must
  either build engine binaries first or the runner must already have them on PATH.
  selftest/smoke assume a published engine release; document the bootstrap order.
- **Token reuse**: the shared sccache-dist token = the Tailscale OAuth secret
  (arbitrary shared string; tailnet already authenticates). Consistent across
  scheduler/server/client renderers.
