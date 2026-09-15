import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import crypto from 'node:crypto';
import { lockedPackages, attributionPath, hasCurrentAttribution, archiveListingNames, parseArchiveMembers, renderNotices, collectNotices } from './notices.mjs';
import { verifyNoticeBundle, noticeName, attributionName } from './notice-artifacts.mjs';

test('Cargo-generated lock identities distinguish registry and prepared packages', () => {
  const packages = lockedPackages(`version = 4
[[package]]
name = "first"
version = "1.2.3"
source = "registry+https://github.com/rust-lang/crates.io-index"
checksum = "${'a'.repeat(64)}"
dependencies = [
 "second",
]
[[package]]
name = "second"
version = "2.0.0"
`);
  assert.equal(packages.size, 2);
  assert.equal(packages.get(JSON.stringify(['first', '1.2.3', 'registry+https://github.com/rust-lang/crates.io-index'])).checksum, 'a'.repeat(64));
  assert.equal(packages.get(JSON.stringify(['second', '2.0.0', null])).name, 'second');
});

test('attribution discovery includes nested and alternative notice filenames', () => {
  for (const file of ['LICENSE-MIT', 'COPYRIGHT', 'UNLICENSE', 'src/spin/LICENSE', 'NOTICES.md', 'docs/fonts/OFL-LICENSE.txt', 'LICENSE-Apache-2.0_WITH_LLVM-exception']) assert.equal(attributionPath(file), true, file);
  for (const file of ['Cargo.toml', 'src/lib.rs', 'README.md']) assert.equal(attributionPath(file), false, file);
});

test('archive listings accept native line endings without trimming member names', () => {
  for (const newline of ['\n', '\r\n']) {
    const listing = ['example-1.0.0/', 'example-1.0.0/LICENSE-MIT ', 'example-1.0.0/README.md', ''].join(newline);
    assert.deepEqual(parseArchiveMembers(listing, 'example-1.0.0'), ['LICENSE-MIT ', 'README.md']);
    const bundle = ['attribution.json', 'crabbox-jj-source.NOTICES.txt', 'crabbox-jj-source.exe', 'crabbox-jj-source.json'];
    assert.deepEqual(archiveListingNames((bundle.join(newline) + newline).trim()), bundle);
  }
});

test('historical notices cannot clear current attribution gaps', () => {
  const historical = [{ kind: 'upstream-overview' }, { kind: 'upstream-historical-license' }];
  assert.equal(hasCurrentAttribution(historical), false);
  assert.equal(hasCurrentAttribution([...historical, { kind: 'package-file' }]), true);
  assert.equal(hasCurrentAttribution([...historical, { kind: 'upstream-license' }]), true);
});

test('rendering retains every package reference while storing identical text once', () => {
  const body = 'Verbatim fixture attribution\r\nwith original line endings.\r\n';
  const digest = 'a'.repeat(64);
  const report = { scope: 'fixture', sourceTreeSHA256: 'b'.repeat(64), helperSHA256: 'c'.repeat(64), targetOS: 'darwin', targetArch: 'arm64', unresolved: [{ name: 'pending', version: '2.0.0', reason: 'no-packaged-attribution-text' }],
    packages: ['first', 'second'].map((name) => ({ name, version: '1.0.0', declaredLicense: 'MIT', origin: { kind: 'fixture' }, materials: [{ path: 'LICENSE', sha256: digest, kind: 'package-file' }] })) };
  report.packages.push({ name: 'pending', version: '2.0.0', declaredLicense: 'MIT', origin: { kind: 'fixture' }, materials: [] });
  const output = renderNotices(report, new Map([[digest, body]]));
  assert.equal(output.split(body).length - 1, 1);
  assert.equal(output.split(`Attribution: LICENSE (package-file; SHA-256 ${digest})`).length - 1, 2);
  assert.ok(output.includes('Unresolved: pending 2.0.0: no-packaged-attribution-text\n'));
});

