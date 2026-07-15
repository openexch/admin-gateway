// Admin console edge Worker.
//
// One same-origin surface at admin.openexch.io: this Worker serves the built
// admin UI (the ASSETS binding = ui/dist) and reverse-proxies the two API
// backends the console talks to. Because the UI and the APIs share this one
// origin, there is NO cross-origin request and thus NO CORS at all (which is
// why the gateway can drop its wildcard Access-Control-Allow-Origin).
//
// Auth is Cloudflare Access in front of admin.openexch.io (human identity) plus
// an Access service token the Worker presents to the tunneled origins (so the
// origins are not open to anything but this Worker). The OMS risk endpoints
// additionally need an OMS admin bearer, injected here so the browser never
// holds it.
//
// Everything that is not an API path falls through to the static admin UI
// (SPA fallback in wrangler serves index.html for client-side routes).

export interface Env {
  ASSETS: Fetcher;
  // Tunneled origins (set as vars in wrangler.jsonc):
  ADMIN_GATEWAY_ORIGIN: string; // e.g. https://admin-api.openexch.io  -> localhost:8082
  OMS_ORIGIN: string;           // e.g. https://oms.openexch.io
  // Secrets (wrangler secret put):
  CF_ACCESS_CLIENT_ID?: string;     // service token the origins' Access policy trusts
  CF_ACCESS_CLIENT_SECRET?: string;
  OMS_ADMIN_TOKEN?: string;         // bearer for the OMS /api/v1/admin/risk endpoints
}

const ADMIN_API_PREFIX = "/api/admin/";
const OMS_RISK_PREFIX = "/api/v1/admin/risk";

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    const url = new URL(request.url);

    if (url.pathname.startsWith(ADMIN_API_PREFIX)) {
      return proxy(request, env.ADMIN_GATEWAY_ORIGIN, url, env);
    }
    if (url.pathname.startsWith(OMS_RISK_PREFIX)) {
      // The OMS additionally needs an admin bearer; the console has no login.
      return proxy(request, env.OMS_ORIGIN, url, env, env.OMS_ADMIN_TOKEN);
    }
    // Static admin UI (SPA fallback handled by the assets binding).
    return env.ASSETS.fetch(request);
  },
} satisfies ExportedHandler<Env>;

// proxy forwards the request to a tunneled origin, presenting the Access
// service token so the origin (which is behind an Access service-token policy)
// accepts it, and optionally a bearer for the OMS.
async function proxy(
  request: Request,
  origin: string,
  url: URL,
  env: Env,
  bearer?: string,
): Promise<Response> {
  if (!origin) {
    return new Response("admin worker: origin not configured", { status: 502 });
  }
  const target = origin.replace(/\/$/, "") + url.pathname + url.search;
  const headers = new Headers(request.headers);
  if (env.CF_ACCESS_CLIENT_ID && env.CF_ACCESS_CLIENT_SECRET) {
    headers.set("CF-Access-Client-Id", env.CF_ACCESS_CLIENT_ID);
    headers.set("CF-Access-Client-Secret", env.CF_ACCESS_CLIENT_SECRET);
  }
  if (bearer) {
    headers.set("Authorization", "Bearer " + bearer);
  }
  // Host must match the origin, not admin.openexch.io.
  headers.set("Host", new URL(origin).host);

  const init: RequestInit = {
    method: request.method,
    headers,
    redirect: "manual",
  };
  if (request.method !== "GET" && request.method !== "HEAD") {
    init.body = request.body;
  }
  return fetch(target, init);
}
