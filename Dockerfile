# syntax=docker/dockerfile:1

FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/verify ./cmd/verify

FROM alpine:3.21 AS api
RUN apk add --no-cache wget
COPY --from=build /out/server /usr/local/bin/server
ENV HTTP_ADDR=:8080 \
    GIN_MODE=release
EXPOSE 8080
ENTRYPOINT ["server"]

FROM alpine:3.21 AS verify
COPY --from=build /out/verify /usr/local/bin/verify
ENTRYPOINT ["verify"]
