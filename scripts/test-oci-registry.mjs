#!/usr/bin/env node

import { createHash, randomUUID } from "node:crypto";
import http from "node:http";

const blobs = new Map();
const manifests = new Map();
const tags = new Map();
const uploads = new Map();
const referrers = new Map();

function digest(bytes) {
  return `sha256:${createHash("sha256").update(bytes).digest("hex")}`;
}

function key(repository, value) {
  return `${repository}\0${value}`;
}

function send(response, status, body, headers = {}) {
  const bytes = body === undefined ? Buffer.alloc(0) : Buffer.from(body);
  response.writeHead(status, {
    "Docker-Distribution-Api-Version": "registry/2.0",
    "Content-Length": bytes.length,
    ...headers,
  });
  response.end(bytes);
}

function distributionError(response, status, code, message) {
  send(
    response,
    status,
    JSON.stringify({ errors: [{ code, message }] }),
    { "Content-Type": "application/json" },
  );
}

async function requestBytes(request) {
  const chunks = [];
  for await (const chunk of request) chunks.push(chunk);
  return Buffer.concat(chunks);
}

function splitRoute(pathname, marker) {
  const rest = pathname.slice("/v2/".length);
  const index = rest.lastIndexOf(marker);
  if (index <= 0) return undefined;
  return {
    repository: rest.slice(0, index),
    value: rest.slice(index + marker.length),
  };
}

function manifestDescriptor(bytes) {
  const manifest = JSON.parse(bytes);
  return {
    mediaType: manifest.mediaType ?? "application/vnd.oci.image.manifest.v1+json",
    digest: digest(bytes),
    size: bytes.length,
    ...(manifest.artifactType ? { artifactType: manifest.artifactType } : {}),
    ...(manifest.annotations ? { annotations: manifest.annotations } : {}),
  };
}

