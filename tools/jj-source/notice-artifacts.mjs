import crypto from 'node:crypto';
import fs from 'node:fs/promises';
import path from 'node:path';

export const noticeName = 'crabbox-jj-source.NOTICES.txt';
export const attributionName = 'attribution.json';
const maximumBytes = 64 * 1024 * 1024;
const sha256 = (bytes) => crypto.createHash('sha256').update(bytes).digest('hex');

async function readArtifact(directory, name) {
  const file = path.join(directory, name);
  const stat = await fs.lstat(file);
  if (!stat.isFile() || stat.size === 0 || stat.size > maximumBytes) throw new Error('notice artifact must be a bounded nonempty regular file');
  const bytes = await fs.readFile(file);
  if (bytes.length === 0 || bytes.length > maximumBytes) throw new Error('notice artifact exceeds its size limit');
  return { bytes, record: { name, sha256: sha256(bytes), size: bytes.length } };
}

// Bind collected attribution to the original build, before Darwin signing.
// This checks artifact consistency, not completeness of legal attribution.
export async function verifyNoticeBundle(directory, receipt, { coinstalled = false } = {}) {
  if (!(await fs.lstat(directory)).isDirectory()) throw new Error('notice bundle must be a regular directory');
  if (!coinstalled && JSON.stringify((await fs.readdir(directory)).sort()) !== JSON.stringify([noticeName, attributionName].sort())) {
    throw new Error('notice bundle must contain exactly its text and attribution report');
  }
  const text = await readArtifact(directory, noticeName);
  const attribution = await readArtifact(directory, attributionName);
  const report = JSON.parse(new TextDecoder('utf-8', { fatal: true }).decode(attribution.bytes));
  if (report.schemaVersion !== 1 || report.sourceTreeSHA256 !== receipt.sourceTreeSHA256 ||
      report.helperSHA256 !== receipt.binarySHA256 || report.targetOS !== receipt.targetOS || report.targetArch !== receipt.targetArch ||
      report.notice?.name !== noticeName || report.notice.sha256 !== text.record.sha256 || report.notice.size !== text.record.size ||
      !Array.isArray(report.packages) || report.packages.length === 0 || !Array.isArray(report.unresolved)) {
    throw new Error('notice artifacts do not match the observed native build');
  }
  const packageKeys = new Set(report.packages.map((item) => JSON.stringify([item.name, item.version])));
  for (const item of report.unresolved) {
    if (typeof item.name !== 'string' || typeof item.version !== 'string' || typeof item.reason !== 'string' || !item.reason ||
        !packageKeys.has(JSON.stringify([item.name, item.version]))) throw new Error('unresolved attribution does not identify an observed package');
  }
  return { text: text.record, attribution: attribution.record, unresolved: report.unresolved };
}
