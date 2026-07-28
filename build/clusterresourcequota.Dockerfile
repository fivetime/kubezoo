# syntax=docker/dockerfile:1.7

FROM --platform=${BUILDPLATFORM} golang:1.26-alpine3.24 AS builder
RUN apk add --no-cache bash git
WORKDIR /build

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
ARG TARGETOS TARGETARCH GIT_VERSION
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    GIT_VERSION=${GIT_VERSION} bash ./hack/build.sh clusterresourcequota

FROM alpine:3.24
RUN apk add --no-cache ca-certificates \
    && addgroup -S -g 65532 nonroot \
    && adduser -S -D -H -u 65532 -G nonroot nonroot
ARG TARGETOS TARGETARCH
COPY --from=builder /build/_output/local/bin/${TARGETOS}/${TARGETARCH}/clusterresourcequota /usr/local/bin/clusterresourcequota
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/clusterresourcequota"]
