import type { LeaseRecord } from "./types";

const novncModuleURL = "/portal/assets/novnc/rfb.js";

export function portalHome(leases: LeaseRecord[], request: Request): Response {
  const active = leases.filter((lease) => lease.state === "active");
  const rows = active.length
    ? active.map((lease) => leaseRow(lease)).join("")
    : `<tr><td colspan="7" class="empty">no active leases</td></tr>`;
  return html(
    "Crabbox Portal",
    `<main>
      <header class="top">
        <div>
          <h1>Crabbox</h1>
          <p>${escapeHTML(new URL(request.url).host)}</p>
        </div>
        <a class="button secondary" href="/portal/logout">log out</a>
      </header>
      <section class="panel">
        <div class="section-head">
          <h2>leases</h2>
          <span>${active.length} active</span>
        </div>
        <table>
          <thead>
            <tr>
              <th>lease</th>
              <th>provider</th>
              <th>target</th>
              <th>class</th>
              <th>desktop</th>
              <th>expires</th>
              <th></th>
            </tr>
          </thead>
          <tbody>${rows}</tbody>
        </table>
      </section>
    </main>`,
  );
}

export function portalVNC(lease: LeaseRecord): Response {
  const nonce = scriptNonce();
  const title = `WebVNC ${lease.slug || lease.id}`;
  const wsPath = `/portal/leases/${encodeURIComponent(lease.id)}/vnc/viewer`;
  return html(
    title,
    `<main class="vnc-page">
      <header class="top">
        <div>
          <h1>${escapeHTML(lease.slug || lease.id)}</h1>
          <p>${escapeHTML(lease.provider)} / ${escapeHTML(lease.target)} / ${escapeHTML(lease.id)}</p>
        </div>
        <nav>
          <a class="button secondary" href="/portal">leases</a>
          <a class="button secondary" href="/portal/logout">log out</a>
        </nav>
      </header>
      <section id="status" class="status">waiting for bridge</section>
      <section id="screen" class="screen" aria-label="WebVNC display"></section>
      <section class="panel commands">
        <h2>bridge</h2>
        <p>run this locally while the browser tab is open:</p>
        <code>crabbox webvnc --id ${escapeHTML(lease.slug || lease.id)} --open</code>
      </section>
    </main>
    <script type="module" nonce="${nonce}">
      import RFBModule from ${JSON.stringify(novncModuleURL)};
      const RFB = RFBModule.default || RFBModule;
      const status = document.getElementById("status");
      const screen = document.getElementById("screen");
      const wsURL = new URL(${JSON.stringify(wsPath)}, window.location.href);
      wsURL.protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
      const fragment = new URLSearchParams(window.location.hash.slice(1));
      const username = fragment.get("username") || "";
      const password = fragment.get("password") || "";
      const credentials = {};
      if (username) credentials.username = username;
      if (password) credentials.password = password;
      const options = Object.keys(credentials).length ? { credentials } : {};
      function setStatus(value, tone = "") {
        status.textContent = value;
        status.dataset.tone = tone;
      }
      let rfb;
      let retryTimer;
      let retryAttempt = 0;
      let connected = false;
      let stopped = false;
      function retryDelay() {
        return Math.min(5000, 500 * 2 ** retryAttempt);
      }
      function scheduleRetry(label) {
        if (stopped) return;
        const delay = retryDelay();
        retryAttempt += 1;
        setStatus(label + "; retrying in " + Math.ceil(delay / 1000) + "s", "warn");
        window.clearTimeout(retryTimer);
        retryTimer = window.setTimeout(connect, delay);
      }
      function connect() {
        if (stopped) return;
        connected = false;
        screen.replaceChildren();
        try {
          setStatus(retryAttempt ? "waiting for bridge" : "connecting");
          rfb = new RFB(screen, wsURL.toString(), options);
          rfb.scaleViewport = true;
          rfb.resizeSession = false;
          rfb.viewOnly = false;
          rfb.addEventListener("connect", () => {
            connected = true;
            retryAttempt = 0;
            setStatus("connected", "ok");
          });
          rfb.addEventListener("disconnect", () => {
            scheduleRetry(connected ? "bridge disconnected" : "waiting for bridge");
          });
          rfb.addEventListener("credentialsrequired", (event) => {
            const types = event.detail?.types || ["password"];
            const values = {};
            if (types.includes("username")) {
              values.username = username || window.prompt("VNC username") || "";
            }
            if (types.includes("password")) {
              values.password = password || window.prompt("VNC password") || "";
            }
            rfb.sendCredentials(values);
          });
          rfb.addEventListener("securityfailure", () => {
            stopped = true;
            window.clearTimeout(retryTimer);
            setStatus("VNC authentication failed", "bad");
          });
        } catch (error) {
          scheduleRetry(error instanceof Error ? error.message : String(error));
        }
      }
      window.addEventListener("beforeunload", () => {
        stopped = true;
        window.clearTimeout(retryTimer);
        rfb?.disconnect();
      });
      connect();
    </script>`,
    200,
    nonce,
  );
}

