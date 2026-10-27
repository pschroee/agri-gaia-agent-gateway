#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 Philipp Schröer
#
# SPDX-License-Identifier: MIT
"""Report whether every pinned artifact of the agent images is still available.

usage: check-artifacts.py [--list] [-v] [--jobs N] [--compose FILE]... [--npm NAME@VERSION]... [--suggest-digests]

Run it before a rollout that rebuilds images (a package an author unpublished breaks the build half way through). It
reads only files next to it and public metadata (HEAD requests, JSON of the registries); it downloads no artifacts and
changes nothing. Python 3.8 or newer, standard library only.

Inventory (see docs/design.md, "Pinned artifacts"):
  image     FROM lines of Dockerfile and images/agw-basis/Dockerfile, image: of compose.yaml / compose.hot.yaml and of the
            agent_* services of every --compose file, baseImage of the Nuclio function (internal/annotation/funczip.go)
  npm       every entry of the four package-lock.json files, the `npm install -g` lines of the Dockerfiles, --npm
  pypi      images/agw-basis/python/requirements.lock (with its hashes), the pins of funczip.go
  go        go.mod
  apt       the packages of apt-get install in the Dockerfiles (names only, Debian keeps no old versions)
  download  GitHub release assets fetched by curl in the Dockerfiles (with the sha256 ARGs)
  typst     images/agw-basis/typst/packages.txt

Output: one line `<OK|WARN|FAIL><TAB><kind><TAB><name@version><TAB><detail>`; without -v only WARN and FAIL, then
`artifacts: <n> ok, <w> warn, <f> fail`. FAIL: not available, hash or digest differs. WARN: could be unpublished by its
author (one maintainer, fewer than 300 downloads last week), deprecated, yanked. Exit 0 without FAIL, 1 with FAIL, 2 on
a read or parse error.
"""
import argparse
import concurrent.futures
import json
import os
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
TIMEOUT = 20
RETRIES = 2
LOW_DOWNLOADS = 300
ARCHES = ("x86_64", "aarch64")
ARCH_ARG = {"x86_64": "AMD64", "aarch64": "ARM64"}
MANIFEST_ACCEPT = ", ".join([
    "application/vnd.oci.image.index.v1+json",
    "application/vnd.docker.distribution.manifest.list.v2+json",
    "application/vnd.oci.image.manifest.v1+json",
    "application/vnd.docker.distribution.manifest.v2+json",
])


class InputError(Exception):
    pass


# ---------------------------------------------------------------------------
# Inventory

def read(rel):
    path = rel if os.path.isabs(rel) else os.path.join(ROOT, rel)
    try:
        with open(path, encoding="utf-8") as f:
            return f.read()
    except OSError as e:
        raise InputError("cannot read %s: %s" % (rel, e))


def item(kind, name, version, **extra):
    d = {"kind": kind, "name": name, "version": version}
    d.update(extra)
    return d


def joined(text):
    """Dockerfile text with line continuations folded into one line."""
    return re.sub(r"\\\n\s*", " ", text)


def dockerfile_args(text):
    return dict(re.findall(r"^ARG\s+([A-Za-z0-9_]+)=(\S+)", text, re.M))


def subst(s, args):
    return re.sub(r"\$\{([A-Za-z0-9_]+)\}", lambda m: args.get(m.group(1), m.group(0)), s)


