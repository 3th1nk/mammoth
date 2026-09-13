# mammoth — main business-facet image (--mode=all|api|runner|prober).
# distroless: static binary, no shell, no package manager (docs/02-architecture.md §5.3).
#
# Restricted networks can redirect the runtime base, e.g.:
#   docker build --build-arg RUNTIME_IMAGE=<mirror>/distroless/static-debian12:nonroot .
# Global build arg so the FROM line below can consume it.
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
# Restricted networks: --build-arg GOPROXY=https://goproxy.cn,direct
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/
ARG TARGETOS TARGETARCH
ARG VERSION=0.1.0-dev
ARG COMMIT=unknown
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X github.com/3th1nk/mammoth/internal/version.Version=${VERSION} -X github.com/3th1nk/mammoth/internal/version.Commit=${COMMIT}" \
    -o /out/mammoth ./cmd/mammoth

FROM ${RUNTIME_IMAGE}
COPY --from=build /out/mammoth /mammoth
# Facets other than builder have no external tool dependencies.
# nonroot on distroless = uid 65532; numeric form works on any substitute base.
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/mammoth"]
CMD ["serve", "--mode=all"]
