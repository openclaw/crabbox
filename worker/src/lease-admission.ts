import {
  legacyAlarmKey,
  setLegacyWake,
  type CoordinatorRuntime,
  type CoordinatorStorageView,
} from "./coordinator-runtime";
import { json } from "./http";

export const leaseAdmissionDeadlineMs = 30_000;

export class LeaseAdmissionExpiredError extends Error {
  constructor() {
    super(
      "lease admission did not begin committing before its deadline; retry the same create attempt",
    );
  }
}

export class LeaseAdmissionFence {
  private state: "pending" | "committing" | "expired" = "pending";

  assertActive(): void {
    if (this.state === "expired") throw new LeaseAdmissionExpiredError();
  }

  beginCommit(): void {
    this.assertActive();
    this.state = "committing";
  }

  async run(
    operation: () => Promise<Response>,
    deadlineMs = leaseAdmissionDeadlineMs,
  ): Promise<Response> {
    let timer!: ReturnType<typeof setTimeout>;
    const expired = new Promise<Response>((resolve) => {
      timer = setTimeout(() => {
        if (this.state !== "pending") return;
        this.state = "expired";
        resolve(
          json(
            {
              error: "lease_admission_timeout",
              message: new LeaseAdmissionExpiredError().message,
              retryable: true,
            },
            { status: 503, headers: { "retry-after": "2" } },
          ),
        );
      }, deadlineMs);
    });
    try {
      // The race owns rejection handling even after expired admission resumes in the background.
      return await Promise.race([operation(), expired]);
    } finally {
      clearTimeout(timer);
    }
  }
}

function isCoordinatorReset(error: unknown): boolean {
  return error instanceof Error && /Durable Object.*reset/i.test(error.message);
}

// Callbacks must be storage-only; failed rereads cannot authorize provider dispatch.
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
      // Expired work never committed and must not adopt another request's later admission.
      if (error instanceof LeaseAdmissionExpiredError) throw error;
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
