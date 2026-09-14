import type { LeaseTelemetry } from "./types";

export function orderedTelemetrySamples(
  samples: readonly (LeaseTelemetry | undefined)[],
): LeaseTelemetry[] {
  const byTime = new Map<string, LeaseTelemetry>();
  for (const sample of samples) {
    if (sample?.capturedAt) {
      byTime.set(sample.capturedAt, sample);
    }
  }
  return [...byTime.values()].toSorted((left, right) =>
    left.capturedAt.localeCompare(right.capturedAt),
  );
}
