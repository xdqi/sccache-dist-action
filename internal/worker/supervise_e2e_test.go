//go:build e2e

// Runs a real `sccache-dist scheduler` + `server` (docker builder):
//
//	go test -tags e2e -run E2E -v ./internal/worker/
//
// Needs sccache-dist on PATH and a docker daemon.
package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func startDist(t *testing.T, logf *os.File, args ...string) (*exec.Cmd, error) {
	cmd := exec.Command("sccache-dist", args...)
	cmd.Env = append(os.Environ(), "SCCACHE_NO_DAEMON=1", "SCCACHE_LOG=info")
	cmd.Stdout, cmd.Stderr = logf, logf
	return cmd, cmd.Start()
}

func schedulerServers(t *testing.T, port int) int {
	t.Helper()
	req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/api/v1/scheduler/status", port), nil)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st struct {
		NumServers int `json:"num_servers"`
	}
	json.NewDecoder(resp.Body).Decode(&st)
	return st.NumServers
}

// The CI failure end to end: one connection that never finishes its TLS
// handshake wedges the server's accept thread for good. The supervisor must
// notice over loopback, replace the server, and the replacement must
// re-register with the scheduler.
func TestE2ESupervisorReplacesWedgedServer(t *testing.T) {
	if _, err := exec.LookPath("sccache-dist"); err != nil {
		t.Skip("sccache-dist not on PATH")
	}
	dir := t.TempDir()
	logf, err := os.Create(filepath.Join(dir, "dist.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		b, _ := os.ReadFile(logf.Name())
		t.Logf("sccache-dist log:\n%s", b)
	}()
	schedPort, srvPort := freePort(t), freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", srvPort)
	schedConf := filepath.Join(dir, "sched.conf")
	os.WriteFile(schedConf, []byte(fmt.Sprintf(`public_addr = "127.0.0.1:%d"
[client_auth]
type = "token"
token = "e2e"
[server_auth]
type = "token"
token = "e2e"
`, schedPort)), 0o644)
	srvConf := filepath.Join(dir, "server.conf")
	os.WriteFile(srvConf, []byte(fmt.Sprintf(`cache_dir = %q
public_addr = %q
bind_address = %q
scheduler_url = "http://127.0.0.1:%d"
[builder]
type = "docker"
[scheduler_auth]
type = "token"
token = "e2e"
`, filepath.Join(dir, "cache"), addr, addr, schedPort)), 0o644)

	sched, err := startDist(t, logf, "scheduler", "--config", schedConf)
	if err != nil {
		t.Fatal(err)
	}
	defer sched.Process.Kill()
	time.Sleep(time.Second)

	sup := &supervisor{
		start: func() (*exec.Cmd, error) { return startDist(t, logf, "server", "--config", srvConf) },
		probe: func(ctx context.Context) error {
			return probeServer(ctx, addr, 2*time.Second)
		},
		interval:  time.Second,
		threshold: 3,
		minUptime: time.Second,
	}
	if err := sup.launch(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sup.run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	waitUntil := func(what string, d time.Duration, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(d)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out after %s waiting for %s", d, what)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	answers := func() bool { return probeServer(context.Background(), addr, 2*time.Second) == nil }
	waitUntil("the server to answer", 20*time.Second, answers)
	waitUntil("the server to register", 20*time.Second, func() bool { return schedulerServers(t, schedPort) == 1 })

	wedge, err := net.Dial("tcp", addr) // connects, never sends a ClientHello
	if err != nil {
		t.Fatal(err)
	}
	defer wedge.Close()
	time.Sleep(200 * time.Millisecond)
	if answers() {
		t.Fatal("the server still answers with a silent connection open; the wedge didn't take")
	}
	t.Log("server wedged")

	waitUntil("a restart", 30*time.Second, func() bool { return sup.restarts.Load() >= 1 })
	waitUntil("the replacement to answer", 20*time.Second, answers)
	// A new nonce replaces the old registration rather than adding one.
	waitUntil("one registered server", 20*time.Second, func() bool { return schedulerServers(t, schedPort) == 1 })
	if !answers() {
		t.Fatal("replacement stopped answering")
	}
}
