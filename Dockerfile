# syntax=docker/dockerfile:1

# ---- build ----
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine3.24 AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/npm-sync ./cmd/npm-sync

# ---- run ----
# Minimal static base with CA certificates and time zone data, running as a
# non-root user (spec section 9).
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/npm-sync /npm-sync
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/npm-sync"]
