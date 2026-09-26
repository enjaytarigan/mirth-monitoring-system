# syntax=docker/dockerfile:1.7

# --- build ---
FROM golang:1.25-alpine AS build
WORKDIR /src

RUN apk add --no-cache ca-certificates git tzdata

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/mirth-monitor ./cmd/server \
 && mkdir -p /out/data && touch /out/data/.keep

# --- runtime ---
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app

# nonroot uid/gid in distroless is 65532
COPY --from=build --chown=65532:65532 /out/mirth-monitor /app/mirth-monitor
COPY --from=build --chown=65532:65532 /out/data /app/data
COPY --from=build /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

ENV LISTEN_ADDR=:8080 \
    DATA_DIR=/app/data \
    TZ=Asia/Jakarta

USER 65532:65532
EXPOSE 8080
VOLUME ["/app/data"]

ENTRYPOINT ["/app/mirth-monitor"]
