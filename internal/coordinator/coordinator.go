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
	"strings"

	"github.com/xdqi/sccache-dist-action/internal/config"
	"github.com/xdqi/sccache-dist-action/internal/sccachedist"
	"github.com/xdqi/sccache-dist-action/internal/tsmesh"
)

// workerPort maps a worker hostname (<run_prefix>-worker-<idx>) to the
// coordinator-local forward port that MUST match the worker's self-assigned
// public_addr: 10501 + (idx - 1). Falls back to 10501 if the suffix isn't a
// number.
func workerPort(host string) int {
	idx := 1
	if i := strings.LastIndex(host, "-"); i >= 0 {
		if n, err := strconv.Atoi(host[i+1:]); err == nil && n >= 1 {
			idx = n
		}
	}
	return 10501 + (idx - 1)
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
	if _, err := sccachedist.StartScheduler(schedConf); err != nil {
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

	for _, w := range online {
		lp := workerPort(w.Host)
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", lp))
		if err != nil {
			return fmt.Errorf("listen forward %d: %w", lp, err)
		}
		target := w.Host + ":10501" // worker server exposed on tsnet :10501
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
