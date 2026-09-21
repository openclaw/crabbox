/** Remote client IDs are meaningful only within the relay's unchanged server lifetime. */
interface Binding {
  client: string;
  server: string;
}

interface Pending {
  socket: WebSocket;
  finish: (retired: boolean) => void;
}

export class WayVNCRetirement {
  private readonly bindings = new WeakMap<WebSocket, Binding>();
  private readonly pending = new Map<string, Pending>();

  handle(socket: WebSocket, message: unknown): boolean {
    if (typeof message !== "string") return false;
    let data: {
      type?: unknown;
      client?: unknown;
      server?: unknown;
      request?: unknown;
      retired?: unknown;
    };
    try {
      data = JSON.parse(message);
    } catch {
      return false;
    }
    if (!data || typeof data !== "object") return false;
    if (data.type === "wayvnc_binding") {
      if (
        !this.bindings.has(socket) &&
        typeof data.client === "string" &&
        /^\d{1,20}$/.test(data.client) &&
        typeof data.server === "string" &&
        /^[a-zA-Z0-9:-]{1,128}$/.test(data.server)
      ) {
        this.bindings.set(socket, { client: data.client, server: data.server });
      }
      return true;
    }
    if (data.type !== "wayvnc_retired") return false;
    const pending = typeof data.request === "string" ? this.pending.get(data.request) : undefined;
    if (pending?.socket === socket) pending.finish(data.retired === true);
    return true;
  }

  async retire(
    previous: WebSocket,
    successor: WebSocket,
    agents: WebSocket[],
    timeout = 8000,
  ): Promise<boolean> {
    const before = this.bindings.get(previous);
    const after = this.bindings.get(successor);
    if (
      !before ||
      !after ||
      before.server !== after.server ||
      before.client === after.client ||
      previous.readyState !== WebSocket.OPEN ||
      successor.readyState !== WebSocket.OPEN
    )
      return false;
    const clients = agents
      .map((socket) => this.bindings.get(socket))
      .filter((binding): binding is Binding => binding?.server === before.server)
      .map((binding) => binding.client);
    if (new Set(clients).size !== clients.length || clients.length > 128) return false;
    const request = crypto.randomUUID();
    return await new Promise<boolean>((resolve) => {
      const finish = (retired: boolean) => {
        clearTimeout(timer);
        this.pending.delete(request);
        resolve(retired && successor.readyState === WebSocket.OPEN);
      };
      const timer = setTimeout(() => finish(false), timeout);
      this.pending.set(request, { socket: previous, finish });
      try {
        previous.send(
          JSON.stringify({
            type: "wayvnc_retire",
            request,
            server: before.server,
            successor: after.client,
            clients,
          }),
        );
      } catch {
        finish(false);
      }
    });
  }
}
