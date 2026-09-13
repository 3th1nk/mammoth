# mammoth-builder — build facet image (--mode=builder).
# Needs external tools (xorriso for media assembly, M3) and loop-mount
# privileges, so it stays on alpine rather than distroless
# (docs/02-architecture.md §5.3).
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

FROM alpine:3.20
# xorriso lands with M3 media assembly; declared here so the deployment
# unit boundary is visible from day one.
RUN apk add --no-cache xorriso ca-certificates && adduser -D -u 1000 mammoth
COPY --from=build /out/mammoth /mammoth
USER mammoth
EXPOSE 8080
ENTRYPOINT ["/mammoth"]
CMD ["serve", "--mode=builder"]
