# mammoth — toolchain image for the provisioning facets (--mode=all/runner).
# This is the repo's default Dockerfile: `docker build .` produces the
# provisioning image, because a full-toolchain run is what most deployments
# need. Needs external tools (xorriso for media assembly) and loop-mount
# privileges, so it stays on alpine rather than distroless
# (docs/02-architecture.md §5.3). The control-plane-only shape lives in
# Dockerfile.api.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
# Restricted networks: --build-arg GOPROXY=https://goproxy.cn,direct
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY api/ api/
COPY assets/ assets/
COPY cmd/ cmd/
COPY internal/ internal/
# .git rides the context (.dockerignore keeps it) purely for version
# detection below; it never reaches a runtime image.
COPY .git .git
ARG TARGETOS TARGETARCH
# VERSION/COMMIT: auto-detected from git (describe --tags --always --dirty)
# when the context carries .git; explicit --build-arg overrides for CI and
# tarball builds. Empty default = auto. Explicit values keep the tag's
# leading v (v1.3.0) like describe does — see internal/version.
ARG VERSION=""
ARG COMMIT=""
RUN VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo v0.1.0-dev)}" \
 && COMMIT="${COMMIT:-$(git rev-parse HEAD 2>/dev/null || echo none)}" \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -buildvcs=false -ldflags "-s -w -X github.com/3th1nk/mammoth/internal/version.Version=${VERSION} -X github.com/3th1nk/mammoth/internal/version.Commit=${COMMIT}" \
    -o /out/mammoth ./cmd/mammoth

FROM alpine:3.20
# xorriso lands with M3 media assembly; declared here so the deployment
# unit boundary is visible from day one. wimlib/p7zip back the windows WIM
# path (wimlib-imagex / 7z). genisoimage never existed on alpine and no
# code path invokes it — dropped.
RUN apk add --no-cache xorriso wimlib p7zip ca-certificates && adduser -D -u 1000 mammoth
COPY --from=build /out/mammoth /mammoth
USER mammoth
EXPOSE 8080
ENTRYPOINT ["/mammoth"]
# The toolchain-bearing image exists to run the provisioning facets — its
# default must be all (media assembly shells out to the apk tools above).
# The old default pointed at --mode=builder, a placeholder mode removed
# entirely in v1.4.2 — it made the image dead on a bare `docker run`.
CMD ["serve", "--mode=all"]
