const maxResponseBytes = 64 * 1024;
const leaseId = "[A-Za-z0-9_-]{1,128}";
const runId = "[A-Za-z0-9_-]{1,128}";
const imageId = "ami-[0-9a-f]{8,32}";
const forbiddenHeaders = [
  "x-http-method-override",
  "x-original-url",
  "x-rewrite-url",
  "x-forwarded-host",
];

function json(value, status) {
  return new Response(JSON.stringify(value), {
    status,
    headers: { "content-type": "application/json; charset=utf-8" },
  });
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

function requireNoQuery(url) {
  return url.search === "";
}

function requireLeaseReadQuery(url) {
  if (url.search === "") return true;
  return (
    [...url.searchParams.keys()].length === 1 &&
    url.searchParams.getAll("providerMetadata").length === 1 &&
    url.searchParams.get("providerMetadata") === "authoritative"
  );
}

function requireImageQuery(url, env) {
  const allowed = new Set(["provider", "target", "region", "serverType", "kind", "architecture"]);
  const keys = [...url.searchParams.keys()];
  if (
    keys.some((key) => !allowed.has(key)) ||
    keys.some((key) => url.searchParams.getAll(key).length !== 1) ||
    url.searchParams.get("provider") !== "aws" ||
    url.searchParams.get("target") !== "linux" ||
    url.searchParams.get("region") !== env.AWS_REGION
  ) {
    return false;
  }
  const serverType = url.searchParams.get("serverType");
  const kind = url.searchParams.get("kind");
  const architecture = url.searchParams.get("architecture");
  return (
    (serverType === null || serverType === "t3.small") &&
    (kind === null || kind === "aws-ami") &&
    (architecture === null || architecture === "x86_64")
  );
}

function canonicalQuery(url, query, env) {
  if (query === requireLeaseReadQuery) {
    return url.search === "" ? "" : "?providerMetadata=authoritative";
  }
  if (query === requireImageQuery) {
    const values = new URLSearchParams({
      provider: "aws",
      region: env.AWS_REGION,
      target: "linux",
    });
    for (const key of ["architecture", "kind", "serverType"]) {
      const value = url.searchParams.get(key);
      if (value !== null) values.set(key, value);
    }
    return `?${values.toString()}`;
  }
  return "";
}

function routeFor(request, env) {
  const url = new URL(request.url);
  if (url.pathname.includes("%") || url.hash) return undefined;
  let pathname = url.pathname;
  let principal = "admin";
  const sharedPrefix = "/qualification/shared";
  if (pathname.startsWith(`${sharedPrefix}/`)) {
    principal = "shared";
    pathname = pathname.slice(sharedPrefix.length);
  }

  const rules = [
    {
      method: "POST",
      pattern: /^\/v1\/leases$/,
      maxBody: 32 * 1024,
      preferAsync: true,
      payload: "lease-create",
    },
    {
      method: "GET",
      pattern: new RegExp(`^/v1/leases/${leaseId}$`),
      maxBody: 0,
      query: requireLeaseReadQuery,
    },
    {
      method: "POST",
      pattern: new RegExp(`^/v1/leases/${leaseId}/(?:release|heartbeat)$`),
      maxBody: 16 * 1024,
    },
    { method: "POST", pattern: /^\/v1\/images$/, maxBody: 16 * 1024 },
    {
      method: "GET",
      pattern: new RegExp(`^/v1/images/${imageId}$`),
      maxBody: 0,
      query: requireImageQuery,
    },
    {
      method: "POST",
      pattern: new RegExp(`^/v1/images/${imageId}/(?:promote|promote-cas|fast-snapshot-restore)$`),
      maxBody: 16 * 1024,
      query: requireImageQuery,
    },
    {
      method: "POST",
      pattern: /^\/v1\/runs$/,
      maxBody: 64 * 1024,
      payload: "run-create",
    },
    {
      method: "POST",
      pattern: new RegExp(`^/v1/runs/${runId}/events$`),
      maxBody: 64 * 1024,
    },
    {
      method: "POST",
      pattern: new RegExp(`^/v1/runs/${runId}/telemetry$`),
      maxBody: 256 * 1024,
    },
    {
      method: "POST",
      pattern: new RegExp(`^/v1/runs/${runId}/finish$`),
      maxBody: 9 * 1024 * 1024,
    },
  ];
  const rule = rules.find(
    (candidate) => candidate.method === request.method && candidate.pattern.test(pathname),
  );
  if (!rule || !(rule.query ? rule.query(url, env) : requireNoQuery(url))) return undefined;
  if (
    principal === "shared" &&
    !(request.method === "POST" && new RegExp(`^/v1/images/${imageId}/promote-cas$`).test(pathname))
  ) {
    return undefined;
  }
  return { ...rule, pathname, principal, search: canonicalQuery(url, rule.query, env) };
}

function validTargetPayload(value, route, env) {
  if (route.payload === "lease-create") {
    return (
      value?.provider === "aws" &&
      value?.target === "linux" &&
      value?.class === "standard" &&
      value?.serverType === "t3.small" &&
      value?.awsRegion === env.AWS_REGION &&
      value?.desktop === false &&
      value?.browser === false &&
      value?.tailscale === false &&
      value?.capacity?.market === "on-demand"
    );
  }
  if (route.payload === "run-create") {
    return (
      value?.provider === "aws" &&
      value?.target === "linux" &&
      (value?.class === "" || value?.class === "standard") &&
      (value?.serverType === "" || value?.serverType === "t3.small")
    );
  }
  return true;
}

async function readBounded(stream, maximum) {
  if (!stream) return new Uint8Array();
  const reader = stream.getReader();
  const chunks = [];
  let length = 0;
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      length += value.byteLength;
      if (length > maximum) {
        await reader.cancel();
        throw new Error("body too large");
      }
      chunks.push(value);
    }
  } finally {
    reader.releaseLock();
  }
  const bytes = new Uint8Array(length);
  let offset = 0;
  for (const chunk of chunks) {
    bytes.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return bytes;
}

