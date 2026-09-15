#!/usr/bin/env node
import { createHash } from 'node:crypto';
import { createReadStream } from 'node:fs';
import * as fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { parseArgs } from 'node:util';
import { spawnSync } from 'node:child_process';

const packageRoot = path.dirname(fileURLToPath(import.meta.url));

async function digest(file) {
  const hash = createHash('sha256');
  for await (const data of createReadStream(file)) hash.update(data);
  return hash.digest('hex');
}

function packagePath(relative) {
  if (path.isAbsolute(relative) || relative.split(/[\\/]/).some((part) => part === '..' || part === '')) {
    throw new Error('source package contains an invalid relative path');
  }
  return path.join(packageRoot, relative);
}

async function verify(file, expected) {
  if ((await digest(file)) !== expected) throw new Error(`source package checksum mismatch: ${file}`);
}

export function isolatedGitConfig(empty) {
  // An owned empty file works across Git ports that disagree on null devices.
  if (!path.isAbsolute(empty)) throw new Error('Git isolation requires an absolute owned config path');
  return { GIT_CONFIG_NOSYSTEM: '1', GIT_CONFIG_GLOBAL: empty, GIT_CONFIG_SYSTEM: empty,
    GIT_CONFIG_COUNT: '0', GIT_TERMINAL_PROMPT: '0' };
}

function command(name, args, cwd, emptyConfig) {
  const env = {};
  for (const key of ['PATH', 'SystemRoot', 'SYSTEMROOT', 'WINDIR', 'TMPDIR', 'TMP', 'TEMP']) {
    if (process.env[key] !== undefined) env[key] = process.env[key];
  }
  Object.assign(env, isolatedGitConfig(emptyConfig), {
    GIT_OPTIONAL_LOCKS: '0',
    LC_ALL: 'C',
  });
  const result = spawnSync(name, args, { cwd, env, encoding: 'utf8', maxBuffer: 65536 });
  if (result.error) throw result.error;
  if (result.status !== 0) throw new Error(`${name} failed (${result.status}): ${result.stderr.trim()}`);
}

export function sourceDigestPath(value, separator = path.sep) {
  return value.split(separator).join('/');
}

export async function sourceTree(root, { includeEntries = false } = {}) {
  const files = [];
  async function walk(directory) {
    for (const entry of await fs.readdir(directory, { withFileTypes: true })) {
      const full = path.join(directory, entry.name);
      const relative = sourceDigestPath(path.relative(root, full));
      if (entry.isDirectory()) await walk(full);
      else if (entry.isFile()) files.push([relative, 'file', await digest(full)]);
      else if (entry.isSymbolicLink()) files.push([relative, 'symlink', sourceDigestPath(await fs.readlink(full))]);
      else throw new Error(`unsupported prepared source entry: ${relative}`);
    }
  }
  await walk(root);
  files.sort((left, right) => Buffer.compare(Buffer.from(left[0]), Buffer.from(right[0])));
  const hash = createHash('sha256');
  for (const row of files) hash.update(`${JSON.stringify(row)}\n`);
  return { algorithm: 'sha256-jsonl-files-v1', sha256: hash.digest('hex'), entryCount: files.length,
    ...(includeEntries ? { entries: files } : {}) };
}

export async function verifyPreparedSource(source, manifest) {
  const actual = await sourceTree(source);
  if (manifest.schemaVersion !== 1 || manifest.sourceTree?.algorithm !== actual.algorithm ||
      manifest.sourceTree.sha256 !== actual.sha256 || manifest.sourceTree.entryCount !== actual.entryCount) {
    throw new Error('prepared native source tree does not match its pinned identity');
  }
  return actual;
}

