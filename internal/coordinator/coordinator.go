package coordinator

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"

	"github.com/xdqi/sccache-dist-action/internal/config"
	"github.com/xdqi/sccache-dist-action/internal/sccachedist"
	"github.com/xdqi/sccache-dist-action/internal/tsmesh"
)

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

	// Forward a deterministic local port for EVERY expected worker, not just
	// the ones online right now: with the min-workers fallback a slow worker
	// can register with the scheduler after this point, and the scheduler then
	// hands jobs to its advertised 127.0.0.1:<port> — which must already have
	// a listener. Hostnames and ports are both index-derived, so no discovery
	// is needed.
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

	home, _ := os.UserHomeDir()
	cfgPath := filepath.Join(home, ".config", "sccache", "config")
	if err := sccachedist.WriteFile(cfgPath, sccachedist.ClientConfig(token, "http://127.0.0.1:10600")); err != nil {
		return err
	}

	j := sccachedist.TotalJ(len(online), slots)
	if ge := os.Getenv("GITHUB_ENV"); ge != "" {
		if f, err := os.OpenFile(ge, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			fmt.Fprintf(f, "SCCACHE_J=%d\n", j)
			fmt.Fprintf(f, "SCCACHE_WORKERS_ONLINE=%d\n", len(online))
			fmt.Fprintf(f, "SCCACHE_DIR=%s\n", filepath.Join(home, ".cache", "sccache"))
			fmt.Fprintf(f, "SCCACHE_DIST_FALLBACK=%s\n", boolStr(c.DistFallback))
			f.Close()
		}
	}
	if gout := os.Getenv("GITHUB_OUTPUT"); gout != "" {
		if f, err := os.OpenFile(gout, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			fmt.Fprintf(f, "workers-online=%d\n", len(online))
			fmt.Fprintf(f, "sccache-j=%d\n", j)
			fmt.Fprintf(f, "scheduler-url=http://127.0.0.1:10600\n")
			f.Close()
		}
	}
	log.Printf("[coord] exported SCCACHE_J=%d, %d workers; client config at %s", j, len(online), cfgPath)
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
