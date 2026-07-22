# Changelog

All notable changes to `admin-gateway` (the Open Exchange process manager
and operations API) are documented here. The stack (`match`, `oms`,
`admin-gateway`, `trading-ui`, `assets`) is versioned together; one version spans all
five repos.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [0.4.0-beta] - 2026-07-22

From process manager to control plane: agentd, multi-cluster management,
runtime profiles, and the admin console comes home.

### Added
- Control plane: `agenthub` + per-host `agentd` + `agentwire` wire protocol
  (#55, #57, #58); opt-in agent hub listener with secure defaults (#59);
  `GET /api/admin/events` — SSE feed of agent events + operation progress
  (#52).
- Multi-cluster management: cluster descriptors, generic clusters[],
  cluster-scoped ops (#74); per-cluster node count — topology store + genesis
  re-form op (#79).
- Runtime profiles: stack-wide light/dev/demo/performance/ultra (#72); live
  switching (#73), including across the driver-mode boundary (#75); custom
  profiles — writable store, CRUD, strict validation (#80).
- Admin console: extracted from trading-ui into a standalone Vite subproject
  (#91), served by a same-origin edge Worker at `admin-api.openexch.io`
  (#92, #94, #95, #96).
- Pre-flight invariant engine with `/preflight`, status + metrics surfacing
  (#46).
- Settlement/assets wiring: settlement-bridge ServiceDef, PM-managed (#82);
  journal-retention endpoint — per-node, watermark-gated (#81);
  `ASSETS_STATE_DIR` + `MATCH_STATE_DIR` on-disk preflights (#83, #89);
  settlement-journal env passthrough to ME node defs (#84).
- Demo plumbing: market simulator managed as `sim` + demo health surfaced
  (#40); market gateway wired to the edge relay (#65); sim canary points at
  the edge-relay viewer path (#66); market gateway TimescaleDB env (#41).

### Fixed
- Rolling updates gated on memory/quorum pre-flight with truthful abort state
  (#48); pre-start artifact check + nice'd builds (#49); rebuild-gateway and
  rebuild-oms build in isolated trees (#50, #51); rebuild-admin resolves an
  explicit Go toolchain and persists failures (#39).
- Never delete a live media-driver dir; refuse orphan-driver starts (#47);
  Aeron cleanup sweep scoped to the target cluster (#77); cluster archive
  wiped at the real StateDir, not /dev/shm (#97).
- Durable op-failure records, cleanup driver-dir guard, assets ops gated
  (#85); balance_snapshots no longer TRUNCATEd (dropped by oms V004) (#88).
- Negative TailLog line count no longer panics the agent (#70); agent hub
  bind failures degrade instead of killing the gateway (#60); SSE stream
  works behind middleware wrappers, auth is header-only (#53).

### Changed
- ProcessManager becomes the LocalAgent behind the extracted ProcessAgent
  contract, with a conformance suite (#38, #54).
- `OMS_AUTH_MODE=demo` public demo auth (#44); demo UI origin allowlisted for
  OMS CORS (#35); light profile simGlobalOps 5 → 60 (#76, #78).
- grpc 1.82.1 (GHSA-hrxh-6v49-42gf), chi 5.3.1, protobuf 1.36.11; x/net +
  grpc CVE bumps (#56, #61, #62, #63, #99).
- Contact email is info@openexch.io (#64).

## [Unreleased]

### Added
- Pre-flight invariant engine (#42, #43, #45): named checks for memory
  headroom, disk space, live-driver-dir integrity, launch artifacts and
  cluster quorum. Surfaced continuously in `/api/admin/status` and the
  `admin_invariant_ok{check}` / `admin_mem_available_bytes` metrics, and on
  demand via `GET /api/admin/preflight`. Knobs: `ADMIN_MIN_MEM_MB`,
  `ADMIN_MIN_ROOT_DISK_GB`, `ADMIN_MAX_SHM_USED_PCT`.

### Added (agentd milestone — Horizon B step 3)
- The gateway↔agentd wire protocol (`agentwire/agent.proto`): control plane
  as the single gRPC server, agents dial in over one persistent session,
  correlated command/result envelopes, artifact bytes pulled over a separate
  stream, static-token auth, versioned handshake.
- `agenthub` (session registry + `RemoteAgent`) and `agentd`/`cmd/agentd`
  (the per-host daemon, empty catalog until topology). Loopback parity is
  tested: the remote pair passes the identical `agent/agenttest` conformance
  suite as the in-process LocalAgent, plus a real-binary TCP smoke in CI
  (`make loopback`). Zero behavior change on existing deployments.

### Added (observability)
- `GET /api/admin/events`: Server-Sent Events stream of agent lifecycle
  events (started/stopped/crashed/cascade-stop/disarmed/adopted) and
  operation progress — the first consumer of the ProcessAgent Subscribe
  stream. Live-only, best-effort delivery; `/status` remains the source of
  truth. Auth is the standard bearer header — URL tokens are rejected
  everywhere (they leak into history and logs); browser clients needing a
  token consume the stream via fetch-streaming instead of EventSource.

### Added (rebuild-oms)
- `POST /api/admin/rebuild-oms {restart, force}`: staged isolated-tree build
  of the OMS uber-jar with sha-verified atomic install — replaces the manual
  build-and-copy flow, and completes the honesty fix where rebuild-gateway
  used to restart oms without building its code.

### Fixed
- rebuild-gateway builds in an isolated rsync'd tree and installs the jar
  via the sha-verified atomic artifact swap (#45) — mvn (whose clean/-am
  phases rebuild upstream modules in place) never runs against the live tree
  again, so it can no longer delete the running cluster jar. Its restart now
  touches only the market gateway: the oms restart never picked up new code
  (oms-app.jar comes from the OMS repo) and was dropped. rebuild-gateway and
  rebuild-cluster are pre-flight gated on memory/disk (`{"force":true}`
  overrides).
- Starting a service whose launch artifact is missing is refused with one
  clear "artifact missing — rebuild in progress?" failure on every start
  path including auto-restart (#45), instead of crash-looping into a
  disarmed auto-restart within seconds. All rebuild build steps (mvn, go
  build, rsync) now run niced (`ADMIN_BUILD_NICE`, default 10, plus ionice
  idle) so builds cannot starve the trading processes.
- Rolling update is gated on pre-flight invariants (#43): refused (409)
  without memory headroom, full quorum, intact driver dirs or disk space;
  `{"force":true}` overrides. Abort messages now report the actual per-node
  cluster state at abort time ("QUORUM LOST" when it is) instead of a
  hardcoded "cluster keeps quorum (2/3)" that lied during the incident.
- Live media-driver dirs can no longer be deleted by any admin path (#42):
  both deleters (rolling-update's per-node cleanup and the pre-start stale
  Aeron sweep) now require the launch script's `<dir>.pid` ground truth to
  agree with the tracked state before removing anything. Starting a driver
  over a live untracked orphan is refused with one actionable error instead
  of burning the crash-loop cap on idempotent exit-0 launches, and
  `force-stop driverN` kills the orphan pid too, making runbook 1 recovery
  fully API-driven.

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
