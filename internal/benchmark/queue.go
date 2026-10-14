package benchmark

import (
	"context"
	"sync"
	"time"
)

// Queue is a durable FIFO of run IDs waiting for workers.
//
// Redis (list push/pop) is the primary backend so workers scale horizontally
// across replicas. When Redis is absent the in-memory fallback keeps a
// single-replica install working with identical semantics.
type Queue struct {
	mu      sync.Mutex
	memory  []string
	redis   RedisList
	key     string
	maxSize int
	queued  int64
}

// RedisList is the minimal Redis surface the queue needs.
type RedisList interface {
	LPush(ctx context.Context, key string, values ...string) (int64, error)
	BRPop(ctx context.Context, timeout time.Duration, keys ...string) (string, error)
	LLen(ctx context.Context, key string) (int64, error)
}

// NewQueue builds a queue. redis may be nil (in-memory only).
func NewQueue(redis RedisList, key string, maxSize int) *Queue {
	if key == "" {
		key = "benchmark:queue"
	}
	if maxSize <= 0 {
		maxSize = 1000
	}
	q := &Queue{redis: redis, key: key, maxSize: maxSize}
	return q
}

// Enqueue adds a run ID. It never blocks the scheduler: when the queue is
// full the run is dropped and reported, because a benchmark must never apply
// backpressure to the gateway.
func (q *Queue) Enqueue(ctx context.Context, runID string) error {
	if q.redis != nil {
		if _, err := q.redis.LPush(ctx, q.key, runID); err == nil {
			return nil
		}
		// Redis failure degrades to memory rather than losing the run.
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.memory) >= q.maxSize {
		return errQueueFull
	}
	q.memory = append(q.memory, runID)
	q.queued++
	return nil
}

// Dequeue removes the next run ID, waiting up to timeout.
func (q *Queue) Dequeue(ctx context.Context, timeout time.Duration) (string, bool) {
	if q.redis != nil {
		if id, err := q.redis.BRPop(ctx, timeout, q.key); err == nil && id != "" {
			return id, true
		}
	}
	// Memory fallback with deadline.
	deadline := time.Now().Add(timeout)
	for {
		q.mu.Lock()
		if len(q.memory) > 0 {
			id := q.memory[0]
			q.memory = q.memory[1:]
			q.mu.Unlock()
			return id, true
		}
		q.mu.Unlock()
		if time.Now().After(deadline) || ctx.Err() != nil {
			return "", false
		}
		select {
		case <-ctx.Done():
			return "", false
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Size reports the pending depth (Redis length preferred, memory otherwise).
func (q *Queue) Size(ctx context.Context) int {
	if q.redis != nil {
		if n, err := q.redis.LLen(ctx, q.key); err == nil {
			return int(n)
		}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.memory)
}

type queueFullError struct{}

func (queueFullError) Error() string { return "the benchmark queue is full" }

var errQueueFull = queueFullError{}
