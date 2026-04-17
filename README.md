# Open Exchange — Admin Gateway

HTTP API for managing the Open Exchange Aeron Cluster. Handles process lifecycle, rolling updates, snapshots, log retrieval, and cluster health monitoring.

## Features

- Process management for all cluster services (nodes, gateways, backup, OMS)
- Zero-downtime rolling updates
- Automatic and manual snapshot creation
- Archive compaction and cleanup
- Per-node log retrieval
- HTTP health probing of managed services (OMS :8080, market gateway :8081)
- Self-update capability
- Dependency-ordered startup and shutdown

## Tech Stack

- **Go 1.22**
- **chi** — HTTP routing
- Runs as a systemd user service

## Getting Started

### Prerequisites

- Go 1.22+
- Running Aeron Cluster (match engine)

### Build & Run

```bash
go build -o admin-gateway .
./admin-gateway
```

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `ADMIN_PORT` | `8082` | HTTP server port |
| `MATCH_PROJECT_DIR` | (auto-detected) | Path to the match engine project |

## API Endpoints

### Status & Monitoring

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/admin/status` | Cluster status |
| `GET` | `/api/admin/progress` | Operation progress |
| `GET` | `/api/admin/logs?node=0&lines=100` | Service logs |
| `GET` | `/health` | Health check |

### Node Operations

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/admin/restart-node` | Restart single node |
| `POST` | `/api/admin/stop-node` | Stop single node |
| `POST` | `/api/admin/start-node` | Start single node |
| `POST` | `/api/admin/stop-all-nodes` | Stop all cluster nodes |
| `POST` | `/api/admin/start-all-nodes` | Start all cluster nodes |

### Cluster Operations

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/admin/rolling-update` | Zero-downtime rolling update |
| `POST` | `/api/admin/snapshot` | Create cluster snapshot |
| `POST` | `/api/admin/compact` | Archive compaction |
| `POST` | `/api/admin/compact-archive` | Full archive compaction |
| `POST` | `/api/admin/rolling-cleanup` | Rolling archive cleanup |
| `POST` | `/api/admin/cleanup` | Full cleanup |
| `POST` | `/api/admin/cleanup-node` | Per-node cleanup |

### Process Manager

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/admin/processes` | List all managed processes |
| `GET` | `/api/admin/processes/summary` | Process summary |
| `GET` | `/api/admin/processes/{name}` | Get specific process |
| `POST` | `/api/admin/processes/{name}/start` | Start process |
| `POST` | `/api/admin/processes/{name}/stop` | Stop process |
| `POST` | `/api/admin/processes/{name}/restart` | Restart process |
| `POST` | `/api/admin/processes/start-all` | Start all (dependency order) |
| `POST` | `/api/admin/processes/stop-all` | Stop all (reverse order) |

### Auto-Snapshot

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/admin/auto-snapshot` | Get auto-snapshot status |
| `POST` | `/api/admin/auto-snapshot` | Start auto-snapshot |
| `DELETE` | `/api/admin/auto-snapshot` | Stop auto-snapshot |

### Build & Recovery

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/admin/rebuild-admin` | Self-update admin gateway |
| `POST` | `/api/admin/rebuild-cluster` | Rebuild cluster module |
| `POST` | `/api/admin/rebuild-gateway` | Rebuild gateway module |
| `GET` | `/api/admin/backup-info` | Backup information |
| `POST` | `/api/admin/recover-from-backup` | Restore from backup |

## License

Licensed under the Apache License 2.0. See [LICENSE](LICENSE) for details.
