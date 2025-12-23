package cache

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNew(t *testing.T) {
	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "cache-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	t.Run("create new cache", func(t *testing.T) {
		c, err := New(tmpDir, 1)
		if err != nil {
			t.Fatalf("New() unexpected error: %v", err)
		}
		if c == nil {
			t.Fatal("New() returned nil cache")
		}
		if c.maxSize != 1*1024*1024*1024 {
			t.Errorf("New() maxSize = %v, want %v", c.maxSize, 1*1024*1024*1024)
		}
	})

	t.Run("create with new subdirectory", func(t *testing.T) {
		subDir := filepath.Join(tmpDir, "subdir", "cache")
		c, err := New(subDir, 1)
		if err != nil {
			t.Fatalf("New() unexpected error: %v", err)
		}
		if c == nil {
			t.Fatal("New() returned nil cache")
		}
		// Verify directory was created
		if _, err := os.Stat(subDir); os.IsNotExist(err) {
			t.Error("New() did not create cache directory")
		}
	})
}

func TestCache_PutAndGet(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cache-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	c, err := New(tmpDir, 1)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	t.Run("put and get item", func(t *testing.T) {
		content := []byte("test content")
		reader := bytes.NewReader(content)

		item, err := c.Put("test-key", reader, "text/plain", time.Hour)
		if err != nil {
			t.Fatalf("Put() unexpected error: %v", err)
		}
		if item == nil {
			t.Fatal("Put() returned nil item")
		}
		if item.Key != "test-key" {
			t.Errorf("Put() item.Key = %v, want %v", item.Key, "test-key")
		}
		if item.Size != int64(len(content)) {
			t.Errorf("Put() item.Size = %v, want %v", item.Size, len(content))
		}
		if item.ContentType != "text/plain" {
			t.Errorf("Put() item.ContentType = %v, want %v", item.ContentType, "text/plain")
		}

		// Get the item
		retrieved, err := c.Get("test-key")
		if err != nil {
			t.Fatalf("Get() unexpected error: %v", err)
		}
		if retrieved.Key != item.Key {
			t.Errorf("Get() item.Key = %v, want %v", retrieved.Key, item.Key)
		}
	})

	t.Run("get non-existent item", func(t *testing.T) {
		_, err := c.Get("non-existent")
		if err == nil {
			t.Error("Get() expected error for non-existent key")
		}
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("Get() error = %v, want ErrNotFound", err)
		}
	})
}

func TestCache_GetReader(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cache-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	c, err := New(tmpDir, 1)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	content := []byte("test content for reader")
	_, err = c.Put("reader-key", bytes.NewReader(content), "text/plain", time.Hour)
	if err != nil {
		t.Fatalf("Put() unexpected error: %v", err)
	}

	t.Run("get reader for cached item", func(t *testing.T) {
		reader, item, err := c.GetReader("reader-key")
		if err != nil {
			t.Fatalf("GetReader() unexpected error: %v", err)
		}
		defer reader.Close()

		if item == nil {
			t.Fatal("GetReader() returned nil item")
		}

		// Read content
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("ReadAll() unexpected error: %v", err)
		}

		if !bytes.Equal(data, content) {
			t.Errorf("GetReader() content = %v, want %v", string(data), string(content))
		}
	})

	t.Run("get reader for non-existent item", func(t *testing.T) {
		_, _, err := c.GetReader("non-existent")
		if err == nil {
			t.Error("GetReader() expected error for non-existent key")
		}
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("GetReader() error = %v, want ErrNotFound", err)
		}
	})
}

func TestCache_Has(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cache-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	c, err := New(tmpDir, 1)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	_, err = c.Put("existing-key", bytes.NewReader([]byte("content")), "text/plain", time.Hour)
	if err != nil {
		t.Fatalf("Put() unexpected error: %v", err)
	}

	t.Run("has existing key", func(t *testing.T) {
		if !c.Has("existing-key") {
			t.Error("Has() returned false for existing key")
		}
	})

	t.Run("has non-existent key", func(t *testing.T) {
		if c.Has("non-existent") {
			t.Error("Has() returned true for non-existent key")
		}
	})
}

