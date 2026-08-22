#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
crabbox="${CRABBOX_BIN:-$ROOT/bin/crabbox}"
input=""
output_dir=""
repo=""
ref=""
commit=""
fingerprint=""
cache_abi_digest=""
os_selector=""
workflow_ref=""
workflow_run_id=""
workflow_run_attempt=""
created_at=""

usage() {
  cat <<'USAGE'
Usage: scripts/qualify-aws-image-candidate.sh \
  --input qualification-input.json --output-dir DIR \
  --repo owner/repository --ref refs/heads/main --commit <40hex> \
  --fingerprint <value> --cache-abi-digest sha256:<64hex> \
  --os-selector ubuntu:26.04 \
  --workflow-ref owner/repository/.github/workflows/devtools-image-qualify.yml@refs/heads/main \
  --workflow-run-id <integer> --workflow-run-attempt <integer> \
  --created-at <RFC3339>

Boot and qualify one immutable Linux AWS image candidate. The resulting public
receipt excludes lease identifiers, endpoints, credentials, and work roots.
USAGE
}

while [[ "$#" -gt 0 ]]; do
  case "$1" in
    --input | --output-dir | --repo | --ref | --commit | --fingerprint | --cache-abi-digest | --os-selector | --workflow-ref | --workflow-run-id | --workflow-run-attempt | --created-at)
      [[ "$#" -ge 2 ]] || { printf '%s requires a value\n' "$1" >&2; exit 2; }
      name="${1#--}"
      name="${name//-/_}"
      printf -v "$name" '%s' "$2"
      shift 2
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      printf 'unknown argument: %s\n' "$1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

for name in input output_dir repo ref commit fingerprint cache_abi_digest os_selector workflow_ref workflow_run_id workflow_run_attempt created_at; do
  [[ -n "${!name}" ]] || { printf '%s is required\n' "--${name//_/-}" >&2; exit 2; }
