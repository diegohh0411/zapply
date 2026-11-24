# Self-hosted CI runners

The `go-checks`, `go` and `web` jobs in `.github/workflows/ci.yml` run on
self-hosted runners (label `zapply`): Linux containers in podman on a developer
machine. The `image` job (Docker Buildx, main only) stays on GitHub-hosted
runners. While no runner is online, jobs wait in the queue rather than fail.

```bash
scripts/ci-runner.sh up 3     # build if needed, run 3 runners (default 2)
scripts/ci-runner.sh status   # containers here, and runners as GitHub sees them
scripts/ci-runner.sh logs     # follow the runners' output
scripts/ci-runner.sh down     # stop; each runner unregisters itself from GitHub
scripts/ci-runner.sh rebuild  # pull newer base images, rebuild, restart
scripts/ci-runner.sh prune    # down, and delete the cache volume
```

`up` enables the podman machine's `podman-restart.service`, so the containers
come back when the machine starts, and deletes runners GitHub still lists from
containers that no longer exist.

## What runs where

- **`postgres`**: one throwaway server shared by every runner, data in memory,
  crash safety off. `deploy/postgres/init.sql` creates the runtime roles once at
  startup; each test creates its own database (`internal/testdb`).
- **`runner` × N**: each container registers an ephemeral runner, takes one job
  as the `runner` user (no sudo, no docker), and registers again. The image
  bundles Typst, poppler, psql and a C toolchain for `-race`. Bump
  `TYPST_VERSION` in the `Containerfile` with `templates/*/*/manifest.json`,
  then run `rebuild`.
- **`zapply-ci_cache` volume**: `GOMODCACHE`, `GOCACHE`, npm, and the tool cache
  (Go and Node installs from `setup-go` / `setup-node`), shared by all runners.
  It survives `down`, `up` and `rebuild`; only `prune` deletes it.

Containers and caches persist across jobs, so run only trusted code. Fine for a
solo repo; don't use this for PRs from forks.

## Token

Registering a runner needs a token with admin rights on the repo. The script
uses `ZAPPLY_RUNNER_PAT`, else `~/.config/zapply-ci-runner/token`, else
`gh auth token`. A fine-grained token limited to this repo with
**Administration: read and write** is the narrowest choice.