func TestCache_Delete(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cache-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	c, err := New(tmpDir, 1)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	_, err = c.Put("delete-key", bytes.NewReader([]byte("content")), "text/plain", time.Hour)
	if err != nil {
		t.Fatalf("Put() unexpected error: %v", err)
	}

	t.Run("delete existing key", func(t *testing.T) {
		if !c.Has("delete-key") {
			t.Fatal("Key should exist before delete")
		}

		err := c.Delete("delete-key")
		if err != nil {
			t.Fatalf("Delete() unexpected error: %v", err)
		}

		if c.Has("delete-key") {
			t.Error("Delete() key still exists after delete")
		}
	})

	t.Run("delete non-existent key", func(t *testing.T) {
		err := c.Delete("non-existent")
		if err != nil {
			t.Errorf("Delete() unexpected error for non-existent key: %v", err)
		}
	})
}

func TestCache_TTL(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cache-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	c, err := New(tmpDir, 1)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	t.Run("expired item returns error", func(t *testing.T) {
		// Put with very short TTL
		_, err := c.Put("expire-key", bytes.NewReader([]byte("content")), "text/plain", time.Millisecond)
		if err != nil {
			t.Fatalf("Put() unexpected error: %v", err)
		}

		// Wait for expiration
		time.Sleep(5 * time.Millisecond)

		_, err = c.Get("expire-key")
		if err == nil {
			t.Error("Get() expected error for expired key")
		}
		if !errors.Is(err, ErrExpired) {
			t.Errorf("Get() error = %v, want ErrExpired", err)
		}
	})
}

func TestCache_Stats(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cache-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	c, err := New(tmpDir, 1)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	t.Run("empty cache stats", func(t *testing.T) {
		stats := c.Stats()
		if stats.ItemCount != 0 {
			t.Errorf("Stats() ItemCount = %v, want 0", stats.ItemCount)
		}
		if stats.CurrentSize != 0 {
			t.Errorf("Stats() CurrentSize = %v, want 0", stats.CurrentSize)
		}
	})

	t.Run("cache with items", func(t *testing.T) {
		content := []byte("test content")
		_, err := c.Put("stats-key", bytes.NewReader(content), "text/plain", time.Hour)
		if err != nil {
			t.Fatalf("Put() unexpected error: %v", err)
		}

		stats := c.Stats()
		if stats.ItemCount != 1 {
			t.Errorf("Stats() ItemCount = %v, want 1", stats.ItemCount)
		}
		if stats.CurrentSize != int64(len(content)) {
			t.Errorf("Stats() CurrentSize = %v, want %v", stats.CurrentSize, len(content))
		}
	})
}

func TestCache_Clear(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cache-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	c, err := New(tmpDir, 1)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	// Add some items
	for i := 0; i < 5; i++ {
		key := "key-" + string(rune('0'+i))
		_, err := c.Put(key, bytes.NewReader([]byte("content")), "text/plain", time.Hour)
		if err != nil {
			t.Fatalf("Put() unexpected error: %v", err)
		}
	}

	stats := c.Stats()
	if stats.ItemCount != 5 {
		t.Fatalf("Expected 5 items before clear, got %d", stats.ItemCount)
	}

	err = c.Clear()
	if err != nil {
		t.Fatalf("Clear() unexpected error: %v", err)
	}

	stats = c.Stats()
	if stats.ItemCount != 0 {
		t.Errorf("Clear() ItemCount = %v, want 0", stats.ItemCount)
	}
	if stats.CurrentSize != 0 {
		t.Errorf("Clear() CurrentSize = %v, want 0", stats.CurrentSize)
	}
}

func TestGenerateKey(t *testing.T) {
	t.Run("consistent hashing", func(t *testing.T) {
		key1 := generateKey("test-url")
		key2 := generateKey("test-url")
		if key1 != key2 {
			t.Errorf("generateKey() not consistent: %v != %v", key1, key2)
		}
	})

	t.Run("different inputs produce different keys", func(t *testing.T) {
		key1 := generateKey("url1")
		key2 := generateKey("url2")
		if key1 == key2 {
			t.Error("generateKey() produced same key for different inputs")
		}
	})
}
