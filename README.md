# check-sidekiq-status

Sensu Go check for the Sidekiq instances running on a host, several instances per host supported (e.g. `myapp_prod` and `myapp_staging`, or `myapp_prod` and `myapp_searchkick_prod`), each with one or several processes.

Processes are discovered from their process title, rewritten by Sidekiq at each heartbeat:

```
sidekiq 8.0.10 myapp_prod [3 of 10 busy]
sidekiq 8.0.10 myapp_prod [0 of 10 busy] stopping
```

Heartbeats, queues, retry and dead sets come from Redis, as in the Sidekiq Web UI. Sidekiq 6.5, 7 and 8 are supported. No CGO, static binary, Linux only (`/proc`).

## Behavior

From the processes (always available):

| Situation | State |
|---|---|
| Instance given by name with `--expect` not running for more than `--restart-grace` | CRITICAL |
| Fewer processes than expected (`--expect name:N`, or `--min-processes`) for more than `--restart-grace` | WARNING |
| Process quiet (`stopping`, TSTP received: it no longer fetches jobs) for more than `--quiet-grace` | WARNING |
| Process RSS above `--rss-warn` / `--rss-crit` | WARNING / CRITICAL |
| Busy threads, all processes of the instance, above `--busy-warn` / `--busy-crit` % | WARNING / CRITICAL |

From Redis:

| Situation | State |
|---|---|
| Redis URL unreachable | WARNING |
| Process older than `--beat-max` not registered in any database (hung for more than a minute, or Redis not listed) | WARNING |
| Process heartbeat older than `--beat-max`, or expired (hung) | CRITICAL |
| Queue latency (age of the oldest job) above `--latency-warn` / `--latency-crit` | WARNING / CRITICAL |
| Jobs waiting in a queue above `--queued-warn` / `--queued-crit` | WARNING / CRITICAL |
| Retry set above `--retry-warn` / `--retry-crit` | WARNING / CRITICAL |
| Dead set above `--dead-warn` / `--dead-crit` | WARNING / CRITICAL |

Queues are checked per instance: only the queues its processes listen to. Retry and dead sets are shared by every process of a Redis database: they are reported once, on a line per database.

A deployment quiets the processes then restarts them, one by one: these transient states are only reported past a grace delay, 300 seconds by default.

- `--quiet-grace`: a quiet process.
- `--restart-grace`: an instance running fewer processes than required, none included (`not running`). Not running then `12/20 process(es)` is one episode, ended when every process runs again.

Within the grace delay, the instance is OK and its line tells it (`not running for 40s, within grace`); past it, the duration is given (`2/5 process(es) for 6m10s`). Sidekiq does not record these times: the check remembers them in a state file, `check-sidekiq-status.json` in `--state-dir` (default `/var/cache/sensu/sensu-agent`, the agent cache directory, writable by the `sensu` user). Without usable state file, they are reported at once with a `state WARNING` line. A grace of 0 reports at once.

`--expect` lists the instances to check: the other running instances (staging…) are ignored and not reported.

- A name (`myapp_prod`) must be running: CRITICAL if not.
- A pattern (`*_prod`, `myapp_*`) checks the running instances it matches, without requiring any: a stopped instance goes unnoticed.
- `:N` sets the number of processes required (`myapp_prod:4`, `*_prod:2`), `--min-processes` (default 1) otherwise.
- Without `--expect` (or `*`), every running instance is checked.

The output gives one line per instance and per Redis database, the state is the worst one:

```
check-sidekiq-status CRITICAL: 1/2 instance(s) in trouble
myapp_prod OK: 4 process(es) (sidekiq_myapp_prod@1,2,3,4), threads 3/40 busy, latency 0s, queued 2, rss max 412MB
myapp_searchkick_prod CRITICAL: sidekiq_myapp_searchkick_prod@2 no heartbeat (expired)
redis 127.0.0.1:6379/1 OK: retry 2, dead 15
```

The busy threads come from the process title, updated at each heartbeat (every 5 seconds with Sidekiq 6, 10 seconds since Sidekiq 7).

## Instances

