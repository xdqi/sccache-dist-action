package tsmesh

import (
	"context"
	"fmt"
	"log"
	"net"
	"strings"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/tsnet"
)

// Peer is a simplified view of a tailnet peer.
type Peer struct {
	Host   string
	Online bool
	IP     string
}

// FilterOnline returns peers whose hostname starts with prefix and are Online.
func FilterOnline(peers []Peer, prefix string) []Peer {
	var out []Peer
	for _, p := range peers {
		if strings.HasPrefix(p.Host, prefix) && p.Online {
			out = append(out, p)
		}
	}
	return out
}

// PresentOnline reports whether a peer with exactly this hostname is Online.
func PresentOnline(peers []Peer, host string) bool {
	for _, p := range peers {
		if p.Host == host && p.Online {
			return true
		}
	}
	return false
}

// splitTags turns "tag:a,tag:b" into ["tag:a","tag:b"], trimming spaces and
// dropping empties. Returns nil for an empty input.
func splitTags(tags string) []string {
	var out []string
	for _, t := range strings.Split(tags, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// Mesh wraps a live tsnet node.
type Mesh struct {
	srv *tsnet.Server
	lc  *local.Client
}

// Up starts a tsnet node with the given hostname/tags/authkey and blocks until
// it has joined the tailnet (or ctx expires). Transient control-plane failures
// (e.g. an OAuth token POST timing out on a flaky runner network — run
// 27257400910 lost worker 9 exactly this way) are retried with backoff rather
// than killing the node one-shot.
func Up(ctx context.Context, hostname, authKey, tags string) (*Mesh, error) {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if attempt > 1 {
			log.Printf("[tsmesh] tsnet up failed (%v); retrying (attempt %d/3)", lastErr, attempt)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(10 * time.Second):
			}
		}
		m, err := upOnce(ctx, hostname, authKey, tags)
		if err == nil {
			return m, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func upOnce(ctx context.Context, hostname, authKey, tags string) (*Mesh, error) {
	srv := &tsnet.Server{
		Hostname:  hostname,
		AuthKey:   authKey,
		Ephemeral: true,
		Dir:       "/tmp/tsnet-" + hostname,
		// REQUIRED for OAuth client secrets used as the auth key.
		AdvertiseTags: splitTags(tags),
	}
	if err := srv.Start(); err != nil {
		return nil, fmt.Errorf("tsnet start: %w", err)
	}
	if _, err := srv.Up(ctx); err != nil {
		srv.Close()
		return nil, fmt.Errorf("tsnet up: %w", err)
	}
	lc, err := srv.LocalClient()
	if err != nil {
		srv.Close()
		return nil, fmt.Errorf("localclient: %w", err)
	}
	return &Mesh{srv: srv, lc: lc}, nil
}

// Peers returns the current tailnet peers as simplified Peer structs.
func (m *Mesh) Peers(ctx context.Context) ([]Peer, error) {
	st, err := m.lc.Status(ctx)
	if err != nil {
		return nil, err
	}
	var out []Peer
	for _, ps := range st.Peer {
		ip := ""
		if len(ps.TailscaleIPs) > 0 {
			ip = ps.TailscaleIPs[0].String()
		}
		out = append(out, Peer{Host: ps.HostName, Online: ps.Online, IP: ip})
	}
	return out, nil
}

// workersReady decides whether the coordinator should stop waiting.
//   - Before timeout: ready only once the FULL expected count is online, so a
//     slow-starting worker still makes the bus instead of being left idle.
//   - After timeout: fall back to the min-workers floor (but never below min).
//
// min is a floor for graceful degradation, NOT a trigger to start early.
func workersReady(online, expected, min int, timedOut bool) bool {
	if !timedOut {
		return online >= expected
	}
	return online >= min
}

// WaitForWorkers polls until the expected number of workers (hostname prefix)
// are online, or until the deadline — after which it accepts >= min. Returns the
// online worker peers found.
func (m *Mesh) WaitForWorkers(ctx context.Context, prefix string, expected, min int, poll time.Duration) ([]Peer, error) {
	for {
		peers, err := m.Peers(ctx)
		var online []Peer
		if err == nil {
			online = FilterOnline(peers, prefix)
			if workersReady(len(online), expected, min, false) {
				return online, nil
			}
		}
		select {
		case <-ctx.Done():
			peers, _ := m.Peers(context.Background())
			online = FilterOnline(peers, prefix)
			if workersReady(len(online), expected, min, true) {
				return online, nil
			}
			return online, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// Dial opens a connection to host:port over the tailnet.
func (m *Mesh) Dial(ctx context.Context, hostport string) (net.Conn, error) {
	return m.srv.Dial(ctx, "tcp", hostport)
}

// Listen accepts tailnet connections on the given address (e.g. ":3632").
func (m *Mesh) Listen(addr string) (net.Listener, error) {
	return m.srv.Listen("tcp", addr)
}

// Close shuts the node down, dropping it from the tailnet (teardown signal).
func (m *Mesh) Close() error { return m.srv.Close() }
