FROM golang:1.26-alpine AS build
WORKDIR /src
COPY main.go .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/crabbox-lambda-microvm-runner ./main.go

FROM ubuntu:24.04
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends bash ca-certificates curl git gzip tar \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/crabbox-lambda-microvm-runner /usr/local/bin/crabbox-lambda-microvm-runner
RUN mkdir -p /workspace/crabbox
WORKDIR /workspace/crabbox
EXPOSE 8080
CMD ["/usr/local/bin/crabbox-lambda-microvm-runner"]

