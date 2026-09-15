import crypto from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawnSync } from 'node:child_process';
import { parseArgs } from 'node:util';
import { verifyPreparedSource } from './materialize.mjs';
import { verifyPair, fileSHA256 } from './artifacts.mjs';
import { noticeName, attributionName } from './notice-artifacts.mjs';

const root = path.dirname(fileURLToPath(import.meta.url));
const maximumText = 1024 * 1024;
const maximumTotal = 64 * 1024 * 1024;
const hash = (data) => crypto.createHash('sha256').update(data).digest('hex');
const key = (pkg) => JSON.stringify([pkg.name, pkg.version, pkg.source ?? null]);
const sorted = (values) => values.sort((a, b) => Buffer.compare(Buffer.from(a), Buffer.from(b)));

// Cargo-generated lock v4 uses one-line basic strings for these identity fields.
// Unsupported representations fail rather than attempting a general TOML parse.
export function lockedPackages(text) {
  if (!/^version = 4\s*$/m.test(text.split('[[package]]')[0])) throw new Error('notice collection requires Cargo lock format 4');
  const records = [];
  let current;
  for (const line of text.split('\n')) {
    if (line === '[[package]]') { current = {}; records.push(current); continue; }
    if (line.startsWith('[')) { current = undefined; continue; }
    if (!current) continue;
    const field = /^(name|version|source|checksum)\s*=\s*(.*)$/.exec(line);
    if (!field) continue;
    if (Object.hasOwn(current, field[1]) || !/^"(?:[^"\\]|\\.)*"$/.test(field[2])) throw new Error('unsupported Cargo lock identity field');
    current[field[1]] = JSON.parse(field[2]);
  }
  const result = new Map();
  for (const record of records) {
    if (!record.name || !record.version || result.has(key(record))) throw new Error('Cargo lock package identity is not unique');
    result.set(key(record), record);
  }
  return result;
}

export function attributionPath(relative) {
  return /(?:^|[-_.])(?:licen[cs]e|unlicense|notice|notices|copying|copyright)(?:$|[-_.])/i.test(path.posix.basename(relative));
}

export function hasCurrentAttribution(materials) {
  return materials.some((item) => item.kind === 'package-file' || item.kind === 'upstream-license');
}

function childRelative(parent, file) {
  const relative = path.relative(parent, file);
  if (!relative || relative === '..' || relative.startsWith(`..${path.sep}`) || path.isAbsolute(relative)) throw new Error('notice input is outside its declared source root');
  return relative.split(path.sep).join('/');
}

function tar(args, limit) {
  const result = spawnSync('tar', args, { encoding: null, maxBuffer: limit, stdio: ['ignore', 'pipe', 'pipe'] });
  if (result.error) throw result.error;
  if (result.status !== 0) throw new Error(`cannot read locked package archive ${path.basename(args[1])}${args.length > 2 ? ` member ${JSON.stringify(args.at(-1))}` : ''} (tar exit ${result.status})`);
  return result.stdout;
}

export function archiveListingNames(listing) {
  return listing.split(/\r?\n/).filter(Boolean);
}

export function parseArchiveMembers(listing, prefix) {
  const names = archiveListingNames(listing);
  const files = [];
  for (const name of names) {
    if (name.endsWith('/')) continue;
    if (!name.startsWith(`${prefix}/`) || name.split('/').some((part) => !part || part === '.' || part === '..')) throw new Error('unexpected locked crate archive path');
    files.push(name.slice(prefix.length + 1));
  }
  return sorted(files);
}

export function archiveMembers(archive, prefix) {
  return parseArchiveMembers(tar(['-tzf', archive], 8 * 1024 * 1024).toString('utf8'), prefix);
}

export function archiveFile(archive, prefix, relative) {
  return tar(['-xOf', archive, '--', `${prefix}/${relative}`], maximumText);
}

function sourceFiles(directory) {
  const files = [];
  function walk(current) {
    for (const entry of fs.readdirSync(current, { withFileTypes: true })) {
      const file = path.join(current, entry.name);
      if (entry.isDirectory()) walk(file);
      else if (entry.isFile()) files.push(childRelative(directory, file));
    }
  }
  walk(directory);
  return sorted(files);
}