done
[[ -x "$crabbox" ]] || { printf 'Crabbox binary is not executable: %s\n' "$crabbox" >&2; exit 2; }
[[ "$repo" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || { printf 'invalid --repo\n' >&2; exit 2; }
[[ "$commit" =~ ^[0-9a-f]{40}$ ]] || { printf 'invalid --commit\n' >&2; exit 2; }
[[ "$cache_abi_digest" =~ ^sha256:[0-9a-f]{64}$ ]] || { printf 'invalid --cache-abi-digest\n' >&2; exit 2; }
[[ "$os_selector" =~ ^ubuntu:(26[.]04|24[.]04)$ ]] || { printf 'invalid --os-selector\n' >&2; exit 2; }
[[ "$workflow_run_id" =~ ^[1-9][0-9]*$ && "$workflow_run_attempt" =~ ^[1-9][0-9]*$ ]] || {
  printf 'invalid workflow run identity\n' >&2
  exit 2
}
[[ ! -e "$output_dir/qualification.json" ]] || {
  printf 'qualification output already exists: %s\n' "$output_dir/qualification.json" >&2
  exit 2
}

mkdir -p "$output_dir/private"
chmod 700 "$output_dir/private"
summary="$(node "$ROOT/scripts/aws-image-qualification.mjs" inspect-input --file "$input")"
readarray -t candidate < <(node -e '
const v = JSON.parse(process.argv[1]);
for (const key of ["amiId", "region", "instanceType", "architecture", "profile", "recipeDigest"]) {
  if (!v[key]) process.exit(1);
  process.stdout.write(`${v[key]}\n`);
}
' "$summary")
ami_id="${candidate[0]}"
region="${candidate[1]}"
instance_type="${candidate[2]}"
architecture="${candidate[3]}"
profile="${candidate[4]}"
recipe_digest="${candidate[5]}"
pool_key="image-qualification-${workflow_run_id}-${workflow_run_attempt}"
compatibility_key="aws-linux-${region}-${instance_type}-${architecture}-${ami_id}"
private="$output_dir/private"
identity="$private/identity.json"
lease_id=""
stopped=0
fixture="$ROOT/scripts/fixtures/aws-image-qualification-overlay.txt"
fixture_backup="$private/fixture.backup"

cleanup() {
  local status=$?
  local cleanup_failed=0
  trap - EXIT
  set +e
  if [[ -n "$lease_id" && "$stopped" -eq 0 ]]; then
    if "$crabbox" stop --provider aws --id "$lease_id" >/dev/null; then
      stopped=1
    else
      printf 'failed to stop qualification lease %s during cleanup\n' "$lease_id" >&2
      cleanup_failed=1
    fi
  fi
  if [[ -f "$fixture_backup" ]]; then
    if ! cp "$fixture_backup" "$fixture"; then
      printf 'failed to restore qualification overlay fixture\n' >&2
      cleanup_failed=1
    fi
  fi
  if [[ "$status" -eq 0 && "$cleanup_failed" -ne 0 ]]; then
    status=1
  fi
  exit "$status"
}
trap cleanup EXIT

provider_json="$("$crabbox" providers describe aws --json)"
cache_advertised="$(
  node -e '
const value = JSON.parse(process.argv[1]);
process.stdout.write(String((value.capabilities?.features ?? value.features ?? []).includes("cache-volume")));
' "$provider_json"
)"

export CRABBOX_AWS_AMI="$ami_id"
export CRABBOX_AWS_REGION="$region"
export CRABBOX_SYNC_GIT_OVERLAY=1
prewarm=(
  "$crabbox" prewarm
  --provider aws
  --target linux
  --os "$os_selector"
  --type "$instance_type"
  --arch "$architecture"
  --market on-demand
  --repo "$repo"
  --ref "$ref"
  --no-hydrate
)
if [[ "$cache_advertised" == true ]]; then
  prewarm+=(--cache-volume "qualification-${workflow_run_id}:/var/cache/crabbox/qualification")
fi
"${prewarm[@]}" >"$private/prewarm.out" 2>"$private/prewarm.err"
lease_id="$(sed -n 's/.*prewarm complete id=\([^ ]*\).*/\1/p' "$private/prewarm.out" | tail -1)"
[[ "$lease_id" =~ ^cbx_[A-Za-z0-9_-]+$ ]] || { printf 'could not parse prewarm lease id\n' >&2; exit 1; }

"$crabbox" pool identity create \
  --id "$lease_id" \
  --repo "$repo" \
  --ref "$ref" \
  --commit "$commit" \
  --fingerprint "$fingerprint" \
  --expected-image "$ami_id" \
  --expected-type "$instance_type" \
  --expected-architecture "$architecture" \
  --expected-profile "$profile" \
  --expected-recipe-digest "$recipe_digest" \
  --cache-abi-digest "$cache_abi_digest" \
  --output "$identity" >"$private/identity.out"

os_version="${os_selector#ubuntu:}"
"$crabbox" run --provider aws --id "$lease_id" --no-sync -- \
  sh -c '. /etc/os-release; test "$ID" = ubuntu && test "$VERSION_ID" = "$1"' sh "$os_version" \
  >/dev/null
printf '{"schema":"crabbox-aws-image-qualification-status/v1","status":"passed"}\n' >"$private/os.json"

"$crabbox" pool register "$pool_key" \
  --id "$lease_id" \
  --repo "$repo" \
  --ref "$ref" \
  --commit "$commit" \
  --fingerprint "$fingerprint" \
  --compatibility-key "$compatibility_key" \
  --identity-file "$identity" >/dev/null

negative_file="$private/negative.json"
printf '{"schema":"crabbox-aws-image-qualification-negative/v1","gates":[' >"$negative_file"
separator=""
for dimension in ami architecture recipe type; do
  mismatch_identity="$identity"
  mismatch_compatibility="$compatibility_key"
  if [[ "$dimension" == type ]]; then
    wrong_instance_type="t3.micro"
    if [[ "$instance_type" == "$wrong_instance_type" ]]; then
      wrong_instance_type="t3.small"
    fi
    mismatch_compatibility="aws-linux-${region}-${wrong_instance_type}-${architecture}-${ami_id}"
  else
    mismatch_identity="$private/identity-$dimension.json"
    node "$ROOT/scripts/aws-image-qualification.mjs" mutate-identity \
      --identity "$identity" --dimension "$dimension" --output "$mismatch_identity"
  fi
  set +e
  borrow_output="$(
    "$crabbox" pool borrow "$pool_key" \
      --repo "$repo" --ref "$ref" --commit "$commit" --fingerprint "$fingerprint" \
      --provider aws --target linux \
      --compatibility-key "$mismatch_compatibility" \
      --identity-file "$mismatch_identity" --json 2>"$private/negative-$dimension.err"
  )"
  borrow_status=$?
  set -e
  if [[ "$borrow_status" -eq 0 ]]; then
    readarray -t unexpected < <(node -e '
const value = JSON.parse(process.argv[1]);
process.stdout.write(`${value.entry?.leaseID ?? ""}\n${value.entry?.borrowToken ?? ""}\n`);
' "$borrow_output")
    unexpected_lease="${unexpected[0]}"
    unexpected_token="${unexpected[1]}"
    [[ "$unexpected_lease" =~ ^cbx_[A-Za-z0-9_-]+$ ]] || {
      printf 'negative %s gate returned success without a valid lease id\n' "$dimension" >&2
      exit 1
    }
    drained=0
    if [[ -n "$unexpected_token" ]] && "$crabbox" pool return "$pool_key" \
      --id "$unexpected_lease" --result drain \
      --reason "qualification-$dimension-mismatch" \
      --borrow-token "$unexpected_token" >/dev/null; then
      drained=1
    fi
    if [[ "$drained" -eq 0 ]]; then
      "$crabbox" stop --provider aws --id "$unexpected_lease" >/dev/null
    fi
    if [[ "$unexpected_lease" == "$lease_id" ]]; then
      stopped=1
    fi
    printf 'negative %s gate unexpectedly borrowed a lease\n' "$dimension" >&2
    exit 1
  fi
  node -e '
const fs = require("fs");
const text = fs.readFileSync(process.argv[1], "utf8");
const match = text.match(/http 409: (\{[^\n]+\})\s*$/);
if (!match) process.exit(1);
const value = JSON.parse(match[1]);
if (
  value.error !== "no_ready_lease" ||
  value.message !== "no matching ready lease in pool" ||
  Object.keys(value).sort().join(",") !== "error,message"
) process.exit(1);
' "$private/negative-$dimension.err" || {
    printf 'negative %s gate did not return the expected no-match result\n' "$dimension" >&2
    exit 1
  }
  printf '%s{"dimension":"%s","status":"passed"}' "$separator" "$dimension" >>"$negative_file"
  separator=","
done
printf ']}\n' >>"$negative_file"

borrow_output="$(
  "$crabbox" pool borrow "$pool_key" \
    --repo "$repo" --ref "$ref" --commit "$commit" --fingerprint "$fingerprint" \
    --provider aws --target linux \
    --compatibility-key "$compatibility_key" \
    --identity-file "$identity" --json
)"
readarray -t positive < <(node -e '
const value = JSON.parse(process.argv[1]);
process.stdout.write(`${value.entry?.leaseID ?? ""}\n${value.entry?.borrowToken ?? ""}\n`);
' "$borrow_output")
[[ "${positive[0]}" == "$lease_id" && -n "${positive[1]}" ]] || {
  printf 'positive typed ready-pool borrow returned the wrong lease\n' >&2
  exit 1
}
"$crabbox" pool return "$pool_key" --id "$lease_id" --result ready \
  --borrow-token "${positive[1]}" --identity-file "$identity" >/dev/null
printf '{"schema":"crabbox-aws-image-qualification-status/v1","status":"passed"}\n' >"$private/positive.json"

"$crabbox" run --provider aws --id "$lease_id" --timing-json -- \
  sh -c 'test "$(cat scripts/fixtures/aws-image-qualification-overlay.txt)" = "crabbox qualification baseline v1"' \
  >"$private/clean.out" 2>"$private/clean.timing"
clean_run_id="$(node -e '
const fs = require("fs");
for (const line of fs.readFileSync(process.argv[1], "utf8").trim().split("\n").reverse()) {
  try {
    const value = JSON.parse(line);
    if (/^run_[0-9a-z]+$/.test(value.runId ?? "")) {
      process.stdout.write(value.runId);
      process.exit(0);
    }
  } catch {}
}
process.exit(1);
' "$private/clean.timing")"
"$crabbox" events "$clean_run_id" --type sync.finished --json >"$private/clean.events.json"

cp "$fixture" "$fixture_backup"
printf 'crabbox qualification dirty overlay v1\n' >"$fixture"
"$crabbox" run --provider aws --id "$lease_id" --timing-json -- \
  cat scripts/fixtures/aws-image-qualification-overlay.txt \
  >"$private/dirty.out" 2>"$private/dirty.timing"
dirty_run_id="$(node -e '
const fs = require("fs");
for (const line of fs.readFileSync(process.argv[1], "utf8").trim().split("\n").reverse()) {
  try {
    const value = JSON.parse(line);
    if (/^run_[0-9a-z]+$/.test(value.runId ?? "")) {
      process.stdout.write(value.runId);
      process.exit(0);
    }
  } catch {}
}
process.exit(1);
' "$private/dirty.timing")"
"$crabbox" events "$dirty_run_id" --type sync.finished --json >"$private/dirty.events.json"
cp "$fixture_backup" "$fixture"
rm "$fixture_backup"
printf 'crabbox qualification dirty overlay v1\n' >"$private/dirty.fixture"

if [[ "$cache_advertised" == true ]]; then
  marker="qualification-${workflow_run_id}-${workflow_run_attempt}"
  "$crabbox" run --provider aws --id "$lease_id" --no-sync -- \
    sh -c 'mountpoint -q /var/cache/crabbox/qualification && findmnt -n -M /var/cache/crabbox/qualification -o SOURCE,FSTYPE | grep -q . && printf %s "$1" > /var/cache/crabbox/qualification/marker' sh "$marker" \
    >/dev/null
  "$crabbox" run --provider aws --id "$lease_id" --no-sync -- \
    sh -c 'test "$(cat /var/cache/crabbox/qualification/marker)" = "$1"' sh "$marker" \
    >/dev/null
  printf '{"schema":"crabbox-aws-image-qualification-cache/v1","advertised":true,"status":"passed","reason":"verified"}\n' >"$private/cache.json"
else
  printf '{"schema":"crabbox-aws-image-qualification-cache/v1","advertised":false,"status":"skipped","reason":"provider_capability_not_advertised"}\n' >"$private/cache.json"
fi

"$crabbox" stop --provider aws --id "$lease_id"
stopped=1
printf '{"schema":"crabbox-aws-image-qualification-status/v1","status":"passed"}\n' >"$private/cleanup.json"

node "$ROOT/scripts/aws-image-qualification.mjs" build \
  --input "$input" \
  --identity "$identity" \
  --negative "$negative_file" \
  --positive "$private/positive.json" \
  --os-evidence "$private/os.json" \
  --dirty-fixture "$private/dirty.fixture" \
  --dirty-output "$private/dirty.out" \
  --clean-timing "$private/clean.timing" \
  --dirty-timing "$private/dirty.timing" \
  --clean-events "$private/clean.events.json" \
  --dirty-events "$private/dirty.events.json" \
  --cache "$private/cache.json" \
  --cleanup "$private/cleanup.json" \
  --workflow-ref "$workflow_ref" \
  --workflow-run-id "$workflow_run_id" \
  --workflow-run-attempt "$workflow_run_attempt" \
  --created-at "$created_at" \
  --os-selector "$os_selector" \
  --output "$output_dir/qualification.json"
