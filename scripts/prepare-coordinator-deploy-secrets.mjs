import fs from "node:fs";

function fail(message) {
  console.error(`::error::${message}`);
  process.exit(1);
}

const snapshot = process.env.CRABBOX_DAYTONA_SNAPSHOT;
if (process.env.CLEAR_DAYTONA_SNAPSHOT === "true" && snapshot) {
  fail(
    "Remove CRABBOX_DAYTONA_SNAPSHOT from the coordinator GitHub environment before clearing the Worker binding.",
  );
}

const secrets = {};
const bundle = process.env.CRABBOX_ARTIFACTS_CREDENTIALS_JSON;
if (bundle) {
  const invalidBundle =
    "CRABBOX_ARTIFACTS_CREDENTIALS_JSON must be a JSON object containing exactly CRABBOX_ARTIFACTS_ACCESS_KEY_ID and CRABBOX_ARTIFACTS_SECRET_ACCESS_KEY as nonempty strings.";
  let credentials;
  try {
    credentials = JSON.parse(bundle);
  } catch {
    fail(invalidBundle);
  }
  const keys = ["CRABBOX_ARTIFACTS_ACCESS_KEY_ID", "CRABBOX_ARTIFACTS_SECRET_ACCESS_KEY"];
  if (
    credentials === null ||
    typeof credentials !== "object" ||
    Array.isArray(credentials) ||
    Object.keys(credentials).length !== keys.length ||
    !keys.every((key) => typeof credentials[key] === "string" && credentials[key].trim().length > 0)
  ) {
    fail(invalidBundle);
  }
  for (const key of keys) secrets[key] = credentials[key];
}
if (snapshot) secrets.CRABBOX_DAYTONA_SNAPSHOT = snapshot;

if (Object.keys(secrets).length > 0) {
  try {
    // The deploy owner precreates this file and owns cleanup on every exit path.
    const fd = fs.openSync(process.env.SECRETS_FILE, "r+");
    try {
      fs.fchmodSync(fd, 0o600);
      fs.writeFileSync(fd, JSON.stringify(secrets));
    } finally {
      fs.closeSync(fd);
    }
  } catch {
    fail("Unable to prepare coordinator deploy secrets file.");
  }
}
