# Changelog

All notable changes to `admin-gateway` (the Open Exchange process manager
and operations API) are documented here. The stack (`match`, `oms`,
`admin-gateway`, `trading-ui`) is versioned together; one version spans all
four repos.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added
- Pre-flight invariant engine (#42, #43, #45): named checks for memory
  headroom, disk space, live-driver-dir integrity, launch artifacts and
  cluster quorum. Surfaced continuously in `/api/admin/status` and the
  `admin_invariant_ok{check}` / `admin_mem_available_bytes` metrics, and on
  demand via `GET /api/admin/preflight`. Knobs: `ADMIN_MIN_MEM_MB`,
  `ADMIN_MIN_ROOT_DISK_GB`, `ADMIN_MAX_SHM_USED_PCT`.

## [0.3.0-beta] - 2026-07-05

The beta hardening release: honest status, guarded operations, secure
defaults, and full observability.

### Added
- External Aeron media-driver management (`driver0-2`) with crash cascade:
  a dead driver takes its node down cleanly instead of wedging it (#14).
- Truthful `/api/admin/status`: per-node process liveness plus counter
  freshness (#16), and crash monitoring for adopted processes (#21).
- Node startup gated on driver health, with a cap on rapid restart loops
  (#19).
- Snapshot and housekeeping refused while any member is down or lagging
  (#18), atomic operation claims so snapshot cannot interleave with a
  rolling update (#23), and archive-safe `/cleanup` plus rolling-update
  hard-fail on rejoin/catch-up timeout (#22).
- Heartbeat-based backup freshness in `/status` and `/backup-info` (#20).
- Admin API auth middleware: bearer token, loopback bind by default,
  shell-free process exec (#25).
- Structured JSON logs with request and operation correlation ids (#28),
  Prometheus `/metrics` (#29).
- Rebuild-admin post-restart verification handshake (#30) and an automated
  stranded-member reseed procedure (#31).
- Operations runbooks (`docs/RUNBOOKS.md`) (#32).

### Fixed
- rebuild-admin builds from this repo's checkout, not the pre-split match
  path (#15).
- Backup environment unwedged (#20); progress slot released when the
  archive-op lag guard refuses an operation (#26).

### Security
- Trivy vulnerability and secret scanning plus `govulncheck` in CI (#27).

## [0.2.0-alpha] - 2026-06-28

- First tagged release, aligned with the Open Exchange v0.2.0-alpha stack.
- Live-safe archive housekeeping wired into the snapshot flow (#6); unsafe
  offline compaction against live archives removed (#7).

[0.3.0-beta]: https://github.com/openexch/admin-gateway/compare/v0.2.0-alpha...v0.3.0-beta
[0.2.0-alpha]: https://github.com/openexch/admin-gateway/releases/tag/v0.2.0-alpha
