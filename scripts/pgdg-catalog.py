#!/usr/bin/env python3
"""Rewrites the PostgreSQL entries of the runtime catalog
(internal/infrastructure/catalog/runtimes.json) from the PGDG package indexes.

For every distro and architecture it takes the newest build of each major
version (server, client and libpq5 .debs), checks the digest and size of the
downloaded files against the index, and writes one artifact set per distro.
MySQL entries are kept as they are. Verify the PGDG InRelease signature of
the indexes before committing the result (see the catalog package comment).

usage: scripts/pgdg-catalog.py [--majors 19 18 17] [--no-download]
"""
import argparse
import functools
import gzip
import hashlib
import json
import os
import re
import sys
import tempfile
import urllib.request

BASE = "https://apt.postgresql.org/pub/repos/apt"
# db-branch distro key -> PGDG suite
DISTROS = {"ubuntu-24.04": "noble-pgdg", "ubuntu-26.04": "resolute-pgdg"}
ARCHES = ["amd64", "arm64"]
CATALOG = os.path.join(os.path.dirname(__file__), "..", "internal/infrastructure/catalog/runtimes.json")


def index(suite, arch):
    with urllib.request.urlopen(f"{BASE}/dists/{suite}/main/binary-{arch}/Packages.gz") as r:
        text = gzip.decompress(r.read()).decode()
    pkgs = {}
    for para in text.split("\n\n"):
        f = dict(re.findall(r"^([A-Za-z0-9-]+): (.*)$", para, re.M))
        if "Package" in f:
            pkgs.setdefault(f["Package"], []).append(f)
    return pkgs


def _isdigit(c):
    return "0" <= c <= "9"


def _char_order(c):
    if c == "~":
        return -1
    if ("A" <= c <= "Z") or ("a" <= c <= "z"):
        return ord(c)
    return ord(c) + 256


def _split_epoch(v):
    if ":" in v:
        e, rest = v.split(":", 1)
        if not re.fullmatch(r"[0-9]+", e):
            raise ValueError(f"invalid Debian epoch in {v!r}")
        return int(e), rest
    return 0, v


def _split_rev(v):
    if "-" in v:
        return v.rsplit("-", 1)
    return v, ""


def _upstream(v):
    return _split_rev(_split_epoch(v)[1])[0]


def _part_cmp(a, b):
    ia, ib = 0, 0
    la, lb = len(a), len(b)
    while ia < la or ib < lb:
        while (ia < la and not _isdigit(a[ia])) or (ib < lb and not _isdigit(b[ib])):
            ca = _char_order(a[ia]) if ia < la and not _isdigit(a[ia]) else 0
            cb = _char_order(b[ib]) if ib < lb and not _isdigit(b[ib]) else 0
            if ca != cb:
                return -1 if ca < cb else 1
            if ia < la and not _isdigit(a[ia]):
                ia += 1
            if ib < lb and not _isdigit(b[ib]):
                ib += 1
        ja = ia
        while ja < la and _isdigit(a[ja]):
            ja += 1
        jb = ib
        while jb < lb and _isdigit(b[jb]):
            jb += 1
        na = int(a[ia:ja]) if ja > ia else 0
        nb = int(b[ib:jb]) if jb > ib else 0
        if na != nb:
            return -1 if na < nb else 1
        ia, ib = ja, jb
    return 0


def deb_cmp(a, b):
    """Compare two Debian package versions per Debian Policy section 5.6.12."""
    ea, ra = _split_epoch(a)
    eb, rb = _split_epoch(b)
    if ea != eb:
        return -1 if ea < eb else 1
    ua, deba = _split_rev(ra)
    ub, debb = _split_rev(rb)
    c = _part_cmp(ua, ub)
    if c != 0:
        return c
    return _part_cmp(deba, debb)


vkey = functools.cmp_to_key(deb_cmp)


def newest(pkgs, name, major=None):
    c = []
    for p in pkgs.get(name, []):
        up = _upstream(p["Version"])
        if "~" in up or (major is not None and up.split(".")[0] != major):
            continue
        c.append(p)
    if not c:
        raise SystemExit(f"{name} {major}: not in the index")
    return max(c, key=lambda p: vkey(p["Version"]))


def check(p, download):
    if not download:
        return
    h, n = hashlib.sha256(), 0
    with urllib.request.urlopen(f"{BASE}/{p['Filename']}") as r:
        while chunk := r.read(1 << 20):
            h.update(chunk)
            n += len(chunk)
    if h.hexdigest() != p["SHA256"] or n != int(p["Size"]):
        raise SystemExit(f"{p['Filename']}: downloaded file does not match the index")


def part(p):
    return {"url": f"{BASE}/{p['Filename']}", "sha256": p["SHA256"], "size": int(p["Size"])}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--majors", nargs="+", default=["19", "18", "17"])
    ap.add_argument("--no-download", action="store_true")
    a = ap.parse_args()
    with open(CATALOG) as f:
        cat = json.load(f)
    idx = {(d, arch): index(s, arch) for d, s in DISTROS.items() for arch in ARCHES}
    rts = [r for r in cat["runtimes"] if r["engine"] != "postgres"]
    libpq_version = None
    for major in a.majors:
        arts = {}
        version = None
        for arch in ARCHES:
            arts[arch] = {"distros": {}}
            for d in DISTROS:
                pk = idx[(d, arch)]
                srv = newest(pk, f"postgresql-{major}", major)
                v = _upstream(srv["Version"])
                if version not in (None, v):
                    raise SystemExit(f"postgres {major}: {d}/{arch} is at {v}, others at {version}")
                version = v
                parts = [srv, newest(pk, f"postgresql-client-{major}", major), newest(pk, "libpq5")]
                for p in parts:
                    check(p, not a.no_download)
                arts[arch]["distros"][d] = {"parts": [part(p) for p in parts]}
        rts.append({"engine": "postgres", "version": version, "artifacts": arts})
    cat["runtimes"] = rts
    with open(CATALOG, "w") as f:
        json.dump(cat, f, indent=2)
        f.write("\n")


if __name__ == "__main__":
    main()
