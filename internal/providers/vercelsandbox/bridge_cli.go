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

function sandboxOptions(extra = {}) {
  const cfg = req.config || {};
  const out = { ...extra };
  if (cfg.projectId) out.projectId = cfg.projectId;
  if (cfg.teamId) out.teamId = cfg.teamId;
  if (cfg.scope) out.scope = cfg.scope;
  return out;
}

function summary(sandbox, metadata) {
  return {
    id: sandbox?.name || sandbox?.id || req.sandboxId || '',
    name: sandbox?.name || req.sandboxId || '',
    status: sandbox?.status || sandbox?.state || '',
    state: sandbox?.state || sandbox?.status || '',
    metadata: metadata || sandbox?.tags || {},
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

async function getSandbox(name) {
  return await Sandbox.get(sandboxOptions({ name, resume: false }));
}

switch (req.action) {
  case 'check':
    process.stdout.write('{}\n');
    break;
  case 'create': {
    const create = req.create || {};
    const cfg = req.config || {};
    const opts = sandboxOptions({
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
    const ports = create.ports || cfg.ports || [];
    if (ports.length > 0) opts.ports = ports.map((port) => Number(String(port).split('-')[0])).filter((port) => Number.isInteger(port) && port > 0);
    const sandbox = await Sandbox.create(opts);
    process.stdout.write(JSON.stringify(summary(sandbox, create.metadata || {})) + '\n');
    break;
  }
  case 'list': {
    const result = await Sandbox.list(sandboxOptions({ sortBy: 'name' }));
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
    const sandbox = await getSandbox(req.sandboxId);
    const content = fs.readFileSync(req.payloadPath);
    await sandbox.writeFiles([{ path: req.remotePath, content }]);
    process.stdout.write('{}\n');
    break;
  }
  case 'exec': {
    const sandbox = await getSandbox(req.sandboxId);
    const execReq = req.exec || {};
    let out = '';
    let err = '';
    const stdout = new Writable({ write(chunk, _enc, cb) { out += chunk.toString(); cb(); } });
    const stderr = new Writable({ write(chunk, _enc, cb) { err += chunk.toString(); cb(); } });
    const controller = execReq.timeoutSecs > 0 ? new AbortController() : null;
    const timer = controller ? setTimeout(() => controller.abort(new Error('vercel-sandbox command timed out')), execReq.timeoutSecs * 1000) : null;
    let result;
    try {
      result = await sandbox.runCommand({
        cmd: 'bash',
        args: ['-lc', execReq.command || ''],
        cwd: execReq.workingDir || undefined,
        env: execReq.env || undefined,
        stdout,
        stderr,
        signal: controller?.signal,
      });
    } finally {
      if (timer) clearTimeout(timer);
    }
    process.stdout.write(JSON.stringify({ stdout: out, stderr: err, exitCode: result.exitCode ?? 0 }) + '\n');
    break;
  }
  default:
    console.error('unsupported vercel-sandbox bridge action: ' + req.action);
    process.exit(2);
}
`
}
