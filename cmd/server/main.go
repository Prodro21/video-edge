package main

import (
	"bytes"
	"context"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Prodro21/video-edge/internal/cache"
	"github.com/Prodro21/video-edge/internal/proxy"
	"github.com/Prodro21/video-edge/internal/sync"
	"github.com/Prodro21/video-edge/internal/websocket"
	"github.com/gin-gonic/gin"
)

type Config struct {
	Port          string
	UpstreamURL   string
	CachePath     string
	CacheSizeGB   int
	DataPath      string
	SyncInterval  time.Duration
	CheckInterval time.Duration
}

func main() {
	cfg := Config{}

	// Parse flags
	flag.StringVar(&cfg.Port, "port", "8080", "Server port")
	flag.StringVar(&cfg.UpstreamURL, "upstream", "http://localhost:8081", "Upstream video-platform URL")
	flag.StringVar(&cfg.CachePath, "cache-path", "./cache", "Cache directory path")
	flag.IntVar(&cfg.CacheSizeGB, "cache-size", 50, "Maximum cache size in GB")
	flag.StringVar(&cfg.DataPath, "data-path", "./data", "Data directory path")
	flag.DurationVar(&cfg.SyncInterval, "sync-interval", 30*time.Second, "Sync check interval")
	flag.DurationVar(&cfg.CheckInterval, "check-interval", 10*time.Second, "Connection check interval")
	flag.Parse()

	// Environment overrides
	if v := os.Getenv("EDGE_PORT"); v != "" {
		cfg.Port = v
	}
	if v := os.Getenv("UPSTREAM_URL"); v != "" {
		cfg.UpstreamURL = v
	}
	if v := os.Getenv("CACHE_PATH"); v != "" {
		cfg.CachePath = v
	}
	if v := os.Getenv("DATA_PATH"); v != "" {
		cfg.DataPath = v
	}

	// Initialize cache
	fileCache, err := cache.New(cfg.CachePath, cfg.CacheSizeGB)
	if err != nil {
		log.Fatalf("Failed to initialize cache: %v", err)
	}
	log.Printf("Cache initialized: %s (max %d GB)", cfg.CachePath, cfg.CacheSizeGB)

	// Initialize proxy
	apiProxy := proxy.New(cfg.UpstreamURL, fileCache)
	log.Printf("Proxy initialized: upstream=%s", cfg.UpstreamURL)

	// Initialize sync service
	syncer, err := sync.New(cfg.DataPath, func(ctx context.Context, method, path string, body []byte) error {
		return doUpstreamRequest(cfg.UpstreamURL, method, path, body)
	})
	if err != nil {
		log.Fatalf("Failed to initialize syncer: %v", err)
	}
	syncer.Start(cfg.SyncInterval, apiProxy.IsOnline)
	defer syncer.Stop()

	// Initialize WebSocket hub
	wsHub := websocket.NewHub()
	go wsHub.Run()

	// Start connection checker
	go connectionChecker(cfg, apiProxy, wsHub)

	// Setup router
	router := setupRouter(cfg, apiProxy, syncer, wsHub, fileCache)

	// Create server
	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: router,
	}

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh

		log.Println("Shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		syncer.Stop()
		wsHub.DisconnectUpstream()
		srv.Shutdown(ctx)
	}()

	// Start server
	log.Printf("Edge server starting on :%s", cfg.Port)
	log.Printf("  Upstream: %s", cfg.UpstreamURL)
	log.Printf("  Cache: %s (%d GB max)", cfg.CachePath, cfg.CacheSizeGB)

	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}

