import https from "node:https";
import { pathToFileURL } from "node:url";

const coordinator = "https://crabbox.openclaw.ai";
const authKinds = new Set(["bearer", "device", "github", "proxy"]);
const maxBodyBytes = 64 * 1024;
const deadlineMS = 10_000;

export async function verifyPublisherAuth(env, request = https.request) {
  const result = { status: "configuration_error", httpStatus: null, auth: "unknown", admin: false };
  const token = env.CRABBOX_COORDINATOR_ADMIN_TOKEN;
  const accessID = env.CRABBOX_ACCESS_CLIENT_ID;
  const accessSecret = env.CRABBOX_ACCESS_CLIENT_SECRET;
  // This deployment-owned check must never send publisher secrets to a configured alternate host.
  if (
    env.CRABBOX_COORDINATOR !== coordinator ||
    !token ||
    Boolean(accessID) !== Boolean(accessSecret)
  ) {
    return result;
  }
  const headers = { Authorization: `Bearer ${token}`, Accept: "application/json" };
  if (accessID && accessSecret) {
    headers["CF-Access-Client-Id"] = accessID;
    headers["CF-Access-Client-Secret"] = accessSecret;
  }
  return new Promise((resolve) => {
    let req;
    let response;
    let settled = false;
    const finish = (status) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      response?.destroy();
      req?.destroy();
      resolve({ ...result, status });
    };
    // A wall deadline covers DNS, TLS, headers, and the entire body, not just socket inactivity.
    const timer = setTimeout(() => finish("timeout"), deadlineMS);
    try {
      req = request(
        `${coordinator}/v1/whoami`,
        {
          method: "GET",
          headers,
          agent: false,
          rejectUnauthorized: true,
          maxHeaderSize: 16 * 1024,
        },
        (res) => {
          response = res;
          res.on("error", () => finish("response_error"));
          res.on("aborted", () => finish("response_error"));
          res.on("close", () => finish("response_error"));
          if (settled) return res.destroy();
          result.httpStatus = res.statusCode;
          // Native HTTPS does not follow redirects; discard every non-200 body without logging it.
          if (res.statusCode !== 200) return finish("http_error");
          const chunks = [];
          let bytes = 0;
          res.on("data", (chunk) => {
            if (settled) return;
            bytes += chunk.length;
            if (bytes > maxBodyBytes) return finish("response_too_large");
            chunks.push(chunk);
          });
          res.on("end", () => {
            if (settled) return;
            let body;
            try {
              body = JSON.parse(Buffer.concat(chunks).toString("utf8"));
            } catch {
              return finish("invalid_response");
            }
            if (
              !body ||
              !authKinds.has(body.auth) ||
              typeof body.admin !== "boolean" ||
              typeof body.owner !== "string" ||
              typeof body.org !== "string"
            ) {
              return finish("invalid_response");
            }
            result.auth = body.auth;
            result.admin = body.admin === true;
            finish(result.admin ? "verified" : "not_admin");
          });
        },
      );
      req.on("error", () => finish("request_error"));
      req.on("close", () => finish("request_error"));
      req.end();
    } catch {
      finish("request_error");
    }
  });
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const result = await verifyPublisherAuth(process.env);
  console.log(JSON.stringify(result));
  process.exitCode = result.status === "verified" ? 0 : 1;
}
