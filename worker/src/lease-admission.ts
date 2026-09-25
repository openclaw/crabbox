import {
  legacyAlarmKey,
  setLegacyWake,
  type CoordinatorRuntime,
  type CoordinatorStorageView,
} from "./coordinator-runtime";

export function isCoordinatorReset(error: unknown): boolean {
  return error instanceof Error && /Durable Object.*reset/i.test(error.message);
}

// Only admission/publication call this. Both callbacks must be storage-only;
// a failed readback retains uncertainty and must never authorize provider dispatch.
export async function commitLeaseAdmission<T>(
  runtime: CoordinatorRuntime,
  commit: (storage: CoordinatorStorageView) => Promise<T>,
  reread: (storage: CoordinatorStorageView) => Promise<T | undefined>,
): Promise<T> {
  const deadline = Date.now() + 500;
  /* oxlint-disable eslint/no-await-in-loop -- serialized bounded storage retries */
  for (let attempt = 0; ; attempt++) {
    try {
      return await (runtime.provisioning
        ? runtime.provisioning.commitAndWake(commit)
        : runtime.storage.transaction(commit));
    } catch (error) {
      const resolved = await runtime.storage.transaction(reread);
      if (resolved !== undefined) return resolved;
      const transient =
        error instanceof Error &&
        "retryable" in error &&
        error.retryable === true &&
        !isCoordinatorReset(error);
      const delay = 25 * 2 ** attempt + Math.floor(Math.random() * 25);
      if (!transient || attempt >= 2 || Date.now() + delay >= deadline) throw error;
      await new Promise((resolve) => setTimeout(resolve, delay));
      if (Date.now() >= deadline) throw error;
    }
  }
  /* oxlint-enable eslint/no-await-in-loop */
}

export async function retainLeaseWake(
  storage: CoordinatorStorageView,
  deadline: number,
): Promise<void> {
  const current = await storage.get<number | null>(legacyAlarmKey);
  await setLegacyWake(storage, current == null ? deadline : Math.min(current, deadline));
}

// The outer Worker can acquire a fresh stub after a reset. Never retry an
// unbound POST, arbitrary mutation, provider error, or returned HTTP 5xx here.
export async function fetchReplayableLeaseCreate(
  request: Request,
  fetch: (request: Request) => Promise<Response>,
): Promise<Response> {
  if (request.method !== "POST" || new URL(request.url).pathname !== "/v1/leases") {
    return fetch(request);
  }
  const body = (await request
    .clone()
    .json()
    .catch(() => undefined)) as { leaseID?: unknown; createAttemptID?: unknown } | undefined;
  if (
    typeof body?.leaseID !== "string" ||
    !/^cbx_[a-f0-9]{12}$/.test(body.leaseID) ||
    typeof body.createAttemptID !== "string" ||
    !/^cat_[a-f0-9]{32}$/.test(body.createAttemptID)
  ) {
    return fetch(request);
  }
  const replay = request.clone();
  try {
    return await fetch(request);
  } catch (error) {
    if (!isCoordinatorReset(error)) throw error;
    await new Promise((resolve) => setTimeout(resolve, 25 + Math.floor(Math.random() * 50)));
    return fetch(replay);
  }
}
