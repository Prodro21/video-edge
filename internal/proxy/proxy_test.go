package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/Prodro21/video-edge/internal/cache"
)

func setupTestCache(t *testing.T) (*cache.Cache, func()) {
	tmpDir, err := os.MkdirTemp("", "proxy-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}

	c, err := cache.New(tmpDir, 1)
	if err != nil {
		os.RemoveAll(tmpDir)
		t.Fatalf("Failed to create cache: %v", err)
	}

	cleanup := func() {
		os.RemoveAll(tmpDir)
	}
	return c, cleanup
}

func TestNew(t *testing.T) {
	c, cleanup := setupTestCache(t)
	defer cleanup()

	p := New("http://localhost:8080", c)
	if p == nil {
		t.Fatal("New() returned nil")
	}

	// Check that trailing slash is trimmed
	p2 := New("http://localhost:8080/", c)
	if p2.upstreamURL != "http://localhost:8080" {
		t.Errorf("Expected trailing slash to be trimmed, got %s", p2.upstreamURL)
	}
}

func TestProxy_IsOnline(t *testing.T) {
	c, cleanup := setupTestCache(t)
	defer cleanup()

	p := New("http://localhost:8080", c)

	// Default should be online
	if !p.IsOnline() {
		t.Error("Expected proxy to be online by default")
	}

	// Set offline
	p.setOnline(false)
	if p.IsOnline() {
		t.Error("Expected proxy to be offline after setOnline(false)")
	}

	// Set online
	p.setOnline(true)
	if !p.IsOnline() {
		t.Error("Expected proxy to be online after setOnline(true)")
	}
}

func TestProxy_CheckConnection(t *testing.T) {
	c, cleanup := setupTestCache(t)
	defer cleanup()

	t.Run("healthy upstream", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/health" {
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		defer server.Close()

		p := New(server.URL, c)
		if !p.CheckConnection(context.Background()) {
			t.Error("Expected healthy upstream to return true")
		}
		if !p.IsOnline() {
			t.Error("Expected proxy to be online after successful health check")
		}
	})

	t.Run("unhealthy upstream", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer server.Close()

		p := New(server.URL, c)
		if p.CheckConnection(context.Background()) {
			t.Error("Expected unhealthy upstream to return false")
		}
		if p.IsOnline() {
			t.Error("Expected proxy to be offline after failed health check")
		}
	})

	t.Run("unreachable upstream", func(t *testing.T) {
		p := New("http://localhost:59999", c)
		if p.CheckConnection(context.Background()) {
			t.Error("Expected unreachable upstream to return false")
		}
		if p.IsOnline() {
			t.Error("Expected proxy to be offline after connection failure")
		}
	})
}

func TestProxy_Forward_NonCacheable(t *testing.T) {
	c, cleanup := setupTestCache(t)
	defer cleanup()

	t.Run("forward POST request", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "POST" {
				t.Errorf("Expected POST, got %s", r.Method)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		}))
		defer server.Close()

		p := New(server.URL, c)

		req := httptest.NewRequest("POST", "/api/v1/sessions", bytes.NewBufferString(`{"name":"test"}`))
		w := httptest.NewRecorder()

		p.Forward(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("Expected status 200, got %d", w.Code)
		}
	})

	t.Run("forward when offline", func(t *testing.T) {
		p := New("http://localhost:59999", c)
		p.setOnline(false)

		req := httptest.NewRequest("POST", "/api/v1/sessions", nil)
		w := httptest.NewRecorder()

		p.Forward(w, req)

		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("Expected status 503, got %d", w.Code)
		}
	})
}

