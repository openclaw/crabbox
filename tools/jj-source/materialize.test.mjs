import assert from 'node:assert/strict';
import * as fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { materialize, isolatedGitConfig, sourceDigestPath } from './materialize.mjs';
import { buildHomes, nativeBuildEnvironment, productionBuildUnits } from './build.mjs';
import { produce } from './produce.mjs';
import { spawnSync } from 'node:child_process';
import { nativeTargets, nativeTarget, fileSHA256, createBuildReceipt, verifyPair, verifyReleaseInputs, verifyReleaseBundle, assembleReleaseBundle, stageReleaseInputs, finalizeDarwinReleaseBundle, receiptName } from './artifacts.mjs';
import { writeNativeNotices } from '../../scripts/test-support/native-artifacts.mjs';

const jjArchive = process.env.CRABBOX_TEST_JJ_ARCHIVE;
const gixArchive = process.env.CRABBOX_TEST_GIX_ARCHIVE;
const nativeBinaries = process.env.CRABBOX_TEST_NATIVE_BINARY_DIR;

test('source identity encodes native path separators without resolving link targets', () => {
  assert.equal(sourceDigestPath('..\\..\\GOVERNANCE.md', '\\'), '../../GOVERNANCE.md');
  assert.equal(sourceDigestPath('../cli/src/config-schema.json', '/'), '../cli/src/config-schema.json');
  assert.equal(sourceDigestPath('literal\\name', '/'), 'literal\\name');
});

test('Git configuration isolation uses an owned empty file', () => {
  const empty = path.join(os.tmpdir(), 'owned-empty-git-config');
  assert.deepEqual(isolatedGitConfig(empty), { GIT_CONFIG_NOSYSTEM: '1',
    GIT_CONFIG_GLOBAL: empty, GIT_CONFIG_SYSTEM: empty, GIT_CONFIG_COUNT: '0', GIT_TERMINAL_PROMPT: '0' });
});

test('materializes identical pinned sources and preserves an existing destination', {
  skip: !jjArchive || !gixArchive ? 'requires the two pinned upstream archives' : false,
}, async (t) => {
  const owned = await fs.mkdtemp(path.join(os.tmpdir(), 'crabbox-jj-package-test-'));
  t.after(() => fs.rm(owned, { recursive: true }));
  const inventoryFile = path.join(owned, 'inventory.json');
  const first = await materialize({ output: path.join(owned, 'first'), jjArchive, gixArchive, inventoryFile });
  const inventory = JSON.parse(await fs.readFile(inventoryFile, 'utf8'));
  assert.equal(inventory.sha256, first.sourceTree.sha256);
  assert.equal(inventory.entries.length, first.sourceTree.entryCount);
  const second = await materialize({ output: path.join(owned, 'second'), jjArchive, gixArchive });
  assert.deepEqual(first.sourceTree, second.sourceTree);
  const marker = path.join(first.source, 'keep-me.txt');
  await fs.writeFile(marker, 'existing user content\n');
  await assert.rejects(materialize({ output: first.source, jjArchive, gixArchive }), { code: 'EEXIST' });
  assert.equal(await fs.readFile(marker, 'utf8'), 'existing user content\n');
  const existingOutput = path.join(owned, 'existing-native-output');
  await fs.mkdir(existingOutput);
  const outputMarker = path.join(existingOutput, 'keep-me.txt');
  await fs.writeFile(outputMarker, 'existing build output\n');
  await assert.rejects(produce({ source: second.source, output: existingOutput }), { code: 'EEXIST' });
  assert.equal(await fs.readFile(outputMarker, 'utf8'), 'existing build output\n');
});

test('build keeps original default and configured toolchain homes before isolating HOME', () => {
  const home = path.join(os.tmpdir(), 'parent-home');
  const cwd = path.join(os.tmpdir(), 'project');
  assert.deepEqual(buildHomes({}, home, cwd), {
    CARGO_HOME: path.join(home, '.cargo'), RUSTUP_HOME: path.join(home, '.rustup'),
  });
  assert.deepEqual(buildHomes({ CARGO_HOME: 'cargo-cache', RUSTUP_HOME: path.join(home, 'rust-toolchains') }, home, cwd), {
    CARGO_HOME: path.join(cwd, 'cargo-cache'), RUSTUP_HOME: path.join(home, 'rust-toolchains'),
  });
  const isolated = path.join(os.tmpdir(), 'isolated-home');
  const temp = path.join(isolated, 'tmp');
  const targetDirectory = path.join(isolated, 'target');
  const env = nativeBuildEnvironment({ home: isolated, temp, targetDirectory }, {
    PATH: '/native/tools', CARGO_HOME: path.join(home, '.cargo'), RUSTUP_HOME: path.join(home, '.rustup'),
    INCLUDE: 'native-headers', LIB: 'native-libraries', UNRELATED_SETTING: 'not inherited',
  });
  assert.equal(env.HOME, isolated);
  assert.equal(env.CARGO_HOME, path.join(home, '.cargo'));
  assert.equal(env.RUSTUP_HOME, path.join(home, '.rustup'));
  assert.equal(env.CARGO_TARGET_DIR, targetDirectory);
  assert.equal(env.TEMP, temp);
  assert.equal(env.INCLUDE, 'native-headers');
  assert.equal(env.LIB, 'native-libraries');
  assert.equal(env.UNRELATED_SETTING, undefined);
});

