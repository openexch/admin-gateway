# Admin UI

The Open Exchange cluster admin console as a standalone React + TypeScript +
Vite single-page app. It was extracted from the trading UI (where it lived at
the `/admin` route) so it can build and deploy on its own, next to the Go admin
gateway in this repo.

Tabs: Overview, Clusters, Services, Profiles, Risk, Backup.

## Configuration

Build-time env vars (Vite `import.meta.env`):

- `VITE_ADMIN_API_URL` — admin-gateway origin for `/api/admin/*` calls and the
  SSE event stream. **Empty = same-origin**, which is the target deploy (the
  console is served from the same host that proxies the admin gateway). Set it
  to e.g. `http://localhost:8082` for local dev against a running gateway.
- `VITE_GRAFANA_URL` — the header "Grafana" link (default
  `http://localhost:3000`).
- `VITE_TRADING_URL` — where the header "Trading" back-link points (default
  `/`). Set to the trading UI origin when admin is on its own host.
- `VITE_ORDER_API_URL` — OMS origin for the Risk tab only (empty = same-origin).
  The Risk tab reads/writes risk config on the order gateway under
  `/api/v1/admin/risk`; auth for those calls is a deployment concern (the
  standalone console has no login of its own).

## Develop / build / test

```sh
npm install
npm run dev      # vite dev server
npm run build    # tsc && vite build -> dist/
npm test         # vitest run
```

`npm run build` emits a static bundle to `dist/`. That `dist/` is what gets
served as the admin console; wiring it up as a Cloudflare Worker (and proxying
`/api/admin` to the gateway) is a separate deployment step, not part of this
subproject.
