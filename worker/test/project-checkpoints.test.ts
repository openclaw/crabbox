import { createHash } from "node:crypto";

import { describe, expect, it, vi } from "vitest";

import { orgKeyForLabel } from "../src/org-identity";
import {
  ProjectCheckpointError,
  ProjectCheckpointStore,
  projectRecoveryPosition,
  type ProjectCheckpointConfig,
  type ProjectCheckpointBinding,
  type ProjectCheckpointIO,
} from "../src/project-checkpoints";
import type { LeaseRecord } from "../src/types";
import { ProvisioningTestStorage } from "./provisioning-fixtures";

const digest = (value: string | Buffer) => createHash("sha256").update(value).digest("hex");
const revision = `sha256:${"a".repeat(64)}`;
const config: ProjectCheckpointConfig = {
  target: "personal",
  authority: "personal-coordinator",
  keyID: "key-1",
  secret: "synthetic-session-secret-with-at-least-32-characters",
  allowedRoots: ["/home/worker/workspaces"],
};

function snapshot(text = "uncommitted and ignored project bytes") {
  const content = Buffer.from(
    JSON.stringify({
      schema: "crabbox-project-files/v1",
      bytes: Buffer.byteLength(text),
      gitMetadata: "excluded",
      processState: "not-captured",
      exclusions: [],
      entries: [
        {
          path: ".ignored-work.txt",
          type: "file",
          mode: 0o600,
          data: Buffer.from(text).toString("base64"),
          sha256: digest(text),
        },
      ],
    }),
  );
  return {
    schema: "crabbox-project-files/v1",
    content: content.toString("base64"),
    sha256: digest(content),
    bytes: Buffer.byteLength(text),
    consistency: "stable-tree",
  };
}

async function fixture() {
  const storage = new ProvisioningTestStorage();
  let now = Date.parse("2026-09-13T12:00:00Z");
  const lease = {
    id: "cbx_project",
    owner: "alice@example.com",
    org: orgKeyForLabel("example-org"),
    provider: "koyeb",
    cloudID: "service-1",
    createAttemptGeneration: "generation-1",
    state: "active",
    expiresAt: new Date(now + 3600_000).toISOString(),
  } as LeaseRecord;
  await storage.put(`lease:${lease.id}`, lease);
  const io = {
    capture: vi.fn<() => Promise<ReturnType<typeof snapshot>>>(async () => snapshot()),
    restore: vi.fn<ProjectCheckpointIO["restore"]>(async (_lease, _root, _allowed, data) => ({
      recovery: "filesystem-only",
      sha256: data.sha256,
    })),
  } satisfies ProjectCheckpointIO;
  const store = new ProjectCheckpointStore(storage, config, io, () => now);
  const bindInput = {
    target: config.target,
    authority: config.authority,
    projectID: "project-1",
    sessionID: "session-1",
    root: "/home/worker/workspaces/project-1",
    acceptedRevision: revision,
  };
  const binding = await store.bind(lease, bindInput);
  const boundLease = (await storage.get<LeaseRecord>(`lease:${lease.id}`))!;
  return {
    storage,
    lease: boundLease,
    binding,
    bindInput,
    io,
    store,
    now: () => now,
    advance: (ms: number) => {
      now += ms;
    },
  };
}

async function replacement(f: Awaited<ReturnType<typeof fixture>>, customConfig = config) {
  await f.storage.put(`lease:${f.lease.id}`, { ...f.lease, state: "released" });
  const lease = {
    ...f.lease,
    id: "cbx_replacement",
    cloudID: "service-2",
    createAttemptGeneration: "generation-2",
  };
  delete lease.projectCheckpointKey;
  delete lease.projectCheckpointGeneration;
  await f.storage.put(`lease:${lease.id}`, lease);
  const store = new ProjectCheckpointStore(f.storage, customConfig, f.io, f.now);
  const binding = await store.bind(lease, {
    ...f.bindInput,
    previousGeneration: f.binding.generation,
  });
  return { lease: (await f.storage.get<LeaseRecord>(`lease:${lease.id}`))!, binding, store };
}

