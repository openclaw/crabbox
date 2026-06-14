package vercelsandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

func RunBridgeCLI(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) error {
	var body struct {
		Request bridgeRequest `json:"request"`
	}
	if err := json.NewDecoder(stdin).Decode(&body); err != nil {
		return fmt.Errorf("decode vercel-sandbox bridge request: %w", err)
	}
	if strings.TrimSpace(body.Request.Action) == "" {
		return fmt.Errorf("vercel-sandbox bridge action is required")
	}
	script := vercelSandboxBridgeScript()
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal vercel-sandbox bridge request: %w", err)
	}
	cmd := exec.CommandContext(ctx, "node", "--input-type=module", "-e", script)
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("vercel-sandbox SDK bridge failed: %w", err)
	}
	return nil
}

func vercelSandboxBridgeScript() string {
	return `
import fs from 'node:fs';
import { isIP } from 'node:net';
import os from 'node:os';
import path from 'node:path';
import { Writable } from 'node:stream';

const input = JSON.parse(fs.readFileSync(0, 'utf8'));
const req = input.request || {};

let mod;
try {
  mod = await import('@vercel/sandbox');
} catch (err) {
  console.error('missing @vercel/sandbox SDK; install it or set CRABBOX_VERCEL_SANDBOX_BRIDGE to a compatible bridge executable');
  process.exit(2);
}
const { Sandbox } = mod;

let authMod;
function linkedProject(start) {
  let current = path.resolve(start);
  while (true) {
    try {
      const value = JSON.parse(fs.readFileSync(path.join(current, '.vercel', 'project.json'), 'utf8'));
      if (typeof value.projectId === 'string' && value.projectId.trim() !== '' &&
          typeof value.orgId === 'string' && value.orgId.trim() !== '') {
        return { cwd: current, projectId: value.projectId, teamId: value.orgId };
      }
    } catch {}
    const parent = path.dirname(current);
    if (parent === current) return null;
    current = parent;
  }
}

const defaultProjectName = 'vercel-sandbox-default-project';

function teamQuery(teamId) {
  if (typeof teamId !== 'string' || teamId.trim() === '') {
    throw new Error('Vercel read-only scope lookup returned an invalid team identifier');
  }
  return teamId.startsWith('team_')
    ? 'teamId=' + encodeURIComponent(teamId)
    : 'slug=' + encodeURIComponent(teamId);
}

async function vercelReadOnlyRequest(token, endpoint) {
  const response = await fetch('https://api.vercel.com' + endpoint, {
    headers: { authorization: 'Bearer ' + token },
  });
  let body = {};
  try {
    body = await response.json();
  } catch {}
  return { response, body };
}

async function probeReadOnlyProject(token, teamId) {
  const projectResult = await vercelReadOnlyRequest(
    token,
    '/v2/projects/' + encodeURIComponent(defaultProjectName) + '?' + teamQuery(teamId),
  );
  if (projectResult.response.ok) {
    return { projectId: defaultProjectName, teamId };
  }
  if (![402, 403, 404].includes(projectResult.response.status)) {
    throw new Error('Vercel read-only project scope lookup failed with status ' + projectResult.response.status);
  }
  return null;
}

async function inferReadOnlyScope(token, requestedTeamId = '') {
  if (requestedTeamId) {
    return { projectId: defaultProjectName, teamId: requestedTeamId };
  }
  const userResult = await vercelReadOnlyRequest(token, '/v2/user');
  if (!userResult.response.ok) {
    throw new Error('Vercel read-only user scope lookup failed with status ' + userResult.response.status);
  }
  const user = userResult.body?.user || {};
  const seen = new Set();
  if (typeof user.defaultTeamId === 'string' && user.defaultTeamId.trim() !== '') {
    seen.add(user.defaultTeamId);
    const scope = await probeReadOnlyProject(token, user.defaultTeamId);
    if (scope) return scope;
  }
  let next = null;
  do {
    const endpoint = next === null ? '/v2/teams?limit=20' : '/v2/teams?limit=20&until=' + encodeURIComponent(next);
    const teamsResult = await vercelReadOnlyRequest(token, endpoint);
    if (!teamsResult.response.ok) {
      throw new Error('Vercel read-only team scope lookup failed with status ' + teamsResult.response.status);
    }
    const teams = Array.isArray(teamsResult.body?.teams) ? teamsResult.body.teams : [];
    const eligible = teams.filter((team) =>
      typeof team?.id === 'string' &&
      team.id.trim() !== '' &&
      typeof team?.slug === 'string' &&
      team?.membership?.role === 'OWNER' &&
      team?.billing?.plan === 'hobby'
    );
    eligible.sort((a, b) => {
      if (a.slug === user.username) return -1;
      if (b.slug === user.username) return 1;
      return (b.updatedAt || 0) - (a.updatedAt || 0);
    });
    for (const team of eligible) {
      if (seen.has(team.id)) continue;
      seen.add(team.id);
      const scope = await probeReadOnlyProject(token, team.id);
      if (scope) return scope;
    }
    next = teamsResult.body?.pagination?.next ?? null;
  } while (next !== null);
  if (typeof user.username === 'string' && user.username.trim() !== '' && !seen.has(user.username)) {
    const scope = await probeReadOnlyProject(token, user.username);
    if (scope) return scope;
  }
  throw new Error('Vercel Sandbox project scope cannot be resolved read-only; set projectId with teamId/scope, use OIDC, or link the checkout');
}

async function resolvedCredentials(readOnly = false) {
  const cfg = req.config || {};
  if (process.env.VERCEL_OIDC_TOKEN) {
    if (cfg.projectId || cfg.teamId || cfg.scope) {
      throw new Error('Vercel OIDC tokens are scoped by their claims; remove explicit projectId, teamId, and scope');
    }
    const token = process.env.VERCEL_OIDC_TOKEN;
    let claims;
    try {
      claims = JSON.parse(Buffer.from(token.split('.')[1], 'base64url').toString('utf8'));
    } catch {
      throw new Error('Vercel OIDC token has invalid claims');
    }
    if (!claims?.project_id || !claims?.owner_id) throw new Error('Vercel OIDC token is missing project/team claims');
    return { token, projectId: claims.project_id, teamId: claims.owner_id };
  }

  let token = process.env.VERCEL_TOKEN || process.env.VERCEL_AUTH_TOKEN || '';
  if (!token) {
    authMod ||= await import('@vercel/sandbox/dist/auth/index.js');
    let auth = authMod.getAuth();
    if (auth?.refreshToken && auth.expiresAt && auth.expiresAt.getTime() <= Date.now()) {
      const refreshed = await (await authMod.OAuth()).refreshToken(auth.refreshToken);
      auth = {
        expiresAt: new Date(Date.now() + refreshed.expires_in * 1000),
        token: refreshed.access_token,
        refreshToken: refreshed.refresh_token || auth.refreshToken,
      };
      authMod.updateAuthConfig(auth);
    }
    token = auth?.token || '';
  }
  if (!token) {
    throw new Error('Vercel Sandbox authentication unavailable; run sandbox login or set VERCEL_OIDC_TOKEN/VERCEL_TOKEN');
  }

  let projectId = cfg.projectId || '';
  let teamId = cfg.teamId || (projectId ? cfg.scope || '' : '');
  const linked = linkedProject(process.cwd());
  if (!projectId && !teamId && !cfg.scope && linked) {
    projectId = linked.projectId;
    teamId = linked.teamId;
  }
  if (projectId && !teamId && !cfg.scope) {
    throw new Error('Vercel Sandbox projectId requires teamId or scope');
  }
  if (readOnly && (!projectId || !teamId)) {
    const inferred = await inferReadOnlyScope(token, teamId || cfg.scope || '');
    projectId ||= inferred.projectId;
    teamId ||= inferred.teamId;
  }
  if (!projectId || !teamId) {
    authMod ||= await import('@vercel/sandbox/dist/auth/index.js');
    let inferCwd = process.cwd();
    let isolatedCwd = '';
    if (cfg.projectId || cfg.teamId || cfg.scope) {
      isolatedCwd = fs.mkdtempSync(path.join(os.tmpdir(), 'crabbox-vercel-scope-'));
      inferCwd = isolatedCwd;
    }
    let inferred;
    try {
      inferred = await authMod.inferScope({
        token,
        teamId: teamId || cfg.scope || undefined,
        cwd: inferCwd,
      });
    } finally {
      if (isolatedCwd) fs.rmSync(isolatedCwd, { recursive: true, force: true });
    }
    projectId ||= inferred.projectId;
    teamId ||= inferred.teamId;
  }
  return { token, projectId, teamId };
}

async function sandboxOptions(extra = {}, readOnly = false) {
  return { ...extra, ...(await resolvedCredentials(readOnly)) };
}

function summary(sandbox, metadata) {
  return {
    id: sandbox?.name || sandbox?.id || req.sandboxId || '',
    name: sandbox?.name || req.sandboxId || '',
    status: sandbox?.status || sandbox?.state || '',
    state: sandbox?.state || sandbox?.status || '',
    metadata: sandbox?.tags || sandbox?.metadata || metadata || {},
  };
}

function isCIDR(value) {
  return typeof value === 'string' && /^([0-9a-fA-F:.]+)\/[0-9]+$/.test(value);
}

function subnetCIDR(value) {
  if (isCIDR(value)) return value;
  const version = isIP(value);
  if (version === 4) return value + '/32';
  if (version === 6) return value + '/128';
  return '';
}

function networkPolicy(create, cfg) {
  const mode = String(create.networkPolicy || cfg.networkPolicy || '').trim().toLowerCase();
  const allow = [...(create.networkAllow || []), ...(cfg.networkAllow || [])].map((entry) => String(entry).trim()).filter(Boolean);
  const deny = [...(create.networkDeny || []), ...(cfg.networkDeny || [])].map((entry) => String(entry).trim()).filter(Boolean);
  if (mode === 'none') return 'deny-all';
  if ((mode === '' || mode === 'default') && allow.length === 0 && deny.length === 0) return undefined;
  const allowDomains = allow.filter((entry) => !subnetCIDR(entry));
  const allowCIDRs = allow.map(subnetCIDR).filter(Boolean);
  const denyCIDRs = deny.map(subnetCIDR).filter(Boolean);
  if (mode === 'public' && denyCIDRs.length === 0) return 'allow-all';
  if ((mode === 'private' || mode === 'restricted') && allowDomains.length === 0 && allowCIDRs.length === 0) return 'deny-all';
  const policy = {};
  if (mode === 'public' || ((mode === '' || mode === 'default') && allowDomains.length === 0 && allowCIDRs.length === 0 && denyCIDRs.length > 0)) {
    policy.allow = ['*'];
  } else if (allowDomains.length > 0) {
    policy.allow = allowDomains;
  }
  if (allowCIDRs.length > 0 || denyCIDRs.length > 0) {
    policy.subnets = {};
    if (allowCIDRs.length > 0) policy.subnets.allow = allowCIDRs;
    if (denyCIDRs.length > 0) policy.subnets.deny = denyCIDRs;
  }
  return Object.keys(policy).length > 0 ? policy : undefined;
}

function expandPortSpecs(values) {
  const out = [];
  for (const value of values || []) {
    const text = String(value).trim();
    if (!text) continue;
    const parts = text.split('-');
    const start = Number(parts[0]);
    const end = parts.length === 2 ? Number(parts[1]) : start;
    if (!Number.isInteger(start) || !Number.isInteger(end) || start < 1 || end < start || end > 65535) continue;
    for (let port = start; port <= end; port++) out.push(port);
  }
  return [...new Set(out)];
}

function writeFrame(type, data = '', exitCode = undefined) {
  const frame = { type };
  if (data !== '') frame.data = data;
  if (exitCode !== undefined) frame.exitCode = exitCode;
  return JSON.stringify(frame) + '\n';
}

function frameStream(type) {
  const outputLimitBytes = 4 * 1024 * 1024;
  let outputBytes = 0;
  let truncated = false;
  return new Writable({
    write(chunk, encoding, callback) {
      if (truncated) {
        callback();
        return;
      }
      const data = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk, encoding);
      const remaining = outputLimitBytes - outputBytes;
      if (data.length <= remaining) {
        process.stdout.write(writeFrame(type, data.toString()));
        outputBytes += data.length;
      } else {
        if (remaining > 0) process.stdout.write(writeFrame(type, data.subarray(0, remaining).toString()));
        process.stdout.write(writeFrame(type, '\n[crabbox: vercel-sandbox ' + type + ' truncated after ' + outputLimitBytes + ' bytes]\n'));
        outputBytes = outputLimitBytes;
        truncated = true;
      }
      callback();
    },
  });
}

async function getSandbox(name, resume = false) {
  return await Sandbox.get(await sandboxOptions({ name, resume }));
}

switch (req.action) {
  case 'check':
    process.stdout.write('{}\n');
    break;
  case 'check-project':
    await Sandbox.list(await sandboxOptions({ sortBy: 'name' }, true));
    process.stdout.write('{}\n');
    break;
  case 'resolve-scope': {
    const credentials = await resolvedCredentials();
    process.stdout.write(JSON.stringify({ projectId: credentials.projectId, teamId: credentials.teamId }) + '\n');
    break;
  }
  case 'resolve-scope-read-only': {
    const credentials = await resolvedCredentials(true);
    process.stdout.write(JSON.stringify({ projectId: credentials.projectId, teamId: credentials.teamId }) + '\n');
    break;
  }
  case 'create': {
    const create = req.create || {};
    const cfg = req.config || {};
    const snapshotId = create.snapshot || cfg.snapshot || '';
    const opts = await sandboxOptions({
      name: create.name,
      ...(!snapshotId && { runtime: create.runtime || cfg.runtime || 'node24' }),
      persistent: !!create.persistent,
      tags: create.metadata || {},
    });
    const timeoutSeconds = create.timeoutSeconds || cfg.timeoutSeconds || 0;
    if (timeoutSeconds > 0) opts.timeout = timeoutSeconds * 1000;
    const vcpus = create.vcpus || cfg.vcpus || 0;
    if (vcpus > 0) opts.resources = { vcpus };
    if (snapshotId) opts.source = { type: 'snapshot', snapshotId };
    const policy = networkPolicy(create, cfg);
    if (policy !== undefined) opts.networkPolicy = policy;
    const ports = expandPortSpecs(create.ports || cfg.ports || []);
    if (ports.length > 0) opts.ports = ports;
    const sandbox = await Sandbox.create(opts);
    process.stdout.write(JSON.stringify(summary(sandbox)) + '\n');
    break;
  }
  case 'list': {
    const result = await Sandbox.list(await sandboxOptions({ sortBy: 'name' }));
    const out = [];
    for await (const sandbox of result) out.push(summary(sandbox));
    process.stdout.write(JSON.stringify(out) + '\n');
    break;
  }
  case 'get': {
    const sandbox = await getSandbox(req.sandboxId);
    process.stdout.write(JSON.stringify(summary(sandbox)) + '\n');
    break;
  }
  case 'update-metadata': {
    console.error('default vercel-sandbox bridge cannot update sandbox metadata after creation; create-time ownership metadata is required');
    process.exit(2);
    break;
  }
  case 'delete': {
    const sandbox = await getSandbox(req.sandboxId);
    await sandbox.delete();
    process.stdout.write('{}\n');
    break;
  }
  case 'upload': {
    const sandbox = await getSandbox(req.sandboxId, true);
    const content = fs.readFileSync(req.payloadPath);
    await sandbox.writeFiles([{ path: req.remotePath, content }]);
    process.stdout.write('{}\n');
    break;
  }
  case 'exec': {
    const sandbox = await getSandbox(req.sandboxId, true);
    const execReq = req.exec || {};
    const result = await sandbox.runCommand({
      cmd: 'bash',
      args: ['-lc', execReq.command || ''],
      cwd: execReq.workingDir || undefined,
      env: execReq.env || undefined,
      stdout: frameStream('stdout'),
      stderr: frameStream('stderr'),
      timeoutMs: execReq.timeoutSecs > 0 ? execReq.timeoutSecs * 1000 : undefined,
    });
    process.stdout.write(writeFrame('result', '', result.exitCode ?? 0));
    break;
  }
  default:
    console.error('unsupported vercel-sandbox bridge action: ' + req.action);
    process.exit(2);
}
`
}
