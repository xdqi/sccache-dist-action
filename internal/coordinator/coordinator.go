package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/xdqi/sccache-dist-action/internal/config"
	"github.com/xdqi/sccache-dist-action/internal/sccachedist"
	"github.com/xdqi/sccache-dist-action/internal/tsmesh"
)

// registeredServers asks the LOCAL scheduler how many build servers are
// registered — i.e. whose heartbeats arrive over real tsnet connections. This
// is ground truth for farm readiness: the tailnet netmap .Online flags used
// before lagged in BOTH directions (run 27287554600 — the coordinator counted
// 5/15 "online" while all 15 workers were registered and heartbeating, and the
// workers' guard simultaneously saw the coordinator "offline" and exited).
// The status endpoint is unauthenticated and serves JSON under this Accept.
func registeredServers() (int, error) {
	req, err := http.NewRequest("GET", "http://127.0.0.1:10600/api/v1/scheduler/status", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var st struct {
		NumServers int `json:"num_servers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return 0, err
	}
	return st.NumServers, nil
}

// waitForRegisteredServers polls the scheduler until `expected` servers have
// registered, or until ctx expires — after which the current count is returned
// (the caller applies the min-workers floor). Same wait semantics as the old
// tsmesh.WaitForWorkers, but counting real registrations instead of netmap
// presence flags.
func waitForRegisteredServers(ctx context.Context, expected int, poll time.Duration) int {
	for {
		n, err := registeredServers()
		if err == nil && n >= expected {
			return n
		}
		select {
		case <-ctx.Done():
			n, _ := registeredServers()
			return n
		case <-time.After(poll):
		}
	}
}

// forwardSpec maps a worker index to its coordinator-local forward port and
// tsnet target. The port MUST match the worker's self-assigned public_addr
// (127.0.0.1:10501 + idx - 1) and the target hostname the worker's index-derived
// tsnet name (<run_prefix>-worker-<idx>, passed here as prefix+idx); the worker
// server itself listens on tsnet :10501.
func forwardSpec(workerPrefix string, idx int) (int, string) {
	return 10501 + (idx - 1), fmt.Sprintf("%s%d:10501", workerPrefix, idx)
}

// Run executes the coordinator: join mesh, start scheduler, wait for workers,
// forward a local port per worker over tsnet, write the client config, export
// env, then return (the caller blocks until job end).
func Run(ctx context.Context, c *config.Config, hostname string) error {
	mesh, err := tsmesh.Up(ctx, hostname, c.OAuthSecret, c.Tags)
	if err != nil {
		return err
	}
	// NOTE: do NOT close mesh — scheduler + forwards live until job end.

	token := c.OAuthSecret

	schedConf := sccachedist.ConfPath("sccache-scheduler.conf")
	if err := sccachedist.WriteFile(schedConf, sccachedist.SchedulerConf(token)); err != nil {
		return err
	}
	// Expose the local scheduler over tsnet for workers' bridges.
	schedExpose, err := mesh.Listen(":10600")
	if err != nil {
		return fmt.Errorf("tsnet listen scheduler: %w", err)
	}
	go acceptForward(schedExpose, func() (net.Conn, error) { return net.Dial("tcp", "127.0.0.1:10600") })
	if _, err := sccachedist.StartScheduler(schedConf, c.ServerLog); err != nil {
		return err
	}

	prefix := c.RunPrefix + "-worker-"

	// Forward a deterministic local port for EVERY expected worker, before
	// waiting: a worker that registers with the scheduler at any point must
	// already have its forward listener, or the scheduler hands jobs to its
	// advertised 127.0.0.1:<port> and they all fail. Hostnames and ports are
	// both index-derived, so no discovery is needed.
	for idx := 1; idx <= c.ExpectedWorkers; idx++ {
		lp, target := forwardSpec(prefix, idx)
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", lp))
		if err != nil {
			return fmt.Errorf("listen forward %d: %w", lp, err)
		}
		go acceptForward(ln, func() (net.Conn, error) {
			return mesh.Dial(context.Background(), target)
		})
		log.Printf("[coord] forward 127.0.0.1:%d -> tsnet %s", lp, target)
	}

	waitCtx, cancel := context.WithTimeout(ctx, c.WaitTimeout)
	defer cancel()
	registered := waitForRegisteredServers(waitCtx, c.ExpectedWorkers, c.PollInterval)
	if registered < c.MinWorkers {
		return fmt.Errorf("only %d/%d workers registered with the scheduler", registered, c.MinWorkers)
	}
	log.Printf("[coord] %d/%d workers registered", registered, c.ExpectedWorkers)

	slots := c.Slots
	if slots == 0 {
		slots = sccachedist.Nproc()
	}

	home, _ := os.UserHomeDir()
	cfgPath := filepath.Join(home, ".config", "sccache", "config")
	if err := sccachedist.WriteFile(cfgPath, sccachedist.ClientConfig(token, "http://127.0.0.1:10600")); err != nil {
		return err
	}

	j := sccachedist.TotalJ(registered, slots)
	if ge := os.Getenv("GITHUB_ENV"); ge != "" {
		if f, err := os.OpenFile(ge, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			fmt.Fprintf(f, "SCCACHE_J=%d\n", j)
			fmt.Fprintf(f, "SCCACHE_WORKERS_ONLINE=%d\n", registered)
			fmt.Fprintf(f, "SCCACHE_DIR=%s\n", filepath.Join(home, ".cache", "sccache"))
			fmt.Fprintf(f, "SCCACHE_DIST_FALLBACK=%s\n", boolStr(c.DistFallback))
			f.Close()
		}
	}
	if gout := os.Getenv("GITHUB_OUTPUT"); gout != "" {
		if f, err := os.OpenFile(gout, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			fmt.Fprintf(f, "workers-online=%d\n", registered)
			fmt.Fprintf(f, "sccache-j=%d\n", j)
			fmt.Fprintf(f, "scheduler-url=http://127.0.0.1:10600\n")
			f.Close()
		}
	}
	log.Printf("[coord] exported SCCACHE_J=%d, %d workers; client config at %s", j, registered, cfgPath)
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
