package coordinator

import (
	"bufio"
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The forwarder outlives its setup step, and in a container job the step's
// stdout is a docker exec stream that closes when the step ends. v0.0.9's
// first status line after that killed the forwarder with SIGPIPE, which took
// the coordinator's tailnet node down, so every worker saw the coordinator
// gone and left (msys2-cross run 37450353873). The scheduler's output also
// runs through the log writer, so a failing write must not stall it either.
func TestLogSurvivesClosedStdout(t *testing.T) {
	if os.Getenv("OPENLOG_HELPER") == "1" {
		return // only meaningful inside helperProcess
	}
	logPath := filepath.Join(t.TempDir(), "coordinator.log")
	cmd := exec.Command(os.Args[0], "-test.run=TestOpenLogHelper")
	cmd.Env = append(os.Environ(), "OPENLOG_HELPER=1", "OPENLOG_PATH="+logPath)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Read the first line, then go away like the step's stdout does.
	if _, err := bufio.NewReader(stdout).ReadString('\n'); err != nil {
		t.Fatalf("no first line from the helper: %v", err)
	}
	stdout.Close()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("helper died after stdout closed: %v", err)
		} else if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		cmd.Process.Kill()
		t.Fatal("helper hung after stdout closed (child output blocked?)")
	}
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"before", "after stdout closed", "helper done"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("log file lacks %q:\n%s", want, tail(string(b)))
		}
	}
	// Everything the scheduler stand-in wrote must reach the file: the file
	// is the only copy once the step's stdout is gone.
	if n := strings.Count(string(b), "x"); n != 300000 {
		t.Fatalf("log file holds %d of the child's 300000 bytes", n)
	}
}

// TestOpenLogHelper is the forwarder stand-in run by TestLogSurvivesClosedStdout.
func TestOpenLogHelper(t *testing.T) {
	if os.Getenv("OPENLOG_HELPER") != "1" {
		t.Skip("helper process only")
	}
	w, _ := openLog(os.Getenv("OPENLOG_PATH"))
	log.Print("before")
	time.Sleep(500 * time.Millisecond) // the parent closes our stdout now
	log.Print("after stdout closed")
	// The scheduler stand-in: more output than a pipe buffer holds.
	child := exec.Command("sh", "-c", "head -c 300000 /dev/zero | tr '\\0' x")
	child.Stdout, child.Stderr = w, w
	if err := child.Run(); err != nil {
		log.Printf("child: %v", err)
	}
	log.Print("helper done")
}

func tail(s string) string {
	if len(s) > 400 {
		return "..." + s[len(s)-400:]
	}
	return s
}
