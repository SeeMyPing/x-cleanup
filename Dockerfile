# syntax=docker/dockerfile:1

# Cross-compile on the build platform (no QEMU needed for multi-arch images).
# Base images are pinned by digest (updated by Dependabot).
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:fa3d596898d9f126af7c01d14ee7597bab3f39cccf392ef36b69fe77cd681f18 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/x-cleanup .

# Distroless static: CA certificates included (HTTPS to api.x.com), runs as non-root.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/x-cleanup /usr/local/bin/x-cleanup
WORKDIR /work
ENTRYPOINT ["/usr/local/bin/x-cleanup"]
