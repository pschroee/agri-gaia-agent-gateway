// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The relay hands the bytes of a connection to the Unix socket unchanged, in both directions.
func TestRelayCopiesBothWays(t *testing.T) {
	dir, err := os.MkdirTemp("", "agwrelay")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "s")
	ul, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ul.Close()
	go func() {
		c, err := ul.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 5)
		io.ReadFull(c, buf)
		c.Write([]byte("echo:" + string(buf)))
	}()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go serveRelay(ln, sock)
	defer ln.Close()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("hello"))
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, _ := io.ReadAll(c)
	if string(got) != "echo:hello" {
		t.Fatalf("got %q", got)
	}
}

// A busy port is retried and the relay starts when it frees up.
func TestRelayRetriesBusyPort(t *testing.T) {
	hold, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := hold.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	warned := make(chan string, 4)
	go startRelay(ctx, addr, "/nonexistent.sock", 50*time.Millisecond, func(m string) { warned <- m })
	select {
	case <-warned:
	case <-time.After(3 * time.Second):
		t.Fatal("no warning for a busy port")
	}
	hold.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			c.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the relay never started")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
