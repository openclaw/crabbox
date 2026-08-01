import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";

const repoRoot = path.resolve(import.meta.dirname, "..");
const workflow = fs.readFileSync(
  path.join(repoRoot, ".github", "workflows", "broker-soak-canary.yml"),
  "utf8",
);

test("broker soak canary is protected, manual, and serialized", () => {
  assert.match(workflow, /^  workflow_dispatch:$/m);
  assert.doesNotMatch(workflow, /^  (?:push|pull_request|schedule):/m);
  assert.match(workflow, /environment: image-publisher/);
  assert.match(workflow, /group: broker-soak-canary/);
  assert.match(workflow, /cancel-in-progress: false/);
  assert.match(
    workflow,
    /expected_workflow_ref="\$GITHUB_REPOSITORY\/\.github\/workflows\/broker-soak-canary\.yml@\$expected_ref"/,
  );
  assert.match(workflow, /\[\[ "\$GITHUB_REF" == "\$expected_ref" \]\]/);
  assert.match(workflow, /\[\[ "\$REF_PROTECTED" == true \]\]/);
  assert.match(workflow, /\[\[ "\$WORKFLOW_SHA" == "\$RUN_SHA" \]\]/);
  assert.match(workflow, /ref: \$\{\{ github\.workflow_sha \}\}/);
  assert.match(workflow, /persist-credentials: false/);
});

test("Daytona canary is broker-only, bounded, and has no warm pool", () => {
  assert.match(workflow, /CRABBOX_COORDINATOR_TOKEN: \$\{\{ secrets\.CRABBOX_COORDINATOR_ADMIN_TOKEN \}\}/);
  assert.match(workflow, /--provider daytona/);
  assert.match(workflow, /--network public/);
  assert.match(workflow, /--tailscale=false/);
  assert.match(workflow, /--ttl 20m/);
  assert.match(workflow, /--idle-timeout 5m/);
  assert.match(workflow, /--no-sync/);
  assert.match(workflow, /--stop-after always/);
  assert.match(workflow, /timeout --signal=TERM --kill-after=30s 20m bin\/crabbox run/);
  assert.doesNotMatch(workflow, /\bwarmup\b/);
  assert.doesNotMatch(
    workflow,
    /AWS_ACCESS_KEY_ID|AWS_SECRET_ACCESS_KEY|AZURE_CLIENT_SECRET|DAYTONA_API_KEY/,
  );
});

test("workflow sanitizes evidence and cleans only its dedicated owner", () => {
  assert.match(workflow, /CF-Access-Client-Id: %s/);
  assert.match(workflow, /CF-Access-Client-Secret: %s/);
  assert.doesNotMatch(workflow, /^  cleanup:$/m);
  assert.match(workflow, /name: Release and verify dedicated canary leases\n        if: always\(\)/);
  assert.match(workflow, /CRABBOX_OWNER: broker-soak-canary-\$\{\{ github\.run_id \}\}@openclaw\.invalid/);
  assert.match(workflow, /encoded_owner=.*CRABBOX_OWNER/);
  assert.match(workflow, /owner=\$encoded_owner&limit=100/);
  assert.match(workflow, /--data '\{"delete":true\}'/);
  assert.match(workflow, /unsafe_count\(\)/);
  assert.match(workflow, /\.state != "released" and \.state != "expired"/);
  assert.match(workflow, /\.releaseDeletesServer != null/);
  assert.match(workflow, /\.cleanupStartedAt != null/);
  assert.match(workflow, /\[\[ "\$remaining" -eq 0 \]\]/);
  assert.match(workflow, /candidateCount: \(\.candidates \| length\)/);
  assert.match(workflow, /def iso_epoch:\n\s+sub\(.+\) \| fromdateiso8601;/);
  assert.match(workflow, /def healthy_sweep:/);
  assert.match(workflow, /\$sweep\.lastRun\.enabled == true/);
  assert.match(workflow, /\$sweep\.lastRun\.errorCount == 0/);
  assert.match(workflow, /\(now - \$finished\) <= \(\$sweep\.config\.intervalSeconds \* 1\.5\)/);
  assert.match(workflow, /now <= \(\$next \+ \(\$sweep\.config\.intervalSeconds \* 0\.5\)\)/);
  assert.match(workflow, /unsafeAfterCleanup/);
  assert.match(workflow, /Upload sanitized broker soak proof/);
  assert.doesNotMatch(workflow, /path: \$\{\{ runner\.temp \}\}\/daytona-canary\.log/);
  assert.doesNotMatch(workflow, /\b(?:leaseId|slug|host|serverID)\b/);
});
