FROM --platform=linux/arm64 golang:1.26-alpine@sha256:5d9aa9fa75b776600e0972ac9d324bb260fdb403b8905202c7576d103a450361 AS build
WORKDIR /src
COPY main.go .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/crabbox-lambda-microvm-runner ./main.go

FROM --platform=linux/arm64 ubuntu:24.04@sha256:ea0b2b8607a61f981b2461f21edc27c0b37ddd53d23d4d350936480adf5de28c
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends bash ca-certificates curl git gzip tar \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/crabbox-lambda-microvm-runner /usr/local/bin/crabbox-lambda-microvm-runner
RUN mkdir -p /workspace/crabbox
WORKDIR /workspace/crabbox
EXPOSE 8080
CMD ["/usr/local/bin/crabbox-lambda-microvm-runner"]
