import type { CoordinatorStorage, CoordinatorStorageView } from "./coordinator-runtime";
import { sha256Hex } from "./encoding";
import { sameOrgIdentityKey } from "./org-identity";
import type { LeaseRecord } from "./types";

const schema = "crabbox-project-checkpoint/v1";
const fileSchema = "crabbox-project-files/v1";
const maxContentBytes = 72 * 1024 * 1024;
const chunkBytes = 48 * 1024;
const operationTTLMS = 5 * 60_000;
const utf8 = new TextEncoder();
export const projectCheckpointFailureCodes = new Set([
  "checkpoint_corrupt",
  "checkpoint_capture_invalid",
  "checkpoint_project_state_failed",
  "checkpoint_project_changed",
  "checkpoint_unpreserved_content_limit",
  "checkpoint_dependency_unsafe",
  "checkpoint_dependency_inventory_limit",
  "checkpoint_dependency_recipe_unsupported",
  "checkpoint_dependency_runtime_mismatch",
  "checkpoint_dependency_rebuild_unavailable",
  "checkpoint_dependency_input_policy_unsafe",
  "checkpoint_dependency_policy_unsupported",
  "checkpoint_dependency_recipe_corrupt",
  "checkpoint_dependency_rebuild_mismatch",
  "checkpoint_revision_conflict",
  "checkpoint_key_unavailable",
  "checkpoint_symlink_escape",
  "checkpoint_symlink_cycle",
]);

export class ProjectCheckpointError extends Error {
  constructor(readonly code: string) {
    super(code);
    this.name = "ProjectCheckpointError";
  }
}

export interface ProjectCheckpointScope {
  target: string;
  authority: string;
  owner: string;
  org: string;
  projectID: string;
  sessionID: string;
}

export interface ProjectCheckpointManifest {
  schema: typeof schema;
  scope: ProjectCheckpointScope;
  generation: number;
  revision: number;
  leaseID: string;
  leaseGeneration: string;
  acceptedRevision: string;
  capturedAt: string;
  contentSHA256: string;
  contentBytes: number;
  projectBytes: number;
  keyID: string;
  iv: string;
  chunks: number;
  ciphertextSHA256: string;
  consistency: "stable-tree";
  recovery: "filesystem-only";
  dependencyPolicy: "none" | "npm-lockfile-v1";
  dependencyBytes: number;
}

export interface ProjectCheckpointBinding {
  schema: typeof schema;
  key: string;
  scope: ProjectCheckpointScope;
  root: string;
  allowedRoot: string;
  leaseID: string;
  leaseGeneration: string;
  generation: number;
  revision: number;
  acceptedRevision: string;
  dependencyPolicy: "none" | "npm-lockfile-v1";
  state: "running" | "checkpointing" | "blocked" | "reclaim-ready" | "lost" | "restoring";
  updatedAt: string;
  checkpoint?: ProjectCheckpointManifest;
  operation?: { id: string; expiresAt: string };
  reasonCode?: "checkpoint-failed" | "restore-failed";
  failureCode?: string;
  loss?: {
    kind: "crash" | "ttl" | "discard";
    at: string;
    lastCheckpointAt: string | null;
    knownLostFrom: string | null;
  };
}

export interface ProjectCheckpointConfig {
  target: string;
  authority: string;
  keyID: string;
  secret: string;
  previousKeys?: Record<string, string>;
  allowedRoots: string[];
}

export interface ProjectCheckpointIO {
  capture(
    lease: LeaseRecord,
    root: string,
    allowedRoot: string,
    dependencyPolicy: "none" | "npm-lockfile-v1",
  ): Promise<unknown>;
  restore(
    lease: LeaseRecord,
    root: string,
    allowedRoot: string,
    snapshot: { content: string; sha256: string },
  ): Promise<unknown>;
}