def inv_dockerfile(rel):
    text = read(rel)
    args = dockerfile_args(text)
    flat = joined(text)
    out = []
    stages = set()
    for line in text.splitlines():
        m = re.match(r"FROM\s+(\S+)(?:\s+AS\s+(\S+))?", line, re.I)
        if not m:
            continue
        if m.group(1) in stages:
            stages.add(m.group(2) or "")
            continue
        if m.group(2):
            stages.add(m.group(2))
        out.append(item("image", m.group(1), "", ref=subst(m.group(1), args), src=rel))
    # apt: the install lists
    pkgs = []
    for m in re.finditer(r"apt-get install ([^&;|\n]*)", flat):
        pkgs += [t for t in m.group(1).split() if not t.startswith("-")]
    for p in sorted(set(pkgs)):
        out.append(item("apt", p, "", src=rel))
    # npm install -g
    for m in re.finditer(r"npm install -g\s+((?:--?\S+\s+)*)(\S+)", flat):
        spec = subst(m.group(2), args)
        n, v = split_npm(spec)
        if n and v:
            out.append(item("npm", n, v, direct=True, src=rel))
    # downloads
    for m in re.finditer(r"curl[^\"\n]*?(?:\"|')(https://[^\"']+)(?:\"|')", flat):
        raw = m.group(1)
        var = re.search(r"\$\{([A-Z0-9]+)_VERSION\}", raw)
        for arch in ARCHES:
            url = subst(raw.replace("${T}", arch), args)
            if "${" in url:
                raise InputError("%s: cannot resolve %s" % (rel, url))
            sha = args.get("%s_SHA256_%s" % (var.group(1), ARCH_ARG[arch])) if var else None
            out.append(item("download", url.rsplit("/", 1)[-1], "", url=url, sha=sha, src=rel))
    return out


def split_npm(spec):
    at = spec.rfind("@")
    if at <= 0:
        return spec, ""
    return spec[:at], spec[at + 1:]


def inv_npm_lock(rel):
    try:
        lock = json.loads(read(rel))
    except ValueError as e:
        raise InputError("%s: %s" % (rel, e))
    pk = lock.get("packages")
    if not isinstance(pk, dict):
        raise InputError("%s: no packages (lockfile version 2 or 3 expected)" % rel)
    root = pk.get("", {})
    direct = set(root.get("dependencies", {})) | set(root.get("devDependencies", {}))
    out = []
    for key, v in sorted(pk.items()):
        if not key or v.get("link"):
            continue
        name = key.rsplit("node_modules/", 1)[-1]
        if not v.get("resolved"):
            raise InputError("%s: %s has no resolved URL" % (rel, key))
        out.append(item("npm", name, v.get("version", ""), resolved=v["resolved"], integrity=v.get("integrity"),
                        direct=name in direct and key == "node_modules/" + name, src=rel))
    return out


def inv_pip_lock(rel):
    text = read(rel)
    out, cur = [], None
    for line in text.splitlines():
        s = line.strip().rstrip("\\").strip()
        if not s or s.startswith("#"):
            continue
        if s.startswith("--hash=sha256:"):
            if cur is None:
                raise InputError("%s: hash before any requirement" % rel)
            cur["hashes"].append(s[len("--hash=sha256:"):])
            continue
        m = re.fullmatch(r"([A-Za-z0-9][A-Za-z0-9._-]*)==(\S+)", s)
        if not m:
            raise InputError("%s: cannot parse %r" % (rel, s))
        cur = item("pypi", m.group(1), m.group(2), hashes=[], src=rel)
        out.append(cur)
    return out


def inv_go_mod(rel):
    out, block = [], False
    for line in read(rel).splitlines():
        s = line.strip()
        if s.startswith("require ("):
            block = True
            continue
        if block and s == ")":
            block = False
            continue
        m = re.match(r"(?:require\s+)?(\S+)\s+(v\S+)", s) if (block or s.startswith("require ")) else None
        if m:
            out.append(item("go", m.group(1), m.group(2), src=rel))
    return out


def inv_typst(rel):
    out = []
    for line in read(rel).splitlines():
        s = line.strip()
        if s and not s.startswith("#"):
            n, _, v = s.partition(":")
            out.append(item("typst", n, v, src=rel))
    return out


