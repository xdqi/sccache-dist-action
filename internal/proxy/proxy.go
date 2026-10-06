// Package proxy relays TCP connections across the farm's tsnet hops: the
// coordinator's per-worker forwards and scheduler exposure, and the worker's
// scheduler bridge and server exposure.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync/atomic"
	"time"
)

// A relay dials its backend up to dialAttempts times, each attempt bounded
// by dialAttemptTimeout. Without a bound, a tsnet dial to an unreachable peer
// held the client's connection open until the client's own timeout: 30 s for
// the scheduler's assign_job, 20 min for the sccache client's run_job.
//
// The retry matters as much as the bound. gVisor's netstack drops a SYN whose
// 4-tuple is still in TIME_WAIT (60 s) unless a listening endpoint owns the
// port, and tsnet serves its listeners through a forwarder instead, so a dial
// that happens to reuse the source port of a recently closed connection
// hangs until that TIME_WAIT expires. The far side closes first (the
// sccache-dist HTTP servers close after every response), so at the farm's
// connection rates about 1 dial in 300 hit this (anyfs-reader CI,
// 2026-10-06). A fresh dial picks a fresh random port.
const (
	dialAttempts       = 3
	dialAttemptTimeout = 4 * time.Second
)

// Forwarder relays connections accepted on a listener to a dialed backend,
// counting them for the periodic status line.
type Forwarder struct {
	name           string
	dial           func(ctx context.Context) (net.Conn, error)
	attemptTimeout time.Duration

	active, total, dialRetries, dialFails atomic.Int64
	slowestDial                           atomic.Int64 // ns, since the last Status
}

// New returns a Forwarder that relays to connections from dial.
func New(name string, dial func(ctx context.Context) (net.Conn, error)) *Forwarder {
	return &Forwarder{name: name, dial: dial, attemptTimeout: dialAttemptTimeout}
}

// Local dials addr on the host network.
func Local(addr string) func(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	return func(ctx context.Context) (net.Conn, error) { return d.DialContext(ctx, "tcp", addr) }
}

// Serve accepts connections on ln and relays each one, until ln is closed.
func (f *Forwarder) Serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// e.g. EMFILE: returning here would end this forward for the
			// rest of the job.
			log.Printf("[proxy] %s: accept: %v; retrying", f.name, err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go f.relay(c)
	}
}

// Status summarizes the relays so far and resets the slowest-dial mark.
func (f *Forwarder) Status() string {
	slowest := time.Duration(f.slowestDial.Swap(0))
	return fmt.Sprintf("%s: active=%d total=%d dial-retry=%d dial-fail=%d slowest-dial=%s",
		f.name, f.active.Load(), f.total.Load(), f.dialRetries.Load(), f.dialFails.Load(),
		slowest.Round(time.Millisecond))
}

// relay copies both directions until both have ended, passing each side's
// EOF on to the other as a half-close. A peer that gives up therefore shows
// up as a closed connection at the far end. sccache-dist's TLS server
// handshakes in its only accept thread with no timeout, so a close that is
// swallowed here leaves that thread waiting forever and the worker accepts
// nothing more (anyfs-reader CI, 2026-10-06).
func (f *Forwarder) relay(c net.Conn) {
	defer c.Close()
	f.total.Add(1)
	f.active.Add(1)
	defer f.active.Add(-1)

	start := time.Now()
	r, err := f.dialRetrying()
	took := time.Since(start)
	for {
		old := f.slowestDial.Load()
		if int64(took) <= old || f.slowestDial.CompareAndSwap(old, int64(took)) {
			break
		}
	}
	if err != nil {
		f.dialFails.Add(1)
		log.Printf("[proxy] %s: dial failed %d times in %s, last: %v",
			f.name, dialAttempts, took.Round(time.Millisecond), err)
		return
	}
	defer r.Close()
	done := make(chan struct{}, 2)
	go pipe(r, c, done)
	go pipe(c, r, done)
	<-done
	<-done
}

// dialRetrying dials with a fresh attempt after each failure; nothing has been
// relayed yet, so a retry is invisible to the client.
func (f *Forwarder) dialRetrying() (net.Conn, error) {
	var err error
	for attempt := 1; attempt <= dialAttempts; attempt++ {
		if attempt > 1 {
			f.dialRetries.Add(1)
		}
		ctx, cancel := context.WithTimeout(context.Background(), f.attemptTimeout)
		start := time.Now()
		var r net.Conn
		r, err = f.dial(ctx)
		cancel()
		if err == nil {
			return r, nil
		}
		if attempt < dialAttempts {
			log.Printf("[proxy] %s: dial attempt %d failed after %s (%v); retrying",
				f.name, attempt, time.Since(start).Round(time.Millisecond), err)
			// A refused dial returns at once; don't spin on it.
			time.Sleep(time.Until(start.Add(100 * time.Millisecond)))
		}
	}
	return nil, err
}

func pipe(dst, src net.Conn, done chan<- struct{}) {
	io.Copy(dst, src)
	closeWrite(dst)
	done <- struct{}{}
}

// closeWrite half-closes c (TCP and tsnet's gonet conns both support it),
// or fully closes a conn that can't.
func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
		return
	}
	c.Close()
}