func TestProxy_Forward_Cacheable(t *testing.T) {
	c, cleanup := setupTestCache(t)
	defer cleanup()

	t.Run("cache miss then hit", func(t *testing.T) {
		requestCount := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestCount++
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"data": "test"})
		}))
		defer server.Close()

		p := New(server.URL, c)

		// First request - cache miss
		req1 := httptest.NewRequest("GET", "/api/v1/sessions", nil)
		w1 := httptest.NewRecorder()
		p.Forward(w1, req1)

		if w1.Code != http.StatusOK {
			t.Errorf("First request: expected status 200, got %d", w1.Code)
		}
		if w1.Header().Get("X-Cache") != "MISS" {
			t.Errorf("First request: expected X-Cache: MISS, got %s", w1.Header().Get("X-Cache"))
		}

		// Wait for background caching
		time.Sleep(50 * time.Millisecond)

		// Second request - cache hit
		req2 := httptest.NewRequest("GET", "/api/v1/sessions", nil)
		w2 := httptest.NewRecorder()
		p.Forward(w2, req2)

		if w2.Code != http.StatusOK {
			t.Errorf("Second request: expected status 200, got %d", w2.Code)
		}
		if w2.Header().Get("X-Cache") != "HIT" {
			t.Errorf("Second request: expected X-Cache: HIT, got %s", w2.Header().Get("X-Cache"))
		}

		// Verify only one upstream request
		if requestCount != 1 {
			t.Errorf("Expected 1 upstream request, got %d", requestCount)
		}
	})

	t.Run("cache fallback when offline", func(t *testing.T) {
		// Pre-populate cache
		cacheKey := "/api/v1/clips/clip-1?"
		content := []byte(`{"id":"clip-1","name":"Test Clip"}`)
		c.Put(cacheKey, bytes.NewReader(content), "application/json", time.Hour)

		p := New("http://localhost:59999", c)
		p.setOnline(false)

		req := httptest.NewRequest("GET", "/api/v1/clips/clip-1", nil)
		w := httptest.NewRecorder()
		p.Forward(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("Expected status 200, got %d", w.Code)
		}
		if w.Header().Get("X-Cache") != "HIT" {
			t.Errorf("Expected X-Cache: HIT, got %s", w.Header().Get("X-Cache"))
		}
	})

	t.Run("return 503 when offline and not cached", func(t *testing.T) {
		cacheNew, cleanupNew := setupTestCache(t) // Fresh cache
		defer cleanupNew()

		p := New("http://localhost:59999", cacheNew)
		p.setOnline(false)

		req := httptest.NewRequest("GET", "/api/v1/clips/not-cached", nil)
		w := httptest.NewRecorder()
		p.Forward(w, req)

		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("Expected status 503, got %d", w.Code)
		}
	})
}

func TestIsCacheable(t *testing.T) {
	tests := []struct {
		path     string
		expected bool
	}{
		{"/api/v1/sessions", true},
		{"/api/v1/clips/abc123", true},
		{"/api/v1/channels", true},
		{"/api/v1/tags", true},
		{"/api/v1/clips/abc123/stream", true},
		{"/api/v1/clips/abc123/thumbnail", true},
		{"/api/v1/clips/abc123/download", true},
		{"/health", false},
		{"/api/v1/something-else", false},
		{"/random/path", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			result := isCacheable(tt.path)
			if result != tt.expected {
				t.Errorf("isCacheable(%s) = %v, want %v", tt.path, result, tt.expected)
			}
		})
	}
}

func TestDetermineTTL(t *testing.T) {
	tests := []struct {
		path     string
		expected time.Duration
	}{
		{"/api/v1/clips/abc123/stream", 24 * time.Hour},
		{"/api/v1/clips/abc123/download", 24 * time.Hour},
		{"/api/v1/clips/abc123/thumbnail", 12 * time.Hour},
		{"/api/v1/sessions", 5 * time.Minute},
		{"/api/v1/clips", 5 * time.Minute},
		{"/api/v1/channels", 5 * time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			result := determineTTL(tt.path)
			if result != tt.expected {
				t.Errorf("determineTTL(%s) = %v, want %v", tt.path, result, tt.expected)
			}
		})
	}
}