def inv_compose(rel, only_agent):
    """image: values of a compose file; services that are built here are skipped."""
    out, svc = [], None
    blocks = {}
    for line in read(rel).splitlines():
        m = re.match(r"^  ([A-Za-z0-9_.-]+):\s*$", line)
        if m:
            svc = m.group(1)
            blocks[svc] = []
            continue
        if re.match(r"^\S", line):
            svc = None
        if svc:
            blocks[svc].append(line)
    for name, lines in blocks.items():
        if only_agent and not name.startswith("agent_"):
            continue
        body = "\n".join(lines)
        built = re.search(r"^    build:(?!\s*!reset)", body, re.M)
        m = re.search(r"^    image:\s*[\"']?([^\s\"'#]+)", body, re.M)
        if m and not built:
            out.append(item("image", m.group(1), "", ref=m.group(1), src="%s:%s" % (os.path.basename(rel), name)))
    return out


def inv_funczip(rel):
    text = read(rel)
    out = []
    m = re.search(r'functionPackages\s*=\s*"([^"]+)"', text)
    if not m:
        raise InputError("%s: functionPackages not found" % rel)
    for p in m.group(1).split():
        n, _, v = p.partition("==")
        out.append(item("pypi", n, v, hashes=[], src=rel))
    for b in re.findall(r"baseImage:\s*([^\s\\]+)", text):
        out.append(item("image", b, "", ref=b, src=rel))
    return out


def inventory(extra_compose, extra_npm):
    out = []
    out += inv_dockerfile("Dockerfile")
    out += inv_dockerfile("images/agw-basis/Dockerfile")
    for lock in ("images/agw-basis/pi-packages/package-lock.json", "images/agw-basis/mmdc/package-lock.json",
                 "images/agw-basis/pi-subagents-deps/package-lock.json", "web/package-lock.json"):
        out += inv_npm_lock(lock)
    out += inv_pip_lock("images/agw-basis/python/requirements.lock")
    out += inv_go_mod("go.mod")
    out += inv_typst("images/agw-basis/typst/packages.txt")
    out += inv_compose("compose.yaml", False)
    out += inv_compose("compose.hot.yaml", False)
    for c in extra_compose:
        out += inv_compose(c, True)
    out += inv_funczip("internal/annotation/funczip.go")
    for spec in extra_npm:
        n, v = split_npm(spec)
        if not n or not v:
            raise InputError("--npm wants NAME@VERSION, got %r" % spec)
        out.append(item("npm", n, v, direct=True, adhoc=True, src="--npm"))
    # the same artifact from several files is checked once
    seen, uniq = set(), []
    for it in out:
        key = (it["kind"], it["name"], it["version"], it.get("ref"), it.get("url"))
        if key not in seen:
            seen.add(key)
            uniq.append(it)
    return uniq


def label(it):
    if it["kind"] == "image":
        return it["ref"]
    return "%s@%s" % (it["name"], it["version"]) if it["version"] else it["name"]


# ---------------------------------------------------------------------------
# Network

