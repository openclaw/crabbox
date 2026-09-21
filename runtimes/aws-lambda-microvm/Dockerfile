FROM --platform=linux/arm64 golang:1.26-alpine@sha256:8ac98ca534ac3f51e1f420a1dd2c15e74c75cfa0f23f3ad27eb5d7236c349a0c AS build
WORKDIR /src
COPY main.go .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/crabbox-lambda-microvm-runner ./main.go

FROM --platform=linux/arm64 ubuntu:24.04@sha256:008173c23f95b170204355c12626cb5a965d779a7e1283b09e9cffbb1bf33ca3
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends bash ca-certificates curl git gzip tar \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/crabbox-lambda-microvm-runner /usr/local/bin/crabbox-lambda-microvm-runner
RUN mkdir -p /workspace/crabbox
WORKDIR /workspace/crabbox
EXPOSE 8080
CMD ["/usr/local/bin/crabbox-lambda-microvm-runner"]
