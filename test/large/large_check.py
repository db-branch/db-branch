#!/usr/bin/env python3
"""Large-dataset check for a db-branch cluster.

Run as root on a cluster node that holds both bases (the bases' origin node):

    sudo python3 large_check.py [--only mysql|postgres] [--keep]

Bases (see README.md next to this file for how they are made):
  employees  mysql@8.0.46   MySQL "employees" sample (datacharmer/test_db, CC BY-SA 3.0), ~3.9M rows
  airlines   postgres@18.6  Postgres Pro "demo" database, big edition (2017-08-15), ~21M rows

Every branch this script makes is named lc-* and deleted at the end (unless --keep).
Secrets are never printed: SQL goes through the servers' local sockets as root/postgres.
"""
import argparse
import http.client
import json
import os
import re
import socket
import subprocess
import sys
import time

CLI = "db-branch"
NODE = socket.gethostname()
AGENT_SOCK = "/run/db-branch/agent.sock"
MYSQL_BIN = "/var/lib/db-branch/runtimes/mysql/{v}/bin/mysql"
PSQL_BIN = "/var/lib/db-branch/runtimes/postgres/{v}/bin/psql"

EMPLOYEES = {  # documented row counts of the sample
    "employees": 300024, "departments": 9, "dept_manager": 24,
    "dept_emp": 331603, "titles": 443308, "salaries": 2844047,
}
AIRLINES = ["bookings", "tickets", "ticket_flights", "flights", "boarding_passes",
            "airports_data", "aircrafts_data", "seats"]

class SkipDiff(Exception):
    pass


results = []
made = []


def step(name, ok, detail="", secs=None):
    results.append((name, ok, detail, secs))
    mark = "PASS" if ok else "FAIL"
    t = f"{secs:7.1f}s" if secs is not None else "        "
    print(f"[{mark}] {t}  {name}  {detail}", flush=True)


def run(*args, inp=None, check=True):
    p = subprocess.run(args, input=inp, capture_output=True, text=True)
    if check and p.returncode != 0:
        raise RuntimeError(f"{args[0]} {args[1] if len(args) > 1 else ''}: {(p.stderr or p.stdout).strip()[-400:]}")
    return p.stdout


def timed(f, *a, **k):
    s = time.time()
    out = f(*a, **k)
    return out, time.time() - s


def api(method, path, body=None):
    c = http.client.HTTPConnection("localhost")
    c.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    c.sock.connect(AGENT_SOCK)
    c.request(method, path, body=json.dumps(body) if body is not None else None,
              headers={"Content-Type": "application/json"})
    r = c.getresponse()
    data = r.read()
    if r.status >= 300:
        raise RuntimeError(f"{method} {path}: {r.status} {data[:300]!r}")
    return json.loads(data) if data else None


def branch(name):
    return api("GET", f"/v1/branches/{name}")


def create(name, base, node=None):
    args = [CLI, "branch", "create", name, "--base", base, "--ttl", "3h"]
    if node:
        args += ["--node", node]
    made.append(name)
    _, secs = timed(run, *args)
    return secs


def delete(name):
    run(CLI, "branch", "delete", name, check=False)
    if name in made:
        made.remove(name)


def own_bytes(name):
    b = branch(name)
    for k in ("storage", "usage"):
        v = b.get(k)
        if isinstance(v, dict):
            for kk in ("own", "own_bytes", "used"):
                if kk in v:
                    return v[kk]
    return None


def human(n):
    if n is None:
        return "?"
    for u in ("B", "KiB", "MiB", "GiB"):
        if n < 1024:
            return f"{n:.1f} {u}"
        n /= 1024
    return f"{n:.1f} TiB"


def runtime_version(name):
    rt = branch(name).get("runtime") or ""
    if isinstance(rt, dict):
        return rt.get("version", "")
    return rt.split("@", 1)[-1]


# ---------------------------------------------------------------- engines
def my_sql(name, sql, kind="branch"):
    v = runtime_version(name) if kind == "branch" else "8.0.46"
    sock = f"/run/db-branch-mysqld/{kind}-{name}/mysqld.sock"
    return run(MYSQL_BIN.format(v=v), f"--socket={sock}", "-uroot", "-N", "-B", "employees", "-e", sql).strip()


def pg_port(name, kind="branch"):
    d = f"/run/db-branch-postgres/{kind}-{name}"
    for f in os.listdir(d):
        m = re.match(r"\.s\.PGSQL\.(\d+)$", f)
        if m:
            return d, m.group(1)
    raise RuntimeError(f"no socket in {d}")


def pg_sql(name, sql, kind="branch"):
    v = runtime_version(name) if kind == "branch" else "18.6"
    d, port = pg_port(name, kind)
    return run("env", "LD_LIBRARY_PATH=/var/lib/db-branch/runtimes/postgres/%s/lib" % v, PSQL_BIN.format(v=v), "-h", d, "-p", port, "-U", "postgres", "-d", "demo",
               "-X", "-q", "-A", "-t", "-v", "ON_ERROR_STOP=1", "-c", sql).strip()


