#!/usr/bin/env bash
# Runs check-sidekiq-status against real Sidekiq processes inside Docker containers, one per
# Sidekiq major version in use.
set -euo pipefail
cd "$(dirname "$0")"

versions=(${SIDEKIQ_VERSIONS:-6.5.12 7.3.10 8.1.7})
failures=0

dx() { docker exec "$container" "$@"; }

# Starts a Sidekiq process: 2 threads, queues default and mailers.
start() { # <tag> <redis db>
  docker exec -d -e REDIS_URL="redis://127.0.0.1:6379/$2" "$container" \
    sh -c "sidekiq -r /srv/myapp/app.rb -g $1 -c 2 -q default -q mailers >>/tmp/sidekiq.log 2>&1"
}

# Pushes jobs into db 1: push <ruby code>.
push() {
  dx env REDIS_URL=redis://127.0.0.1:6379/1 ruby -r/srv/myapp/app.rb -e "$1" >/dev/null
}

# Waits until a Redis database holds n registered processes.
wait_registered() { # <db> <n>
  for _ in $(seq 30); do
    [[ $(dx redis-cli -n "$1" scard processes) == "$2" ]] && return
    sleep 1
  done
  echo "timeout waiting for $2 process(es) in db $1" >&2
  dx cat /tmp/sidekiq.log >&2
  exit 1
}

# Runs the check as an unprivileged user, like sensu-agent does.
expect() { # <exit code> [VAR=value...] <args>...
  local want=$1 env=(); shift
  while [[ ${1:-} == CHECK_*=* ]]; do env+=("$1"); shift; done
  local out rc=0
  out=$(dx runuser -u nobody -- env ${env[@]+"${env[@]}"} check-sidekiq-status "$@" 2>&1) || rc=$?
  if [[ $rc -eq $want ]]; then
    echo "ok   [$rc] ${env[*]+${env[*]}} check-sidekiq-status $*"
  else
    echo "FAIL [$rc, want $want] ${env[*]+${env[*]}} check-sidekiq-status $*"
    failures=$((failures + 1))
  fi
  # UNKNOWN: the SDK prints the usage, only its error line is kept.
  [[ $rc -eq 3 ]] && out=$(grep '^Error' <<<"$out" || true)
  sed 's/^/       /' <<<"$out"
}

pid_of() { dx pgrep -f "^sidekiq .* $1 \[" | sed -n "${2:-1}p"; }

suite() { # <sidekiq version>
  local version=$1 image=check-sidekiq-integration:$1
  container=check-sidekiq-integration-$$
  docker build -q --build-arg SIDEKIQ_VERSION="$version" -t "$image" . >/dev/null
  docker run -d --rm --name "$container" "$image" >/dev/null
  trap 'docker stop -t 1 "$container" >/dev/null' RETURN

  echo "=== sidekiq $version"
  # Production in db 1 (2 processes sharing the tag), staging in db 10.
  start myapp_prod 1
  start myapp_prod 1
  start myapp_staging 10
  wait_registered 1 2
  wait_registered 10 1
  sleep 6 # processes older than --beat-max 5

  local scan=redis://127.0.0.1:6379
  # Databases scanned, staging ignored (only expected patterns are checked), db 10 not reported.
  expect 0 -r $scan -e '*_prod:2'
  expect 0 -r $scan/1 -e myapp_prod:2
  expect 1 -r $scan -e myapp_prod:3
  expect 2 -r $scan -e '*_prod,other_prod'
  expect 0 -r $scan -e myapp_staging

  # Invalid configuration.
  expect 3 -e myapp_prod
  expect 3 -r http://127.0.0.1:6379 -e myapp_prod
  expect 3 -r $scan -e 'myapp_[prod'
  expect 3 -r $scan -e 'myapp_prod:0'

  # Configuration through environment variables, as in the README CheckConfig.
  expect 0 CHECK_SIDEKIQ_REDIS_URL="$scan" CHECK_SIDEKIQ_EXPECT='*_prod:2'
  expect 2 CHECK_SIDEKIQ_REDIS_URL="$scan" CHECK_SIDEKIQ_EXPECT='*_prod,other_prod'

  # Redis unreachable, or database without the processes.
  expect 1 -r redis://127.0.0.1:6390 -e myapp_prod
  expect 1 -r $scan/5 -e myapp_prod --beat-max 5

  # Dead and retry sets.
  push 'DeadJob.perform_async; RetryJob.perform_async'
  sleep 3
  expect 1 -r $scan -e myapp_prod --dead-warn 1
  expect 2 -r $scan -e myapp_prod --retry-crit 1

  # Latency: 4 threads, 6 jobs of 20 seconds, 2 of them waiting. The busy threads are read from
  # the process title, updated at each heartbeat (every 5 or 10 seconds).
  push '6.times { SleepJob.perform_async(20) }'
  sleep 11
  expect 1 -r $scan -e myapp_prod --latency-warn 10
  expect 2 -r $scan -e myapp_prod --latency-crit 10
  expect 2 -r $scan -e myapp_prod --busy-crit 100
  sleep 30

  # Hung process: no heartbeat while stopped (every 5 or 10 seconds).
  local hung; hung=$(pid_of myapp_prod 2)
  dx kill -STOP "$hung"
  sleep 15
  expect 2 -r $scan -e myapp_prod --beat-max 12
  dx kill -CONT "$hung"

  # Quiet process: title updated at the next heartbeat.
  local quiet; quiet=$(pid_of myapp_prod 1)
  dx kill -TSTP "$quiet"
  sleep 11
  local state; state=$(dx runuser -u nobody -- mktemp -d)
  expect 0 -r $scan -e myapp_prod --state-dir "$state"                 # within the grace delay
  sleep 3
  expect 1 -r $scan -e myapp_prod --state-dir "$state" --quiet-grace 2 # past the grace delay
  expect 1 -r $scan -e myapp_prod --quiet-grace 0
  expect 1 -r $scan -e myapp_prod --state-dir /nonexistent             # no state: no grace

  # Process gone.
  dx kill -KILL "$quiet"
  sleep 1
  expect 1 -r $scan -e myapp_prod:2
  expect 0 -r $scan -e myapp_prod
}

for v in "${versions[@]}"; do suite "$v"; done
echo "$failures failure(s)"
exit $failures
