FROM docker.io/library/golang:1.27.4-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea AS go-runtime

FROM docker.io/library/node:24.22.3-bookworm@sha256:9d266751ca3d7fdbe651c878ffd5ad0a42fed49a18c283509267c97f025b7496

ARG TARGETARCH
ARG GH_VERSION=2.105.0
ARG GH_SHA256_AMD64=79e7ab5db738c48777b696e374f8b428f0647debfb424f44d2f426370407fe2f
ARG GH_SHA256_ARM64=b1278c75107423d818f1937fd92cafc9e5e9b606631568f68d7fb816f5885a02
ARG PNPM_VERSION=12.21.0
ENV NPM_CONFIG_CACHE=/var/cache/crabbox/npm \
    PATH=/usr/local/go/bin:$PATH

RUN apt-get update \
  && apt-get install -y --no-install-recommends ca-certificates curl git jq ripgrep tar tini \
  && mkdir -p /var/cache/crabbox/npm /var/cache/crabbox/pnpm \
  && rm -rf /var/lib/apt/lists/* \
  && case "${TARGETARCH}" in \
      amd64) gh_arch="amd64"; gh_sha256="${GH_SHA256_AMD64}" ;; \
      arm64) gh_arch="arm64"; gh_sha256="${GH_SHA256_ARM64}" ;; \
      *) echo "unsupported GitHub CLI target arch: ${TARGETARCH}" >&2; exit 1 ;; \
    esac \
  && curl -fsSL "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_${gh_arch}.tar.gz" -o /tmp/gh.tgz \
  && printf '%s  %s\n' "${gh_sha256}" /tmp/gh.tgz | sha256sum -c - \
  && tar -xzf /tmp/gh.tgz -C /tmp \
  && install -m 0755 "/tmp/gh_${GH_VERSION}_linux_${gh_arch}/bin/gh" /usr/local/bin/gh \
  && rm -rf /tmp/gh.tgz "/tmp/gh_${GH_VERSION}_linux_${gh_arch}" \
  && corepack enable \
  && corepack prepare "pnpm@${PNPM_VERSION}" --activate \
  && pnpm config set store-dir /var/cache/crabbox/pnpm

COPY --from=go-runtime /usr/local/go /usr/local/go
# Commands run through the Worker's native exec() as `bash -l`; the profile
# carries image defaults because exec() does not inherit the image ENV. A
# forwarded NPM_CONFIG_CACHE, even an empty one, keeps its value.
RUN ln -sf /usr/local/go/bin/go /usr/local/bin/go \
  && ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt \
  && printf '%s\n' 'export NPM_CONFIG_CACHE="${NPM_CONFIG_CACHE-/var/cache/crabbox/npm}"' 'export PATH=/usr/local/go/bin:$PATH' > /etc/profile.d/crabbox.sh

WORKDIR /workspace
ENTRYPOINT ["/usr/bin/tini", "--", "sleep", "infinity"]
