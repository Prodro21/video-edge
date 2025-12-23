package sync

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Operation types for offline queue
type OperationType string

const (
	OpCreateClip    OperationType = "create_clip"
	OpUpdateClip    OperationType = "update_clip"
	OpFavoriteClip  OperationType = "favorite_clip"
	OpCreateTag     OperationType = "create_tag"
	OpUpdateTag     OperationType = "update_tag"
	OpStartSession  OperationType = "start_session"
	OpEndSession    OperationType = "end_session"
)

// QueuedOperation represents an operation to be synced when online
type QueuedOperation struct {
	ID        string          `json:"id"`
	Type      OperationType   `json:"type"`
	Path      string          `json:"path"`
	Method    string          `json:"method"`
	Body      json.RawMessage `json:"body,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	Attempts  int             `json:"attempts"`
	LastError string          `json:"last_error,omitempty"`
}

// Syncer manages offline queue and cloud synchronization
type Syncer struct {
	mu         sync.Mutex
	queue      []*QueuedOperation
	queueFile  string
	upstreamFn func(ctx context.Context, method, path string, body []byte) error
	stopCh     chan struct{}
	wg         sync.WaitGroup
}

// New creates a new syncer instance
func New(dataDir string, upstreamFn func(ctx context.Context, method, path string, body []byte) error) (*Syncer, error) {
	queueFile := filepath.Join(dataDir, "sync_queue.json")

	s := &Syncer{
		queueFile:  queueFile,
		upstreamFn: upstreamFn,
		stopCh:     make(chan struct{}),
	}

	// Load existing queue
	if err := s.loadQueue(); err != nil {
		log.Printf("Warning: failed to load sync queue: %v", err)
	}

	return s, nil
}

// loadQueue loads the queue from disk
func (s *Syncer) loadQueue() error {
	data, err := os.ReadFile(s.queueFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}

	return json.Unmarshal(data, &s.queue)
}

// saveQueue persists the queue to disk
func (s *Syncer) saveQueue() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := json.MarshalIndent(s.queue, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(s.queueFile, data, 0644)
}

// Enqueue adds an operation to the sync queue
func (s *Syncer) Enqueue(op OperationType, method, path string, body interface{}) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var bodyJSON json.RawMessage
	if body != nil {
		var err error
		bodyJSON, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}

	qop := &QueuedOperation{
		ID:        generateID(),
		Type:      op,
		Path:      path,
		Method:    method,
		Body:      bodyJSON,
		CreatedAt: time.Now(),
	}

	s.queue = append(s.queue, qop)

	// Persist to disk
	go s.saveQueue()

	log.Printf("Enqueued operation: %s %s", method, path)
	return nil
}

// QueueLength returns the number of pending operations
func (s *Syncer) QueueLength() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue)
}

// GetQueue returns a copy of the current queue
func (s *Syncer) GetQueue() []*QueuedOperation {
	s.mu.Lock()
	defer s.mu.Unlock()

	result := make([]*QueuedOperation, len(s.queue))
	copy(result, s.queue)
	return result
}

// Start begins the background sync process
func (s *Syncer) Start(checkInterval time.Duration, isOnlineFn func() bool) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.syncLoop(checkInterval, isOnlineFn)
	}()
}

// Stop halts the background sync process
func (s *Syncer) Stop() {
	close(s.stopCh)
	s.wg.Wait()
}

// syncLoop periodically attempts to sync queued operations
func (s *Syncer) syncLoop(interval time.Duration, isOnlineFn func() bool) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			if isOnlineFn() && s.QueueLength() > 0 {
				s.processQueue()
			}
		}
	}
}

// processQueue attempts to sync all queued operations
func (s *Syncer) processQueue() {
	s.mu.Lock()
	queue := make([]*QueuedOperation, len(s.queue))
	copy(queue, s.queue)
	s.mu.Unlock()

	var failed []*QueuedOperation
	var synced int

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, op := range queue {
		err := s.upstreamFn(ctx, op.Method, op.Path, op.Body)
		if err != nil {
			op.Attempts++
			op.LastError = err.Error()
			failed = append(failed, op)
			log.Printf("Sync failed for %s %s: %v (attempt %d)", op.Method, op.Path, err, op.Attempts)
		} else {
			synced++
			log.Printf("Synced: %s %s", op.Method, op.Path)
		}
	}

	// Update queue with only failed operations
	s.mu.Lock()
	s.queue = failed
	s.mu.Unlock()

	if synced > 0 {
		log.Printf("Sync complete: %d synced, %d remaining", synced, len(failed))
		s.saveQueue()
	}
}

// ClearQueue removes all pending operations
func (s *Syncer) ClearQueue() {
	s.mu.Lock()
	s.queue = nil
	s.mu.Unlock()
	s.saveQueue()
}

// RemoveOperation removes a specific operation from the queue
func (s *Syncer) RemoveOperation(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, op := range s.queue {
		if op.ID == id {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			go s.saveQueue()
			return true
		}
	}
	return false
}

// generateID creates a unique operation ID
func generateID() string {
	return time.Now().Format("20060102150405.000000")
}

// SyncStatus represents the current sync state
type SyncStatus struct {
	IsOnline       bool      `json:"is_online"`
	PendingOps     int       `json:"pending_operations"`
	LastSync       time.Time `json:"last_sync,omitempty"`
	OldestPending  time.Time `json:"oldest_pending,omitempty"`
}

// Status returns the current sync status
func (s *Syncer) Status(isOnline bool) SyncStatus {
	s.mu.Lock()
	defer s.mu.Unlock()

	status := SyncStatus{
		IsOnline:   isOnline,
		PendingOps: len(s.queue),
	}

	if len(s.queue) > 0 {
		status.OldestPending = s.queue[0].CreatedAt
	}

	return status
}
