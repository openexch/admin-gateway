# Agent architecture: control plane + per-host agents

Status: **design, approved direction** (2026-07-05). Horizon A (the
`ProcessAgent` interface extraction, single box unchanged) ships in the next
release; Horizon B (the `agentd` daemon, multi-host) is the release after.

## Why

The admin gateway today is a single-box process manager: it `exec`s every
managed process directly (3 media drivers, 3 cluster nodes, backup, OMS,
market gateway), signals process groups, reads Aeron CnC counters off
`/dev/shm`, pins to one CPU's core map, and hardcodes `127.0.0.1` everywhere.
That is exactly right for development and demo, and a non-starter for any
real deployment where engine nodes live on separate machines.

The direction: split into a **control plane** (the gateway: API, UI,
quorum-aware orchestration) and a **per-host agent** (`agentd`: owns local
process lifecycle next to the processes it manages). The current
`ProcessManager` already has the right shape; this design mostly relocates
its boundary rather than reinventing it.

## The contract (Horizon A, shipping now)

A new `agent` package defines `ProcessAgent`, implemented today by
`ProcessManager` in-process ("LocalAgent") and tomorrow by a gRPC client
talking to a remote `agentd`:

- **Process verbs** — `List / Get / Summary / Start / StartUnchecked / Stop /
  ForceStop / Restart / StartAll / StopAll / RestartAll` (today's exported
  surface plus the two unexported calls OperationsService makes).
