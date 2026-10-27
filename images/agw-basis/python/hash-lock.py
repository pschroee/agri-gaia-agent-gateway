#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 Philipp Schröer
#
# SPDX-License-Identifier: MIT
"""Write the --hash lines of a pip lock file.

usage: hash-lock.py LOCKFILE [--python 311] [--arch x86_64,aarch64]

LOCKFILE holds one `name==version` per requirement (hash lines and backslashes from an earlier run are ignored; comment
lines at the top are kept). For each requirement the script asks https://pypi.org/pypi/<name>/<version>/json and writes
the sha256 of every wheel that pip would install on Debian 12 (glibc 2.36) for CPython <--python>: the tags
cp<python>, abi3 and py3 on `any` or manylinux for each architecture in --arch. Source distributions are never used
(the image installs with --only-binary :all:). Exit 1 if a requirement has no such wheel for an architecture, is
yanked or is missing on PyPI; the file is then left untouched.

To bump: change the versions in the file (e.g. from `pip freeze` of a tested image), run `python3 -I hash-lock.py
requirements.lock`, review the diff.
"""
import argparse
import json
import re
import sys
import urllib.error
import urllib.request

GLIBC_MINOR = 36  # Debian 12 (bookworm)


def fetch(name, version):
    url = "https://pypi.org/pypi/%s/%s/json" % (name, version)
    last = None
    for _ in range(3):
        try:
            with urllib.request.urlopen(url, timeout=30) as r:
                return json.load(r)
        except urllib.error.HTTPError as e:
            if e.code == 404:
                return None
            last = e
        except Exception as e:  # network hiccup: retry
            last = e
    raise SystemExit("hash-lock: %s==%s: %s" % (name, version, last))


def wheel_tags(filename):
    # name-version(-build)?-python-abi-platform.whl
    parts = filename[:-4].split("-")
    if len(parts) < 5:
        return None
    return parts[-3].split("."), parts[-2].split("."), parts[-1].split(".")


def manylinux_ok(plat, arch):
    if not plat.endswith("_" + arch):
        return False
    m = re.match(r"manylinux_2_(\d+)_", plat)
    if m:
        return int(m.group(1)) <= GLIBC_MINOR
    return plat.startswith(("manylinux1_", "manylinux2010_", "manylinux2014_"))


def fits(filename, py, arch):
    tags = wheel_tags(filename)
    if tags is None:
        return False
    pys, abis, plats = tags
    py_ok = False
    for p in pys:
        if p == "cp" + py or p == "py3" or p == "py2":
            py_ok = p != "py2" or "py3" in pys
        elif re.fullmatch(r"cp3\d+", p) and "abi3" in abis and int(p[3:]) <= int(py[1:]):
            py_ok = True
        if py_ok:
            break
    if not py_ok:
        return False
    if "cp" + py in pys or "abi3" in abis:
        pass
    elif "none" not in abis:
        return False
    return any(p == "any" or manylinux_ok(p, arch) for p in plats)


def main():
    ap = argparse.ArgumentParser(description="write --hash lines of a pip lock file")
    ap.add_argument("lockfile")
    ap.add_argument("--python", default="311")
    ap.add_argument("--arch", default="x86_64,aarch64")
    a = ap.parse_args()
    archs = a.arch.split(",")

    header, reqs = [], []
    for line in open(a.lockfile, encoding="utf-8"):
        s = line.strip().rstrip("\\").strip()
        if not s or s.startswith("--hash="):
            continue
        if s.startswith("#"):
            if not reqs:
                header.append(line.rstrip("\n"))
            continue
        m = re.fullmatch(r"([A-Za-z0-9][A-Za-z0-9._-]*)==([A-Za-z0-9.+!_-]+)", s)
        if not m:
            raise SystemExit("hash-lock: cannot parse %r" % s)
        reqs.append((m.group(1), m.group(2)))

    out, bad = [], []
    for name, version in reqs:
        data = fetch(name, version)
        if data is None:
            bad.append("%s==%s: not on PyPI" % (name, version))
            continue
        hashes = set()
        for arch in archs:
            got = set()
            for f in data["urls"]:
                if f["packagetype"] != "bdist_wheel" or f.get("yanked"):
                    continue
                if fits(f["filename"], a.python, arch):
                    got.add(f["digests"]["sha256"])
            if not got:
                bad.append("%s==%s: no wheel for cp%s %s" % (name, version, a.python, arch))
            hashes |= got
        if data["info"].get("yanked"):
            bad.append("%s==%s: yanked (%s)" % (name, version, data["info"].get("yanked_reason") or ""))
        out.append("%s==%s \\\n%s" % (name, version, " \\\n".join("    --hash=sha256:" + h for h in sorted(hashes))))
    if bad:
        print("\n".join("hash-lock: " + b for b in bad), file=sys.stderr)
        sys.exit(1)
    with open(a.lockfile, "w", encoding="utf-8") as f:
        f.write("\n".join(header) + ("\n" if header else "") + "\n".join(out) + "\n")
    print("hash-lock: %d requirements, %s" % (len(out), ",".join(archs)))


if __name__ == "__main__":
    main()