test('notice bundles retain build binding and unresolved provenance without requiring a Cargo cache', async (t) => {
  const owned = await fs.mkdtemp(path.join(os.tmpdir(), 'crabbox-notice-binding-'));
  t.after(() => fs.rm(owned, { recursive: true }));
  const text = 'Synthetic package attribution\n';
  const receipt = { sourceTreeSHA256: 'a'.repeat(64), binarySHA256: 'b'.repeat(64), targetOS: 'darwin', targetArch: 'arm64' };
  const unresolved = [{ name: 'fixture', version: '1.0.0', reason: 'no-packaged-attribution-text' }];
  const report = { schemaVersion: 1, sourceTreeSHA256: receipt.sourceTreeSHA256, helperSHA256: receipt.binarySHA256,
    targetOS: receipt.targetOS, targetArch: receipt.targetArch, packages: [{ name: 'fixture', version: '1.0.0' }], unresolved,
    notice: { name: noticeName, sha256: crypto.createHash('sha256').update(text).digest('hex'), size: Buffer.byteLength(text) } };
  await fs.writeFile(path.join(owned, noticeName), text);
  const writeReport = () => fs.writeFile(path.join(owned, attributionName), JSON.stringify(report) + '\n');
  await writeReport();
  const actual = await verifyNoticeBundle(owned, receipt);
  assert.deepEqual(actual.unresolved, unresolved);
  await assert.rejects(verifyNoticeBundle(owned, { ...receipt, targetArch: 'amd64' }), /do not match/);
  report.unresolved = [{ ...unresolved[0], name: 'absent' }];
  await writeReport();
  await assert.rejects(verifyNoticeBundle(owned, receipt), /does not identify an observed package/);
  report.unresolved = unresolved;
  await writeReport();
  await fs.writeFile(path.join(owned, 'unrelated.txt'), 'fixture');
  await assert.rejects(verifyNoticeBundle(owned, receipt), /exactly its text and attribution report/);
  assert.deepEqual(await verifyNoticeBundle(owned, receipt, { coinstalled: true }), actual);
});

test('observed native build notice collection is deterministic', { skip: !process.env.CRABBOX_TEST_JJ_NOTICE_INPUTS }, async (t) => {
  const inputs = JSON.parse(await fs.readFile(process.env.CRABBOX_TEST_JJ_NOTICE_INPUTS, 'utf8'));
  const owned = await fs.mkdtemp(path.join(os.tmpdir(), 'crabbox-notices-test-'));
  t.after(() => fs.rm(owned, { recursive: true }));
  const first = await collectNotices({ ...inputs, output: path.join(owned, 'first') });
  const second = await collectNotices({ ...inputs, output: path.join(owned, 'second') });
  assert.equal(first.noticeSHA256, second.noticeSHA256);
  for (const name of [noticeName, attributionName]) {
    assert.deepEqual(await fs.readFile(path.join(first.directory, name)), await fs.readFile(path.join(second.directory, name)));
  }
  const receipt = JSON.parse(await fs.readFile(path.join(inputs.pair, 'crabbox-jj-source.json'), 'utf8'));
  const verified = await verifyNoticeBundle(first.directory, receipt);
  assert.equal(verified.text.sha256, first.noticeSHA256);
  assert.deepEqual(verified.unresolved, first.unresolved);
  if (receipt.targetOS === 'darwin') {
    const report = JSON.parse(await fs.readFile(path.join(first.directory, attributionName), 'utf8'));
    const historical = report.packages.filter((pkg) => pkg.materials.some((item) => item.kind === 'upstream-historical-license'));
    assert.deepEqual(historical.map((pkg) => pkg.name).sort(), ['block2', 'objc2', 'objc2-encode']);
    for (const pkg of historical) assert.ok(report.unresolved.some((item) => item.name === pkg.name && item.version === pkg.version));
  }
  await assert.rejects(verifyNoticeBundle(first.directory, { ...receipt, binarySHA256: '0'.repeat(64) }), /do not match/);
  await fs.appendFile(path.join(first.directory, noticeName), '\nchanged fixture text\n');
  await assert.rejects(verifyNoticeBundle(first.directory, receipt), /do not match/);
});
