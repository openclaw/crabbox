import type {
  ExternalRunnerRecord,
  LeaseRecord,
  Provider,
  RunEventRecord,
  RunRecord,
} from "./types";
import { coordinatorProviderSpec } from "./types";

const novncModuleURL = "/portal/assets/novnc/rfb.js";
const copyIcon = `<svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="9" y="9" width="12" height="12" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h10"/></svg>`;
const lockIcon = `<svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="4" y="11" width="16" height="10" rx="2"/><path d="M8 11V7a4 4 0 0 1 8 0v4"/></svg>`;
const ejectIcon = `<svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M5 17h14"/><path d="m12 5 7 9H5z"/></svg>`;
const powerIcon = `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3v9"/><path d="M6.3 5.7a8 8 0 1 0 11.4 0"/></svg>`;
const serverIcon = `<svg viewBox="0 0 24 24" aria-hidden="true"><rect x="5" y="3" width="14" height="18" rx="2"/><path d="M8 8h8M8 12h8M8 16h4"/></svg>`;
const dedicatedHostIcon = `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 7h16"/><path d="M7 7V4h10v3"/><rect x="5" y="7" width="14" height="13" rx="2"/><path d="M9 11h6M9 15h3"/></svg>`;
const vncIcon = `<svg viewBox="0 0 24 24" aria-hidden="true"><rect x="3" y="4" width="18" height="13" rx="2"/><path d="M8 21h8M12 17v4"/></svg>`;
const codeIcon = `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="m9 8-4 4 4 4"/><path d="m15 8 4 4-4 4"/><path d="m13 5-2 14"/></svg>`;
const shareIcon = `<svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="18" cy="5" r="3"/><circle cx="6" cy="12" r="3"/><circle cx="18" cy="19" r="3"/><path d="m8.6 10.5 6.8-4"/><path d="m8.6 13.5 6.8 4"/></svg>`;
const themeMoonIcon = `<svg class="theme-icon-moon" viewBox="0 0 20 20" aria-hidden="true"><path d="M14.6 12.1A6.5 6.5 0 0 1 7.4 2.7a6.5 6.5 0 1 0 7.2 9.4z" fill="currentColor"/></svg>`;
const themeSunIcon = `<svg class="theme-icon-sun" viewBox="0 0 20 20" aria-hidden="true"><circle cx="10" cy="10" r="3.4" fill="currentColor"/><g stroke="currentColor" stroke-width="1.6" stroke-linecap="round"><line x1="10" y1="2" x2="10" y2="4"/><line x1="10" y1="16" x2="10" y2="18"/><line x1="2" y1="10" x2="4" y2="10"/><line x1="16" y1="10" x2="18" y2="10"/><line x1="4.2" y1="4.2" x2="5.6" y2="5.6"/><line x1="14.4" y1="14.4" x2="15.8" y2="15.8"/><line x1="4.2" y1="15.8" x2="5.6" y2="14.4"/><line x1="14.4" y1="5.6" x2="15.8" y2="4.2"/></g></svg>`;
const themeSystemIcon = `<svg class="theme-icon-system" viewBox="0 0 20 20" aria-hidden="true"><rect x="3" y="4" width="14" height="10" rx="1.8" fill="none" stroke="currentColor" stroke-width="1.6"/><path d="M7 17h6M10 14v3" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/></svg>`;
const portalBrand = "🦀 crabbox";
const genericProviderIcon = `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 6h16v12H4z"/><path d="m7 10 3 2-3 2M12 15h5"/></svg>`;
const providerIcons: Record<string, string> = {
  "blacksmith-testbox": `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 6h16v5H4z"/><path d="M4 13h16v5H4z"/><path d="M8 8.5h.01M8 15.5h.01M12 8.5h5M12 15.5h5"/></svg>`,
  aws: `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 15.5c3.8 2.2 9.1 2.5 14.8.9"/><path d="M17.5 13.2 20 16l-3.7.7"/><path d="M7 8.5h10l1.8 4H5.2z"/></svg>`,
  azure: `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M7.5 17.5h9.2a4 4 0 0 0 .5-8 5.5 5.5 0 0 0-10.5-1.6A4.8 4.8 0 0 0 7.5 17.5Z"/><path d="M9 13h6"/></svg>`,
  gcp: `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M6 17 3.5 12.5 9.5 2h5L20.5 12.5 18 17z"/><path d="M8.5 17h9.5M9.5 2l3 5.5M14.5 2l-3 5.5"/></svg>`,
  hetzner: `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3 20 7.5v9L12 21l-8-4.5v-9z"/><path d="M8 8v8M16 8v8M8 12h8"/></svg>`,
};

function themeToggleButton(): string {
  return `<button class="icon-btn theme-toggle" type="button" aria-label="Theme: system" aria-pressed="false" title="Theme: system" data-theme-toggle>${themeMoonIcon}${themeSunIcon}${themeSystemIcon}</button>`;
}

interface PortalHeaderOptions {
  variant?: "top" | "bar";
  meta: string;
  actions?: string;
}

export interface PortalLeaseBridgeStatus {
  webVNCBridgeConnected: boolean;
  webVNCViewerConnected: boolean;
  codeBridgeConnected: boolean;
  egress?: {
    profile: string;
    allow: string[];
    hostConnected: boolean;
    clientConnected: boolean;
    updatedAt: string;
  };
}

export interface PortalMacHostRecord {
  id: string;
  provider: string;
  target: string;
  state: string;
  region: string;
  availabilityZone: string;
  instanceType: string;
  autoPlacement: string;
  allocationTime?: string;
  lease?: LeaseRecord;
}

export interface PortalAdminProviderStatus {
  provider: Provider;
  status: "ok" | "warning" | "bad" | "disabled";
  configured: boolean;
  message: string;
  missing: string[];
  machineCount?: number;
  activeLeases: number;
  totalLeases: number;
  users: number;
  recentLeases: PortalAdminLeaseSummary[];
  error?: string;
}

export interface PortalAdminLeaseSummary {
  id: string;
  slug?: string;
  provider: string;
  lifecycle?: LeaseRecord["lifecycle"];
  runtimeAdapterID?: string;
  runtimeAdapterWorkspaceID?: string;
  state: LeaseRecord["state"];
  target: string;
  owner: string;
  org: string;
  class: string;
  serverType: string;
  host?: string;
  createdAt: string;
  expiresAt: string;
  updatedAt: string;
}

export interface PortalAdminUserSummary {
  owner: string;
  orgs: string[];
  activeLeases: number;
  totalLeases: number;
  providers: string[];
  lastSeenAt: string;
}

export interface PortalAdminView {
  generatedAt: string;
  providers: PortalAdminProviderStatus[];
  users: PortalAdminUserSummary[];
  leases: PortalAdminLeaseSummary[];
}

export type PortalAdminTab = "health" | "leases" | "users";

export function portalHome(
  leases: LeaseRecord[],
  runners: ExternalRunnerRecord[],
  request: Request,
  macHosts: PortalMacHostRecord[] = [],
  manageableLeaseIDs: ReadonlySet<string> = new Set(),
): Response {
  const sortedLeases = leases.toSorted((a, b) => leaseSortTime(b).localeCompare(leaseSortTime(a)));
  const active = sortedLeases.filter((lease) => lease.state === "active");
  const ended = sortedLeases.length - active.length;
  const sortedRunners = runners.toSorted((a, b) =>
    runnerSortTime(b).localeCompare(runnerSortTime(a)),
  );
  const activeRunners = sortedRunners.filter((runner) => !runner.stale);
  const activeMacHosts = macHosts.filter((host) => host.state !== "released");
  const admin = request.headers.get("x-crabbox-admin") === "true";
  const owner = request.headers.get("x-crabbox-owner") || "";
  const org = request.headers.get("x-crabbox-org") || "";
  const systemLeases = admin
    ? sortedLeases.filter((lease) => leaseOwnership(lease, owner, org) === "system").length
    : 0;
  const systemRunners = admin
    ? sortedRunners.filter((runner) => runnerOwnership(runner, owner, org) === "system").length
    : 0;
  const system = systemLeases + systemRunners;
  const defaultFilter =
    active.length + activeRunners.length + activeMacHosts.length > 0 ? "active" : "all";
  const filterGroups = portalHomeFilterGroups(sortedLeases, sortedRunners, macHosts, {
    admin,
    defaultState: defaultFilter,
  });
  const rows = [
    ...sortedLeases.map((lease) => ({ kind: "lease" as const, sort: leaseSortTime(lease), lease })),
    ...sortedRunners.map((runner) => ({
      kind: "runner" as const,
      sort: runnerSortTime(runner),
      runner,
    })),
    ...macHosts.map((host) => ({
      kind: "mac-host" as const,
      sort: macHostSortTime(host),
      host,
    })),
  ]
    .toSorted((a, b) => b.sort.localeCompare(a.sort))
    .map((row) =>
      row.kind === "lease"
        ? leaseRow(row.lease, {
            admin,
            owner,
            org,
            canManage: manageableLeaseIDs.has(row.lease.id),
          })
        : row.kind === "runner"
          ? externalRunnerLeaseRow(row.runner, { admin, owner, org })
          : macHostRow(row.host, { admin, owner, org }),
    )
    .join("");
  const summary = admin
    ? `${active.length + activeRunners.length + activeMacHosts.length} active / ${ended} ended / ${sortedRunners.length} external / ${system} system`
    : `${active.length + activeRunners.length + activeMacHosts.length} active / ${ended} ended / ${sortedRunners.length} external`;
  return html(
    "Crabbox Portal",
    `<main class="portal-shell">
      ${portalHeader({
        meta: `${escapeHTML(new URL(request.url).host)}${admin ? ` <span class="pill admin-pill">admin</span>` : ""}`,
        actions: `${admin ? `<a class="button secondary admin-nav-link" href="/portal/admin">${lockIcon}<span>admin</span></a>` : ""}<a class="button secondary" href="/portal/logout">log out</a>`,
      })}
      ${admin ? portalAdminSummary({ owner, org, active: active.length, ended, runners: sortedRunners.length, system, providers: portalProviderSummary(sortedLeases, sortedRunners, macHosts) }) : ""}
      <section class="panel table-panel">
        <div class="section-head">
          <h2>leases</h2>
          <span>${escapeHTML(summary)}</span>
        </div>
        <table class="lease-table" data-portal-table data-page-size="12" data-search-placeholder="search leases" data-filter-groups="${escapeHTML(filterGroups)}" data-filter-default="${defaultFilter}">
          <thead>
            <tr>
              <th>lease</th>
              <th>state</th>
              <th>provider</th>
              <th>target</th>
              <th>class</th>
              <th>access</th>
              <th>time</th>
              <th></th>
            </tr>
          </thead>
          <tbody>${rows || `<tr><td colspan="8" class="empty">no leases or external runners visible</td></tr>`}</tbody>
        </table>
      </section>
    </main>`,
  );
}

export function portalAdmin(
  view: PortalAdminView,
  request: Request,
  tab: PortalAdminTab = "health",
): Response {
  const { generatedAt, providers, users, leases } = view;
  const url = new URL(request.url);
  const owner = request.headers.get("x-crabbox-owner") || "";
  const org = request.headers.get("x-crabbox-org") || "";
  const ok = providers.filter((provider) => provider.status === "ok").length;
  const warning = providers.filter((provider) => provider.status === "warning").length;
  const bad = providers.filter((provider) => provider.status === "bad").length;
  const disabled = providers.filter((provider) => provider.status === "disabled").length;
  const activeLeases = leases.filter((lease) => lease.state === "active").length;
  const failedLeases = leases.filter((lease) => lease.state === "failed").length;
  const providerFilter = normalizeAdminProviderFilter(url.searchParams.get("provider"), providers);
  const filteredLeases = providerFilter
    ? leases.filter((lease) => lease.provider === providerFilter)
    : leases;
  const attention =
    bad > 0 || failedLeases > 0
      ? `${bad} bad provider${bad === 1 ? "" : "s"} / ${failedLeases} failed lease${failedLeases === 1 ? "" : "s"}`
      : warning > 0
        ? `${warning} provider warning${warning === 1 ? "" : "s"}`
        : "clear";
  const providerRows = providers.map((provider) => providerAdminRow(provider)).join("");
  const userRows = users.map((user) => adminUserRow(user)).join("");
  const leaseRows = filteredLeases.map((lease) => adminLeaseRow(lease, providerFilter)).join("");
  const body =
    tab === "users"
      ? adminUsersPanel(users, userRows)
      : tab === "leases"
        ? adminLeasesPanel(providers, filteredLeases, leaseRows, providerFilter)
        : adminHealthPanel(providers, providerRows);
  return html(
    "Crabbox Admin",
    `<main class="portal-shell admin-shell">
      ${portalHeader({
        meta: `${escapeHTML(new URL(request.url).host)} <span class="pill admin-pill">admin</span>`,
        actions: `
          <a class="button secondary" href="/portal">leases</a>
          <a class="button secondary admin-nav-link" href="/portal/admin">${lockIcon}<span>admin</span></a>
          <a class="button secondary" href="/portal/logout">log out</a>
        `,
      })}
      ${adminTabs(tab, providerFilter)}
      <section class="admin-status-strip">
        ${adminMetric("identity", `${owner || "unknown"} / ${org || "unknown"}`)}
        ${adminMetric("attention", attention, bad > 0 || failedLeases > 0 ? "bad" : warning > 0 ? "warn" : "ok")}
        ${adminMetric("providers", `${ok} ok / ${warning} warn / ${bad} bad / ${disabled} off`)}
        ${adminMetric("leases", `${activeLeases} active / ${leases.length} total`)}
        ${adminMetric("users", String(users.length))}
        ${adminMetric("checked", shortTime(generatedAt))}
      </section>
      ${body}
    </main>`,
  );
}

function adminTabs(tab: PortalAdminTab, providerFilter: Provider | undefined): string {
  const leaseHref = `/portal/admin/leases${providerFilter ? `?provider=${encodeURIComponent(providerFilter)}` : ""}`;
  return `<nav class="admin-tabs" aria-label="Admin sections">
    <a href="/portal/admin" data-active="${tab === "health"}">health</a>
    <a href="${leaseHref}" data-active="${tab === "leases"}">leases</a>
    <a href="/portal/admin/users" data-active="${tab === "users"}">users</a>
  </nav>`;
}

function adminHealthPanel(providers: PortalAdminProviderStatus[], providerRows: string): string {
  return `<section class="panel provider-panel">
    <div class="section-head">
      <h2>provider health</h2>
      <span>${providers.length} supported</span>
    </div>
    <div class="provider-status-grid">${providerRows}</div>
  </section>`;
}

function adminUsersPanel(users: PortalAdminUserSummary[], userRows: string): string {
  return `<section class="panel table-panel admin-full-panel">
    <div class="section-head">
      <h2>users</h2>
      <span>${users.length} owners</span>
    </div>
    <table class="admin-user-table" data-portal-table data-page-size="14" data-search-placeholder="search users">
      <thead><tr><th>user</th><th>active</th><th>total</th><th>providers</th><th>last seen</th></tr></thead>
      <tbody>${userRows || `<tr><td colspan="5" class="empty">no lease owners recorded</td></tr>`}</tbody>
    </table>
  </section>`;
}

function adminLeasesPanel(
  providers: PortalAdminProviderStatus[],
  leases: PortalAdminLeaseSummary[],
  leaseRows: string,
  providerFilter: Provider | undefined,
): string {
  const providerControls = [
    `<a class="admin-filter-chip" data-active="${providerFilter === undefined}" href="/portal/admin/leases">all providers</a>`,
    ...providers.map(
      (provider) =>
        `<a class="admin-filter-chip" data-active="${providerFilter === provider.provider}" data-provider="${escapeHTML(provider.provider)}" href="/portal/admin/leases?provider=${encodeURIComponent(provider.provider)}">${providerIcon(provider.provider)}<span>${escapeHTML(coordinatorProviderSpec(provider.provider).label)}</span></a>`,
    ),
  ].join("");
  const title = providerFilter
    ? `${coordinatorProviderSpec(providerFilter).label} leases`
    : "all leases";
  return `<section class="panel table-panel admin-full-panel">
    <div class="section-head">
      <h2>${escapeHTML(title)}</h2>
      <span>${leases.length} shown</span>
    </div>
    <div class="admin-filter-row">${providerControls}</div>
    <table class="admin-lease-table" data-portal-table data-page-size="16" data-search-placeholder="search leases" data-filter-buttons="active:active,provisioning:provisioning,failed:failed,ended:ended,all:all">
      <thead>
        <tr><th>lease</th><th>state</th><th>provider</th><th>owner</th><th>target</th><th>type</th><th>expires</th><th></th></tr>
      </thead>
      <tbody>${leaseRows || `<tr><td colspan="8" class="empty">no leases recorded</td></tr>`}</tbody>
    </table>
  </section>`;
}

function providerAdminRow(provider: PortalAdminProviderStatus): string {
  const details = provider.error
    ? provider.error
    : provider.missing.length > 0
      ? `missing ${provider.missing.join(", ")}`
      : provider.message;
  const machineCount = provider.machineCount === undefined ? "-" : String(provider.machineCount);
  const readinessPath = `/v1/providers/${encodeURIComponent(provider.provider)}/readiness`;
  const machinesPath = `/v1/pool?provider=${encodeURIComponent(provider.provider)}`;
  const providerSpec = coordinatorProviderSpec(provider.provider);
  const auditPath = providerSpec.adminAudit
    ? `/v1/admin/lease-audit?provider=${encodeURIComponent(provider.provider)}`
    : "";
  const actions = provider.configured
    ? [
        `<a href="${readinessPath}">readiness</a>`,
        `<a href="${machinesPath}">machines</a>`,
        `<a href="/portal/admin/leases?provider=${encodeURIComponent(provider.provider)}">leases</a>`,
        auditPath ? `<a href="${auditPath}">audit</a>` : "",
      ]
        .filter(Boolean)
        .join("")
    : [
        `<button type="button" disabled>readiness</button>`,
        `<button type="button" disabled>machines</button>`,
        `<button type="button" disabled>leases</button>`,
        auditPath ? `<button type="button" disabled>audit</button>` : "",
      ]
        .filter(Boolean)
        .join("");
  const tone =
    provider.status === "bad"
      ? "bad"
      : provider.status === "warning"
        ? "warn"
        : provider.status === "ok"
          ? "ok"
          : "";
  return `<article class="provider-status-card" data-tone="${provider.status}">
    <div class="provider-status-head">
      <span class="provider-favicon" data-provider="${escapeHTML(provider.provider)}">${providerIcon(provider.provider)}</span>
      <div class="provider-status-title">
        <strong>${escapeHTML(providerSpec.label)}</strong>
        <span><span class="traffic-light" data-tone="${provider.status}" aria-label="${provider.status}"></span>${escapeHTML(provider.configured ? "configured" : "missing config")}</span>
      </div>
      <span class="pill"${tone ? ` data-tone="${tone}"` : ""}>${escapeHTML(provider.status)}</span>
    </div>
    <dl class="provider-status-meta">
      <div><dt>config</dt><dd>${provider.configured ? "ready" : "missing"}</dd></div>
      <div><dt>machines</dt><dd>${escapeHTML(machineCount)}</dd></div>
      <div><dt>active</dt><dd>${provider.activeLeases}</dd></div>
      <div><dt>users</dt><dd>${provider.users}</dd></div>
    </dl>
    <p>${escapeHTML(details)}</p>
    <div class="provider-status-actions">${actions}</div>
  </article>`;
}

function adminUserRow(user: PortalAdminUserSummary): string {
  return `<tr data-filter-tags="${escapeHTML([user.owner, ...user.orgs, ...user.providers].join(" "))}">
    <td><strong>${escapeHTML(user.owner || "unknown")}</strong><small>${escapeHTML(user.orgs.join(", ") || "no org")}</small></td>
    <td>${user.activeLeases}</td>
    <td>${user.totalLeases}</td>
    <td>${escapeHTML(user.providers.join(", ") || "-")}</td>
    ${timeCell(user.lastSeenAt)}
  </tr>`;
}

function adminLeaseRow(
  lease: PortalAdminLeaseSummary,
  providerFilter: Provider | undefined,
): string {
  const stateGroup =
    lease.state === "active" || lease.state === "provisioning" || lease.state === "failed"
      ? lease.state
      : "ended";
  const returnPath = `/portal/admin/leases${providerFilter ? `?provider=${encodeURIComponent(providerFilter)}` : ""}`;
  const canEject =
    lease.state === "active" || lease.state === "provisioning" || lease.state === "failed";
  const releaseLabel =
    lease.lifecycle === "registered" && lease.runtimeAdapterID && lease.runtimeAdapterWorkspaceID
      ? "Delete workspace"
      : lease.lifecycle === "registered"
        ? "Remove registration"
        : "Emergency release";
  const releaseConfirmation = leaseReleaseConfirmation(lease);
  return `<tr data-filter-tags="${escapeHTML([stateGroup, lease.state, lease.provider, lease.owner, lease.org, lease.target, lease.serverType].join(" "))}">
    <td><a class="lease-link" href="/portal/leases/${encodeURIComponent(lease.id)}"><strong>${escapeHTML(lease.slug || lease.id)}</strong><small>${escapeHTML(lease.id)}</small></a></td>
    <td><span class="pill" data-state="${escapeHTML(lease.state)}">${escapeHTML(lease.state)}</span></td>
    <td>${providerBadge(lease.provider)}</td>
    <td><strong>${escapeHTML(lease.owner || "unknown")}</strong><small>${escapeHTML(lease.org || "no org")}</small></td>
    <td>${targetBadge(lease.target)}</td>
    <td>${escapeHTML(`${lease.class} / ${lease.serverType}`)}</td>
    ${timeCell(lease.state === "active" || lease.state === "provisioning" ? lease.expiresAt : lease.updatedAt)}
    <td>${canEject ? `<form class="admin-eject-form" method="post" action="/portal/leases/${encodeURIComponent(lease.id)}/release?return=${encodeURIComponent(returnPath)}" data-confirm="${escapeHTML(releaseConfirmation)}"><button class="admin-eject" type="submit" title="${releaseLabel} ${escapeHTML(lease.slug || lease.id)}" aria-label="${releaseLabel} ${escapeHTML(lease.slug || lease.id)}">${ejectIcon}</button></form>` : ""}</td>
  </tr>`;
}

function normalizeAdminProviderFilter(
  value: string | null,
  providers: PortalAdminProviderStatus[],
): Provider | undefined {
  const normalized = value?.trim().toLowerCase();
  return providers.some((provider) => provider.provider === normalized)
    ? (normalized as Provider)
    : undefined;
}

function portalAdminSummary(input: {
  owner: string;
  org: string;
  active: number;
  ended: number;
  runners: number;
  system: number;
  providers: string;
}): string {
  return `<section class="panel admin-panel" data-admin-panel>
    <div class="section-head">
      <h2>admin mode</h2>
      <span class="pill admin-pill">all scopes</span>
    </div>
    <div class="admin-grid">
      ${adminMetric("identity", `${input.owner || "unknown"} / ${input.org || "unknown"}`)}
      ${adminMetric("fleet", `${input.active} active / ${input.ended} ended`)}
      ${adminMetric("system", `${input.system} external or other-owner`)}
      ${adminMetric("providers", input.providers || "none")}
    </div>
    <div class="admin-actions">
      <a class="button secondary" href="/v1/admin/leases">leases JSON</a>
      <a class="button secondary" href="/v1/pool">pool JSON</a>
      <a class="button secondary" href="/v1/usage?scope=all">usage JSON</a>
    </div>
  </section>`;
}

function adminMetric(label: string, value: string, tone = ""): string {
  return `<div class="admin-metric"${tone ? ` data-tone="${escapeHTML(tone)}"` : ""}>
    <span>${escapeHTML(label)}</span>
    <strong>${escapeHTML(value)}</strong>
  </div>`;
}

