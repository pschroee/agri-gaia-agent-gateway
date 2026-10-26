#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 Philipp Schröer
#
# SPDX-License-Identifier: MIT

"""Build-time check (issue #229): Chromium must not reach the proxy from the environment.

Usage: netcheck.py [--allow host,.domain] -- <cmd> [args...]

An --allow entry starting with a dot allows the domain and every host under it (".google.com").

Two listeners are opened. "dead" is 127.0.0.1:9, where Chromium's fail-closed --proxy-server points: whatever it
still tries ends there. "env" is a free local port set as http_proxy/https_proxy (the gateway relay in the sandbox):
nothing may arrive there. Both answer the first request line with 403 and close.
"""
import os
import socket
import subprocess
import sys
import threading
import time


class Listener:
    def __init__(self, port):
        self.sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self.sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        self.sock.bind(("127.0.0.1", port))
        self.sock.listen(64)
        self.port = self.sock.getsockname()[1]
        self.hosts = set()
        self.lock = threading.Lock()
        threading.Thread(target=self._serve, daemon=True).start()

    def _serve(self):
        while True:
            try:
                conn, _ = self.sock.accept()
            except OSError:
                return
            threading.Thread(target=self._handle, args=(conn,), daemon=True).start()

    def _handle(self, conn):
        try:
            conn.settimeout(5)
            data = b""
            while b"\n" not in data and len(data) < 4096:
                chunk = conn.recv(1024)
                if not chunk:
                    break
                data += chunk
            host = parse_host(data.split(b"\n", 1)[0].decode("latin-1").strip())
            with self.lock:
                self.hosts.add(host or "?")
            conn.sendall(b"HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
        except OSError:
            pass
        finally:
            conn.close()

    def seen(self):
        with self.lock:
            return sorted(self.hosts)


def parse_host(line):
    parts = line.split()
    if len(parts) < 2:
        return ""
    target = parts[1]
    if parts[0].upper() == "CONNECT":
        return target.rsplit(":", 1)[0].lower()
    if "://" in target:
        target = target.split("://", 1)[1]
    return target.split("/", 1)[0].rsplit(":", 1)[0].lower()


def allowed(host, allow):
    """True if host is listed, or lies under a listed ".domain" (dead-port hosts only; the env proxy allows nothing)."""
    for a in allow:
        if host == a or (a.startswith(".") and (host.endswith(a) or host == a[1:])):
            return True
    return False


def main(argv):
    allow = set()
    args = argv[1:]
    if args[:1] == ["--allow"] and len(args) > 1:
        allow = {h.strip().lower() for h in args[1].split(",") if h.strip()}
        args = args[2:]
    if args[:1] != ["--"] or len(args) < 2:
        print("usage: netcheck.py [--allow host,.domain] -- <cmd> [args...]", file=sys.stderr)
        return 2
    cmd = args[1:]

    dead = Listener(9)
    env_l = Listener(0)
    proxy = "http://127.0.0.1:%d" % env_l.port
    env = dict(os.environ)
    for k in ("http_proxy", "https_proxy", "HTTP_PROXY", "HTTPS_PROXY"):
        env[k] = proxy
    for k in ("no_proxy", "NO_PROXY"):
        env[k] = ""

    try:
        res = subprocess.run(cmd, env=env, timeout=120)
        rc = res.returncode
    except subprocess.TimeoutExpired:
        print("netcheck: command timed out: %s" % " ".join(cmd), file=sys.stderr)
        return 1
    time.sleep(2)

    if rc != 0:
        print("netcheck: command failed (%d): %s" % (rc, " ".join(cmd)), file=sys.stderr)
        return 1
    bypassed = env_l.seen()
    if bypassed:
        print("netcheck: request bypassed --proxy-server: %s" % ", ".join(bypassed), file=sys.stderr)
        return 1
    extra = [h for h in dead.seen() if not allowed(h, allow)]
    if extra:
        print("netcheck: Chromium still calls out: %s" % ", ".join(extra), file=sys.stderr)
        return 1
    print("netcheck OK: %s" % " ".join(cmd))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
