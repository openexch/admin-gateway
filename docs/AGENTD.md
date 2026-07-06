# agentd — the per-host process agent (milestone 3: loopback)

`agentd` is the standalone daemon side of the gateway↔agent split
([AGENT-ARCHITECTURE.md](AGENT-ARCHITECTURE.md)). It dials the control plane
(the admin gateway's agent hub), holds one persistent gRPC session, executes
process-lifecycle commands against its embedded ProcessManager, and streams
lifecycle events back. All sub-second local reactions — driver gating,
restart cascades, rapid-crash disarm, PID adoption — stay inside the agent
and keep working when the control plane is down.

## Milestone 3 scope

- **The catalog is empty.** The real service catalog arrives with the
  topology milestone; until then agentd manages nothing, which makes it
  provably harmless next to a gateway running the in-process LocalAgent on
  the same box (no shared PID files, no adoption split-brain). `TailLog`,
  `NodeCounters` and `InstallArtifact` are catalog-independent and fully
  functional.
- **Auth is a static shared token** (`AGENTD_TOKEN` or `-token-file`),
  checked on every RPC. The join-token → CA-issued-certificate enrollment is
  declared in the protocol (`Enroll`) but ships with the topology milestone.
- **TLS**: `-tls-ca <pem>` pins the control plane's CA (TLS 1.3 minimum).
  `-insecure` is for loopback only; agentd refuses a non-TLS dial without it.

## Running

```bash
agentd -control 127.0.0.1:8083 -host-id demo-probe -token-file /etc/openexchange/agent.token -insecure
```

| Flag / env | Meaning |
|---|---|
| `-control` | control-plane address (host:port); required |
| `-host-id` | stable host identity; required (duplicate connects displace the older session) |
| `-token-file` / `AGENTD_TOKEN` | shared agent token |
| `-tls-ca` | PEM file pinning the control plane's TLS CA |
| `-insecure` | plaintext dial (loopback only) |
| `-log-dir` / `-pid-dir` | managed-process dirs (defaults `~/.local/log/cluster`, `~/.local/run/match`) |
| `AGENTD_LOG_FORMAT` | `json` (default) or `text` |

The gateway side listens only when `ADMIN_AGENT_LISTEN` is set (see
README); with it unset the hub is never constructed and the gateway is
byte-identical to the pre-agentd builds.

## Parity guarantees

The remote pair (RemoteAgent ↔ agentd) passes the **same conformance suite**
(`agent/agenttest`) as the in-process LocalAgent — lifecycle semantics,
artifact installs, event delivery, and the anti-split-brain property (an
agent-issued stop stays stopped) are tested identically on both. CI also
smokes the real binary against a real TCP hub (`make loopback`).

## Wire protocol

See `agentwire/agent.proto`. One bidirectional `Session` stream carries
correlated commands/results and unsolicited events; artifact bytes travel on
a separate `FetchArtifact` stream of the same connection (a large jar can
never head-of-line-block a crash event). Unknown verbs answer
`unsupported=true`; the proto version is checked at Hello.