function portalProviderSummary(
  leases: LeaseRecord[],
  runners: ExternalRunnerRecord[],
  macHosts: PortalMacHostRecord[],
): string {
  const counts = new Map<string, number>();
  for (const provider of [
    ...leases.map((lease) => lease.provider),
    ...runners.map((runner) => runner.provider),
    ...macHosts.map((host) => host.provider),
  ]) {
    counts.set(provider, (counts.get(provider) ?? 0) + 1);
  }
  return [...counts.entries()]
    .toSorted(([a], [b]) => a.localeCompare(b))
    .map(([provider, count]) => `${provider}:${count}`)
    .join(" ");
}

export function portalLeaseDetail(
  lease: LeaseRecord,
  runs: RunRecord[],
  bridgeStatus: PortalLeaseBridgeStatus,
  options: { canManage?: boolean } = {},
): Response {
  const slug = lease.slug || lease.id;
  const target = lease.target || "linux";
  const active = lease.state === "active";
  const registered = lease.lifecycle === "registered";
  const canManage = options.canManage === true;
  const runRows = runs.length
    ? runs.map((run) => runRow(run)).join("")
    : `<tr><td colspan="8" class="empty">no recorded runs for this lease</td></tr>`;
  const vncAction =
    active && lease.desktop
      ? `<a class="button" href="/portal/leases/${encodeURIComponent(lease.id)}/vnc">open VNC</a>`
      : `<span class="muted">no desktop</span>`;
  const codeAction =
    active && lease.code
      ? `<a class="button" href="/portal/leases/${encodeURIComponent(lease.id)}/code/">open code</a>`
      : `<span class="muted">no code</span>`;
  const egressAction =
    active && bridgeStatus.egress
      ? `<span class="muted">${escapeHTML(canManage ? egressSummary(bridgeStatus.egress) : "active")}</span>`
      : `<span class="muted">no egress</span>`;
  const commands = active
    ? [
        commandBlock("shell", `crabbox ssh --id ${shellArg(slug)}`),
        commandBlock("run", `crabbox run --id ${shellArg(slug)} -- <command>`),
        lease.desktop && canManage ? commandBlock("WebVNC bridge", webVNCBridgeCommand(lease)) : "",
        lease.code ? commandBlock("code bridge", codeBridgeCommand(lease)) : "",
        bridgeStatus.egress && canManage
          ? commandBlock("egress status", `crabbox egress status --id ${shellArg(slug)}`)
          : "",
        bridgeStatus.egress && canManage
          ? commandBlock("egress stop", `crabbox egress stop --id ${shellArg(slug)}`)
          : "",
      ]
        .filter(Boolean)
        .join("")
    : `<p class="muted">lease ${escapeHTML(lease.state)} ${escapeHTML(shortTime(lease.endedAt || lease.releasedAt || lease.updatedAt))}</p>`;
  return html(
    `${slug} lease`,
    `<main class="portal-shell lease-shell">
      ${portalHeader({
        meta: `${escapeHTML(slug)} · ${escapeHTML(lease.provider)} ${escapeHTML(target)} ${registered ? "registered " : ""}lease <span class="mono">${escapeHTML(lease.id)}</span>`,
        actions: `
          ${
            canManage
              ? `<a class="icon-btn" href="/portal/leases/${encodeURIComponent(lease.id)}/share" title="share lease" aria-label="share lease">${shareIcon}</a>`
              : ""
          }
          <a class="button secondary" href="/portal">leases</a>
          <a class="button secondary" href="/portal/logout">log out</a>
        `,
      })}
      <section class="detail-grid">
        <div class="panel detail-card">
          <div class="section-head">
            <h2>status</h2>
            <span class="pill" data-state="${escapeHTML(lease.state)}">${escapeHTML(lease.state)}</span>
          </div>
          <dl class="meta-grid">
            ${metaHTMLRow("provider", providerBadge(lease.provider))}
            ${metaRow("lifecycle", registered ? (lease.runtimeAdapterID ? "runtime adapter managed" : "client managed") : "coordinator managed")}
            ${metaHTMLRow("target", targetBadge(target, lease.windowsMode))}
            ${metaRow("class", lease.class)}
            ${metaRow("host", lease.host || "pending")}
            ${metaRow("ssh", lease.sshPort ? `${lease.sshUser || "crabbox"}@${lease.host || "host"}:${lease.sshPort}` : "pending")}
            ${metaRow("work root", lease.workRoot || "pending")}
            ${leaseTelemetryRows(lease.telemetry)}
            ${metaRow("expires", shortTime(lease.expiresAt))}
          </dl>
          ${leaseTelemetryTimeline(lease.telemetry, lease.telemetryHistory)}
          ${
            active && canManage
              ? `<form method="post" action="/portal/leases/${encodeURIComponent(lease.id)}/release" class="stop-form" data-confirm="${escapeHTML(leaseReleaseConfirmation(lease))}">
                  <button class="button ${registered && !lease.runtimeAdapterID ? "secondary" : "danger"}" type="submit">${registered ? (lease.runtimeAdapterID ? "delete workspace" : "remove registration") : "stop lease"}</button>
                </form>`
              : ""
          }
        </div>
        <div class="panel detail-card">
          <div class="section-head">
            <h2>access</h2>
            <span>copy locally</span>
          </div>
          <div class="bridge-grid">
            ${bridgeRow("WebVNC", active && lease.desktop === true, bridgeStatus.webVNCBridgeConnected, bridgeStatus.webVNCViewerConnected, vncAction)}
            ${bridgeRow("code", active && lease.code === true, bridgeStatus.codeBridgeConnected, false, codeAction)}
            ${bridgeRow("egress", active && Boolean(bridgeStatus.egress), bridgeStatus.egress?.hostConnected ?? false, bridgeStatus.egress?.clientConnected ?? false, egressAction, "host", "client")}
          </div>
          <div class="access-commands">${commands}</div>
        </div>
      </section>
      <section class="panel table-panel">
        <div class="section-head">
          <h2>recent runs</h2>
          <span>${runs.length}</span>
        </div>
        <table class="run-table" data-portal-table data-page-size="8" data-search-placeholder="search runs" data-filter-buttons="succeeded:succeeded,failed:failed,running:running,all:all">
          <thead>
            <tr>
              <th>run</th>
              <th>state</th>
              <th>started</th>
              <th>duration</th>
              <th>box</th>
              <th></th>
            </tr>
          </thead>
          <tbody>${runRows}</tbody>
        </table>
      </section>
    </main>`,
  );
}

export function portalShareLease(
  lease: LeaseRecord,
  options: { embedded?: boolean } = {},
): Response {
  const slug = lease.slug || lease.id;
  const sharePath = `/portal/leases/${encodeURIComponent(lease.id)}/share${options.embedded ? "?embed=1" : ""}`;
  const users = Object.entries(lease.share?.users ?? {}).toSorted(([a], [b]) => a.localeCompare(b));
  const userRows = users.length
    ? users
        .map(
          ([user, role]) => `<tr>
            <td>${escapeHTML(user)}</td>
            <td><span class="pill">${escapeHTML(role)}</span></td>
            <td>
              <form method="post" action="${sharePath}">
                <input type="hidden" name="action" value="remove-user">
                <input type="hidden" name="user" value="${escapeHTML(user)}">
                <button class="button secondary" type="submit">remove</button>
              </form>
            </td>
          </tr>`,
        )
        .join("")
    : `<tr><td colspan="3" class="empty">no shared users</td></tr>`;
  return html(
    `Share ${slug}`,
    `<main class="portal-shell run-shell share-shell${options.embedded ? " share-shell-embedded" : ""}">
      ${
        options.embedded
          ? ""
          : portalHeader({
              meta: `share ${escapeHTML(slug)} <span class="mono">${escapeHTML(lease.id)}</span>`,
              actions: `
                <a class="button secondary" href="/portal/leases/${encodeURIComponent(lease.id)}">back to lease</a>
                <a class="button secondary" href="/portal">leases</a>
                <a class="button secondary" href="/portal/logout">log out</a>
              `,
            })
      }
      <section class="panel">
        <div class="section-head">
          <h2>org access</h2>
          <span class="pill">${escapeHTML(lease.share?.org ?? "off")}</span>
        </div>
        <form class="share-form" method="post" action="${sharePath}">
          <input type="hidden" name="action" value="set-org">
          <select name="role" aria-label="org role">
            <option value=""${lease.share?.org ? "" : " selected"}>off</option>
            <option value="use"${lease.share?.org === "use" ? " selected" : ""}>use</option>
            <option value="manage"${lease.share?.org === "manage" ? " selected" : ""}>manage</option>
          </select>
          <button class="button action" type="submit">save</button>
        </form>
      </section>
      <section class="panel">
        <div class="section-head"><h2>users</h2></div>
        <form class="share-form" method="post" action="${sharePath}">
          <input type="hidden" name="action" value="add-user">
          <input name="user" type="email" placeholder="friend@example.com" required>
          <select name="role" aria-label="user role">
            <option value="use">use</option>
            <option value="manage">manage</option>
          </select>
          <button class="button action" type="submit">add</button>
        </form>
        <div class="table-scroll">
          <table>
            <thead><tr><th>user</th><th>role</th><th></th></tr></thead>
            <tbody>${userRows}</tbody>
          </table>
        </div>
      </section>
      <form method="post" action="${sharePath}">
        <input type="hidden" name="action" value="clear">
        <button class="button danger" type="submit">clear sharing</button>
      </form>
    </main>`,
    200,
    "",
    options.embedded ? { frameAncestors: "'self'" } : {},
  );
}

export function portalExternalRunnerDetail(
  runner: ExternalRunnerRecord,
  context: { admin: boolean },
): Response {
  const actionState = externalRunnerActionState(runner);
  const actionsLinks = externalRunnerActionsLinks(runner);
  const stopCommand = externalRunnerStopCommand(runner);
  const ownerLabel = context.admin ? `${runner.owner} / ${runner.org}` : runner.org;
  const actionSummary = [
    runner.actionsRepo,
    runner.actionsRunID ? `run ${runner.actionsRunID}` : undefined,
    runner.actionsRunStatus,
    runner.actionsRunConclusion,
  ]
    .filter(Boolean)
    .join(" · ");
  return html(
    `${runner.id} runner`,
    `<main class="portal-shell runner-shell">
      ${portalHeader({
        meta: `${escapeHTML(runner.id)} · ${escapeHTML(runner.provider)} external runner`,
        actions: `
          <a class="button secondary" href="/portal">leases</a>
          <a class="button secondary" href="/portal/logout">log out</a>
        `,
      })}
      <section class="detail-grid">
        <div class="panel detail-card">
          <div class="section-head">
            <h2>runner</h2>
            <div class="state-stack">
              <span class="pill" data-tone="${runner.stale ? "warn" : runnerStatusTone(runner.status)}">${escapeHTML(runner.status || "-")}</span>
              ${externalRunnerActionBadge(actionState)}
            </div>
          </div>
          <dl class="meta-grid">
            ${metaRow("id", runner.id)}
            ${metaHTMLRow("provider", providerBadge(runner.provider))}
            ${metaRow("owner", ownerLabel)}
            ${metaRow("visibility", "external")}
            ${metaRow("first seen", shortTime(runner.firstSeenAt))}
            ${metaRow("last seen", shortTime(runner.lastSeenAt))}
            ${metaRow("updated", shortTime(runner.updatedAt))}
            ${metaRow("created", runner.createdAt ? shortTime(runner.createdAt) : undefined)}
          </dl>
        </div>
        <div class="panel detail-card">
          <div class="section-head">
            <h2>actions owner</h2>
            <span>${actionsLinks || "not inferred"}</span>
          </div>
          <dl class="meta-grid">
            ${metaRow("repo", runner.actionsRepo || runner.repo)}
            ${metaRow("workflow", runner.actionsWorkflowName || workflowBasename(runner.workflow))}
            ${metaRow("job", runner.job)}
            ${metaRow("ref", runner.ref)}
            ${metaRow("run", runner.actionsRunID)}
            ${metaRow("state", actionSummary || runner.actionsRunStatus || runner.actionsRunConclusion)}
          </dl>
        </div>
      </section>
      <section class="panel command-panel">
        <div class="section-head">
          <h2>operator action</h2>
          <span>local only</span>
        </div>
        <div class="commands">
          ${stopCommand ? commandBlock("stop runner", stopCommand) : `<p class="empty">no local stop command available</p>`}
        </div>
      </section>
      <section class="panel detail-card">
        <div class="section-head">
          <h2>boundary</h2>
          <span>visibility only</span>
        </div>
        <p class="detail-note">Blacksmith owns the machine, workspace, logs, and lifecycle. Crabbox stores this row so the portal can show who owns the runner and whether the backing workflow looks stuck. There is no Crabbox SSH, VNC, code, telemetry, cost, or expiry control for this row.</p>
      </section>
    </main>`,
  );
}

export function portalMacHostDetail(
  host: PortalMacHostRecord,
  bridgeStatus: PortalLeaseBridgeStatus | undefined,
  options: { canManage?: boolean } = {},
): Response {
  const lease = host.lease;
  const activeLease = lease?.state === "active" ? lease : undefined;
  const canManage = options.canManage === true;
  const stateTone = macHostStateTone(host.state);
  const hostID = shortHostID(host.id);
  const startDesktopCommand = activeLease ? "" : macHostStartDesktopCommand(host);
  const activeLeaseVNC = activeLease
    ? activeLease.desktop === true || activeLease.target === "macos"
    : false;
  const vncAction =
    activeLease && activeLeaseVNC
      ? `<a class="button" href="/portal/leases/${encodeURIComponent(activeLease.id)}/vnc">open VNC</a>`
      : activeLease
        ? `<form method="post" action="${macHostVNCPath(host)}"><button class="button" type="submit">enable VNC</button></form>`
        : `<button class="button" type="button" data-copy-value="${escapeHTML(startDesktopCommand)}">copy start command</button>`;
  const codeAction =
    activeLease?.code === true
      ? `<a class="button" href="/portal/leases/${encodeURIComponent(activeLease.id)}/code/">open code</a>`
      : `<span class="muted">${activeLease ? "no code" : "no active lease"}</span>`;
  const commands = activeLease
    ? [
        commandBlock("shell", `crabbox ssh --id ${shellArg(activeLease.slug || activeLease.id)}`),
        commandBlock(
          "run",
          `crabbox run --id ${shellArg(activeLease.slug || activeLease.id)} -- <command>`,
        ),
        activeLeaseVNC && canManage
          ? commandBlock("WebVNC bridge", webVNCBridgeCommand(activeLease))
          : "",
        activeLease.code ? commandBlock("code bridge", codeBridgeCommand(activeLease)) : "",
      ]
        .filter(Boolean)
        .join("")
    : [
        commandBlock("start desktop lease", startDesktopCommand),
        commandBlock("open WebVNC", "crabbox webvnc --id <lease-id-or-slug> --open"),
        commandBlock(
          "host-pinned macOS run",
          `CRABBOX_HOST_ID=${shellArg(host.id)} crabbox run --provider ${shellArg(host.provider)} --target macos --market on-demand --desktop -- <command>`,
        ),
      ].join("");
  return html(
    `${hostID} dedicated host`,
    `<main class="portal-shell runner-shell">
      ${portalHeader({
        meta: `${escapeHTML(host.id)} · ${escapeHTML(host.provider)} ${escapeHTML(host.target)} dedicated host`,
        actions: `
          <a class="button secondary" href="/portal">leases</a>
          <a class="button secondary" href="/portal/logout">log out</a>
        `,
      })}
      <section class="detail-grid">
        <div class="panel detail-card">
          <div class="section-head">
            <h2>dedicated host</h2>
            <span class="pill" data-tone="${stateTone}">${escapeHTML(host.state || "-")}</span>
          </div>
          <dl class="meta-grid">
            ${metaRow("id", host.id)}
            ${metaHTMLRow("provider", providerBadge(host.provider))}
            ${metaHTMLRow("target", targetBadge(host.target))}
            ${metaRow("type", host.instanceType)}
            ${metaRow("region", host.region)}
            ${metaRow("zone", host.availabilityZone)}
            ${metaRow("placement", host.autoPlacement)}
            ${metaRow("allocated", host.allocationTime ? shortTime(host.allocationTime) : undefined)}
          </dl>
        </div>
        <div class="panel detail-card">
          <div class="section-head">
            <h2>attached lease</h2>
            <span>${activeLease ? escapeHTML(activeLease.slug || activeLease.id) : "none"}</span>
          </div>
          ${
            activeLease
              ? `<dl class="meta-grid">
                  ${metaRow("lease", activeLease.slug ? `${activeLease.slug} / ${activeLease.id}` : activeLease.id)}
                  ${metaRow("host", activeLease.host || "pending")}
                  ${metaRow("ssh", activeLease.sshPort ? `${activeLease.sshUser || "crabbox"}@${activeLease.host || "host"}:${activeLease.sshPort}` : "pending")}
                  ${metaRow("desktop", activeLeaseVNC ? "enabled" : "disabled")}
                  ${metaRow("expires", shortTime(activeLease.expiresAt))}
                </dl>`
              : `<p class="detail-note">No active Crabbox lease is attached to this Dedicated Host. It is still usable as macOS capacity for a host-pinned run.</p>`
          }
        </div>
      </section>
      <section class="panel detail-card">
        <div class="section-head">
          <h2>access</h2>
          <span>${activeLease ? "copy locally" : "host pin"}</span>
        </div>
        <div class="bridge-grid">
          ${bridgeRow("WebVNC", activeLeaseVNC, bridgeStatus?.webVNCBridgeConnected ?? false, bridgeStatus?.webVNCViewerConnected ?? false, vncAction)}
          ${bridgeRow("code", activeLease?.code === true, bridgeStatus?.codeBridgeConnected ?? false, false, codeAction)}
        </div>
        <div id="access-commands" class="access-commands">${commands}</div>
      </section>
    </main>`,
  );
}

export function portalRunDetail(
  run: RunRecord,
  events: RunEventRecord[],
  logTail: string,
): Response {
  const stateTone = run.state === "succeeded" ? "ok" : run.state === "failed" ? "bad" : "warn";
  const eventRows = events.length
    ? events.map((event) => eventRow(event)).join("")
    : `<tr><td colspan="5" class="empty">no events recorded</td></tr>`;
  const failureRows = run.results?.failed.length
    ? run.results.failed
        .slice(0, 8)
        .map(
          (failure) => `<li>
            <strong>${escapeHTML(failure.name)}</strong>
            <small>${escapeHTML([failure.suite, failure.file].filter(Boolean).join(" / "))}</small>
            ${failure.message ? `<p>${escapeHTML(truncate(failure.message, 240))}</p>` : ""}
          </li>`,
        )
        .join("")
    : "";
  const logBlock = logTail
    ? `<pre id="run-log-tail" class="log-preview">${escapeHTML(logTail)}</pre>`
    : `<p class="empty">no retained log output</p>`;
  return html(
    `${run.id} run`,
    `<main class="portal-shell run-shell">
      ${portalHeader({
        meta: `${escapeHTML(run.id)} · ${escapeHTML(run.slug || run.leaseID)} · ${escapeHTML(run.state)}`,
        actions: `
          <a class="button secondary" href="/portal/leases/${encodeURIComponent(run.leaseID)}">lease</a>
          <a class="button secondary" href="/portal">leases</a>
          <a class="button secondary" href="/portal/logout">log out</a>
        `,
      })}
      <section class="detail-grid">
        <div class="panel detail-card run-summary-card">
          <div class="section-head">
            <h2>run</h2>
            <span class="pill" data-tone="${stateTone}">${escapeHTML(run.state)}</span>
          </div>
          <dl class="meta-grid">
            ${metaRow("lease", run.slug ? `${run.slug} / ${run.leaseID}` : run.leaseID)}
            ${run.label ? metaRow("label", run.label) : ""}
            ${metaHTMLRow("provider", providerBadge(run.provider))}
            ${metaHTMLRow("target", targetBadge(run.target || "linux", run.windowsMode))}
            ${metaRow("class", run.class)}
            ${metaRow("server type", run.serverType)}
            ${metaRow("phase", run.phase || run.state)}
            ${metaRow("exit", formatExitCode(run.exitCode))}
            ${run.blockedStage === "unknown" ? metaRow("area", "command") : run.blockedStage ? metaRow("blocked", run.blockedStage) : ""}
            ${run.retryLikely && run.retryLikely !== "unknown" ? metaRow("retry", run.retryLikely) : ""}
            ${metaRow("started", shortTime(run.startedAt))}
            ${metaRow("duration", formatDuration(run.durationMs))}
            ${metaRow("log", run.logBytes > 0 ? formatBytes(run.logBytes) : "empty")}
          </dl>
          ${runTelemetryPanel(run.telemetry)}
        </div>
        <div class="panel detail-card run-artifact-card">
          <div class="section-head">
            <h2>artifacts</h2>
            <span>${run.results ? "junit" : "logs"}</span>
          </div>
          <div class="run-artifacts">
            <a class="button" href="/portal/runs/${encodeURIComponent(run.id)}/logs">raw logs</a>
            <a class="button secondary" href="/portal/runs/${encodeURIComponent(run.id)}/events">events json</a>
            ${resultsSummary(run)}
          </div>
        </div>
      </section>
      <section class="panel command-panel">
        <div class="section-head">
          <h2>command</h2>
          <span>${escapeHTML(run.owner)}</span>
        </div>
        <div class="commands">${commandBlock("remote command", run.command.join(" "))}</div>
      </section>
      ${
        failureRows
          ? `<section class="panel">
              <div class="section-head">
                <h2>failures</h2>
                <span>${run.results?.failed.length ?? 0}</span>
              </div>
              <ul class="failure-list">${failureRows}</ul>
            </section>`
          : ""
      }
      <section class="panel log-panel">
        <div class="section-head">
          <h2>log tail</h2>
          <div class="section-actions">
            <span>${run.logTruncated ? "truncated" : "retained"}</span>
            ${
              logTail
                ? `<button class="icon-btn" type="button" title="copy log tail" aria-label="copy log tail" data-copy-target="#run-log-tail">${copyIcon}</button>`
                : ""
            }
          </div>
        </div>
        ${logBlock}
      </section>
      <section class="panel table-panel">
        <div class="section-head">
          <h2>events</h2>
          <span>${events.length}</span>
        </div>
        <table class="event-table" data-portal-table data-page-size="12" data-search-placeholder="search events" data-filter-buttons="run:run,command:command,sync:sync,stdout:stdout,stderr:stderr,all:all">
          <thead>
            <tr>
              <th>seq</th>
              <th>type</th>
              <th>phase</th>
              <th>time</th>
              <th>message</th>
            </tr>
          </thead>
          <tbody>${eventRows}</tbody>
        </table>
      </section>
    </main>`,
  );
}

