import { describe, expect, it } from "vitest";

import { orderedTelemetrySamples } from "../src/telemetry";

describe("orderedTelemetrySamples", () => {
  it("orders out-of-order timestamps", () => {
    const samples = [
      { capturedAt: "2026-05-01T00:00:30.000Z", load1: 3 },
      { capturedAt: "2026-05-01T00:00:10.000Z", load1: 1 },
      { capturedAt: "2026-05-01T00:00:20.000Z", load1: 2 },
    ];

    expect(orderedTelemetrySamples(samples)).toEqual([
      { capturedAt: "2026-05-01T00:00:10.000Z", load1: 1 },
      { capturedAt: "2026-05-01T00:00:20.000Z", load1: 2 },
      { capturedAt: "2026-05-01T00:00:30.000Z", load1: 3 },
    ]);
  });

  it("keeps the last whole sample for duplicate timestamps", () => {
    const samples = [
      {
        capturedAt: "2026-05-01T00:00:10.000Z",
        load1: 1,
        memoryPercent: 10,
        diskPercent: 20,
      },
      { capturedAt: "2026-05-01T00:00:20.000Z", load1: 2 },
      { capturedAt: "2026-05-01T00:00:10.000Z", load1: 3 },
    ];

    expect(orderedTelemetrySamples(samples)).toEqual([
      { capturedAt: "2026-05-01T00:00:10.000Z", load1: 3 },
      { capturedAt: "2026-05-01T00:00:20.000Z", load1: 2 },
    ]);
  });

  it("preserves sample object identity", () => {
    const first = { capturedAt: "2026-05-01T00:00:10.000Z", load1: 1 };
    const replaced = { capturedAt: "2026-05-01T00:00:20.000Z", load1: 2 };
    const winner = { capturedAt: "2026-05-01T00:00:20.000Z", load1: 3 };

    const result = orderedTelemetrySamples([replaced, first, winner]);

    expect(result).toHaveLength(2);
    expect(result[0]).toBe(first);
    expect(result[1]).toBe(winner);
  });

  it("returns a fresh array without changing frozen inputs", () => {
    const samples = Object.freeze([
      Object.freeze({ capturedAt: "2026-05-01T00:00:20.000Z", load1: 2 }),
      Object.freeze({ capturedAt: "2026-05-01T00:00:10.000Z", load1: 1 }),
    ]);

    const result = orderedTelemetrySamples(samples);

    expect(result).not.toBe(samples);
    expect(orderedTelemetrySamples(samples)).not.toBe(result);
    expect(result).toEqual([
      { capturedAt: "2026-05-01T00:00:10.000Z", load1: 1 },
      { capturedAt: "2026-05-01T00:00:20.000Z", load1: 2 },
    ]);
    expect(samples).toEqual([
      { capturedAt: "2026-05-01T00:00:20.000Z", load1: 2 },
      { capturedAt: "2026-05-01T00:00:10.000Z", load1: 1 },
    ]);
  });

  it("filters undefined samples and empty timestamps", () => {
    expect(
      orderedTelemetrySamples([
        undefined,
        { capturedAt: "", load1: 9 },
        { capturedAt: "2026-05-01T00:00:10.000Z", load1: 1 },
        undefined,
      ]),
    ).toEqual([{ capturedAt: "2026-05-01T00:00:10.000Z", load1: 1 }]);
    expect(orderedTelemetrySamples([undefined, { capturedAt: "" }])).toEqual([]);
  });

  it("does not cap the number of samples", () => {
    const samples = Array.from({ length: 1_000 }, (_, index) => ({
      capturedAt: new Date(Date.UTC(2026, 4, 1, 0, 0, index)).toISOString(),
      load1: index,
    }));

    const result = orderedTelemetrySamples(samples);

    expect(result).toHaveLength(1_000);
    expect(result).toEqual(samples);
    expect(result[0]).toBe(samples[0]);
    expect(result.at(-1)).toBe(samples.at(-1));
  });
});
