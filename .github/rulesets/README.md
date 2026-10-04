# Repository rulesets

`master-gate.json` is the branch ruleset enforced on `master`:

- **Required status checks** — `build / vet / test / lint` and
  `shim contract (no daemon, no network)` must both pass.
- **No force-pushes** (`non_fast_forward`) to `master`.
- **Owner bypass** — the repository owner role bypasses, so a stuck rule can never
  block a fix from landing.

## Current state: APPLIED

The repository was **private** while this file was first written, and GitHub gates
both branch protection and rulesets behind Pro for private repositories — the
`POST /rulesets` call returned *"Upgrade to GitHub Pro or make this repository
public"*. The repo was subsequently made **public**, and the ruleset applied:

```bash
gh api repos/Belphemur/pi-supervisor/rulesets \
  --method POST --input .github/rulesets/master-gate.json
# → {"id":24452137, "enforcement":"active", ...}
```

**If the repo is made private again, this ruleset stops being enforced** (GitHub
disables it rather than deleting it). Re-check after any visibility change:

```bash
gh api repos/Belphemur/pi-supervisor/rulesets --jq '.[] | {id, enforcement}'
```

Until CI has reported at least once, `do_not_enforce_on_create: true` lets a PR be
opened; from the second push onward the checks are mandatory.

## Why a local pre-push hook also exists

`.githooks/pre-push` runs the same gates before the push, because a ruleset only
gates the *merge*, not the *commit*. It is the faster feedback loop, and it is
what keeps a failing tree from reaching the remote at all:

```bash
git config core.hooksPath .githooks
git push --no-verify      # deliberate escape hatch
```

The hook and `.github/workflows/ci.yml` list the same gates on purpose — a gate
that differs between the two is a gate nobody can trust.