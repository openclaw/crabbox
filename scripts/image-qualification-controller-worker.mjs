const maxBodyBytes = 4096;

function json(value, status = 200) {
  return new Response(JSON.stringify(value), {
    status,
    headers: { "content-type": "application/json; charset=utf-8", "cache-control": "no-store" },
  });
}

async function bounded(promise) {
  let timer;
  try {
    return await Promise.race([
      promise,
      new Promise((_, reject) => {
        timer = setTimeout(() => reject(new Error("controller timeout")), 5000);
      }),
    ]);
  } finally {
    clearTimeout(timer);
  }
}

async function body(request) {
  const reader = request.body?.getReader();
  if (!reader) return {};
  const chunks = [];
  let length = 0;
  try {
    await bounded(
      (async () => {
        for (;;) {
          const { done, value } = await reader.read();
          if (done) break;
          length += value.byteLength;
          if (length > maxBodyBytes) throw new Error("request body too large");
          chunks.push(value);
        }
      })(),
    );
  } finally {
    // Cancellation must not wait for a hostile producer to acknowledge it.
    void reader.cancel().catch(() => {});
  }
  if (!length) return {};
  const bytes = new Uint8Array(length);
  let offset = 0;
  for (const chunk of chunks) {
    bytes.set(chunk, offset);
    offset += chunk.length;
  }
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
    if (!(await authorized(request, env.CONTROLLER_TOKEN ?? ""))) {
      return json({ error: "forbidden" }, 403);
    }
    if (request.method !== "POST") return json({ error: "method_not_allowed" }, 405);
    try {
      const pathname = new URL(request.url).pathname;
      if (
        env.IDLE_CONTROLLER_ONLY === "1" &&
        !["/binding-identity", "/discover"].includes(pathname)
      ) {
        return json({ error: "not_found" }, 404);
      }
      const input = await body(request);
      if (env.IDLE_CONTROLLER_ONLY === "1" && pathname === "/discover" && Object.keys(input).length)
        throw new Error("invalid idle input");
      switch (pathname) {
        case "/binding-identity": {
          if (
            Object.keys(input).length !== 1 ||
            typeof input.challenge !== "string" ||
            !/^[0-9a-f]{64}$/.test(input.challenge)
          ) {
            throw new Error("invalid challenge");
          }
          const controllerVersion = env.CF_VERSION_METADATA?.id;
          if (
            typeof controllerVersion !== "string" ||
            !/^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/.test(controllerVersion)
          ) {
            throw new Error("executing version missing");
          }
          const identity = await bounded(env.AUTHORITY.bindingIdentity(input.challenge));
          const reply = { identity, controllerVersion };
          if (new TextEncoder().encode(JSON.stringify(reply)).byteLength > maxBodyBytes)
            throw new Error("reply too large");
          const response = json(reply);
          response.headers.set("Cache-Control", "no-store");
          return response;
        }
        case "/transport-open":
          return json(await bounded(env.AUTHORITY.openTransportProbe(input)));
        case "/transport-discover":
          if (Object.keys(input).length) throw new Error("invalid discovery");
          return json({ probe: (await bounded(env.AUTHORITY.discoverTransportProbe())) ?? null });
        case "/transport-settle":
          if (Object.keys(input).sort().join(",") !== "challenge,verified")
            throw new Error("invalid settlement");
          return json(
            await bounded(env.AUTHORITY.settleTransportProbe(input.challenge, input.verified)),
          );
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
          return json({ run: (await bounded(env.AUTHORITY.discover())) ?? null });
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
