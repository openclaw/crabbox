import { Script, createContext } from "node:vm";

import { describe, expect, it } from "vitest";

import { portalVNC } from "../src/portal";
import type { LeaseRecord, TargetOS } from "../src/types";

type State = { viewerRole?: string; controllerID?: string; controllerLabel?: string };

function settle() {
  return new Promise<void>((resolve) => setImmediate(resolve));
}

class Element {
  value = "";
  hidden = true;
  disabled = false;
  textContent = "";
  dataset: Record<string, string> = {};
  listeners = new Map<string, () => unknown>();
  addEventListener(name: string, listener: () => unknown) {
    this.listeners.set(name, listener);
  }
  focus() {}
  replaceChildren() {}
  async fire(name: string) {
    await this.listeners.get(name)?.();
  }
}

async function viewer(target: TargetOS = "linux", desktopEnv = "wayland") {
  const page = await portalVNC({
    id: "cbx_sizing",
    provider: "hetzner",
    target,
    desktopEnv,
  } as LeaseRecord).text();
  const script = [...page.matchAll(/<script\b[^>]*>([\s\S]*?)<\/script>/g)]
    .map((match) => match[1]!)
    .find((body) => body.includes("import RFBModule"))!
    .replace(/import RFBModule from [^;]+;/, "const RFBModule = FakeRFB;");
  const elements = new Map<string, Element>();
  for (const id of [
    "screen",
    "status",
    "vnc-sizing",
    "vnc-sizing-notice",
    "vnc-takeover",
    "vnc-reconnect",
  ]) {
    elements.set(id, new Element());
  }
  elements.get("vnc-sizing")!.value = target === "linux" ? "match" : "fit";
  const clients: FakeRFB[] = [];
  class FakeRFB extends Element {
    viewOnly = false;
    scaleViewport = false;
    changes: boolean[] = [];
    resize = false;
    constructor() {
      super();
      clients.push(this);
    }
    get resizeSession() {
      return this.resize;
    }
    set resizeSession(value: boolean) {
      // A request must never be issued before viewOnly is cleared.
      if (value) expect(this.viewOnly).toBe(false);
      this.resize = value;
      this.changes.push(value);
    }
    disconnect() {
      void this.fire("disconnect");
    }
  }
  let state: State = { viewerRole: "observer", controllerID: "other" };
  let statusReply: (() => Promise<State>) | undefined;
  let controlReply: (() => Promise<State>) | undefined;
  let controlFetch: (() => Promise<void>) | undefined;
  let controlCalls = 0;
  const intervals = new Map<number, () => unknown>();
  let timerID = 0;
  const context = createContext({
    FakeRFB,
    URL,
    URLSearchParams,
    crypto,
    document: {
      getElementById: (id: string) => elements.get(id) ?? null,
      body: { dataset: {} },
      documentElement: { dataset: {} },
    },
    navigator: {},
    window: {
      location: {
        href: "https://example.test/portal/leases/cbx_sizing/vnc",
        protocol: "https:",
        hash: "",
      },
      history: { state: null },
      addEventListener() {},
      clearTimeout() {},
      setTimeout: () => ++timerID,
      setInterval: (callback: () => unknown) => {
        intervals.set(++timerID, callback);
        return timerID;
      },
      clearInterval: (id: number) => intervals.delete(id),
    },
    fetch: async (url: URL) => {
      if (url.pathname.endsWith("/control")) {
        controlCalls++;
        await controlFetch?.();
        return {
          ok: true,
          json: async () =>
            controlReply ? controlReply() : { viewerRole: "controller", controllerID: "self" },
        };
      }
      return {
        ok: true,
        json: async () => ({
          bridgeConnected: true,
          availableViewerSlots: 1,
          ...(statusReply ? await statusReply() : state),
        }),
      };
    },
  });
  new Script(script).runInContext(context);
  await settle();
  return {
    page,
    clients,
    elements,
    settle,
    setState: (value: State) => {
      state = value;
    },
    holdStatus: (reply?: () => Promise<State>) => {
      statusReply = reply;
    },
    holdControl: (reply?: () => Promise<State>) => {
      controlReply = reply;
    },
    holdControlFetch: (reply?: () => Promise<void>) => {
      controlFetch = reply;
    },
    controlCalls: () => controlCalls,
    poll: async () => {
      for (const callback of intervals.values()) callback();
      await settle();
    },
    connect: async () => {
      await clients.at(-1)!.fire("connect");
      await settle();
    },
    mode: async (value: string) => {
      elements.get("vnc-sizing")!.value = value;
      await elements.get("vnc-sizing")!.fire("change");
    },
    reconnect: async () => {
      await elements.get("vnc-reconnect")!.fire("click");
      await settle();
    },
  };
}

