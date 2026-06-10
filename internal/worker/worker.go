package worker

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
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

// workerIndex reads INPUT_WORKER_INDEX (1-based); defaults to 1 if unset/bad.
func workerIndex() int {
	if n, err := strconv.Atoi(os.Getenv("INPUT_WORKER_INDEX")); err == nil && n >= 1 {
		return n
	}
	return 1
}

// serverPublicAddr is the coordinator-local address the client uses to reach
// THIS worker's server. Must match the coordinator's per-worker forward port:
// 10501 + (index - 1).
func serverPublicAddr(idx int) string {
	return fmt.Sprintf("127.0.0.1:%d", 10501+(idx-1))
}

// Run executes the worker: join mesh, bridge to the coordinator's scheduler,
// expose the server over tsnet, start sccache-dist server, then guard.
func Run(ctx context.Context, c *config.Config, hostname string) error {
	mesh, err := tsmesh.Up(ctx, hostname, c.OAuthSecret, c.Tags)
	if err != nil {
		return err
	}
	defer mesh.Close()

	coordHost := c.RunPrefix + "-coordinator"
	idx := workerIndex()

	// Bridge local 127.0.0.1:10600 -> coordinator scheduler over tsnet.
	schedLn, err := net.Listen("tcp", "127.0.0.1:10600")
	if err != nil {
		return fmt.Errorf("listen scheduler bridge: %w", err)
	}
	go forward(schedLn, func() (net.Conn, error) {
		return mesh.Dial(context.Background(), coordHost+":10600")
	})

	// Expose the local server (:10501) over tsnet so the coordinator's forward
	// bridge can reach it.
	exposeLn, err := mesh.Listen(":10501")
	if err != nil {
		return fmt.Errorf("tsnet listen server: %w", err)
	}
	go forward(exposeLn, func() (net.Conn, error) {
		return net.Dial("tcp", "127.0.0.1:10501")
	})

	// Wait until the coordinator's scheduler is actually reachable before
	// starting the server, so the server's first heartbeat lands. This is a
	// REAL tsnet dial on the same path heartbeats use — the netmap .Online
	// flag used before lags in both directions (run 27287554600: workers
	// "saw" the coordinator offline and exited while heartbeats flowed).
	log.Printf("[worker] %s up (idx=%d), waiting for coordinator %s", hostname, idx, coordHost)
	for !probeCoordinator(mesh, coordHost) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.PollInterval):
		}
	}

	cacheDir := sccachedist.ConfPath("sccache-dist-server-cache")
	conf := sccachedist.ServerConf(c.OAuthSecret, serverPublicAddr(idx),
		"http://127.0.0.1:10600", cacheDir)
	confPath := sccachedist.ConfPath("sccache-server.conf")
	if err := sccachedist.WriteFile(confPath, conf); err != nil {
		return err
	}
	srv, err := sccachedist.StartServer(confPath, c.ServerLog)
	if err != nil {
		return err
	}
	defer srv.Process.Kill()
	log.Printf("[worker] server up (public_addr=%s), guarding coordinator", serverPublicAddr(idx))

	g := &guard{threshold: c.TeardownThresh}
	for {
		if g.observe(probeCoordinator(mesh, coordHost)) {
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

// probeCoordinator reports whether the coordinator's scheduler port accepts a
// connection over the tailnet RIGHT NOW — a real dial on the very path the
// server's heartbeats use, immune to netmap presence-flag lag. A failed dial
// costs its own timeout, so consecutive guard misses are spaced by real
// unreachability, not by a flag going stale.
func probeCoordinator(mesh *tsmesh.Mesh, coordHost string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := mesh.Dial(ctx, coordHost+":10600")
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
