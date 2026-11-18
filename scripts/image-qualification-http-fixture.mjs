// Offline fault-test transport: fixtures still traverse a real HTTP connection.
import http from "node:http";
import { once } from "node:events";
const network = globalThis.fetch;
export async function serveQualificationFixture(handler) {
  const server = http.createServer(async (req, res) => {
    const chunks = [];
    for await (const chunk of req) chunks.push(chunk);
    const bytes = Buffer.concat(chunks);
    const originalURL = req.headers["x-qualification-fixture-url"];
    const request = new Request(originalURL, {
      method: req.method,
      headers: Object.fromEntries(
        Object.entries(req.headers).filter(
          ([name]) => !["host", "connection", "x-qualification-fixture-url"].includes(name),
        ),
      ),
      ...(bytes.length ? { body: bytes } : {}),
    });
    const cancel = new AbortController();
    res.on("close", () => cancel.abort());
    try {
      const multipart = request.headers.get("content-type")?.startsWith("multipart/form-data");
      const reply = await handler(originalURL, {
        method: req.method,
        headers: Object.fromEntries(request.headers),
        signal: cancel.signal,
        ...(bytes.length
          ? { body: multipart ? await request.formData() : bytes.toString("utf8") }
          : {}),
      });
      res.setHeader("connection", "close");
      res.writeHead(reply.status, Object.fromEntries(reply.headers));
      if (reply.body) for await (const data of reply.body) res.write(data);
      res.end();
    } catch {
      res.destroy();
    }
  });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  server.unref();
  const base = `http://127.0.0.1:${server.address().port}`;
  return async (input, init = {}) =>
    network(base, {
      ...init,
      headers: {
        ...init.headers,
        "x-qualification-fixture-url": String(input),
      },
    });
}
