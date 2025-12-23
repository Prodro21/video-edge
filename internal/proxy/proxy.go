package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Prodro21/video-edge/internal/cache"
	"github.com/Prodro21/video-edge/internal/metrics"
)

// Proxy forwards requests to the upstream video-platform server
// and caches responses for offline operation
type Proxy struct {
	upstreamURL string
	cache       *cache.Cache
	client      *http.Client
	mu          sync.RWMutex
	isOnline    bool
	lastCheck   time.Time
}

// New creates a new proxy instance
func New(upstreamURL string, cache *cache.Cache) *Proxy {
	return &Proxy{
		upstreamURL: strings.TrimSuffix(upstreamURL, "/"),
		cache:       cache,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
		isOnline: true,
	}
}

// IsOnline returns whether the upstream server is reachable
func (p *Proxy) IsOnline() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.isOnline
}

// CheckConnection verifies connectivity to the upstream server
func (p *Proxy) CheckConnection(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", p.upstreamURL+"/health", nil)
	if err != nil {
		p.setOnline(false)
		return false
	}

	resp, err := p.client.Do(req)
	if err != nil {
		p.setOnline(false)
		return false
	}
	defer resp.Body.Close()

	online := resp.StatusCode == http.StatusOK
	p.setOnline(online)
	return online
}

func (p *Proxy) setOnline(online bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.isOnline = online
	p.lastCheck = time.Now()
}

// Forward proxies a request to the upstream server
func (p *Proxy) Forward(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Check if this is a cacheable GET request
	if r.Method == "GET" && isCacheable(path) {
		p.handleCacheableRequest(w, r)
		return
	}

	// For non-cacheable requests, require online connection
	if !p.IsOnline() {
		http.Error(w, "Upstream server unavailable", http.StatusServiceUnavailable)
		return
	}

	p.forwardRequest(w, r)
}

// handleCacheableRequest tries cache first, then upstream
func (p *Proxy) handleCacheableRequest(w http.ResponseWriter, r *http.Request) {
	cacheKey := r.URL.Path + "?" + r.URL.RawQuery

	// Try cache first
	reader, item, err := p.cache.GetReader(cacheKey)
	if err == nil {
		defer reader.Close()
		metrics.IncCacheHit()
		w.Header().Set("Content-Type", item.ContentType)
		w.Header().Set("X-Cache", "HIT")
		w.Header().Set("X-Cache-Created", item.CreatedAt.Format(time.RFC3339))
		io.Copy(w, reader)
		return
	}

	metrics.IncCacheMiss()

	// Try upstream
	if p.IsOnline() {
		if p.forwardAndCache(w, r, cacheKey) {
			return
		}
	}

	// Return 503 if both fail
	http.Error(w, "Resource unavailable", http.StatusServiceUnavailable)
}

// forwardRequest proxies a request without caching
func (p *Proxy) forwardRequest(w http.ResponseWriter, r *http.Request) {
	upstreamURL := p.upstreamURL + r.URL.Path
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}

	// Create upstream request
	req, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, r.Body)
	if err != nil {
		http.Error(w, "Failed to create request", http.StatusInternalServerError)
		return
	}

	// Copy headers
	for key, values := range r.Header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	// Execute request
	resp, err := p.client.Do(req)
	if err != nil {
		log.Printf("Upstream request failed: %v", err)
		p.setOnline(false)
		http.Error(w, "Upstream server error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy response headers
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}

	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// forwardAndCache fetches from upstream and caches the response
func (p *Proxy) forwardAndCache(w http.ResponseWriter, r *http.Request, cacheKey string) bool {
	upstreamURL := p.upstreamURL + r.URL.Path
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}

	req, err := http.NewRequestWithContext(r.Context(), "GET", upstreamURL, nil)
	if err != nil {
		return false
	}

	// Copy relevant headers
	for _, h := range []string{"Accept", "Accept-Encoding", "Range"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}

	resp, err := p.client.Do(req)
	if err != nil {
		log.Printf("Upstream fetch failed: %v", err)
		p.setOnline(false)
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Forward non-OK responses without caching
		for key, values := range resp.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
		return true
	}

	// Buffer the response for caching
	var buf bytes.Buffer
	tee := io.TeeReader(resp.Body, &buf)

	// Copy to response writer first
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.Header().Set("X-Cache", "MISS")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, tee)

	// Cache the response
	contentType := resp.Header.Get("Content-Type")
	ttl := determineTTL(r.URL.Path)
	go func() {
		_, err := p.cache.Put(cacheKey, &buf, contentType, ttl)
		if err != nil {
			log.Printf("Failed to cache %s: %v", cacheKey, err)
		}
	}()

	return true
}

// isCacheable determines if a path should be cached
func isCacheable(path string) bool {
	// Cache video streams, thumbnails, and static API responses
	cacheablePaths := []string{
		"/api/v1/clips/",
		"/api/v1/sessions",
		"/api/v1/channels",
		"/api/v1/tags",
	}

	for _, prefix := range cacheablePaths {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}

	// Always cache stream and thumbnail endpoints
	if strings.Contains(path, "/stream") || strings.Contains(path, "/thumbnail") || strings.Contains(path, "/download") {
		return true
	}

	return false
}

// determineTTL determines cache TTL based on content type
func determineTTL(path string) time.Duration {
	// Video files - cache for 24 hours
	if strings.Contains(path, "/stream") || strings.Contains(path, "/download") {
		return 24 * time.Hour
	}

	// Thumbnails - cache for 12 hours
	if strings.Contains(path, "/thumbnail") {
		return 12 * time.Hour
	}

	// API responses - cache for 5 minutes
	return 5 * time.Minute
}

// GetJSON fetches JSON from upstream or cache
func (p *Proxy) GetJSON(ctx context.Context, path string, result interface{}) error {
	cacheKey := path

	// Try cache first
	reader, _, err := p.cache.GetReader(cacheKey)
	if err == nil {
		defer reader.Close()
		return json.NewDecoder(reader).Decode(result)
	}

	// Try upstream
	if !p.IsOnline() {
		return fmt.Errorf("upstream unavailable and not in cache")
	}

	req, err := http.NewRequestWithContext(ctx, "GET", p.upstreamURL+path, nil)
	if err != nil {
		return err
	}

	resp, err := p.client.Do(req)
	if err != nil {
		p.setOnline(false)
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upstream returned %d", resp.StatusCode)
	}

	// Buffer for caching
	var buf bytes.Buffer
	tee := io.TeeReader(resp.Body, &buf)

	// Decode response
	if err := json.NewDecoder(tee).Decode(result); err != nil {
		return err
	}

	// Cache in background
	go func() {
		p.cache.Put(cacheKey, &buf, "application/json", 5*time.Minute)
	}()

	return nil
}

// PostJSON sends a POST request to upstream (not cached)
func (p *Proxy) PostJSON(ctx context.Context, path string, body interface{}, result interface{}) error {
	if !p.IsOnline() {
		return fmt.Errorf("upstream unavailable")
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", p.upstreamURL+path, bytes.NewReader(jsonBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		p.setOnline(false)
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upstream error %d: %s", resp.StatusCode, string(body))
	}

	if result != nil {
		return json.NewDecoder(resp.Body).Decode(result)
	}
	return nil
}
