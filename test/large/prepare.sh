#!/bin/sh
# Downloads two public sample databases and registers them as bases.
#
#   sudo sh prepare.sh [workdir]        (default workdir: /var/tmp/db-branch-large)
#
#   employees  mysql@8.0.46   MySQL "employees" sample, datacharmer/test_db v1.0.7
#                             (CC BY-SA 3.0), 6 tables, about 3.9 million rows
#   airlines   postgres@18.6  Postgres Pro "demo" database, big edition (2017-08-15),
#                             8 tables, about 21 million rows, about 2.5 GB on disk
#
# Run it on the node that should be the bases' origin. Each base is skipped
# when it already exists. Needs about 1.5 GB in workdir while it runs.
set -eu
W=${1:-/var/tmp/db-branch-large}
mkdir -p "$W"
cd "$W"

has_base() { db-branch base show "$1" >/dev/null 2>&1; }

if ! has_base employees; then
	[ -f test_db-1.0.7.tar.gz ] || curl -fsSL -o test_db-1.0.7.tar.gz \
		https://github.com/datacharmer/test_db/archive/refs/tags/v1.0.7.tar.gz
	rm -rf test_db-1.0.7 && tar xzf test_db-1.0.7.tar.gz
	# employees.sql loads its data with mysql's `source`; inline those files
	# so the dump is one stream (and leave out the timing report).
	python3 - test_db-1.0.7 employees.sql.gz <<'EOF'
import gzip, os, re, sys
src, out = sys.argv[1], sys.argv[2]
with gzip.open(out, "wt") as o:
    for line in open(os.path.join(src, "employees.sql")):
        m = re.match(r"\s*source\s+(\S+)\s*;", line)
        if not m:
            o.write(line)
        elif m.group(1) != "show_elapsed.sql":
            o.write(open(os.path.join(src, m.group(1))).read() + "\n")
EOF
	rm -rf test_db-1.0.7
	db-branch base create employees --runtime mysql@8.0.46 --dump "$W/employees.sql.gz"
fi

if ! has_base airlines; then
	[ -f demo-big-en.zip ] || curl -fsSL -o demo-big-en.zip https://edu.postgrespro.com/demo-big-en.zip
	# The 2017 dump drops the database without IF EXISTS and sets
	# default_with_oids, which PostgreSQL 12 and later no longer know.
	unzip -p demo-big-en.zip | sed -e 's/^DROP DATABASE demo;/DROP DATABASE IF EXISTS demo;/' \
		-e '/^SET default_with_oids/d' | gzip >airlines.sql.gz
	db-branch base create airlines --runtime postgres@18.6 --dump "$W/airlines.sql.gz"
fi

echo "bases ready; the dumps stay in $W (delete them when no longer needed)"
