FROM --platform=$BUILDPLATFORM mcr.microsoft.com/devcontainers/go:1.26-bookworm@sha256:de99286e746c99e359edddaf0c47cbfbed34f4d6a3cd6310c99c87fa27c3c341 AS runner-build

ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY cloudflare-container-runner/go.mod cloudflare-container-runner/main.go ./
RUN target_arch="${TARGETARCH:-$(go env GOARCH)}" \
  && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH="$target_arch" go build -trimpath -ldflags="-s -w" -o /out/crabbox-container-runner .

FROM mcr.microsoft.com/dotnet/runtime-deps:9.0-bookworm-slim@sha256:608b519f61bce1ad7496a2544041d6c1538a5c48d056adf55af7fdc35f924283

RUN apt-get update \
  && apt-get install -y --no-install-recommends bash ca-certificates curl git jq ripgrep tar \
  && rm -rf /var/lib/apt/lists/*

COPY --from=runner-build /out/crabbox-container-runner /usr/local/bin/crabbox-container-runner

WORKDIR /workspace
EXPOSE 8787
ENTRYPOINT ["/usr/local/bin/crabbox-container-runner"]