function fail(code: string): never {
  throw new ProjectCheckpointError(code);
}
function nonempty(value: unknown, max = 1024): value is string {
  return (
    typeof value === "string" &&
    value.trim() === value &&
    value.length > 0 &&
    value.length <= max &&
    // oxlint-disable-next-line eslint/no-control-regex -- Checkpoint metadata must not contain control bytes.
    !/[\u0000-\u001f]/u.test(value)
  );
}
function canonicalRoot(value: unknown): value is string {
  return (
    nonempty(value) &&
    value.startsWith("/") &&
    !value.endsWith("/") &&
    !value.includes("\\") &&
    !value
      .split("/")
      .slice(1)
      .some((part) => part === "" || part === "." || part === "..")
  );
}
function toBase64(value: Uint8Array): string {
  let text = "";
  for (let i = 0; i < value.length; i += 8192)
    text += String.fromCharCode(...value.subarray(i, i + 8192));
  return btoa(text);
}
function fromBase64(value: string): Uint8Array {
  try {
    if (!/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(value))
      return fail("checkpoint_corrupt");
    return Uint8Array.from(atob(value), (c) => c.charCodeAt(0));
  } catch {
    return fail("checkpoint_corrupt");
  }
}
async function digest(value: Uint8Array): Promise<string> {
  return [...new Uint8Array(await crypto.subtle.digest("SHA-256", value))]
    .map((b) => b.toString(16).padStart(2, "0"))
    .join("");
}

const deniedParts = new Set([
  ".git",
  "browser-profile",
  "worker-browser",
  "google-chrome",
  "chromium",
  ".mozilla",
]);

function validateSymlinkGraph(tree: Map<string, Record<string, unknown>>): void {
  const links = new Map(
    [...tree]
      .filter(([, entry]) => entry["type"] === "symlink")
      .map(([path, entry]) => [path, entry["target"] as string]),
  );
  for (const [path, target] of links) {
    let expansions = 0;
    const resolve = (parent: string[], tokens: string[], active: Set<string>): string[] => {
      let parts = [...parent];
      for (const token of tokens) {
        if (token === "" || token === ".") continue;
        if (token === "..") {
          if (!parts.length) fail("checkpoint_symlink_escape");
          parts.pop();
          continue;
        }
        if (deniedParts.has(token)) fail("checkpoint_symlink_escape");
        const candidate = [...parts, token].join("/");
        const linked = links.get(candidate);
        if (linked !== undefined) {
          if (active.has(candidate) || ++expansions > 40) fail("checkpoint_symlink_cycle");
          // Expand the link before consuming a following '..', just like path lookup.
          parts = resolve(parts, linked.split("/"), new Set([...active, candidate]));
        } else parts.push(token);
      }
      return parts;
    };
    resolve(path.split("/").slice(0, -1), target.split("/"), new Set([path]));
  }
}

/** Validate the actual recovery document before a checkpoint can authorize deletion.
 * A digest alone can faithfully commit unusable or malicious recovery content.
 */
