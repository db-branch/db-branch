#!/usr/bin/env python3
"""Uploads a repository built by scripts/apt-repo.sh to the R2 bucket behind
apt.db-branch.com.

  pool/**                      immutable, cached for a year; a file that is
                               already published with the same size is skipped
                               (a file name is never reused for other content)
  dists/**, db-branch.{asc,gpg}  cached for 60 s (they change on every release)

Order: pool files first, the indexes next, InRelease last, so a client never
reads an index that points to a package that is not there yet.

usage: scripts/publish-apt.py <repo-dir> [--bucket db-branch-apt] [--prefix P]
                              [--public-url https://apt.db-branch.com]
                              [--delete]   (remove what this repo would upload)

Needs CLOUDFLARE_API_TOKEN (R2 edit) and CLOUDFLARE_ACCOUNT_ID in the
environment, or a logged-in `wrangler`.
"""
import argparse
import os
import subprocess
import sys

WRANGLER = "wrangler@4.160.0"  # pinned: the release job runs with secrets

ap = argparse.ArgumentParser()
ap.add_argument("repo")
ap.add_argument("--bucket", default="db-branch-apt")
ap.add_argument("--prefix", default="")
ap.add_argument("--public-url", default="https://apt.db-branch.com")
ap.add_argument("--delete", action="store_true")
a = ap.parse_args()

repo = os.path.abspath(a.repo)
prefix = a.prefix.strip("/")
public = a.public_url.rstrip("/") + ("/" + prefix if prefix else "")


def rels():
    out = []
    for d, _, fs in os.walk(repo):
        for f in fs:
            out.append(os.path.relpath(os.path.join(d, f), repo))
    return sorted(out)


def obj(rel):
    return f"{a.bucket}/{prefix + '/' if prefix else ''}{rel}"


def wrangler(*args):
    r = subprocess.run(["npx", "--yes", WRANGLER, "r2", "object", *args, "--remote"], capture_output=True, text=True)
    if r.returncode != 0:
        sys.exit(f"wrangler {' '.join(args[:2])} failed:\n{r.stderr[-600:]}")


def published_size(rel):
    """Size of the published file, or -1 (unknown or missing). Uses curl so the
    answer does not depend on this Python's certificate store."""
    # Cloudflare answers 403 to some default User-Agents.
    r = subprocess.run(["curl", "-sI", "-A", "db-branch-publish/1.0", "--max-time", "30", f"{public}/{rel}"], capture_output=True, text=True)
    if r.returncode != 0:
        print(f"warning: could not check {rel} ({r.stderr.strip() or 'curl exit ' + str(r.returncode)}); uploading it", file=sys.stderr)
        return -1
    status, size = 0, -1
    for line in r.stdout.splitlines():
        if line.upper().startswith("HTTP/"):
            parts = line.split()
            status = int(parts[1]) if len(parts) > 1 and parts[1].isdigit() else 0
        elif line.lower().startswith("content-length:"):
            try:
                size = int(line.split(":", 1)[1].strip())
            except ValueError:
                size = -1
    return size if status == 200 else -1


files = rels()
pool = [r for r in files if r.startswith("pool/")]
rest = sorted((r for r in files if not r.startswith("pool/")), key=lambda r: (r.endswith("InRelease"), r))

if a.delete:
    for r in pool + rest:
        wrangler("delete", obj(r))
        print("deleted", obj(r))
    sys.exit(0)

new = skipped = 0
for r in pool:
    path = os.path.join(repo, r)
    if published_size(r) == os.path.getsize(path):
        skipped += 1
        continue
    wrangler("put", obj(r), "--file", path, "--cache-control", "public, max-age=31536000, immutable")
    new += 1
    print("uploaded", r)
for r in rest:
    wrangler("put", obj(r), "--file", os.path.join(repo, r), "--cache-control", "public, max-age=60")
    print("uploaded", r)
print(f"done: {new} new pool files, {skipped} already published, {len(rest)} index files")