export function portalError(title: string, message: string, status = 400): Response {
  return html(
    title,
    `<main>
      <section class="panel error">
        <h1>${escapeHTML(title)}</h1>
        <p>${escapeHTML(message)}</p>
        <a class="button secondary" href="/portal">back to portal</a>
      </section>
    </main>`,
    status,
  );
}

function leaseRow(lease: LeaseRecord): string {
  const label = lease.slug || lease.id;
  const vnc = lease.desktop
    ? `<a class="button" href="/portal/leases/${encodeURIComponent(lease.id)}/vnc">open</a>`
    : `<span class="muted">no desktop</span>`;
  return `<tr>
    <td><strong>${escapeHTML(label)}</strong><small>${escapeHTML(lease.id)}</small></td>
    <td>${escapeHTML(lease.provider)}</td>
    <td>${escapeHTML(lease.target)}</td>
    <td>${escapeHTML(lease.class)}</td>
    <td>${lease.desktop ? "yes" : "no"}</td>
    <td>${escapeHTML(shortTime(lease.expiresAt))}</td>
    <td>${vnc}</td>
  </tr>`;
}

function html(title: string, body: string, status = 200, nonce = ""): Response {
  const scriptSource = nonce ? `'self' 'nonce-${nonce}'` : "'self'";
  return new Response(
    `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <title>${escapeHTML(title)}</title>
  <style>
    :root { color-scheme: dark; --bg:#111315; --fg:#f3f5f7; --muted:#9ca3af; --line:#30363d; --panel:#1b1f23; --accent:#38bdf8; --bad:#f87171; --warn:#fbbf24; --ok:#34d399; }
    * { box-sizing: border-box; }
    body { margin:0; min-height:100vh; background:var(--bg); color:var(--fg); font:14px/1.45 ui-sans-serif,system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif; }
    main { width:min(1180px, calc(100vw - 32px)); margin:0 auto; padding:24px 0; }
    h1,h2,p { margin:0; }
    h1 { font-size:22px; font-weight:700; }
    h2 { font-size:14px; text-transform:uppercase; color:var(--muted); }
    a { color:inherit; }
    code { display:block; overflow:auto; padding:12px; border:1px solid var(--line); border-radius:6px; background:#0c0e10; color:#d1fae5; }
    table { width:100%; border-collapse:collapse; table-layout:fixed; }
    th,td { padding:12px; border-bottom:1px solid var(--line); text-align:left; vertical-align:middle; }
    th { color:var(--muted); font-weight:600; }
    td small { display:block; color:var(--muted); margin-top:2px; }
    .top { display:flex; justify-content:space-between; gap:16px; align-items:center; margin-bottom:20px; }
    .top p,.muted,.empty { color:var(--muted); }
    .panel { border:1px solid var(--line); border-radius:8px; background:var(--panel); overflow:hidden; }
    .section-head { display:flex; justify-content:space-between; align-items:center; padding:14px 16px; border-bottom:1px solid var(--line); }
    .button { display:inline-flex; align-items:center; justify-content:center; min-height:32px; padding:0 12px; border-radius:6px; background:var(--accent); color:#001018; text-decoration:none; font-weight:700; }
    .button.secondary { background:transparent; color:var(--fg); border:1px solid var(--line); }
    .vnc-page { width:100vw; height:100vh; padding:12px; display:grid; grid-template-rows:auto auto 1fr auto; gap:10px; }
    .screen { min-height:0; border:1px solid var(--line); border-radius:8px; background:#050607; overflow:hidden; }
    .screen div { margin:0 auto; }
    .status { border:1px solid var(--line); border-radius:6px; padding:8px 10px; color:var(--muted); }
    .status[data-tone="ok"] { color:var(--ok); }
    .status[data-tone="warn"] { color:var(--warn); }
    .status[data-tone="bad"] { color:var(--bad); }
    .commands { padding:12px; display:grid; gap:8px; }
    .error { margin-top:20vh; padding:24px; display:grid; gap:12px; }
    @media (max-width: 760px) { main { width:min(100vw - 20px, 1180px); padding:10px 0; } th:nth-child(4),td:nth-child(4),th:nth-child(6),td:nth-child(6){ display:none; } .top{align-items:flex-start;} }
  </style>
</head>
<body>${body}</body>
</html>`,
    {
      status,
      headers: {
        "content-security-policy": [
          "default-src 'none'",
          "base-uri 'none'",
          "connect-src 'self' ws: wss:",
          "frame-ancestors 'none'",
          "img-src 'self' data: blob:",
          `script-src ${scriptSource}`,
          "style-src 'unsafe-inline'",
        ].join("; "),
        "content-type": "text/html; charset=utf-8",
      },
    },
  );
}

function scriptNonce(): string {
  return crypto.randomUUID().replaceAll("-", "");
}

function shortTime(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return value;
  }
  return date.toISOString().replace(".000Z", "Z");
}

function escapeHTML(value: string | undefined): string {
  return (value ?? "")
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;");
}
