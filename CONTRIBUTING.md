# Contributing to db-branch

Thank you for helping. Before you open a pull request:

1. Read `CLAUDE.md` (development process: test-first, coverage ≥ 95%, clean architecture).
2. Run `make cover` and, for the console, `cd web && npx tsc`.

## Developer Certificate of Origin (DCO)

Every commit must be signed off, certifying the [Developer Certificate of
Origin 1.1](https://developercertificate.org/): that you wrote the change or
otherwise have the right to submit it under the project's licence
(Apache-2.0). Add the line with `git commit -s`:

    Signed-off-by: Your Name <you@example.com>

Use your real name and an address you can be reached at (a GitHub noreply
address is fine). The `dco` check fails a pull request with an unsigned
commit; fix it with `git rebase --signoff main` and force-push.

Contributions are licensed under Apache-2.0 (section 5 of the licence), as
is the rest of the repository.
