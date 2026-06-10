package main

import (
	"context"
	"log"
	"os"
	"time"

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
	// The forwarder only falls back to min-workers when wait-timeout expires,
	// so the parent must outlive that deadline or the fallback is unreachable
	// (run 27257400910: 14/15 workers healthy, parent gave up at a fixed 6min
	// while the forwarder still had 4min of full-count wait left).
	if !waitForEnvExport(c.WaitTimeout + time.Minute) {
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
