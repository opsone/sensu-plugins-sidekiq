# Changelog

## [0.3.0] - 2026-11-11

### Added
- `--restart-grace` (default 300 seconds): an instance running fewer processes than required, none included (`not running`), is only reported after this delay, with its duration. Same state file as `--quiet-grace`.

### Changed
- A condition within its grace delay is shown on the OK line of the instance (`… for 40s, within grace`).

## [0.2.0] - 2026-11-11

### Added
- `--quiet-grace` (default 300 seconds): a quiet process (deployment) is only reported after this delay, with its quiet duration. The time a process went quiet is kept in a state file, `--state-dir` (default `/var/cache/sensu/sensu-agent`); without usable state file, it is reported at once with a `state WARNING` line.

## [0.1.1] - 2026-11-10

### Changed
- `check-sidekiq-status`: the instances not selected by `--expect` are no longer listed in the output (`ignored:` line removed).

## [0.1.0] - 2026-11-09

### Added
- `check-sidekiq-status`: discovery of the Sidekiq processes from their process title, grouped into instances by tag (`_N` index of systemd template units removed), systemd unit read from the cgroup.
- `--expect` with names (must run), patterns (`*_prod`) and process counts (`myapp_prod:4`).
- Process checks: process count, quiet processes, busy threads, RSS.
- Redis checks (`--redis-url`, every database scanned without database number, processes matched by hostname and PID): heartbeat, queue latency and size per instance, retry and dead sets per database.
- Sidekiq 6.5, 7 and 8 supported, integration tests against real processes in Docker.
