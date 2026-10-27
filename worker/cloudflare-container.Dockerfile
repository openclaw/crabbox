FROM docker.io/library/golang:1.27.3-bookworm@sha256:5c3e9b43cfd3bb95c7937462e29d87ada57b481a96fa23ad0eb61778d7a00110 AS go-runtime

FROM docker.io/library/node:24.22.1-bookworm@sha256:5630d96570d4ca4a8523fda3d169d0114554d0f48293e1ae3db6be7c79b42a17

ARG TARGETARCH
ARG GH_VERSION=2.102.1
ARG GH_SHA256_AMD64=77b9e297e94c50336823d0f84de7ef1be60cba704601fa0d98f129f47a94987b
ARG GH_SHA256_ARM64=5019b3cc6e8a03bb4d88600f8dbda2add8e6407705c159b0c63151036e77a1af
ARG PNPM_VERSION=12.15.0
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
