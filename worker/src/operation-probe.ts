// Temporary, operation-only diagnostics. Native tail events provide request correlation.
// Remove after the bounded investigation; never pass records, request data, or errors here.
const operations = new Set([
  "lease_history",
  "lease_scan",
  "lease_page",
  "runner_history",
  "run_event_history",
  "aws_images_body",
  "aws_images_parse",
  "aws_ec2_body",
  "aws_ec2_parse",
  "aws_sts_body",
  "aws_sts_parse",
  "aws_ssm_body",
  "aws_ssm_parse",
  "aws_quota_body",
] as const);
type Operation = typeof operations extends Set<infer T> ? T : never;
type Measurements = { count?: number; codeUnits?: number };

function emit(
  operation: Operation,
  phase: "start" | "end" | "error",
  startedAt: number,
  measurements?: Measurements,
): void {
  try {
    if (!operations.has(operation)) return;
    const event: Record<string, string | number> = {
      component: "crabbox_operation_probe",
      operation,
      phase,
    };
    if (phase !== "start")
      event["durationMs"] = Math.max(0, Math.min(Number.MAX_SAFE_INTEGER, Date.now() - startedAt));
    for (const key of ["count", "codeUnits"] as const) {
      const value = measurements?.[key];
      if (typeof value === "number" && Number.isSafeInteger(value) && value >= 0)
        event[key] = value;
    }
    console.debug(JSON.stringify(event));
  } catch {
    // Diagnostics must never affect the observed operation or its error.
  }
}

export function startOperationProbe(
  operation: Operation,
): (measurements?: Measurements, failed?: boolean) => void {
  const startedAt = Date.now();
  emit(operation, "start", startedAt);
  return (measurements, failed = false) =>
    emit(operation, failed ? "error" : "end", startedAt, measurements);
}

export async function observeOperation<T>(
  operation: Operation,
  callback: () => Promise<T>,
  measurements?: (value: T) => Measurements,
): Promise<T> {
  const finish = startOperationProbe(operation);
  let value: T;
  try {
    value = await callback();
  } catch (error) {
    finish(undefined, true);
    throw error;
  }
  let scalars: Measurements | undefined;
  try {
    scalars = measurements?.(value);
  } catch {
    /* Observation only. */
  }
  finish(scalars);
  return value;
}

export function observeOperationSync<T>(operation: Operation, callback: () => T): T {
  const finish = startOperationProbe(operation);
  try {
    const value = callback();
    finish();
    return value;
  } catch (error) {
    finish(undefined, true);
    throw error;
  }
}