- **Host-local observability** — `TailLog(service, lines)` and
  `NodeCounters(nodeID)` (Aeron CnC reads are host-local by nature; the
  rolling update's cross-node catch-up comparison happens control-plane-side
  from both agents' counter reports).
- **`InstallArtifact(spec{DestPath, Sha256, Mode}, io.Reader)`** — stream to
  a temp file on the destination filesystem, verify sha256, atomic rename.
  This shape is chosen NOW so the Horizon B version (chunked gRPC
  Stage + Activate) is a refinement, not a break.
- **`Subscribe()` event stream** — started / stopped / crashed /
  cascade-stopped / disarmed / adopted. Non-blocking sends with bounded
  buffers: a slow consumer can never wedge the crash path. First consumer:
  the `GET /api/admin/events` SSE feed (agent events + operation progress
  for the admin UI's live activity panel).

### What stays agent-side (never crosses the wire)

Gating (`waitForGate`), restart cascades, rapid-crash disarm, PID-file
adoption, port-orphan cleanup, Aeron stale-state cleanup, log rotation.
These need local sub-second reactions and must keep working when the
control plane is down or unreachable. The control plane observes them via
events; it never mediates them.

### What stays control-plane

Rolling-update orchestration and its quorum guard, leader detection,
snapshot/housekeeping coordination and their safety interlocks, backup
freshness policy, reseed, auto-snapshot scheduling, the HTTP API, auth, UI.
Anything that reasons about MORE THAN ONE host lives here.

### Known seams, deliberately not in the interface yet

- `cluster.go` invokes ClusterTool/ArchiveTool via local `java` against
  local `/dev/shm` dirs. Horizon B adds **typed** RPCs
  (`ClusterToolOp(nodeID, op)`, `ArchiveInfo(nodeID)`) — never a generic
  remote-exec primitive.
- `status.go`'s `isProcessAlive(pid)` `/proc` probe and its
  `localhost:8080/8081` health probes assume one host. Horizon B:
  `ProcessInfo` gains an authoritative agent-reported `PidAlive`, and HTTP
  probes move to a per-host probe list derived from topology.
- Cleanup sweep, recover-from-backup, and reseed do direct filesystem work.
  Horizon B turns each into an agent primitive (`CleanNode`, `RestoreDir`,
  `CopyNodeState`) invoked by control-plane orchestration.

## Horizon B decisions

### Topology model

A `topology.yaml` replaces the hardcoded Go catalog in `NewProcessManager`:

```yaml
hosts:
  - id: engine-0
    agent: dial              # agent dials in; no listener on engine hosts
    coreProfile: prod-isolated
    aeronDirTemplate: /dev/shm/aeron-{{user}}-{{nodeId}}-driver
services:
  - name: node0
    host: engine-0
    role: cluster-node
    gatedBy: driver0
    # command/env templated from role + host profile
```

- Core maps become named profiles (`dev-13700k` is today's quad layout;
  `prod-isolated` is one node+driver per host with DEDICATED busy-spin).
- `CLUSTER_ADDRESSES` is derived from topology, killing the hardcoded
  `127.0.0.1,127.0.0.1,127.0.0.1` in three places.
- **No file = implicit single-host topology**, byte-identical to today's
  catalog. Dev boxes change nothing.

### RPC and security

**gRPC over mTLS.** Bidirectional streaming carries commands, events, log
follow, and artifact chunks; protobuf gives versioned evolution. REST/JSON
was rejected: no sane server-push for crash events, hand-rolled chunking
for artifacts.

**The agent dials the control plane**, holds one persistent bidirectional
stream, and commands are multiplexed over it. Engine hosts open no listener
ports and can sit behind NAT. Registration: one-time join token in the agent
config; the control plane's embedded private CA issues the agent's client
cert; mTLS thereafter. Assumption: same trusted low-latency LAN/VPC — Aeron
cluster traffic never touches the control plane.

### State and failure semantics

- The control plane owns **desired state** (topology + per-service
  running/stopped), persisted to disk. Agents cache the last-received
  desired-state document and act on it autonomously.
- Control plane down: auto-restart, gating, cascades, disarm all continue
  (they are agent-side). Events buffer in a bounded ring, replayed on
  reconnect; reconnect starts with a full agent state snapshot then
  incremental events — the same reconcile shape `adoptExisting` +
  `refreshAdoptedProcesses` already implement in-process.
- Agent restart: same PID-file adoption dance, host-local
  (`~/.local/run/match` per host); `agentd.service` runs with
  `KillMode=process` so agent self-update leaves children running, exactly
  like `admin.service` today.
- **Anti-split-brain rule: only the agent execs/signals on its host.** The
  control plane never SSHes in or kills remotely. An operator "stop" becomes
  durable agent-side desired state (replacing the in-process `stopChan`
  intentional-stop marker), so a control-plane-issued Stop can never fight
  the agent's auto-restart. This is the subtlest correctness point in the
  whole design.
- Rolling update resume: the orchestration journal (current step, node
  states) persists control-plane-side. If the control plane crashes
  mid-update, agents hold position; the operator resumes or aborts
  explicitly.

### Logs and metrics

- Logs stay on their host. The agent serves `TailLog` plus a follow-stream
  RPC for the UI's live tail. Central log shipping (Loki etc.) is out of
  scope for the open core.
- `agentd` exposes a host-local Prometheus endpoint with the process gauges
  it already computes; the gateway's `/metrics` aggregates fleet-level
  gauges for the UI. Real monitoring scrapes per host.

### Artifact distribution

Build ONCE (control plane or CI), address by sha256, stream to each agent's
staging area, then **Activate** as a separate orchestrated step: the rolling
update stages on all hosts up front and activates per node between stop and
start. Never build on engine hosts — Maven on pinned cores and toolchain
drift are both real incidents waiting to happen (see admin-gateway#36 for
the single-box version of that lesson).

### Packaging and the demo box

Single static Go binary; one `agentd.service` per host (`Restart=on-failure`,
`KillMode=process`); config at `/etc/openexchange/agentd.toml` (control-plane
address, join-token path, host id). Agent self-update reuses the staged-swap
plus post-restart sha handshake generalized from `rebuild_verify.go`.

The demo box keeps the in-process LocalAgent — **no agentd required, no
operational change**. An optional loopback mode (gateway + agentd on one
host over real gRPC) exists purely as the CI integration test of the RPC
path.

### Migration and the commercial layer

The gateway holds `map[hostID]agent.ProcessAgent`; single-box topology binds
every service to host `local` backed by LocalAgent. Mixed fleets fall out
for free, which is also the migration path: move one low-risk service class
(the backup node) to a remote host first.

The versioned agent protocol is the product boundary for the "managed
deployments" paid layer teased on the website: a hosted control plane
driving customer-premise agents. The protocol and `agentd` stay
Apache-licensed with the core; the hosted control plane is the paid part.

## Risks

1. **Crash-cascade latency over RPC** — eliminated by design: cascades never
   cross the wire.
2. **Counter reads on the rolling-update hot path** — `waitForFollowerCatchUp`
   polls at 500ms; over LAN gRPC that budget holds, but the counters RPC
   must not spawn a JVM agent-side (the Go CnC reader already avoids that).
3. **Status fan-out** — today's status poll shells out serially per node;
   multi-host it must fan out per-agent with timeouts or the 2s cache poll
   collapses.
4. **Gateway-vs-agent restart split-brain** — addressed by durable
   agent-side desired-state stops; verify explicitly in the loopback CI.
5. **Adoption across agent self-update** — re-verify the KillMode=process +
   PID-file dance under agentd before trusting remote self-update.
6. **Event backpressure** — bounded buffers, drop-and-count, never block
   `handleCrash`.

## Sequencing

1. ✅ (shipped, #38) `agent` package + `ProcessManager` implements it +
   consumers switch to the interface. Behavior-preserving; single box only.
2. ✅ (shipped, #39) rebuild-admin #36 fix: failure records persisted, build
   env pinned.
3. ✅ (shipped, 2026-07-06) `agentd` binary + gRPC protocol + loopback CI
   mode: `agentwire/` (versioned proto, control plane is the server, agents
   dial in, artifacts pull over a separate stream), `agenthub/` (session
   registry + RemoteAgent), `agentd/` + `cmd/agentd` (empty catalog until
   topology). Parity = the LocalAgent's conformance suite over the wire +
   the real-binary TCP smoke in CI. See docs/AGENTD.md.
4. (Next) topology.yaml + host profiles + derived cluster addresses; agent
   catalogs move from the builtin Go catalog to per-host topology; Enroll
   (join token → embedded-CA client cert) and mTLS-required mode.
5. (Next) migrate the backup node to a second host as the first real
   multi-host deployment.
