# Security Policy

## Supported versions

Only the latest release receives security fixes. Please upgrade to the newest version before reporting, if you can.

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub: open the **Security** tab of this repository and choose **Report a vulnerability**. Do not open a public issue or pull request for a security problem.

Please include:

- the db-branch version (`db-branch version`), Ubuntu version and database engine/version
- a description of the problem and its impact
- steps to reproduce, or a proof of concept
- whether the issue is already public, and any suggested fix

## What to expect

This is a small project and the response is best effort, with no SLA and no guaranteed response times. Reports are read when possible. We follow coordinated disclosure: we ask that you keep the details private until a fix is released, and we will credit you in the release notes if you wish.

## Scope

This repository is the Apache-2.0 core, including the free modules shipped from it. The paid modules (auth/SSO, audit, history, diff, FSx storage) are separate, closed-source packages and are not in this repository, but you may report issues in them through the same channel.

Out of scope: vulnerabilities in third-party software that db-branch installs or drives (MySQL, PostgreSQL, ProxySQL, ZFS) should be reported to those projects, and findings that need a `root` or `db-branch-admin` group account on the node, which are already trusted. Privilege escalation from the unprivileged `db-branch` or `db-branch-rt-*` database service accounts/groups to `db-branch-admin` or `root` is in scope.
