# syntax=docker/dockerfile:1.7

# ---- build stage -----------------------------------------------------------
FROM golang:1.26-bookworm AS build

WORKDIR /src

# Dependency layer: only invalidated when go.mod/go.sum change.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ARG VERSION=1.0.0
ARG TARGETOS=linux
ARG TARGETARCH=amd64

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -buildvcs=false \
      -ldflags "-s -w -X main.buildVersion=${VERSION}" \
      -o /out/server ./cmd/server

# Fail the build if the binary accidentally depends on libc.
RUN if ldd /out/server 2>&1 | grep -qE "=>|libc\.so"; then \
      echo "server is dynamically linked" >&2; exit 1; \
    fi

# ---- runtime stage ---------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

ARG VERSION=1.0.0
LABEL org.opencontainers.image.title="tavilyd" \
      org.opencontainers.image.description="Self-hosted Tavily-compatible search and extract API" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.licenses="MIT"

COPY --from=build /out/server /server

USER nonroot:nonroot
EXPOSE 8080

# The probe is the binary itself: distroless has no shell or curl.
HEALTHCHECK --interval=15s --timeout=3s --start-period=10s --retries=5 \
  CMD ["/server", "-healthcheck"]

ENTRYPOINT ["/server"]
