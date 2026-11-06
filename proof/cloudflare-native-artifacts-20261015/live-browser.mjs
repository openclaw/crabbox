// Credential-free fixture: a real Chromium screenshot and Playwright trace.
import { createHash } from 'node:crypto';
import { createReadStream } from 'node:fs';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { chromium } from 'playwright';

const mode = process.argv[2];
if (!['success', 'failure'].includes(mode)) throw new Error('expected success or failure');
await mkdir('reports', { recursive: true });
const browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] });
let version;
try {
  version = browser.version();
  const context = await browser.newContext({ viewport: { width: 640, height: 480 } });
  // The page is wholly local; tracing cannot capture store/customer credentials.
  await context.route('**/*', route => route.abort());
  await context.tracing.start({ screenshots: true, snapshots: true, sources: true });
  const page = await context.newPage();
  await page.setContent('<h1>Cloudflare artifact proof</h1><canvas width="480" height="320"></canvas>');
  await page.locator('canvas').evaluate(canvas => {
    const context = canvas.getContext('2d');
    const pixels = context.createImageData(canvas.width, canvas.height);
    let state = 123456789;
    for (let i = 0; i < pixels.data.length; i += 4) {
      for (let c = 0; c < 3; c++) {
        state ^= state << 13; state ^= state >>> 17; state ^= state << 5;
        pixels.data[i + c] = state & 255;
      }
      pixels.data[i + 3] = 255;
    }
    context.putImageData(pixels, 0, 0);
  });
  await page.screenshot({ path: 'reports/screen.png' });
  await context.tracing.stop({ path: 'reports/trace.zip' });
  await context.close();
} finally {
  await browser.close();
}
const hash = bytes => createHash('sha256').update(bytes).digest('hex');
const files = {};
for (const name of ['screen.png', 'trace.zip']) {
  const bytes = await readFile(`reports/${name}`);
  files[name] = { bytes: bytes.length, sha256: hash(bytes) };
}
if (files['screen.png'].bytes <= 65536) throw new Error('PNG must exceed old 64 KiB boundary');
const exitCode = mode === 'failure' ? 23 : 0;
const executableHash = createHash('sha256');
for await (const chunk of createReadStream(chromium.executablePath())) executableHash.update(chunk);
const packageMetadata = JSON.parse(await readFile('node_modules/playwright/package.json', 'utf8'));
await writeFile('reports/result.json', JSON.stringify({ mode, exitCode, browser: version,
  platform: process.platform, arch: process.arch, playwright: packageMetadata.version,
  packageLockSha256: hash(await readFile('package-lock.json')),
  chromiumSha256: executableHash.digest('hex'), files }) + '\n');
const result = await readFile('reports/result.json');
files['result.json'] = { bytes: result.length, sha256: hash(result) };
// Hashes only. Artifact bytes must travel through Crabbox's binary download route.
console.log('ARTIFACT_PROOF ' + JSON.stringify({ mode, exitCode, files }));
process.exitCode = exitCode;