describe("emitted WebVNC sizing policy", () => {
  it("requests matching only for the connected controller and does not resend on unchanged polls", async () => {
    const v = await viewer();
    const rfb = v.clients[0]!;
    expect(rfb.resizeSession).toBe(false);
    await v.connect();
    expect(rfb.viewOnly).toBe(true);
    expect(rfb.resizeSession).toBe(false);
    v.setState({ viewerRole: "controller", controllerID: "self" });
    await v.poll();
    expect(rfb.scaleViewport).toBe(true);
    expect(rfb.resizeSession).toBe(true);
    const writes = rfb.changes.length;
    await v.poll();
    expect(rfb.changes).toHaveLength(writes);
    await v.mode("fit");
    expect(rfb.resizeSession).toBe(false);
    await v.poll();
    expect(rfb.resizeSession).toBe(false);
    await v.mode("match");
    expect(rfb.resizeSession).toBe(true);
    v.setState({ viewerRole: "observer", controllerID: "another" });
    await v.poll();
    expect(rfb.viewOnly).toBe(true);
    expect(rfb.resizeSession).toBe(false);
  });

  it("keeps the Wayland takeover notice visible across status polls, including mobile text", async () => {
    const v = await viewer();
    await v.connect();
    await v.elements.get("vnc-takeover")!.fire("click");
    const notice = v.elements.get("vnc-sizing-notice")!;
    expect(notice.hidden).toBe(false);
    expect(v.page).toContain("Close that viewer, then reconnect.");
    v.setState({ viewerRole: "controller", controllerID: "self" });
    await v.poll();
    expect(notice.hidden).toBe(false);
    await v.mode("fit");
    expect(notice.hidden).toBe(true);
    await v.mode("match");
    expect(notice.hidden).toBe(false);
  });

  it("does not show a Wayland warning for known XFCE", async () => {
    const v = await viewer("linux", "xfce");
    await v.connect();
    await v.elements.get("vnc-takeover")!.fire("click");
    expect(v.elements.get("vnc-sizing-notice")!.hidden).toBe(true);
  });

  it.each(["macos", "windows"] as const)(
    "keeps %s on Fit until explicitly requested",
    async (target) => {
      const v = await viewer(target);
      v.setState({ viewerRole: "controller", controllerID: "self" });
      await v.connect();
      expect(v.clients[0]!.resizeSession).toBe(false);
      await v.mode("match");
      expect(v.clients[0]!.resizeSession).toBe(true);
    },
  );

  it("discards a status response superseded by takeover", async () => {
    const v = await viewer();
    await v.connect();
    let resolve!: (state: State) => void;
    v.holdStatus(
      () =>
        new Promise((done) => {
          resolve = done;
        }),
    );
    await v.poll();
    await v.elements.get("vnc-takeover")!.fire("click");
    resolve({ viewerRole: "observer", controllerID: "other" });
    await v.settle();
    expect(v.clients[0]!.resizeSession).toBe(true);
  });

  it("coalesces slow polls without starving controller confirmation", async () => {
    const v = await viewer();
    await v.connect();
    let calls = 0;
    let resolve!: (state: State) => void;
    v.holdStatus(() => {
      calls++;
      return new Promise((done) => {
        resolve = done;
      });
    });
    await v.poll();
    await v.poll();
    await v.poll();
    expect(calls).toBe(1);
    resolve({ viewerRole: "controller", controllerID: "self" });
    await v.settle();
    expect(v.clients[0]!.resizeSession).toBe(true);
  });

  it("does not paint a reconnected viewer with an obsolete takeover rejection", async () => {
    const v = await viewer();
    await v.connect();
    let reject!: (error: Error) => void;
    v.holdControlFetch(
      () =>
        new Promise((_, fail) => {
          reject = fail;
        }),
    );
    const takeover = v.elements.get("vnc-takeover")!.fire("click");
    await v.settle();
    await v.reconnect();
    await v.connect();
    reject(new Error("obsolete failure"));
    await takeover;
    expect(v.elements.get("status")!.textContent).toBe("connected");
    expect(v.clients[1]!.resizeSession).toBe(false);
  });

  it("fences old status, control, and RFB events when reconnecting", async () => {
    const v = await viewer();
    await v.connect();
    const old = v.clients[0]!;
    let resolve!: (state: State) => void;
    v.holdControl(
      () =>
        new Promise((done) => {
          resolve = done;
        }),
    );
    const takeover = v.elements.get("vnc-takeover")!.fire("click");
    await v.settle();
    await v.reconnect();
    resolve({ viewerRole: "controller", controllerID: "self" });
    await takeover;
    await v.connect();
    await old.fire("connect");
    await old.fire("disconnect");
    await old.fire("securityfailure");
    await v.settle();
    expect(v.clients).toHaveLength(2);
    expect(v.clients[1]!.viewOnly).toBe(true);
    expect(v.clients[1]!.resizeSession).toBe(false);
    expect(v.elements.get("status")!.textContent).toBe("connected");
    expect(v.controlCalls()).toBe(1);
  });
});
