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
    if (!(await authorized(request, env.CONTROLLER_TOKEN ?? ""))) {
      return json({ error: "forbidden" }, 403);
    }
    if (request.method !== "POST") return json({ error: "method_not_allowed" }, 405);
    try {
      const input = await body(request);
      switch (new URL(request.url).pathname) {
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
