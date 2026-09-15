#!/usr/bin/env node
import * as fs from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { parseArgs } from 'node:util';
import { build } from './build.mjs';
import { collectNotices } from './notices.mjs';
import { assembleReleaseBundle, newOutputOutside } from './artifacts.mjs';

// Private build evidence stays beside, never inside, the four-file bundle.
export async function produce({ source, output }) {
  source = await fs.realpath(source);
  output = await newOutputOutside(source, output);
  await fs.mkdir(output, { mode: 0o700 });
  try {
    const pair = path.join(output, 'pair');
    const { packageMetadata, ...report } = await build({ source, output: pair, profile: 'release' });
    const buildReport = path.join(output, 'build.json');
    const metadata = path.join(output, 'metadata.json');
    await fs.writeFile(buildReport, JSON.stringify(report, null, 2) + '\n', { flag: 'wx', mode: 0o600 });
    await fs.writeFile(metadata, JSON.stringify(packageMetadata, null, 2) + '\n', { flag: 'wx', mode: 0o600 });
    const notices = await collectNotices({ source, pair, buildReport, metadata, output: path.join(output, 'notices') });
    const bundle = path.join(output, 'bundle');
    const artifact = await assembleReleaseBundle({ input: pair, notices: notices.directory, output: bundle });
    return { directory: output, bundle, artifact };
  } catch (error) {
    try { await fs.rm(output, { recursive: true }); }
    catch (cleanup) { throw new AggregateError([error, cleanup], 'native production and cleanup failed'); }
    throw error;
  }
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    const { values } = parseArgs({ options: { source: { type: 'string' }, output: { type: 'string' } } });
    if (!values.source || !values.output) throw new Error('usage: produce.mjs --source PREPARED_SOURCE --output NEW_WORK_DIRECTORY');
    process.stdout.write(JSON.stringify(await produce(values), null, 2) + '\n');
  } catch (error) { console.error(error); process.exitCode = 1; }
}
