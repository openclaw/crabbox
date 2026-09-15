#!/usr/bin/env node
import * as fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { parseArgs } from 'node:util';
import { spawnSync } from 'node:child_process';
import { verifyPreparedSource } from './materialize.mjs';
import { nativeTarget, fileSHA256, createBuildReceipt, verifyPair, verifySourceVersion, receiptName, newOutputOutside } from './artifacts.mjs';

const root = path.dirname(fileURLToPath(import.meta.url));

function run(name, args, cwd, env, maxBuffer = 65536) {
  const result = spawnSync(name, args, {
    cwd, env, encoding: 'utf8', maxBuffer,
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  if (result.error) throw result.error;
  if (result.status !== 0) throw new Error(`${name} failed (${result.status})`);
  return result.stdout?.trim() ?? '';
}

export function productionBuildUnits(output) {
  const messages = output.split('\n').filter((line) => line.trim()).map((line) => JSON.parse(line));
  const artifacts = messages.filter((item) => item.reason === 'compiler-artifact');
  const executable = artifacts.filter((item) => item.target?.name === 'crabbox-jj-source' && item.target.kind.includes('bin') && item.executable);
  if (messages.at(-1)?.reason !== 'build-finished' || messages.at(-1).success !== true ||
      executable.length !== 1 || executable[0].profile?.test !== false ||
      JSON.stringify(executable[0].features) !== JSON.stringify(['git'])) {
    throw new Error('Cargo did not report the requested production helper build');
  }
  const units = artifacts.map((item) => ({
    packageId: item.package_id, targetName: item.target.name, targetKind: item.target.kind,
    crateTypes: item.target.crate_types, features: item.features,
  }));
  // These records describe observed build inputs, not exact binary linkage.
  units.sort((left, right) => JSON.stringify(left).localeCompare(JSON.stringify(right), 'en'));
  return { executable: executable[0].executable, units };
}

export function buildHomes(environment = process.env, home = os.homedir(), cwd = process.cwd()) {
  return {
    CARGO_HOME: path.resolve(cwd, environment.CARGO_HOME || path.join(home, '.cargo')),
    RUSTUP_HOME: path.resolve(cwd, environment.RUSTUP_HOME || path.join(home, '.rustup')),
  };
}

export function nativeBuildEnvironment({ home, temp, targetDirectory }, environment = process.env) {
  const env = {};
  for (const name of ['PATH', 'SystemRoot', 'SYSTEMROOT', 'WINDIR', 'COMSPEC', 'PATHEXT', 'INCLUDE', 'LIB', 'LIBPATH', 'VCINSTALLDIR', 'VSINSTALLDIR', 'VCToolsInstallDir', 'WindowsSdkDir', 'WindowsSDKVersion', 'UCRTVersion', 'UniversalCRTSdkDir']) {
    if (environment[name] !== undefined) env[name] = environment[name];
  }
  return Object.assign(env, buildHomes(environment), {
    HOME: home, USERPROFILE: home, XDG_CONFIG_HOME: home,
    APPDATA: path.join(home, 'AppData', 'Roaming'), LOCALAPPDATA: path.join(home, 'AppData', 'Local'),
    JJ_CONFIG: path.join(home, 'jj.toml'), TMPDIR: temp, TMP: temp, TEMP: temp,
    CARGO_TARGET_DIR: targetDirectory, CARGO_BUILD_JOBS: '4', CARGO_PROFILE_DEV_DEBUG: '0',
    LC_ALL: 'C',
  });
}

export async function build({ source, output, profile = 'release' }) {
  if (!['dev', 'release'].includes(profile)) throw new Error('profile must be dev or release');
  const target = nativeTarget(process.platform === 'win32' ? 'windows' : process.platform, process.arch === 'x64' ? 'amd64' : process.arch);
  const { targetTriple: triple } = target;
  source = await fs.realpath(path.resolve(source));
  output = await newOutputOutside(source, output);
  const tempBase = await fs.realpath(os.tmpdir());
  const withinSource = (candidate) => {
    const relative = path.relative(source, candidate);
    return relative === '' || (!relative.startsWith(`..${path.sep}`) && relative !== '..' && !path.isAbsolute(relative));
  };
  if (withinSource(tempBase)) throw new Error('helper build temporary base must be outside prepared source');
  const manifest = JSON.parse(await fs.readFile(path.join(root, 'manifest.json'), 'utf8'));
  const verifySource = () => verifyPreparedSource(source, manifest);
  await verifySource();
  let created = false;
  let owned;
  try {
    await fs.mkdir(output);
    created = true;
    owned = await fs.mkdtemp(path.join(tempBase, 'crabbox-jj-build-'));
    const home = path.join(owned, 'home');
    const temp = path.join(owned, 'tmp');
    await fs.mkdir(home);
    await fs.mkdir(temp);
    await fs.writeFile(path.join(home, 'jj.toml'), '');
    const env = nativeBuildEnvironment({ home, temp, targetDirectory: path.join(owned, 'target') });
    const rustc = run('rustc', ['--version'], home, env);
    const cargo = run('cargo', ['--version'], home, env);
    run('go', ['version'], home, { ...env, GOENV: 'off', GOTOOLCHAIN: 'local' });
    const compilation = spawnSync('cargo', ['build', '--locked', '--offline', '--manifest-path', path.join(source, 'Cargo.toml'),
      '-p', 'jj-cli', '--bin', 'crabbox-jj-source', '--no-default-features', '--features', 'git',
      '--target', triple, '--profile', profile, '--message-format', 'json'], {
      cwd: source, env, encoding: 'utf8', maxBuffer: 16 * 1024 * 1024, stdio: ['ignore', 'pipe', 'inherit'],
    });
    if (compilation.error || compilation.status !== 0) {
      for (const line of (compilation.stdout ?? '').split('\n')) {
        try {
          const message = JSON.parse(line);
          if (message.reason === 'compiler-message' && typeof message.message?.rendered === 'string') process.stderr.write(message.message.rendered);
        } catch { /* A truncated final message cannot replace the primary build failure. */ }
      }
      throw compilation.error ?? new Error(`Cargo production build failed (${compilation.status})`);
    }
    const buildUnits = productionBuildUnits(compilation.stdout);
    const packageMetadata = JSON.parse(run('cargo', ['metadata', '--locked', '--offline', '--format-version', '1',
      '--manifest-path', path.join(source, 'Cargo.toml'), '--filter-platform', triple], source, env, 16 * 1024 * 1024));
    await verifySource();
    const name = target.binaryName;
    const built = path.join(env.CARGO_TARGET_DIR, triple, profile === 'dev' ? 'debug' : 'release', name);
    if (path.resolve(buildUnits.executable) !== built) throw new Error('Cargo helper executable does not match the owned target directory');
    const version = JSON.parse(run(built, ['--no-pager', '--color=never', 'source-version'], home, env));
    verifySourceVersion(version);
    const staged = path.join(output, name);
    await fs.copyFile(built, staged);
    await fs.chmod(staged, 0o755);
    const receipt = createBuildReceipt({ target, profile, rustc, cargo, binarySHA256: await fileSHA256(staged) });
    await fs.writeFile(path.join(output, receiptName), `${JSON.stringify(receipt, null, 2)}\n`, { flag: 'wx', mode: 0o644 });
    await verifyPair(output, { target });
    await fs.rm(owned, { recursive: true });
    owned = undefined;
    return { directory: output, ...receipt, buildUnits: buildUnits.units, packageMetadata };
  } catch (error) {
    const errors = [error];
    for (const directory of [owned, created ? output : undefined]) {
      if (directory === undefined) continue;
      try { await fs.rm(directory, { recursive: true }); }
      catch (cleanup) { errors.push(new Error(`remove owned helper build state ${directory}`, { cause: cleanup })); }
    }
    if (errors.length === 1) throw error;
    throw new AggregateError(errors, 'native helper build and cleanup failed');
  }
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    const { values } = parseArgs({ options: { source: { type: 'string' }, output: { type: 'string' }, profile: { type: 'string', default: 'release' } } });
    if (!values.source || !values.output) throw new Error('usage: node tools/jj-source/build.mjs --source PREPARED_SOURCE --output NEW_INSTALL_DIRECTORY [--profile dev|release]');
    process.stdout.write(`${JSON.stringify(await build(values), null, 2)}\n`);
  } catch (error) {
    console.error(error);
    process.exitCode = 1;
  }
}
