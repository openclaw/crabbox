FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26-bookworm@sha256:a688600ca24f8a4d3ca77f95b0dd40704a9fc787c826660eb7ba0b641b8b175d AS runner-build

ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY cloudflare-container-runner/go.mod cloudflare-container-runner/*.go ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/crabbox-cloudflare-container-runner .

FROM docker.io/library/golang:1.26-bookworm@sha256:a688600ca24f8a4d3ca77f95b0dd40704a9fc787c826660eb7ba0b641b8b175d AS go-runtime

FROM docker.io/library/node:24-bookworm@sha256:64af3819f9275802414d7cdc38c27e9d82bd564dec4d4da87d008255d36c63b4

ARG TARGETARCH
ARG GH_VERSION=2.101.0
ARG GH_SHA256_AMD64=9bca2d1c16825f109907a23307628a2f0698fbf99662b73a5cf0b020293072b8
ARG GH_SHA256_ARM64=b57e8063f18862647c9d22727c32e9da1b963f8bf9db648fe123a6975695640f
ARG PNPM_VERSION=12.5.1
ENV NPM_CONFIG_CACHE=/var/cache/crabbox/npm \
    PATH=/usr/local/go/bin:$PATH

RUN apt-get update \
  && apt-get install -y --no-install-recommends ca-certificates curl git jq ripgrep tar \
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
COPY --from=runner-build /out/crabbox-cloudflare-container-runner /usr/local/bin/crabbox-cloudflare-container-runner
RUN ln -sf /usr/local/go/bin/go /usr/local/bin/go \
  && ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt

WORKDIR /workspace
EXPOSE 8787
ENTRYPOINT ["/usr/local/bin/crabbox-cloudflare-container-runner"]
