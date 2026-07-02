import { AwsClient } from "aws4fetch";

import { base64URL } from "./auth";
import type { Env } from "./types";

export interface ArtifactUploadRequest {
  files?: ArtifactUploadFile[];
  prefix?: string;
}

export interface ArtifactUploadFile {
  name?: string;
  size?: number;
  contentType?: string;
  sha256?: string;
}

export interface ArtifactUploadGrant {
  name: string;
  key: string;
  upload: {
    method: "PUT";
    url: string;
    headers: Record<string, string>;
    expiresAt: string;
  };
  url: string;
}

export interface ArtifactUploadResponse {
  backend: string;
  bucket: string;
  prefix: string;
  expiresAt: string;
  files: ArtifactUploadGrant[];
}

interface ArtifactConfig {
  backend: "s3" | "r2";
  bucket: string;
  prefix: string;
  baseURL: string;
  endpointURL: string;
  region: string;
  accessKeyID: string;
  secretAccessKey: string;
  sessionToken: string;
  uploadExpiresSeconds: number;
  urlExpiresSeconds: number;
}

const defaultUploadExpiresSeconds = 15 * 60;
const defaultURLExpiresSeconds = 7 * 24 * 60 * 60;
const maxArtifactFiles = 100;
const maxArtifactFileBytes = 1024 * 1024 * 1024;
const maxArtifactBatchBytes = 5 * 1024 * 1024 * 1024;

export async function artifactUploadResponse(
  env: Env,
  request: ArtifactUploadRequest,
  owner: string,
  org: string,
): Promise<ArtifactUploadResponse> {
  const config = artifactConfig(env);
  const files = normalizeArtifactFiles(request.files ?? []);
  if (files.length === 0) {
    throw new Error("artifacts upload request requires at least one file");
  }
  const prefix = artifactPrefix(config.prefix, org, owner, request.prefix);
  const now = new Date();
  const uploadExpiresAt = new Date(
    now.getTime() + config.uploadExpiresSeconds * 1000,
  ).toISOString();
  const grants = await Promise.all(
    files.map(async (file) => {
      const key = artifactObjectKey(prefix, file.name);
      const headers = artifactUploadHeaders(file);
      return {
        name: file.name,
        key,
        upload: {
          method: "PUT" as const,
          url: await presignArtifactURL(config, "PUT", key, config.uploadExpiresSeconds, headers),
          headers,
          expiresAt: uploadExpiresAt,
        },
        url: await artifactReadURL(config, key),
      };
    }),
  );
  return {
    backend: config.backend,
    bucket: config.bucket,
    prefix,
    expiresAt: uploadExpiresAt,
    files: grants,
  };
}

function artifactConfig(env: Env): ArtifactConfig {
  const backend = normalizedBackend(env.CRABBOX_ARTIFACTS_BACKEND);
  const bucket = trimmed(env.CRABBOX_ARTIFACTS_BUCKET);
  const accessKeyID = trimmed(env.CRABBOX_ARTIFACTS_ACCESS_KEY_ID);
  const secretAccessKey = trimmed(env.CRABBOX_ARTIFACTS_SECRET_ACCESS_KEY);
  if (!backend || !bucket || !accessKeyID || !secretAccessKey) {
    throw new Error(
      "artifact broker is not configured; set CRABBOX_ARTIFACTS_BACKEND, BUCKET, ACCESS_KEY_ID, and SECRET_ACCESS_KEY",
    );
  }
  const endpointURL = stripTrailingSlash(env.CRABBOX_ARTIFACTS_ENDPOINT_URL);
  if (backend === "r2" && !endpointURL) {
    throw new Error("artifact broker r2 backend requires CRABBOX_ARTIFACTS_ENDPOINT_URL");
  }
  const region = trimmed(env.CRABBOX_ARTIFACTS_REGION) || (backend === "r2" ? "auto" : "us-east-1");
  return {
    backend,
    bucket,
    prefix: trimmed(env.CRABBOX_ARTIFACTS_PREFIX) || "crabbox-artifacts",
    baseURL: stripTrailingSlash(env.CRABBOX_ARTIFACTS_BASE_URL),
    endpointURL,
    region,
    accessKeyID,
    secretAccessKey,
    sessionToken: trimmed(env.CRABBOX_ARTIFACTS_SESSION_TOKEN),
    uploadExpiresSeconds: positiveInt(
      env.CRABBOX_ARTIFACTS_UPLOAD_EXPIRES_SECONDS,
      defaultUploadExpiresSeconds,
    ),
    urlExpiresSeconds: positiveInt(
      env.CRABBOX_ARTIFACTS_URL_EXPIRES_SECONDS,
      defaultURLExpiresSeconds,
    ),
  };
}

function normalizedBackend(value: string | undefined): "s3" | "r2" | "" {
  switch (trimmed(value).toLowerCase()) {
    case "s3":
    case "aws":
    case "aws-s3":
      return "s3";
    case "r2":
    case "cloudflare-r2":
      return "r2";
    default:
      return "";
  }
}