export function portalVNC(lease: LeaseRecord, options: { canManage?: boolean } = {}): Response {
  const nonce = scriptNonce();
  const slug = lease.slug || lease.id;
  const target = lease.target || "linux";
  const title = `WebVNC ${slug}`;
  const wsPath = `/portal/leases/${encodeURIComponent(lease.id)}/vnc/viewer`;
  const statusPath = `/portal/leases/${encodeURIComponent(lease.id)}/vnc/status`;
  const controlPath = `/portal/leases/${encodeURIComponent(lease.id)}/vnc/control`;
  const themePath = `/portal/leases/${encodeURIComponent(lease.id)}/vnc/theme`;
  const handoffPath = `/portal/leases/${encodeURIComponent(lease.id)}/vnc/handoff`;
  const sharePath = `/portal/leases/${encodeURIComponent(lease.id)}/share`;
  const shareAPIPath = `${sharePath}?format=json`;
  const canManage = options.canManage === true;
  const shareData = canManage
    ? {
        leaseID: lease.id,
        slug,
        owner: lease.owner,
        org: lease.org,
        share: {
          users: lease.share?.users ?? {},
          org: lease.share?.org ?? "",
        },
      }
    : {
        leaseID: lease.id,
        slug,
        owner: "",
        org: "",
        share: {
          users: {},
          org: "",
        },
      };
  const bridgeCmd = canManage ? webVNCBridgeCommand(lease) : "";
  const bridgeMissingMessage = canManage
    ? "WebVNC daemon not running; run the bridge command below"
    : "WebVNC daemon not running; ask a lease manager to start or refresh the bridge";
  const fullscreenIcon = `<svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M4 9V4h5"/><path d="M20 9V4h-5"/><path d="M4 15v5h5"/><path d="M20 15v5h-5"/></svg>`;
  const reconnectIcon = `<svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M21 12a9 9 0 1 1-3-6.7"/><path d="M21 4v5h-5"/></svg>`;
  const pasteIcon = `<svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M9 2h6a2 2 0 0 1 2 2v1H7V4a2 2 0 0 1 2-2Z"/><path d="M16 4h2a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2h2"/><path d="M12 11v6"/><path d="m9 14 3 3 3-3"/></svg>`;
  return html(
    title,
    `<main class="vnc-page">
      ${portalHeader({
        variant: "bar",
        meta: `<span>WebVNC ${escapeHTML(slug)}</span><span class="vnc-dot"></span>${providerBadge(lease.provider)}<span class="vnc-dot"></span>${targetBadge(target, lease.windowsMode)}<span class="vnc-dot"></span><span class="vnc-id">${escapeHTML(lease.id)}</span>`,
        actions: `
          <span id="status" class="status-pill">waiting for bridge</span>
          <button id="vnc-takeover" class="button secondary vnc-control" type="button" hidden>take control</button>
          <button id="vnc-copy-remote" class="icon-btn" type="button" title="copy remote clipboard" aria-label="copy remote clipboard" disabled>${copyIcon}</button>
          <button id="vnc-paste" class="icon-btn" type="button" title="paste clipboard" aria-label="paste clipboard">${pasteIcon}</button>
          <button id="vnc-reconnect" class="icon-btn" type="button" title="reconnect" aria-label="reconnect">${reconnectIcon}</button>
          <button id="vnc-fullscreen" class="icon-btn" type="button" title="fullscreen" aria-label="toggle fullscreen">${fullscreenIcon}</button>
          ${canManage ? `<button id="vnc-share" class="button secondary" type="button">share</button>` : ""}
          <a class="button secondary" href="/portal">leases</a>
          <a class="button secondary" href="/portal/logout">log out</a>
        `,
      })}
      <section id="screen" class="screen" aria-label="WebVNC display" tabindex="0"></section>
      ${
        canManage
          ? `<footer class="vnc-bridge">
        <span class="vnc-bridge-label">bridge</span>
        <code id="vnc-bridge-cmd" class="vnc-bridge-cmd">${escapeHTML(bridgeCmd)}</code>
        <button id="vnc-copy" class="icon-btn" type="button" title="copy command" aria-label="copy bridge command">${copyIcon}</button>
      </footer>`
          : ""
      }
      ${
        canManage
          ? `<dialog id="vnc-share-dialog" class="vnc-share-dialog" aria-label="Share lease">
        <div class="vnc-share-head">
          <div><strong>Share ${escapeHTML(slug)}</strong><small>${escapeHTML(lease.id)}</small></div>
          <button id="vnc-share-close" class="icon-btn" type="button" title="close share" aria-label="close share">×</button>
        </div>
        <div class="vnc-share-body">
          <div class="vnc-share-add" aria-label="Add people">
            <input id="vnc-share-user" type="email" autocomplete="email" placeholder="friend@example.com" aria-label="person email">
            <select id="vnc-share-role" aria-label="new user role">
              <option value="use">Can use</option>
              <option value="manage">Can manage</option>
            </select>
            <button id="vnc-share-add" class="button action" type="button">add</button>
          </div>
          <section class="vnc-share-section" aria-label="People with access">
            <h2>People with access</h2>
            <div id="vnc-share-people" class="vnc-share-list"></div>
          </section>
          <section class="vnc-share-section" aria-label="General access">
            <h2>General access</h2>
            <div class="vnc-share-access-row">
              <div class="vnc-share-avatar" aria-hidden="true">org</div>
              <div class="vnc-share-access-text">
                <strong>${escapeHTML(lease.org || "org")}</strong>
                <span id="vnc-share-org-summary">Only explicitly shared users can access</span>
              </div>
              <select id="vnc-share-org" aria-label="org access">
                <option value="">Off</option>
                <option value="use">Can use</option>
                <option value="manage">Can manage</option>
              </select>
            </div>
          </section>
          <p id="vnc-share-status" class="vnc-share-status" role="status"></p>
        </div>
        <div class="vnc-share-foot">
          <button id="vnc-share-copy-link" class="button secondary" type="button">copy WebVNC link</button>
          <button id="vnc-share-clear" class="button secondary danger-text" type="button">clear sharing</button>
          <button id="vnc-share-done" class="button" type="button">done</button>
        </div>
      </dialog>`
          : ""
      }
    </main>
    <script type="module" nonce="${nonce}">
      import RFBModule from ${JSON.stringify(novncModuleURL)};
      const RFB = RFBModule.default || RFBModule;
      const status = document.getElementById("status");
      const screen = document.getElementById("screen");
      const wsURL = new URL(${JSON.stringify(wsPath)}, window.location.href);
      wsURL.protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
      const statusURL = new URL(${JSON.stringify(statusPath)}, window.location.href);
      const controlURL = new URL(${JSON.stringify(controlPath)}, window.location.href);
      const themeURL = new URL(${JSON.stringify(themePath)}, window.location.href);
      const handoffURL = new URL(${JSON.stringify(handoffPath)}, window.location.href);
      const sharePageURL = new URL(${JSON.stringify(sharePath)}, window.location.href);
      const shareAPIURL = new URL(${JSON.stringify(shareAPIPath)}, window.location.href);
      const shareInitial = ${scriptJSON(shareData)};
      const viewerID = "viewer_" + (crypto.randomUUID?.() || String(Date.now()) + Math.random()).replace(/[^A-Za-z0-9_.:-]/g, "");
      wsURL.searchParams.set("viewer", viewerID);
      statusURL.searchParams.set("viewer", viewerID);
      const fragment = new URLSearchParams(window.location.hash.slice(1));
      const target = ${JSON.stringify(target)};
      let username = fragment.get("username") || "";
      let password = fragment.get("password") || "";
      const handoffTicket = fragment.get("handoff") || "";
      let credentialsReady = !handoffTicket;
      const takeControlOnConnect = fragment.get("control") === "take";
      const bridgeMissingMessage = ${JSON.stringify(bridgeMissingMessage)};
      const missingVNCCredentialMessage = "VNC credentials missing; open WebVNC from crabbox webvnc status";
      const failedVNCCredentialMessage = "VNC authentication failed; reopen WebVNC from crabbox webvnc status";
      function rfbOptions() {
        const credentials = {};
        if (username) credentials.username = username;
        if (password) credentials.password = password;
        return Object.keys(credentials).length ? { credentials } : {};
      }
      async function loadHandoffCredentials() {
        if (credentialsReady) return;
        const response = await fetch(handoffURL, {
          method: "POST",
          headers: { "content-type": "application/json", accept: "application/json" },
          body: JSON.stringify({ ticket: handoffTicket }),
        });
        const body = await response.json().catch(() => ({}));
        if (!response.ok) throw new Error(body.message || body.error || "VNC handoff failed");
        username = typeof body.username === "string" ? body.username : "";
        password = typeof body.password === "string" ? body.password : "";
        credentialsReady = true;
        const cleanFragment = new URLSearchParams(window.location.hash.slice(1));
        cleanFragment.delete("handoff");
        const cleanURL = new URL(window.location.href);
        cleanURL.hash = cleanFragment.toString();
        window.history.replaceState(null, "", cleanURL);
      }
      function setStatus(value, tone = "") {
        status.textContent = value;
        status.dataset.tone = tone;
      }
      let rfb;
      let retryTimer;
      let retryAttempt = 0;
      let connected = false;
      let stopped = false;
      let remoteClipboardText = "";
      let statusTimer;
      let controllerLabel = "";
      let isController = false;
      let takeControlAttempted = false;
      let credentialsSent = false;
      let authenticationFailed = false;
      let pendingDesktopTheme = "";
      let lastDesktopTheme = "";
      let desktopThemeTimer;
      const terminalStatusCodes = new Set([403, 404, 409, 410]);
      function focusVNC() {
        if (!isController || document.body.dataset.portalDialogOpen === "true") return;
        try {
          screen.focus({ preventScroll: true });
        } catch (_) {
          screen.focus();
        }
        try {
          rfb?.focus?.({ preventScroll: true });
        } catch (_) {}
      }
      function captureVNCInput(event, options = {}) {
        if (!isController) return;
        if (options.preventDefault && event.cancelable) event.preventDefault();
        focusVNC();
      }
      screen.addEventListener("contextmenu", (event) => {
        event.preventDefault();
      });
      screen.addEventListener("pointerdown", (event) => captureVNCInput(event), { capture: true });
      screen.addEventListener("mousedown", (event) => captureVNCInput(event, { preventDefault: true }), { capture: true });
      function retryDelay() {
        return Math.min(5000, 500 * 2 ** retryAttempt);
      }
      async function responseMessage(response, fallback) {
        try {
          const body = await response.json();
          return body.message || body.error || fallback;
        } catch (_) {
          return fallback;
        }
      }
      function fallbackCopyText(text) {
        const ta = document.createElement("textarea");
        ta.value = text;
        ta.setAttribute("readonly", "");
        ta.style.position = "fixed";
        ta.style.left = "-9999px";
        document.body.appendChild(ta);
        ta.select();
        try {
          document.execCommand("copy");
        } finally {
          ta.remove();
        }
      }
      async function writeClipboardText(text) {
        if (navigator.clipboard?.writeText) {
          try {
            await navigator.clipboard.writeText(text);
            return;
          } catch (_) {}
        }
        fallbackCopyText(text);
      }
      async function bridgeState() {
        try {
          const response = await fetch(statusURL, { cache: "no-store" });
          if (response.ok) {
            return await response.json();
          }
          const message = await responseMessage(response, "WebVNC bridge unavailable");
          if (terminalStatusCodes.has(response.status)) {
            return { terminal: true, message };
          }
          return { transient: true, message };
        } catch (error) {
          return { transient: true, message: error instanceof Error ? error.message : String(error) };
        }
      }
      function applyCollaborationState(state) {
        if (!state) return;
        const role = state.viewerRole || "none";
        const takeoverBtn = document.getElementById("vnc-takeover");
        const previousControllerLabel = controllerLabel;
        const wasController = isController;
        controllerLabel = state.controllerLabel || "";
        const controlling = role === "controller";
        const connectedViewer = role === "controller" || role === "observer";
        isController = controlling;
        if (rfb) {
          rfb.viewOnly = !controlling;
          if (controlling) window.setTimeout(focusVNC, 0);
        }
        if (takeoverBtn) {
          takeoverBtn.hidden = !connectedViewer;
          takeoverBtn.disabled = controlling || !connectedViewer;
          takeoverBtn.dataset.role = controlling ? "controller" : "observer";
          takeoverBtn.textContent = controlling ? "you control" : "take control";
          takeoverBtn.title = controlling
            ? "You are controlling this session"
            : controllerLabel
              ? "Currently observing; " + controllerLabel + " controls"
              : "Currently observing";
        }
        if (!controlling && connectedViewer && previousControllerLabel && controllerLabel && previousControllerLabel !== controllerLabel) {
          setStatus(controllerLabel + " took control", "warn");
        }
        if (controlling && !wasController) {
          queueDesktopTheme();
        }
      }
      async function refreshCollaborationState() {
        const state = await bridgeState();
        applyCollaborationState(state);
        return state;
      }
      async function refreshCollaborationStateAndMaybeTakeControl() {
        const state = await refreshCollaborationState();
        await takeControlIfRequested(state);
        return state;
      }
      async function takeControl(label = "you took control") {
        const response = await fetch(controlURL, {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ viewerID }),
        });
        const state = response.ok ? await response.json() : undefined;
        if (!response.ok) throw new Error(state?.message || "takeover failed");
        applyCollaborationState(state);
        setStatus(label, "ok");
        queueDesktopTheme();
        focusVNC();
        return state;
      }
      async function takeControlIfRequested(state) {
        if (!takeControlOnConnect || takeControlAttempted) return;
        if (state?.viewerRole === "controller") {
          takeControlAttempted = true;
          return;
        }
        if (state?.viewerRole !== "observer") return;
        await takeControl();
        takeControlAttempted = true;
      }
      function resolvedPortalTheme() {
        return document.documentElement.dataset.theme === "light" ? "light" : "dark";
      }
      function queueDesktopTheme(theme = resolvedPortalTheme()) {
        if (target !== "linux") return;
        pendingDesktopTheme = theme === "light" ? "light" : "dark";
        if (!connected) return;
        window.clearTimeout(desktopThemeTimer);
        desktopThemeTimer = window.setTimeout(syncDesktopTheme, 120);
      }
      async function syncDesktopTheme() {
        if (target !== "linux" || !connected || !pendingDesktopTheme || pendingDesktopTheme === lastDesktopTheme) return;
        const theme = pendingDesktopTheme;
        const response = await fetch(themeURL, {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ viewerID, theme }),
        });
        if (!response.ok) return;
        lastDesktopTheme = theme;
      }
      window.addEventListener("crabbox-theme-change", (event) => {
        queueDesktopTheme(event.detail?.mode);
      });
      function clearDesktopThemeSyncState() {
        lastDesktopTheme = "";
        window.clearTimeout(desktopThemeTimer);
      }
      function stopPolling(label) {
        stopped = true;
        connected = false;
        window.clearTimeout(retryTimer);
        window.clearInterval(statusTimer);
        clearDesktopThemeSyncState();
        try { rfb?.disconnect(); } catch (_) {}
        screen.replaceChildren();
        setStatus(label, "bad");
      }
      function scheduleRetry(label) {
        if (stopped) return;
        const delay = retryDelay();
        retryAttempt += 1;
        setStatus(label + "; retrying in " + Math.ceil(delay / 1000) + "s", "warn");
        window.clearTimeout(retryTimer);
        retryTimer = window.setTimeout(connect, delay);
      }
      async function connect() {
        if (stopped) return;
        connected = false;
        credentialsSent = false;
        authenticationFailed = false;
        screen.replaceChildren();
        try {
          await loadHandoffCredentials();
          const state = await bridgeState();
          if (state?.terminal) {
            stopPolling(state.message || "WebVNC bridge unavailable");
            return;
          }
          if (state?.transient) {
            scheduleRetry(state.message || "WebVNC status unavailable");
            return;
          }
          if (state && !state.bridgeConnected) {
            scheduleRetry(state.message || bridgeMissingMessage);
            return;
          }
          if (state && state.availableViewerSlots === 0) {
            scheduleRetry(state.message || "waiting for an available WebVNC observer slot");
            return;
          }
          if (target === "macos" && !password) {
            stopPolling(missingVNCCredentialMessage);
            return;
          }
          setStatus(retryAttempt ? "bridge connected; opening viewer" : "connecting");
          clearDesktopThemeSyncState();
          rfb = new RFB(screen, wsURL.toString(), rfbOptions());
          rfb.showDotCursor = true;
          rfb.focusOnClick = true;
          rfb.scaleViewport = true;
          rfb.resizeSession = false;
          rfb.viewOnly = true;
          if (target === "macos") {
            rfb.compressionLevel = 1;
            rfb.qualityLevel = 2;
          } else {
            rfb.compressionLevel = 0;
            rfb.qualityLevel = 6;
          }
          rfb.addEventListener("connect", () => {
            connected = true;
            retryAttempt = 0;
            setStatus("connected", "ok");
            queueDesktopTheme();
            void refreshCollaborationStateAndMaybeTakeControl().then(focusVNC).catch(() => {});
            window.clearInterval(statusTimer);
            statusTimer = window.setInterval(() => {
              void refreshCollaborationStateAndMaybeTakeControl().catch(() => {});
            }, 1500);
          });
          rfb.addEventListener("clipboard", (event) => {
            remoteClipboardText = event.detail?.text || "";
            if (copyRemoteBtn) {
              copyRemoteBtn.disabled = !remoteClipboardText;
            }
            if (remoteClipboardText) {
              setStatus("remote clipboard ready", "ok");
            }
          });
          rfb.addEventListener("disconnect", () => {
            if (stopped) return;
            const wasConnected = connected;
            connected = false;
            clearDesktopThemeSyncState();
            if (!wasConnected && (authenticationFailed || credentialsSent)) {
              stopPolling(authenticationFailed ? failedVNCCredentialMessage : "VNC authentication timed out; reopen WebVNC from crabbox webvnc status");
              return;
            }
            scheduleRetry(wasConnected ? "VNC bridge disconnected" : "waiting for VNC bridge");
          });
          rfb.addEventListener("credentialsrequired", (event) => {
            const types = event.detail?.types || ["password"];
            const values = {};
            if (types.includes("username")) {
              if (!username) {
                stopPolling(missingVNCCredentialMessage);
                return;
              }
              values.username = username;
            }
            if (types.includes("password")) {
              if (!password) {
                stopPolling(missingVNCCredentialMessage);
                return;
              }
              values.password = password;
            }
            credentialsSent = true;
            rfb.sendCredentials(values);
          });
          rfb.addEventListener("securityfailure", () => {
            authenticationFailed = true;
            stopPolling(failedVNCCredentialMessage);
          });
        } catch (error) {
          scheduleRetry(error instanceof Error ? error.message : String(error));
        }
      }
      window.addEventListener("beforeunload", () => {
        stopped = true;
        window.clearTimeout(retryTimer);
        window.clearInterval(statusTimer);
        window.clearTimeout(desktopThemeTimer);
        rfb?.disconnect();
      });
      const takeoverBtn = document.getElementById("vnc-takeover");
      takeoverBtn?.addEventListener("click", async () => {
        try {
          await takeControl();
        } catch (error) {
          setStatus(error instanceof Error ? error.message : String(error), "bad");
        }
      });
      const reconnectBtn = document.getElementById("vnc-reconnect");
      reconnectBtn?.addEventListener("click", () => {
        window.clearTimeout(retryTimer);
        retryAttempt = 0;
        stopped = false;
        try { rfb?.disconnect(); } catch (_) {}
        connect();
      });
      const fullscreenBtn = document.getElementById("vnc-fullscreen");
      fullscreenBtn?.addEventListener("click", () => {
        if (document.fullscreenElement) {
          document.exitFullscreen();
        } else {
          document.documentElement.requestFullscreen?.().catch(() => {});
        }
      });
      const shareBtn = document.getElementById("vnc-share");
      const shareDialog = document.getElementById("vnc-share-dialog");
      const shareCloseBtn = document.getElementById("vnc-share-close");
      const shareDoneBtn = document.getElementById("vnc-share-done");
      const shareAddBtn = document.getElementById("vnc-share-add");
      const shareUserInput = document.getElementById("vnc-share-user");
      const shareRoleSelect = document.getElementById("vnc-share-role");
      const shareOrgSelect = document.getElementById("vnc-share-org");
      const sharePeople = document.getElementById("vnc-share-people");
      const shareStatus = document.getElementById("vnc-share-status");
      const shareCopyLinkBtn = document.getElementById("vnc-share-copy-link");
      const shareClearBtn = document.getElementById("vnc-share-clear");
      const shareControls = [shareAddBtn, shareUserInput, shareRoleSelect, shareOrgSelect, shareCopyLinkBtn, shareClearBtn].filter(Boolean);
      let shareState = {
        users: { ...(shareInitial.share?.users || {}) },
        org: shareInitial.share?.org || "",
      };
      let shareStatusTimer;
      function shareRoleLabel(role) {
        return role === "manage" ? "Can manage" : "Can use";
      }
      function normalizedShareUser(value) {
        return String(value || "").trim().toLowerCase();
      }
      async function shareableWebVNCURL() {
        if (!username && !password) throw new Error(missingVNCCredentialMessage);
        const response = await fetch(handoffURL, {
          method: "POST",
          headers: { "content-type": "application/json", accept: "application/json" },
          body: JSON.stringify({ username, password }),
        });
        const body = await response.json().catch(() => ({}));
        if (!response.ok) throw new Error(body.message || body.error || "VNC handoff failed");
        const url = new URL(window.location.href);
        const linkFragment = new URLSearchParams();
        linkFragment.set("handoff", body.ticket);
        url.hash = linkFragment.toString();
        return url.toString();
      }
      function setShareBusy(busy) {
        for (const control of shareControls) {
          control.disabled = busy;
        }
      }
      function setShareStatus(message, tone = "") {
        if (!shareStatus) return;
        shareStatus.textContent = message;
        shareStatus.dataset.tone = tone;
        window.clearTimeout(shareStatusTimer);
        if (message && tone !== "bad") {
          shareStatusTimer = window.setTimeout(() => setShareStatus(""), 1800);
        }
      }
      function sharePayload() {
        const payload = {};
        const users = {};
        for (const [user, role] of Object.entries(shareState.users || {}).sort(([a], [b]) => a.localeCompare(b))) {
          if (user && (role === "use" || role === "manage")) users[user] = role;
        }
        if (Object.keys(users).length) payload.users = users;
        if (shareState.org === "use" || shareState.org === "manage") payload.org = shareState.org;
        return payload;
      }
      function shareStateFromResponse(body) {
        return {
          users: { ...(body.share?.users || {}) },
          org: body.share?.org || "",
        };
      }
      async function refreshShareState() {
        const response = await fetch(shareAPIURL, { headers: { accept: "application/json" } });
        const body = await response.json().catch(() => ({}));
        if (!response.ok) throw new Error(body.message || body.error || "share refresh failed");
        shareState = shareStateFromResponse(body);
        renderSharePeople();
        return shareState;
      }
      function renderSharePeople() {
        if (!sharePeople) return;
        sharePeople.replaceChildren();
        const ownerRow = document.createElement("div");
        ownerRow.className = "vnc-share-person";
        ownerRow.innerHTML = '<div class="vnc-share-avatar" aria-hidden="true">you</div><div class="vnc-share-person-main"><strong></strong><span></span></div><span class="vnc-share-role-label">Owner</span>';
        ownerRow.querySelector("strong").textContent = shareInitial.owner || "Owner";
        ownerRow.querySelector("span").textContent = "Lease owner";
        sharePeople.appendChild(ownerRow);
        const users = Object.entries(shareState.users || {}).sort(([a], [b]) => a.localeCompare(b));
        if (!users.length) {
          const empty = document.createElement("p");
          empty.className = "vnc-share-empty";
          empty.textContent = "No shared users yet";
          sharePeople.appendChild(empty);
        }
        for (const [user, role] of users) {
          const row = document.createElement("div");
          row.className = "vnc-share-person";
          const avatar = document.createElement("div");
          avatar.className = "vnc-share-avatar";
          avatar.setAttribute("aria-hidden", "true");
          avatar.textContent = user.slice(0, 2) || "u";
          const main = document.createElement("div");
          main.className = "vnc-share-person-main";
          const strong = document.createElement("strong");
          strong.textContent = user;
          const small = document.createElement("span");
          small.textContent = shareRoleLabel(role);
          main.append(strong, small);
          const remove = document.createElement("button");
          remove.className = "icon-btn mini";
          remove.type = "button";
          remove.title = "remove " + user;
          remove.setAttribute("aria-label", "remove " + user);
          remove.dataset.removeUser = user;
          remove.textContent = "×";
          row.append(avatar, main, remove);
          sharePeople.appendChild(row);
        }
        if (shareOrgSelect) shareOrgSelect.value = shareState.org || "";
        const orgSummary = document.getElementById("vnc-share-org-summary");
        if (orgSummary) {
          orgSummary.textContent = shareState.org
            ? shareInitial.org + " members " + shareRoleLabel(shareState.org).toLowerCase()
            : "Only explicitly shared users can access";
        }
      }
      async function saveShare(updateShareState, message) {
        setShareBusy(true);
        setShareStatus("saving");
        const previousState = shareState;
        try {
          await refreshShareState();
          shareState = updateShareState(shareState);
          const response = await fetch(shareAPIURL, {
            method: "POST",
            headers: { "content-type": "application/json", accept: "application/json" },
            body: JSON.stringify(sharePayload()),
          });
          const body = await response.json().catch(() => ({}));
          if (!response.ok) throw new Error(body.message || body.error || "share update failed");
          shareState = shareStateFromResponse(body);
          renderSharePeople();
          setShareStatus(message || "saved", "ok");
          return true;
        } catch (error) {
          shareState = previousState;
          renderSharePeople();
          setShareStatus(error instanceof Error ? error.message : String(error), "bad");
          return false;
        } finally {
          setShareBusy(false);
        }
      }
      shareBtn?.addEventListener("click", () => {
        renderSharePeople();
        if (shareDialog?.showModal) {
          shareDialog.showModal();
        } else {
          window.location.href = sharePageURL.toString();
        }
        setShareBusy(true);
        setShareStatus("refreshing");
        void refreshShareState()
          .then(() => setShareStatus(""))
          .catch((error) => setShareStatus(error instanceof Error ? error.message : String(error), "bad"))
          .finally(() => setShareBusy(false));
      });
      shareCloseBtn?.addEventListener("click", () => shareDialog?.close());
      shareDoneBtn?.addEventListener("click", () => shareDialog?.close());
      shareDialog?.addEventListener("click", (event) => {
        if (event.target === shareDialog) shareDialog.close();
      });
      shareAddBtn?.addEventListener("click", () => {
        const user = normalizedShareUser(shareUserInput?.value);
        if (!user) {
          setShareStatus("enter an email address", "bad");
          shareUserInput?.focus();
          return;
        }
        const role = shareRoleSelect?.value === "manage" ? "manage" : "use";
        void saveShare((state) => ({ users: { ...state.users, [user]: role }, org: state.org }), "shared with " + user).then((saved) => {
          if (saved && shareUserInput) shareUserInput.value = "";
        });
      });
      shareUserInput?.addEventListener("keydown", (event) => {
        if (event.key === "Enter") {
          event.preventDefault();
          shareAddBtn?.click();
        }
      });
      shareOrgSelect?.addEventListener("change", () => {
        const role = shareOrgSelect.value === "manage" || shareOrgSelect.value === "use" ? shareOrgSelect.value : "";
        void saveShare((state) => ({ users: { ...state.users }, org: role }), role ? "org access updated" : "org access off");
      });
      sharePeople?.addEventListener("click", (event) => {
        const target = event.target;
        if (!(target instanceof HTMLElement)) return;
        const user = target.dataset.removeUser;
        if (!user) return;
        void saveShare(
          (state) => {
            const users = { ...state.users };
            delete users[user];
            return { users, org: state.org };
          },
          "removed " + user,
        );
      });
      shareCopyLinkBtn?.addEventListener("click", async () => {
        try {
          await writeClipboardText(await shareableWebVNCURL());
          setShareStatus("WebVNC link copied", "ok");
        } catch (error) {
          setShareStatus(error instanceof Error ? error.message : String(error), "bad");
        }
      });
      shareClearBtn?.addEventListener("click", () => {
        void saveShare(() => ({ users: {}, org: "" }), "sharing cleared");
      });
      async function readClipboardText() {
        if (navigator.clipboard?.readText) {
          try {
            return await navigator.clipboard.readText();
          } catch (_) {}
        }
        const text = await window.crabboxDialog?.prompt(
          "Clipboard access is unavailable. Enter the text to send to the remote desktop.",
          {
            title: "Paste text",
            label: "Text to paste",
            confirmLabel: "paste",
          },
        );
        return text || "";
      }
      function pasteModifier() {
        return target === "macos"
          ? { keysym: 0xffeb, code: "MetaLeft" }
          : { keysym: 0xffe3, code: "ControlLeft" };
      }
      function sendPasteShortcut() {
        if (!rfb?.sendKey) return;
        const modifier = pasteModifier();
        rfb.sendKey(modifier.keysym, modifier.code, true);
        rfb.sendKey(0x0076, "KeyV");
        rfb.sendKey(modifier.keysym, modifier.code, false);
      }
      const pasteBtn = document.getElementById("vnc-paste");
      let pasteResetTimer;
      pasteBtn?.addEventListener("click", async () => {
        if (!rfb || !connected) {
          setStatus("connect before paste", "warn");
          return;
        }
        if (!isController) {
          setStatus(controllerLabel ? controllerLabel + " is controlling" : "observer mode", "warn");
          return;
        }
        const text = await readClipboardText();
        if (!text) return;
        try {
          rfb.clipboardPasteFrom(text);
          window.setTimeout(sendPasteShortcut, 80);
          pasteBtn.dataset.state = "ok";
          setStatus("pasted clipboard", "ok");
        } catch (error) {
          setStatus(error instanceof Error ? error.message : String(error), "bad");
        }
        window.clearTimeout(pasteResetTimer);
        pasteResetTimer = window.setTimeout(() => { delete pasteBtn.dataset.state; }, 1200);
      });
      const copyRemoteBtn = document.getElementById("vnc-copy-remote");
      let copyRemoteResetTimer;
      copyRemoteBtn?.addEventListener("click", async () => {
        if (!remoteClipboardText) {
          setStatus("no remote clipboard yet", "warn");
          return;
        }
        try {
          await writeClipboardText(remoteClipboardText);
          copyRemoteBtn.dataset.state = "ok";
          setStatus("copied remote clipboard", "ok");
        } catch (error) {
          setStatus(error instanceof Error ? error.message : String(error), "bad");
        }
        window.clearTimeout(copyRemoteResetTimer);
        copyRemoteResetTimer = window.setTimeout(() => { delete copyRemoteBtn.dataset.state; }, 1200);
      });
      const copyBtn = document.getElementById("vnc-copy");
      const cmdEl = document.getElementById("vnc-bridge-cmd");
      let copyResetTimer;
      copyBtn?.addEventListener("click", async () => {
        const text = cmdEl?.textContent || "";
        try {
          await writeClipboardText(text);
        } catch (_) {
          const range = document.createRange();
          if (cmdEl) {
            range.selectNodeContents(cmdEl);
            const sel = window.getSelection();
            sel?.removeAllRanges();
            sel?.addRange(range);
          }
        }
        copyBtn.dataset.state = "ok";
        window.clearTimeout(copyResetTimer);
        copyResetTimer = window.setTimeout(() => { delete copyBtn.dataset.state; }, 1200);
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

export function portalCode(lease: LeaseRecord): Response {
  const nonce = scriptNonce();
  const slug = lease.slug || lease.id;
  const target = lease.target || "linux";
  const bridgeCmd = codeBridgeCommand(lease);
  const statusPath = `/portal/leases/${encodeURIComponent(lease.id)}/code/health`;
  const reloadIcon = `<svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M21 12a9 9 0 1 1-3-6.7"/><path d="M21 4v5h-5"/></svg>`;
  return html(
    `Code ${slug}`,
    `<main class="vnc-page code-wait-page">
      ${portalHeader({
        variant: "bar",
        meta: `<span>code ${escapeHTML(slug)}</span><span class="vnc-dot"></span>${providerBadge(lease.provider)}<span class="vnc-dot"></span>${targetBadge(target, lease.windowsMode)}<span class="vnc-dot"></span><span>code workspace</span><span class="vnc-dot"></span><span class="vnc-id">${escapeHTML(lease.id)}</span>`,
        actions: `
          <span id="code-status" class="status-pill">checking bridge</span>
          <button id="code-reload" class="icon-btn" type="button" title="reload" aria-label="reload">${reloadIcon}</button>
          <a class="button secondary" href="/portal">leases</a>
          <a class="button secondary" href="/portal/logout">log out</a>
        `,
      })}
      <section class="screen code-wait-screen" aria-label="Code bridge waiting state">
        <div class="code-wait-card">
          <span class="code-wait-kicker">code bridge</span>
          <h2>waiting for local bridge</h2>
          <p id="code-hint">Run the command below. This page will open VS Code when the bridge connects.</p>
        </div>
      </section>
      <footer class="vnc-bridge">
        <span class="vnc-bridge-label">bridge</span>
        <code id="code-bridge-cmd" class="vnc-bridge-cmd">${escapeHTML(bridgeCmd)}</code>
        <button id="code-copy" class="icon-btn" type="button" title="copy command" aria-label="copy bridge command">${copyIcon}</button>
      </footer>
      <script nonce="${nonce}">
        const status = document.getElementById("code-status");
        const hint = document.getElementById("code-hint");
        const statusURL = new URL(${JSON.stringify(statusPath)}, window.location.href);
        let pollTimer;
        let stopped = false;
        const terminalStatusCodes = new Set([403, 404, 409, 410]);
        function setStatus(value, tone = "") {
          status.textContent = value;
          status.dataset.tone = tone;
        }
        async function responseMessage(response, fallback) {
          try {
            const body = await response.json();
            return body.message || body.error || fallback;
          } catch (_) {
            return fallback;
          }
        }
        function stopPolling(message) {
          stopped = true;
          window.clearTimeout(pollTimer);
          setStatus("bridge unavailable", "bad");
          hint.textContent = message || "This lease is no longer available. Open a current lease from the portal.";
        }
        async function pollBridge() {
          if (stopped) return;
          window.clearTimeout(pollTimer);
          try {
            const response = await fetch(statusURL, { cache: "no-store" });
            if (!response.ok) {
              const message = await responseMessage(response, "Code bridge status unavailable");
              if (terminalStatusCodes.has(response.status)) {
                stopPolling(message);
                return;
              }
              throw new Error(message);
            }
            const state = await response.json();
            if (state?.code?.agentConnected) {
              setStatus("bridge connected; opening", "ok");
              hint.textContent = "Bridge connected. Opening workspace...";
              window.setTimeout(() => window.location.reload(), 250);
              return;
            }
            setStatus("waiting for bridge", "warn");
            hint.textContent = "Start the local bridge below. This page checks again automatically.";
          } catch (error) {
            setStatus("status unavailable", "bad");
            hint.textContent = "Could not read bridge status. Reload or use the command below.";
          }
          if (!stopped) {
            pollTimer = window.setTimeout(pollBridge, 2000);
          }
        }
        document.getElementById("code-reload")?.addEventListener("click", () => {
          window.location.reload();
        });
        const copyBtn = document.getElementById("code-copy");
        const cmdEl = document.getElementById("code-bridge-cmd");
        let copyResetTimer;
        copyBtn?.addEventListener("click", async () => {
          const text = cmdEl?.textContent || "";
          try {
            await navigator.clipboard.writeText(text);
          } catch (_) {
            const range = document.createRange();
            if (cmdEl) {
              range.selectNodeContents(cmdEl);
              const sel = window.getSelection();
              sel?.removeAllRanges();
              sel?.addRange(range);
            }
          }
          copyBtn.dataset.state = "ok";
          window.clearTimeout(copyResetTimer);
          copyResetTimer = window.setTimeout(() => { delete copyBtn.dataset.state; }, 1200);
        });
        pollBridge();
        window.addEventListener("beforeunload", () => window.clearTimeout(pollTimer));
      </script>
    </main>`,
    200,
    nonce,
  );
}

export function portalCodeBootstrapHandoff(action: URL, ticket: string): Response {
  const nonce = scriptNonce();
  const actionURL = action.toString();
  const contentSecurityPolicy = [
    "default-src 'none'",
    "base-uri 'none'",
    `form-action ${action.origin}`,
    "frame-ancestors 'none'",
    `script-src 'nonce-${nonce}'`,
    `style-src 'nonce-${nonce}'`,
  ].join("; ");
  return new Response(
    `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <title>Opening Code</title>
  <style nonce="${nonce}">
    :root { color-scheme: dark light; font: 16px/1.5 system-ui,sans-serif; }
    body { min-height: 100vh; margin: 0; display: grid; place-items: center; }
    form { text-align: center; }
    button { font: inherit; padding: .55rem 1rem; }
  </style>
</head>
<body>
  <form id="code-bootstrap" method="post" action="${escapeHTML(actionURL)}" autocomplete="off">
    <input type="hidden" name="ticket" value="${escapeHTML(ticket)}">
    <p>Opening Code…</p>
    <button type="submit">Continue</button>
  </form>
  <script nonce="${nonce}">document.getElementById("code-bootstrap").requestSubmit();</script>
</body>
</html>`,
    {
      headers: {
        "cache-control": "no-store",
        "content-security-policy": contentSecurityPolicy,
        "content-type": "text/html; charset=utf-8",
        "referrer-policy": "no-referrer",
        "x-content-type-options": "nosniff",
      },
    },
  );
}

export function codeBridgeCommand(lease: LeaseRecord): string {
  return ["crabbox", "code", "--id", lease.slug || lease.id, "--open"].map(shellArg).join(" ");
}

export function webVNCBridgeCommand(lease: LeaseRecord): string {
  const target = lease.target || "linux";
  const args = [
    "crabbox",
    "webvnc",
    "--provider",
    lease.provider,
    "--target",
    target,
    "--id",
    lease.slug || lease.id,
  ];
  if (target === "windows" && lease.windowsMode && lease.windowsMode !== "normal") {
    args.push("--windows-mode", lease.windowsMode);
  }
  args.push("--open");
  return args.map(shellArg).join(" ");
}

function shellArg(value: string): string {
  if (/^[A-Za-z0-9_./:@=-]+$/.test(value)) {
    return value;
  }
  return `'${value.replaceAll("'", "'\"'\"'")}'`;
}

function leaseRow(
  lease: LeaseRecord,
  context: { admin: boolean; owner: string; org: string; canManage: boolean },
): string {
  const label = lease.slug || lease.id;
  const detailPath = `/portal/leases/${encodeURIComponent(lease.id)}`;
  const active = lease.state === "active";
  const filterValue = active ? "active" : "ended";
  const target = lease.target || "linux";
  const ownership = context.admin ? leaseOwnership(lease, context.owner, context.org) : "mine";
  const subline =
    context.admin && ownership === "system"
      ? `${lease.id} · ${lease.owner || "unknown"}`
      : lease.id;
  const timeLabel = active
    ? lease.lastTouchedAt || lease.updatedAt || lease.createdAt
    : lease.endedAt || lease.releasedAt || lease.updatedAt;
  const groupTags = portalRowFilterGroupTags({
    kind: "lease",
    owner: ownership,
    provider: lease.provider,
    state: filterValue,
    target,
  });
  return `<tr data-filter-tags="${escapeHTML(["lease", filterValue, ownership, lease.provider, target].join(" "))}" data-filter-group-tags="${escapeHTML(groupTags)}">
    <td><a class="lease-link" href="${detailPath}"><strong>${escapeHTML(label)}</strong><small>${escapeHTML(subline)}</small></a></td>
    <td><span class="pill" data-state="${escapeHTML(lease.state)}">${escapeHTML(lease.state)}</span></td>
    <td>${providerBadge(lease.provider)}</td>
    <td>${targetBadge(target, lease.windowsMode)}</td>
    <td>${escapeHTML(lease.class)}</td>
    <td>${accessCell(lease, detailPath)}</td>
    ${timeCell(timeLabel)}
    <td>${active && context.canManage ? leaseReleaseAction(lease) : ""}</td>
  </tr>`;
}

function leaseReleaseAction(lease: LeaseRecord): string {
  const registered = lease.lifecycle === "registered";
  const adapterManaged = Boolean(lease.runtimeAdapterID && lease.runtimeAdapterWorkspaceID);
  const label = lease.slug || lease.id;
  const actionLabel = registered
    ? adapterManaged
      ? `Delete ${label} workspace`
      : `Remove ${label} registration`
    : `Stop ${label}`;
  return `<form class="lease-release-form" method="post" action="/portal/leases/${encodeURIComponent(lease.id)}/release?return=%2Fportal" data-confirm="${escapeHTML(leaseReleaseConfirmation(lease))}">
    <button class="access-icon lease-release" data-release-kind="${registered ? (adapterManaged ? "adapter" : "registered") : "managed"}" type="submit" title="${escapeHTML(actionLabel)}" aria-label="${escapeHTML(actionLabel)}">${registered && !adapterManaged ? ejectIcon : powerIcon}</button>
  </form>`;
}

function leaseReleaseConfirmation(lease: {
  id: string;
  slug?: string;
  lifecycle?: LeaseRecord["lifecycle"];
  runtimeAdapterID?: string;
  runtimeAdapterWorkspaceID?: string;
}): string {
  const label = lease.slug || lease.id;
  if (lease.lifecycle !== "registered") {
    return `Stop ${label}? This deletes the backing machine.`;
  }
  return lease.runtimeAdapterID && lease.runtimeAdapterWorkspaceID
    ? `Delete ${label}? This permanently deletes the external workspace through its runtime adapter.`
    : `Remove ${label} from Crabbox? The external machine will keep running. Use crabbox stop locally to shut it down.`;
}

function portalRowFilterGroupTags(groups: Record<string, string | string[] | undefined>): string {
  return Object.entries(groups)
    .flatMap(([group, rawValues]) => {
      const values = Array.isArray(rawValues) ? rawValues : [rawValues];
      return values
        .filter((value): value is string => Boolean(value))
        .map((value) => `${group}:${value}`);
    })
    .join(" ");
}

type PortalHomeFilterGroup = {
  key: string;
  label: string;
  defaultValue: string;
  options: Array<{ value: string; label: string }>;
};

function portalHomeFilterGroups(
  leases: LeaseRecord[],
  runners: ExternalRunnerRecord[],
  macHosts: PortalMacHostRecord[],
  options: { admin: boolean; defaultState: string },
): string {
  const providerValues = orderedFilterValues(
    [
      ...leases.map((lease) => lease.provider),
      ...runners.map((runner) => runner.provider),
      ...macHosts.map((host) => host.provider),
    ],
    ["aws", "azure", "hetzner", "blacksmith-testbox"],
  );
  const targetValues = orderedFilterValues(
    [
      ...leases.map((lease) => lease.target || "linux"),
      ...macHosts.map((host) => host.target || "macos"),
    ],
    ["linux", "macos", "windows"],
  );
  const groups: PortalHomeFilterGroup[] = [
    {
      key: "state",
      label: "state",
      defaultValue: options.defaultState,
      options: [
        { value: "all", label: "any state" },
        { value: "active", label: "active" },
        { value: "ended", label: "ended" },
        { value: "stale", label: "stale" },
        { value: "stuck", label: "stuck" },
      ],
    },
    {
      key: "provider",
      label: "provider",
      defaultValue: "all",
      options: [
        { value: "all", label: "any provider" },
        ...providerValues.map((value) => ({ value, label: providerFilterLabel(value) })),
      ],
    },
    {
      key: "target",
      label: "os",
      defaultValue: "all",
      options: [
        { value: "all", label: "any os" },
        ...targetValues.map((value) => ({ value, label: targetFilterLabel(value) })),
      ],
    },
    {
      key: "kind",
      label: "kind",
      defaultValue: "all",
      options: [
        { value: "all", label: "any kind" },
        { value: "lease", label: "lease" },
        { value: "external", label: "external" },
        { value: "dedicated", label: "dedicated" },
      ],
    },
  ];
  if (options.admin) {
    groups.push({
      key: "owner",
      label: "owner",
      defaultValue: "all",
      options: [
        { value: "all", label: "any owner" },
        { value: "mine", label: "mine" },
        { value: "system", label: "system" },
      ],
    });
  }
  return groups
    .map((group) =>
      [
        group.key,
        group.label,
        group.defaultValue,
        group.options.map((option) => `${option.value}:${option.label}`).join(","),
      ].join("|"),
    )
    .join(";");
}

function orderedFilterValues(values: Array<string | undefined>, preferred: string[]): string[] {
  const seen = new Set(values.filter((value): value is string => Boolean(value)));
  const preferredSet = new Set(preferred);
  const extras = Array.from(seen)
    .filter((value) => !preferredSet.has(value))
    .toSorted((a, b) => a.localeCompare(b));
  return [...preferred, ...extras];
}

function providerFilterLabel(value: string): string {
  return value === "blacksmith-testbox" ? "blacksmith" : value;
}

function targetFilterLabel(value: string): string {
  if (value === "macos") {
    return "macOS";
  }
  if (value === "windows") {
    return "Windows";
  }
  if (value === "linux") {
    return "Linux";
  }
  return value;
}

function macHostRow(
  host: PortalMacHostRecord,
  context: { admin: boolean; owner: string; org: string },
): string {
  const stateTone = macHostStateTone(host.state);
  const lease = host.lease;
  const activeLease = lease?.state === "active" ? lease : undefined;
  const detailPath = `/portal/hosts/${encodeURIComponent(host.provider)}/${encodeURIComponent(host.id)}`;
  const ownership =
    activeLease && context.admin ? leaseOwnership(activeLease, context.owner, context.org) : "mine";
  const tags = [
    host.state === "released" ? "ended" : "active",
    ownership,
    "dedicated",
    "host",
    host.provider,
    host.target,
    host.state,
    host.instanceType,
    host.region,
    host.availabilityZone,
    activeLease ? "attached" : undefined,
  ];
  const filterValue = host.state === "released" ? "ended" : "active";
  const groupTags = portalRowFilterGroupTags({
    kind: "dedicated",
    owner: ownership,
    provider: host.provider,
    state: filterValue,
    target: host.target,
  });
  const leaseMeta = activeLease
    ? `lease ${activeLease.slug || activeLease.id}`
    : [host.region, host.availabilityZone].filter(Boolean).join(" · ");
  return `<tr class="capacity-row" data-filter-tags="${escapeHTML(tags.filter(Boolean).join(" "))}" data-filter-group-tags="${escapeHTML(groupTags)}">
    <td><a class="lease-link dedicated-link" href="${detailPath}"><span class="dedicated-mark" title="dedicated host" aria-label="dedicated host">${dedicatedHostIcon}</span><span><strong>${escapeHTML(shortHostID(host.id))}</strong><small>${escapeHTML(leaseMeta || host.id)}</small></span></a></td>
    <td><span class="pill" data-tone="${stateTone}">${escapeHTML(host.state || "-")}</span></td>
    <td>${providerBadge(host.provider)}</td>
    <td>${targetBadge(host.target)}</td>
    <td><span title="${escapeHTML([host.availabilityZone, host.autoPlacement].filter(Boolean).join(" · ") || "Dedicated Host")}">${escapeHTML(host.instanceType || "dedicated")}</span></td>
    <td>${macHostAccessCell(host, detailPath)}</td>
    ${timeCell(host.allocationTime)}
    <td></td>
  </tr>`;
}

function macHostAccessCell(host: PortalMacHostRecord, detailPath: string): string {
  const activeLease = host.lease?.state === "active" ? host.lease : undefined;
  const pieces = [
    `<a class="access-icon" href="${detailPath}" title="dedicated host" aria-label="dedicated host">${dedicatedHostIcon}</a>`,
  ];
  if (activeLease?.code) {
    pieces.push(
      `<a class="access-icon" data-access="vscode" href="${detailPath}/code/" title="VS Code" aria-label="open VS Code">${codeIcon}</a>`,
    );
  }
  const vncTitle = activeLease?.desktop ? "VNC" : activeLease ? "enable VNC" : "start locally";
  pieces.push(
    `<a class="access-icon" data-access="vnc" href="${macHostVNCPath(host)}" title="${vncTitle}" aria-label="${vncTitle}">${vncIcon}</a>`,
  );
  return `<div class="access-cell">${pieces.join("")}</div>`;
}

function macHostVNCPath(host: PortalMacHostRecord): string {
  return `/portal/hosts/${encodeURIComponent(host.provider)}/${encodeURIComponent(host.id)}/vnc`;
}

function macHostStartDesktopCommand(host: PortalMacHostRecord): string {
  return [
    `CRABBOX_HOST_ID=${shellArg(host.id)}`,
    "crabbox warmup",
    `--provider ${shellArg(host.provider)}`,
    `--target ${shellArg(host.target || "macos")}`,
    "--market on-demand",
    host.instanceType ? `--type ${shellArg(host.instanceType)}` : "",
    "--desktop",
  ]
    .filter(Boolean)
    .join(" ");
}

function shortHostID(value: string): string {
  if (value.length <= 12) {
    return value;
  }
  return `${value.slice(0, 6)}…${value.slice(-4)}`;
}

function macHostStateTone(value: string): "ok" | "warn" | "bad" {
  switch (value) {
    case "available":
      return "ok";
    case "pending":
      return "warn";
    default:
      return "bad";
  }
}

function externalRunnerLeaseRow(
  runner: ExternalRunnerRecord,
  context: { admin: boolean; owner: string; org: string },
): string {
  const ownership = context.admin ? runnerOwnership(runner, context.owner, context.org) : "mine";
  const state = runner.stale ? "stale" : "active";
  const subline =
    context.admin && ownership === "system"
      ? `${runner.id} · ${runner.owner || "unknown"}`
      : [runner.repo, runner.workflow].filter(Boolean).join(" · ") || runner.id;
  const jobRef = [runner.job, runner.ref].filter(Boolean).join(" / ") || "-";
  const actionState = externalRunnerActionState(runner);
  const actionsLinks = externalRunnerActionsLinks(runner);
  const detailPath = externalRunnerDetailPath(runner, context);
  const filterTags = [
    state,
    actionState?.stuck ? "stuck" : undefined,
    actionState ? "actions" : undefined,
    ownership,
    "external",
    runner.provider,
    runner.status,
    runner.actionsRunStatus,
    runner.actionsRunConclusion,
    runner.repo,
    runner.workflow,
    runner.job,
    runner.ref,
  ];
  const groupTags = portalRowFilterGroupTags({
    kind: "external",
    owner: ownership,
    provider: runner.provider,
    state: actionState?.stuck ? [state, "stuck"] : state,
  });
  return `<tr class="external-row" data-filter-tags="${escapeHTML(filterTags.filter(Boolean).join(" "))}" data-filter-group-tags="${escapeHTML(groupTags)}">
    <td><a class="lease-link" href="${escapeHTML(detailPath)}"><strong>${escapeHTML(runner.id)}</strong><small>${escapeHTML(subline)}</small></a></td>
    <td><div class="state-stack"><span class="pill" data-tone="${runner.stale ? "warn" : runnerStatusTone(runner.status)}">${escapeHTML(runner.status || "-")}</span>${externalRunnerActionBadge(actionState)}</div></td>
    <td>${providerBadge(runner.provider)}</td>
    <td><span class="muted" title="Blacksmith owns runner host details">-</span></td>
    <td><span title="${escapeHTML([runner.repo, runner.workflow, jobRef].filter(Boolean).join(" · "))}">${externalRunnerActionsCell(runner, actionsLinks)}</span></td>
    <td>${externalRunnerAccessCell(runner, actionsLinks.length > 0)}</td>
    ${timeCell(runnerSortTime(runner))}
    <td></td>
  </tr>`;
}

function externalRunnerDetailPath(
  runner: ExternalRunnerRecord,
  context?: { admin: boolean; owner: string; org: string },
): string {
  const path = `/portal/runners/${encodeURIComponent(runner.provider)}/${encodeURIComponent(runner.id)}`;
  if (!context?.admin) {
    return path;
  }
  const params = new URLSearchParams();
  params.set("owner", runner.owner);
  params.set("org", runner.org);
  return `${path}?${params.toString()}`;
}

type ExternalRunnerActionState = {
  label: string;
  title: string;
  tone: string;
  stuck: boolean;
};

function externalRunnerActionState(
  runner: ExternalRunnerRecord,
): ExternalRunnerActionState | undefined {
  const status = runner.actionsRunStatus;
  const conclusion = runner.actionsRunConclusion;
  if (!status && !conclusion) {
    return undefined;
  }
  const ageMs = runner.createdAt ? Date.now() - Date.parse(runner.createdAt) : undefined;
  const validAge = ageMs !== undefined && Number.isFinite(ageMs) && ageMs >= 0;
  const lowerStatus = status?.toLowerCase();
  const lowerConclusion = conclusion?.toLowerCase();
  const queuedTooLong = lowerStatus === "queued" && validAge && ageMs > 20 * 60 * 1000;
  const runningTooLong =
    (lowerStatus === "in_progress" || lowerStatus === "running") &&
    validAge &&
    ageMs > 90 * 60 * 1000;
  const stuck = Boolean(!conclusion && (queuedTooLong || runningTooLong));
  const title = [
    runner.actionsRepo,
    runner.actionsRunID ? `run ${runner.actionsRunID}` : undefined,
    status,
    conclusion,
    runner.createdAt ? `created ${relativeTime(runner.createdAt)}` : undefined,
  ]
    .filter(Boolean)
    .join(" · ");
  if (stuck) {
    return {
      label: `stuck ${compactAge(runner.createdAt)}`,
      title,
      tone: "warn",
      stuck: true,
    };
  }
  const label = conclusion || status || "actions";
  return {
    label: `gha ${label.replaceAll("_", " ")}`,
    title,
    tone: externalRunnerActionTone(lowerStatus, lowerConclusion),
    stuck: false,
  };
}

function externalRunnerActionTone(
  status: string | undefined,
  conclusion: string | undefined,
): string {
  if (conclusion === "success") {
    return "ok";
  }
  if (conclusion === "failure" || conclusion === "timed_out") {
    return "bad";
  }
  if (conclusion === "cancelled" || conclusion === "skipped" || conclusion === "neutral") {
    return "warn";
  }
  if (status === "in_progress" || status === "running") {
    return "ok";
  }
  if (status === "queued" || status === "pending" || status === "waiting") {
    return "warn";
  }
  return "";
}

function externalRunnerActionBadge(state: ExternalRunnerActionState | undefined): string {
  if (!state) {
    return "";
  }
  return `<span class="pill action-pill" data-tone="${escapeHTML(state.tone)}" title="${escapeHTML(state.title)}">${escapeHTML(state.label)}</span>`;
}

function externalRunnerActionsCell(runner: ExternalRunnerRecord, actionsLinks: string): string {
  const label = runner.actionsWorkflowName || workflowBasename(runner.workflow) || "external";
  const meta = [runner.job, runner.ref].filter(Boolean).join(" / ");
  return `<div class="actions-stack">${actionsLinks || `<span class="muted">${escapeHTML(label)}</span>`}<small>${escapeHTML(meta || label)}</small></div>`;
}

function externalRunnerAccessCell(runner: ExternalRunnerRecord, hasActions: boolean): string {
  const label = hasActions ? "no box access" : "no access";
  const command = externalRunnerStopCommand(runner);
  const copy = command
    ? `<button class="icon-btn mini" type="button" title="copy stop command" aria-label="copy stop command" data-copy-value="${escapeHTML(command)}">${copyIcon}</button>`
    : "";
  return `<div class="access-cell disabled-cell external-access" title="external runner; no Crabbox access data"><span>${label}</span>${copy}</div>`;
}

function externalRunnerStopCommand(runner: ExternalRunnerRecord): string {
  return runner.provider && runner.id
    ? `crabbox stop --provider ${shellArg(runner.provider)} ${shellArg(runner.id)}`
    : "";
}

function externalRunnerActionsLinks(runner: ExternalRunnerRecord): string {
  const links: string[] = [];
  if (runner.actionsRunURL) {
    const status = [runner.actionsRunStatus, runner.actionsRunConclusion].filter(Boolean).join("/");
    links.push(
      `<a class="row-link" href="${escapeHTML(runner.actionsRunURL)}" target="_blank" rel="noopener" title="${escapeHTML(status || "GitHub Actions run")}">run</a>`,
    );
  }
  if (runner.actionsWorkflowURL) {
    links.push(
      `<a class="row-link secondary" href="${escapeHTML(runner.actionsWorkflowURL)}" target="_blank" rel="noopener" title="${escapeHTML(runner.actionsWorkflowName || runner.workflow || "GitHub Actions workflow")}">workflow</a>`,
    );
  }
  return links.length ? `<span class="row-links">${links.join("")}</span>` : "";
}

function workflowBasename(workflow: string | undefined): string | undefined {
  if (!workflow) {
    return undefined;
  }
  const parts = workflow.split("/");
  for (let index = parts.length - 1; index >= 0; index -= 1) {
    const part = parts[index];
    if (part) {
      return part;
    }
  }
  return workflow;
}

function portalHeader(options: PortalHeaderOptions): string {
  const variant = options.variant || "top";
  const headerClass = variant === "bar" ? "vnc-bar" : "top";
  const metaClass = variant === "bar" ? "vnc-meta portal-header-meta" : "portal-header-meta";
  const actions = `<div class="portal-actions">${themeToggleButton()}${options.actions?.trim() ?? ""}</div>`;
  return `<header class="${headerClass}">
    <div class="${metaClass}">
      <h1>${portalBrand}</h1>
      <p>${options.meta}</p>
    </div>
    ${actions}
  </header>`;
}

function leaseOwnership(lease: LeaseRecord, owner: string, org: string): "mine" | "system" {
  return lease.owner === owner && lease.org === org ? "mine" : "system";
}

function runnerOwnership(
  runner: ExternalRunnerRecord,
  owner: string,
  org: string,
): "mine" | "system" {
  return runner.owner === owner && runner.org === org ? "mine" : "system";
}

function runnerSortTime(runner: ExternalRunnerRecord): string {
  return runner.lastSeenAt || runner.updatedAt || runner.createdAt || runner.firstSeenAt;
}

function macHostSortTime(host: PortalMacHostRecord): string {
  return host.lease?.lastTouchedAt || host.lease?.updatedAt || host.allocationTime || "";
}

function runRow(run: RunRecord): string {
  const stateTone = run.state === "succeeded" ? "ok" : run.state === "failed" ? "bad" : "warn";
  const subtitle = run.label || run.command.join(" ");
  return `<tr data-filter-tags="${escapeHTML([run.state, run.provider, run.target || "linux"].filter(Boolean).join(" "))}">
    <td><a class="lease-link" href="/portal/runs/${encodeURIComponent(run.id)}"><strong>${escapeHTML(run.id)}</strong><small>${escapeHTML(subtitle)}</small></a></td>
    <td><span class="pill" data-tone="${stateTone}">${escapeHTML(run.state)}</span></td>
    ${elapsedTimeCell(run.startedAt)}
    <td>${escapeHTML(formatDuration(run.durationMs))}</td>
    <td>${escapeHTML(runTelemetryCell(run.telemetry))}</td>
    <td><div class="actions-cell"><a class="button secondary" href="/portal/runs/${encodeURIComponent(run.id)}/logs">logs</a><a class="button secondary" href="/portal/runs/${encodeURIComponent(run.id)}/events">events</a></div></td>
  </tr>`;
}

function eventRow(event: RunEventRecord): string {
  const eventGroup = event.type.split(".")[0] || event.type;
  return `<tr data-filter-tags="${escapeHTML([eventGroup, event.phase, event.stream].filter(Boolean).join(" "))}">
    <td>${event.seq}</td>
    <td><strong>${escapeHTML(event.type)}</strong><small>${escapeHTML(event.stream || "")}</small></td>
    <td>${escapeHTML(event.phase || "-")}</td>
    ${timeCell(event.createdAt)}
    <td>${escapeHTML(truncate(event.message || event.data || "", 220))}</td>
  </tr>`;
}

function accessCell(lease: LeaseRecord, detailPath: string): string {
  const active = lease.state === "active";
  const pieces = [
    `<a class="access-icon" href="${detailPath}" title="server" aria-label="server">${serverIcon}</a>`,
  ];
  if (active && lease.code) {
    pieces.push(
      `<a class="access-icon" data-access="vscode" href="/portal/leases/${encodeURIComponent(lease.id)}/code/" title="VS Code" aria-label="open VS Code">${codeIcon}</a>`,
    );
  }
  if (active && lease.desktop) {
    pieces.push(
      `<a class="access-icon" data-access="vnc" href="/portal/leases/${encodeURIComponent(lease.id)}/vnc" title="VNC" aria-label="open VNC">${vncIcon}</a>`,
    );
  }
  return `<div class="access-cell">${pieces.join("")}</div>`;
}

function timeCell(value: string | undefined): string {
  const date = value ? new Date(value) : undefined;
  const valid = !!date && !Number.isNaN(date.getTime());
  const time = valid ? date.getTime() : 0;
  const iso = valid ? date.toISOString().replace(".000Z", "Z") : value || "-";
  return `<td data-sort="${time}" title="${escapeHTML(iso)}"><time datetime="${escapeHTML(iso)}">${escapeHTML(relativeTime(value))}</time></td>`;
}

function elapsedTimeCell(value: string | undefined): string {
  const date = value ? new Date(value) : undefined;
  const valid = !!date && !Number.isNaN(date.getTime());
  const time = valid ? date.getTime() : 0;
  const iso = valid ? date.toISOString().replace(".000Z", "Z") : value || "-";
  const label = valid ? formatDuration(Date.now() - time) : "-";
  return `<td data-sort="${time}" title="${escapeHTML(iso)}"><time datetime="${escapeHTML(iso)}">${escapeHTML(label)}</time></td>`;
}

function metaRow(label: string, value: string | undefined): string {
  return `<div><dt>${escapeHTML(label)}</dt><dd>${escapeHTML(value || "-")}</dd></div>`;
}

function metaHTMLRow(label: string, value: string): string {
  return `<div><dt>${escapeHTML(label)}</dt><dd>${value}</dd></div>`;
}

function providerBadge(provider: string | undefined): string {
  const value = provider || "-";
  return `<span class="icon-label" data-provider="${escapeHTML(value)}">${providerIcon(value)}<span>${escapeHTML(value)}</span></span>`;
}

function targetBadge(target: string | undefined, windowsMode?: string): string {
  const value = target || "linux";
  const label =
    value === "windows"
      ? windowsMode && windowsMode !== "normal"
        ? `win (${windowsMode})`
        : "win"
      : value;
  return `<span class="icon-label" data-target="${escapeHTML(value)}">${targetIcon(value)}<span>${escapeHTML(label)}</span></span>`;
}

function leaseTelemetryRows(telemetry: LeaseRecord["telemetry"]): string {
  if (!telemetry) {
    return "";
  }
  return [
    metaRow("load", telemetryLoad(telemetry)),
    metaRow("cpu", telemetryCPUCount(telemetry.cpuCount)),
    metaRow(
      "memory",
      telemetryStorage(
        telemetry.memoryUsedBytes,
        telemetry.memoryTotalBytes,
        telemetry.memoryPercent,
      ),
    ),
    metaRow(
      "disk",
      telemetryStorage(telemetry.diskUsedBytes, telemetry.diskTotalBytes, telemetry.diskPercent),
    ),
    metaRow(
      "uptime",
      telemetry.uptimeSeconds !== undefined ? formatSeconds(telemetry.uptimeSeconds) : undefined,
    ),
    metaRow("seen", telemetry.capturedAt ? relativeTime(telemetry.capturedAt) : undefined),
  ].join("");
}

function leaseTelemetryTimeline(
  telemetry: LeaseRecord["telemetry"],
  history: LeaseRecord["telemetryHistory"],
): string {
  const samples = telemetrySamples(telemetry, history);
  if (!telemetry && samples.length === 0) {
    return "";
  }
  const health = telemetryHealthPills(telemetry);
  return `<div class="telemetry-strip">
    <div class="telemetry-strip-head">
      <span>box telemetry</span>
      <div>${health}</div>
    </div>
    ${telemetrySparkline(
      "load",
      samples.map((sample) => sample.load1),
      "load",
    )}
    ${telemetrySparkline(
      "memory",
      samples.map((sample) => sample.memoryPercent),
      "%",
    )}
    ${telemetrySparkline(
      "disk",
      samples.map((sample) => sample.diskPercent),
      "%",
    )}
  </div>`;
}

function telemetrySamples(
  telemetry: LeaseRecord["telemetry"],
  history: LeaseRecord["telemetryHistory"],
): LeaseTelemetrySample[] {
  const byTime = new Map<string, LeaseTelemetrySample>();
  for (const sample of Array.isArray(history) ? history : []) {
    if (sample?.capturedAt) {
      byTime.set(sample.capturedAt, sample);
    }
  }
  if (telemetry?.capturedAt) {
    byTime.set(telemetry.capturedAt, telemetry);
  }
  return [...byTime.values()].toSorted((left, right) =>
    left.capturedAt.localeCompare(right.capturedAt),
  );
}

type LeaseTelemetrySample = NonNullable<LeaseRecord["telemetry"]>;

function telemetryHealthPills(telemetry: LeaseRecord["telemetry"]): string {
  if (!telemetry?.capturedAt) {
    return `<span class="pill" data-tone="warn">no signal</span>`;
  }
  const pills = [];
  const ageMs = Date.now() - Date.parse(telemetry.capturedAt);
  if (!Number.isFinite(ageMs) || ageMs > 10 * 60 * 1000) {
    pills.push(
      `<span class="pill" data-tone="warn">stale ${escapeHTML(relativeTime(telemetry.capturedAt))}</span>`,
    );
  } else {
    pills.push(`<span class="pill" data-tone="ok">live</span>`);
  }
  if ((telemetry.memoryPercent ?? 0) >= 85) {
    pills.push(
      `<span class="pill" data-tone="bad">memory ${Math.round(telemetry.memoryPercent ?? 0)}%</span>`,
    );
  }
  if ((telemetry.diskPercent ?? 0) >= 85) {
    pills.push(
      `<span class="pill" data-tone="bad">disk ${Math.round(telemetry.diskPercent ?? 0)}%</span>`,
    );
  }
  if ((telemetry.load1 ?? 0) >= 16) {
    pills.push(`<span class="pill" data-tone="warn">load ${telemetry.load1?.toFixed(1)}</span>`);
  }
  return pills.join("");
}

function telemetrySparkline(
  label: string,
  rawValues: Array<number | undefined>,
  unit: string,
): string {
  const values = rawValues.filter((value): value is number => Number.isFinite(value));
  const latest = values.at(-1);
  if (values.length < 2 || latest === undefined) {
    return `<div class="telemetry-line"><span>${escapeHTML(label)}</span><span class="muted">waiting for samples</span></div>`;
  }
  const max = unit === "%" ? 100 : Math.max(1, ...values);
  const points = telemetryPolylinePoints(values, max);
  return `<div class="telemetry-line">
    <span>${escapeHTML(label)}</span>
    <svg class="telemetry-chart" viewBox="0 0 100 28" preserveAspectRatio="none" aria-label="${escapeHTML(label)} telemetry trend">
      <polyline points="${points}" />
    </svg>
    <span>${escapeHTML(formatTelemetryValue(latest, unit))}</span>
  </div>`;
}

function telemetryPolylinePoints(values: number[], max: number): string {
  const lastIndex = Math.max(1, values.length - 1);
  return values
    .map((value, index) => {
      const x = (index / lastIndex) * 100;
      const y = 26 - (Math.max(0, Math.min(value, max)) / max) * 24;
      return `${x.toFixed(1)},${y.toFixed(1)}`;
    })
    .join(" ");
}

function formatTelemetryValue(value: number, unit: string): string {
  if (unit === "%") {
    return `${Math.round(value)}%`;
  }
  return value.toFixed(2);
}

function runTelemetryPanel(telemetry: RunRecord["telemetry"]): string {
  if (!telemetry) {
    return `<div class="run-telemetry-panel"><div class="run-telemetry-grid"><div class="run-metric" data-muted="true"><span>telemetry</span><strong>not sampled</strong><small>no box metrics</small></div></div></div>`;
  }
  const samples = runTelemetrySamples(telemetry);
  const current = telemetry.end || samples.at(-1) || telemetry.start;
  if (!current) {
    return `<div class="run-telemetry-panel"><div class="run-telemetry-grid"><div class="run-metric" data-muted="true"><span>telemetry</span><strong>not sampled</strong><small>no box metrics</small></div></div></div>`;
  }
  const memory = telemetryStorage(
    current.memoryUsedBytes,
    current.memoryTotalBytes,
    current.memoryPercent,
  );
  const disk = telemetryStorage(current.diskUsedBytes, current.diskTotalBytes, current.diskPercent);
  const sampleLabel =
    samples.length > 1
      ? `${samples.length} samples`
      : samples.length === 1
        ? "1 sample"
        : "no history";
  return `<div class="run-telemetry-panel">
    <div class="run-telemetry-grid">
      ${runMetric("load", telemetryLoad(current), "1 / 5 / 15")}
      ${runMetric("memory", memory, telemetryDeltaLabel("delta", telemetry.start?.memoryUsedBytes, telemetry.end?.memoryUsedBytes))}
      ${runMetric("disk", disk, telemetryDeltaLabel("delta", telemetry.start?.diskUsedBytes, telemetry.end?.diskUsedBytes))}
      ${runMetric("sampled", current.capturedAt ? relativeTime(current.capturedAt) : undefined, sampleLabel)}
    </div>
    <div class="run-telemetry-trends">
      ${telemetrySparkline(
        "load",
        samples.map((sample) => sample.load1),
        "load",
      )}
      ${telemetrySparkline(
        "memory",
        samples.map((sample) => sample.memoryPercent),
        "%",
      )}
      ${telemetrySparkline(
        "disk",
        samples.map((sample) => sample.diskPercent),
        "%",
      )}
    </div>
  </div>`;
}

function runMetric(label: string, value: string | undefined, detail: string | undefined): string {
  return `<div class="run-metric">
    <span>${escapeHTML(label)}</span>
    <strong>${escapeHTML(value || "-")}</strong>
    <small>${escapeHTML(detail || "")}</small>
  </div>`;
}

function telemetryDeltaLabel(
  label: string,
  start: number | undefined,
  end: number | undefined,
): string | undefined {
  const delta = telemetryDelta(start, end);
  return delta ? `${label} ${delta}` : undefined;
}

function runTelemetryCell(telemetry: RunRecord["telemetry"]): string {
  if (!telemetry) {
    return "-";
  }
  const current = telemetry.end || runTelemetrySamples(telemetry).at(-1) || telemetry.start;
  if (!current) {
    return "-";
  }
  const parts = [];
  if (current.load1 !== undefined) {
    parts.push(`load ${current.load1.toFixed(2)}`);
  }
  if (current.memoryPercent !== undefined) {
    parts.push(`mem ${Math.round(current.memoryPercent)}%`);
  }
  const delta = telemetryDelta(telemetry.start?.memoryUsedBytes, telemetry.end?.memoryUsedBytes);
  if (delta) {
    parts.push(delta);
  }
  return parts.length ? parts.join(" · ") : "-";
}

function runTelemetrySamples(telemetry: RunRecord["telemetry"]): LeaseTelemetrySample[] {
  if (!telemetry) {
    return [];
  }
  const byTime = new Map<string, LeaseTelemetrySample>();
  for (const sample of [telemetry.start, ...(telemetry.samples ?? []), telemetry.end]) {
    if (sample?.capturedAt) {
      byTime.set(sample.capturedAt, sample);
    }
  }
  return [...byTime.values()].toSorted((left, right) =>
    left.capturedAt.localeCompare(right.capturedAt),
  );
}

function telemetryDelta(start: number | undefined, end: number | undefined): string | undefined {
  if (start === undefined || end === undefined) {
    return undefined;
  }
  const delta = end - start;
  if (delta === 0) {
    return "0 B";
  }
  const prefix = delta > 0 ? "+" : "-";
  return `${prefix}${formatBytes(Math.abs(delta))}`;
}

function telemetryLoad(telemetry: LeaseRecord["telemetry"]): string | undefined {
  if (!telemetry || telemetry.load1 === undefined) {
    return undefined;
  }
  const load5 = telemetry.load5 === undefined ? "" : ` / ${telemetry.load5.toFixed(2)}`;
  const load15 = telemetry.load15 === undefined ? "" : ` / ${telemetry.load15.toFixed(2)}`;
  return `${telemetry.load1.toFixed(2)}${load5}${load15}`;
}

function telemetryCPUCount(count: number | undefined): string | undefined {
  if (count === undefined) {
    return undefined;
  }
  return `${count} vCPU${count === 1 ? "" : "s"}`;
}

function telemetryStorage(
  used: number | undefined,
  total: number | undefined,
  percent: number | undefined,
): string | undefined {
  if (used !== undefined && total !== undefined) {
    const percentLabel = percent === undefined ? "" : ` (${Math.round(percent)}%)`;
    return `${formatBytes(used)} / ${formatBytes(total)}${percentLabel}`;
  }
  return percent === undefined ? undefined : `${Math.round(percent)}%`;
}

function providerIcon(provider: string): string {
  return providerIcons[provider] ?? genericProviderIcon;
}

function runnerStatusTone(status: string): string {
  if (status === "ready" || status === "running") {
    return "ok";
  }
  if (status === "queued" || status === "starting" || status === "pending") {
    return "warn";
  }
  if (status === "failed" || status === "error") {
    return "bad";
  }
  return "";
}

function targetIcon(target: string): string {
  if (target === "windows") {
    return `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 5.5 11 4v7H4z"/><path d="m13 3.7 7-1.5V11h-7z"/><path d="M4 13h7v7l-7-1.5z"/><path d="M13 13h7v8.8l-7-1.5z"/></svg>`;
  }
  if (target === "macos") {
    return `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M15.5 4.5c-.7.8-1.5 1.3-2.5 1.2-.1-1 .4-1.9 1.1-2.5.7-.7 1.7-1.1 2.4-1.2.1.9-.3 1.8-1 2.5z"/><path d="M18.7 16.2c-.5 1.1-.8 1.5-1.4 2.4-.9 1.3-2.1 2.9-3.6 2.9-1.3 0-1.7-.8-3.5-.8s-2.2.8-3.6.8c-1.5 0-2.6-1.4-3.5-2.7-2.4-3.7-2.7-8 .1-10.3 1-.8 2.4-1.3 3.7-1.3 1.4 0 2.6.9 3.5.9.8 0 2.3-1.1 4-1 1.8.1 3.1.8 4 2.1-3.5 1.9-2.9 6.1.3 7z"/></svg>`;
  }
  return `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 5h16v14H4z"/><path d="M7 9h10M7 12h10M7 15h6"/></svg>`;
}

function bridgeRow(
  label: string,
  enabled: boolean,
  bridgeConnected: boolean,
  viewerConnected: boolean,
  action: string,
  bridgeLabel = "bridge",
  viewerLabel = "viewer",
): string {
  const status = enabled
    ? bridgeConnected
      ? viewerConnected
        ? `${viewerLabel} active`
        : `${bridgeLabel} ready`
      : `waiting for ${bridgeLabel}`
    : "unavailable";
  const tone = enabled ? (bridgeConnected ? "ok" : "warn") : "";
  return `<div class="bridge-row">
    <div><strong>${escapeHTML(label)}</strong><small>${escapeHTML(status)}</small></div>
    <span class="pill" data-tone="${tone}">${escapeHTML(enabled ? (bridgeConnected ? "connected" : "waiting") : "off")}</span>
    ${action}
  </div>`;
}

function egressSummary(egress: NonNullable<PortalLeaseBridgeStatus["egress"]>): string {
  const profile = egress.profile || "custom";
  const allow = egress.allow.length
    ? egress.allow.slice(0, 3).join(", ") + (egress.allow.length > 3 ? " +" : "")
    : "no allowlist";
  const updated = egress.updatedAt ? ` · ${relativeTime(egress.updatedAt)}` : "";
  return `${profile} · ${allow}${updated}`;
}

function commandBlock(label: string, command: string): string {
  return `<div class="command-row"><div><small>${escapeHTML(label)}</small><code>${escapeHTML(command)}</code></div><button class="icon-btn" type="button" title="copy command" aria-label="copy ${escapeHTML(label)} command" data-copy-command>${copyIcon}</button></div>`;
}

function resultsSummary(run: RunRecord): string {
  if (!run.results) {
    return `<p class="muted">no test result summary</p>`;
  }
  const result = run.results;
  return `<dl class="result-grid">
    ${metaRow("tests", String(result.tests))}
    ${metaRow("failures", String(result.failures))}
    ${metaRow("errors", String(result.errors))}
    ${metaRow("skipped", String(result.skipped))}
    ${metaRow("time", `${result.timeSeconds}s`)}
  </dl>`;
}

function html(
  title: string,
  body: string,
  status = 200,
  nonce = "",
  options: { frameAncestors?: string } = {},
): Response {
  const pageNonce = nonce || scriptNonce();
  const scriptSource = `'self' 'nonce-${pageNonce}'`;
  return new Response(
    `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <meta name="color-scheme" content="dark light">
  <meta name="theme-color" content="#0b0d0f">
  <title>${escapeHTML(title)}</title>
  <script nonce="${pageNonce}">(function(){var s;try{s=localStorage.getItem('crabbox-theme-source')}catch(e){}var m=(s==='light'||s==='dark'||s==='system')?s:'system';var d=window.matchMedia&&matchMedia('(prefers-color-scheme: dark)').matches;document.documentElement.dataset.themeSource=m;document.documentElement.dataset.theme=m==='system'?(d?'dark':'light'):m})();</script>
  <style>
    :root {
      color-scheme: dark;
      --bg:#0b0d0f; --fg:#f3f5f7; --muted:#9ca3af; --line:#262b31; --line-soft:#1d2126;
      --panel:#15181c; --panel-2:#0f1215; --accent:#38bdf8; --accent-fg:#001018; --accent-soft-fg:#bae6fd;
      --bad:#f87171; --warn:#fbbf24; --ok:#34d399;
      --inset:#0c0e10; --inset-deep:#080a0c; --hover:#1b1f24; --hover-active:#22272d; --hover-line:#3a4046;
      --code-fg:#d1fae5; --danger-fg:#fecaca; --icon:#cbd5e1; --subtle:#6b7280; --arrow:#4b5563;
      --ext-fg:#b6beca; --ext-badge:#8b949e; --ext-icon:#7c8490;
      --mono: ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;
    }
    :root[data-theme="light"] {
      color-scheme: light;
      --bg:#f4f6f8; --fg:#1a1d22; --muted:#5c636e; --line:#d8dce2; --line-soft:#e7eaee;
      --panel:#ffffff; --panel-2:#f1f3f6; --accent:#0284c7; --accent-fg:#ffffff; --accent-soft-fg:#0369a1;
      --bad:#dc2626; --warn:#b45309; --ok:#047857;
      --inset:#f1f3f6; --inset-deep:#e9ecf0; --hover:#eceef2; --hover-active:#e1e4e9; --hover-line:#b9bec7;
      --code-fg:#0f5132; --danger-fg:#b42318; --icon:#5c636e; --subtle:#9aa1ab; --arrow:#b9bec7;
      --ext-fg:#6b7280; --ext-badge:#8b929c; --ext-icon:#9aa1ab;
    }
    * { box-sizing: border-box; }
    html { min-height:100%; background:var(--bg); }
    body { margin:0; min-height:100vh; overflow-x:hidden; background:var(--bg); color:var(--fg); font:14px/1.45 ui-sans-serif,system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif; transition:background-color .18s,color .18s; }
    main { width:min(1180px, calc(100vw - 32px)); max-width:100%; margin:0 auto; padding:10px 0 22px; }
    .portal-shell { width:min(1240px, calc(100vw - 16px)); max-width:100%; height:100dvh; display:grid; grid-template-rows:auto minmax(0,1fr); gap:8px; padding:6px 0 8px; overflow:hidden; }
    .lease-shell { grid-template-rows:auto auto minmax(0,1fr); }
    .run-shell { height:auto; min-height:100dvh; overflow:visible; grid-template-rows:auto; }
    h1,h2,p { margin:0; }
    h1 { font-size:20px; font-weight:700; }
    h2 { font-size:12px; text-transform:uppercase; color:var(--muted); letter-spacing:0.04em; }
    a { color:inherit; }
    form { margin:0; }
    button { font:inherit; }
    code { display:block; max-width:100%; overflow:auto; padding:9px 10px; border:1px solid var(--line); border-radius:6px; background:var(--inset); color:var(--code-fg); font-family:var(--mono); }
    table { width:100%; border-collapse:collapse; table-layout:fixed; }
    th,td { padding:7px 10px; border-bottom:1px solid var(--line); text-align:left; vertical-align:middle; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; line-height:1.25; }
    th { position:sticky; top:0; z-index:2; color:var(--muted); font-size:11px; font-weight:700; text-transform:uppercase; background:var(--panel); box-shadow:0 1px 0 var(--line); }
    th[data-sortable] { cursor:pointer; user-select:none; }
    th[data-sortable]::after { content:""; display:inline-block; width:0; height:0; margin-left:6px; vertical-align:middle; border-left:3px solid transparent; border-right:3px solid transparent; border-top:4px solid var(--arrow); opacity:0.75; }
    th[aria-sort="ascending"]::after { border-top:0; border-bottom:4px solid var(--accent); opacity:1; }
    th[aria-sort="descending"]::after { border-top-color:var(--accent); opacity:1; }
    td { font-size:13px; }
    td small { display:block; color:var(--muted); margin-top:1px; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
    .top { position:sticky; top:0; z-index:20; display:flex; justify-content:space-between; gap:12px; align-items:center; min-height:38px; margin:0; padding:4px 0; background:linear-gradient(180deg, var(--bg) 72%, color-mix(in srgb, var(--bg) 0%, transparent)); backdrop-filter:blur(10px); }
    .portal-header-meta { flex:1 1 auto; min-width:0; overflow:hidden; }
    .portal-header-meta h1 { white-space:nowrap; }
    .portal-header-meta p { font-size:12px; min-width:0; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
    .top p,.muted,.empty { color:var(--muted); }
    .panel { min-width:0; border:1px solid var(--line); border-radius:8px; background:var(--panel); overflow:hidden; }
    .admin-panel { border-color:color-mix(in srgb, var(--accent) 30%, var(--line)); background:linear-gradient(180deg, color-mix(in srgb, var(--accent) 8%, var(--panel)), var(--panel)); }
    .admin-grid { display:grid; grid-template-columns:repeat(4,minmax(0,1fr)); gap:1px; background:var(--line-soft); border-bottom:1px solid var(--line); }
    .admin-metric { min-width:0; padding:10px; background:var(--panel); }
    .admin-metric span { display:block; margin-bottom:3px; color:var(--muted); font-size:10px; text-transform:uppercase; }
    .admin-metric strong { display:block; min-width:0; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; font-size:13px; }
    .admin-actions { display:flex; gap:6px; flex-wrap:wrap; padding:8px 10px; background:color-mix(in srgb, var(--panel-2) 70%, transparent); }
    .admin-shell { height:auto; min-height:100dvh; align-content:start; align-items:start; grid-template-rows:auto auto auto auto; overflow:visible; }
    .admin-shell > * { width:100%; }
    .admin-status-strip { display:grid; grid-template-columns:1.45fr 1.35fr repeat(4,minmax(0,.82fr)); gap:1px; border:1px solid var(--line); border-radius:8px; background:var(--line-soft); overflow:hidden; }
    .admin-status-strip .admin-metric { min-height:54px; }
    .admin-metric[data-tone="ok"] strong { color:var(--ok); }
    .admin-metric[data-tone="warn"] strong { color:var(--warn); }
    .admin-metric[data-tone="bad"] strong { color:var(--bad); }
    .admin-tabs { display:flex; gap:1px; border:1px solid var(--line); border-radius:8px; background:var(--line-soft); overflow:hidden; }
    .admin-tabs a { min-height:32px; display:inline-flex; align-items:center; justify-content:center; min-width:92px; padding:0 14px; background:var(--panel); color:var(--muted); text-decoration:none; font-size:12px; font-weight:800; text-transform:uppercase; }
    .admin-tabs a[data-active="true"] { color:var(--fg); background:var(--panel-2); box-shadow:inset 0 -2px 0 var(--accent); }
    .admin-tabs a:hover { color:var(--fg); background:var(--hover); }
    .admin-kicker { color:var(--muted); font-size:10px; font-weight:800; text-transform:uppercase; }
    .provider-status-grid { display:grid; grid-template-columns:repeat(auto-fit,minmax(220px,1fr)); gap:1px; background:var(--line-soft); }
    .provider-status-card { min-width:0; display:grid; grid-template-rows:auto auto auto 1fr; gap:9px; padding:12px; background:var(--panel); border-top:2px solid transparent; }
    .provider-status-card[data-tone="ok"] { border-top-color:var(--ok); }
    .provider-status-card[data-tone="warning"] { border-top-color:var(--warn); }
    .provider-status-card[data-tone="bad"] { border-top-color:var(--bad); }
    .provider-status-card[data-tone="disabled"] { opacity:0.58; border-top-color:var(--line); }
    .provider-status-head { display:grid; grid-template-columns:auto minmax(0,1fr) auto; gap:9px; align-items:center; }
    .provider-favicon { width:24px; height:24px; display:grid; place-items:center; border:1px solid var(--line); border-radius:6px; background:var(--panel-2); color:var(--icon); }
    .provider-favicon svg { width:16px; height:16px; fill:none; stroke:currentColor; stroke-width:1.8; stroke-linecap:round; stroke-linejoin:round; }
    .provider-favicon[data-provider="aws"] { color:#fbbf24; }
    .provider-favicon[data-provider="azure"] { color:#60a5fa; }
    .provider-favicon[data-provider="gcp"] { color:#34d399; }
    .provider-favicon[data-provider="hetzner"] { color:#f87171; }
    .provider-status-title { min-width:0; display:grid; gap:1px; }
    .provider-status-title strong,.provider-status-title span { min-width:0; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
    .provider-status-title > span { display:flex; gap:6px; align-items:center; color:var(--muted); font-size:11px; }
    .traffic-light { width:10px; height:10px; border-radius:50%; background:var(--muted); box-shadow:0 0 0 3px color-mix(in srgb, currentColor 16%, transparent); }
    .traffic-light[data-tone="ok"] { color:var(--ok); background:var(--ok); }
    .traffic-light[data-tone="warning"] { color:var(--warn); background:var(--warn); }
    .traffic-light[data-tone="bad"] { color:var(--bad); background:var(--bad); }
    .traffic-light[data-tone="disabled"] { color:var(--muted); background:var(--muted); }
    .provider-status-meta { display:grid; grid-template-columns:repeat(2,minmax(0,1fr)); gap:1px; margin:0; background:var(--line-soft); border:1px solid var(--line-soft); }
    .provider-status-meta div { min-width:0; padding:7px 8px; background:var(--panel-2); }
    .provider-status-meta dt { margin:0 0 2px; color:var(--muted); font-size:10px; text-transform:uppercase; }
    .provider-status-meta dd { margin:0; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
    .provider-status-card p { margin:0; color:var(--muted); font-size:12px; overflow-wrap:anywhere; }
    .provider-status-actions { display:flex; gap:6px; flex-wrap:wrap; align-self:end; }
    .provider-status-actions a,.provider-status-actions button { min-height:24px; display:inline-flex; align-items:center; padding:0 7px; border:1px solid var(--line); border-radius:6px; color:var(--muted); background:transparent; text-decoration:none; font-size:11px; font-weight:700; }
    .provider-status-actions a:hover { color:var(--fg); background:var(--hover); border-color:var(--hover-line); }
    .provider-status-actions button:disabled { opacity:0.55; cursor:not-allowed; }
    .admin-full-panel { min-height:0; max-height:none; }
    .admin-filter-row { display:flex; gap:6px; flex-wrap:wrap; padding:8px 10px; border-bottom:1px solid var(--line); background:var(--panel-2); }
    .admin-filter-chip { min-height:26px; display:inline-flex; align-items:center; gap:6px; padding:0 9px; border:1px solid var(--line); border-radius:6px; color:var(--muted); text-decoration:none; font-size:12px; font-weight:700; }
    .admin-filter-chip svg { width:14px; height:14px; fill:none; stroke:currentColor; stroke-width:1.8; stroke-linecap:round; stroke-linejoin:round; }
    .admin-filter-chip[data-active="true"] { color:var(--fg); background:var(--hover-active); border-color:var(--hover-line); }
    .admin-filter-chip:hover { color:var(--fg); background:var(--hover); }
    .admin-user-table th:nth-child(2),.admin-user-table td:nth-child(2),.admin-user-table th:nth-child(3),.admin-user-table td:nth-child(3) { text-align:right; }
    .admin-lease-table th:last-child,.admin-lease-table td:last-child { width:42px; text-align:right; }
    .admin-eject-form { display:flex; justify-content:flex-end; }
    .admin-eject { width:28px; height:28px; display:grid; place-items:center; border:1px solid transparent; border-radius:7px; background:transparent; color:var(--subtle); cursor:pointer; }
    .admin-eject:hover,.admin-eject:focus-visible { color:var(--bad); border-color:color-mix(in srgb, var(--bad) 45%, var(--line)); background:color-mix(in srgb, var(--bad) 12%, transparent); }
    .admin-nav-link { gap:6px; }
    .section-head { display:flex; justify-content:space-between; align-items:center; min-height:34px; padding:7px 10px; border-bottom:1px solid var(--line); }
    .section-actions { display:flex; align-items:center; justify-content:flex-end; gap:8px; min-width:0; color:var(--muted); }
    .section-actions span { overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
    .button { display:inline-flex; align-items:center; justify-content:center; min-height:28px; padding:0 10px; border-radius:7px; border:1px solid transparent; background:var(--accent); color:var(--accent-fg); text-decoration:none; font-size:12px; font-weight:700; white-space:nowrap; cursor:pointer; }
    .button.secondary { background:transparent; color:var(--fg); border:1px solid var(--line); font-weight:500; }
    .button.secondary:hover { background:var(--hover); border-color:var(--hover-line); }
    .button.action { min-width:56px; border:1px solid color-mix(in srgb, var(--accent) 42%, var(--line)); background:color-mix(in srgb, var(--accent) 10%, transparent); color:var(--accent-soft-fg); }
    .button.action:hover { background:color-mix(in srgb, var(--accent) 16%, transparent); border-color:color-mix(in srgb, var(--accent) 58%, var(--line)); }
    .button:disabled { opacity:0.45; cursor:not-allowed; }
    .button.danger { border:1px solid color-mix(in srgb, var(--bad) 42%, var(--line)); background:color-mix(in srgb, var(--bad) 18%, transparent); color:var(--danger-fg); cursor:pointer; }
    .button[data-state="ok"] { border-color:color-mix(in srgb, var(--ok) 45%, var(--line)); color:var(--ok); background:color-mix(in srgb, var(--ok) 12%, transparent); }
    .portal-dialog { width:min(460px, calc(100vw - 28px)); padding:0; border:1px solid var(--line); border-radius:12px; background:var(--panel); color:var(--fg); box-shadow:0 24px 90px rgba(0,0,0,0.58); overflow:hidden; }
    .portal-dialog::backdrop { background:rgba(0,0,0,0.58); backdrop-filter:blur(2px); }
    .portal-dialog[data-fallback-modal="true"] { position:fixed; top:50%; left:50%; z-index:101; display:block; max-height:calc(100dvh - 28px); margin:0; transform:translate(-50%,-50%); }
    .portal-dialog-backdrop { position:fixed; inset:0; z-index:100; background:rgba(0,0,0,0.58); backdrop-filter:blur(2px); }
    .portal-dialog-backdrop[hidden] { display:none; }
    body[data-portal-dialog-open="true"] { overflow:hidden; }
    .portal-dialog-form { display:grid; }
    .portal-dialog-head { padding:17px 18px 13px; border-bottom:1px solid var(--line); background:var(--panel-2); }
    .portal-dialog-head small { display:block; margin-bottom:3px; color:var(--muted); font-size:10px; font-weight:800; letter-spacing:0.06em; text-transform:uppercase; }
    .portal-dialog-head h2 { color:var(--fg); font-size:18px; letter-spacing:0; text-transform:none; }
    .portal-dialog-body { display:grid; gap:13px; padding:16px 18px; }
    .portal-dialog-message { color:var(--muted); line-height:1.55; white-space:pre-wrap; }
    .portal-dialog-input { display:grid; gap:6px; color:var(--muted); font-size:12px; font-weight:700; }
    .portal-dialog-input[hidden] { display:none; }
    .portal-dialog-input textarea { width:100%; min-height:112px; resize:vertical; padding:10px 11px; border:1px solid var(--line); border-radius:8px; background:var(--inset); color:var(--fg); font:13px/1.45 var(--mono); }
    .portal-dialog-input textarea:focus { outline:2px solid color-mix(in srgb, var(--accent) 35%, transparent); outline-offset:1px; border-color:color-mix(in srgb, var(--accent) 58%, var(--line)); }
    .portal-dialog-actions { display:flex; justify-content:flex-end; gap:8px; padding:12px 18px 16px; border-top:1px solid var(--line); background:var(--panel-2); }
    .lease-link { display:block; min-width:0; text-decoration:none; overflow:hidden; text-overflow:ellipsis; }
    .lease-link strong { display:block; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
    .mono { font-family:var(--mono); }
    .detail-grid { display:grid; grid-template-columns:minmax(0,1.1fr) minmax(280px,0.9fr); gap:8px; min-height:0; }
    .detail-card { min-width:0; }
    .meta-grid { display:grid; grid-template-columns:repeat(2,minmax(0,1fr)); gap:0; margin:0; }
    .meta-grid div { padding:8px 10px; border-bottom:1px solid var(--line-soft); }
    .meta-grid dt { color:var(--muted); font-size:11px; text-transform:uppercase; margin-bottom:3px; }
    .meta-grid dd { margin:0; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
    .telemetry-strip { display:grid; gap:7px; padding:9px 10px; border-top:1px solid var(--line-soft); background:var(--panel-2); }
    .telemetry-strip-head { display:flex; justify-content:space-between; align-items:center; gap:8px; color:var(--muted); font-size:11px; text-transform:uppercase; }
    .telemetry-strip-head div { display:flex; gap:4px; align-items:center; flex-wrap:wrap; justify-content:flex-end; }
    .telemetry-line { display:grid; grid-template-columns:58px minmax(0,1fr) 52px; gap:8px; align-items:center; min-height:24px; font-size:12px; }
    .telemetry-line > span:first-child { color:var(--muted); text-transform:uppercase; font-size:10px; }
    .telemetry-line > span:last-child { text-align:right; font-family:var(--mono); color:var(--code-fg); }
    .telemetry-chart { width:100%; height:24px; display:block; overflow:visible; }
    .telemetry-chart polyline { fill:none; stroke:var(--accent); stroke-width:1.8; vector-effect:non-scaling-stroke; }
    .stop-form { padding:10px; }
    .bridge-grid { display:grid; gap:0; }
    .bridge-row { display:grid; grid-template-columns:minmax(0,1fr) auto auto; gap:8px; align-items:center; padding:9px 10px; border-bottom:1px solid var(--line-soft); }
    .bridge-row small { display:block; color:var(--muted); margin-top:2px; }
    .access-commands { display:grid; gap:8px; padding:10px; border-top:1px solid var(--line-soft); }
    .run-artifacts { display:grid; grid-template-columns:repeat(2,minmax(0,1fr)); gap:8px; padding:10px; }
    .run-shell .detail-grid { grid-template-columns:minmax(0,1fr) minmax(260px,0.42fr); }
    .run-shell .meta-grid { grid-template-columns:repeat(3,minmax(0,1fr)); }
    .run-shell .meta-grid div { padding:6px 10px; }
    .run-shell .meta-grid dt { margin-bottom:2px; font-size:10px; }
    .run-shell .meta-grid dd { font-size:13px; }
    .runner-shell .detail-grid { grid-template-columns:minmax(0,1fr) minmax(260px,0.64fr); }
    .detail-note { margin:0; padding:12px 14px; color:var(--muted); line-height:1.45; }
    .run-artifact-card .run-artifacts { gap:6px; padding:8px; }
    .run-artifact-card .button { width:100%; }
    .run-artifact-card .result-grid { grid-column:1 / -1; }
    .run-telemetry-panel { border-top:1px solid var(--line-soft); background:var(--panel-2); }
    .run-telemetry-grid { display:grid; grid-template-columns:repeat(4,minmax(0,1fr)); }
    .run-metric { min-width:0; padding:7px 10px; border-right:1px solid var(--line-soft); }
    .run-metric:last-child { border-right:0; }
    .run-metric span { display:block; color:var(--muted); font-size:10px; font-weight:700; letter-spacing:0.04em; text-transform:uppercase; }
    .run-metric strong { display:block; margin-top:2px; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; font-size:13px; }
    .run-metric small { display:block; margin-top:1px; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; color:var(--muted); font-size:11px; }
    .run-metric[data-muted="true"] { grid-column:1 / -1; }
    .run-telemetry-trends { display:grid; grid-template-columns:repeat(3,minmax(0,1fr)); border-top:1px solid var(--line-soft); }
    .run-telemetry-trends .telemetry-line { grid-template-columns:54px minmax(0,1fr) 46px; padding:7px 10px; border-right:1px solid var(--line-soft); }
    .run-telemetry-trends .telemetry-line:last-child { border-right:0; }
    .result-grid { display:grid; grid-template-columns:repeat(2,minmax(0,1fr)); gap:0; margin:4px -14px -14px; border-top:1px solid var(--line-soft); }
    .result-grid div { padding:8px 10px; border-bottom:1px solid var(--line-soft); }
    .result-grid dt { color:var(--muted); font-size:11px; text-transform:uppercase; margin-bottom:3px; }
    .result-grid dd { margin:0; }
    .log-preview { margin:0; height:100%; min-height:0; overflow:auto; padding:10px; background:var(--inset-deep); color:var(--code-fg); border:0; border-radius:0; font-family:var(--mono); font-size:12px; line-height:1.4; white-space:pre-wrap; overflow-wrap:anywhere; }
    .failure-list { display:grid; gap:0; margin:0; padding:0; list-style:none; }
    .failure-list li { padding:10px; border-bottom:1px solid var(--line-soft); }
    .failure-list small { display:block; color:var(--muted); margin-top:2px; }
    .failure-list p { margin-top:8px; color:var(--danger-fg); }
    .pill { display:inline-flex; align-items:center; justify-content:center; min-height:22px; padding:0 7px; border-radius:999px; border:1px solid var(--line); color:var(--muted); background:var(--panel-2); font-size:11px; white-space:nowrap; }
    .admin-pill { color:var(--accent-soft-fg); border-color:color-mix(in srgb, var(--accent) 42%, var(--line)); background:color-mix(in srgb, var(--accent) 12%, transparent); }
    .pill[data-tone="ok"],.pill[data-state="active"] { color:var(--ok); border-color:color-mix(in srgb, var(--ok) 35%, var(--line)); }
    .pill[data-tone="warn"] { color:var(--warn); border-color:color-mix(in srgb, var(--warn) 35%, var(--line)); }
    .pill[data-tone="bad"],.pill[data-state="released"],.pill[data-state="expired"] { color:var(--bad); border-color:color-mix(in srgb, var(--bad) 45%, var(--line)); }
    .icon-label { display:inline-flex; align-items:center; gap:7px; min-width:0; }
    .icon-label svg { width:14px; height:14px; flex:0 0 14px; fill:none; stroke:currentColor; stroke-width:1.8; stroke-linecap:round; stroke-linejoin:round; color:var(--icon); }
    .icon-label span { overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
    .icon-label[data-provider="aws"] svg { color:#fbbf24; }
    .icon-label[data-provider="azure"] svg { color:#60a5fa; }
    .icon-label[data-provider="hetzner"] svg { color:#ef4444; }
    .icon-label[data-provider="blacksmith-testbox"] svg { color:#a78bfa; }
    .icon-label[data-target="linux"] svg { color:#34d399; }
    .icon-label[data-target="windows"] svg { color:#38bdf8; }
    .icon-label[data-target="macos"] svg { color:#d8b4fe; }
    .actions-cell { display:flex; align-items:center; gap:5px; flex-wrap:nowrap; }
    .access-cell { display:flex; align-items:center; gap:5px; min-width:0; }
    .disabled-cell { color:var(--subtle); font-size:12px; }
    .state-stack { display:flex; align-items:center; gap:4px; flex-wrap:wrap; }
    .action-pill { max-width:100%; overflow:hidden; text-overflow:ellipsis; }
    .actions-stack { display:grid; gap:2px; min-width:0; }
    .actions-stack small { min-width:0; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; color:var(--muted); font-size:10px; }
    .external-access { flex-wrap:nowrap; }
    .external-access span { min-width:0; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
    .capacity-row { background:color-mix(in srgb, var(--panel-2) 18%, transparent); }
    .dedicated-link { display:grid; grid-template-columns:18px minmax(0,1fr); align-items:center; }
    .dedicated-link .dedicated-mark { display:inline-flex; width:18px; height:18px; color:#fbbf24; }
    .dedicated-link .dedicated-mark svg { width:15px; height:15px; fill:none; stroke:currentColor; stroke-width:1.8; stroke-linecap:round; stroke-linejoin:round; }
    .access-icon { display:inline-flex; align-items:center; justify-content:center; width:24px; height:24px; border-radius:6px; border:1px solid var(--line); color:var(--icon); background:var(--inset); text-decoration:none; }
    .access-icon svg { width:14px; height:14px; fill:none; stroke:currentColor; stroke-width:1.8; stroke-linecap:round; stroke-linejoin:round; }
    .access-icon[data-access="vscode"] { color:#d8b4fe; }
    .access-icon[data-access="vnc"] { color:#38bdf8; }
    .access-icon:hover { border-color:var(--hover-line); background:var(--hover); }
    .lease-release-form { display:flex; justify-content:flex-end; }
    .lease-release { color:var(--muted); cursor:pointer; }
    .lease-release[data-release-kind="managed"], .lease-release[data-release-kind="adapter"] { color:var(--bad); }
    .lease-release:hover,.lease-release:focus-visible { border-color:color-mix(in srgb, var(--bad) 45%, var(--line)); background:color-mix(in srgb, var(--bad) 12%, transparent); }
    .lease-release:disabled { opacity:0.5; cursor:wait; }
    .table-panel { min-height:0; display:grid; grid-template-rows:auto auto minmax(0,1fr) auto; overflow:hidden; }
    .command-panel,.log-panel { min-height:0; overflow:hidden; }
    .run-shell .table-panel { max-height:55dvh; }
    .run-shell .log-panel { max-height:34dvh; }
    .table-scroll { min-height:0; overflow:auto; }
    .table-tools { display:grid; grid-template-columns:minmax(180px,320px) minmax(0,1fr) auto; align-items:center; gap:8px; padding:6px 8px; border-bottom:1px solid var(--line-soft); background:var(--panel-2); }
    .table-search { width:100%; height:28px; padding:0 9px; border:1px solid var(--line); border-radius:7px; background:var(--inset); color:var(--fg); font:inherit; font-size:12px; }
    .table-search::placeholder { color:var(--subtle); }
    .table-search:focus { outline:2px solid color-mix(in srgb, var(--accent) 45%, transparent); outline-offset:1px; border-color:color-mix(in srgb, var(--accent) 55%, var(--line)); }
    .share-shell { height:auto; min-height:100dvh; align-content:start; grid-auto-rows:max-content; }
    .share-shell-embedded { width:100%; min-height:0; padding:0; gap:8px; }
    .share-shell .panel { align-self:start; }
    .share-form { display:flex; align-items:center; gap:8px; padding:8px 10px; border-bottom:1px solid var(--line-soft); background:color-mix(in srgb, var(--panel-2) 44%, transparent); }
    .share-form input,.share-form select { height:32px; min-width:0; padding:0 10px; border:1px solid var(--line); border-radius:7px; background:var(--inset); color:var(--fg); font:inherit; font-size:12px; }
    .share-form input:focus,.share-form select:focus { outline:2px solid color-mix(in srgb, var(--accent) 34%, transparent); outline-offset:1px; border-color:color-mix(in srgb, var(--accent) 55%, var(--line)); }
    .share-form input { flex:1; }
    .table-filters { display:flex; align-items:center; gap:3px; min-width:0; overflow-x:auto; padding:2px; border:1px solid var(--line); border-radius:7px; background:var(--inset); scrollbar-width:none; }
    .table-filters::-webkit-scrollbar { display:none; }
    .table-filter { flex:0 0 auto; min-height:22px; padding:0 7px; border:0; border-radius:5px; background:transparent; color:var(--muted); cursor:pointer; font:inherit; font-size:11px; }
    .table-filter[aria-pressed="true"] { background:var(--panel); color:var(--fg); }
    .table-filters-selects { display:grid; grid-template-columns:repeat(4,minmax(112px,1fr)); gap:6px; padding:0; border:0; background:transparent; overflow:visible; }
    .table-filter-select { display:grid; grid-template-columns:auto minmax(0,1fr); align-items:center; gap:6px; min-width:0; }
    .table-filter-select span { color:var(--muted); font-size:10px; font-weight:700; letter-spacing:0.04em; text-transform:uppercase; }
    .table-filter-select select { width:100%; height:28px; min-width:0; padding:0 8px; border:1px solid var(--line); border-radius:7px; background:var(--inset); color:var(--fg); font:inherit; font-size:12px; }
    .table-filter-select select:focus { outline:2px solid color-mix(in srgb, var(--accent) 34%, transparent); outline-offset:1px; border-color:color-mix(in srgb, var(--accent) 55%, var(--line)); }
    .table-count { color:var(--muted); font-size:12px; white-space:nowrap; }
    .table-footer { display:flex; justify-content:flex-end; align-items:center; gap:6px; min-height:36px; padding:5px 8px; background:var(--panel-2); }
    .table-page { min-width:64px; color:var(--muted); font-size:12px; text-align:center; }
    tr[hidden] { display:none; }
    .external-row { color:var(--muted); background:color-mix(in srgb, var(--panel-2) 58%, transparent); }
    .external-row td { border-bottom-color:var(--line-soft); }
    .external-row .lease-link { color:var(--ext-fg); pointer-events:none; }
    .external-row .lease-link strong::after { content:"external"; display:inline-flex; margin-left:8px; min-height:18px; align-items:center; padding:0 6px; border:1px solid var(--line); border-radius:999px; color:var(--ext-badge); font-size:10px; font-weight:700; text-transform:uppercase; vertical-align:middle; }
    .external-row .icon-label svg { color:var(--ext-icon); }
    .external-row .pill { opacity:0.82; }
    .row-links { display:inline-flex; align-items:center; gap:5px; min-width:0; }
    .row-link { display:inline-flex; align-items:center; min-height:22px; padding:0 7px; border:1px solid color-mix(in srgb, var(--accent) 36%, var(--line)); border-radius:6px; color:var(--accent-soft-fg); background:color-mix(in srgb, var(--accent) 9%, transparent); text-decoration:none; font-size:11px; font-weight:700; }
    .row-link.secondary { color:var(--icon); border-color:var(--line); background:var(--inset); font-weight:600; }
    .row-link:hover { border-color:color-mix(in srgb, var(--accent) 58%, var(--line)); background:color-mix(in srgb, var(--accent) 14%, transparent); }
    .lease-table th:nth-child(1) { width:25%; }
    .lease-table th:nth-child(2) { width:86px; }
    .lease-table th:nth-child(3),.lease-table th:nth-child(4) { width:104px; }
    .lease-table th:nth-child(5) { width:82px; }
    .lease-table th:nth-child(6) { width:118px; }
    .lease-table th:nth-child(7) { width:148px; }
    .lease-table th:nth-child(8),.lease-table td:nth-child(8) { width:40px; padding-left:4px; padding-right:4px; text-align:right; }
    .run-table th:nth-child(2) { width:104px; }
    .run-table th:nth-child(3) { width:112px; }
    .run-table th:nth-child(4) { width:92px; }
    .run-table th:nth-child(5) { width:190px; }
    .run-table th:nth-child(6) { width:154px; }
    .event-table th:nth-child(1) { width:58px; }
    .event-table th:nth-child(2) { width:24%; }
    .event-table th:nth-child(3) { width:96px; }
    .event-table th:nth-child(4) { width:150px; }
    .vnc-page { width:100vw; height:100vh; padding:0 12px 10px; display:grid; grid-template-rows:auto minmax(0,1fr) auto; gap:10px; overflow:auto; }
    .vnc-bar { position:sticky; top:0; z-index:10; display:flex; align-items:center; justify-content:space-between; gap:14px; min-height:42px; margin:0 -12px; padding:6px 16px; border-bottom:1px solid var(--line); background:color-mix(in srgb, var(--panel) 92%, transparent); box-shadow:0 8px 24px rgba(0,0,0,0.25); }
    .vnc-meta { display:flex; align-items:baseline; gap:12px; min-width:0; }
    .vnc-meta h1 { font-size:18px; font-weight:700; letter-spacing:-0.01em; white-space:nowrap; }
    .vnc-meta p { display:inline-flex; align-items:center; gap:8px; color:var(--muted); font-size:12px; min-width:0; overflow:hidden; }
    .vnc-meta .vnc-id { font-family:var(--mono); font-size:11px; opacity:0.85; }
    .vnc-meta .vnc-dot { width:3px; height:3px; border-radius:50%; background:var(--hover-line); flex-shrink:0; }
    .portal-actions { display:flex; align-items:center; justify-content:flex-end; gap:6px; flex-shrink:0; flex-wrap:wrap; }
    .status-pill { display:inline-flex; align-items:center; gap:8px; height:32px; padding:0 12px 0 11px; border-radius:8px; background:var(--panel-2); border:1px solid var(--line); font-size:12px; color:var(--muted); white-space:nowrap; transition:color 0.2s, border-color 0.2s; }
    .status-pill::before { content:""; width:8px; height:8px; border-radius:50%; background:currentColor; box-shadow:0 0 0 3px color-mix(in srgb, currentColor 18%, transparent); flex-shrink:0; }
    .status-pill[data-tone="ok"] { color:var(--ok); border-color:color-mix(in srgb, var(--ok) 35%, var(--line)); }
    .status-pill[data-tone="warn"] { color:var(--warn); border-color:color-mix(in srgb, var(--warn) 35%, var(--line)); }
    .status-pill[data-tone="bad"] { color:var(--bad); border-color:color-mix(in srgb, var(--bad) 45%, var(--line)); }
    .vnc-control { min-width:112px; transition:background 0.15s,border-color 0.15s,color 0.15s; }
    .vnc-control[data-role="controller"]:disabled { opacity:1; cursor:default; color:var(--fg); border-color:var(--line); background:var(--panel-2); }
    .vnc-control[data-role="observer"] { color:var(--accent-soft-fg); border-color:color-mix(in srgb, var(--accent) 38%, var(--line)); background:color-mix(in srgb, var(--accent) 8%, transparent); }
    .icon-btn { display:inline-flex; align-items:center; justify-content:center; width:32px; height:32px; padding:0; border-radius:8px; background:transparent; color:var(--fg); border:1px solid var(--line); cursor:pointer; transition:background 0.15s, border-color 0.15s, color 0.15s; }
    .icon-btn:hover { background:var(--hover); border-color:var(--hover-line); }
    .icon-btn:active { background:var(--hover-active); }
    .icon-btn[data-state="ok"] { color:var(--ok); border-color:color-mix(in srgb, var(--ok) 45%, var(--line)); }
    .icon-btn.mini { width:24px; height:24px; border-radius:6px; color:var(--muted); flex:0 0 24px; }
    .icon-btn.mini svg { width:13px; height:13px; }
    .theme-toggle { color:var(--muted); }
    .theme-toggle:hover { color:var(--fg); }
    .theme-toggle svg { width:15px; height:15px; display:block; }
    .theme-toggle .theme-icon-moon,.theme-toggle .theme-icon-sun,.theme-toggle .theme-icon-system { display:none; }
    :root[data-theme-source="system"] .theme-toggle .theme-icon-system { display:block; }
    :root[data-theme-source="dark"] .theme-toggle .theme-icon-sun { display:block; }
    :root[data-theme-source="light"] .theme-toggle .theme-icon-moon { display:block; }
    .screen { min-height:0; border:1px solid var(--line); border-radius:8px; background:var(--bg); overflow:hidden; box-shadow:inset 0 0 0 1px rgba(255,255,255,0.02); }
    .screen div { margin:0 auto; }
    .code-wait-screen { display:grid; place-items:center; padding:clamp(18px,5vw,64px); }
    .code-wait-card { width:min(720px,100%); display:grid; gap:9px; }
    .code-wait-kicker { color:var(--muted); font-size:11px; font-weight:700; letter-spacing:0.06em; text-transform:uppercase; }
    .code-wait-card h2 { margin:0; font-size:22px; letter-spacing:0; }
    .code-wait-card p { max-width:62ch; color:var(--muted); font-size:14px; line-height:1.55; }
    .vnc-bridge { display:flex; align-items:center; gap:10px; padding:6px 10px; border:1px solid var(--line); border-radius:8px; background:var(--panel); }
    .vnc-bridge-label { font-size:10px; text-transform:uppercase; letter-spacing:0.08em; color:var(--muted); flex-shrink:0; padding-left:4px; }
    .vnc-bridge-cmd { display:block; flex:1; min-width:0; padding:6px 10px; border:none; border-radius:5px; background:transparent; color:var(--code-fg); font-family:var(--mono); font-size:13px; overflow-x:auto; white-space:nowrap; }
    .vnc-share-dialog { width:min(640px, calc(100vw - 36px)); max-height:min(720px, calc(100dvh - 48px)); padding:0; border:1px solid var(--line); border-radius:12px; background:var(--panel); color:var(--fg); box-shadow:0 24px 90px rgba(0,0,0,0.58); overflow:hidden; }
    .vnc-share-dialog::backdrop { background:rgba(0,0,0,0.58); backdrop-filter:blur(2px); }
    .vnc-share-head { display:flex; align-items:center; justify-content:space-between; gap:12px; min-height:58px; padding:12px 14px 10px 18px; border-bottom:1px solid var(--line); background:var(--panel-2); }
    .vnc-share-head strong { display:block; font-size:18px; letter-spacing:0; }
    .vnc-share-head small { display:block; margin-top:1px; color:var(--muted); font-family:var(--mono); font-size:11px; }
    .vnc-share-body { display:grid; gap:18px; padding:16px 18px 14px; background:var(--panel); }
    .vnc-share-add { display:grid; grid-template-columns:minmax(0,1fr) 132px auto; gap:8px; }
    .vnc-share-add input,.vnc-share-add select,.vnc-share-access-row select { min-width:0; height:38px; padding:0 11px; border:1px solid var(--line); border-radius:8px; background:var(--inset); color:var(--fg); font:inherit; font-size:13px; }
    .vnc-share-add input:focus,.vnc-share-add select:focus,.vnc-share-access-row select:focus { outline:2px solid color-mix(in srgb, var(--accent) 35%, transparent); outline-offset:1px; border-color:color-mix(in srgb, var(--accent) 58%, var(--line)); }
    .vnc-share-section { display:grid; gap:8px; }
    .vnc-share-list { display:grid; gap:2px; }
    .vnc-share-person,.vnc-share-access-row { display:grid; grid-template-columns:36px minmax(0,1fr) auto; gap:12px; align-items:center; min-height:48px; }
    .vnc-share-avatar { display:grid; place-items:center; width:36px; height:36px; border-radius:50%; border:1px solid color-mix(in srgb, var(--accent) 32%, var(--line)); background:color-mix(in srgb, var(--accent) 13%, var(--panel-2)); color:var(--accent-soft-fg); font-size:10px; font-weight:800; text-transform:uppercase; }
    .vnc-share-person-main,.vnc-share-access-text { min-width:0; }
    .vnc-share-person-main strong,.vnc-share-access-text strong { display:block; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; font-size:13px; }
    .vnc-share-person-main span,.vnc-share-access-text span { display:block; margin-top:1px; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; color:var(--muted); font-size:12px; }
    .vnc-share-role-label { color:var(--muted); font-size:12px; white-space:nowrap; }
    .vnc-share-empty { margin-left:48px; color:var(--muted); font-size:13px; }
    .vnc-share-status { min-height:18px; color:var(--muted); font-size:12px; }
    .vnc-share-status[data-tone="ok"] { color:var(--ok); }
    .vnc-share-status[data-tone="bad"] { color:var(--bad); }
    .vnc-share-foot { display:flex; align-items:center; justify-content:flex-end; gap:8px; padding:12px 18px 16px; border-top:1px solid var(--line); background:var(--panel-2); }
    .vnc-share-foot #vnc-share-copy-link { margin-right:auto; }
    .danger-text { color:var(--danger-fg); border-color:color-mix(in srgb, var(--bad) 38%, var(--line)); }
    .danger-text:hover { background:color-mix(in srgb, var(--bad) 13%, transparent); border-color:color-mix(in srgb, var(--bad) 52%, var(--line)); }
    .commands { padding:12px; display:grid; gap:8px; }
    .command-row { display:grid; grid-template-columns:minmax(0,1fr) auto; gap:8px; align-items:end; }
    .command-row > div { min-width:0; overflow:hidden; }
    .command-row small { display:block; color:var(--muted); margin-bottom:4px; text-transform:uppercase; font-size:11px; }
    .command-row code { min-width:0; white-space:pre; }
    .error { margin-top:20vh; padding:24px; display:grid; gap:12px; }
    @media (max-width: 980px) {
      .admin-grid { grid-template-columns:repeat(2,minmax(0,1fr)); }
      .admin-status-strip { grid-template-columns:repeat(2,minmax(0,1fr)); }
      .admin-two-col { grid-template-columns:1fr; }
      .run-shell .detail-grid { grid-template-columns:1fr; }
      .run-shell .meta-grid { grid-template-columns:repeat(2,minmax(0,1fr)); }
      .run-telemetry-grid { grid-template-columns:repeat(2,minmax(0,1fr)); }
      .run-telemetry-trends { grid-template-columns:1fr; }
      .run-telemetry-trends .telemetry-line { border-right:0; border-bottom:1px solid var(--line-soft); }
      .run-telemetry-trends .telemetry-line:last-child { border-bottom:0; }
    }
    @media (max-width: 760px) {
      main { width:min(100vw - 20px, 1180px); padding:10px 0; }
      .portal-shell { width:min(100vw - 12px, 1180px); height:auto; min-height:100dvh; overflow:visible; }
      .lease-shell,.run-shell { grid-template-rows:auto; }
      th:nth-child(4),td:nth-child(4),th:nth-child(6),td:nth-child(6){ display:none; }
      .detail-grid { grid-template-columns:1fr; }
      .meta-grid { grid-template-columns:1fr; }
      .run-shell .meta-grid { grid-template-columns:1fr; }
      .run-telemetry-grid { grid-template-columns:1fr; }
      .run-artifacts { grid-template-columns:1fr; }
      .result-grid { grid-template-columns:1fr; }
      .bridge-row { grid-template-columns:1fr; align-items:start; }
      .table-panel { max-height:none; }
      .table-scroll { max-height:65dvh; }
      .table-tools { grid-template-columns:1fr; align-items:stretch; }
      .table-filters { justify-content:stretch; }
      .table-filter { flex:1; }
      .table-filters-selects { grid-template-columns:repeat(2,minmax(0,1fr)); }
      .table-filter-select { grid-template-columns:1fr; gap:2px; }
      .table-footer { justify-content:space-between; }
      .top{align-items:flex-start;}
      .vnc-bar { flex-wrap:wrap; gap:8px; min-height:0; padding:6px 10px; margin:0 -12px; }
      .vnc-meta { flex-wrap:wrap; gap:4px 10px; }
      .vnc-meta p .vnc-id { display:none; }
      .portal-actions { gap:6px; }
      .portal-actions .button { min-height:30px; padding:0 10px; }
      .admin-grid { grid-template-columns:1fr; }
      .admin-status-strip { grid-template-columns:1fr; }
      .provider-lease-list li { grid-template-columns:1fr; align-items:start; }
      .vnc-share-dialog { width:calc(100vw - 20px); }
      .vnc-share-add { grid-template-columns:1fr; }
      .vnc-share-person,.vnc-share-access-row { grid-template-columns:32px minmax(0,1fr); }
      .vnc-share-access-row select,.vnc-share-person .icon-btn,.vnc-share-role-label { grid-column:2; justify-self:start; }
      .vnc-share-foot { align-items:stretch; flex-direction:column; }
      .vnc-share-foot #vnc-share-copy-link { margin-right:0; }
      .vnc-bridge-label { display:none; }
    }
  </style>
</head>
<body>${body}
  <div id="portal-dialog-backdrop" class="portal-dialog-backdrop" hidden></div>
  <dialog id="portal-dialog" class="portal-dialog" aria-modal="true" aria-labelledby="portal-dialog-title" aria-describedby="portal-dialog-message">
    <div class="portal-dialog-form">
      <div class="portal-dialog-head">
        <small id="portal-dialog-kind">Confirmation</small>
        <h2 id="portal-dialog-title">Confirm action</h2>
      </div>
      <div class="portal-dialog-body">
        <p id="portal-dialog-message" class="portal-dialog-message"></p>
        <label id="portal-dialog-input-wrap" class="portal-dialog-input" hidden>
          <span id="portal-dialog-input-label">Value</span>
          <textarea id="portal-dialog-input"></textarea>
        </label>
      </div>
      <div class="portal-dialog-actions">
        <button id="portal-dialog-cancel" class="button secondary" type="button">cancel</button>
        <button id="portal-dialog-confirm" class="button" type="button">confirm</button>
      </div>
    </div>
  </dialog>
  <script nonce="${pageNonce}">${portalEnhancementsScript()}</script>
</body>
</html>`,
    {
      status,
      headers: {
        "content-security-policy": [
          "default-src 'none'",
          "base-uri 'none'",
          "connect-src 'self' ws: wss:",
          "frame-src 'self'",
          `frame-ancestors ${options.frameAncestors ?? "'none'"}`,
          "img-src 'self' data: blob:",
          `script-src ${scriptSource}`,
          "style-src 'unsafe-inline'",
        ].join("; "),
        "content-type": "text/html; charset=utf-8",
      },
    },
  );
}

function portalEnhancementsScript(): string {
  return `
(() => {
  const themeRoot = document.documentElement;
  const systemDark = window.matchMedia && matchMedia("(prefers-color-scheme: dark)");
  function storedTheme() {
    try { return localStorage.getItem("crabbox-theme-source"); } catch (_) { return null; }
  }
  function themeSource(value) {
    return value === "light" || value === "dark" || value === "system" ? value : "system";
  }
  function resolvedTheme(source) {
    return source === "system" ? (systemDark && systemDark.matches ? "dark" : "light") : source;
  }
  function applyTheme(source) {
    source = themeSource(source);
    const mode = resolvedTheme(source);
    themeRoot.dataset.themeSource = source;
    themeRoot.dataset.theme = mode;
    const dark = mode !== "light";
    document.querySelectorAll("[data-theme-toggle]").forEach((btn) => {
      btn.setAttribute("aria-pressed", dark ? "true" : "false");
      btn.setAttribute("aria-label", "Theme: " + source);
      btn.setAttribute("title", "Theme: " + source);
    });
    const meta = document.querySelector('meta[name="theme-color"]');
    if (meta) meta.setAttribute("content", dark ? "#0b0d0f" : "#f4f6f8");
    window.dispatchEvent(new CustomEvent("crabbox-theme-change", { detail: { source, mode } }));
  }
  applyTheme(themeRoot.dataset.themeSource || storedTheme());
  document.querySelectorAll("[data-theme-toggle]").forEach((btn) => {
    btn.addEventListener("click", () => {
      const current = themeSource(themeRoot.dataset.themeSource);
      const next = current === "system" ? "dark" : current === "dark" ? "light" : "system";
      applyTheme(next);
      try { localStorage.setItem("crabbox-theme-source", next); } catch (_) {}
    });
  });
  if (systemDark) {
    const onSystemChange = () => {
      if (themeSource(storedTheme()) !== "system") return;
      applyTheme("system");
    };
    if (systemDark.addEventListener) systemDark.addEventListener("change", onSystemChange);
    else if (systemDark.addListener) systemDark.addListener(onSystemChange);
  }
  const portalDialog = document.getElementById("portal-dialog");
  const portalDialogBackdrop = document.getElementById("portal-dialog-backdrop");
  const portalDialogKind = document.getElementById("portal-dialog-kind");
  const portalDialogTitle = document.getElementById("portal-dialog-title");
  const portalDialogMessage = document.getElementById("portal-dialog-message");
  const portalDialogInputWrap = document.getElementById("portal-dialog-input-wrap");
  const portalDialogInputLabel = document.getElementById("portal-dialog-input-label");
  const portalDialogInput = document.getElementById("portal-dialog-input");
  const portalDialogCancel = document.getElementById("portal-dialog-cancel");
  const portalDialogConfirm = document.getElementById("portal-dialog-confirm");
  let portalDialogMode = "confirm";
  let portalDialogResolve;
  let portalDialogClosing = false;
  function resolvePortalDialog(returnValue) {
    const resolve = portalDialogResolve;
    portalDialogResolve = undefined;
    portalDialogClosing = false;
    delete document.body.dataset.portalDialogOpen;
    if (!resolve) return;
    const accepted = returnValue === "confirm";
    resolve(portalDialogMode === "prompt" ? (accepted ? portalDialogInput.value : null) : accepted);
  }
  function closePortalDialog(returnValue) {
    if (!portalDialog?.hasAttribute("open")) return;
    if (portalDialog.dataset.fallbackModal === "true") {
      portalDialog.removeAttribute("open");
      delete portalDialog.dataset.fallbackModal;
      portalDialogBackdrop.hidden = true;
      resolvePortalDialog(returnValue);
      return;
    }
    portalDialogClosing = true;
    portalDialog.close(returnValue);
  }
  function showPortalDialog(mode, message, options = {}) {
    if (!portalDialog) return Promise.resolve(mode === "prompt" ? null : false);
    if (portalDialogClosing || portalDialog.hasAttribute("open")) {
      return Promise.resolve(mode === "prompt" ? null : false);
    }
    portalDialogMode = mode;
    portalDialogKind.textContent = mode === "prompt" ? "Input required" : "Confirmation";
    portalDialogTitle.textContent = options.title || (mode === "prompt" ? "Enter a value" : "Confirm action");
    portalDialogMessage.textContent = message || "";
    portalDialogInputWrap.hidden = mode !== "prompt";
    portalDialogInputLabel.textContent = options.label || "Value";
    portalDialogInput.value = options.value || "";
    portalDialogCancel.textContent = options.cancelLabel || "cancel";
    portalDialogConfirm.textContent = options.confirmLabel || (mode === "prompt" ? "continue" : "confirm");
    portalDialogConfirm.classList.toggle("danger", options.danger === true);
    portalDialog.returnValue = "";
    document.body.dataset.portalDialogOpen = "true";
    return new Promise((resolve) => {
      portalDialogResolve = resolve;
      if (portalDialog.showModal) {
        portalDialog.showModal();
      } else {
        portalDialog.dataset.fallbackModal = "true";
        portalDialog.setAttribute("open", "");
        portalDialogBackdrop.hidden = false;
      }
      window.setTimeout(() => {
        if (mode === "prompt") {
          portalDialogInput.focus();
          portalDialogInput.select();
        } else {
          portalDialogConfirm.focus();
        }
      }, 0);
    });
  }
  portalDialog?.addEventListener("cancel", () => {
    portalDialogClosing = true;
    portalDialog.returnValue = "cancel";
  });
  portalDialog?.addEventListener("close", () => {
    resolvePortalDialog(portalDialog.returnValue);
  });
  portalDialog?.addEventListener("keydown", (event) => {
    if (portalDialog.dataset.fallbackModal !== "true") return;
    if (event.key === "Escape") {
      event.preventDefault();
      closePortalDialog("cancel");
      return;
    }
    if (event.key !== "Tab") return;
    const controls = Array.from(
      portalDialog.querySelectorAll("button:not([disabled]), textarea:not([disabled])"),
    ).filter((control) => !control.closest("[hidden]"));
    if (!controls.length) return;
    const first = controls[0];
    const last = controls[controls.length - 1];
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
  });
  portalDialogCancel?.addEventListener("click", () => closePortalDialog("cancel"));
  portalDialogConfirm?.addEventListener("click", () => closePortalDialog("confirm"));
  portalDialogBackdrop?.addEventListener("click", () => closePortalDialog("cancel"));
  window.crabboxDialog = Object.freeze({
    confirm(message, options) {
      return showPortalDialog("confirm", message, options);
    },
    prompt(message, options) {
      return showPortalDialog("prompt", message, options);
    },
  });
  const confirmedForms = new WeakSet();
  document.querySelectorAll("form[data-confirm]").forEach((form) => {
    form.addEventListener("submit", async (event) => {
      if (confirmedForms.delete(form)) {
        if (event.submitter) {
          event.submitter.disabled = true;
          event.submitter.setAttribute("aria-busy", "true");
        }
        return;
      }
      const message = form.dataset.confirm || "";
      if (!message) return;
      event.preventDefault();
      const submitter = event.submitter;
      const confirmed = await window.crabboxDialog.confirm(message, {
        title: "Confirm lifecycle action",
        confirmLabel:
          submitter?.getAttribute("aria-label") || submitter?.textContent?.trim() || "confirm",
        danger: true,
      });
      if (!confirmed) return;
      if (typeof form.requestSubmit === "function") {
        confirmedForms.add(form);
        form.requestSubmit(submitter || undefined);
        return;
      }
      if (submitter) {
        submitter.disabled = true;
        submitter.setAttribute("aria-busy", "true");
      }
      HTMLFormElement.prototype.submit.call(form);
    });
  });
  function copyText(text, source) {
    const finish = () => {
      source.dataset.state = "ok";
      window.setTimeout(() => { delete source.dataset.state; }, 1200);
    };
    if (navigator.clipboard?.writeText) {
      navigator.clipboard.writeText(text).then(finish).catch(() => fallbackCopy(text, source, finish));
      return;
    }
    fallbackCopy(text, source, finish);
  }
  function fallbackCopy(text, source, finish) {
    const area = document.createElement("textarea");
    area.value = text;
    area.setAttribute("readonly", "");
    area.style.position = "fixed";
    area.style.opacity = "0";
    document.body.append(area);
    area.select();
    try { document.execCommand("copy"); } catch (_) {}
    area.remove();
    finish();
  }
  document.querySelectorAll("[data-copy-target]").forEach((button) => {
    button.addEventListener("click", () => {
      const selector = button.getAttribute("data-copy-target");
      if (!selector) return;
      const target = document.querySelector(selector);
      copyText(target?.textContent || "", button);
    });
  });
  document.querySelectorAll("[data-copy-command]").forEach((button) => {
    button.addEventListener("click", () => {
      const target = button.closest(".command-row")?.querySelector("code");
      copyText(target?.textContent || "", button);
    });
  });
  document.querySelectorAll("[data-copy-value]").forEach((button) => {
    button.addEventListener("click", () => {
      copyText(button.getAttribute("data-copy-value") || "", button);
    });
  });
  document.querySelectorAll("table[data-portal-table]").forEach((table, index) => {
    const body = table.tBodies[0];
    if (!body) return;
    const rows = Array.from(body.rows);
    const dataRows = rows.filter((row) => !row.querySelector(".empty"));
    const originalEmpty = rows.find((row) => row.querySelector(".empty"));
    const generatedEmpty = document.createElement("tr");
    const emptyCell = document.createElement("td");
    emptyCell.className = "empty";
    emptyCell.colSpan = table.tHead?.rows[0]?.cells.length || table.rows[0]?.cells.length || 1;
    emptyCell.textContent = "no matches";
    generatedEmpty.append(emptyCell);
    generatedEmpty.hidden = true;
    body.append(generatedEmpty);
    const pageSize = Math.max(1, Number.parseInt(table.dataset.pageSize || "10", 10) || 10);
    let query = "";
    let page = 1;
    let selectedFilter = table.dataset.filterDefault || "all";
    let sortColumn = -1;
    let sortDirection = "descending";
    const tools = document.createElement("div");
    tools.className = "table-tools";
    const input = document.createElement("input");
    input.className = "table-search";
    input.type = "search";
    input.placeholder = table.dataset.searchPlaceholder || "search table";
    input.setAttribute("aria-label", input.placeholder);
    input.disabled = dataRows.length === 0;
    const filterButtons = (table.dataset.filterButtons || "")
      .split(",")
      .map((item) => item.trim())
      .filter(Boolean)
      .map((item) => {
        const parts = item.split(":");
        return { value: parts[0], label: parts[1] || parts[0] };
      });
    const filterGroups = (table.dataset.filterGroups || "")
      .split(";")
      .map((item) => item.trim())
      .filter(Boolean)
      .map((item) => {
        const parts = item.split("|");
        return {
          key: parts[0],
          label: parts[1] || parts[0],
          defaultValue: parts[2] || "all",
          options: (parts[3] || "")
            .split(",")
            .map((option) => option.trim())
            .filter(Boolean)
            .map((option) => {
              const optionParts = option.split(":");
              return { value: optionParts[0], label: optionParts[1] || optionParts[0] };
            }),
        };
      })
      .filter((group) => group.key && group.options.length > 0);
    const selectedGroups = new Map();
    const filters = document.createElement("div");
    filters.className = "table-filters";
    filters.hidden = filterButtons.length === 0 && filterGroups.length === 0;
    const filterControls = [];
    if (filterGroups.length > 0) {
      filters.classList.add("table-filters-selects");
      filterGroups.forEach((group) => {
        const label = document.createElement("label");
        label.className = "table-filter-select";
        const labelText = document.createElement("span");
        labelText.textContent = group.label;
        const select = document.createElement("select");
        select.setAttribute("aria-label", group.label + " filter");
        group.options.forEach((option) => {
          const element = document.createElement("option");
          element.value = option.value;
          element.textContent = option.label;
          select.append(element);
        });
        select.value = group.options.some((option) => option.value === group.defaultValue)
          ? group.defaultValue
          : "all";
        selectedGroups.set(group.key, select.value);
        select.addEventListener("change", () => {
          selectedGroups.set(group.key, select.value);
          page = 1;
          apply();
        });
        label.append(labelText, select);
        filters.append(label);
        filterControls.push({ group, select });
      });
    } else {
      filterButtons.forEach((filter) => {
        const button = document.createElement("button");
        button.className = "table-filter";
        button.type = "button";
        button.textContent = filter.label;
        button.addEventListener("click", () => {
          selectedFilter = filter.value;
          page = 1;
          apply();
        });
        filters.append(button);
        filterControls.push({ ...filter, button });
      });
    }
    const count = document.createElement("span");
    count.className = "table-count";
    tools.append(input, filters, count);
    const footer = document.createElement("div");
    footer.className = "table-footer";
    const prev = document.createElement("button");
    prev.className = "button secondary";
    prev.type = "button";
    prev.textContent = "prev";
    const pageLabel = document.createElement("span");
    pageLabel.className = "table-page";
    const next = document.createElement("button");
    next.className = "button secondary";
    next.type = "button";
    next.textContent = "next";
    footer.append(prev, pageLabel, next);
    const tableScroll = document.createElement("div");
    tableScroll.className = "table-scroll";
    table.before(tools);
    tools.after(tableScroll);
    tableScroll.append(table);
    tableScroll.after(footer);
    table.dataset.enhancedIndex = String(index);
    const headers = Array.from(table.tHead?.rows[0]?.cells || []);
    headers.forEach((header, headerIndex) => {
      if (!header.textContent.trim()) return;
      header.dataset.sortable = "true";
      header.tabIndex = 0;
      header.addEventListener("click", () => {
        if (sortColumn === headerIndex) {
          sortDirection = sortDirection === "ascending" ? "descending" : "ascending";
        } else {
          sortColumn = headerIndex;
          sortDirection = "ascending";
        }
        page = 1;
        apply();
      });
      header.addEventListener("keydown", (event) => {
        if (event.key !== "Enter" && event.key !== " ") return;
        event.preventDefault();
        header.click();
      });
    });
    function sortValue(row, column) {
      const cell = row.cells[column];
      const value = cell?.dataset.sort || cell?.textContent.trim() || "";
      const number = Number(value);
      return value !== "" && Number.isFinite(number) ? number : value.toLowerCase();
    }
    function sorted(rows) {
      if (sortColumn < 0) return rows;
      return rows.toSorted((a, b) => {
        const left = sortValue(a, sortColumn);
        const right = sortValue(b, sortColumn);
        const result = typeof left === "number" && typeof right === "number"
          ? left - right
          : String(left).localeCompare(String(right));
        return sortDirection === "ascending" ? result : -result;
      });
    }
    function apply() {
      const filtered = sorted(dataRows.filter(
        (row) => {
          const tags = (row.dataset.filterTags || row.dataset.filterValue || "").split(/\\s+/);
          const groupTags = (row.dataset.filterGroupTags || "").split(/\\s+/);
          const groupMatch = filterGroups.length === 0 || filterGroups.every((group) => {
            const value = selectedGroups.get(group.key) || "all";
            return value === "all" || groupTags.includes(group.key + ":" + value);
          });
          return (
            groupMatch &&
            (filterGroups.length > 0 || selectedFilter === "all" || tags.includes(selectedFilter)) &&
            row.textContent.toLowerCase().includes(query)
          );
        },
      ));
      const pages = Math.max(1, Math.ceil(filtered.length / pageSize));
      page = Math.min(page, pages);
      const start = (page - 1) * pageSize;
      const visible = new Set(filtered.slice(start, start + pageSize));
      sorted(dataRows).forEach((row) => body.append(row));
      body.append(generatedEmpty);
      if (originalEmpty) body.append(originalEmpty);
      dataRows.forEach((row) => { row.hidden = !visible.has(row); });
      generatedEmpty.hidden = dataRows.length === 0 || filtered.length > 0;
      if (originalEmpty) originalEmpty.hidden = dataRows.length > 0;
      filterControls.forEach((filter) => {
        if (filter.button) {
          filter.button.setAttribute("aria-pressed", String(filter.value === selectedFilter));
        }
      });
      count.textContent = dataRows.length ? filtered.length + " of " + dataRows.length : "0";
      pageLabel.textContent = page + " / " + pages;
      prev.disabled = page <= 1;
      next.disabled = page >= pages;
      footer.hidden = dataRows.length <= pageSize && query === "";
      headers.forEach((header, headerIndex) => {
        if (!header.dataset.sortable) return;
        header.setAttribute("aria-sort", headerIndex === sortColumn ? sortDirection : "none");
      });
    }
    input.addEventListener("input", () => {
      query = input.value.trim().toLowerCase();
      page = 1;
      apply();
    });
    prev.addEventListener("click", () => {
      page = Math.max(1, page - 1);
      apply();
    });
    next.addEventListener("click", () => {
      page += 1;
      apply();
    });
    apply();
  });
})();
`;
}

function scriptNonce(): string {
  return crypto.randomUUID().replaceAll("-", "");
}

function shortTime(value: string): string {
  return relativeTime(value);
}

function relativeTime(value: string | undefined): string {
  if (!value) {
    return "-";
  }
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return value;
  }
  const deltaSeconds = Math.round((date.getTime() - Date.now()) / 1000);
  const absSeconds = Math.abs(deltaSeconds);
  const units: Array<[number, string]> = [
    [60 * 60 * 24 * 30, "mo"],
    [60 * 60 * 24, "d"],
    [60 * 60, "h"],
    [60, "m"],
  ];
  const unit = units.find(([seconds]) => absSeconds >= seconds);
  const amount = unit ? Math.max(1, Math.round(absSeconds / unit[0])) : Math.max(0, absSeconds);
  const suffix = deltaSeconds >= 0 ? "from now" : "ago";
  return `${amount}${unit ? unit[1] : "s"} ${suffix}`;
}

