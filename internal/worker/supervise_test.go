package worker

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeServer records every process the supervisor starts.
type fakeServer struct {
	mu    sync.Mutex
	cmds  []*exec.Cmd
	argvs [][]string // argv per launch; the last entry repeats once used up
}

func (f *fakeServer) start() (*exec.Cmd, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	argv := f.argvs[min(len(f.cmds), len(f.argvs)-1)]
	cmd := exec.Command(argv[0], argv[1:]...)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	f.cmds = append(f.cmds, cmd)
	return cmd, nil
}

func (f *fakeServer) launches() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.cmds)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// runSupervisor launches s and runs it until the returned stop is called;
// stop returns once run has returned.
func runSupervisor(t *testing.T, s *supervisor) (stop func()) {
	t.Helper()
	if err := s.launch(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.run(ctx); close(done) }()
	return func() { cancel(); <-done }
}

func TestSupervisorRestartsUnresponsiveServer(t *testing.T) {
	f := &fakeServer{argvs: [][]string{{"sleep", "60"}}}
	var probes atomic.Int64
	s := &supervisor{
		start: f.start,
		probe: func(context.Context) error {
			probes.Add(1)
			return errors.New("no answer")
		},
		interval:  5 * time.Millisecond,
		threshold: 3,
	}
	stop := runSupervisor(t, s)
	waitFor(t, "a restart", func() bool { return f.launches() >= 2 })
	stop()
	if probes.Load() < 3 {
		t.Fatalf("restarted after %d probes, want >= 3", probes.Load())
	}
	if f.cmds[0].ProcessState == nil {
		t.Fatal("the unresponsive server was not reaped before its replacement")
	}
}

func TestSupervisorKeepsAnsweringServer(t *testing.T) {
	f := &fakeServer{argvs: [][]string{{"sleep", "60"}}}
	var probes atomic.Int64
	s := &supervisor{
		start: f.start,
		probe: func(context.Context) error {
			// fail twice in a row, then answer: below the threshold
			if probes.Add(1)%3 == 0 {
				return nil
			}
			return errors.New("slow")
		},
		interval:  5 * time.Millisecond,
		threshold: 3,
	}
	stop := runSupervisor(t, s)
	waitFor(t, "probes", func() bool { return probes.Load() >= 12 })
	stop()
	if n := f.launches(); n != 1 {
		t.Fatalf("%d launches, want 1: misses below the threshold must not restart", n)
	}
}

func TestSupervisorRestartsExitedServer(t *testing.T) {
	f := &fakeServer{argvs: [][]string{{"true"}, {"sleep", "60"}}}
	s := &supervisor{
		start:     f.start,
		probe:     func(context.Context) error { return nil },
		interval:  time.Hour, // only the exit can trigger the restart
		threshold: 3,
	}
	s.minUptime = 10 * time.Millisecond
	stop := runSupervisor(t, s)
	waitFor(t, "a restart", func() bool { return f.launches() >= 2 })
	stop()
}

// Leaving is the guard's call; stopping the supervisor must kill whichever
// server is current, not just the first one.
func TestSupervisorStopKillsCurrentServer(t *testing.T) {
	f := &fakeServer{argvs: [][]string{{"sleep", "60"}}}
	s := &supervisor{
		start:     f.start,
		probe:     func(context.Context) error { return errors.New("no answer") },
		interval:  5 * time.Millisecond,
		threshold: 1,
	}
	stop := runSupervisor(t, s)
	waitFor(t, "a restart", func() bool { return f.launches() >= 2 })
	stop()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, cmd := range f.cmds {
		if cmd.ProcessState == nil {
			t.Fatalf("server %d still running after stop", i)
		}
	}
}

func TestProbeServerAnswered(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	if err := probeServer(context.Background(), srv.Listener.Addr().String(), time.Second); err != nil {
		t.Fatalf("answering server: %v", err)
	}
}

// The failure that killed CI: the server's accept thread is stuck, so the
// connection lands in the backlog and the handshake never starts.
func TestProbeServerStuckAccept(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close() // never accepts
	start := time.Now()
	if err := probeServer(context.Background(), ln.Addr().String(), 200*time.Millisecond); err == nil {
		t.Fatal("a server that never handshakes passed the probe")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("probe took %s, want ~its timeout", time.Since(start))
	}
}
