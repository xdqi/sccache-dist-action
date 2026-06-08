package tsmesh

import "testing"

func TestFilterOnline(t *testing.T) {
	peers := []Peer{
		{Host: "r1-worker-1", Online: true},
		{Host: "r1-worker-2", Online: false},
		{Host: "r1-coordinator", Online: true},
	}
	got := FilterOnline(peers, "r1-worker-")
	if len(got) != 1 || got[0].Host != "r1-worker-1" {
		t.Fatalf("got %+v", got)
	}
}

func TestWorkersReady(t *testing.T) {
	if workersReady(1, 2, 1, false) { t.Fatal("before timeout needs full expected") }
	if !workersReady(2, 2, 1, false) { t.Fatal("full expected ready") }
	if !workersReady(1, 2, 1, true) { t.Fatal("after timeout min suffices") }
	if workersReady(0, 2, 1, true) { t.Fatal("below min not ready") }
}