export async function validateProjectCheckpointContent(
  content: Uint8Array,
  expectedBytes: number,
): Promise<{ dependencyPolicy: "none" | "npm-lockfile-v1"; dependencyBytes: number }> {
  let document: Record<string, unknown>;
  try {
    document = JSON.parse(
      new TextDecoder("utf-8", { fatal: true, ignoreBOM: true }).decode(content),
    ) as Record<string, unknown>;
  } catch {
    return fail("checkpoint_corrupt");
  }
  if (
    !document ||
    typeof document !== "object" ||
    Array.isArray(document) ||
    document["schema"] !== fileSchema ||
    document["processState"] !== "not-captured" ||
    document["gitMetadata"] !== "excluded" ||
    document["bytes"] !== expectedBytes ||
    !Array.isArray(document["entries"]) ||
    document["entries"].length > 20_000
  )
    fail("checkpoint_corrupt");
  const seen = new Set<string>();
  const directories = new Set([""]);
  const files = new Map<string, string>();
  const tree = new Map<string, Record<string, unknown>>();
  let total = 0;
  for (const value of document["entries"]) {
    if (!value || typeof value !== "object" || Array.isArray(value)) fail("checkpoint_corrupt");
    const entry = value as Record<string, unknown>;
    const path = entry["path"];
    if (!nonempty(path, 4096) || path.includes("\\")) fail("checkpoint_corrupt");
    const parts = path.split("/");
    if (
      parts.some((part) => part === "" || part === "." || part === ".." || deniedParts.has(part)) ||
      seen.has(path) ||
      !directories.has(parts.slice(0, -1).join("/")) ||
      !Number.isSafeInteger(entry["mode"]) ||
      Number(entry["mode"]) < 0 ||
      Number(entry["mode"]) > 0o777
    )
      fail("checkpoint_corrupt");
    seen.add(path);
    tree.set(path, entry);
    if (entry["type"] === "directory") directories.add(path);
    else if (entry["type"] === "file") {
      if (typeof entry["data"] !== "string" || entry["data"].length > 45 * 1024 * 1024)
        fail("checkpoint_corrupt");
      const data = fromBase64(entry["data"]);
      total += data.byteLength;
      // oxlint-disable-next-line eslint/no-await-in-loop -- Validate each bounded file before accepting the manifest.
      if (total > 32 * 1024 * 1024 || (await digest(data)) !== entry["sha256"])
        fail("checkpoint_corrupt");
      files.set(path, entry["sha256"] as string);
    } else if (entry["type"] === "symlink") {
      const target = entry["target"];
      if (!nonempty(target, 4096) || target.startsWith("/") || target.includes("\\"))
        fail("checkpoint_corrupt");
      const resolved = parts.slice(0, -1);
      for (const part of target.split("/")) {
        if (part === "." || part === "") continue;
        if (part === "..") {
          if (!resolved.length) fail("checkpoint_corrupt");
          resolved.pop();
        } else resolved.push(part);
      }
      if (resolved.some((part) => deniedParts.has(part))) fail("checkpoint_corrupt");
    } else fail("checkpoint_corrupt");
  }
  if (total !== expectedBytes) fail("checkpoint_corrupt");
  const exclusions = document["exclusions"];
  if (!Array.isArray(exclusions) || exclusions.length > 1) fail("checkpoint_corrupt");
  if (!exclusions.length) {
    validateSymlinkGraph(tree);
    return { dependencyPolicy: "none", dependencyBytes: 0 };
  }
  const recipe = exclusions[0] as Record<string, unknown>;
  if (
    !recipe ||
    typeof recipe !== "object" ||
    recipe["policy"] !== "npm-lockfile-v1" ||
    recipe["path"] !== "node_modules" ||
    recipe["recipe"] !== "npm-ci-ignore-scripts/v1" ||
    typeof recipe["npmVersion"] !== "string" ||
    !/^\d+\.\d+\.\d+$/.test(recipe["npmVersion"]) ||
    typeof recipe["treeSHA256"] !== "string" ||
    !/^[a-f0-9]{64}$/.test(recipe["treeSHA256"]) ||
    !Number.isSafeInteger(recipe["entries"]) ||
    Number(recipe["entries"]) < 1 ||
    Number(recipe["entries"]) > 250_000 ||
    !Number.isSafeInteger(recipe["bytes"]) ||
    Number(recipe["bytes"]) < 0 ||
    Number(recipe["bytes"]) > 4 * 1024 ** 3
  )
    fail("checkpoint_corrupt");
  const inputs = recipe["inputs"] as Record<string, unknown>;
  if (
    !inputs ||
    Object.keys(inputs).length !== 2 ||
    !files.has("package.json") ||
    !files.has("package-lock.json") ||
    inputs["package.json"] !== files.get("package.json") ||
    inputs["package-lock.json"] !== files.get("package-lock.json")
  )
    fail("checkpoint_corrupt");
  for (const field of ["omittedPaths", "removedPaths"]) {
    const paths = recipe[field];
    if (
      !Array.isArray(paths) ||
      paths.length > 250_000 ||
      new Set(paths).size !== paths.length ||
      paths.some(
        (path) =>
          !nonempty(path, 4096) ||
          (path !== "node_modules" && !path.startsWith("node_modules/")) ||
          path.includes("\\") ||
          path
            .split("/")
            .some((part) => part === "" || part === "." || part === ".." || deniedParts.has(part)),
      )
    )
      fail("checkpoint_corrupt");
  }
  const symlinks = recipe["symlinks"];
  if (
    !symlinks ||
    typeof symlinks !== "object" ||
    Array.isArray(symlinks) ||
    Object.keys(symlinks).length > 250_000
  )
    fail("checkpoint_corrupt");
  const removed = new Set(recipe["removedPaths"] as string[]);
  const combined = new Map<string, Record<string, unknown>>();
  for (const [path, target] of Object.entries(symlinks)) {
    if (
      !nonempty(path, 4096) ||
      !path.startsWith("node_modules/") ||
      path.includes("\\") ||
      path
        .split("/")
        .some((part) => part === "" || part === "." || part === ".." || deniedParts.has(part)) ||
      !nonempty(target, 4096) ||
      target.startsWith("/") ||
      target.includes("\\")
    )
      fail("checkpoint_corrupt");
    if (!pathOrAncestorInSet(path, removed)) combined.set(path, { type: "symlink", target });
  }
  for (const [path, entry] of tree) combined.set(path, entry);
  validateSymlinkGraph(combined);
  return { dependencyPolicy: "npm-lockfile-v1", dependencyBytes: Number(recipe["bytes"]) };
}

