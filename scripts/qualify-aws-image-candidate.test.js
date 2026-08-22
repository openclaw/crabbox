import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { readFile } from "node:fs/promises";
import path from "node:path";
import test from "node:test";
import { promisify } from "node:util";

const execFileAsync = promisify(execFile);
const repoRoot = path.resolve(import.meta.dirname, "..");
const script = path.join(repoRoot, "scripts", "qualify-aws-image-candidate.sh");

test("qualification runner is syntactically valid and pins every live AWS selector", async () => {
  await execFileAsync("bash", ["-n", script], { cwd: repoRoot });
  const source = await readFile(script, "utf8");
  assert.match(source, /export CRABBOX_AWS_AMI="\$ami_id"/);
  assert.match(source, /export CRABBOX_AWS_REGION="\$region"/);
  assert.match(source, /--market on-demand/);
  assert.match(source, /--os "\$os_selector"/);
  assert.match(source, /VERSION_ID/);
  assert.match(source, /--type "\$instance_type"/);
  assert.match(source, /--arch "\$architecture"/);
  assert.match(source, /for dimension in ami architecture recipe type/);
  assert.match(
    source,
    /mismatch_compatibility="aws-linux-\$\{region\}-\$\{wrong_instance_type\}-\$\{architecture\}-\$\{ami_id\}"/,
  );
  assert.match(source, /--result drain/);
  assert.match(source, /stop --provider aws --id "\$unexpected_lease"/);
  assert.match(source, /no_ready_lease/);
  assert.match(source, /mountpoint -q \/var\/cache\/crabbox\/qualification/);
  assert.match(source, /CRABBOX_SYNC_GIT_OVERLAY=1/);
  assert.match(source, /events "\$clean_run_id" --type sync\.finished --json/);
  assert.match(source, /events "\$dirty_run_id" --type sync\.finished --json/);
  assert.doesNotMatch(source, /promote|fast-snapshot-restores|enable-fast-snapshot/);
});

test("failure cleanup stops leases before fixture restoration and never hides negative-borrow leaks", async () => {
  const source = await readFile(script, "utf8");
  const cleanupStart = source.indexOf("cleanup() {");
  const cleanupEnd = source.indexOf("\n}\ntrap cleanup EXIT", cleanupStart);
  const cleanup = source.slice(cleanupStart, cleanupEnd);
  assert.ok(cleanup.indexOf('stop --provider aws --id "$lease_id"') >= 0);
  assert.ok(
    cleanup.indexOf('stop --provider aws --id "$lease_id"') <
      cleanup.indexOf('cp "$fixture_backup" "$fixture"'),
  );
  assert.doesNotMatch(cleanup, /\|\| true/);

  const negativeStart = source.indexOf('if [[ "$borrow_status" -eq 0 ]]');
  const negativeEnd = source.indexOf("\n  node -e '", negativeStart);
  const negative = source.slice(negativeStart, negativeEnd);
  assert.match(negative, /--result drain/);
  assert.match(negative, /stop --provider aws --id "\$unexpected_lease"/);
  assert.doesNotMatch(negative, /\|\| true/);
});

test("receipt is emitted only after cleanup and cache skips are capability-explicit", async () => {
  const source = await readFile(script, "utf8");
  const stop = source.lastIndexOf('"$crabbox" stop --provider aws --id "$lease_id"');
  const cleanup = source.lastIndexOf('"status":"passed"}\\n\' >"$private/cleanup.json"');
  const build = source.lastIndexOf('aws-image-qualification.mjs" build');
  assert.ok(stop >= 0 && cleanup > stop && build > cleanup);
  assert.match(
    source,
    /"reason":"provider_capability_not_advertised"/,
  );
  assert.match(source, /"advertised":true,"status":"passed","reason":"verified"/);
  assert.doesNotMatch(
    source.slice(build),
    /lease_id|borrowToken|ssh|work-root/,
  );
});
