import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";

const repoRoot = path.resolve(import.meta.dirname, "..");
const workflow = fs.readFileSync(
  path.join(repoRoot, ".github", "workflows", "coordinator-deploy.yml"),
  "utf8",
);

test("coordinator deploy publishes a configured Daytona snapshot with the Worker version", () => {
  assert.match(workflow, /CRABBOX_DAYTONA_SNAPSHOT: \$\{\{ secrets\.CRABBOX_DAYTONA_SNAPSHOT \}\}/);
  assert.match(workflow, /\[ -n "\$\{CRABBOX_DAYTONA_SNAPSHOT\}" \]/);
  assert.match(
    workflow,
    /elif \[ -n "\$\{CRABBOX_DAYTONA_SNAPSHOT\}" \]; then[^]*?JSON\.stringify\(\{\s+CRABBOX_DAYTONA_SNAPSHOT: process\.env\.CRABBOX_DAYTONA_SNAPSHOT,[^]*?npm run deploy -- --secrets-file "\$\{SECRETS_FILE\}"[^]*?else/,
  );
  assert.doesNotMatch(workflow, /wrangler secret put/);
});

test("coordinator deploy preserves secret bindings when no snapshot is configured", () => {
  assert.match(
    workflow,
    /else\s+echo "::notice::CRABBOX_DAYTONA_SNAPSHOT is not set; deploying without changing existing secret bindings\."\s+npm run deploy\s+fi/,
  );
  assert.doesNotMatch(workflow, /CRABBOX_DAYTONA_SNAPSHOT is not set[^]*?exit 1/);
});

test("manual deploy can explicitly clear a stale Daytona snapshot binding", () => {
  assert.match(workflow, /^      clearDaytonaSnapshot:$/m);
  assert.match(
    workflow,
    /description: Clear the Worker snapshot binding after removing the coordinator environment secret/,
  );
  assert.match(workflow, /type: boolean/);
  assert.match(workflow, /CLEAR_DAYTONA_SNAPSHOT: \$\{\{ inputs\.clearDaytonaSnapshot \}\}/);
  assert.match(
    workflow,
    /if \[ "\$\{CLEAR_DAYTONA_SNAPSHOT\}" = "true" \]; then\s+if \[ -n "\$\{CRABBOX_DAYTONA_SNAPSHOT\}" \]; then[^]*?Remove CRABBOX_DAYTONA_SNAPSHOT from the coordinator GitHub environment[^]*?exit 1\s+fi\s+npm run deploy\s+printf '\{"CRABBOX_DAYTONA_SNAPSHOT":null\}\\n' \|\s+npx wrangler secret bulk\s+elif/,
  );
});