async function requestBody(request, route, env) {
  const declared = request.headers.get("content-length");
  if (declared && (!/^[0-9]+$/.test(declared) || Number(declared) > route.maxBody)) {
    throw new Error("request body too large");
  }
  if (route.maxBody === 0) {
    if (request.body || declared !== null || request.headers.has("content-type")) {
      throw new Error("body not allowed");
    }
    return undefined;
  }
  const contentType = request.headers.get("content-type")?.toLowerCase() ?? "";
  if (!/^application\/json(?:;\s*charset=utf-8)?$/.test(contentType)) {
    throw new Error("content type not allowed");
  }
  const bytes = await readBounded(request.body, route.maxBody);
  if (bytes.byteLength === 0 || bytes.byteLength > route.maxBody) {
    throw new Error("request body size invalid");
  }
  const value = JSON.parse(new TextDecoder().decode(bytes));
  if (
    !value ||
    typeof value !== "object" ||
    Array.isArray(value) ||
    !validTargetPayload(value, route, env)
  ) {
    throw new Error("request body contract invalid");
  }
  return bytes;
}

function headersAllowed(request, route) {
  if (forbiddenHeaders.some((name) => request.headers.has(name))) return false;
  const prefer = request.headers.get("prefer");
  return route.preferAsync ? prefer === null || prefer === "respond-async" : prefer === null;
}

function redactCandidateTokens(bytes, env) {
  let text = new TextDecoder().decode(bytes);
  for (const secret of [env.CANDIDATE_ADMIN_TOKEN, env.CANDIDATE_SHARED_TOKEN]) {
    if (typeof secret === "string" && secret.length >= 32) {
      text = text.replaceAll(secret, "[candidate-token]");
    }
  }
  return new TextEncoder().encode(text);
}

export default {
  async fetch(request, env) {
    if (!(await authorized(request, env.EXECUTOR_TOKEN ?? ""))) {
      return json({ error: "forbidden" }, 403);
    }
    const route = routeFor(request, env);
    if (!route || !headersAllowed(request, route)) {
      return json({ error: "not_allowed" }, 404);
    }
    let body;
    try {
      body = await requestBody(request, route, env);
    } catch {
      return json({ error: "invalid_request" }, 400);
    }
    const candidateToken =
      route.principal === "shared" ? env.CANDIDATE_SHARED_TOKEN : env.CANDIDATE_ADMIN_TOKEN;
    const headers = new Headers({
      authorization: `Bearer ${candidateToken}`,
      "x-crabbox-owner": env.QUALIFICATION_OWNER,
      "x-crabbox-org": env.QUALIFICATION_ORG,
    });
    if (body) headers.set("content-type", "application/json");
    if (route.preferAsync) headers.set("prefer", "respond-async");
    const response = await env.CANDIDATE.fetch(
      new Request(`https://candidate.invalid${route.pathname}${route.search}`, {
        method: request.method,
        headers,
        body,
      }),
    );
    const responseType = response.headers.get("content-type")?.toLowerCase() ?? "";
    if (!responseType.startsWith("application/json")) {
      return json({ error: "candidate_response_type_invalid" }, 502);
    }
    let bytes;
    try {
      bytes = await readBounded(response.body, maxResponseBytes);
    } catch {
      return json({ error: "candidate_response_too_large" }, 502);
    }
    bytes = redactCandidateTokens(bytes, env);
    return new Response(bytes, {
      status: response.status,
      headers: { "content-type": "application/json; charset=utf-8" },
    });
  },
};
