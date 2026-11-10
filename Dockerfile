# syntax=docker/dockerfile:1

# Cross-compile on the build platform (no QEMU needed for multi-arch images).
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/x-cleanup .

# Distroless static: CA certificates included (HTTPS to api.x.com), runs as non-root.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/x-cleanup /usr/local/bin/x-cleanup
WORKDIR /work
ENTRYPOINT ["/usr/local/bin/x-cleanup"]