function pathOrAncestorInSet(path: string, paths: ReadonlySet<string>): boolean {
  let candidate = path;
  for (;;) {
    if (paths.has(candidate)) return true;
    const separator = candidate.lastIndexOf("/");
    if (separator < 0) return false;
    candidate = candidate.slice(0, separator);
  }
}

function contentKey(
  binding: string,
  manifest: Pick<ProjectCheckpointManifest, "generation" | "revision">,
  index: number,
): string {
  return `${binding}:content:${manifest.generation}:${manifest.revision}:${index}`;
}
function aad(
  manifest: Omit<ProjectCheckpointManifest, "iv" | "chunks" | "ciphertextSHA256">,
): Uint8Array {
  // Explicit field order survives JSONB key reordering across storage implementations.
  const scope = manifest.scope;
  return utf8.encode(
    JSON.stringify([
      schema,
      scope.target,
      scope.authority,
      scope.owner,
      scope.org,
      scope.projectID,
      scope.sessionID,
      manifest.generation,
      manifest.revision,
      manifest.leaseID,
      manifest.leaseGeneration,
      manifest.acceptedRevision,
      manifest.capturedAt,
      manifest.contentSHA256,
      manifest.contentBytes,
      manifest.projectBytes,
      manifest.keyID,
      manifest.consistency,
      manifest.recovery,
      manifest.dependencyPolicy,
      manifest.dependencyBytes,
    ]),
  );
}

export class ProjectCheckpointStore {
  constructor(
    private readonly storage: CoordinatorStorage,
    private readonly config: ProjectCheckpointConfig,
    private readonly io: ProjectCheckpointIO,
    private readonly now: () => number = Date.now,
  ) {
    if (
      ![config.target, config.authority, config.keyID].every((v) => nonempty(v, 128)) ||
      typeof config.secret !== "string" ||
      config.secret.length < 32 ||
      !config.allowedRoots.length ||
      !config.allowedRoots.every(canonicalRoot) ||
      !Object.entries(config.previousKeys ?? {}).every(
        ([id, secret]) =>
          id !== config.keyID &&
          nonempty(id, 128) &&
          typeof secret === "string" &&
          secret.length >= 32,
      )
    )
      fail("checkpoint_storage_unconfigured");
  }

  private async encryptionKey(keyID = this.config.keyID): Promise<CryptoKey> {
    const secret =
      keyID === this.config.keyID ? this.config.secret : this.config.previousKeys?.[keyID];
    if (!secret) fail("checkpoint_key_unavailable");
    const key = await crypto.subtle.importKey("raw", utf8.encode(secret), "HKDF", false, [
      "deriveKey",
    ]);
    return crypto.subtle.deriveKey(
      {
        name: "HKDF",
        hash: "SHA-256",
        salt: utf8.encode(`${schema}:${keyID}`),
        info: utf8.encode(JSON.stringify([this.config.target, this.config.authority])),
      },
      key,
      { name: "AES-GCM", length: 256 },
      false,
      ["encrypt", "decrypt"],
    );
  }

