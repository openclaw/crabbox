FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.27.4-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea AS runner-build

ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY cloudflare-container-runner/go.mod cloudflare-container-runner/*.go ./
RUN target_arch="${TARGETARCH:-$(go env GOARCH)}" \
  && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH="$target_arch" go build -trimpath -ldflags="-s -w" -o /out/crabbox-container-runner .

FROM mcr.microsoft.com/dotnet/runtime-deps:9.0-bookworm-slim@sha256:de397d253f352d6c2102f7ecc15036496f4b2b3248b099b74499da14a9651adb

RUN apt-get update \
  && apt-get install -y --no-install-recommends bash ca-certificates curl git jq ripgrep tar \
  && rm -rf /var/lib/apt/lists/*

COPY --from=runner-build /out/crabbox-container-runner /usr/local/bin/crabbox-container-runner

WORKDIR /workspace
EXPOSE 8787
ENTRYPOINT ["/usr/local/bin/crabbox-container-runner"]