def my_counts(name, kind="branch"):
    q = " UNION ALL ".join(f"SELECT '{t}', COUNT(*) FROM {t}" for t in EMPLOYEES)
    return {l.split("\t")[0]: int(l.split("\t")[1]) for l in my_sql(name, q, kind).splitlines()}


def pg_counts(name, kind="branch"):
    q = " UNION ALL ".join(f"SELECT '{t}', COUNT(*) FROM bookings.{t}" for t in AIRLINES)
    return {l.split("|")[0]: int(l.split("|")[1]) for l in pg_sql(name, q, kind).splitlines()}


# ---------------------------------------------------------------- shared steps
def holders(base):
    b = api("GET", f"/v1/bases/{base}")
    return [h["node"] for h in b.get("holders", []) if h.get("state") == "held"]


def create_everywhere(base, tag):
    for n in holders(base):
        name = f"lc-{tag}-on-{n[-3:]}"
        try:
            secs = create(name, base, n)
            st = branch(name)["status"]
            step(f"{base}: create on {n}", st == "ready" and secs < 15, f"status {st}", secs)
        except Exception as e:
            step(f"{base}: create on {n}", False, str(e))
        delete(name)


def checkpoint(name, label):
    run(CLI, "branch", "checkpoint", name, "--name", label)


def restore(name, label):
    _, secs = timed(run, CLI, "branch", "restore", name, label)
    return secs


def reset(name):
    _, secs = timed(run, CLI, "branch", "reset", name)
    return secs


def has_diff():
    try:
        api("GET", "/v1/ext/diff/status")
        return True
    except Exception:
        return False


def diff(a, b, timeout=1800):
    r = api("POST", "/v1/ext/diff/compare", {"a": a, "b": b, "samples": 3})
    url = r["result_url"]
    s = time.time()
    while True:
        res = api("GET", url)
        if res.get("state") != "running":
            return res, time.time() - s
        if time.time() - s > timeout:
            raise RuntimeError("diff timed out")
        time.sleep(2)


# ---------------------------------------------------------------- MySQL
def mysql_suite():
    print("\n== MySQL: employees (mysql@8.0.46)")
    create_everywhere("employees", "my")

    a, b = "lc-my-a", "lc-my-b"
    secs = create(a, "employees", NODE)
    got = my_counts(a)
    step("employees: branch A has the sample's rows", got == EMPLOYEES, f"own {human(own_bytes(a))} {'' if got == EMPLOYEES else json.dumps(got)}", secs)
    secs = create(b, "employees", NODE)
    step("employees: branch B", True, "", secs)

    # heavy writes on A only
    (_, s1) = timed(my_sql, a, "UPDATE salaries SET salary = salary + 1 WHERE emp_no % 10 = 0")
    upd = int(my_sql(a, "SELECT COUNT(*) FROM salaries WHERE emp_no % 10 = 0"))
    (_, s2) = timed(my_sql, a, "DELETE FROM titles WHERE emp_no < 10500")
    deleted = EMPLOYEES["titles"] - int(my_sql(a, "SELECT COUNT(*) FROM titles"))
    (_, s3) = timed(my_sql, a, "ALTER TABLE employees ADD COLUMN note VARCHAR(20) NULL")
    step("employees: writes on A", True,
         f"{upd} salaries updated ({s1:.1f}s), {deleted} titles deleted ({s2:.1f}s), column added ({s3:.1f}s); own {human(own_bytes(a))}",
         s1 + s2 + s3)
    c = "lc-my-c"
    create(c, "employees", NODE)
    step("employees: a new branch after the writes is unchanged (base untouched)", my_counts(c) == EMPLOYEES)
    delete(c)
    step("employees: branch B unchanged", my_counts(b) == EMPLOYEES)

    try:
        if not has_diff():
            raise SkipDiff()
        res, secs = diff(a, b)
        sm = res.get("summary", {})
        ok = (res.get("state") == "done" and not res.get("equal")
              and sm.get("rows_changed") == upd and sm.get("rows_only_b") == deleted
              and sm.get("schema_differences", 0) >= 1)
        step("employees: diff A vs B finds exactly the writes", ok,
             f"changed {sm.get('rows_changed')} (want {upd}), only B {sm.get('rows_only_b')} (want {deleted}), "
             f"schema {sm.get('schema_differences')}, read {sm.get('rows_read')}, state {res.get('state')}", secs)
    except SkipDiff:
        print("[SKIP]           employees: diff (the diff module is not enabled)")
    except Exception as e:
        step("employees: diff A vs B", False, str(e))

    after = my_counts(a)
    checkpoint(a, "lc-after-writes")
    my_sql(a, "DELETE FROM salaries WHERE emp_no < 20000")
    secs = restore(a, "lc-after-writes")
    step("employees: checkpoint restore", my_counts(a) == after, "", secs)
    secs = reset(a)
    step("employees: reset back to the base", my_counts(a) == EMPLOYEES, "", secs)

    # engine upgrade on a large branch
    try:
        out, secs = timed(run, CLI, "branch", "upgrade", b, "--runtime", "mysql@8.4.13")
        cp = re.search(r"(before-upgrade-\S+)", out)
        v = my_sql(b, "SELECT VERSION()")
        step("employees: upgrade B to 8.4.13", v.startswith("8.4") and my_counts(b) == EMPLOYEES, f"version {v}", secs)
        if cp:
            secs = restore(b, cp.group(1).rstrip(";."))
            v = my_sql(b, "SELECT VERSION()")
            step("employees: back to 8.0 from before-upgrade", v.startswith("8.0") and my_counts(b) == EMPLOYEES,
                 f"version {v}", secs)
    except Exception as e:
        step("employees: upgrade", False, str(e))


