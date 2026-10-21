// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// The relay (issue #175): in the execution sandbox the proxy variables point at 127.0.0.1:3128, and this listener
// hands every connection, byte for byte, to the slot's Unix socket. The gateway behind the socket decides (web
// proxy for CONNECT and absolute URLs, package gate for /index/ and /npm/). The relay itself understands nothing of
// HTTP, so it cannot be talked into anything; and as the sandbox's network has no other route, a program that
// ignores the proxy variables is simply offline.

const (
	relayAddr      = "127.0.0.1:3128"
	relaySocketEnv = "AGW_SOCKET"
	relaySocketDef = "/run/agw/agw.sock"
)

func relaySocket() string {
	if s := os.Getenv(relaySocketEnv); s != "" {
		return s
	}
	return relaySocketDef
}

// startRelay listens on addr until ctx ends. A busy port (an earlier supervisor that has not exited yet, or a
// process of the agent) is retried every retry; each failure is reported once through logf.
func startRelay(ctx context.Context, addr, socket string, retry time.Duration, logf func(string)) {
	warned := false
	for ctx.Err() == nil {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			if !warned {
				warned = true
				logf(fmt.Sprintf("relay: cannot listen on %s: %v (retrying)", addr, err))
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(retry):
			}
			continue
		}
		go func() {
			<-ctx.Done()
			ln.Close()
		}()
		serveRelay(ln, socket)
		return
	}
}

func serveRelay(ln net.Listener, socket string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go relayConn(c, socket)
	}
}

// relayConn copies both directions until either side ends.
func relayConn(c net.Conn, socket string) {
	defer c.Close()
	u, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		return
	}
	defer u.Close()
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}
	go cp(u, c)
	go cp(c, u)
	wg.Wait()
}