const server = http.createServer(async (request, response) => {
  try {
    const url = new URL(request.url, "http://127.0.0.1");
    const pathname = decodeURIComponent(url.pathname);
    process.stderr.write(`${request.method} ${pathname}${url.search}\n`);
    if ((request.method === "GET" || request.method === "HEAD") && pathname === "/v2/") {
      send(response, 200);
      return;
    }

    if (request.method === "POST" && pathname === "/__test__/tamper-signature") {
      const repository = url.searchParams.get("repository");
      const subject = url.searchParams.get("subject");
      if (!repository || !subject) {
        distributionError(response, 400, "INVALID_REQUEST", "repository and subject are required");
        return;
      }
      const subjectKey = key(repository, subject);
      const descriptors = referrers.get(subjectKey) ?? [];
      if (descriptors.length !== 1) {
        distributionError(
          response,
          409,
          "INVALID_STATE",
          "subject must have exactly one signature referrer",
        );
        return;
      }
      const originalDescriptor = descriptors[0];
      const originalManifestBytes = manifests.get(key(repository, originalDescriptor.digest));
      if (!originalManifestBytes) {
        distributionError(response, 404, "MANIFEST_UNKNOWN", "signature manifest is unavailable");
        return;
      }
      const manifest = JSON.parse(originalManifestBytes);
      if (!Array.isArray(manifest.layers) || manifest.layers.length !== 1) {
        distributionError(
          response,
          409,
          "INVALID_STATE",
          "signature manifest must have exactly one layer",
        );
        return;
      }
      const originalLayer = manifest.layers[0];
      const originalBundleBytes = blobs.get(key(repository, originalLayer.digest));
      if (!originalBundleBytes) {
        distributionError(response, 404, "BLOB_UNKNOWN", "signature bundle is unavailable");
        return;
      }
      const bundle = JSON.parse(originalBundleBytes);
      let changed = false;
      const visit = (value) => {
        if (!value || typeof value !== "object" || changed) return;
        for (const [field, item] of Object.entries(value)) {
          if (
            (field === "sig" || field === "signature") &&
            typeof item === "string" &&
            item.length > 3
          ) {
            value[field] = `${item[0] === "A" ? "B" : "A"}${item.slice(1)}`;
            changed = true;
            return;
          }
          visit(item);
        }
      };
      visit(bundle);
      if (!changed) {
        distributionError(response, 409, "INVALID_STATE", "signature bundle has no signature");
        return;
      }
      const bundleBytes = Buffer.from(JSON.stringify(bundle));
      const bundleDigest = digest(bundleBytes);
      blobs.set(key(repository, bundleDigest), bundleBytes);
      manifest.layers[0] = {
        ...originalLayer,
        digest: bundleDigest,
        size: bundleBytes.length,
      };
      const manifestBytes = Buffer.from(JSON.stringify(manifest));
      const descriptor = manifestDescriptor(manifestBytes);
      manifests.set(key(repository, descriptor.digest), manifestBytes);
      referrers.set(subjectKey, [descriptor]);
      send(
        response,
        200,
        JSON.stringify({
          manifestDigest: descriptor.digest,
          bundleDigest,
        }),
        { "Content-Type": "application/json" },
      );
      return;
    }

    let route = splitRoute(pathname, "/blobs/uploads/");
    if (route?.value) {
      const upload = uploads.get(route.value);
      if (!upload || upload.repository !== route.repository) {
        distributionError(response, 404, "BLOB_UPLOAD_UNKNOWN", "unknown blob upload");
        return;
      }
      if (request.method === "PATCH") {
        upload.chunks.push(await requestBytes(request));
        const size = upload.chunks.reduce((total, chunk) => total + chunk.length, 0);
        send(response, 202, undefined, {
          Location: pathname,
          "Docker-Upload-UUID": route.value,
          Range: size === 0 ? "0-0" : `0-${size - 1}`,
        });
        return;
      }
      if (request.method === "PUT") {
        upload.chunks.push(await requestBytes(request));
        const bytes = Buffer.concat(upload.chunks);
        const expected = url.searchParams.get("digest");
        if (!expected || digest(bytes) !== expected) {
          distributionError(response, 400, "DIGEST_INVALID", "blob digest mismatch");
          return;
        }
        blobs.set(key(route.repository, expected), bytes);
        uploads.delete(route.value);
        send(response, 201, undefined, {
          Location: `/v2/${route.repository}/blobs/${expected}`,
          "Docker-Content-Digest": expected,
        });
        return;
      }
    }

    route = splitRoute(pathname, "/blobs/uploads");
    if (route && request.method === "POST") {
      const bytes = await requestBytes(request);
      const expected = url.searchParams.get("digest");
      if (expected) {
        if (digest(bytes) !== expected) {
          distributionError(response, 400, "DIGEST_INVALID", "blob digest mismatch");
          return;
        }
        blobs.set(key(route.repository, expected), bytes);
        send(response, 201, undefined, {
          Location: `/v2/${route.repository}/blobs/${expected}`,
          "Docker-Content-Digest": expected,
        });
        return;
      }
      const id = randomUUID();
      uploads.set(id, { repository: route.repository, chunks: [] });
      send(response, 202, undefined, {
        Location: `/v2/${route.repository}/blobs/uploads/${id}`,
        "Docker-Upload-UUID": id,
        Range: "0-0",
      });
      return;
    }

    route = splitRoute(pathname, "/blobs/");
    if (route && (request.method === "GET" || request.method === "HEAD")) {
      const bytes = blobs.get(key(route.repository, route.value));
      if (!bytes) {
        distributionError(response, 404, "BLOB_UNKNOWN", "unknown blob");
        return;
      }
      send(response, 200, request.method === "HEAD" ? undefined : bytes, {
        "Content-Type": "application/octet-stream",
        "Content-Length": bytes.length,
        "Docker-Content-Digest": route.value,
      });
      return;
    }

    route = splitRoute(pathname, "/manifests/");
    if (route && request.method === "PUT") {
      const bytes = await requestBytes(request);
      const descriptor = manifestDescriptor(bytes);
      if (route.value.startsWith("sha256:") && route.value !== descriptor.digest) {
        distributionError(response, 400, "DIGEST_INVALID", "manifest digest mismatch");
        return;
      }
      manifests.set(key(route.repository, descriptor.digest), bytes);
      if (!route.value.startsWith("sha256:")) {
        tags.set(key(route.repository, route.value), descriptor.digest);
      }
      const manifest = JSON.parse(bytes);
      if (manifest.subject?.digest) {
        const subjectKey = key(route.repository, manifest.subject.digest);
        const values = referrers.get(subjectKey) ?? [];
        const existing = values.findIndex((item) => item.digest === descriptor.digest);
        if (existing >= 0) values[existing] = descriptor;
        else values.push(descriptor);
        referrers.set(subjectKey, values);
      }
      send(response, 201, undefined, {
        Location: `/v2/${route.repository}/manifests/${descriptor.digest}`,
        "Docker-Content-Digest": descriptor.digest,
      });
      return;
    }
    if (route && (request.method === "GET" || request.method === "HEAD")) {
      const resolved = route.value.startsWith("sha256:")
        ? route.value
        : tags.get(key(route.repository, route.value));
      const bytes = resolved && manifests.get(key(route.repository, resolved));
      if (!bytes) {
        distributionError(response, 404, "MANIFEST_UNKNOWN", "unknown manifest");
        return;
      }
      const manifest = JSON.parse(bytes);
      send(response, 200, request.method === "HEAD" ? undefined : bytes, {
        "Content-Type": manifest.mediaType ?? "application/vnd.oci.image.manifest.v1+json",
        "Content-Length": bytes.length,
        "Docker-Content-Digest": resolved,
      });
      return;
    }

    route = splitRoute(pathname, "/referrers/");
    if (route && request.method === "GET") {
      const artifactType = url.searchParams.get("artifactType");
      const values = [...(referrers.get(key(route.repository, route.value)) ?? [])]
        .filter((item) => !artifactType || item.artifactType === artifactType)
        .sort((left, right) => left.digest.localeCompare(right.digest));
      const body = JSON.stringify({
        schemaVersion: 2,
        mediaType: "application/vnd.oci.image.index.v1+json",
        manifests: values,
      });
      send(response, 200, body, {
        "Content-Type": "application/vnd.oci.image.index.v1+json",
      });
      return;
    }

    route = splitRoute(pathname, "/tags/list");
    if (route && request.method === "GET") {
      const repositoryTags = [...tags.keys()]
        .filter((item) => item.startsWith(`${route.repository}\0`))
        .map((item) => item.slice(route.repository.length + 1))
        .sort();
      send(response, 200, JSON.stringify({ name: route.repository, tags: repositoryTags }), {
        "Content-Type": "application/json",
      });
      return;
    }

    distributionError(response, 404, "UNSUPPORTED", `${request.method} ${pathname}`);
  } catch (error) {
    distributionError(response, 500, "UNKNOWN", error.message);
  }
});

server.listen(0, "127.0.0.1", () => {
  const address = server.address();
  process.stdout.write(`${JSON.stringify({ host: "127.0.0.1", port: address.port })}\n`);
});

for (const signal of ["SIGINT", "SIGTERM"]) {
  process.on(signal, () => server.close(() => process.exit(0)));
}
