import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const dockerfile = await readFile("worker/cloudflare-container.Dockerfile", "utf8");

test("Cloudflare image pins GitHub CLI artifacts for each supported architecture", () => {
  assert.match(dockerfile, /^ARG TARGETARCH$/m);
  assert.doesNotMatch(dockerfile, /^ARG TARGETARCH=/m);
  assert.match(dockerfile, /^ARG GH_VERSION=2\.92\.0$/m);
  assert.match(dockerfile, /^ARG GH_SHA256_AMD64=[0-9a-f]{64}$/m);
  assert.match(dockerfile, /^ARG GH_SHA256_ARM64=[0-9a-f]{64}$/m);
  assert.match(dockerfile, /amd64\) gh_arch="amd64"; gh_sha256="\$\{GH_SHA256_AMD64\}" ;;/);
  assert.match(dockerfile, /arm64\) gh_arch="arm64"; gh_sha256="\$\{GH_SHA256_ARM64\}" ;;/);
});

test("Cloudflare image verifies GitHub CLI before extraction", () => {
  const download = dockerfile.indexOf('curl -fsSL "https://github.com/cli/cli/releases/');
  const verify = dockerfile.indexOf("sha256sum -c -");
  const extract = dockerfile.indexOf("tar -xzf /tmp/gh.tgz");

  assert.notEqual(download, -1);
  assert.ok(verify > download, "checksum verification must follow the download");
  assert.ok(extract > verify, "archive extraction must follow checksum verification");
  assert.match(dockerfile, /printf '%s  %s\\n' "\$\{gh_sha256\}" \/tmp\/gh\.tgz \| sha256sum -c -/);
});