export async function materialize({ output, jjArchive, gixArchive, inventoryFile }) {
  const manifest = JSON.parse(await fs.readFile(path.join(packageRoot, 'manifest.json'), 'utf8'));
  if (manifest.schemaVersion !== 1 || manifest.sourceTree.algorithm !== 'sha256-jsonl-files-v1') {
    throw new Error('unsupported native source package manifest');
  }
  output = path.resolve(output);
  if (inventoryFile) {
    inventoryFile = path.resolve(inventoryFile);
    const relative = path.relative(output, inventoryFile);
    if (relative === '' || (!relative.startsWith(`..${path.sep}`) && relative !== '..' && !path.isAbsolute(relative))) {
      throw new Error('source inventory must be outside prepared source');
    }
  }
  jjArchive = path.resolve(jjArchive);
  gixArchive = path.resolve(gixArchive);
  await verify(jjArchive, manifest.jj.archiveSha256);
  await verify(gixArchive, manifest.gix.archiveSha256);
  for (const patch of Object.values(manifest.patches)) await verify(packagePath(patch.path), patch.sha256);
  for (const overlay of manifest.overlays) {
    packagePath(overlay.target);
    await verify(packagePath(overlay.path), overlay.sha256);
  }

  let created = false;
  let metadata;
  try {
    // Never reuse or erase an existing destination, including an empty one.
    await fs.mkdir(output);
    created = true;
    metadata = await fs.mkdtemp(path.join(os.tmpdir(), 'crabbox-jj-patch-'));
    const emptyConfig = path.join(metadata, 'empty-git-config');
    await fs.writeFile(emptyConfig, '', { flag: 'wx', mode: 0o600 });
    const run = (name, args, cwd) => command(name, args, cwd, emptyConfig);
    run('git', ['init', '--bare', '--template=', '--quiet', metadata], output);
    run('tar', ['-xzf', jjArchive, '--strip-components=1', '-C', output], output);
    const vendor = path.join(output, 'vendor', 'gix');
    await fs.mkdir(vendor, { recursive: true });
    run('tar', ['-xzf', gixArchive, '--strip-components=1', '-C', vendor], output);
    for (const [name, root] of [['jj', output], ['gix', vendor]]) {
      const patch = packagePath(manifest.patches[name].path);
      const args = ['--git-dir', metadata, '--work-tree', root, '-c', 'core.autocrlf=false', 'apply', '--whitespace=nowarn'];
      run('git', [...args, '--check', patch], root);
      run('git', [...args, patch], root);
    }
    for (const overlay of manifest.overlays) {
      const target = path.join(output, overlay.target);
      await fs.mkdir(path.dirname(target), { recursive: true });
      await fs.copyFile(packagePath(overlay.path), target);
    }
    await fs.rm(metadata, { recursive: true });
    metadata = undefined;
    if (inventoryFile) {
      await fs.writeFile(inventoryFile, JSON.stringify(await sourceTree(output, { includeEntries: true }), null, 2) + '\n', { flag: 'wx', mode: 0o600 });
    }
    const actual = await verifyPreparedSource(output, manifest);
    return { source: output, jjCommit: manifest.jj.commit, jjVersion: manifest.jj.version, gixVersion: manifest.gix.version, sourceTree: actual };
  } catch (error) {
    const errors = [error];
    for (const owned of [metadata, created ? output : undefined]) {
      if (owned === undefined) continue;
      try { await fs.rm(owned, { recursive: true }); }
      catch (cleanup) { errors.push(new Error(`remove owned source state ${owned}`, { cause: cleanup })); }
    }
    if (errors.length === 1) throw error;
    throw new AggregateError(errors, 'native source preparation and cleanup failed');
  }
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    const { values } = parseArgs({ options: {
      output: { type: 'string' }, 'jj-archive': { type: 'string' }, 'gix-archive': { type: 'string' },
    } });
    if (!values.output || !values['jj-archive'] || !values['gix-archive']) {
      throw new Error('usage: node tools/jj-source/materialize.mjs --output NEW_DIRECTORY --jj-archive ARCHIVE --gix-archive ARCHIVE');
    }
    const receipt = await materialize({ output: values.output, jjArchive: values['jj-archive'], gixArchive: values['gix-archive'] });
    process.stdout.write(`${JSON.stringify(receipt, null, 2)}\n`);
  } catch (error) {
    console.error(error);
    process.exitCode = 1;
  }
}
