export default {
  async fetch(request, env) {
    const supplied = request.headers.get("authorization") ?? "";
    const token = env.PROBE_TOKEN ?? "";
    if (!/^[A-Za-z0-9]{64}$/.test(token) || supplied.length !== token.length + 7) {
      return new Response(null, { status: 403 });
    }
    const hashes = await Promise.all(
      [supplied, `Bearer ${token}`].map(
        async (value) =>
          new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(value))),
      ),
    );
    let different = 0;
    for (let i = 0; i < hashes[0].length; i++) different |= hashes[0][i] ^ hashes[1][i];
    if (different) return new Response(null, { status: 403 });
    if (request.method !== "POST" || new URL(request.url).pathname !== "/observe") {
      return new Response(null, { status: 405 });
    }
    // Caller input cannot select a service, method, props, or forwarded URL.
    return Response.json(await env.RECEIVER.describe());
  },
};
