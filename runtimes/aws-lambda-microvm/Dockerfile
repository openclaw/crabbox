FROM --platform=linux/arm64 golang:1.27.4-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build
RUN apk add --no-cache --upgrade ca-certificates=20261020-r0 ca-certificates-bundle=20261020-r0
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