  async bind(
    lease: LeaseRecord,
    input: Omit<ProjectCheckpointScope, "owner" | "org"> & {
      root: string;
      acceptedRevision: string;
      previousGeneration?: number;
      dependencyPolicy?: "none" | "npm-lockfile-v1";
    },
  ): Promise<ProjectCheckpointBinding> {
    if (
      input.target !== this.config.target ||
      input.authority !== this.config.authority ||
      !nonempty(input.projectID) ||
      !nonempty(input.sessionID) ||
      typeof input.acceptedRevision !== "string" ||
      !/^sha256:[a-f0-9]{64}$/.test(input.acceptedRevision) ||
      !canonicalRoot(input.root) ||
      (input.dependencyPolicy !== undefined &&
        !["none", "npm-lockfile-v1"].includes(input.dependencyPolicy))
    )
      fail("checkpoint_scope_mismatch");
    const dependencyPolicy = input.dependencyPolicy ?? "none";
    const allowedRoot = this.config.allowedRoots.find((prefix) =>
      input.root.startsWith(`${prefix}/`),
    );
    if (!allowedRoot) fail("checkpoint_root_denied");
    const scope: ProjectCheckpointScope = {
      target: input.target,
      authority: input.authority,
      owner: lease.owner,
      org: lease.org,
      projectID: input.projectID,
      sessionID: input.sessionID,
    };
    const key = `project-checkpoint:v1:${await sha256Hex(JSON.stringify([scope.target, scope.authority, scope.owner, scope.org, scope.projectID, scope.sessionID]))}`;
    return this.storage.transaction(async (transaction) => {
      const currentLease = await transaction.get<LeaseRecord>(`lease:${lease.id}`);
      this.assertLease(currentLease, lease);
      if (currentLease!.projectCheckpointKey && currentLease!.projectCheckpointKey !== key)
        fail("checkpoint_lease_already_bound");
      const previous = await transaction.get<ProjectCheckpointBinding>(key);
      if (
        previous?.leaseID === lease.id &&
        previous.leaseGeneration === lease.createAttemptGeneration
      ) {
        if (
          previous.root !== input.root ||
          previous.acceptedRevision !== input.acceptedRevision ||
          previous.dependencyPolicy !== dependencyPolicy
        )
          fail("checkpoint_binding_conflict");
        return previous;
      }
      if (previous) {
        if (
          input.previousGeneration !== previous.generation ||
          !["lost", "reclaim-ready"].includes(previous.state)
        )
          fail("checkpoint_previous_generation_active");
        const previousLease = await transaction.get<LeaseRecord>(`lease:${previous.leaseID}`);
        if (previousLease?.state === "active" && Date.parse(previousLease.expiresAt) > this.now())
          fail("checkpoint_previous_lease_active");
      } else if (input.previousGeneration !== undefined)
        fail("checkpoint_previous_generation_missing");
      const binding: ProjectCheckpointBinding = {
        schema,
        key,
        scope,
        root: input.root,
        allowedRoot,
        leaseID: lease.id,
        leaseGeneration: lease.createAttemptGeneration!,
        generation: (previous?.generation ?? 0) + 1,
        revision: previous?.revision ?? 0,
        acceptedRevision: input.acceptedRevision,
        dependencyPolicy,
        state: "running",
        updatedAt: new Date(this.now()).toISOString(),
        ...(previous?.checkpoint ? { checkpoint: previous.checkpoint } : {}),
        ...(previous?.loss ? { loss: previous.loss } : {}),
      };
      await transaction.put(key, binding);
      await transaction.put(`lease:${lease.id}`, {
        ...currentLease,
        projectCheckpointKey: key,
        projectCheckpointGeneration: binding.generation,
      });
      return binding;
    });
  }

  private assertLease(current: LeaseRecord | undefined, expected: LeaseRecord): void {
    if (
      !current ||
      current.state !== "active" ||
      current.cleanupStartedAt ||
      Date.parse(current.expiresAt) <= this.now() ||
      !current.createAttemptGeneration ||
      current.createAttemptGeneration !== expected.createAttemptGeneration ||
      current.owner !== expected.owner ||
      !sameOrgIdentityKey(current.org, expected.org) ||
      current.cloudID !== expected.cloudID
    )
      fail("checkpoint_lease_fenced");
  }

  private async current(
    transaction: CoordinatorStorageView,
    lease: LeaseRecord,
    generation: number,
    active = true,
  ): Promise<ProjectCheckpointBinding> {
    const currentLease = await transaction.get<LeaseRecord>(`lease:${lease.id}`);
    if (active) this.assertLease(currentLease, lease);
    if (
      !currentLease?.projectCheckpointKey ||
      currentLease.projectCheckpointGeneration !== generation ||
      currentLease.createAttemptGeneration !== lease.createAttemptGeneration
    )
      fail("checkpoint_generation_fenced");
    const binding = await transaction.get<ProjectCheckpointBinding>(
      currentLease.projectCheckpointKey,
    );
    if (
      !binding ||
      binding.schema !== schema ||
      binding.generation !== generation ||
      binding.leaseID !== lease.id ||
      binding.leaseGeneration !== lease.createAttemptGeneration ||
      binding.scope.owner !== lease.owner ||
      !sameOrgIdentityKey(binding.scope.org, lease.org) ||
      binding.scope.target !== this.config.target ||
      binding.scope.authority !== this.config.authority
    )
      fail("checkpoint_generation_fenced");
    return binding;
  }

  status(lease: LeaseRecord, generation: number): Promise<ProjectCheckpointBinding> {
    return this.storage.transaction((transaction) =>
      this.current(transaction, lease, generation, false),
    );
  }

