# Changelog

## [0.1.0] - 2026-11-09

### Added
- `check-sidekiq-status`: discovery of the Sidekiq processes from their process title, grouped into instances by tag (`_N` index of systemd template units removed), systemd unit read from the cgroup.
- `--expect` with names (must run), patterns (`*_prod`) and process counts (`myapp_prod:4`).
- Process checks: process count, quiet processes, busy threads, RSS.
- Redis checks (`--redis-url`, every database scanned without database number, processes matched by hostname and PID): heartbeat, queue latency and size per instance, retry and dead sets per database.
- Sidekiq 6.5, 7 and 8 supported, integration tests against real processes in Docker.
