#!/usr/bin/env python3
"""Rewrites the PostgreSQL entries of the runtime catalog
(internal/infrastructure/catalog/runtimes.json) from the PGDG package indexes.

For every distro and architecture it takes the newest build of each major
version (server, client and libpq5 .debs), checks the digest and size of the
downloaded files against the index, and writes one artifact set per distro.
MySQL entries are kept as they are. Verify the PGDG InRelease signature of
the indexes before committing the result (see the catalog package comment).

usage: scripts/pgdg-catalog.py [--majors 18 17] [--no-download]
"""
import argparse
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


def vkey(v):
    return [int(x) for x in re.findall(r"\d+", v)]


def newest(pkgs, name, major=None):
    c = [p for p in pkgs.get(name, []) if major is None or p["Version"].split(".")[0] == major]
    if not c:
        raise SystemExit(f"{name} {major}: not in the index")
    return max(c, key=lambda p: vkey(p["Version"]))


def check(p, download):
    if not download:
        return
    h, n = hashlib.sha256(), 0
    with urllib.request.urlopen(f"{BASE}/{p["Filename"]}") as r:
        while chunk := r.read(1 << 20):
            h.update(chunk)
            n += len(chunk)
    if h.hexdigest() != p["SHA256"] or n != int(p["Size"]):
        raise SystemExit(f"{p["Filename"]}: downloaded file does not match the index")


def part(p):
    return {"url": f"{BASE}/{p["Filename"]}", "sha256": p["SHA256"], "size": int(p["Size"])}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--majors", nargs="+", default=["18", "17"])
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
                v = srv["Version"].split("-")[0]
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