describe("durable project filesystem checkpoints", () => {
  it("atomically encrypts user bytes and restores them on a new fenced lease", async () => {
    const f = await fixture();
    await f.store.beforeReclaim(f.lease);
    const captured = await f.store.status(f.lease, 1);
    expect(captured).toMatchObject({
      state: "reclaim-ready",
      revision: 1,
      checkpoint: {
        acceptedRevision: revision,
        leaseGeneration: "generation-1",
        recovery: "filesystem-only",
      },
    });
    expect(JSON.stringify([...f.storage.values])).not.toContain(
      "uncommitted and ignored project bytes",
    );
    f.advance(5000);
    const next = await replacement(f);
    const restored = await next.store.restore(next.lease, next.binding.generation);
    expect(restored.recovery).toMatchObject({
      processResume: false,
      kind: "filesystem-only",
      checkpointRevision: 1,
      acceptedRevision: revision,
      checkpointAgeMs: 5000,
    });
    expect(f.io.restore.mock.calls[0]![3]).toEqual({
      content: snapshot().content,
      sha256: snapshot().sha256,
    });
    await expect(f.store.capture(f.lease, 1, revision)).rejects.toBeInstanceOf(
      ProjectCheckpointError,
    );
  });

  it("a failed fresh checkpoint holds ordinary reclaim even after an earlier successful capture", async () => {
    const f = await fixture();
    await f.store.capture(f.lease, 1, revision);
    f.io.capture.mockRejectedValue(
      new ProjectCheckpointError("checkpoint_unpreserved_content_limit"),
    );
    await expect(f.store.beforeReclaim(f.lease)).rejects.toMatchObject({
      code: "checkpoint_unpreserved_content_limit_reclaim_held",
    });
    expect(await f.store.status(f.lease, 1)).toMatchObject({
      state: "blocked",
      revision: 1,
      failureCode: "checkpoint_unpreserved_content_limit",
    });
    expect(await f.storage.get(`lease:${f.lease.id}`)).toMatchObject({ state: "active" });
  });

  it("rejects a digest-valid unusable filesystem document before committing", async () => {
    const f = await fixture();
    const content = Buffer.from(
      JSON.stringify({
        schema: "crabbox-project-files/v1",
        entries: [{ path: "../../outside", type: "file", mode: 0o600, data: "" }],
        bytes: 0,
        gitMetadata: "excluded",
        processState: "not-captured",
        exclusions: [],
      }),
    );
    f.io.capture.mockResolvedValue({
      ...snapshot(),
      bytes: 0,
      content: content.toString("base64"),
      sha256: digest(content),
    });
    await expect(f.store.beforeReclaim(f.lease)).rejects.toMatchObject({
      code: "checkpoint_corrupt_reclaim_held",
    });
    expect([...f.storage.values.keys()].filter((key) => key.includes(":content:"))).toEqual([]);
  });

  it("rolls back chunk writes if publishing the manifest fails", async () => {
    const f = await fixture();
    await f.store.capture(f.lease, 1, revision);
    const before = [...f.storage.values.keys()].filter((key) => key.includes(":content:"));
    f.storage.failKey = `${f.binding.key}:manifest:1:2`;
    await expect(f.store.beforeReclaim(f.lease)).rejects.toMatchObject({
      code: "checkpoint_failed_reclaim_held",
    });
    expect([...f.storage.values.keys()].filter((key) => key.includes(":content:"))).toEqual(before);
    expect(await f.store.status(f.lease, 1)).toMatchObject({ revision: 1, state: "blocked" });
  });

  it("removes superseded checkpoint chunks and manifests with the pointer update", async () => {
    const f = await fixture();
    const first = await f.store.capture(f.lease, 1, revision);
    const previous = first.checkpoint!;
    f.io.capture.mockResolvedValue(snapshot("replacement project bytes"));

    const second = await f.store.capture(f.lease, 1, revision);

    expect(second.checkpoint?.revision).toBe(2);
    expect(
      [...f.storage.values.keys()].filter((key) =>
        key.startsWith(`${f.binding.key}:content:${previous.generation}:${previous.revision}:`),
      ),
    ).toEqual([]);
    expect(
      await f.storage.get(`${f.binding.key}:manifest:${previous.generation}:${previous.revision}`),
    ).toBeUndefined();
    expect(
      [...f.storage.values.keys()].filter((key) =>
        key.startsWith(
          `${f.binding.key}:content:${second.checkpoint!.generation}:${second.checkpoint!.revision}:`,
        ),
      ),
    ).toHaveLength(second.checkpoint!.chunks);
  });

  it.each(["escape", "cycle"])(
    "holds reclaim for a digest-valid chained symlink %s",
    async (kind) => {
      const f = await fixture();
      const content = Buffer.from(
        JSON.stringify({
          schema: "crabbox-project-files/v1",
          bytes: 0,
          gitMetadata: "excluded",
          processState: "not-captured",
          exclusions: [],
          entries: [
            { path: "dir", type: "directory", mode: 0o755 },
            { path: "dir/up", type: "symlink", mode: 0o777, target: ".." },
            {
              path: "escape",
              type: "symlink",
              mode: 0o777,
              target: kind === "escape" ? "dir/up/../outside" : "escape",
            },
          ],
        }),
      );
      f.io.capture.mockResolvedValue({
        ...snapshot(),
        bytes: 0,
        content: content.toString("base64"),
        sha256: digest(content),
      });
      await expect(f.store.beforeReclaim(f.lease)).rejects.toMatchObject({
        code: `checkpoint_symlink_${kind}_reclaim_held`,
      });
      expect(await f.store.status(f.lease, 1)).toMatchObject({ state: "blocked", revision: 0 });
    },
  );

  it("rejects non-manifest accepted revisions without starting capture", async () => {
    const f = await fixture();
    await expect(f.store.capture(f.lease, 1, "latest")).rejects.toMatchObject({
      code: "checkpoint_revision_required",
    });
    expect(f.io.capture).not.toHaveBeenCalled();
  });

  it("fences captures that finish after discard and preserves the explicit loss kind", async () => {
    const f = await fixture();
    let finish!: (value: ReturnType<typeof snapshot>) => void;
    let started!: () => void;
    const ready = new Promise<void>((resolve) => {
      started = resolve;
    });
    f.io.capture.mockImplementation(async () => {
      started();
      return new Promise((resolve) => {
        finish = resolve;
      });
    });
    const pending = f.store.capture(f.lease, 1, revision);
    await ready;
    await f.store.recordLoss(f.lease, 1, "discard");
    finish(snapshot());
    await expect(pending).rejects.toBeInstanceOf(ProjectCheckpointError);
    expect(await f.store.status(f.lease, 1)).toMatchObject({
      state: "lost",
      revision: 0,
      loss: { kind: "discard", lastCheckpointAt: null },
    });
    expect([...f.storage.values.keys()].filter((key) => key.includes(":content:"))).toEqual([]);
  });

  it("uses the latest heartbeat when deciding TTL loss and keeps crash/discard distinct", async () => {
    const f = await fixture();
    await f.store.capture(f.lease, 1, revision);
    f.advance(3600_001);
    await f.storage.put(`lease:${f.lease.id}`, {
      ...f.lease,
      expiresAt: new Date(f.now() + 3600_000).toISOString(),
    });
    await expect(f.store.recordLoss(f.lease, 1, "ttl")).rejects.toMatchObject({
      code: "checkpoint_ttl_not_elapsed",
    });
    f.advance(3600_001);
    await f.store.beforeReclaim(f.lease);
    const lost = await f.store.status(f.lease, 1);
    expect(projectRecoveryPosition(lost, f.now()).loss).toMatchObject({
      kind: "ttl",
      knownLostFrom: lost.checkpoint!.capturedAt,
    });
    await expect(f.store.recordLoss(f.lease, 1, "crash")).rejects.toMatchObject({
      code: "checkpoint_loss_conflict",
    });
  });

  it.each(["missing", "corrupt", "scope"])(
    "fails closed before remote restore on %s content",
    async (kind) => {
      const f = await fixture();
      await f.store.beforeReclaim(f.lease);
      const next = await replacement(f);
      const chunk = [...f.storage.values.keys()].find((key) => key.includes(":content:"))!;
      if (kind === "missing") await f.storage.delete(chunk);
      else if (kind === "corrupt") await f.storage.put(chunk, "AAAA");
      else {
        const binding = (await f.storage.get<ProjectCheckpointBinding>(f.binding.key))!;
        binding.checkpoint!.scope.target = "team";
        await f.storage.put(f.binding.key, binding);
      }
      await expect(next.store.restore(next.lease, 2)).rejects.toBeInstanceOf(
        ProjectCheckpointError,
      );
      expect(f.io.restore).not.toHaveBeenCalled();
      expect(await next.store.status(next.lease, 2)).toMatchObject({
        state: "blocked",
        reasonCode: "restore-failed",
      });
    },
  );

  it("supports explicit retained key versions and denies cross-target authority", async () => {
    const f = await fixture();
    await f.store.beforeReclaim(f.lease);
    const next = await replacement(f, {
      ...config,
      keyID: "key-2",
      secret: "replacement-session-secret-with-at-least-32-characters",
      previousKeys: { "key-1": config.secret },
    });
    expect((await next.store.restore(next.lease, 2)).recovery.processResume).toBe(false);
    const wrong = new ProjectCheckpointStore(
      f.storage,
      { ...config, authority: "team-coordinator" },
      f.io,
      f.now,
    );
    await expect(wrong.status(next.lease, 2)).rejects.toMatchObject({
      code: "checkpoint_generation_fenced",
    });
  });
});
