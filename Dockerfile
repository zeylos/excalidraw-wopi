# Dockerfile.goreleaser stays the release-path Dockerfile; it expects a
# pre-built binary. This file builds everything from source instead.

FROM node:24-trixie-slim@sha256:50c3b2f6988dfc307b86e5301d69611af31f4789bdf232863b07d3b02fe55ae0 AS web-build
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npx vite build

FROM golang:1.26-trixie@sha256:e6e8ff4b72b128bb673613645c5ac415e4f537b2390e77a86ffc40622ab56da8 AS go-build
ARG VERSION=dev
ARG COMMIT=none
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web-build /src/web/dist ./web/dist
RUN CGO_ENABLED=0 go build \
    -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
    -o /excalidraw-wopi ./cmd/excalidraw-wopi

FROM gcr.io/distroless/static-debian13:nonroot@sha256:1c2c046bc09ed40fad370b599a0b1ae7987f55b01e247cf27a7c27cd97e5bbc7
COPY --from=go-build /excalidraw-wopi /usr/local/bin/excalidraw-wopi

EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/excalidraw-wopi"]
