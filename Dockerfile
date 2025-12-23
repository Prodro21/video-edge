# Build stage
FROM golang:1.25-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /video-edge ./cmd/server

# Runtime stage
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata

RUN adduser -D -g '' appuser

COPY --from=builder /video-edge /usr/local/bin/

RUN mkdir -p /cache /data && chown -R appuser:appuser /cache /data

USER appuser

EXPOSE 8080

ENTRYPOINT ["video-edge"]
CMD ["-cache-path", "/cache", "-data-path", "/data"]
