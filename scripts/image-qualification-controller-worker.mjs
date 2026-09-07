const maxBodyBytes = 4096;

function json(value, status = 200) {
  return new Response(JSON.stringify(value), {
    status,
    headers: { "content-type": "application/json; charset=utf-8" },
  });
}

async function body(request) {
  const bytes = new Uint8Array(await request.arrayBuffer());
  if (bytes.byteLength > maxBodyBytes) throw new Error("request body too large");
  if (bytes.byteLength === 0) return {};
  const value = JSON.parse(new TextDecoder().decode(bytes));
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new Error("request body must be an object");
  }
  return value;
}

async function authorized(request, token) {
  const header = request.headers.get("authorization") ?? "";
  const supplied = header.startsWith("Bearer ") ? header.slice(7) : "";
  if (token.length < 32 || supplied.length !== token.length) return false;
  const [left, right] = await Promise.all(
    [supplied, token].map(
      async (value) =>
        new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(value))),
    ),
  );
  let difference = 0;
  for (let index = 0; index < left.length; index += 1) difference |= left[index] ^ right[index];
  return difference === 0;
}

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    if (url.pathname === "/executor") {
      try {
        // Same-zone Worker subrequests can replace x-real-ip. Admit only direct edge
        // requests; never use forwarded/client-supplied address overrides.
        if (
          request.method !== "POST" ||
          url.search ||
          !request.cf ||
          ["cf-worker", "x-real-ip", "cf-connecting-ipv6", "cf-pseudo-ipv4"].some((name) =>
            request.headers.has(name),
          )
        ) {
          return json({ error: "forbidden" }, 403);
        }
        const input = await body(request);
        if (Object.keys(input).length !== 1 || typeof input.runId !== "string")
          throw new Error("invalid registration");
        const token = (request.headers.get("authorization") ?? "").replace(/^Bearer /, "");
        return json(
          await env.AUTHORITY.executorReady(
            input.runId,
            token,
            request.headers.get("cf-connecting-ip") ?? "",
          ),
        );
      } catch {
        return json({ error: "preflight_rejected" }, 409);
      }
    }
    if (!(await authorized(request, env.CONTROLLER_TOKEN ?? ""))) {
      return json({ error: "forbidden" }, 403);
    }
    if (request.method !== "POST") return json({ error: "method_not_allowed" }, 405);
    try {
      const input = await body(request);
      switch (new URL(request.url).pathname) {
        case "/prepare-executor":
          await env.AUTHORITY.prepareExecutor(input.runId, input.tokenDigest);
          return json({ prepared: true });
        case "/network":
          return json(await env.AUTHORITY.network(input.runId));
        case "/prepare-network":
          return json(await env.AUTHORITY.prepareNetwork(input.runId));
        case "/dispatch-network":
          return json(await env.AUTHORITY.dispatchNetwork(input.runId, input.attemptId));
        case "/confirm-network":
          await env.AUTHORITY.confirmNetwork(input.runId, input.attemptId, input.ruleId);
          return json({ confirmed: true });
        case "/confirm-network-revocation":
          await env.AUTHORITY.confirmNetworkRevocation(input.runId, input.attemptId, input.ruleId);
          return json({ confirmed: true });
        case "/clear-network":
          await env.AUTHORITY.clearNetwork(input.runId, input.attemptId);
          return json({ cleared: true });
        case "/arm-execution":
          await env.AUTHORITY.armExecution(input.runId);
          return json({ armed: true });
        case "/claim":
          return json(await env.AUTHORITY.claim(input.identity));
        case "/begin-finalization":
          await env.AUTHORITY.beginFinalization(input.runId);
          return json({ finalizing: true });
        case "/finalize":
          return json(await env.AUTHORITY.finalize(input.runId));
        case "/attest":
          return json(await env.AUTHORITY.attest(input.runId));
        case "/discover":
          return json({ run: (await env.AUTHORITY.discover()) ?? null });
        case "/retire":
          await env.AUTHORITY.retire(input.runId);
          return json({ retired: true });
        default:
          return json({ error: "not_found" }, 404);
      }
    } catch {
      return json({ error: "controller_error" }, 409);
    }
  },
};