An instance is a Sidekiq tag (`-g`, or `:tag:` in `config/sidekiq.yml`). Convention: `{app}_{env}`, plus a role when an application runs several kinds of processes (`myapp_searchkick_prod`). Without tag, Sidekiq uses the name of its directory (the parent of `current` with Capistrano): prod and staging of the same application are then mixed up.

Processes sharing a tag form one instance, whatever their systemd unit:

```
sidekiq_goasso_prod.service              -g goasso_prod  ┐
sidekiq_goasso_searchkick1_prod.service  -g goasso_prod  ┘ goasso_prod, 2 processes
```

Processes of a systemd template unit (`name@N.service`, system or user unit) usually carry their index in the tag (`-g myapp_prod_%i`). The `_N` segment is removed from the tag, wherever it is, so that they form one instance:

```
sidekiq_procivis_prod@1.service             -g procivis_prod_1              ┐ procivis_prod
sidekiq_procivis_prod@2.service             -g procivis_prod_2              ┘
sidekiq_rubypayeur_searchkick_prod@1.service -g rubypayeur_searchkick_1_prod ┐ rubypayeur_searchkick_prod
sidekiq_rubypayeur_searchkick_prod@2.service -g rubypayeur_searchkick_2_prod ┘
```

The systemd unit is read from `/proc/<pid>/cgroup` and names the processes in the messages.

## Redis

`--redis-url` (required, repeatable) gives the Redis servers holding the Sidekiq data:

- Without database number (`redis://127.0.0.1:6379`), every database listed by `INFO keyspace` and holding Sidekiq processes is used: prod in db 1 and staging in db 10 need no configuration.
- With a database number (`redis://172.16.12.2:6379/1`), only this database is used.

Processes are matched to their database by **hostname and PID** (the Sidekiq process identity `hostname:pid:nonce`): a Redis shared by several hosts is supported, each host checks its own processes. The hostname is the one of the Sensu agent host (`gethostname`), as Sidekiq does.

The Sensu agent user needs no access to the application: process titles and cgroups are world readable, and Redis is reached over the network. A Redis password goes in the URL (`redis://:password@host:6379`), redacted in the output; prefer a Sensu secret for it.

Not supported: `redis-namespace` (Sidekiq < 7), Sidekiq running in a container (PIDs differ from the host ones).

## Usage

```
check-sidekiq-status --redis-url <url> [flags]
```

| Flag | Environment variable | Default | Description |
|---|---|---|---|
| `-r`, `--redis-url` | `CHECK_SIDEKIQ_REDIS_URL` | required | Redis URL of the Sidekiq data, every database scanned without database number |
| `-e`, `--expect` | `CHECK_SIDEKIQ_EXPECT` | every instance | Instances (must run) or patterns (`*_prod`) to check, with an optional process count (`:4`), the others are ignored |
| `--min-processes` | `CHECK_SIDEKIQ_MIN_PROCESSES` | `1` | Processes per instance when `--expect` gives no count |
| `--beat-max` | `CHECK_SIDEKIQ_BEAT_MAX` | `60` | Seconds without heartbeat before CRITICAL (0 disables the Redis process checks) |
| `--latency-warn` | `CHECK_SIDEKIQ_LATENCY_WARN` | `300` | Queue latency in seconds for WARNING |
| `--latency-crit` | `CHECK_SIDEKIQ_LATENCY_CRIT` | `900` | Queue latency in seconds for CRITICAL |
| `--queued-warn` | `CHECK_SIDEKIQ_QUEUED_WARN` | `0` | Jobs waiting in a queue for WARNING |
| `--queued-crit` | `CHECK_SIDEKIQ_QUEUED_CRIT` | `0` | Jobs waiting in a queue for CRITICAL |
| `--retry-warn` | `CHECK_SIDEKIQ_RETRY_WARN` | `0` | Jobs in the retry set for WARNING |
| `--retry-crit` | `CHECK_SIDEKIQ_RETRY_CRIT` | `0` | Jobs in the retry set for CRITICAL |
| `--dead-warn` | `CHECK_SIDEKIQ_DEAD_WARN` | `0` | Jobs in the dead set for WARNING |
| `--dead-crit` | `CHECK_SIDEKIQ_DEAD_CRIT` | `0` | Jobs in the dead set for CRITICAL |
| `--busy-warn` | `CHECK_SIDEKIQ_BUSY_WARN` | `0` | Busy threads %, all processes of an instance, for WARNING |
| `--busy-crit` | `CHECK_SIDEKIQ_BUSY_CRIT` | `0` | Busy threads %, all processes of an instance, for CRITICAL |
| `--rss-warn` | `CHECK_SIDEKIQ_RSS_WARN` | `0` | Process RSS in MB for WARNING |
| `--rss-crit` | `CHECK_SIDEKIQ_RSS_CRIT` | `0` | Process RSS in MB for CRITICAL |
| `--quiet-grace` | `CHECK_SIDEKIQ_QUIET_GRACE` | `300` | Seconds a process may stay quiet (deployment) before WARNING, 0 reports it at once |
| `--restart-grace` | `CHECK_SIDEKIQ_RESTART_GRACE` | `300` | Seconds an instance may run fewer processes than required, none included (restart), 0 reports it at once |
| `--timeout` | `CHECK_SIDEKIQ_TIMEOUT` | `5` | Redis timeout in seconds |
| `--proc-root` | `CHECK_SIDEKIQ_PROC_ROOT` | `/proc` | procfs mount point |
| `--state-dir` | `CHECK_SIDEKIQ_STATE_DIR` | `/var/cache/sensu/sensu-agent` | Directory of the state file of the grace delays |