# ---------------------------------------------------------------- PostgreSQL
def pg_suite():
    print("\n== PostgreSQL: airlines (postgres@18.6)")
    create_everywhere("airlines", "pg")

    a, b = "lc-pg-a", "lc-pg-b"
    secs = create(b, "airlines", NODE)
    base = pg_counts(b)
    step("airlines: branch B (reference)", all(v > 0 for v in base.values()), json.dumps(base), secs)
    secs = create(a, "airlines", NODE)
    step("airlines: branch A has the same rows", pg_counts(a) == base, f"own {human(own_bytes(a))}", secs)

    (_, s1) = timed(pg_sql, a, "UPDATE bookings.bookings SET total_amount = total_amount + 1 WHERE book_ref LIKE '%0'")
    upd = int(pg_sql(a, "SELECT COUNT(*) FROM bookings.bookings WHERE book_ref LIKE '%0'"))
    (_, s2) = timed(pg_sql, a, "DELETE FROM bookings.boarding_passes WHERE flight_id < 2000")
    deleted = base["boarding_passes"] - int(pg_sql(a, "SELECT COUNT(*) FROM bookings.boarding_passes"))
    step("airlines: writes on A", True,
         f"{upd} bookings updated ({s1:.1f}s), {deleted} boarding passes deleted ({s2:.1f}s); own {human(own_bytes(a))}",
         s1 + s2)
    c = "lc-pg-c"
    create(c, "airlines", NODE)
    step("airlines: a new branch after the writes is unchanged (base untouched)", pg_counts(c) == base)
    delete(c)
    step("airlines: branch B unchanged", pg_counts(b) == base)

    try:
        if not has_diff():
            raise SkipDiff()
        res, secs = diff(a, b)
        sm = res.get("summary", {})
        ok = (res.get("state") == "done" and not res.get("equal")
              and sm.get("rows_changed") == upd and sm.get("rows_only_b") == deleted)
        step("airlines: diff A vs B finds exactly the writes", ok,
             f"changed {sm.get('rows_changed')} (want {upd}), only B {sm.get('rows_only_b')} (want {deleted}), "
             f"read {sm.get('rows_read')}, state {res.get('state')}", secs)
    except SkipDiff:
        print("[SKIP]           airlines: diff (the diff module is not enabled)")
    except Exception as e:
        step("airlines: diff A vs B", False, str(e))

    after = pg_counts(a)
    checkpoint(a, "lc-after-writes")
    pg_sql(a, "DELETE FROM bookings.ticket_flights WHERE flight_id < 5000 AND NOT EXISTS (SELECT 1 FROM bookings.boarding_passes bp WHERE bp.ticket_no = ticket_flights.ticket_no AND bp.flight_id = ticket_flights.flight_id)")
    secs = restore(a, "lc-after-writes")
    step("airlines: checkpoint restore", pg_counts(a) == after, "", secs)
    secs = reset(a)
    step("airlines: reset back to the base", pg_counts(a) == base, "", secs)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--only", choices=["mysql", "postgres"])
    ap.add_argument("--keep", action="store_true", help="keep the lc-* branches")
    o = ap.parse_args()
    if os.geteuid() != 0:
        sys.exit("run as root (sudo)")
    try:
        if o.only in (None, "mysql"):
            mysql_suite()
        if o.only in (None, "postgres"):
            pg_suite()
    except Exception as e:
        step("unexpected error", False, str(e))
    finally:
        if not o.keep:
            for n in list(made):
                delete(n)
    bad = [r for r in results if not r[1]]
    print(f"\n{len(results) - len(bad)} passed, {len(bad)} failed")
    sys.exit(1 if bad else 0)


if __name__ == "__main__":
    main()
