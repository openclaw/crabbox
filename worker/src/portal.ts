import type { ExternalRunnerRecord, LeaseRecord, RunEventRecord, RunRecord } from "./types";

const novncModuleURL = "/portal/assets/novnc/rfb.js";
const copyIcon = `<svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="9" y="9" width="12" height="12" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h10"/></svg>`;
const serverIcon = `<svg viewBox="0 0 24 24" aria-hidden="true"><rect x="5" y="3" width="14" height="18" rx="2"/><path d="M8 8h8M8 12h8M8 16h4"/></svg>`;
const vncIcon = `<svg viewBox="0 0 24 24" aria-hidden="true"><rect x="3" y="4" width="18" height="13" rx="2"/><path d="M8 21h8M12 17v4"/></svg>`;
const codeIcon = `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="m9 8-4 4 4 4"/><path d="m15 8 4 4-4 4"/><path d="m13 5-2 14"/></svg>`;
const shareIcon = `<svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="18" cy="5" r="3"/><circle cx="6" cy="12" r="3"/><circle cx="18" cy="19" r="3"/><path d="m8.6 10.5 6.8-4"/><path d="m8.6 13.5 6.8 4"/></svg>`;
const portalBrand = "🦀 crabbox";

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

export function portalHome(
  leases: LeaseRecord[],
  runners: ExternalRunnerRecord[],
  request: Request,
): Response {
  const sortedLeases = leases.toSorted((a, b) => leaseSortTime(b).localeCompare(leaseSortTime(a)));
  const active = sortedLeases.filter((lease) => lease.state === "active");
  const ended = sortedLeases.length - active.length;
  const sortedRunners = runners.toSorted((a, b) =>
    runnerSortTime(b).localeCompare(runnerSortTime(a)),
  );
  const activeRunners = sortedRunners.filter((runner) => !runner.stale);
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
  const defaultFilter = active.length + activeRunners.length > 0 ? "active" : "all";
  const filterButtons = [
    "active:active",
    "ended:ended",
    "external:external",
    "stale:stale",
    "stuck:stuck",
    ...(admin ? ["mine:mine", "system:system"] : []),
    "aws:aws",
    "azure:azure",
    "hetzner:hetzner",
    "blacksmith-testbox:blacksmith",
    "linux:linux",
    "macos:macos",
    "windows:windows",
    "all:all",
  ].join(",");
  const rows = [
    ...sortedLeases.map((lease) => ({ kind: "lease" as const, sort: leaseSortTime(lease), lease })),
    ...sortedRunners.map((runner) => ({
      kind: "runner" as const,
      sort: runnerSortTime(runner),
      runner,
    })),
  ]
    .toSorted((a, b) => b.sort.localeCompare(a.sort))
    .map((row) =>
      row.kind === "lease"
        ? leaseRow(row.lease, { admin, owner, org })
        : externalRunnerLeaseRow(row.runner, { admin, owner, org }),
    )
    .join("");
  const summary = admin
    ? `${active.length + activeRunners.length} active / ${ended} ended / ${sortedRunners.length} external / ${system} system`
    : `${active.length + activeRunners.length} active / ${ended} ended / ${sortedRunners.length} external`;
  return html(
    "Crabbox Portal",
    `<main class="portal-shell">
      ${portalHeader({
        meta: escapeHTML(new URL(request.url).host),
        actions: `<a class="button secondary" href="/portal/logout">log out</a>`,
      })}
      <section class="panel table-panel">
        <div class="section-head">
          <h2>leases</h2>
          <span>${escapeHTML(summary)}</span>
        </div>
        <table class="lease-table" data-portal-table data-page-size="12" data-search-placeholder="search leases" data-filter-buttons="${escapeHTML(filterButtons)}" data-filter-default="${defaultFilter}">
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

export function portalLeaseDetail(
  lease: LeaseRecord,
  runs: RunRecord[],
  bridgeStatus: PortalLeaseBridgeStatus,
  options: { canManage?: boolean } = {},
): Response {
  const slug = lease.slug || lease.id;
  const target = lease.target || "linux";
  const active = lease.state === "active";
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
      ? `<span class="muted">${escapeHTML(egressSummary(bridgeStatus.egress))}</span>`
      : `<span class="muted">no egress</span>`;
  const commands = active
    ? [
        commandBlock("shell", `crabbox ssh --id ${shellArg(slug)}`),
        commandBlock("run", `crabbox run --id ${shellArg(slug)} -- <command>`),
        lease.desktop ? commandBlock("WebVNC bridge", webVNCBridgeCommand(lease)) : "",
        lease.code ? commandBlock("code bridge", codeBridgeCommand(lease)) : "",
        bridgeStatus.egress
          ? commandBlock("egress status", `crabbox egress status --id ${shellArg(slug)}`)
          : "",
        bridgeStatus.egress
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
        meta: `${escapeHTML(slug)} · ${escapeHTML(lease.provider)} ${escapeHTML(target)} lease <span class="mono">${escapeHTML(lease.id)}</span>`,
        actions: `
          ${
            options.canManage
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
            active && options.canManage
              ? `<form method="post" action="/portal/leases/${encodeURIComponent(lease.id)}/release" class="stop-form">
                  <button class="button danger" type="submit">stop lease</button>
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
            ${metaHTMLRow("provider", providerBadge(run.provider))}
            ${metaHTMLRow("target", targetBadge(run.target || "linux", run.windowsMode))}
            ${metaRow("class", run.class)}
            ${metaRow("server type", run.serverType)}
            ${metaRow("phase", run.phase || run.state)}
            ${metaRow("exit", formatExitCode(run.exitCode))}
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

export function portalVNC(lease: LeaseRecord): Response {
  const nonce = scriptNonce();
  const slug = lease.slug || lease.id;
  const target = lease.target || "linux";
  const title = `WebVNC ${slug}`;
  const wsPath = `/portal/leases/${encodeURIComponent(lease.id)}/vnc/viewer`;
  const statusPath = `/portal/leases/${encodeURIComponent(lease.id)}/vnc/status`;
  const controlPath = `/portal/leases/${encodeURIComponent(lease.id)}/vnc/control`;
  const sharePath = `/portal/leases/${encodeURIComponent(lease.id)}/share`;
  const embeddedSharePath = `${sharePath}?embed=1`;
  const bridgeCmd = webVNCBridgeCommand(lease);
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
          <button id="vnc-share" class="button secondary" type="button">share</button>
          <a class="button secondary" href="/portal">leases</a>
          <a class="button secondary" href="/portal/logout">log out</a>
        `,
      })}
      <section id="screen" class="screen" aria-label="WebVNC display"></section>
      <footer class="vnc-bridge">
        <span class="vnc-bridge-label">bridge</span>
        <code id="vnc-bridge-cmd" class="vnc-bridge-cmd">${escapeHTML(bridgeCmd)}</code>
        <button id="vnc-copy" class="icon-btn" type="button" title="copy command" aria-label="copy bridge command">${copyIcon}</button>
      </footer>
      <dialog id="vnc-share-dialog" class="vnc-share-dialog" aria-label="Share lease">
        <div class="vnc-share-head">
          <div><strong>share ${escapeHTML(slug)}</strong><small>${escapeHTML(lease.id)}</small></div>
          <button id="vnc-share-close" class="icon-btn" type="button" title="close share" aria-label="close share">×</button>
        </div>
        <iframe id="vnc-share-frame" class="vnc-share-frame" title="Share ${escapeHTML(slug)}" loading="lazy"></iframe>
      </dialog>
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
      const shareURL = new URL(${JSON.stringify(embeddedSharePath)}, window.location.href);
      const viewerID = "viewer_" + (crypto.randomUUID?.() || String(Date.now()) + Math.random()).replace(/[^A-Za-z0-9_.:-]/g, "");
      wsURL.searchParams.set("viewer", viewerID);
      statusURL.searchParams.set("viewer", viewerID);
      const fragment = new URLSearchParams(window.location.hash.slice(1));
      const target = ${JSON.stringify(target)};
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
      let remoteClipboardText = "";
      let statusTimer;
      let controllerLabel = "";
      let isController = false;
      const terminalStatusCodes = new Set([403, 404, 409, 410]);
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
        controllerLabel = state.controllerLabel || "";
        const controlling = role === "controller";
        const connectedViewer = role === "controller" || role === "observer";
        isController = controlling;
        if (rfb) {
          rfb.viewOnly = !controlling;
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
      }
      async function refreshCollaborationState() {
        const state = await bridgeState();
        applyCollaborationState(state);
        return state;
      }
      function stopPolling(label) {
        stopped = true;
        connected = false;
        window.clearTimeout(retryTimer);
        window.clearInterval(statusTimer);
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
        screen.replaceChildren();
        try {
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
            scheduleRetry(state.message || "WebVNC daemon not running; run the bridge command below");
            return;
          }
          if (state && state.availableViewerSlots === 0) {
            scheduleRetry(state.message || "waiting for an available WebVNC observer slot");
            return;
          }
          setStatus(retryAttempt ? "bridge connected; opening viewer" : "connecting");
          rfb = new RFB(screen, wsURL.toString(), options);
          rfb.showDotCursor = true;
          rfb.scaleViewport = true;
          rfb.resizeSession = false;
          rfb.viewOnly = true;
          rfb.addEventListener("connect", () => {
            connected = true;
            retryAttempt = 0;
            setStatus("connected", "ok");
            void refreshCollaborationState();
            window.clearInterval(statusTimer);
            statusTimer = window.setInterval(refreshCollaborationState, 1500);
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
            scheduleRetry(connected ? "VNC bridge disconnected" : "waiting for VNC bridge");
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
            setStatus("VNC authentication failed; reopen WebVNC or copy the password from crabbox webvnc status", "bad");
          });
        } catch (error) {
          scheduleRetry(error instanceof Error ? error.message : String(error));
        }
      }
      window.addEventListener("beforeunload", () => {
        stopped = true;
        window.clearTimeout(retryTimer);
        window.clearInterval(statusTimer);
        rfb?.disconnect();
      });
      const takeoverBtn = document.getElementById("vnc-takeover");
      takeoverBtn?.addEventListener("click", async () => {
        try {
          const response = await fetch(controlURL, {
            method: "POST",
            headers: { "content-type": "application/json" },
            body: JSON.stringify({ viewerID }),
          });
          const state = response.ok ? await response.json() : undefined;
          if (!response.ok) throw new Error(state?.message || "takeover failed");
          applyCollaborationState(state);
          setStatus("you took control", "ok");
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
      const shareFrame = document.getElementById("vnc-share-frame");
      const shareCloseBtn = document.getElementById("vnc-share-close");
      shareBtn?.addEventListener("click", () => {
        if (shareFrame && !shareFrame.src) {
          shareFrame.src = shareURL.toString();
        }
        if (shareDialog?.showModal) {
          shareDialog.showModal();
        } else {
          window.location.href = ${JSON.stringify(sharePath)};
        }
      });
      shareCloseBtn?.addEventListener("click", () => shareDialog?.close());
      shareDialog?.addEventListener("click", (event) => {
        if (event.target === shareDialog) shareDialog.close();
      });
      async function readClipboardText() {
        if (navigator.clipboard?.readText) {
          try {
            return await navigator.clipboard.readText();
          } catch (_) {}
        }
        return window.prompt("Text to paste") || "";
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
  context: { admin: boolean; owner: string; org: string },
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
  return `<tr data-filter-tags="${escapeHTML([filterValue, ownership, lease.provider, target].join(" "))}">
    <td><a class="lease-link" href="${detailPath}"><strong>${escapeHTML(label)}</strong><small>${escapeHTML(subline)}</small></a></td>
    <td><span class="pill" data-state="${escapeHTML(lease.state)}">${escapeHTML(lease.state)}</span></td>
    <td>${providerBadge(lease.provider)}</td>
    <td>${targetBadge(target, lease.windowsMode)}</td>
    <td>${escapeHTML(lease.class)}</td>
    <td>${accessCell(lease, detailPath)}</td>
    ${timeCell(timeLabel)}
    <td></td>
  </tr>`;
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
  return `<tr class="external-row" data-filter-tags="${escapeHTML(filterTags.filter(Boolean).join(" "))}">
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
  const actions = options.actions?.trim()
    ? `<div class="portal-actions">${options.actions.trim()}</div>`
    : "";
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

function runRow(run: RunRecord): string {
  const stateTone = run.state === "succeeded" ? "ok" : run.state === "failed" ? "bad" : "warn";
  return `<tr data-filter-tags="${escapeHTML([run.state, run.provider, run.target || "linux"].filter(Boolean).join(" "))}">
    <td><a class="lease-link" href="/portal/runs/${encodeURIComponent(run.id)}"><strong>${escapeHTML(run.id)}</strong><small>${escapeHTML(run.command.join(" "))}</small></a></td>
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
  if (provider === "blacksmith-testbox") {
    return `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 6h16v5H4z"/><path d="M4 13h16v5H4z"/><path d="M8 8.5h.01M8 15.5h.01M12 8.5h5M12 15.5h5"/></svg>`;
  }
  if (provider === "aws") {
    return `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 15.5c3.8 2.2 9.1 2.5 14.8.9"/><path d="M17.5 13.2 20 16l-3.7.7"/><path d="M7 8.5h10l1.8 4H5.2z"/></svg>`;
  }
  if (provider === "azure") {
    return `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M7.5 17.5h9.2a4 4 0 0 0 .5-8 5.5 5.5 0 0 0-10.5-1.6A4.8 4.8 0 0 0 7.5 17.5Z"/><path d="M9 13h6"/></svg>`;
  }
  if (provider === "hetzner") {
    return `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3 20 7.5v9L12 21l-8-4.5v-9z"/><path d="M8 8v8M16 8v8M8 12h8"/></svg>`;
  }
  return `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 6h16v12H4z"/><path d="m7 10 3 2-3 2M12 15h5"/></svg>`;
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
  <meta name="color-scheme" content="dark">
  <meta name="theme-color" content="#0b0d0f">
  <title>${escapeHTML(title)}</title>
  <style>
    :root { color-scheme: dark; --bg:#0b0d0f; --fg:#f3f5f7; --muted:#9ca3af; --line:#262b31; --line-soft:#1d2126; --panel:#15181c; --panel-2:#0f1215; --accent:#38bdf8; --bad:#f87171; --warn:#fbbf24; --ok:#34d399; --mono: ui-monospace,SFMono-Regular,Menlo,Consolas,monospace; }
    * { box-sizing: border-box; }
    html { min-height:100%; background:var(--bg); }
    body { margin:0; min-height:100vh; overflow-x:hidden; background:var(--bg); color:var(--fg); font:14px/1.45 ui-sans-serif,system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif; }
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
    code { display:block; max-width:100%; overflow:auto; padding:9px 10px; border:1px solid var(--line); border-radius:6px; background:#0c0e10; color:#d1fae5; font-family:var(--mono); }
    table { width:100%; border-collapse:collapse; table-layout:fixed; }
    th,td { padding:7px 10px; border-bottom:1px solid var(--line); text-align:left; vertical-align:middle; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; line-height:1.25; }
    th { position:sticky; top:0; z-index:2; color:var(--muted); font-size:11px; font-weight:700; text-transform:uppercase; background:var(--panel); box-shadow:0 1px 0 var(--line); }
    th[data-sortable] { cursor:pointer; user-select:none; }
    th[data-sortable]::after { content:""; display:inline-block; width:0; height:0; margin-left:6px; vertical-align:middle; border-left:3px solid transparent; border-right:3px solid transparent; border-top:4px solid #4b5563; opacity:0.75; }
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
    .section-head { display:flex; justify-content:space-between; align-items:center; min-height:34px; padding:7px 10px; border-bottom:1px solid var(--line); }
    .section-actions { display:flex; align-items:center; justify-content:flex-end; gap:8px; min-width:0; color:var(--muted); }
    .section-actions span { overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
    .button { display:inline-flex; align-items:center; justify-content:center; min-height:28px; padding:0 10px; border-radius:7px; background:var(--accent); color:#001018; text-decoration:none; font-size:12px; font-weight:700; white-space:nowrap; }
    .button.secondary { background:transparent; color:var(--fg); border:1px solid var(--line); font-weight:500; }
    .button.secondary:hover { background:#1b1f24; border-color:#3a4046; }
    .button.action { min-width:56px; border:1px solid color-mix(in srgb, var(--accent) 42%, var(--line)); background:color-mix(in srgb, var(--accent) 10%, transparent); color:#bae6fd; }
    .button.action:hover { background:color-mix(in srgb, var(--accent) 16%, transparent); border-color:color-mix(in srgb, var(--accent) 58%, var(--line)); }
    .button:disabled { opacity:0.45; cursor:not-allowed; }
    .button.danger { border:1px solid color-mix(in srgb, var(--bad) 42%, var(--line)); background:color-mix(in srgb, var(--bad) 18%, transparent); color:#fecaca; cursor:pointer; }
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
    .telemetry-line > span:last-child { text-align:right; font-family:var(--mono); color:#d1fae5; }
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
    .log-preview { margin:0; height:100%; min-height:0; overflow:auto; padding:10px; background:#080a0c; color:#d1fae5; border:0; border-radius:0; font-family:var(--mono); font-size:12px; line-height:1.4; white-space:pre-wrap; overflow-wrap:anywhere; }
    .failure-list { display:grid; gap:0; margin:0; padding:0; list-style:none; }
    .failure-list li { padding:10px; border-bottom:1px solid var(--line-soft); }
    .failure-list small { display:block; color:var(--muted); margin-top:2px; }
    .failure-list p { margin-top:8px; color:#fecaca; }
    .pill { display:inline-flex; align-items:center; justify-content:center; min-height:22px; padding:0 7px; border-radius:999px; border:1px solid var(--line); color:var(--muted); background:var(--panel-2); font-size:11px; white-space:nowrap; }
    .pill[data-tone="ok"],.pill[data-state="active"] { color:var(--ok); border-color:color-mix(in srgb, var(--ok) 35%, var(--line)); }
    .pill[data-tone="warn"] { color:var(--warn); border-color:color-mix(in srgb, var(--warn) 35%, var(--line)); }
    .pill[data-tone="bad"],.pill[data-state="released"],.pill[data-state="expired"] { color:var(--bad); border-color:color-mix(in srgb, var(--bad) 45%, var(--line)); }
    .icon-label { display:inline-flex; align-items:center; gap:7px; min-width:0; }
    .icon-label svg { width:14px; height:14px; flex:0 0 14px; fill:none; stroke:currentColor; stroke-width:1.8; stroke-linecap:round; stroke-linejoin:round; color:#cbd5e1; }
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
    .disabled-cell { color:#6b7280; font-size:12px; }
    .state-stack { display:flex; align-items:center; gap:4px; flex-wrap:wrap; }
    .action-pill { max-width:100%; overflow:hidden; text-overflow:ellipsis; }
    .actions-stack { display:grid; gap:2px; min-width:0; }
    .actions-stack small { min-width:0; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; color:var(--muted); font-size:10px; }
    .external-access { flex-wrap:nowrap; }
    .external-access span { min-width:0; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
    .access-icon { display:inline-flex; align-items:center; justify-content:center; width:24px; height:24px; border-radius:6px; border:1px solid var(--line); color:#cbd5e1; background:#0c0e10; text-decoration:none; }
    .access-icon svg { width:14px; height:14px; fill:none; stroke:currentColor; stroke-width:1.8; stroke-linecap:round; stroke-linejoin:round; }
    .access-icon[data-access="vscode"] { color:#d8b4fe; }
    .access-icon[data-access="vnc"] { color:#38bdf8; }
    .access-icon:hover { border-color:#3a4046; background:#1b1f24; }
    .table-panel { min-height:0; display:grid; grid-template-rows:auto auto minmax(0,1fr) auto; overflow:hidden; }
    .command-panel,.log-panel { min-height:0; overflow:hidden; }
    .run-shell .table-panel { max-height:55dvh; }
    .run-shell .log-panel { max-height:34dvh; }
    .table-scroll { min-height:0; overflow:auto; }
    .table-tools { display:grid; grid-template-columns:minmax(180px,320px) minmax(0,1fr) auto; align-items:center; gap:8px; padding:6px 8px; border-bottom:1px solid var(--line-soft); background:var(--panel-2); }
    .table-search { width:100%; height:28px; padding:0 9px; border:1px solid var(--line); border-radius:7px; background:#0c0e10; color:var(--fg); font:inherit; font-size:12px; }
    .table-search::placeholder { color:#6b7280; }
    .table-search:focus { outline:2px solid color-mix(in srgb, var(--accent) 45%, transparent); outline-offset:1px; border-color:color-mix(in srgb, var(--accent) 55%, var(--line)); }
    .share-shell { height:auto; min-height:100dvh; align-content:start; grid-auto-rows:max-content; }
    .share-shell-embedded { width:100%; min-height:0; padding:0; gap:8px; }
    .share-shell .panel { align-self:start; }
    .share-form { display:flex; align-items:center; gap:8px; padding:8px 10px; border-bottom:1px solid var(--line-soft); background:color-mix(in srgb, var(--panel-2) 44%, transparent); }
    .share-form input,.share-form select { height:32px; min-width:0; padding:0 10px; border:1px solid var(--line); border-radius:7px; background:#0b0d0f; color:var(--fg); font:inherit; font-size:12px; }
    .share-form input:focus,.share-form select:focus { outline:2px solid color-mix(in srgb, var(--accent) 34%, transparent); outline-offset:1px; border-color:color-mix(in srgb, var(--accent) 55%, var(--line)); }
    .share-form input { flex:1; }
    .table-filters { display:flex; align-items:center; gap:3px; min-width:0; overflow-x:auto; padding:2px; border:1px solid var(--line); border-radius:7px; background:#0c0e10; scrollbar-width:none; }
    .table-filters::-webkit-scrollbar { display:none; }
    .table-filter { flex:0 0 auto; min-height:22px; padding:0 7px; border:0; border-radius:5px; background:transparent; color:var(--muted); cursor:pointer; font:inherit; font-size:11px; }
    .table-filter[aria-pressed="true"] { background:var(--panel); color:var(--fg); }
    .table-count { color:var(--muted); font-size:12px; white-space:nowrap; }
    .table-footer { display:flex; justify-content:flex-end; align-items:center; gap:6px; min-height:36px; padding:5px 8px; background:var(--panel-2); }
    .table-page { min-width:64px; color:var(--muted); font-size:12px; text-align:center; }
    tr[hidden] { display:none; }
    .external-row { color:var(--muted); background:color-mix(in srgb, var(--panel-2) 58%, transparent); }
    .external-row td { border-bottom-color:var(--line-soft); }
    .external-row .lease-link { color:#b6beca; pointer-events:none; }
    .external-row .lease-link strong::after { content:"external"; display:inline-flex; margin-left:8px; min-height:18px; align-items:center; padding:0 6px; border:1px solid var(--line); border-radius:999px; color:#8b949e; font-size:10px; font-weight:700; text-transform:uppercase; vertical-align:middle; }
    .external-row .icon-label svg { color:#7c8490; }
    .external-row .pill { opacity:0.82; }
    .row-links { display:inline-flex; align-items:center; gap:5px; min-width:0; }
    .row-link { display:inline-flex; align-items:center; min-height:22px; padding:0 7px; border:1px solid color-mix(in srgb, var(--accent) 36%, var(--line)); border-radius:6px; color:#bae6fd; background:color-mix(in srgb, var(--accent) 9%, transparent); text-decoration:none; font-size:11px; font-weight:700; }
    .row-link.secondary { color:#cbd5e1; border-color:var(--line); background:#0c0e10; font-weight:600; }
    .row-link:hover { border-color:color-mix(in srgb, var(--accent) 58%, var(--line)); background:color-mix(in srgb, var(--accent) 14%, transparent); }
    .lease-table th:nth-child(1) { width:25%; }
    .lease-table th:nth-child(2) { width:86px; }
    .lease-table th:nth-child(3),.lease-table th:nth-child(4) { width:104px; }
    .lease-table th:nth-child(5) { width:82px; }
    .lease-table th:nth-child(6) { width:118px; }
    .lease-table th:nth-child(7) { width:148px; }
    .lease-table th:nth-child(8) { width:24px; }
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
    .vnc-meta .vnc-dot { width:3px; height:3px; border-radius:50%; background:#3a4046; flex-shrink:0; }
    .portal-actions { display:flex; align-items:center; justify-content:flex-end; gap:6px; flex-shrink:0; flex-wrap:wrap; }
    .status-pill { display:inline-flex; align-items:center; gap:8px; height:32px; padding:0 12px 0 11px; border-radius:8px; background:var(--panel-2); border:1px solid var(--line); font-size:12px; color:var(--muted); white-space:nowrap; transition:color 0.2s, border-color 0.2s; }
    .status-pill::before { content:""; width:8px; height:8px; border-radius:50%; background:currentColor; box-shadow:0 0 0 3px color-mix(in srgb, currentColor 18%, transparent); flex-shrink:0; }
    .status-pill[data-tone="ok"] { color:var(--ok); border-color:color-mix(in srgb, var(--ok) 35%, var(--line)); }
    .status-pill[data-tone="warn"] { color:var(--warn); border-color:color-mix(in srgb, var(--warn) 35%, var(--line)); }
    .status-pill[data-tone="bad"] { color:var(--bad); border-color:color-mix(in srgb, var(--bad) 45%, var(--line)); }
    .vnc-control { min-width:112px; transition:background 0.15s,border-color 0.15s,color 0.15s; }
    .vnc-control[data-role="controller"]:disabled { opacity:1; cursor:default; color:var(--fg); border-color:var(--line); background:var(--panel-2); }
    .vnc-control[data-role="observer"] { color:#bae6fd; border-color:color-mix(in srgb, var(--accent) 38%, var(--line)); background:color-mix(in srgb, var(--accent) 8%, transparent); }
    .icon-btn { display:inline-flex; align-items:center; justify-content:center; width:32px; height:32px; padding:0; border-radius:8px; background:transparent; color:var(--fg); border:1px solid var(--line); cursor:pointer; transition:background 0.15s, border-color 0.15s, color 0.15s; }
    .icon-btn:hover { background:#1b1f24; border-color:#3a4046; }
    .icon-btn:active { background:#22272d; }
    .icon-btn[data-state="ok"] { color:var(--ok); border-color:color-mix(in srgb, var(--ok) 45%, var(--line)); }
    .icon-btn.mini { width:24px; height:24px; border-radius:6px; color:#9ca3af; flex:0 0 24px; }
    .icon-btn.mini svg { width:13px; height:13px; }
    .screen { min-height:0; border:1px solid var(--line); border-radius:8px; background:var(--bg); overflow:hidden; box-shadow:inset 0 0 0 1px rgba(255,255,255,0.02); }
    .screen div { margin:0 auto; }
    .code-wait-screen { display:grid; place-items:center; padding:clamp(18px,5vw,64px); }
    .code-wait-card { width:min(720px,100%); display:grid; gap:9px; }
    .code-wait-kicker { color:var(--muted); font-size:11px; font-weight:700; letter-spacing:0.06em; text-transform:uppercase; }
    .code-wait-card h2 { margin:0; font-size:22px; letter-spacing:0; }
    .code-wait-card p { max-width:62ch; color:var(--muted); font-size:14px; line-height:1.55; }
    .vnc-bridge { display:flex; align-items:center; gap:10px; padding:6px 10px; border:1px solid var(--line); border-radius:8px; background:var(--panel); }
    .vnc-bridge-label { font-size:10px; text-transform:uppercase; letter-spacing:0.08em; color:var(--muted); flex-shrink:0; padding-left:4px; }
    .vnc-bridge-cmd { display:block; flex:1; min-width:0; padding:6px 10px; border:none; border-radius:5px; background:transparent; color:#d1fae5; font-family:var(--mono); font-size:13px; overflow-x:auto; white-space:nowrap; }
    .vnc-share-dialog { width:min(760px, calc(100vw - 36px)); max-height:min(640px, calc(100dvh - 48px)); padding:0; border:1px solid var(--line); border-radius:10px; background:var(--panel); color:var(--fg); box-shadow:0 24px 90px rgba(0,0,0,0.58); overflow:hidden; }
    .vnc-share-dialog::backdrop { background:rgba(0,0,0,0.58); backdrop-filter:blur(2px); }
    .vnc-share-head { display:flex; align-items:center; justify-content:space-between; gap:12px; min-height:42px; padding:8px 10px 8px 14px; border-bottom:1px solid var(--line); background:var(--panel-2); }
    .vnc-share-head strong { display:block; font-size:13px; }
    .vnc-share-head small { display:block; margin-top:1px; color:var(--muted); font-family:var(--mono); font-size:11px; }
    .vnc-share-frame { display:block; width:100%; height:min(540px, calc(100dvh - 104px)); border:0; background:var(--bg); }
    .commands { padding:12px; display:grid; gap:8px; }
    .command-row { display:grid; grid-template-columns:minmax(0,1fr) auto; gap:8px; align-items:end; }
    .command-row > div { min-width:0; overflow:hidden; }
    .command-row small { display:block; color:var(--muted); margin-bottom:4px; text-transform:uppercase; font-size:11px; }
    .command-row code { min-width:0; white-space:pre; }
    .error { margin-top:20vh; padding:24px; display:grid; gap:12px; }
    @media (max-width: 980px) {
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
      .table-footer { justify-content:space-between; }
      .top{align-items:flex-start;}
      .vnc-bar { flex-wrap:wrap; gap:8px; min-height:0; padding:6px 10px; margin:0 -12px; }
      .vnc-meta { flex-wrap:wrap; gap:4px 10px; }
      .vnc-meta p .vnc-id { display:none; }
      .portal-actions { gap:6px; }
      .portal-actions .button { min-height:30px; padding:0 10px; }
      .vnc-share-dialog { width:calc(100vw - 20px); }
      .vnc-share-frame { height:calc(100dvh - 104px); }
      .vnc-bridge-label { display:none; }
    }
  </style>
</head>
<body>${body}<script nonce="${pageNonce}">${portalEnhancementsScript()}</script></body>
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
    const filters = document.createElement("div");
    filters.className = "table-filters";
    filters.hidden = filterButtons.length === 0;
    const filterControls = filterButtons.map((filter) => {
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
      return { ...filter, button };
    });
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
          return (
            (selectedFilter === "all" || tags.includes(selectedFilter)) &&
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
        filter.button.setAttribute("aria-pressed", String(filter.value === selectedFilter));
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
