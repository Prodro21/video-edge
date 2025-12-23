# Video Edge Server

Edge server for stadium/field deployment of the video streaming platform. Provides local caching, offline operation, and cloud synchronization.

## Features

- **Local Caching** - Caches video clips and thumbnails for fast local access
- **Offline Operation** - Continues working when upstream is unavailable
- **Sync Queue** - Queues operations during offline mode, syncs when reconnected
- **WebSocket Relay** - Forwards real-time events from upstream to local clients
- **API Proxy** - Transparent proxy to upstream video-platform

## Architecture

```
┌─────────────────────────────────────────────────────────────────────┐
│                        STADIUM NETWORK                               │
├─────────────────────────────────────────────────────────────────────┤
│                                                                      │
│   ┌──────────────────────────────────────────────────────────┐      │
│   │                    video-edge                             │      │
│   │                                                           │      │
│   │  ┌─────────┐  ┌─────────┐  ┌─────────┐  ┌─────────┐     │      │
│   │  │  Cache  │  │  Proxy  │  │  Sync   │  │   WS    │     │      │
│   │  │  Layer  │  │  Layer  │  │  Queue  │  │   Hub   │     │      │
│   │  └─────────┘  └─────────┘  └─────────┘  └─────────┘     │      │
│   │       │            │            │            │           │      │
│   │       └────────────┴────────────┴────────────┘           │      │
│   │                         │                                 │      │
│   └─────────────────────────┼─────────────────────────────────┘      │
│                             │ :8080                                  │
│         ┌───────────────────┼───────────────────┐                   │
│         │                   │                   │                   │
│         ▼                   ▼                   ▼                   │
│    ┌─────────┐         ┌─────────┐         ┌─────────┐             │
│    │  iPad   │         │  iPad   │         │Dashboard│             │
│    └─────────┘         └─────────┘         └─────────┘             │
│                                                                      │
└─────────────────────────────────────────────────────────────────────┘
                              │
                              │ WAN (when available)
                              ▼
                    ┌─────────────────┐
                    │ video-platform  │
                    │   (upstream)    │
                    └─────────────────┘
```

## Installation

```bash
# Build
go build -o video-edge ./cmd/server

# Install
sudo cp video-edge /usr/local/bin/
```

## Usage

### Command Line

```bash
# Default configuration
./video-edge

# Custom upstream and cache
./video-edge \
  -upstream http://192.168.1.100:8080 \
  -cache-path /var/cache/video-edge \
  -cache-size 100 \
  -port 8080

# Using environment variables
UPSTREAM_URL=http://cloud.example.com:8080 \
CACHE_PATH=/data/cache \
./video-edge
```

### Configuration Options

| Flag | Env Variable | Default | Description |
|------|--------------|---------|-------------|
| `-port` | `EDGE_PORT` | 8080 | Server port |
| `-upstream` | `UPSTREAM_URL` | http://localhost:8081 | Upstream video-platform URL |
| `-cache-path` | `CACHE_PATH` | ./cache | Local cache directory |
| `-cache-size` | - | 50 | Maximum cache size in GB |
| `-data-path` | `DATA_PATH` | ./data | Data directory for sync queue |
| `-sync-interval` | - | 30s | How often to attempt sync |
| `-check-interval` | - | 10s | Connection check interval |

## API Endpoints

### Edge-Specific

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/health` | GET | Health check with online status |
| `/edge/status` | GET | Edge server status (cache, sync, clients) |
| `/edge/sync/queue` | GET | View pending sync operations |
| `/edge/sync/force` | POST | Trigger immediate sync |
| `/edge/cache/clear` | POST | Clear local cache |
| `/ws` | WS | WebSocket for real-time events |

### Proxied (from video-platform)

All `/api/v1/*` requests are proxied to the upstream server:
- Sessions, Clips, Channels, Tags CRUD
- Video streaming, thumbnails, downloads

## Offline Behavior

When the upstream server is unavailable:

1. **Read Operations** - Served from cache if available
2. **Write Operations** - Queued for later sync:
   - Create/update clips
   - Favorite clips
   - Create/update tags
   - Start/complete sessions
3. **WebSocket** - Local events only (no upstream events)

When connection is restored:
1. Queued operations are synced automatically
2. WebSocket reconnects to upstream
3. Cache continues to be updated

## Cache Strategy

| Content Type | TTL | Reason |
|--------------|-----|--------|
| Video streams | 24 hours | Large files, rarely change |
| Thumbnails | 12 hours | Small, frequently accessed |
| API responses | 5 minutes | Need fresh data |

Cache eviction uses LRU (Least Recently Used) when size limit is reached.

## Sync Queue

Operations are persisted to `{data-path}/sync_queue.json` and survive restarts.

Example queue entry:
```json
{
  "id": "20241223143015.123456",
  "type": "favorite_clip",
  "path": "/api/v1/clips/abc123/favorite",
  "method": "POST",
  "body": null,
  "created_at": "2024-12-23T14:30:15Z",
  "attempts": 0
}
```

## Stadium Deployment

### Recommended Hardware

- CPU: 4+ cores
- RAM: 8GB+
- Storage: 500GB+ SSD
- Network: Gigabit Ethernet

### Network Configuration

```
Edge Server: 10.0.0.1
DHCP Range: 10.0.0.100-199 (for iPads)
WiFi: 5GHz for best performance
```

### Docker Deployment

```dockerfile
FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /video-edge ./cmd/server

FROM alpine:3.19
COPY --from=builder /video-edge /usr/local/bin/
RUN mkdir -p /cache /data
EXPOSE 8080
CMD ["video-edge", "-cache-path", "/cache", "-data-path", "/data"]
```

```yaml
# docker-compose.yml
services:
  video-edge:
    build: .
    ports:
      - "8080:8080"
    environment:
      - UPSTREAM_URL=http://video-platform:8080
    volumes:
      - edge_cache:/cache
      - edge_data:/data

volumes:
  edge_cache:
  edge_data:
```

## Monitoring

### Health Check

```bash
curl http://localhost:8080/health
# {"status":"healthy","mode":"edge","online":true,"timestamp":"..."}
```

### Status

```bash
curl http://localhost:8080/edge/status
# {
#   "online": true,
#   "upstream_url": "http://...",
#   "cache": {"item_count": 150, "current_size_bytes": 5368709120, ...},
#   "sync": {"pending_operations": 0, ...},
#   "connected_clients": 5
# }
```

## Development

```bash
# Run with live reload
go run ./cmd/server -upstream http://localhost:8081

# Run tests
go test ./...
```
