import { describe, expect, it } from "vitest";

import { WayVNCRetirement } from "../src/wayvnc-retirement";

function socket() {
  const sent: string[] = [];
  const value = {
    readyState: 1,
    send: (message: string) => {
      sent.push(message);
    },
  };
  return { socket: value as unknown as WebSocket, sent, value };
}
function binding(
  retirement: WayVNCRetirement,
  target: WebSocket,
  client: string,
  server = "boot:100:20",
) {
  retirement.handle(target, JSON.stringify({ type: "wayvnc_binding", client, server }));
}

describe("WayVNC retirement acknowledgement", () => {
  it("accepts only the exact previous bridge's response to the current request", async () => {
    const retirement = new WayVNCRetirement();
    const old = socket(),
      next = socket(),
      other = socket();
    binding(retirement, old.socket, "1");
    binding(retirement, next.socket, "2");
    const result = retirement.retire(old.socket, next.socket, [old.socket, next.socket]);
    const request = JSON.parse(old.sent[0]!);
    expect(request.clients).toEqual(["1", "2"]);
    expect(request.successor).toBe("2");
    let finished = false;
    void result.then(() => {
      finished = true;
      return undefined;
    });
    retirement.handle(
      other.socket,
      JSON.stringify({ type: "wayvnc_retired", request: request.request, retired: true }),
    );
    retirement.handle(
      old.socket,
      JSON.stringify({ type: "wayvnc_retired", request: "stale", retired: true }),
    );
    await Promise.resolve();
    expect(finished).toBe(false);
    retirement.handle(
      old.socket,
      JSON.stringify({ type: "wayvnc_retired", request: request.request, retired: true }),
    );
    expect(await result).toBe(true);
  });

  it("falls back after a bounded deadline, including a local transport close without acknowledgement", async () => {
    const retirement = new WayVNCRetirement();
    const old = socket(),
      next = socket();
    binding(retirement, old.socket, "1");
    binding(retirement, next.socket, "2");
    const result = retirement.retire(old.socket, next.socket, [old.socket, next.socket], 10);
    old.value.readyState = 3;
    expect(await result).toBe(false);
  });

  it.each(["unbound", "different lifetime", "duplicate client"])(
    "rejects %s identities",
    async (mode) => {
      const retirement = new WayVNCRetirement();
      const old = socket(),
        next = socket();
      binding(retirement, old.socket, "1");
      if (mode !== "unbound")
        binding(
          retirement,
          next.socket,
          mode === "duplicate client" ? "1" : "2",
          mode === "different lifetime" ? "new-boot:100:20" : undefined,
        );
      expect(await retirement.retire(old.socket, next.socket, [old.socket, next.socket])).toBe(
        false,
      );
      expect(old.sent).toHaveLength(0);
    },
  );

  it("does not accept acknowledgement after the successor connection closes", async () => {
    const retirement = new WayVNCRetirement();
    const old = socket(),
      next = socket();
    binding(retirement, old.socket, "1");
    binding(retirement, next.socket, "2");
    const result = retirement.retire(old.socket, next.socket, [old.socket, next.socket]);
    const request = JSON.parse(old.sent[0]!);
    next.value.readyState = 3;
    retirement.handle(
      old.socket,
      JSON.stringify({ type: "wayvnc_retired", request: request.request, retired: true }),
    );
    expect(await result).toBe(false);
  });
});
