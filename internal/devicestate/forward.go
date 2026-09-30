package devicestate

import (
	"context"
	"io"
	"net"
	"sync"
	"time"

	"github.com/go-logr/logr"
)

// Forward listens on listen (a loopback address) and relays every connection
// to target. The node-agent uses it to make the cluster-internal snapshot
// registry reachable as localhost:<port> from the node's container runtime,
// which treats localhost registries as plain HTTP, so no host configuration
// (registries.yaml, hosts.toml, certificates, DNS) is needed. It returns when
// ctx ends.
func Forward(ctx context.Context, listen, target string, log logr.Logger) error {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			relay(ctx, c, target, log)
		}()
	}
}

func relay(ctx context.Context, client net.Conn, target string, log logr.Logger) {
	defer client.Close()
	d := net.Dialer{Timeout: 5 * time.Second}
	upstream, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		log.V(1).Info("registry forwarder: dial upstream", "target", target, "err", err.Error())
		return
	}
	defer upstream.Close()
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(upstream, client)
	go cp(client, upstream)
	select {
	case <-done:
		<-done
	case <-ctx.Done():
	}
}