  private async begin(
    lease: LeaseRecord,
    generation: number,
    state: "checkpointing" | "restoring",
  ): Promise<ProjectCheckpointBinding> {
    const operation = {
      id: crypto.randomUUID(),
      expiresAt: new Date(this.now() + operationTTLMS).toISOString(),
    };
    return this.storage.transaction(async (transaction) => {
      const current = await this.current(transaction, lease, generation);
      if (current.state === "lost" || current.state === "reclaim-ready")
        fail("checkpoint_project_ended");
      if (current.operation && Date.parse(current.operation.expiresAt) > this.now())
        fail("checkpoint_operation_in_progress");
      const next: ProjectCheckpointBinding = {
        ...current,
        state,
        operation,
        updatedAt: new Date(this.now()).toISOString(),
      };
      delete next.reasonCode;
      await transaction.put(next.key, next);
      return next;
    });
  }

  private async failOperation(
    binding: ProjectCheckpointBinding,
    reasonCode: "checkpoint-failed" | "restore-failed",
    failureCode: string,
  ): Promise<void> {
    await this.storage.transaction(async (transaction) => {
      const current = await transaction.get<ProjectCheckpointBinding>(binding.key);
      if (
        current?.generation !== binding.generation ||
        current?.operation?.id !== binding.operation?.id
      )
        return;
      const next: ProjectCheckpointBinding = {
        ...current,
        state: "blocked",
        reasonCode,
        failureCode,
        updatedAt: new Date(this.now()).toISOString(),
      };
      delete next.operation;
      await transaction.put(next.key, next);
    });
  }

  async capture(
    lease: LeaseRecord,
    generation: number,
    acceptedRevision: string,
    reclaim = false,
  ): Promise<ProjectCheckpointBinding> {
    if (typeof acceptedRevision !== "string" || !/^sha256:[a-f0-9]{64}$/.test(acceptedRevision))
      fail("checkpoint_revision_required");
    const binding = await this.begin(lease, generation, "checkpointing");
    try {
      const value = await this.io.capture(
        lease,
        binding.root,
        binding.allowedRoot,
        binding.dependencyPolicy,
      );
      if (!value || typeof value !== "object" || Array.isArray(value))
        fail("checkpoint_capture_invalid");
      const snapshot = value as Record<string, unknown>;
      if (
        snapshot["schema"] !== fileSchema ||
        snapshot["consistency"] !== "stable-tree" ||
        typeof snapshot["content"] !== "string" ||
        snapshot["content"].length > maxContentBytes * 1.4 ||
        typeof snapshot["sha256"] !== "string" ||
        !/^[a-f0-9]{64}$/.test(snapshot["sha256"]) ||
        !Number.isSafeInteger(snapshot["bytes"]) ||
        Number(snapshot["bytes"]) < 0 ||
        Number(snapshot["bytes"]) > 32 * 1024 * 1024
      )
        fail("checkpoint_capture_invalid");
      const content = fromBase64(snapshot["content"]);
      if (content.byteLength > maxContentBytes || (await digest(content)) !== snapshot["sha256"])
        fail("checkpoint_corrupt");
      const dependencies = await validateProjectCheckpointContent(
        content,
        Number(snapshot["bytes"]),
      );
      if (
        dependencies.dependencyPolicy !== "none" &&
        dependencies.dependencyPolicy !== binding.dependencyPolicy
      )
        fail("checkpoint_dependency_policy_unsupported");
      const metadata = {
        schema: schema as typeof schema,
        scope: binding.scope,
        generation,
        revision: binding.revision + 1,
        leaseID: lease.id,
        leaseGeneration: binding.leaseGeneration,
        acceptedRevision,
        capturedAt: new Date(this.now()).toISOString(),
        contentSHA256: snapshot["sha256"],
        contentBytes: content.byteLength,
        projectBytes: Number(snapshot["bytes"]),
        keyID: this.config.keyID,
        consistency: "stable-tree" as const,
        recovery: "filesystem-only" as const,
        ...dependencies,
      };
      const iv = crypto.getRandomValues(new Uint8Array(12));
      const ciphertext = new Uint8Array(
        await crypto.subtle.encrypt(
          { name: "AES-GCM", iv, additionalData: aad(metadata) },
          await this.encryptionKey(),
          content,
        ),
      );
      const manifest: ProjectCheckpointManifest = {
        ...metadata,
        iv: toBase64(iv),
        chunks: Math.ceil(ciphertext.byteLength / chunkBytes),
        ciphertextSHA256: await digest(ciphertext),
      };
      return await this.storage.transaction(async (transaction) => {
        const current = await this.current(transaction, lease, generation);
        if (
          current.operation?.id !== binding.operation?.id ||
          Date.parse(current.operation!.expiresAt) <= this.now()
        )
          fail("checkpoint_operation_fenced");
        for (let index = 0; index < manifest.chunks; index++) {
          // oxlint-disable-next-line eslint/no-await-in-loop -- Bounded chunks commit atomically with the manifest and current pointer.
          await transaction.put(
            contentKey(binding.key, manifest, index),
            toBase64(ciphertext.subarray(index * chunkBytes, (index + 1) * chunkBytes)),
          );
        }
        const next: ProjectCheckpointBinding = {
          ...current,
          revision: manifest.revision,
          checkpoint: manifest,
          acceptedRevision,
          state: reclaim ? "reclaim-ready" : "running",
          updatedAt: manifest.capturedAt,
        };
        delete next.operation;
        delete next.reasonCode;
        delete next.failureCode;
        await transaction.put(
          `${binding.key}:manifest:${manifest.generation}:${manifest.revision}`,
          manifest,
        );
        const previous = current.checkpoint;
        if (previous) {
          if (
            !Number.isSafeInteger(previous.chunks) ||
            previous.chunks < 1 ||
            previous.chunks > Math.ceil((maxContentBytes + 16) / chunkBytes)
          )
            fail("checkpoint_key_or_manifest_unavailable");
          for (let index = 0; index < previous.chunks; index++) {
            // oxlint-disable-next-line eslint/no-await-in-loop -- Superseded content is removed in the same atomic publication.
            await transaction.delete(contentKey(binding.key, previous, index));
          }
          await transaction.delete(
            `${binding.key}:manifest:${previous.generation}:${previous.revision}`,
          );
        }
        await transaction.put(binding.key, next);
        return next;
      });
    } catch (error) {
      const code =
        error instanceof ProjectCheckpointError && projectCheckpointFailureCodes.has(error.code)
          ? error.code
          : "checkpoint_failed";
      await this.failOperation(binding, "checkpoint-failed", code);
      return fail(`${code}_reclaim_held`);
    }
  }