A threshold set to 0 disables its test. Saturated threads are normal for a job queue under load: the latency is the signal that jobs wait too long, busy thresholds are disabled by default. Lists (`--redis-url`, `--expect`) are repeatable or comma separated, also in their environment variable. A missing `--redis-url`, an invalid URL scheme or an invalid `--expect` is UNKNOWN.

## Check definition

Every option is passed through `env_vars`, filled per host from entity annotations with token substitution (applied by the agent to the whole check definition, `env_vars` included). One annotation maps to one option.

Naming rule: an annotation is named after its environment variable, lowercased, without the `CHECK_` prefix (`CHECK_SIDEKIQ_LATENCY_WARN` → `sidekiq_latency_warn`).

Every annotation is optional: each token has an explicit, non-empty `default` in the check definition, so a host without annotations runs the check with the values below. Never use `| default ""`: Sensu rejects an empty environment variable, and the agent then silently drops the check. `sidekiq_expect` defaults to `*_prod`: every production instance, staging ignored.

Lists (`sidekiq_expect`, `sidekiq_redis_url`) are comma separated.

Annotations, e.g. in the agent configuration (`/etc/sensu/agent.yml`):

```yaml
annotations:
  sidekiq_expect: "myapp_prod:4,myapp_searchkick_prod:6"  # names must run, :N processes, default *_prod
  sidekiq_redis_url: "redis://172.16.12.2:6379/1"        # default redis://127.0.0.1:6379 (every database)
  sidekiq_min_processes: "1"                             # default 1
  sidekiq_beat_max: "60"                                 # seconds, default 60
  sidekiq_latency_warn: "300"                            # seconds, default 300
  sidekiq_latency_crit: "900"                            # seconds, default 900
  sidekiq_queued_warn: "1000"                            # jobs per queue, default 0 (disabled)
  sidekiq_queued_crit: "0"                               # jobs per queue, default 0 (disabled)
  sidekiq_retry_warn: "50"                               # default 0 (disabled)
  sidekiq_retry_crit: "0"                                # default 0 (disabled)
  sidekiq_dead_warn: "0"                                 # default 0 (disabled)
  sidekiq_dead_crit: "0"                                 # default 0 (disabled)
  sidekiq_busy_warn: "0"                                 # %, default 0 (disabled)
  sidekiq_busy_crit: "0"                                 # %, default 0 (disabled)
  sidekiq_rss_warn: "1200"                               # MB per process, default 0 (disabled)
  sidekiq_rss_crit: "1500"                               # MB per process, default 0 (disabled)
  sidekiq_quiet_grace: "300"                             # seconds, default 300
  sidekiq_restart_grace: "300"                           # seconds, default 300
  sidekiq_timeout: "5"                                   # seconds, default 5
```

