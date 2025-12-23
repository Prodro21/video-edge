package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var (
	ErrNotFound = errors.New("item not found in cache")
	ErrExpired  = errors.New("cache item expired")
)

// CacheItem represents a cached file with metadata
type CacheItem struct {
	Key         string    `json:"key"`
	FilePath    string    `json:"file_path"`
	Size        int64     `json:"size"`
	ContentType string    `json:"content_type"`
	CreatedAt   time.Time `json:"created_at"`
	LastAccess  time.Time `json:"last_access"`
	TTL         time.Duration `json:"ttl"`
}

// Cache manages local file caching for the edge server
type Cache struct {
	mu       sync.RWMutex
	basePath string
	items    map[string]*CacheItem
	maxSize  int64 // Maximum cache size in bytes
	curSize  int64 // Current cache size
}

// New creates a new cache instance
func New(basePath string, maxSizeGB int) (*Cache, error) {
	// Ensure cache directory exists
	if err := os.MkdirAll(basePath, 0755); err != nil {
		return nil, err
	}

	c := &Cache{
		basePath: basePath,
		items:    make(map[string]*CacheItem),
		maxSize:  int64(maxSizeGB) * 1024 * 1024 * 1024, // Convert GB to bytes
	}

	// Load existing cache items
	if err := c.loadExisting(); err != nil {
		return nil, err
	}

	return c, nil
}

// loadExisting scans the cache directory for existing files
func (c *Cache) loadExisting() error {
	return filepath.Walk(c.basePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		// Skip metadata files
		if filepath.Ext(path) == ".meta" {
			return nil
		}

		relPath, _ := filepath.Rel(c.basePath, path)
		key := relPath

		c.items[key] = &CacheItem{
			Key:        key,
			FilePath:   path,
			Size:       info.Size(),
			CreatedAt:  info.ModTime(),
			LastAccess: info.ModTime(),
		}
		c.curSize += info.Size()

		return nil
	})
}

// generateKey creates a cache key from a URL or identifier
func generateKey(identifier string) string {
	hash := sha256.Sum256([]byte(identifier))
	return hex.EncodeToString(hash[:16])
}

// Put stores a file in the cache
func (c *Cache) Put(key string, reader io.Reader, contentType string, ttl time.Duration) (*CacheItem, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Generate file path
	hashedKey := generateKey(key)
	filePath := filepath.Join(c.basePath, hashedKey)

	// Create file
	file, err := os.Create(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	// Copy content
	size, err := io.Copy(file, reader)
	if err != nil {
		os.Remove(filePath)
		return nil, err
	}

	// Evict if necessary
	for c.curSize+size > c.maxSize {
		if err := c.evictOldest(); err != nil {
			break // No more items to evict
		}
	}

	// Create cache item
	item := &CacheItem{
		Key:         key,
		FilePath:    filePath,
		Size:        size,
		ContentType: contentType,
		CreatedAt:   time.Now(),
		LastAccess:  time.Now(),
		TTL:         ttl,
	}

	c.items[key] = item
	c.curSize += size

	return item, nil
}

// Get retrieves a file from the cache
func (c *Cache) Get(key string) (*CacheItem, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	item, exists := c.items[key]
	if !exists {
		return nil, ErrNotFound
	}

	// Check if expired
	if item.TTL > 0 && time.Since(item.CreatedAt) > item.TTL {
		c.removeItem(key)
		return nil, ErrExpired
	}

	// Update last access
	item.LastAccess = time.Now()

	return item, nil
}

// GetReader returns a reader for a cached file
func (c *Cache) GetReader(key string) (io.ReadCloser, *CacheItem, error) {
	item, err := c.Get(key)
	if err != nil {
		return nil, nil, err
	}

	file, err := os.Open(item.FilePath)
	if err != nil {
		return nil, nil, err
	}

	return file, item, nil
}

// Has checks if a key exists in the cache
func (c *Cache) Has(key string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, exists := c.items[key]
	return exists
}

// Delete removes an item from the cache
func (c *Cache) Delete(key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.removeItem(key)
}

// removeItem removes an item without locking (caller must hold lock)
func (c *Cache) removeItem(key string) error {
	item, exists := c.items[key]
	if !exists {
		return nil
	}

	if err := os.Remove(item.FilePath); err != nil && !os.IsNotExist(err) {
		return err
	}

	c.curSize -= item.Size
	delete(c.items, key)
	return nil
}

// evictOldest removes the least recently accessed item
func (c *Cache) evictOldest() error {
	var oldestKey string
	var oldestTime time.Time

	for key, item := range c.items {
		if oldestKey == "" || item.LastAccess.Before(oldestTime) {
			oldestKey = key
			oldestTime = item.LastAccess
		}
	}

	if oldestKey == "" {
		return errors.New("no items to evict")
	}

	return c.removeItem(oldestKey)
}

// Stats returns cache statistics
func (c *Cache) Stats() CacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return CacheStats{
		ItemCount:   len(c.items),
		CurrentSize: c.curSize,
		MaxSize:     c.maxSize,
		UsagePercent: float64(c.curSize) / float64(c.maxSize) * 100,
	}
}

// CacheStats contains cache statistics
type CacheStats struct {
	ItemCount    int     `json:"item_count"`
	CurrentSize  int64   `json:"current_size_bytes"`
	MaxSize      int64   `json:"max_size_bytes"`
	UsagePercent float64 `json:"usage_percent"`
}

// Clear removes all items from the cache
func (c *Cache) Clear() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	for key := range c.items {
		if err := c.removeItem(key); err != nil {
			return err
		}
	}
	return nil
}
