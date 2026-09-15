#!/usr/bin/env node
import assert from 'node:assert/strict';
import * as fs from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { parseArgs } from 'node:util';
import { spawnSync } from 'node:child_process';
import { fileSHA256, nativeTarget, verifyPair, verifySourceVersion } from './artifacts.mjs';
import { isolatedGitConfig } from './materialize.mjs';

async function runtimeSourceSnapshot(root) {
  const records = [];
  async function visit(file, relative) {
    const info = await fs.lstat(file, { bigint: true });
    const record = { path: relative, mode: Number(info.mode), mtimeNs: info.mtimeNs.toString() };
    if (info.isDirectory()) {
      records.push({ ...record, kind: 'directory' });
      for (const name of (await fs.readdir(file)).sort()) await visit(path.join(file, name), relative ? `${relative}/${name}` : name);
    } else if (info.isSymbolicLink()) records.push({ ...record, kind: 'symlink', target: await fs.readlink(file) });
    else if (info.isFile()) records.push({ ...record, kind: 'file', sha256: await fileSHA256(file) });
    else throw new Error('unexpected fixture entry kind');
  }
  await visit(root, '');
  return records;
}

// Compare materialization with the host's stock JJ checkout, not POSIX mode
// assumptions. Remote SSH lifecycle remains a separate fixture.
export async function smoke({ jj, crabbox, output }) {
  jj = await fs.realpath(jj);
  crabbox = await fs.realpath(crabbox);
  const target = nativeTarget(process.platform === 'win32' ? 'windows' : process.platform,
    process.arch === 'x64' ? 'amd64' : process.arch);
  const pair = await verifyPair(path.dirname(crabbox), { target, release: true, coinstalled: true });
  const helper = path.join(path.dirname(crabbox), target.binaryName);
  output = path.resolve(output);
  await fs.mkdir(output, { mode: 0o700 });
  output = await fs.realpath(output);
  const source = path.join(output, 'source');
  const home = path.join(output, 'home');
  const temp = path.join(output, 'temp');
  await fs.mkdir(home);
  await fs.mkdir(temp);
  const config = path.join(home, 'jj.toml');
  await fs.writeFile(config, '[user]\nname = "Native smoke"\nemail = "smoke@example.invalid"\n');
  const emptyGitConfig = path.join(home, 'empty-git-config');
  await fs.writeFile(emptyGitConfig, '', { flag: 'wx', mode: 0o600 });
  const env = {};
  for (const name of ['PATH', 'SystemRoot', 'SYSTEMROOT', 'WINDIR']) {
    if (process.env[name] !== undefined) env[name] = process.env[name];
  }
  Object.assign(env, isolatedGitConfig(emptyGitConfig), { HOME: home, USERPROFILE: home, APPDATA: home, LOCALAPPDATA: home,
    XDG_CONFIG_HOME: home, XDG_CACHE_HOME: path.join(output, 'cache'),
    XDG_STATE_HOME: path.join(output, 'state'), JJ_CONFIG: config,
    GOENV: 'off', GOWORK: 'off', GOTOOLCHAIN: 'local',
    TMPDIR: temp, TEMP: temp, TMP: temp, LANG: 'C', LC_ALL: 'C' });
  const run = (binary, args, cwd = source) => {
    const result = spawnSync(binary, args, { cwd, env, encoding: 'utf8', timeout: 180_000, maxBuffer: 4 * 1024 * 1024 });
    if (result.error) throw result.error;
    assert.equal(result.status, 0, `${path.basename(binary)} ${args[0]}: ${result.stderr}`);
    return result.stdout;
  };
  const fileInfoOracle = path.join(output, `fixture-file-info${process.platform === 'win32' ? '.exe' : ''}`);
  run('go', ['build', '-trimpath', '-o', fileInfoOracle,
    fileURLToPath(new URL('./fixture-file-info.go', import.meta.url))], output);
  const nativeFileInfo = async (root, paths, label) => {
    const request = path.join(output, `${label}-file-info-request.json`);
    await fs.writeFile(request, JSON.stringify(paths), { flag: 'wx' });
    const entries = JSON.parse(run(fileInfoOracle, [root, request], output));
    assert.deepEqual(entries.map((entry) => entry.path), paths);
    assert.ok(entries.every((entry) => Number.isSafeInteger(entry.bytes) && entry.bytes >= 0));
    await fs.writeFile(path.join(output, `${label}-file-info.json`), JSON.stringify(entries, null, 2) + '\n', { flag: 'wx' });
    return entries;
  };
  let attributeRequest = 0;
  const windowsAttributes = async (root, relativePaths) => {
    if (process.platform !== 'win32') return null;
    const input = path.join(output, `attributes-${++attributeRequest}.json`);
    await fs.writeFile(input, JSON.stringify(relativePaths), { flag: 'wx' });
    const powershell = path.join(env.SystemRoot ?? env.SYSTEMROOT, 'System32', 'WindowsPowerShell', 'v1.0', 'powershell.exe');
    const script = fileURLToPath(new URL('./windows-file-attributes.ps1', import.meta.url));
    const rows = JSON.parse(run(powershell, ['-NoLogo', '-NoProfile', '-NonInteractive', '-File', script, '-Root', root, '-PathsFile', input], output));
    assert.deepEqual(rows.map((item) => item.path), relativePaths);
    return new Map(rows.map((item) => [item.path, item.attributes]));
  };
  const sourceSnapshot = async () => {
    const rows = await runtimeSourceSnapshot(source);
    const attributes = await windowsAttributes(source, rows.map((item) => item.path));
    return rows.map((item) => attributes ? { ...item, windowsAttributes: attributes.get(item.path) } : item);
  };
  const stock = (...args) => run(jj, ['--no-pager', '--color=never', ...args]);
  const jjVersion = run(jj, ['--version'], output).trim();
  assert.match(jjVersion, new RegExp(`^jj ${pair.receipt.nativeVersion.replaceAll('.', '\\.')}([ -]|$)`));
  run(jj, ['git', 'init', '--no-colocate', source], output);
  const recorded = 'Recorded UTF-8: café\n';
  const live = 'Live UTF-8: café\n';
  const pending = 'New ordinary file\n';
  const paths = ['data/value.txt', 'recorded.txt', 'tool.sh', 'value-link'];
  await fs.mkdir(path.join(source, 'data'));
  await fs.writeFile(path.join(source, 'data/value.txt'), 'target-A\n');
  await fs.writeFile(path.join(source, 'recorded.txt'), recorded);
  await fs.writeFile(path.join(source, 'tool.sh'), '#!/bin/sh\nprintf "fixture-A\\n"\n');
  await fs.symlink('data/value.txt', path.join(source, 'value-link'), 'file');
  stock('file', 'chmod', 'x', 'tool.sh');
  stock('new', '-m', 'live workspace');
  stock('bookmark', 'create', 'native-smoke', '-r', '@-');
  const oracle = path.join(output, 'stock-checkout');
  stock('workspace', 'add', '--name', 'oracle', '--revision', 'native-smoke', oracle);
  // Finish stock setup before freezing source/admin invariance measurements.
  stock('status');
  const commit = stock('log', '--no-graph', '-r', 'native-smoke', '-T', 'commit_id').trim();
  const recordedTypes = stock('file', 'list', '-r', 'native-smoke', '-T',
    'path ++ "\\t" ++ file_type ++ "\\t" ++ executable ++ "\\n"').trim().split('\n');
  assert.ok(recordedTypes.includes('tool.sh\tfile\ttrue'));
  assert.ok(recordedTypes.includes('value-link\tsymlink\tfalse'));
  const materializedFiles = async (root, selectedPaths) => {
    const attributes = await windowsAttributes(root, selectedPaths);
    return Promise.all(selectedPaths.map(async (name) => {
      const file = path.join(root, name);
      const stat = await fs.lstat(file);
      const kind = stat.isSymbolicLink() ? 'symlink' : stat.isFile() ? 'file' : 'other';
      assert.notEqual(kind, 'other', `${name} is not file-like`);
      return { path: name, kind, mode: stat.mode & 0o777,
        ...(attributes ? { windowsAttributes: attributes.get(name) } : {}),
        ...(kind === 'symlink' ? { target: await fs.readlink(file) } : { bytes: stat.size, sha256: await fileSHA256(file) }) };
    }));
  };
  const oracleFiles = await materializedFiles(oracle, paths);
  await fs.writeFile(path.join(source, 'recorded.txt'), live);
  await fs.writeFile(path.join(source, 'data/value.txt'), 'target-B\n');
  await fs.writeFile(path.join(source, 'pending.txt'), pending);
  const before = await sourceSnapshot();
  const beforeFile = path.join(output, 'source-before.json');
  await fs.writeFile(beforeFile, JSON.stringify(before, null, 2) + '\n', { flag: 'wx' });
  const read = (...args) => JSON.parse(run(helper, ['--no-pager', '--color=never',
    '--max-object-allocation-bytes', '16777216', '--max-input-bytes', '16777216',
    '--max-conflict-scratch-bytes', '16777216', ...args]));
  const version = read('source-version');
  verifySourceVersion(version);
  const context = read('source-context');
  await fs.writeFile(path.join(output, 'context.json'), JSON.stringify(context, null, 2) + '\n', { flag: 'wx' });
  const inventory = read('--at-op', context.operation_heads[0], 'source-recorded-inventory', '--revision', 'native-smoke');
  assert.equal(inventory.identity.commit, commit);
  assert.deepEqual(inventory.entries.map((item) => item.path).sort(), paths);
  const liveInventory = read('--at-op', context.operation_heads[0], 'source-live-inventory');
  assert.equal(liveInventory.identity.operation, context.operation_heads[0]);
  const selected = path.join(output, 'selected.json');
  await fs.writeFile(selected, JSON.stringify({ protocol: 'crabbox-jj-source', schema_version: 1,
    identity: inventory.identity, paths }));
  const payload = path.join(output, 'payload');
  const checkout = path.join(output, 'checkout');
  await fs.mkdir(payload);
  await fs.mkdir(checkout);
  const exported = read('--at-op', inventory.identity.operation, 'source-recorded-export',
    '--selected', selected, '--output', payload, '--state', checkout,
    '--max-entry-bytes', '1048576', '--max-output-bytes', '1048576');
  await fs.writeFile(path.join(output, 'export.json'), JSON.stringify(exported, null, 2) + '\n', { flag: 'wx' });
  assert.deepEqual(exported.identity, inventory.identity);
  assert.deepEqual((await fs.readdir(payload)).sort(), ['data', 'recorded.txt', 'tool.sh', 'value-link']);
  assert.deepEqual(await fs.readdir(path.join(payload, 'data')), ['value.txt']);
  assert.deepEqual(await materializedFiles(payload, paths), oracleFiles, 'helper materialization differs from stock native JJ');
  assert.equal(await fs.readFile(path.join(payload, 'recorded.txt'), 'utf8'), recorded);
  const cliConfig = path.join(output, 'crabbox.json');
  await fs.writeFile(cliConfig, JSON.stringify({ provider: 'ssh', target: 'linux',
    sync: { source: 'jj', include: [...paths, 'pending.txt'] }, results: { auto: false } }));
  env.CRABBOX_CONFIG = cliConfig;
  const plan = (revision) => JSON.parse(run(crabbox, ['sync-plan', '--sync-source', 'jj',
    '--sync-revision', revision, '--json']));
  const recordedPlan = plan('native-smoke');
  await fs.writeFile(path.join(output, 'recorded-plan.json'), JSON.stringify(recordedPlan, null, 2) + '\n', { flag: 'wx' });
  assert.equal(recordedPlan.jujutsu.recordedCommit, commit);
  // Go's Lstat size estimate differs from libuv's symlink-target length on Windows.
  // Link contents and materialization are independently checked above.
  const recordedFileInfo = await nativeFileInfo(oracle, paths, 'recorded');
  const estimatedBytes = (files) => files.reduce((sum, file) => sum + file.bytes, 0);
  assert.equal(recordedPlan.candidate.files, paths.length);
  assert.equal(recordedPlan.candidate.bytes, estimatedBytes(recordedFileInfo));
  const livePlan = plan('');
  await fs.writeFile(path.join(output, 'live-plan.json'), JSON.stringify(livePlan, null, 2) + '\n', { flag: 'wx' });
  const liveFiles = await materializedFiles(source, [...paths, 'pending.txt']);
  const liveFileInfo = await nativeFileInfo(source, [...paths, 'pending.txt'], 'live');
  assert.equal(livePlan.candidate.files, paths.length + 1);
  assert.equal(livePlan.candidate.bytes, estimatedBytes(liveFileInfo));
  assert.notEqual(recordedPlan.jujutsu.contentSha256, livePlan.jujutsu.contentSha256);
  const after = await sourceSnapshot();
  await fs.writeFile(path.join(output, 'source-after.json'), JSON.stringify(after, null, 2) + '\n', { flag: 'wx' });
  assert.deepEqual(after, before, 'native source or administration files changed during reads');
  const receipt = { schemaVersion: 1, scope: 'Native file/executable/symlink materialization compared with stock JJ and installed CLI plans; not SSH lifecycle or release acceptance.',
    node: process.version, platform: process.platform, architecture: process.arch, jjVersion,
    cliSHA256: await fileSHA256(crabbox), helper: pair,
    sourceUnchanged: true, sourceSnapshotSHA256: await fileSHA256(beforeFile), recordedExportMatchesFixture: true, stockMaterializationMatches: true,
    recordedTypes, oracleFiles, liveFiles, recordedFileInfo, liveFileInfo, recordedPlan, livePlan, exportCapabilities: exported.capabilities };
  await fs.writeFile(path.join(output, 'receipt.json'), JSON.stringify(receipt, null, 2) + '\n', { flag: 'wx' });
  return receipt;
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    const { values } = parseArgs({ options: { jj: { type: 'string' }, crabbox: { type: 'string' }, output: { type: 'string' } } });
    if (!values.jj || !values.crabbox || !values.output) throw new Error('usage: smoke.mjs --jj STOCK_JJ --crabbox INSTALLED_CLI --output NEW_DIRECTORY');
    process.stdout.write(JSON.stringify(await smoke(values), null, 2) + '\n');
  } catch (error) { console.error(error); process.exitCode = 1; }
}
