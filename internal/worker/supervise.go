package worker

import (
	"context"
	"crypto/tls"
	"log"
	"net/http"
	"os/exec"
	"sync/atomic"
	"time"
)

// supervisor keeps this worker's sccache-dist server answering. The server's
// HTTPS front end (tiny_http) does the TLS handshake in its only accept
// thread with no timeout, so one stuck connection stops it from accepting
// anything while its separate heartbeat thread keeps it registered with the
// scheduler, which then goes on assigning jobs that each wait out a 30 s
// timeout (anyfs-reader CI, 2026-10-06). A server process that exits is just
// as invisible: the scheduler only drops it 90 s after its last heartbeat.
//
// The supervisor probes the server directly on loopback, never over tsnet, so
// a stalled tailnet path can't trigger it. It restarts the server when it
// exits or stops answering; the new process registers with a fresh nonce,
// which also makes the scheduler drop any jobs it had leaked onto the old
// one. It never ends the worker: leaving is the guard's call, made only when
// the coordinator is gone.
type supervisor struct {
	start     func() (*exec.Cmd, error)
	probe     func(ctx context.Context) error
	interval  time.Duration // between probes
	threshold int           // consecutive failed probes before a restart
	minUptime time.Duration // restart a crashing server no faster than this

	cmd      *exec.Cmd
	exited   chan error
	launched time.Time
	restarts atomic.Int64
}

// launch starts a server and watches for its exit.
func (s *supervisor) launch() error {
	s.launched = time.Now()
	cmd, err := s.start()
	if err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	s.cmd, s.exited = cmd, exited
	return nil
}

// kill stops the current server, if any, and reaps it.
func (s *supervisor) kill() {
	if s.cmd == nil {
		return
	}
	s.cmd.Process.Kill()
	<-s.exited
	s.cmd, s.exited = nil, nil
}

// run supervises the launched server until ctx ends, then kills it.
func (s *supervisor) run(ctx context.Context) {
	defer s.kill()
	misses := 0
	for {
		select {
		case <-ctx.Done():
			return
		case err := <-s.exited:
			s.cmd, s.exited = nil, nil
			log.Printf("[worker] sccache-dist server exited (%v); restarting it", err)
		case <-time.After(s.interval):
			err := s.probe(ctx)
			if ctx.Err() != nil {
				return
			}
			if err == nil {
				misses = 0
				continue
			}
			misses++
			log.Printf("[worker] server probe failed (%d/%d): %v", misses, s.threshold, err)
			if misses < s.threshold {
				continue
			}
			log.Printf("[worker] sccache-dist server stopped answering; restarting it")
		}
		misses = 0
		s.restart(ctx)
	}
}

// restart replaces the server, retrying a failed start, until ctx ends.
func (s *supervisor) restart(ctx context.Context) {
	s.kill()
	for {
		if wait := s.minUptime - time.Since(s.launched); wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
		err := s.launch()
		if err == nil {
			break
		}
		log.Printf("[worker] starting sccache-dist server failed: %v", err)
	}
	log.Printf("[worker] sccache-dist server restarted (restart #%d)", s.restarts.Add(1))
}

// probeClient talks to the local server, whose certificate is self-signed
// for its coordinator-side public_addr.
var probeClient = &http.Client{Transport: &http.Transport{
	TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
	DisableKeepAlives: true,
}}

// probeServer asks the server at addr for something it must refuse: an
// assign_job without a job token (401, not logged by the server). Any HTTP
// answer proves both the accept thread, which does the TLS handshake, and a
// request handler are free.
func probeServer(ctx context.Context, addr string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", "https://"+addr+"/api/v1/distserver/assign_job/0", nil)
	if err != nil {
		return err
	}
	resp, err := probeClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}
