import { fileURLToPath } from "node:url";

type Docker = (args: string[]) => Promise<string>;
const directory = "/tmp/crabbox-pool-diagnostic";
const source = fileURLToPath(new URL("./koyeb-pool-diagnostic.py", import.meta.url));
const vocabulary = {
  operation: ["baseline", "request", "pool_check", "pool_claim"],
  stage: [
    "observer",
    "state_directory",
    "lock_open",
    "lock_acquire",
    "consumed_marker",
    "workspace_empty",
    "browser_absent",
    "baseline_read",
    "home_snapshot",
    "home_integrity",
    "claim_format",
    "claim_marker_write",
    "claim_marker_sync",
    "claim_directory_sync",
    "claim_complete",
    "lock_close",
    "pool_dispatch",
    "baseline_capture",
    "baseline_request",
    "request_path",
    "request_read",
    "request_remove",
    "lease_identity",
  ],
  reason: [
    "entered",
    "clean",
    "claimed",
    "ready",
    "source_mismatch",
    "observer_error",
    "unclassified_error",
    "unpreserved_content_limit",
    "invalid_state",
    "already_consumed",
    "workspace_not_empty",
    "browser_profile_present",
    "home_changed",
    "invalid_claim",
    "invalid_request",
    "lease_mismatch",
    "invalid_baseline_authority",
    "home_repository_present",
    "home_changed_during_snapshot",
    "home_symlink_invalid",
    "home_unsupported_entry",
    "home_entry_limit",
    "home_path_invalid",
  ],
  exception: [
    "none",
    "value_error",
    "os_error",
    "permission_error",
    "not_found",
    "already_exists",
    "lock_blocked",
    "not_directory",
    "is_directory",
    "invalid_json",
    "dependency_error",
    "missing_key",
    "type_error",
    "other_exception",
  ],
  errno: [
    "none",
    "other_errno",
    "EACCES",
    "EPERM",
    "ENOENT",
    "EEXIST",
    "EAGAIN",
    "EWOULDBLOCK",
    "EIO",
    "ENOSPC",
    "EROFS",
    "ELOOP",
    "ENOTDIR",
    "EISDIR",
    "EINTR",
    "EINVAL",
    "EBADF",
    "ENOTSUP",
  ],
};
const homeVocabulary = {
  operation: ["baseline", "pool_check", "pool_claim"],
  reference: ["bootstrap", "last_clean"],
  reason: [
    "changed",
    "projection_missing",
    "projection_invalid",
    "projection_overflow",
    "baseline_binding_changed",
    "projection_exists",
    "projection_error",
    "unclassified_snapshot_mismatch",
    "groups_omitted",
  ],
  pathClass: [
    "home_cache",
    "home_config",
    "home_local_share",
    "home_local_state",
    "home_top_level",
    "home_other",
    "none",
  ],
  field: [
    "entry_added",
    "entry_removed",
    "type",
    "mode",
    "file_content",
    "symlink_target",
    "order",
    "top_level",
    "none",
  ],
  count: ["none", "one", "two_to_eight", "nine_or_more"],
};
const referenceVocabulary = {
  operation: ["baseline", "pool_check"],
  reference: ["bootstrap", "last_clean"],
  reason: ["projection_saved", "snapshot_not_captured"],
};
const entryVocabulary = {
  schema: ["crabbox-home-entry/v1"],
  operation: ["pool_check", "pool_claim"],
  reference: ["bootstrap", "last_clean"],
  field: ["entry_added"],
  pathClass: ["home_top_level"],
  count: ["one"],
  entryClass: [
    "local_root",
    "x_authority_candidate",
    "ice_authority_candidate",
    "desktop_candidate",
    "dbus_candidate",
    "other_top_level",
  ],
  kind: ["file", "directory", "symlink"],
  payload: ["empty", "nonempty", "not_applicable"],
};

export const unavailablePoolDiagnostic = () => ({ event: "runner_pool_diagnostic_unavailable" });

// Reconstruct only fixed vocabulary. Never echo an untrusted JSON field, line,
// exception, path, profile, claim, content hash or credential into the CI log.
export function projectPoolDiagnostic(line: string) {
  try {
    if (new TextEncoder().encode(line).length > 8192) return unavailablePoolDiagnostic();
    const value: unknown = JSON.parse(line);
    if (!value || typeof value !== "object" || Array.isArray(value))
      return unavailablePoolDiagnostic();
    const record = value as Record<string, unknown>;
    const home = record["event"] === "runner_home_comparison";
    const reference = record["event"] === "runner_home_reference";
    const entry = record["event"] === "runner_home_entry";
    const result: Record<string, string> = {
      event: entry
        ? "runner_home_entry"
        : reference
          ? "runner_home_reference"
          : home
            ? "runner_home_comparison"
            : "runner_pool_diagnostic",
    };
    const fields = entry
      ? entryVocabulary
      : reference
        ? referenceVocabulary
        : home
          ? homeVocabulary
          : vocabulary;
    if (entry) {
      const expected = ["event", ...Object.keys(fields)].toSorted();
      if (Object.keys(record).toSorted().join("\0") !== expected.join("\0"))
        return unavailablePoolDiagnostic();
    }
    for (const [key, allowed] of Object.entries(fields)) {
      const field: unknown = record[key];
      if (typeof field !== "string" || !allowed.includes(field)) return unavailablePoolDiagnostic();
      result[key] = field;
    }
    if (
      entry &&
      ((result.kind === "file" && result.payload === "not_applicable") ||
        (result.kind !== "file" && result.payload !== "not_applicable"))
    )
      return unavailablePoolDiagnostic();
    return result;
  } catch {
    return unavailablePoolDiagnostic();
  }
}

export async function installPoolDiagnostic(docker: Docker, container: string) {
  await docker(["exec", container, "install", "-d", "-m", "0700", directory]);
  await docker(["cp", source, `${container}:${directory}/crabbox_pool_diagnostic.py`]);
  // Python's startup loader observes the unmodified packaged helper. Exceptions
  // in the observer/loader are isolated from the helper and its terminal code.
  const loader = `import sys; exec(${JSON.stringify(
    `try:\n sys.path.append('${directory}')\n import crabbox_pool_diagnostic\nexcept Exception:\n pass`,
  )})\n`;
  await docker([
    "exec",
    container,
    "/usr/bin/python3",
    "-c",
    [
      "import pathlib,site,sys",
      "candidates=[p for p in site.getsitepackages() if p.startswith('/usr/local/')]",
      "assert candidates",
      "destination=pathlib.Path(candidates[0]); destination.mkdir(parents=True,exist_ok=True)",
      "(destination/'crabbox_pool_diagnostic.pth').write_text(sys.argv[1])",
    ].join(";"),
    loader,
  ]);
}

export async function readPoolDiagnostics(docker: Docker, container: string) {
  const output = await docker(["exec", container, "cat", `${directory}/events.jsonl`]);
  const lines = output.split("\n");
  if (lines.length > 128) return [unavailablePoolDiagnostic()];
  return lines.map(projectPoolDiagnostic);
}