  async restore(
    lease: LeaseRecord,
    generation: number,
  ): Promise<{
    binding: ProjectCheckpointBinding;
    recovery: ReturnType<typeof projectRecoveryPosition>;
  }> {
    const binding = await this.begin(lease, generation, "restoring");
    try {
      const manifest = binding.checkpoint;
      if (!manifest) fail("checkpoint_missing");
      if (
        !Number.isSafeInteger(manifest.chunks) ||
        manifest.chunks < 1 ||
        manifest.chunks > Math.ceil((maxContentBytes + 16) / chunkBytes)
      )
        fail("checkpoint_key_or_manifest_unavailable");
      if (binding.acceptedRevision !== manifest.acceptedRevision)
        fail("checkpoint_revision_conflict");
      if (
        manifest.scope.target !== binding.scope.target ||
        manifest.scope.authority !== binding.scope.authority ||
        manifest.scope.owner !== binding.scope.owner ||
        !sameOrgIdentityKey(manifest.scope.org, binding.scope.org) ||
        manifest.scope.projectID !== binding.scope.projectID ||
        manifest.scope.sessionID !== binding.scope.sessionID
      )
        fail("checkpoint_scope_mismatch");
      const chunks: Uint8Array[] = [];
      for (let index = 0; index < manifest.chunks; index++) {
        // oxlint-disable-next-line eslint/no-await-in-loop -- Keep restore memory and storage concurrency bounded.
        const chunk = await this.storage.get<string>(contentKey(binding.key, manifest, index));
        if (typeof chunk !== "string" || chunk.length > 65536) fail("checkpoint_missing");
        chunks.push(fromBase64(chunk));
      }
      const ciphertext = new Uint8Array(chunks.reduce((sum, chunk) => sum + chunk.byteLength, 0));
      let offset = 0;
      for (const chunk of chunks) {
        ciphertext.set(chunk, offset);
        offset += chunk.byteLength;
      }
      if ((await digest(ciphertext)) !== manifest.ciphertextSHA256) fail("checkpoint_corrupt");
      const content = new Uint8Array(
        await crypto.subtle.decrypt(
          { name: "AES-GCM", iv: fromBase64(manifest.iv), additionalData: aad(manifest) },
          await this.encryptionKey(manifest.keyID),
          ciphertext,
        ),
      );
      if (
        content.byteLength !== manifest.contentBytes ||
        (await digest(content)) !== manifest.contentSHA256
      )
        fail("checkpoint_corrupt");
      await validateProjectCheckpointContent(content, manifest.projectBytes);
      // Recheck the lease and operation after storage/key reads and before touching a runner.
      await this.storage.transaction(async (transaction) => {
        const current = await this.current(transaction, lease, generation);
        if (
          current.operation?.id !== binding.operation?.id ||
          Date.parse(current.operation!.expiresAt) <= this.now()
        )
          fail("checkpoint_operation_fenced");
      });
      const result = await this.io.restore(lease, binding.root, binding.allowedRoot, {
        content: toBase64(content),
        sha256: manifest.contentSHA256,
      });
      if (
        !result ||
        typeof result !== "object" ||
        (result as Record<string, unknown>)["recovery"] !== "filesystem-only" ||
        (result as Record<string, unknown>)["sha256"] !== manifest.contentSHA256
      )
        fail("checkpoint_restore_invalid");
      const committed = await this.storage.transaction(async (transaction) => {
        const current = await this.current(transaction, lease, generation);
        if (
          current.operation?.id !== binding.operation?.id ||
          Date.parse(current.operation!.expiresAt) <= this.now()
        )
          fail("checkpoint_operation_fenced");
        const next: ProjectCheckpointBinding = {
          ...current,
          state: "running",
          updatedAt: new Date(this.now()).toISOString(),
        };
        delete next.operation;
        delete next.reasonCode;
        delete next.failureCode;
        await transaction.put(next.key, next);
        return next;
      });
      return { binding: committed, recovery: projectRecoveryPosition(committed, this.now()) };
    } catch (error) {
      const code =
        error instanceof ProjectCheckpointError && projectCheckpointFailureCodes.has(error.code)
          ? error.code
          : "checkpoint_restore_failed";
      await this.failOperation(binding, "restore-failed", code);
      return fail(code);
    }
  }

