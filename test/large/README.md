# Large-dataset check

Checks that branches of bigger databases behave on a real cluster: creation on every node holding the base,
writes, a check that the base and other branches are not affected, diff (paid module), checkpoint restore,
reset, and a MySQL 8.0 to 8.4 upgrade.

Datasets (downloaded from their publishers, not stored here):

| Base | Runtime | Source | Size |
|---|---|---|---|
| `employees` | mysql@8.0.46 | [datacharmer/test_db](https://github.com/datacharmer/test_db) v1.0.7, CC BY-SA 3.0 | 6 tables, about 3.9 million rows |
| `airlines` | postgres@18.6 | [Postgres Pro demo database](https://postgrespro.com/community/demodb), big edition (2017-08-15) | 8 tables, about 21 million rows, about 2.5 GB |

## Use

On the node that should be the bases' origin, as root:

```sh
sh prepare.sh               # downloads the dumps and creates the bases (skips bases that exist)
python3 large_check.py      # or --only mysql / --only postgres; --keep keeps the lc-* branches
```

The check prints one line per step with PASS or FAIL and the time it took, and exits non-zero when a step fails.
It works through the local agent socket and the servers' local sockets, so it prints no password or DSN.
Every branch it makes is named `lc-*` and deleted at the end.

Needs runtimes mysql@8.0.46, mysql@8.4.13 and postgres@18.6 on the node. The diff steps run when the paid `diff`
module is enabled and are skipped otherwise. They change about 10% of a 2.8-million-row table, so the diff reads
more rows than its default limit (`max_rows`, 5 million) and would end `incomplete`; raise the limit for the run and
restore it afterwards:

```sh
db-branch settings set diff max_rows=50000000
python3 large_check.py
db-branch settings set diff max_rows=
```

Reference run (4-node test cluster, 2-core VMs, 2026-11-13): every step passed; branch creation 0.4 to 4 s,
MySQL diff 56 s, PostgreSQL diff 88 s, MySQL 8.0 to 8.4 upgrade of the employees branch 6 s.
