package metrics

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	// HTTP metrics
	httpRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total number of HTTP requests",
		},
		[]string{"method", "path", "status"},
	)

	httpRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request duration in seconds",
			Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		},
		[]string{"method", "path"},
	)

	httpRequestsInFlight = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "http_requests_in_flight",
			Help: "Number of HTTP requests currently being processed",
		},
	)

	// Cache metrics
	cacheHitsTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "cache_hits_total",
			Help: "Total number of cache hits",
		},
	)

	cacheMissesTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "cache_misses_total",
			Help: "Total number of cache misses",
		},
	)

	cacheSizeBytes = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "cache_size_bytes",
			Help: "Current cache size in bytes",
		},
	)

	cacheEntries = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "cache_entries",
			Help: "Number of entries in the cache",
		},
	)

	// Proxy metrics
	upstreamRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "upstream_requests_total",
			Help: "Total number of requests to upstream server",
		},
		[]string{"method", "status"},
	)

	upstreamRequestDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "upstream_request_duration_seconds",
			Help:    "Duration of requests to upstream server",
			Buckets: []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
		},
	)

	upstreamOnline = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "upstream_online",
			Help: "Whether the upstream server is reachable (1=online, 0=offline)",
		},
	)

	// Sync queue metrics
	syncQueueLength = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "sync_queue_length",
			Help: "Number of operations pending sync",
		},
	)

	syncOperationsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sync_operations_total",
			Help: "Total number of sync operations",
		},
		[]string{"type", "status"},
	)

	// WebSocket metrics
	wsConnectionsActive = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "websocket_connections_active",
			Help: "Number of active WebSocket connections",
		},
	)

	wsMessagesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "websocket_messages_total",
			Help: "Total number of WebSocket messages",
		},
		[]string{"direction", "type"},
	)

	// Edge mode status
	edgeMode = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "edge_mode",
			Help: "Edge device mode status",
		},
		[]string{"mode"},
	)
)

// GinMiddleware returns a Gin middleware that records HTTP metrics
func GinMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Skip metrics endpoint itself
		if c.Request.URL.Path == "/metrics" {
			c.Next()
			return
		}

		httpRequestsInFlight.Inc()
		start := time.Now()

		c.Next()

		httpRequestsInFlight.Dec()
		duration := time.Since(start).Seconds()

		// Normalize path to avoid high cardinality
		path := normalizePath(c.FullPath())
		if path == "" {
			path = "unknown"
		}

		status := strconv.Itoa(c.Writer.Status())
		method := c.Request.Method

		httpRequestsTotal.WithLabelValues(method, path, status).Inc()
		httpRequestDuration.WithLabelValues(method, path).Observe(duration)
	}
}

// Handler returns the Prometheus metrics HTTP handler
func Handler() gin.HandlerFunc {
	h := promhttp.Handler()
	return func(c *gin.Context) {
		h.ServeHTTP(c.Writer, c.Request)
	}
}

// normalizePath normalizes URL paths to reduce cardinality
func normalizePath(path string) string {
	if path == "" {
		return ""
	}
	return path
}

// Cache metrics functions
func IncCacheHit() {
	cacheHitsTotal.Inc()
}

func IncCacheMiss() {
	cacheMissesTotal.Inc()
}

func SetCacheSize(bytes float64) {
	cacheSizeBytes.Set(bytes)
}

func SetCacheEntries(count float64) {
	cacheEntries.Set(count)
}

// Proxy metrics functions
func IncUpstreamRequest(method string, status int) {
	upstreamRequestsTotal.WithLabelValues(method, strconv.Itoa(status)).Inc()
}

func ObserveUpstreamDuration(seconds float64) {
	upstreamRequestDuration.Observe(seconds)
}

func SetUpstreamOnline(online bool) {
	if online {
		upstreamOnline.Set(1)
	} else {
		upstreamOnline.Set(0)
	}
}

// Sync metrics functions
func SetSyncQueueLength(count int) {
	syncQueueLength.Set(float64(count))
}

func IncSyncOperation(opType, status string) {
	syncOperationsTotal.WithLabelValues(opType, status).Inc()
}

// WebSocket metrics functions
func IncWSConnections() {
	wsConnectionsActive.Inc()
}

func DecWSConnections() {
	wsConnectionsActive.Dec()
}

func IncWSMessages(direction, msgType string) {
	wsMessagesTotal.WithLabelValues(direction, msgType).Inc()
}

// Edge mode metrics
func SetEdgeMode(mode string, active bool) {
	val := float64(0)
	if active {
		val = 1
	}
	edgeMode.WithLabelValues(mode).Set(val)
}
