# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS builder
WORKDIR /src

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

ENV CGO_ENABLED=0 \
    GOTOOLCHAIN=auto

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w -X telemount/internal/version.Version=${VERSION}" -o /out/telemount ./cmd/telemount

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata \
    && mkdir -p /data /tmp/telemount

COPY --from=builder /out/telemount /usr/local/bin/telemount

ENV LISTEN_ADDR=":8080" \
    DATA_DIR="/data" \
    TMP_DIR="/tmp/telemount"

EXPOSE 8080
VOLUME ["/data"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/api/status >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/telemount"]