class NoRedirectAuth(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        new = super().redirect_request(req, fp, code, msg, headers, newurl)
        if new is not None and urllib.parse.urlparse(newurl).netloc != urllib.parse.urlparse(req.full_url).netloc:
            new.headers.pop("Authorization", None)  # never send a registry token to another host
            new.unredirected_hdrs.pop("Authorization", None)
        return new


OPENER = urllib.request.build_opener(NoRedirectAuth)


def http(method, url, headers=None, want_body=False):
    """(status, headers, body); status 0 on a network error. Retries server errors and network errors."""
    last = (0, {}, b"")
    for attempt in range(RETRIES + 1):
        req = urllib.request.Request(url, method=method, headers=dict(headers or {}, **{"User-Agent": "check-artifacts"}))
        try:
            with OPENER.open(req, timeout=TIMEOUT) as r:
                return r.status, r.headers, (r.read() if want_body else b"")
        except urllib.error.HTTPError as e:
            last = (e.code, e.headers, e.read() if want_body else b"")
            if e.code < 500 and e.code != 429:
                return last
        except Exception as e:  # timeouts, DNS, TLS
            last = (0, {}, str(e).encode())
        if attempt < RETRIES:
            time.sleep(1 + attempt)
    return last


def get_json(url, headers=None):
    status, _, body = http("GET", url, headers, True)
    if status != 200:
        return status, None
    try:
        return status, json.loads(body.decode("utf-8"))
    except ValueError:
        return status, None


def head(url):
    status, h, _ = http("HEAD", url)
    if status in (403, 405, 501):  # some hosts refuse HEAD
        status, h, _ = http("GET", url, {"Range": "bytes=0-0"})
        if status == 206:
            status = 200
    return status, h


# --- container registries

def parse_ref(ref):
    digest = None
    if "@" in ref:
        ref, digest = ref.split("@", 1)
    tag = None
    last = ref.rsplit("/", 1)[-1]
    if ":" in last:
        ref, tag = ref.rsplit(":", 1)
    parts = ref.split("/")
    if len(parts) > 1 and ("." in parts[0] or ":" in parts[0] or parts[0] == "localhost"):
        host, repo = parts[0], "/".join(parts[1:])
    else:
        host, repo = "docker.io", ref
        if "/" not in repo:
            repo = "library/" + repo
    if host == "docker.io":
        host = "registry-1.docker.io"
    if digest is None and tag is None:
        tag = "latest"
    return host, repo, tag, digest


def registry_head(ref, reference=None):
    """(status, digest header) of the manifest of an image reference."""
    host, repo, tag, digest = parse_ref(ref)
    reference = reference or digest or tag
    url = "https://%s/v2/%s/manifests/%s" % (host, repo, reference)
    hdr = {"Accept": MANIFEST_ACCEPT}
    status, h, _ = http("HEAD", url, hdr)
    if status == 401:
        chal = h.get("WWW-Authenticate", "")
        m = re.match(r'Bearer realm="([^"]+)"(?:,service="([^"]*)")?', chal)
        if not m:
            return status, None
        q = {"scope": "repository:%s:pull" % repo}
        if m.group(2):
            q["service"] = m.group(2)
        st, tok = get_json(m.group(1) + "?" + urllib.parse.urlencode(q))
        if tok:
            hdr["Authorization"] = "Bearer " + (tok.get("token") or tok.get("access_token") or "")
            status, h, _ = http("HEAD", url, hdr)
    return status, h.get("Docker-Content-Digest") if h else None


# ---------------------------------------------------------------------------
# Checks; each returns a list of (status, detail)

def check_image(it):
    ref = it["ref"]
    if "$" in ref:
        return [("OK", "skipped, reference has a variable")]
    host, repo, tag, digest = parse_ref(ref)
    status, _ = registry_head(ref)
    if status != 200:
        return [("FAIL", "manifest %s not available (HTTP %s)" % (digest or tag, status))]
    if digest:
        return [("OK", "digest available")]
    return [("OK", "tag available (not pinned by digest)")]


def packument(name):
    return get_json("https://registry.npmjs.org/" + name.replace("/", "%2f"))


def check_npm(it):
    res = []
    name, version = it["name"], it["version"]
    url = it.get("resolved")
    pk = None
    if it.get("direct"):
        st, pk = packument(name)
        if pk is None:
            return [("FAIL", "packument not available (HTTP %s)" % st)]
        v = pk.get("versions", {}).get(version)
        if v is None:
            return [("FAIL", "version %s not published (any more)" % version)]
        url = url or v.get("dist", {}).get("tarball")
        if v.get("deprecated"):
            res.append(("WARN", "deprecated: %s" % str(v["deprecated"]).strip()))
        maint = len(pk.get("maintainers") or [])
        st, dl = get_json("https://api.npmjs.org/downloads/point/last-week/" + name.replace("/", "%2f"))
        n = dl.get("downloads") if dl else None
        if maint == 1 and n is not None and n < LOW_DOWNLOADS:
            res.append(("WARN", "could be unpublished by its author (1 maintainer, %d downloads last week)" % n))
    if url:
        st, _ = head(url)
        if st != 200:
            res.append(("FAIL", "tarball not available (HTTP %s)" % st))
    if not any(s == "FAIL" for s, _ in res):
        res.insert(0, ("OK", "available"))
    return res


def check_pypi(it):
    st, data = get_json("https://pypi.org/pypi/%s/%s/json" % (urllib.parse.quote(it["name"]), urllib.parse.quote(it["version"])))
    if data is None:
        return [("FAIL", "not available on PyPI (HTTP %s)" % st)]
    res = []
    files = data.get("urls", [])
    have = set(f["digests"]["sha256"] for f in files)
    missing = [h for h in it.get("hashes", []) if h not in have]
    if missing:
        res.append(("FAIL", "%d of %d locked hashes are not among the files of PyPI" % (len(missing), len(it["hashes"]))))
    if any(f.get("yanked") for f in files):
        res.append(("WARN", "yanked"))
    if not res:
        res.append(("OK", "available, %d hashes match" % len(it["hashes"]) if it.get("hashes") else "available"))
    return res


def go_escape(path):
    return re.sub(r"[A-Z]", lambda m: "!" + m.group(0).lower(), path)


def check_go(it):
    st, data = get_json("https://proxy.golang.org/%s/@v/%s.info" % (go_escape(it["name"]), go_escape(it["version"])))
    if data is None:
        return [("FAIL", "not available at proxy.golang.org (HTTP %s)" % st)]
    return [("OK", "available")]


def check_typst(it):
    st, _ = head("https://packages.typst.org/preview/%s-%s.tar.gz" % (it["name"], it["version"]))
    return [("OK", "available")] if st == 200 else [("FAIL", "not available (HTTP %s)" % st)]


_release_cache = {}


def github_digest(url, filename):
    m = re.match(r"https://github\.com/([^/]+/[^/]+)/releases/download/([^/]+)/", url)
    if not m:
        return None, "not a GitHub release URL"
    key = (m.group(1), m.group(2))
    if key not in _release_cache:
        hdr = {"Accept": "application/vnd.github+json"}
        if os.environ.get("GITHUB_TOKEN"):
            hdr["Authorization"] = "Bearer " + os.environ["GITHUB_TOKEN"]
        st, data = get_json("https://api.github.com/repos/%s/releases/tags/%s" % key, hdr)
        _release_cache[key] = (st, data)
    st, data = _release_cache[key]
    if data is None:
        return None, "GitHub API HTTP %s (set GITHUB_TOKEN against rate limits)" % st
    for a in data.get("assets", []):
        if a.get("name") == filename:
            return a.get("digest"), None
    return None, "asset not in the release"


def check_download(it):
    st, _ = head(it["url"])
    if st != 200:
        return [("FAIL", "not available (HTTP %s)" % st)]
    if not it.get("sha"):
        return [("WARN", "available, but no sha256 ARG belongs to it")]
    digest, why = github_digest(it["url"], it["name"])
    if digest is None:
        return [("WARN", "available; digest not compared: %s" % why)]
    if digest != "sha256:" + it["sha"]:
        return [("FAIL", "sha256 ARG %s differs from the release asset %s" % (it["sha"], digest))]
    return [("OK", "available, sha256 matches the release asset")]


def check_apt(items):
    names = sorted(set(i["name"] for i in items))
    url = "https://api.ftp-master.debian.org/madison?f=json&s=bookworm&package=" + urllib.parse.quote(" ".join(names))
    st, data = get_json(url)
    if data is None:
        return dict((n, [("FAIL", "madison not reachable (HTTP %s)" % st)]) for n in names)
    found = {}
    for entry in data:
        for pkg, suites in entry.items():
            for suite, versions in suites.items():
                if suite.endswith("-debug"):
                    continue
                for ver in versions:
                    found.setdefault(pkg, ver)
    return dict((n, [("OK", "bookworm has %s" % found[n])] if n in found else [("FAIL", "not in bookworm")]) for n in names)


CHECKS = {"image": check_image, "npm": check_npm, "pypi": check_pypi, "go": check_go, "typst": check_typst,
          "download": check_download}


def run_check(it):
    try:
        return CHECKS[it["kind"]](it)
    except Exception as e:  # a bug here must not look like a missing artifact
        return [("FAIL", "check error: %s" % e)]


def suggest(items, jobs):
    todo = [i for i in items if i["kind"] == "image" and "$" not in i["ref"] and parse_ref(i["ref"])[2]]
    def one(i):
        host, repo, tag, digest = parse_ref(i["ref"])
        st, d = registry_head(i["ref"], tag)
        return i, st, d
    with concurrent.futures.ThreadPoolExecutor(max_workers=jobs) as ex:
        for i, st, d in ex.map(one, todo):
            host, repo, tag, digest = parse_ref(i["ref"])
            base = i["ref"].split("@", 1)[0]
            now = "sha256:..." if not d else d
            mark = "same" if digest == d else ("new" if digest else "unpinned")
            print("%s\t%s@%s\t%s" % (mark, base, now, i["src"]))


# ---------------------------------------------------------------------------

def main():
    ap = argparse.ArgumentParser(description="report whether every pinned artifact of the agent images is available")
    ap.add_argument("--list", action="store_true", help="print the inventory without network access")
    ap.add_argument("-v", action="store_true", help="print OK lines, too")
    ap.add_argument("--jobs", type=int, default=8)
    ap.add_argument("--compose", action="append", default=[], metavar="FILE",
                    help="another compose file; its agent_* services are checked (e.g. the platform's docker-compose.yml)")
    ap.add_argument("--npm", action="append", default=[], metavar="NAME@VERSION", help="check one more npm package")
    ap.add_argument("--suggest-digests", action="store_true", help="print the current index digest of each tagged image")
    a = ap.parse_args()
    try:
        items = inventory(a.compose, a.npm)
    except InputError as e:
        print("check-artifacts: %s" % e, file=sys.stderr)
        return 2

    if a.list:
        for it in items:
            print("%s\t%s" % (it["kind"], label(it)))
        return 0
    if a.suggest_digests:
        suggest(items, a.jobs)
        return 0

    apt_items = [i for i in items if i["kind"] == "apt"]
    todo = [i for i in items if i["kind"] != "apt"]
    results = []
    with concurrent.futures.ThreadPoolExecutor(max_workers=max(1, a.jobs)) as ex:
        futs = [(i, ex.submit(run_check, i)) for i in todo]
        apt_res = check_apt(apt_items) if apt_items else {}
        for i, f in futs:
            results.append((i, f.result()))
    for i in apt_items:
        results.append((i, apt_res.get(i["name"], [("FAIL", "no result")])))

    counts = {"OK": 0, "WARN": 0, "FAIL": 0}
    lines = []
    for i, res in results:
        worst = "FAIL" if any(s == "FAIL" for s, _ in res) else ("WARN" if any(s == "WARN" for s, _ in res) else "OK")
        counts[worst] += 1
        for s, detail in res:
            if s == "OK" and worst != "OK":
                continue  # the problem lines say it
            if s != "OK" or a.v:
                lines.append("%s\t%s\t%s\t%s" % (s, i["kind"], label(i), detail))
    order = {"FAIL": 0, "WARN": 1, "OK": 2}
    for line in sorted(lines, key=lambda l: (order[l.split("\t", 1)[0]], l)):
        print(line)
    print("artifacts: %d ok, %d warn, %d fail" % (counts["OK"], counts["WARN"], counts["FAIL"]))
    return 1 if counts["FAIL"] else 0


if __name__ == "__main__":
    sys.exit(main())
