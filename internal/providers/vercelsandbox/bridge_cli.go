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
function linkedProjectCwd(start) {
  let current = path.resolve(start);
  while (true) {
    if (fs.existsSync(path.join(current, '.vercel', 'project.json'))) return current;
    const parent = path.dirname(current);
    if (parent === current) return start;
    current = parent;
  }
}

async function resolvedCredentials() {
  const cfg = req.config || {};
  if (process.env.VERCEL_OIDC_TOKEN) {
    if (cfg.projectId || cfg.teamId || cfg.scope) {
      throw new Error('Vercel OIDC tokens are scoped by their claims; remove explicit projectId, teamId, and scope');
    }
    return {};
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
  if (projectId && !teamId && !cfg.scope) {
    throw new Error('Vercel Sandbox projectId requires teamId or scope');
  }
  if (!projectId || !teamId) {
    authMod ||= await import('@vercel/sandbox/dist/auth/index.js');
    let inferCwd = linkedProjectCwd(process.cwd());
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

async function sandboxOptions(extra = {}) {
  return { ...extra, ...(await resolvedCredentials()) };
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

function networkPolicy(create, cfg) {
  const mode = String(create.networkPolicy || cfg.networkPolicy || '').trim().toLowerCase();
  const allow = [...(create.networkAllow || []), ...(cfg.networkAllow || [])].filter(Boolean);
  const deny = [...(create.networkDeny || []), ...(cfg.networkDeny || [])].filter(Boolean);
  if ((mode === '' || mode === 'default' || mode === 'none') && allow.length === 0 && deny.length === 0) return undefined;
  if (mode === 'public') return 'allow-all';
  const allowDomains = allow.filter((entry) => !isCIDR(entry));
  const allowCIDRs = allow.filter(isCIDR);
  const denyCIDRs = deny.filter(isCIDR);
  const denyDomains = deny.filter((entry) => !isCIDR(entry));
  if ((mode === 'private' || mode === 'restricted') && allowDomains.length === 0 && allowCIDRs.length === 0 && denyCIDRs.length === 0 && denyDomains.length === 0) return 'deny-all';
  const policy = {};
  if (allowDomains.length > 0) policy.allow = allowDomains;
  if (denyDomains.length > 0) policy.deny = denyDomains;
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
  case 'create': {
    const create = req.create || {};
    const cfg = req.config || {};
    const opts = await sandboxOptions({
      name: create.name,
      runtime: create.runtime || cfg.runtime || 'node24',
      persistent: !!create.persistent,
      tags: create.metadata || {},
    });
    const timeoutSeconds = create.timeoutSeconds || cfg.timeoutSeconds || 0;
    if (timeoutSeconds > 0) opts.timeout = timeoutSeconds * 1000;
    const vcpus = create.vcpus || cfg.vcpus || 0;
    if (vcpus > 0) opts.resources = { vcpus };
    if (create.snapshot || cfg.snapshot) opts.source = { snapshotId: create.snapshot || cfg.snapshot };
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