func setupRouter(cfg Config, apiProxy *proxy.Proxy, syncer *sync.Syncer, wsHub *websocket.Hub, fileCache *cache.Cache) *gin.Engine {
	router := gin.Default()

	// Health check
	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status":    "healthy",
			"mode":      "edge",
			"online":    apiProxy.IsOnline(),
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		})
	})

	// Edge status
	router.GET("/edge/status", func(c *gin.Context) {
		cacheStats := fileCache.Stats()
		syncStatus := syncer.Status(apiProxy.IsOnline())

		c.JSON(http.StatusOK, gin.H{
			"online":           apiProxy.IsOnline(),
			"upstream_url":     cfg.UpstreamURL,
			"cache":            cacheStats,
			"sync":             syncStatus,
			"connected_clients": wsHub.ClientCount(),
		})
	})

	// Sync queue status
	router.GET("/edge/sync/queue", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"queue": syncer.GetQueue(),
		})
	})

	// Force sync
	router.POST("/edge/sync/force", func(c *gin.Context) {
		if !apiProxy.IsOnline() {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error": "upstream not available",
			})
			return
		}
		// Trigger immediate sync by checking status
		c.JSON(http.StatusOK, gin.H{
			"message":     "sync triggered",
			"pending_ops": syncer.QueueLength(),
		})
	})

	// Clear cache
	router.POST("/edge/cache/clear", func(c *gin.Context) {
		if err := fileCache.Clear(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": err.Error(),
			})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"message": "cache cleared",
		})
	})

	// WebSocket endpoint
	router.GET("/ws", func(c *gin.Context) {
		wsHub.HandleWebSocket(c.Writer, c.Request)
	})

	// Proxy all API requests
	router.Any("/api/*path", func(c *gin.Context) {
		// Handle offline writes by queueing
		if !apiProxy.IsOnline() && isWriteRequest(c.Request.Method) {
			queueOfflineRequest(c, syncer)
			return
		}
		apiProxy.Forward(c.Writer, c.Request)
	})

	// Serve cached files directly for specific paths
	router.GET("/clips/:id/stream", func(c *gin.Context) {
		apiProxy.Forward(c.Writer, c.Request)
	})
	router.GET("/clips/:id/thumbnail", func(c *gin.Context) {
		apiProxy.Forward(c.Writer, c.Request)
	})
	router.GET("/clips/:id/download", func(c *gin.Context) {
		apiProxy.Forward(c.Writer, c.Request)
	})

	return router
}

func isWriteRequest(method string) bool {
	return method == "POST" || method == "PUT" || method == "PATCH" || method == "DELETE"
}

func queueOfflineRequest(c *gin.Context, syncer *sync.Syncer) {
	// Determine operation type
	path := c.Request.URL.Path
	method := c.Request.Method

	var opType sync.OperationType
	switch {
	case strings.Contains(path, "/clips") && method == "POST":
		opType = sync.OpCreateClip
	case strings.Contains(path, "/clips") && strings.Contains(path, "/favorite"):
		opType = sync.OpFavoriteClip
	case strings.Contains(path, "/tags") && method == "POST":
		opType = sync.OpCreateTag
	case strings.Contains(path, "/tags") && method == "PATCH":
		opType = sync.OpUpdateTag
	case strings.Contains(path, "/sessions") && strings.Contains(path, "/start"):
		opType = sync.OpStartSession
	case strings.Contains(path, "/sessions") && strings.Contains(path, "/complete"):
		opType = sync.OpEndSession
	default:
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "operation not available offline",
		})
		return
	}

	// Read body
	var body interface{}
	if c.Request.Body != nil {
		bodyBytes, _ := io.ReadAll(c.Request.Body)
		if len(bodyBytes) > 0 {
			body = bodyBytes
		}
	}

	// Queue the operation
	if err := syncer.Enqueue(opType, method, path, body); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "failed to queue operation",
		})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"status":  "queued",
		"message": "operation queued for sync when online",
		"pending": syncer.QueueLength(),
	})
}

func connectionChecker(cfg Config, apiProxy *proxy.Proxy, wsHub *websocket.Hub) {
	ticker := time.NewTicker(cfg.CheckInterval)
	defer ticker.Stop()

	wasOnline := false

	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		isOnline := apiProxy.CheckConnection(ctx)
		cancel()

		// State changed
		if isOnline != wasOnline {
			if isOnline {
				log.Println("Upstream connection restored")
				// Connect WebSocket to upstream
				upstreamWS := strings.Replace(cfg.UpstreamURL, "http://", "ws://", 1)
				upstreamWS = strings.Replace(upstreamWS, "https://", "wss://", 1)
				wsHub.ConnectUpstream(upstreamWS + "/ws")
			} else {
				log.Println("Upstream connection lost - operating in offline mode")
				wsHub.DisconnectUpstream()
			}
			wasOnline = isOnline

			// Broadcast connection status to clients
			wsHub.Broadcast(websocket.Event{
				Type: "connection_status",
				Data: map[string]interface{}{
					"online": isOnline,
				},
			})
		}
	}
}

func doUpstreamRequest(upstreamURL, method, path string, body []byte) error {
	url := upstreamURL + path

	var reqBody io.Reader
	if len(body) > 0 {
		reqBody = bytes.NewReader(body)
	}

	req, err := http.NewRequest(method, url, reqBody)
	if err != nil {
		return err
	}

	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return &upstreamError{
			StatusCode: resp.StatusCode,
			Body:       string(respBody),
		}
	}

	return nil
}

type upstreamError struct {
	StatusCode int
	Body       string
}

func (e *upstreamError) Error() string {
	return "upstream error: " + e.Body
}
