import { createHash } from 'node:crypto';
import { createReadStream } from 'node:fs';
import * as fs from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawnSync } from 'node:child_process';
import { parseArgs } from 'node:util';
import { verifyNoticeBundle, noticeName, attributionName } from './notice-artifacts.mjs';

const root = path.dirname(fileURLToPath(import.meta.url));
const manifest = JSON.parse(await fs.readFile(path.join(root, 'manifest.json'), 'utf8'));
export const sourceIdentity = identityFromManifest(manifest);

export function identityFromManifest(value) {
  const identity = { nativeVersion: value?.jj?.version, sourceTreeSHA256: value?.sourceTree?.sha256 };
  if (value?.schemaVersion !== 1 || typeof identity.nativeVersion !== 'string' ||
      !/^[0-9]+\.[0-9]+\.[0-9]+$/.test(identity.nativeVersion) || !/^[0-9a-f]{64}$/.test(identity.sourceTreeSHA256)) {
    throw new Error('native source manifest has no valid pinned identity');
  }
  return Object.freeze(identity);
}
export const receiptName = 'crabbox-jj-source.json';
export function verifySourceVersion(value, identity = sourceIdentity) {
  if (value?.protocol !== 'crabbox-jj-source' || value.schema_version !== 1 || value.kind !== 'version' ||
      value.jj_version !== identity.nativeVersion || !Array.isArray(value.capabilities) ||
      !['context', 'live_inventory', 'recorded_inventory', 'recorded_export', 'environment_conditions'].every((item) => value.capabilities.includes(item))) {
    throw new Error('native helper protocol does not match the source package');
  }
}
export const nativeTargets = Object.freeze([
  ['darwin', 'amd64', 'x86_64-apple-darwin'],
  ['darwin', 'arm64', 'aarch64-apple-darwin'],
  ['linux', 'amd64', 'x86_64-unknown-linux-gnu'],
  ['linux', 'arm64', 'aarch64-unknown-linux-gnu'],
  ['windows', 'amd64', 'x86_64-pc-windows-msvc'],
  ['windows', 'arm64', 'aarch64-pc-windows-msvc'],
].map(([targetOS, targetArch, targetTriple]) => Object.freeze({
  targetOS, targetArch, targetTriple, key: `${targetOS}_${targetArch}`,
  binaryName: `crabbox-jj-source${targetOS === 'windows' ? '.exe' : ''}`,
})));

export function nativeTarget(targetOS, targetArch) {
  const target = nativeTargets.find((item) => item.targetOS === targetOS && item.targetArch === targetArch);
  if (!target) throw new Error('unsupported native helper target');
  return target;
}

export async function fileSHA256(file) {
  const hash = createHash('sha256');
  for await (const data of createReadStream(file)) hash.update(data);
  return hash.digest('hex');
}

export function createBuildReceipt({ target, profile, rustc, cargo, binarySHA256 }) {
  const receipt = {
    schemaVersion: 1, ...sourceIdentity,
    binarySHA256, targetOS: target.targetOS, targetArch: target.targetArch,
    targetTriple: target.targetTriple, profile, rustc, cargo,
  };
  validateReceipt(receipt);
  return receipt;
}

export function validateReceipt(receipt, identity = sourceIdentity) {
  const keys = ['schemaVersion', 'nativeVersion', 'sourceTreeSHA256', 'binarySHA256', 'targetOS', 'targetArch', 'targetTriple', 'profile', 'rustc', 'cargo'];
  if (!receipt || typeof receipt !== 'object' || Array.isArray(receipt) ||
      JSON.stringify(Object.keys(receipt).sort()) !== JSON.stringify(keys.sort())) {
    throw new Error('native helper receipt fields do not match the installation schema');
  }
  const target = nativeTarget(receipt.targetOS, receipt.targetArch);
  if (receipt.schemaVersion !== 1 || receipt.nativeVersion !== identity.nativeVersion ||
      receipt.sourceTreeSHA256 !== identity.sourceTreeSHA256 || receipt.targetTriple !== target.targetTriple ||
      !/^[0-9a-f]{64}$/.test(receipt.binarySHA256) || !['dev', 'release'].includes(receipt.profile)) {
    throw new Error('native helper receipt does not match the pinned source and target');
  }
  for (const name of ['rustc', 'cargo']) {
    if (typeof receipt[name] !== 'string' || receipt[name].length > 256 ||
        !new RegExp(`^${name} [0-9]+\\.[0-9]+\\.[0-9]+(?:[ -]|$)`).test(receipt[name]) || /[\r\n\0]/.test(receipt[name])) {
      throw new Error(`native helper receipt requires one ${name} version line`);
    }
  }
  return target;
}