function normalizeArtifactFiles(files: ArtifactUploadFile[]): Required<ArtifactUploadFile>[] {
  if (files.length > maxArtifactFiles) {
    throw new Error(`artifacts upload request supports at most ${maxArtifactFiles} files`);
  }
  let totalSize = 0;
  const normalized = files.map((file) => {
    const name = normalizeArtifactName(file.name);
    const size = Number(file.size ?? 0);
    if (!Number.isFinite(size) || size < 0 || size > maxArtifactFileBytes) {
      throw new Error(`invalid artifact size for ${name}`);
    }
    totalSize += size;
    return {
      name,
      size,
      contentType: normalizeContentType(file.contentType),
      sha256: normalizeHash(file.sha256),
    };
  });
  if (totalSize > maxArtifactBatchBytes) {
    throw new Error(`artifacts upload request supports at most ${maxArtifactBatchBytes} bytes`);
  }
  return normalized;
}

function normalizeArtifactName(value: string | undefined): string {
  const name = trimmed(value).replace(/\\/g, "/").replace(/^\/+/, "");
  const parts = name.split("/").filter(Boolean);
  if (parts.length === 0 || parts.some((part) => part === "." || part === "..")) {
    throw new Error(`invalid artifact name: ${value ?? ""}`);
  }
  return parts.join("/");
}

function normalizeContentType(value: string | undefined): string {
  return trimmed(value).slice(0, 200);
}

function normalizeHash(value: string | undefined): string {
  const hash = trimmed(value).toLowerCase();
  return /^[a-f0-9]{64}$/.test(hash) ? hash : "";
}

function artifactPrefix(
  configPrefix: string,
  org: string,
  owner: string,
  requestPrefix: string | undefined,
): string {
  const parts = [
    normalizePrefixPart(configPrefix),
    "v2",
    "org",
    opaqueArtifactIdentity(org),
    "owner",
    opaqueArtifactIdentity(owner),
    normalizePrefixPart(requestPrefix),
  ].filter(Boolean);
  return parts.join("/");
}

function opaqueArtifactIdentity(value: string): string {
  return base64URL(new TextEncoder().encode(value));
}

function normalizePrefixPart(value: string | undefined): string {
  return trimmed(value)
    .replace(/\\/g, "/")
    .split("/")
    .filter((part) => part && part !== "." && part !== "..")
    .join("/");
}

function artifactObjectKey(prefix: string, name: string): string {
  return [prefix, name].filter(Boolean).join("/");
}

function artifactUploadHeaders(file: Required<ArtifactUploadFile>): Record<string, string> {
  return {
    ...(file.contentType ? { "content-type": file.contentType } : {}),
    "content-length": String(file.size),
  };
}

async function artifactReadURL(config: ArtifactConfig, key: string): Promise<string> {
  if (config.baseURL) {
    return joinURLPath(config.baseURL, pathEscapeSegments(key));
  }
  return presignArtifactURL(config, "GET", key, config.urlExpiresSeconds);
}

async function presignArtifactURL(
  config: ArtifactConfig,
  method: "GET" | "PUT",
  key: string,
  expiresSeconds: number,
  headers: Record<string, string> = {},
): Promise<string> {
  const client = new AwsClient({
    accessKeyId: config.accessKeyID,
    secretAccessKey: config.secretAccessKey,
    service: "s3",
    region: config.region,
    ...(config.sessionToken ? { sessionToken: config.sessionToken } : {}),
  });
  const url = new URL(artifactS3ObjectURL(config, key));
  url.searchParams.set("X-Amz-Expires", String(expiresSeconds));
  const signed = await client.sign(url.toString(), {
    method,
    headers,
    aws: { signQuery: true, allHeaders: true },
  });
  return signed.url;
}

function artifactS3ObjectURL(config: ArtifactConfig, key: string): string {
  const encodedKey = pathEscapeSegments(key);
  if (config.endpointURL) {
    return joinURLPath(config.endpointURL, `${config.bucket}/${encodedKey}`);
  }
  if (config.region === "us-east-1") {
    return `https://${config.bucket}.s3.amazonaws.com/${encodedKey}`;
  }
  return `https://${config.bucket}.s3.${config.region}.amazonaws.com/${encodedKey}`;
}

function joinURLPath(base: string, suffix: string): string {
  return `${stripTrailingSlash(base)}/${suffix.replace(/^\/+/, "")}`;
}

function pathEscapeSegments(value: string): string {
  return value.split("/").map(encodeURIComponent).join("/");
}

function stripTrailingSlash(value: string | undefined): string {
  return trimmed(value).replace(/\/+$/, "");
}

function positiveInt(value: string | undefined, fallback: number): number {
  const n = Number.parseInt(trimmed(value), 10);
  return Number.isFinite(n) && n > 0 ? n : fallback;
}

function trimmed(value: string | undefined): string {
  return (value ?? "").trim();
}
