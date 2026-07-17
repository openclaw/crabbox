#!/usr/bin/env bash
set -Eeuo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
smoke_root="$(mktemp -d "${TMPDIR:-/tmp}/crabbox-cua-diagnostic.XXXXXX")"
bin="$smoke_root/crabbox"
smoke_repo="$smoke_root/repo"

cleanup() {
  rm -rf -- "$smoke_root"
}
trap cleanup EXIT

cd "$repo_root"
go build -trimpath -o "$bin" ./cmd/crabbox

mkdir -p "$smoke_repo"
cd "$smoke_repo"
git init -q
git config user.email smoke@example.com
git config user.name "Crabbox CUA Diagnostic"
printf 'provider: cua\n' >.crabbox.yaml
git add .crabbox.yaml
git commit -qm "test: seed CUA diagnostic fixture"

set +e
guard_output="$("$bin" warmup --provider cua 2>&1)"
guard_status=$?
set -e
if [[ $guard_status -eq 0 ]] || ! grep -Fq 'provider=cua provisioning is disabled' <<<"$guard_output" || ! grep -Fq 'https://github.com/openclaw/crabbox/issues/381' <<<"$guard_output"; then
  printf 'validation_failed reason=provisioning_guard_missing\n'
  exit 1
fi

doctor_stdout="$smoke_root/doctor.json"
doctor_stderr="$smoke_root/doctor.stderr"
doctor_status=0
"$bin" doctor --provider cua --json >"$doctor_stdout" 2>"$doctor_stderr" || doctor_status=$?

if ! node - "$doctor_stdout" "$doctor_status" <<'NODE'
const fs = require("node:fs");
const [file, statusText] = process.argv.slice(2);
const status = Number(statusText);
let result;
try {
  result = JSON.parse(fs.readFileSync(file, "utf8"));
} catch {
  process.exit(1);
}
const checks = Array.isArray(result.checks) ? result.checks : [];
const mode = checks.find((item) =>
  item?.details?.provider === "cua" &&
  item?.details?.experimental === "true" &&
  item?.details?.provisioning === "false" &&
  item?.details?.mutation === "false"
);
const failed = checks.filter((item) => item?.status === "failed");
const classified = failed.length > 0 && failed.every((item) => item?.details?.class === "environment_blocked");
if (result?.provider !== "cua" || !mode) process.exit(1);
if (status === 0 && result?.ok === true && failed.length === 0) process.exit(0);
if (status !== 0 && result?.ok === false && classified) process.exit(0);
process.exit(1);
NODE
then
	printf 'validation_failed reason=invalid_or_unclassified_doctor_result doctor_exit=%s\n' "$doctor_status"
	exit 1
fi

if [[ $doctor_status -ne 0 ]]; then
	printf 'environment_blocked reason=doctor_not_ready provisioning_guard=passed\n'
	exit 0
fi

printf 'diagnostic_only mode=experimental_non_provisioning provisioning_guard=passed\n'