export function verifyBinaryFormat(binary, target) {
  const environment = { CGO_ENABLED: '0', GOENV: 'off', GOWORK: 'off', GO111MODULE: 'off' };
  for (const name of ['PATH', 'HOME', 'USERPROFILE', 'LOCALAPPDATA', 'SystemRoot', 'SYSTEMROOT', 'WINDIR',
    'TMPDIR', 'TMP', 'TEMP', 'GOCACHE', 'GOMODCACHE', 'GOTMPDIR', 'GOTOOLCHAIN', 'GOROOT']) {
    if (process.env[name] !== undefined) environment[name] = process.env[name];
  }
  // Only protected verifier code runs. The supplied executable is opened as data.
  const result = spawnSync('go', ['run', path.join(root, '../../scripts/verify-native-binary/main.go'),
    path.resolve(binary), target.targetOS, target.targetArch], {
    env: environment, encoding: 'utf8', maxBuffer: 65536, stdio: ['ignore', 'pipe', 'pipe'],
  });
  if (result.error) throw result.error;
  if (result.status !== 0) throw new Error(`native helper static format verification failed (${result.status})`);
  return JSON.parse(result.stdout);
}

async function regularFile(file, executable = false) {
  const info = await fs.lstat(file);
  if (!info.isFile() || info.size <= 0 || (executable && (info.mode & 0o111) === 0)) {
    throw new Error('native helper input must be a nonempty regular file with the required mode');
  }
  return info;
}

export async function verifyPair(directory, { target: expected, release = false, identity = sourceIdentity, coinstalled = false } = {}) {
  if (!(await fs.lstat(directory)).isDirectory()) throw new Error('native helper input must be a regular directory');
  const receiptPath = path.join(directory, receiptName);
  const info = await regularFile(receiptPath);
  if (info.size > 8192) throw new Error('native helper receipt exceeds the installation limit');
  const bytes = await fs.readFile(receiptPath);
  if (bytes.length > 8192) throw new Error('native helper receipt exceeds the installation limit');
  const receipt = JSON.parse(bytes.toString('utf8'));
  const target = validateReceipt(receipt, identity);
  if ((expected && target.key !== expected.key) || (release && receipt.profile !== 'release')) {
    throw new Error('native helper input does not match the required target and release profile');
  }
  if (release && bytes.toString('utf8') !== `${JSON.stringify(receipt, null, 2)}\n`) {
    throw new Error('release helper receipt must use the canonical builder encoding');
  }
  const entries = (await fs.readdir(directory)).sort();
  if (!coinstalled && JSON.stringify(entries) !== JSON.stringify([target.binaryName, receiptName].sort())) {
    throw new Error('native helper pair must contain exactly its binary and receipt');
  }
  const binary = path.join(directory, target.binaryName);
  const binaryInfo = await regularFile(binary, target.targetOS !== 'windows');
  const binarySHA256 = await fileSHA256(binary);
  if (binarySHA256 !== receipt.binarySHA256) throw new Error('native helper bytes do not match their receipt');
  const format = verifyBinaryFormat(binary, target);
  return {
    target: target.key, receipt, receiptSHA256: createHash('sha256').update(bytes).digest('hex'),
    binarySize: binaryInfo.size, binaryMode: binaryInfo.mode & 0o777, format,
  };
}

export function releaseBundleNames(target) {
  return [target.binaryName, receiptName, noticeName, attributionName];
}

