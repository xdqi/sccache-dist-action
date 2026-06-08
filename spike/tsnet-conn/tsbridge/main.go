// tsbridge: join a tailnet and forward a local TCP port to peer:port over tsnet,
// and/or expose a tsnet port forwarding to a local target. Minimal stand-in for
// the coordinator/worker bridges, used by the M0 connection-model spike.
//
// Usage examples:
//   tsbridge -hostname H -authkey K -tags T -listen 127.0.0.1:LP -target PEER:PP
//   tsbridge -hostname H -authkey K -tags T -expose :PP -local-target 127.0.0.1:LP
package main

import (
	"context"
	"flag"
	"io"
	"log"
	"net"
	"strings"

	"tailscale.com/tsnet"
)

func main() {
	hostname := flag.String("hostname", "", "tsnet hostname")
	authkey := flag.String("authkey", "", "tailscale authkey")
	tags := flag.String("tags", "", "comma-separated tags")
	listen := flag.String("listen", "", "local addr to listen on (empty = none)")
	target := flag.String("target", "", "peer host:port to forward -listen to (over tsnet)")
	expose := flag.String("expose", "", "tsnet addr to expose (empty = none)")
	localTarget := flag.String("local-target", "", "local host:port that -expose forwards to")
	flag.Parse()

	var at []string
	for _, t := range strings.Split(*tags, ",") {
		if t = strings.TrimSpace(t); t != "" {
			at = append(at, t)
		}
	}
	srv := &tsnet.Server{
		Hostname:      *hostname,
		AuthKey:       *authkey,
		Ephemeral:     true,
		Dir:           "/tmp/tsnet-" + *hostname,
		AdvertiseTags: at,
	}
	if err := srv.Start(); err != nil {
		log.Fatalf("start: %v", err)
	}
	if _, err := srv.Up(context.Background()); err != nil {
		log.Fatalf("up: %v", err)
	}
	log.Printf("tsbridge %s joined", *hostname)

	// Client side: forward a LOCAL listener to a tsnet peer.
	if *listen != "" && *target != "" {
		ln, err := net.Listen("tcp", *listen)
		if err != nil {
			log.Fatalf("listen %s: %v", *listen, err)
		}
		go acceptLoop(ln, func() (net.Conn, error) {
			return srv.Dial(context.Background(), "tcp", *target)
		})
		log.Printf("forward %s -> tsnet:%s", *listen, *target)
	}
	// Server side: expose a tsnet listener forwarding to a LOCAL target.
	if *expose != "" && *localTarget != "" {
		ln, err := srv.Listen("tcp", *expose)
		if err != nil {
			log.Fatalf("tsnet listen %s: %v", *expose, err)
		}
		go acceptLoop(ln, func() (net.Conn, error) {
			return net.Dial("tcp", *localTarget)
		})
		log.Printf("expose tsnet:%s -> %s", *expose, *localTarget)
	}
	select {}
}

func acceptLoop(ln net.Listener, dial func() (net.Conn, error)) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			r, err := dial()
			if err != nil {
				log.Printf("dial: %v", err)
				return
			}
			defer r.Close()
			go io.Copy(r, c)
			io.Copy(c, r)
		}()
	}
}