func TestProxy_GetJSON(t *testing.T) {
	c, cleanup := setupTestCache(t)
	defer cleanup()

	t.Run("successful fetch", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"name": "test"})
		}))
		defer server.Close()

		p := New(server.URL, c)

		var result map[string]string
		err := p.GetJSON(context.Background(), "/api/test", &result)
		if err != nil {
			t.Fatalf("GetJSON() unexpected error: %v", err)
		}

		if result["name"] != "test" {
			t.Errorf("Expected name=test, got %s", result["name"])
		}
	})

	t.Run("from cache", func(t *testing.T) {
		cacheKey := "/api/cached"
		content := []byte(`{"cached": true}`)
		c.Put(cacheKey, bytes.NewReader(content), "application/json", time.Hour)

		p := New("http://localhost:59999", c)

		var result map[string]bool
		err := p.GetJSON(context.Background(), cacheKey, &result)
		if err != nil {
			t.Fatalf("GetJSON() unexpected error: %v", err)
		}

		if !result["cached"] {
			t.Error("Expected cached=true")
		}
	})

	t.Run("offline and not cached", func(t *testing.T) {
		cacheNew, cleanupNew := setupTestCache(t)
		defer cleanupNew()

		p := New("http://localhost:59999", cacheNew)
		p.setOnline(false)

		var result map[string]string
		err := p.GetJSON(context.Background(), "/api/not-cached", &result)
		if err == nil {
			t.Error("Expected error when offline and not cached")
		}
	})

	t.Run("upstream error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()

		cacheNew, cleanupNew := setupTestCache(t)
		defer cleanupNew()

		p := New(server.URL, cacheNew)

		var result map[string]string
		err := p.GetJSON(context.Background(), "/api/error", &result)
		if err == nil {
			t.Error("Expected error for upstream error")
		}
	})
}

func TestProxy_PostJSON(t *testing.T) {
	c, cleanup := setupTestCache(t)
	defer cleanup()

	t.Run("successful post", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "POST" {
				t.Errorf("Expected POST, got %s", r.Method)
			}
			if r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("Expected Content-Type: application/json, got %s", r.Header.Get("Content-Type"))
			}

			// Read and verify body
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			if body["name"] != "test" {
				t.Errorf("Expected name=test in body, got %s", body["name"])
			}

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"id": "123", "name": "test"})
		}))
		defer server.Close()

		p := New(server.URL, c)

		requestBody := map[string]string{"name": "test"}
		var result map[string]string
		err := p.PostJSON(context.Background(), "/api/test", requestBody, &result)
		if err != nil {
			t.Fatalf("PostJSON() unexpected error: %v", err)
		}

		if result["id"] != "123" {
			t.Errorf("Expected id=123, got %s", result["id"])
		}
	})

	t.Run("offline", func(t *testing.T) {
		p := New("http://localhost:59999", c)
		p.setOnline(false)

		var result map[string]string
		err := p.PostJSON(context.Background(), "/api/test", map[string]string{}, &result)
		if err == nil {
			t.Error("Expected error when offline")
		}
	})

	t.Run("upstream error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"bad request"}`))
		}))
		defer server.Close()

		p := New(server.URL, c)

		var result map[string]string
		err := p.PostJSON(context.Background(), "/api/test", map[string]string{}, &result)
		if err == nil {
			t.Error("Expected error for upstream error")
		}
	})

	t.Run("nil result", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
		defer server.Close()

		p := New(server.URL, c)

		err := p.PostJSON(context.Background(), "/api/test", map[string]string{}, nil)
		if err != nil {
			t.Fatalf("PostJSON() unexpected error: %v", err)
		}
	})
}

func TestProxy_ForwardRequest_Headers(t *testing.T) {
	c, cleanup := setupTestCache(t)
	defer cleanup()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify headers are forwarded
		if r.Header.Get("X-Custom-Header") != "test-value" {
			t.Errorf("Expected X-Custom-Header: test-value, got %s", r.Header.Get("X-Custom-Header"))
		}

		// Set response headers
		w.Header().Set("X-Response-Header", "response-value")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "OK")
	}))
	defer server.Close()

	p := New(server.URL, c)

	req := httptest.NewRequest("POST", "/api/test", nil)
	req.Header.Set("X-Custom-Header", "test-value")
	w := httptest.NewRecorder()

	p.Forward(w, req)

	// Verify response headers are forwarded
	if w.Header().Get("X-Response-Header") != "response-value" {
		t.Errorf("Expected X-Response-Header: response-value, got %s", w.Header().Get("X-Response-Header"))
	}
}