export async function verifyReleaseBundle(directory, { target, identity = sourceIdentity, originalReceipt } = {}) {
  const pair = await verifyPair(directory, { target, identity, release: true, coinstalled: true });
  const actualTarget = nativeTarget(pair.receipt.targetOS, pair.receipt.targetArch);
  if (JSON.stringify((await fs.readdir(directory)).sort()) !== JSON.stringify(releaseBundleNames(actualTarget).sort())) {
    throw new Error('native release bundle must contain exactly its binary, receipt and notice artifacts');
  }
  const noticeReceipt = originalReceipt ?? pair.receipt;
  validateReceipt(noticeReceipt, identity);
  if (JSON.stringify({ ...noticeReceipt, binarySHA256: pair.receipt.binarySHA256 }) !== JSON.stringify(pair.receipt)) {
    throw new Error('native release notices changed their original build identity');
  }
  return { ...pair, notices: await verifyNoticeBundle(directory, noticeReceipt, { coinstalled: true }) };
}

export async function verifyReleaseInputs(directory, { identity = sourceIdentity, originals } = {}) {
  if (!(await fs.lstat(directory)).isDirectory()) throw new Error('native helper set must be a regular directory');
  const entries = (await fs.readdir(directory)).sort();
  if (JSON.stringify(entries) !== JSON.stringify(nativeTargets.map((item) => item.key).sort())) {
    throw new Error('native release inputs must contain exactly all six target directories');
  }
  const inputs = [];
  for (const target of nativeTargets) {
    const original = originals?.find((item) => item.target === target.key);
    if (originals && !original) throw new Error('native final release bundle is missing its original build record');
    inputs.push(await verifyReleaseBundle(path.join(directory, target.key), { target, identity, originalReceipt: original?.receipt }));
  }
  return inputs;
}

export async function newOutputOutside(input, output) {
  input = await fs.realpath(input);
  output = path.join(await fs.realpath(path.dirname(path.resolve(output))), path.basename(output));
  const relative = path.relative(input, output);
  if (relative === '' || (!relative.startsWith(`..${path.sep}`) && relative !== '..' && !path.isAbsolute(relative))) {
    throw new Error('native artifact output must be outside its immutable input');
  }
  return output;
}

export async function stageReleaseInputs({ input, output, identity = sourceIdentity }) {
  const original = await verifyReleaseInputs(input, { identity });
  output = await newOutputOutside(input, output);
  await fs.mkdir(output, { mode: 0o700 });
  try {
    for (const target of nativeTargets) {
      const destination = path.join(output, target.key);
      await fs.mkdir(destination, { mode: 0o700 });
      for (const name of releaseBundleNames(target)) {
        await fs.copyFile(path.join(input, target.key, name), path.join(destination, name));
      }
    }
    const staged = await verifyReleaseInputs(output, { identity });
    if (JSON.stringify(staged) !== JSON.stringify(original) ||
        JSON.stringify(await verifyReleaseInputs(input, { identity })) !== JSON.stringify(original)) {
      throw new Error('native build inputs changed during the producer handoff');
    }
    return staged;
  } catch (error) {
    try { await fs.rm(output, { recursive: true }); }
    catch (cleanup) { throw new AggregateError([error, cleanup], 'native input staging and cleanup failed'); }
    throw error;
  }
}

export async function assembleReleaseBundle({ input, notices, output, identity = sourceIdentity }) {
  const pair = await verifyPair(input, { release: true, identity });
  const attribution = await verifyNoticeBundle(notices, pair.receipt);
  const original = { ...pair, notices: attribution };
  const target = nativeTarget(pair.receipt.targetOS, pair.receipt.targetArch);
  output = await newOutputOutside(input, output);
  output = await newOutputOutside(notices, output);
  await fs.mkdir(output, { mode: 0o700 });
  try {
    for (const name of releaseBundleNames(target)) {
      const source = name === target.binaryName || name === receiptName ? input : notices;
      await fs.copyFile(path.join(source, name), path.join(output, name));
    }
    const assembled = await verifyReleaseBundle(output, { target, identity });
    const rechecked = { ...await verifyPair(input, { release: true, identity }),
      notices: await verifyNoticeBundle(notices, pair.receipt) };
    if (JSON.stringify(assembled) !== JSON.stringify(original) || JSON.stringify(rechecked) !== JSON.stringify(original)) {
      throw new Error('native build or attribution inputs changed during bundle assembly');
    }
    return assembled;
  } catch (error) {
    try { await fs.rm(output, { recursive: true }); }
    catch (cleanup) { throw new AggregateError([error, cleanup], 'native bundle assembly and cleanup failed'); }
    throw error;
  }
}

