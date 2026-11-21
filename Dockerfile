# Cross-compiles on the build platform, so multi-arch builds need no emulation.
FROM --platform=$BUILDPLATFORM golang:1.26.3-alpine AS build
ARG TARGETOS TARGETARCH APP_VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${APP_VERSION}" -o /tender ./cmd

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /tender /tender
USER 65532:65532
ENTRYPOINT ["/tender"]
