package worker

import (
	"os"
	"testing"
)

func TestGuard(t *testing.T) {
	g := &guard{threshold: 3}
	if g.observe(true) {
		t.Fatal("present: no exit")
	}
	if g.observe(false) || g.observe(false) {
		t.Fatal("2 misses < 3")
	}
	if !g.observe(false) {
		t.Fatal("3rd miss -> exit")
	}
	g.observe(true)
	if g.observe(false) {
		t.Fatal("reset then 1 miss < 3")
	}
}

func TestServerPublicAddr(t *testing.T) {
	if serverPublicAddr(1) != "127.0.0.1:10501" {
		t.Fatalf("idx1 got %s", serverPublicAddr(1))
	}
	if serverPublicAddr(3) != "127.0.0.1:10503" {
		t.Fatalf("idx3 got %s", serverPublicAddr(3))
	}
}

func TestWorkerIndex(t *testing.T) {
	os.Setenv("INPUT_WORKER_INDEX", "2")
	defer os.Unsetenv("INPUT_WORKER_INDEX")
	if workerIndex() != 2 {
		t.Fatalf("got %d", workerIndex())
	}
}