function compactAge(value: string | undefined): string {
  return relativeTime(value).replace(" ago", "").replace(" from now", "");
}

function leaseSortTime(lease: LeaseRecord): string {
  return lease.endedAt || lease.releasedAt || lease.updatedAt || lease.expiresAt || lease.createdAt;
}

function formatDuration(value: number | undefined): string {
  if (!Number.isFinite(value)) {
    return "-";
  }
  const seconds = Math.max(0, Math.round((value ?? 0) / 1000));
  if (seconds < 60) {
    return `${seconds}s`;
  }
  const minutes = Math.floor(seconds / 60);
  const rest = seconds % 60;
  return `${minutes}m ${rest}s`;
}

function formatSeconds(value: number): string {
  const seconds = Math.max(0, Math.round(value));
  if (seconds < 60) {
    return `${seconds}s`;
  }
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) {
    return `${minutes}m`;
  }
  const hours = Math.floor(minutes / 60);
  if (hours < 48) {
    return `${hours}h`;
  }
  return `${Math.floor(hours / 24)}d`;
}

function formatExitCode(value: number | undefined): string {
  return Number.isFinite(value) ? String(value) : "-";
}

function formatBytes(value: number): string {
  if (value < 1024) {
    return `${value} B`;
  }
  if (value < 1024 * 1024) {
    return `${(value / 1024).toFixed(1)} KiB`;
  }
  if (value < 1024 * 1024 * 1024) {
    return `${(value / 1024 / 1024).toFixed(1)} MiB`;
  }
  return `${(value / 1024 / 1024 / 1024).toFixed(1)} GiB`;
}

function truncate(value: string, maxLength: number): string {
  return value.length > maxLength ? `${value.slice(0, maxLength - 1)}...` : value;
}

function escapeHTML(value: string | undefined): string {
  return (value ?? "")
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;");
}

function scriptJSON(value: unknown): string {
  return JSON.stringify(value)
    .replaceAll("<", "\\u003c")
    .replaceAll(">", "\\u003e")
    .replaceAll("&", "\\u0026")
    .replaceAll("\u2028", "\\u2028")
    .replaceAll("\u2029", "\\u2029");
}
