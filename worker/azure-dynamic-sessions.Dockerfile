FROM --platform=$BUILDPLATFORM mcr.microsoft.com/devcontainers/go:1.26-bookworm@sha256:5b40bf0530204ec28eac4bd95b0398b15bfbef337d47ed77d7b29daca9fac18a AS runner-build

ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY cloudflare-container-runner/go.mod cloudflare-container-runner/*.go ./
RUN target_arch="${TARGETARCH:-$(go env GOARCH)}" \
  && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH="$target_arch" go build -trimpath -ldflags="-s -w" -o /out/crabbox-container-runner .

FROM mcr.microsoft.com/dotnet/runtime-deps:9.0-bookworm-slim@sha256:92eda3f0c5172c8b3741233933c86b6393b2402e6bcc507f5c645f5ba682c54c

RUN apt-get update \
  && apt-get install -y --no-install-recommends bash ca-certificates curl git jq ripgrep tar \
  && rm -rf /var/lib/apt/lists/*

COPY --from=runner-build /out/crabbox-container-runner /usr/local/bin/crabbox-container-runner

WORKDIR /workspace
EXPOSE 8787
ENTRYPOINT ["/usr/local/bin/crabbox-container-runner"]
