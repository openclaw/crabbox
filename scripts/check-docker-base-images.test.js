import assert from "node:assert/strict";
import test from "node:test";

import { unpinnedBaseImages } from "./check-docker-base-images.mjs";

test("accepts digest-pinned base images with readable tags", () => {
  const digest = "a".repeat(64);
  assert.deepEqual(
    unpinnedBaseImages(
      `FROM --platform=$BUILDPLATFORM node:24-bookworm@sha256:${digest} AS build\nFROM node:24-bookworm@sha256:${digest}\n`,
    ),
    [],
  );
});

test("reports unpinned and malformed base-image digests", () => {
  assert.deepEqual(
    unpinnedBaseImages(
      "FROM node:24-bookworm AS build\nFROM node:24-bookworm@sha256:abc\n",
      "Dockerfile",
    ),
    [
      "Dockerfile:1: base image lacks a valid SHA-256 digest: node:24-bookworm",
      "Dockerfile:2: base image lacks a valid SHA-256 digest: node:24-bookworm@sha256:abc",
    ],
  );
});
