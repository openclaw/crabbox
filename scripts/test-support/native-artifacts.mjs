import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import crypto from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { nativeTargets, createBuildReceipt, receiptName } from '../../tools/jj-source/artifacts.mjs';
import { noticeName, attributionName } from '../../tools/jj-source/notice-artifacts.mjs';

let built;

// Ordinary compiler outputs exercise real executable readers. They are format
// fixtures, never evidence that a native Rust helper was built or executed.
export function formatBinaries() {
  if (process.env.CRABBOX_TEST_NATIVE_BINARY_DIR) return process.env.CRABBOX_TEST_NATIVE_BINARY_DIR;
  if (built) return built;
  built = fs.mkdtempSync(path.join(os.tmpdir(), 'crabbox-native-formats-'));
  process.once('exit', () => fs.rmSync(built, { recursive: true, force: true }));
  const source = path.join(built, 'main.go');
  fs.writeFileSync(source, 'package main\nfunc main() {}\n');
  for (const target of nativeTargets) {
    execFileSync('go', ['build', '-p', '2', '-trimpath', '-o', path.join(built, target.key), source], {
      cwd: built, timeout: 240000, stdio: 'pipe',
      env: { ...process.env, GOOS: target.targetOS, GOARCH: target.targetArch, CGO_ENABLED: '0', GOENV: 'off',
        GO111MODULE: 'off', GOWORK: 'off', GOFLAGS: '', GOTOOLCHAIN: 'local', GOPROXY: 'off', GOSUMDB: 'off' },
    });
  }
  return built;
}

export function writeNativeNotices(directory, receipt) {
  const text = 'Synthetic attribution for compiler format fixture; not Rust license evidence.\n';
  const report = { schemaVersion: 1, sourceTreeSHA256: receipt.sourceTreeSHA256, helperSHA256: receipt.binarySHA256,
    targetOS: receipt.targetOS, targetArch: receipt.targetArch,
    packages: [{ name: 'format-fixture', version: '1.0.0' }], unresolved: [],
    notice: { name: noticeName, sha256: crypto.createHash('sha256').update(text).digest('hex'), size: Buffer.byteLength(text) } };
  fs.writeFileSync(path.join(directory, noticeName), text);
  fs.writeFileSync(path.join(directory, attributionName), `${JSON.stringify(report, null, 2)}\n`);
}

export function writeNativeInputs(directory) {
  const binaries = formatBinaries();
  fs.mkdirSync(directory, { mode: 0o700 });
  for (const target of nativeTargets) {
    const pair = path.join(directory, target.key);
    fs.mkdirSync(pair, { mode: 0o700 });
    const binary = path.join(pair, target.binaryName);
    fs.copyFileSync(path.join(binaries, target.key), binary);
    fs.chmodSync(binary, 0o755);
    const receipt = createBuildReceipt({ target, profile: 'release', rustc: 'rustc 1.98.1 (format fixture)',
      cargo: 'cargo 1.98.1 (format fixture)', binarySHA256: crypto.createHash('sha256').update(fs.readFileSync(binary)).digest('hex') });
    fs.writeFileSync(path.join(pair, receiptName), `${JSON.stringify(receipt, null, 2)}\n`);
    writeNativeNotices(pair, receipt);
  }
}
