package tsmesh

import (
	"testing"

	"tailscale.com/ipn/ipnstate"
)

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

func TestPeerPath(t *testing.T) {
	cases := []struct {
		ps   ipnstate.PeerStatus
		want string
	}{
		{ipnstate.PeerStatus{CurAddr: "1.2.3.4:41641", Relay: "sea"}, "direct 1.2.3.4:41641"},
		{ipnstate.PeerStatus{PeerRelay: "5.6.7.8:7777:9", Relay: "sea"}, "peer-relay 5.6.7.8:7777:9"},
		{ipnstate.PeerStatus{Relay: "sea"}, "derp sea"},
		{ipnstate.PeerStatus{}, "none"},
	}
	for _, c := range cases {
		if got := peerPath(&c.ps); got != c.want {
			t.Fatalf("%+v: got %q want %q", c.ps, got, c.want)
		}
	}
	peers := []Peer{{Host: "r1-coordinator", Path: "derp sea"}}
	if PathTo(peers, "r1-coordinator") != "derp sea" || PathTo(peers, "r1-worker-1") != "not in netmap" {
		t.Fatal("PathTo")
	}
}
