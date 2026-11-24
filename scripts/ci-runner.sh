#!/usr/bin/env bash
# Runs this repo's self-hosted GitHub Actions runners (label `zapply`) in
# podman: N runner containers sharing one cache volume and one test Postgres.
#
#   scripts/ci-runner.sh up [N]   build if needed and run N runners (default 2, or ZAPPLY_RUNNERS)
#   scripts/ci-runner.sh down     stop them; they unregister from GitHub (caches are kept)
#   scripts/ci-runner.sh status   containers here, runners on GitHub
#   scripts/ci-runner.sh logs     follow the runners' logs
#   scripts/ci-runner.sh rebuild  pull the base images, rebuild, restart
#   scripts/ci-runner.sh prune    down, then delete the cache volume
#
# Registering runners needs a token with admin rights on the repo: ZAPPLY_RUNNER_PAT,
# else ~/.config/zapply-ci-runner/token, else your `gh auth token`.
set -euo pipefail

root=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
compose_file="$root/deploy/ci-runner/compose.yaml"
engine=${ZAPPLY_RUNNER_ENGINE:-podman}

compose() { "$engine" compose -f "$compose_file" "$@"; }

load_env() {
  GITHUB_REPOSITORY=$(gh repo view --json nameWithOwner -q .nameWithOwner)
  if [ -n "${ZAPPLY_RUNNER_PAT:-}" ]; then
    RUNNER_PAT=$ZAPPLY_RUNNER_PAT
  elif [ -r "$HOME/.config/zapply-ci-runner/token" ]; then
    RUNNER_PAT=$(<"$HOME/.config/zapply-ci-runner/token")
  else
    RUNNER_PAT=$(gh auth token)
  fi
  export GITHUB_REPOSITORY RUNNER_PAT
}

# `restart: unless-stopped` only brings containers back after a podman machine
# reboot if the machine's podman-restart.service is enabled; it is off by default.
start_on_boot() {
  [ "$engine" = podman ] && podman machine inspect >/dev/null 2>&1 || return 0
  if [ "$(podman info -f '{{.Host.Security.Rootless}}')" = true ]; then
    podman machine ssh -- 'systemctl --user enable podman-restart.service && loginctl enable-linger' >/dev/null 2>&1
  else
    podman machine ssh -- 'sudo systemctl enable podman-restart.service' >/dev/null 2>&1
  fi || echo "ci-runner: could not enable podman-restart.service; after a machine restart, run 'up' again." >&2
}

# A runner killed without a clean stop (VM crash, `podman rm -f`) stays listed
# on GitHub as offline. Delete those whose container no longer exists here.
forget_dead_runners() {
  local live
  live=$("$engine" ps -a --filter label=com.docker.compose.project=zapply-ci --format '{{.ID}}' | cut -c1-12)
  gh api "repos/{owner}/{repo}/actions/runners" \
    -q '.runners[] | select(.status == "offline" and (.name | startswith("zapply-ci-"))) | "\(.id) \(.name)"' |
    while read -r id name; do
      grep -qx "${name#zapply-ci-}" <<<"$live" && continue
      gh api -X DELETE "repos/{owner}/{repo}/actions/runners/$id" && echo "ci-runner: removed stale runner $name"
    done
}

case "${1:-}" in
  up)
    start_on_boot
    load_env
    export ZAPPLY_RUNNERS=${2:-${ZAPPLY_RUNNERS:-2}}
    compose up -d --build --scale "runner=$ZAPPLY_RUNNERS"
    forget_dead_runners
    echo "ci-runner: $ZAPPLY_RUNNERS runner(s) starting; 'scripts/ci-runner.sh status' lists them on GitHub."
    ;;
  down)
    # Placeholders: compose interpolates the whole file even to stop it.
    GITHUB_REPOSITORY=- RUNNER_PAT=- compose down
    ;;
  status)
    GITHUB_REPOSITORY=- RUNNER_PAT=- compose ps
    # A runner killed mid-job shows as online/busy for a few minutes, so `up`
    # can miss it; sweep again here once GitHub has marked it offline.
    forget_dead_runners
    gh api "repos/{owner}/{repo}/actions/runners" \
      -q '.runners[] | "\(.name)\t\(.status)\t\(if .busy then "busy" else "idle" end)"'
    ;;
  logs)
    GITHUB_REPOSITORY=- RUNNER_PAT=- compose logs -f runner
    ;;
  rebuild)
    load_env
    export ZAPPLY_RUNNERS=${2:-${ZAPPLY_RUNNERS:-$(compose ps -q runner | wc -l | tr -d ' ')}}
    [ "$ZAPPLY_RUNNERS" -gt 0 ] || ZAPPLY_RUNNERS=2
    compose build --pull
    compose up -d --force-recreate --scale "runner=$ZAPPLY_RUNNERS"
    forget_dead_runners
    ;;
  prune)
    GITHUB_REPOSITORY=- RUNNER_PAT=- compose down --volumes
    ;;
  *)
    sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//'
    exit 2
    ;;
esac
