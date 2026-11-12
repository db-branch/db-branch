## What and why

<!-- What does this change, and why? Link the issue if there is one. -->

## Checklist

- [ ] Every commit is signed off (`git commit -s`), certifying the Developer Certificate of Origin 1.1. The `dco` check fails on unsigned commits; fix with `git rebase --signoff main` and force-push. See CONTRIBUTING.md.
- [ ] Tests were written first (Red, Green, Refactor) and `make cover` passes: coverage stays at 95% or more (`internal/app` is the only exception).
- [ ] `make vet` passes; for console changes, `cd web && npx tsc` passes.
- [ ] Docs are updated if behavior changed.
- [ ] No secrets, hostnames or personal data in code, tests or docs.
