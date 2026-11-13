import { isAdminRequest } from "./auth";
import { json } from "./http";
import type { Env } from "./types";

// Runs after normal coordinator authentication and before Fleet. No run, lease or
// AWS operation is admitted here, and no candidate HTTP reply is an attestation.
export async function qualificationTransportProbeResponse(
  request: Request,
  env: Env,
): Promise<Response | undefined> {
  const url = new URL(request.url);
  if (url.pathname !== "/v1/qualification/transport-probe") return undefined;
  if (
    request.method !== "POST" ||
    url.search ||
    !isAdminRequest(request) ||
    !env.CRABBOX_AWS_QUALIFICATION_TRANSPORT?.observeBinding
  )
    return json({ error: "not_found" }, { status: 404 });
  const reader = request.body?.getReader();
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    const work = (async () => {
      if (!reader) throw new Error("missing challenge");
      let text = "";
      const decoder = new TextDecoder();
      let size = 0;
      for (;;) {
        // Reads on one bounded stream must remain ordered.
        // eslint-disable-next-line no-await-in-loop
        const { value, done } = await reader.read();
        if (done) break;
        size += value.byteLength;
        if (size > 128) throw new Error("challenge too large");
        text += decoder.decode(value, { stream: true });
      }
      text += decoder.decode();
      const input = JSON.parse(text);
      if (
        !input ||
        Object.keys(input).join(",") !== "challenge" ||
        typeof input.challenge !== "string" ||
        !/^[0-9a-f]{64}$/.test(input.challenge)
      )
        throw new Error("invalid challenge");
      await env.CRABBOX_AWS_QUALIFICATION_TRANSPORT!.observeBinding!(input.challenge);
      return json({ accepted: true }, { headers: { "cache-control": "no-store" } });
    })();
    return await Promise.race([
      work,
      new Promise<Response>((_, reject) => {
        timer = setTimeout(() => reject(new Error("probe timeout")), 5000);
      }),
    ]);
  } catch {
    return json({ error: "transport_probe_failed" }, { status: 409 });
  } finally {
    if (timer !== undefined) clearTimeout(timer);
    void reader?.cancel().catch(() => {});
  }
}