// The protected packager verifies signing before calling this. Receipt rebinding
// is a byte-consistency operation, not signature verification or authentication.
export async function finalizeDarwinReleaseBundle({ input, signedBinary, output, identity = sourceIdentity }) {
  const original = await verifyReleaseBundle(input, { identity });
  const target = nativeTarget(original.receipt.targetOS, original.receipt.targetArch);
  if (target.targetOS !== 'darwin') throw new Error('only Darwin signing changes native release helper bytes');
  output = await newOutputOutside(input, output);
  await regularFile(signedBinary, true);
  verifyBinaryFormat(signedBinary, target);
  const signedSHA256 = await fileSHA256(signedBinary);
  await fs.mkdir(output);
  try {
    const binary = path.join(output, target.binaryName);
    await fs.copyFile(signedBinary, binary);
    await fs.chmod(binary, 0o755);
    const receipt = { ...original.receipt, binarySHA256: signedSHA256 };
    await fs.writeFile(path.join(output, receiptName), `${JSON.stringify(receipt, null, 2)}\n`, { flag: 'wx', mode: 0o644 });
    for (const name of [noticeName, attributionName]) await fs.copyFile(path.join(input, name), path.join(output, name));
    const finalized = await verifyReleaseBundle(output, { target, identity, originalReceipt: original.receipt });
    if (JSON.stringify(finalized.notices) !== JSON.stringify(original.notices) ||
        JSON.stringify(await verifyReleaseBundle(input, { target, identity })) !== JSON.stringify(original)) {
      throw new Error('native helper build input changed while finalizing the installation');
    }
    return { original, finalized };
  } catch (error) {
    try { await fs.rm(output, { recursive: true }); }
    catch (cleanup) { throw new AggregateError([error, cleanup], 'native helper finalization and cleanup failed'); }
    throw error;
  }
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    const { positionals, values } = parseArgs({ allowPositionals: true, options: {
      input: { type: 'string' }, output: { type: 'string' }, manifest: { type: 'string' }, notices: { type: 'string' }, 'signed-binary': { type: 'string' },
    } });
    if (positionals.length !== 1 || !values.input) throw new Error('usage: artifacts.mjs verify-set|stage-set|assemble-bundle|finalize-darwin|verify-version --input PATH [--output NEW_DIR] [--notices NOTICE_DIR] [--manifest PINNED_SOURCE_MANIFEST] [--signed-binary FILE]');
    const identity = values.manifest ? identityFromManifest(JSON.parse(await fs.readFile(values.manifest, 'utf8'))) : sourceIdentity;
    let result;
    if (positionals[0] === 'verify-version') {
      if ((await regularFile(values.input)).size > 65536) throw new Error('native version output exceeds its limit');
      verifySourceVersion(JSON.parse(await fs.readFile(values.input, 'utf8')), identity);
      result = { verified: true };
    }
    else if (positionals[0] === 'verify-set') result = await verifyReleaseInputs(values.input, { identity });
    else if (positionals[0] === 'stage-set' && values.output) result = await stageReleaseInputs({ input: values.input, output: values.output, identity });
    else if (positionals[0] === 'assemble-bundle' && values.output && values.notices) result = await assembleReleaseBundle({ input: values.input, notices: values.notices, output: values.output, identity });
    else if (positionals[0] === 'finalize-darwin' && values.output && values['signed-binary']) {
      result = await finalizeDarwinReleaseBundle({ input: values.input, output: values.output, signedBinary: values['signed-binary'], identity });
    } else throw new Error('unknown or incomplete native artifact operation');
    process.stdout.write(`${JSON.stringify(result, null, 2)}\n`);
  } catch (error) {
    console.error(error);
    process.exitCode = 1;
  }
}
