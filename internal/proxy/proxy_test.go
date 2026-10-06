package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

// forwardTo serves a Forwarder from a fresh listener to addr and returns the
// listener's address.
func forwardTo(t *testing.T, addr string) (string, *Forwarder) {
	t.Helper()
	ln := listen(t)
	f := New("test", Local(addr))
	go f.Serve(ln)
	return ln.Addr().String(), f
}

// backend accepts one connection, reads it to EOF, then writes reply and
// closes. It reports what it read, or times out if EOF never arrives.
func backend(t *testing.T, reply string) (string, <-chan string) {
	t.Helper()
	ln := listen(t)
	got := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		b, err := io.ReadAll(c)
		if err != nil {
			got <- "no EOF: " + err.Error()
			return
		}
		c.Write([]byte(reply))
		got <- string(b)
	}()
	return ln.Addr().String(), got
}

// A client that gives up must look like a closed connection to the backend.
// sccache-dist's TLS server handshakes in its only accept thread with no
// timeout; when the scheduler abandoned an assign_job mid-handshake, the old
// forward never passed the close on, so that thread waited forever and the
// worker stopped accepting connections (anyfs-reader CI, 2026-10-06).
func TestClientCloseReachesBackendThroughTwoHops(t *testing.T) {
	addr, got := backend(t, "")
	// coordinator forward -> worker forward -> server, as in the farm
	hop, _ := forwardTo(t, addr)
	front, _ := forwardTo(t, hop)

	c, err := net.Dial("tcp", front)
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte("client hello"))
	c.Close()

	if s := <-got; s != "client hello" {
		t.Fatalf("backend got %q, want the data then EOF", s)
	}
}

// Half-closing the request side must not cut off the response.
func TestResponseAfterClientHalfClose(t *testing.T) {
	addr, got := backend(t, "response")
	hop, _ := forwardTo(t, addr)
	front, _ := forwardTo(t, hop)

	c, err := net.Dial("tcp", front)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("request"))
	c.(*net.TCPConn).CloseWrite()

	if s := <-got; s != "request" {
		t.Fatalf("backend got %q", s)
	}
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	b, err := io.ReadAll(c)
	if err != nil || string(b) != "response" {
		t.Fatalf("client got %q, %v; want the response then EOF", b, err)
	}
}

// A backend that can't be reached must cost the client a bounded wait, not
// hold its connection open forever.
func TestDialTimeoutClosesClient(t *testing.T) {
	ln := listen(t)
	f := New("stuck", func(ctx context.Context) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	f.dialTimeout = 200 * time.Millisecond
	go f.Serve(ln)

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("client read %v, want EOF once the dial gives up", err)
	}
	if s := f.Status(); !strings.Contains(s, "dial-fail=1") {
		t.Fatalf("status %q, want dial-fail=1", s)
	}
}

func TestStatusCountsRelays(t *testing.T) {
	addr, got := backend(t, "ok")
	front, f := forwardTo(t, addr)

	c, err := net.Dial("tcp", front)
	if err != nil {
		t.Fatal(err)
	}
	c.(*net.TCPConn).CloseWrite()
	<-got
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	io.ReadAll(c)
	c.Close()

	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(f.Status(), "active=0 total=1 ") {
		if time.Now().After(deadline) {
			t.Fatalf("status %q, want active=0 total=1", f.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// flakyListener fails its first Accept with a non-close error.
type flakyListener struct {
	net.Listener
	failed bool
}

func (l *flakyListener) Accept() (net.Conn, error) {
	if !l.failed {
		l.failed = true
		return nil, errors.New("accept: too many open files")
	}
	return l.Listener.Accept()
}

// A transient Accept error must not end the forward for the rest of the job.
func TestServeSurvivesAcceptError(t *testing.T) {
	addr, got := backend(t, "")
	ln := listen(t)
	go New("flaky", Local(addr)).Serve(&flakyListener{Listener: ln})

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte("x"))
	c.Close()
	if s := <-got; s != "x" {
		t.Fatalf("backend got %q after a failed Accept", s)
	}
}
