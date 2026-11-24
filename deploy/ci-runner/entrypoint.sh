#!/usr/bin/env bash
# Runs as root: registers an ephemeral runner, lets it take one job as the
# unprivileged `runner` user, and repeats. Each job gets a fresh registration,
# while the container and the shared caches stay warm.
set -euo pipefail

: "${GITHUB_REPOSITORY:?set GITHUB_REPOSITORY (owner/repo)}"
: "${RUNNER_PAT:?set RUNNER_PAT (a token allowed to manage the repo runners)}"
pat=$RUNNER_PAT
unset RUNNER_PAT  # never reaches the job: its environment is built below

labels=${RUNNER_LABELS:-zapply}
name="zapply-ci-$(hostname)"
api="https://api.github.com/repos/$GITHUB_REPOSITORY/actions/runners"
cd /home/runner

# The named volume starts out owned by root. Only chown the directories.
mkdir -p /cache/{go/mod,go/build,npm,xdg,toolcache}
chown runner:runner /cache /cache/go /cache/{go/mod,go/build,npm,xdg,toolcache}

# Only these variables reach the runner and its jobs.
job_env=(HOME=/home/runner USER=runner PATH=/usr/local/bin:/usr/bin:/bin LANG=C.UTF-8)
while IFS= read -r var; do job_env+=("$var"); done < <(env | grep -E '^(GOMODCACHE|GOCACHE|NPM_CONFIG_CACHE|XDG_CACHE_HOME|RUNNER_TOOL_CACHE|ZAPPLY_[A-Z_]+)=' || true)

as_runner() { setpriv --reuid=runner --regid=runner --init-groups env -i "${job_env[@]}" "$@"; }

token() {  # registration-token | remove-token
  curl -fsS -X POST -H "Authorization: Bearer $pat" -H "Accept: application/vnd.github+json" \
    "$api/$1" | jq -er .token
}

child=""
stop() {
  if [ -n "$child" ]; then kill -TERM "$child" 2>/dev/null || true; wait "$child" 2>/dev/null || true; fi
  # An idle ephemeral runner is still registered; take it off GitHub's list.
  if [ -f .runner ]; then as_runner ./config.sh remove --token "$(token remove-token)" >/dev/null 2>&1 || true; fi
  exit 0
}
trap stop TERM INT

while true; do
  rm -f .runner .credentials .credentials_rsaparams
  if ! reg=$(token registration-token); then
    echo "ci-runner: could not get a registration token; retrying in 30s" >&2
    sleep 30 & child=$!; wait "$child"; continue
  fi
  as_runner ./config.sh --unattended --ephemeral --replace \
    --url "https://github.com/$GITHUB_REPOSITORY" --token "$reg" \
    --name "$name" --labels "$labels" --work _work >/dev/null
  echo "ci-runner: $name waiting for a job"
  as_runner ./run.sh & child=$!
  wait "$child" || true
  child=""
done