Instances whose tag does not end with `_prod` (e.g. `redmine`, `artishoc_manager`) need `sidekiq_expect`.

```yaml
---
type: CheckConfig
api_version: core/v2
metadata:
  name: check-sidekiq-status
spec:
  command: check-sidekiq-status
  env_vars:
    - CHECK_SIDEKIQ_REDIS_URL={{ .annotations.sidekiq_redis_url | default "redis://127.0.0.1:6379" }}
    - CHECK_SIDEKIQ_EXPECT={{ .annotations.sidekiq_expect | default "*_prod" }}
    - CHECK_SIDEKIQ_MIN_PROCESSES={{ .annotations.sidekiq_min_processes | default "1" }}
    - CHECK_SIDEKIQ_BEAT_MAX={{ .annotations.sidekiq_beat_max | default "60" }}
    - CHECK_SIDEKIQ_LATENCY_WARN={{ .annotations.sidekiq_latency_warn | default "300" }}
    - CHECK_SIDEKIQ_LATENCY_CRIT={{ .annotations.sidekiq_latency_crit | default "900" }}
    - CHECK_SIDEKIQ_QUEUED_WARN={{ .annotations.sidekiq_queued_warn | default "0" }}
    - CHECK_SIDEKIQ_QUEUED_CRIT={{ .annotations.sidekiq_queued_crit | default "0" }}
    - CHECK_SIDEKIQ_RETRY_WARN={{ .annotations.sidekiq_retry_warn | default "0" }}
    - CHECK_SIDEKIQ_RETRY_CRIT={{ .annotations.sidekiq_retry_crit | default "0" }}
    - CHECK_SIDEKIQ_DEAD_WARN={{ .annotations.sidekiq_dead_warn | default "0" }}
    - CHECK_SIDEKIQ_DEAD_CRIT={{ .annotations.sidekiq_dead_crit | default "0" }}
    - CHECK_SIDEKIQ_BUSY_WARN={{ .annotations.sidekiq_busy_warn | default "0" }}
    - CHECK_SIDEKIQ_BUSY_CRIT={{ .annotations.sidekiq_busy_crit | default "0" }}
    - CHECK_SIDEKIQ_RSS_WARN={{ .annotations.sidekiq_rss_warn | default "0" }}
    - CHECK_SIDEKIQ_RSS_CRIT={{ .annotations.sidekiq_rss_crit | default "0" }}
    - CHECK_SIDEKIQ_QUIET_GRACE={{ .annotations.sidekiq_quiet_grace | default "300" }}
    - CHECK_SIDEKIQ_RESTART_GRACE={{ .annotations.sidekiq_restart_grace | default "300" }}
    - CHECK_SIDEKIQ_TIMEOUT={{ .annotations.sidekiq_timeout | default "5" }}
  runtime_assets:
    - opsone/sensu-plugins-sidekiq
  interval: 60
  timeout: 30
  publish: true
  subscriptions:
    - sidekiq
```

## Replacing the process check

`check-sidekiq-status` replaces a plain process check such as `sensu-processes-check --search sidekiq`: it also requires the expected instances to run, and adds per-instance process counts, hung processes, queue latency and job sets. Every host running Sidekiq needs the `sidekiq` subscription.

## Build and release

```sh
make test          # go vet + unit tests
make integration   # runs the check against real Sidekiq 6.5, 7.3 and 8.1 processes in Docker
```

`SIDEKIQ_VERSIONS="8.1.7" make integration` limits the integration test to some versions.

Release:

```sh
git tag 0.1.0 && git push origin 0.1.0
make release       # version taken from the tag, or forced: make release VERSION=0.1.0
sensuctl create -f dist/opsone-sensu-plugins-sidekiq_0.1.0.yml
```

`make release` runs `make dist`, which runs the tests then produces in `dist/` the `linux/amd64` archive in Sensu asset layout (`bin/check-sidekiq-status`), the sha512 checksums file and the asset definition, then creates the GitHub release of the tag with `gh`.

`check-sidekiq-status version` prints the version, commit and build date.