export function renderNotices(report, materialText) {
  let text = `Crabbox native JJ helper: observed build attribution\n\n${report.scope}\nSource tree: ${report.sourceTreeSHA256}\nHelper: ${report.helperSHA256}\nTarget: ${report.targetOS}/${report.targetArch}\nUnresolved packages: ${report.unresolved.length}\n`;
  for (const item of report.unresolved) text += `Unresolved: ${item.name} ${item.version}: ${item.reason}\n`;
  for (const record of report.packages) {
    text += `\n============================================================\n${record.name} ${record.version}\nDeclared license: ${record.declaredLicense ?? 'not declared'}\nSource: ${JSON.stringify(record.origin)}\n`;
    for (const material of record.materials) {
      text += `Attribution: ${material.path} (${material.kind}; SHA-256 ${material.sha256})${material.url ? `\nUpstream: ${material.url}` : ''}\n`;
      if (material.kind === 'upstream-historical-license') text += `Historical notice for package revision ${material.packageCommit}; does not resolve current attribution.\n`;
    }
  }
  for (const sha256 of sorted([...materialText.keys()])) text += `\n============================================================\nVerbatim attribution text SHA-256 ${sha256}\n\n${materialText.get(sha256)}\n`;
  return text;
}

export async function collectNotices({ source, buildReport, metadata, pair, output }) {
  source = fs.realpathSync(source);
  const manifest = JSON.parse(fs.readFileSync(path.join(root, 'manifest.json'), 'utf8'));
  const verifySource = () => verifyPreparedSource(source, manifest);
  await verifySource();
  const build = JSON.parse(fs.readFileSync(buildReport, 'utf8'));
  const dictionary = JSON.parse(fs.readFileSync(metadata, 'utf8'));
  const installation = await verifyPair(pair, { release: true });
  for (const field of Object.keys(installation.receipt)) {
    if (build[field] !== installation.receipt[field]) throw new Error('build report does not match the native input pair');
  }
  if (!Array.isArray(build.buildUnits) || build.buildUnits.length === 0) throw new Error('observed Cargo build inputs are missing');
  const packages = new Map(dictionary.packages.map((pkg) => [pkg.id, pkg]));
  if (packages.size !== dictionary.packages.length) throw new Error('package dictionary contains duplicate identities');
  const lock = lockedPackages(fs.readFileSync(path.join(source, 'Cargo.lock'), 'utf8'));
  const supplements = JSON.parse(fs.readFileSync(path.join(root, 'notice-supplements.json'), 'utf8'));
  if (supplements.schemaVersion !== 1 || !Array.isArray(supplements.sources)) throw new Error('unsupported notice supplement manifest');
  const selected = sorted([...new Set(build.buildUnits.map((unit) => unit.packageId))]);
  const records = [];
  const materialText = new Map();
  const unresolved = [];
  let total = 0;
  for (const id of selected) {
    const pkg = packages.get(id);
    if (!pkg) throw new Error('observed Cargo package is absent from the package dictionary');
    const locked = lock.get(key(pkg));
    if (!locked) throw new Error(`observed package is absent from the lockfile: ${pkg.name}`);
    const directory = fs.realpathSync(path.dirname(pkg.manifest_path));
    let paths, read, origin;
    if (pkg.source?.startsWith('registry+')) {
      const registry = path.dirname(path.dirname(path.dirname(directory)));
      const archive = path.join(registry, 'cache', path.basename(path.dirname(directory)), `${pkg.name}-${pkg.version}.crate`);
      if (!/^[0-9a-f]{64}$/.test(locked.checksum ?? '') || await fileSHA256(archive) !== locked.checksum) throw new Error(`crate archive checksum mismatch: ${pkg.name}`);
      const prefix = `${pkg.name}-${pkg.version}`;
      paths = archiveMembers(archive, prefix);
      read = (relative) => archiveFile(archive, prefix, relative);
      if (!read('Cargo.toml').equals(fs.readFileSync(pkg.manifest_path))) throw new Error(`package dictionary manifest differs from the locked archive: ${pkg.name}`);
      origin = { kind: 'registry', source: pkg.source, archiveSHA256: locked.checksum };
    } else if (pkg.source == null) {
      const relative = childRelative(source, pkg.manifest_path);
      paths = sourceFiles(directory);
      read = (file) => fs.readFileSync(path.join(directory, file));
      origin = { kind: 'prepared-source', manifest: relative, sourceTreeSHA256: manifest.sourceTree.sha256 };
    } else throw new Error(`unsupported observed package source: ${pkg.name}`);
    const admitted = new Set(paths.filter(attributionPath));
    if (pkg.license_file) admitted.add(childRelative(directory, path.resolve(directory, pkg.license_file)));
    const materials = [];
    for (const relative of sorted([...admitted])) {
      const bytes = read(relative);
      total += bytes.length;
      if (!bytes.length || bytes.length > maximumText || total > maximumTotal) throw new Error('attribution text exceeds collection limits or is empty');
      const text = new TextDecoder('utf-8', { fatal: true, ignoreBOM: true }).decode(bytes);
      const sha256 = hash(bytes);
      materials.push({ path: relative, sha256, bytes: bytes.length, kind: 'package-file' });
      materialText.set(sha256, text);
    }
    for (const supplement of supplements.sources) {
      const match = supplement.packages.find((item) => item.name === pkg.name && item.version === pkg.version);
      if (!match) continue;
      const vcs = JSON.parse(read('.cargo_vcs_info.json').toString('utf8'));
      const historical = supplement.kind === 'historical-license';
      const packageCommit = historical ? match.commit : supplement.commit;
      if (pkg.repository?.replace(/\.git$/, '') !== supplement.repository || vcs.git?.sha1 !== packageCommit ||
          !/^[0-9a-f]{40}$/.test(supplement.commit) ||
          (historical && (!/^[0-9a-f]{40}$/.test(packageCommit ?? '') || packageCommit === supplement.commit)) ||
          (!historical && match.commit !== undefined) ||
          vcs.path_in_vcs !== match.vcsPath || !['overview', 'license', 'historical-license'].includes(supplement.kind)) {
        throw new Error(`notice supplement does not match the crate's recorded origin: ${pkg.name}`);
      }
      const file = fs.realpathSync(path.join(root, supplement.file));
      childRelative(root, file);
      const bytes = fs.readFileSync(file);
      total += bytes.length;
      if (!bytes.length || bytes.length > maximumText || total > maximumTotal || hash(bytes) !== supplement.sha256) throw new Error('notice supplement checksum or size mismatch');
      materialText.set(supplement.sha256, new TextDecoder('utf-8', { fatal: true, ignoreBOM: true }).decode(bytes));
      materials.push({ path: supplement.upstreamPath, sha256: supplement.sha256, bytes: bytes.length,
        kind: `upstream-${supplement.kind}`, url: `${supplement.repository}/blob/${supplement.commit}/${supplement.upstreamPath}`,
        ...(historical ? { packageCommit } : {}) });
    }
    // Historical notices preserve known attribution without clearing current gaps.
    if (!hasCurrentAttribution(materials)) unresolved.push({ name: pkg.name, version: pkg.version,
      reason: materials.length ? 'upstream-overview-without-license-text' : 'no-packaged-attribution-text' });
    const featureSets = [...new Set(build.buildUnits.filter((unit) => unit.packageId === id).map((unit) => JSON.stringify(unit.features)))].sort().map(JSON.parse);
    records.push({ name: pkg.name, version: pkg.version, declaredLicense: pkg.license, repository: pkg.repository,
      origin, observedFeatureSets: featureSets, materials });
  }
  records.sort((a, b) => Buffer.compare(Buffer.from(key(a)), Buffer.from(key(b))));
  await verifySource();
  const report = { schemaVersion: 1, scope: 'Attribution-file candidates from observed Cargo build inputs; not exact linkage or legal clearance. Native/system library attribution requires separate review. Upstream asset-bundling statements describe their projects, not necessarily this executable.',
    sourceTreeSHA256: manifest.sourceTree.sha256, helperSHA256: build.binarySHA256,
    targetOS: build.targetOS, targetArch: build.targetArch, packages: records, unresolved };
  const text = renderNotices(report, materialText);
  report.notice = { name: noticeName, sha256: hash(text), size: Buffer.byteLength(text) };
  output = path.resolve(output);
  const parent = fs.realpathSync(path.dirname(output));
  output = path.join(parent, path.basename(output));
  const relative = path.relative(source, output);
  if (relative === '' || (relative !== '..' && !relative.startsWith(`..${path.sep}`) && !path.isAbsolute(relative))) throw new Error('notice output must be outside the prepared source');
  fs.mkdirSync(output, { mode: 0o700 });
  try {
    fs.writeFileSync(path.join(output, noticeName), text, { flag: 'wx' });
    fs.writeFileSync(path.join(output, attributionName), JSON.stringify(report, null, 2) + '\n', { flag: 'wx' });
  } catch (error) {
    try { fs.rmSync(output, { recursive: true }); } catch (cleanup) { throw new AggregateError([error, cleanup], 'notice output and cleanup failed'); }
    throw error;
  }
  return { directory: output, packages: records.length, unresolved, noticeSHA256: hash(text) };
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    const { values } = parseArgs({ options: { source: { type: 'string' }, 'build-report': { type: 'string' }, metadata: { type: 'string' }, pair: { type: 'string' }, output: { type: 'string' } } });
    if (Object.keys(values).length !== 5) throw new Error('usage: notices.mjs --source PREPARED --build-report BUILD_JSON --metadata CARGO_JSON --pair NATIVE_PAIR --output NEW_DIRECTORY');
    process.stdout.write(JSON.stringify(await collectNotices({ ...values, buildReport: values['build-report'] }), null, 2) + '\n');
  } catch (error) { console.error(error); process.exitCode = 1; }
}