  async recordLoss(
    lease: LeaseRecord,
    generation: number,
    kind: "crash" | "ttl" | "discard",
  ): Promise<ProjectCheckpointBinding> {
    return this.storage.transaction(async (transaction) => {
      const current = await this.current(transaction, lease, generation, false);
      const currentLease = await transaction.get<LeaseRecord>(`lease:${lease.id}`);
      if (kind === "ttl" && (!currentLease || Date.parse(currentLease.expiresAt) > this.now()))
        fail("checkpoint_ttl_not_elapsed");
      if (current.state === "lost") {
        if (current.loss?.kind !== kind) fail("checkpoint_loss_conflict");
        return current;
      }
      const at = new Date(this.now()).toISOString();
      const next: ProjectCheckpointBinding = {
        ...current,
        state: "lost",
        updatedAt: at,
        loss: {
          kind,
          at,
          lastCheckpointAt: current.checkpoint?.capturedAt ?? null,
          knownLostFrom: current.checkpoint?.capturedAt ?? null,
        },
      };
      delete next.operation;
      await transaction.put(next.key, next);
      return next;
    });
  }

  async beforeReclaim(lease: LeaseRecord): Promise<void> {
    if (!lease.projectCheckpointKey) return;
    const generation = lease.projectCheckpointGeneration;
    if (!Number.isSafeInteger(generation)) fail("checkpoint_generation_fenced");
    const current = await this.status(lease, generation!);
    if (current.state === "lost" || current.state === "reclaim-ready") return;
    if (Date.parse(lease.expiresAt) <= this.now()) {
      await this.recordLoss(lease, generation!, "ttl");
      return;
    }
    await this.capture(lease, generation!, current.acceptedRevision, true);
  }
}

export function projectRecoveryPosition(binding: ProjectCheckpointBinding, now = Date.now()) {
  return {
    kind: "filesystem-only" as const,
    checkpointRevision: binding.checkpoint?.revision ?? null,
    acceptedRevision: binding.checkpoint?.acceptedRevision ?? binding.acceptedRevision,
    contentSHA256: binding.checkpoint?.contentSHA256 ?? null,
    capturedAt: binding.checkpoint?.capturedAt ?? null,
    checkpointAgeMs: binding.checkpoint
      ? Math.max(0, now - Date.parse(binding.checkpoint.capturedAt))
      : null,
    dependencies: {
      policy: binding.checkpoint?.dependencyPolicy ?? "none",
      reconstructedBytes: binding.checkpoint?.dependencyBytes ?? 0,
    },
    loss: binding.loss ?? null,
    processResume: false,
  };
}