test('production build inventory retains observed fresh and newly built artifacts', () => {
  const artifact = (name, kind, features, fresh) => ({ reason: 'compiler-artifact', package_id: `${name}@1.0.0`,
    target: { name, kind: [kind], crate_types: [kind] }, features, fresh, profile: { test: false },
    executable: kind === 'bin' ? '/owned/release/crabbox-jj-source' : null });
  const result = productionBuildUnits([
    artifact('cached-library', 'lib', ['std'], true),
    artifact('crabbox-jj-source', 'bin', ['git'], false),
    { reason: 'build-finished', success: true },
  ].map((item) => JSON.stringify(item)).join('\n') + '\n');
  assert.equal(result.executable, '/owned/release/crabbox-jj-source');
  assert.equal(result.units.length, 2);
  assert.deepEqual(result.units.find((item) => item.targetName === 'cached-library').features, ['std']);
});

test('native artifact receipts retain one source identity across six explicit targets', () => {
  for (const target of nativeTargets) {
    const receipt = createBuildReceipt({ target, profile: 'release', rustc: 'rustc 1.98.1', cargo: 'cargo 1.98.1', binarySHA256: 'a'.repeat(64) });
    assert.equal(receipt.targetTriple, target.targetTriple);
    assert.equal(receipt.targetArch, target.targetArch);
  }
  assert.throws(() => nativeTarget('linux', '386'), /unsupported/);
});

test('native release set and signed-byte finalization preserve the original build evidence', {
  skip: !nativeBinaries ? 'requires ordinary six-target compiler outputs (format fixtures, not Rust provenance)' : false,
}, async (t) => {
  const owned = await fs.mkdtemp(path.join(os.tmpdir(), 'crabbox-jj-artifact-test-'));
  t.after(() => fs.rm(owned, { recursive: true }));
  const inputs = path.join(owned, 'inputs');
  await fs.mkdir(inputs);
  for (const target of nativeTargets) {
    const directory = path.join(owned, `${target.key}-pair`);
    await fs.mkdir(directory);
    const binary = path.join(directory, target.binaryName);
    await fs.copyFile(path.join(nativeBinaries, target.key), binary);
    await fs.chmod(binary, 0o755);
    // These are compiler-generated format fixtures, not native-source build proof.
    const receipt = createBuildReceipt({ target, profile: 'release', rustc: 'rustc 1.98.1 (fixture)', cargo: 'cargo 1.98.1 (fixture)', binarySHA256: await fileSHA256(binary) });
    await fs.writeFile(path.join(directory, receiptName), `${JSON.stringify(receipt, null, 2)}\n`);
    const notices = path.join(owned, `${target.key}-notices`);
    await fs.mkdir(notices);
    writeNativeNotices(notices, receipt);
    const output = path.join(inputs, target.key);
    const assembled = await assembleReleaseBundle({ input: directory, notices, output });
    assert.deepEqual(await verifyReleaseBundle(output), assembled);
    await assert.rejects(assembleReleaseBundle({ input: directory, notices, output }), { code: 'EEXIST' });
    assert.deepEqual((await verifyPair(directory)).receipt, receipt);
  }
  const original = await verifyReleaseInputs(inputs);
  assert.equal(original.length, nativeTargets.length);
  const staged = path.join(owned, 'staged');
  assert.deepEqual(await stageReleaseInputs({ input: inputs, output: staged }), original);
  assert.deepEqual(await verifyReleaseInputs(inputs), original);
  await assert.rejects(stageReleaseInputs({ input: inputs, output: staged }), { code: 'EEXIST' });
  assert.deepEqual(await verifyReleaseInputs(staged), original);
  const target = nativeTarget('darwin', 'arm64');
  const input = path.join(inputs, target.key);
  await assert.rejects(verifyPair(input), /exactly its binary and receipt/);
  await assert.rejects(verifyPair(input, { target: nativeTarget('darwin', 'amd64'), release: true }), /required target/);

  const development = path.join(owned, 'development');
  await fs.mkdir(development);
  for (const name of [target.binaryName, receiptName]) await fs.copyFile(path.join(input, name), path.join(development, name));
  const developmentReceipt = JSON.parse(await fs.readFile(path.join(development, receiptName), 'utf8'));
  developmentReceipt.profile = 'dev';
  await fs.writeFile(path.join(development, receiptName), JSON.stringify(developmentReceipt));
  await verifyPair(development);
  await assert.rejects(verifyPair(development, { release: true }), /release profile/);

  if (process.platform === 'darwin') {
    const signed = path.join(owned, 'signed-fixture');
    await fs.copyFile(path.join(input, target.binaryName), signed);
    // Ad-hoc signing tests the ordinary byte transformation without release keys.
    // It is never accepted as official signing or native execution proof.
    const sign = spawnSync('/usr/bin/codesign', ['--force', '--sign', '-', '--identifier', 'org.example.native-artifact-fixture', signed], { encoding: 'utf8' });
    assert.equal(sign.status, 0, sign.stderr);
    assert.notEqual(await fileSHA256(signed), (await verifyReleaseBundle(input)).receipt.binarySHA256);
    const output = path.join(owned, 'finalized');
    const finalized = await finalizeDarwinReleaseBundle({ input, signedBinary: signed, output });
    assert.deepEqual(finalized.original, original.find((item) => item.target === target.key));
    assert.deepEqual(finalized.finalized.receipt, { ...finalized.original.receipt, binarySHA256: await fileSHA256(signed) });
    assert.deepEqual(await verifyReleaseInputs(inputs), original);
    await assert.rejects(finalizeDarwinReleaseBundle({ input, signedBinary: signed, output }), { code: 'EEXIST' });
    assert.deepEqual(finalized.finalized.notices, finalized.original.notices);
    assert.deepEqual(await verifyReleaseBundle(output, { originalReceipt: finalized.original.receipt }), finalized.finalized);
    await assert.rejects(verifyReleaseBundle(output), /notice artifacts do not match/);
  }
});
