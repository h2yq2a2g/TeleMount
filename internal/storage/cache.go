package storage

import (
	"container/list"
	"context"
	"sync"
)

type chunkKey struct {
	msgID    int
	chunkIdx int64
}

type cacheEntry struct {
	key  chunkKey
	data []byte
}

type CacheStats struct {
	UsedBytes int64 `json:"used_bytes"`
	MaxBytes  int64 `json:"max_bytes"`
	Entries   int   `json:"entries"`
	Hits      int64 `json:"hits"`
	Misses    int64 `json:"misses"`
}

// ChunkCache is a thread-safe in-RAM LRU cache for raw Telegram 512 KiB chunks
// whose byte budget can be resized live from the Web UI without restarting.
type ChunkCache struct {
	mu       sync.Mutex
	maxBytes int64
	used     int64
	ll       *list.List
	items    map[chunkKey]*list.Element
	hits     int64
	misses   int64

	// Singleflight deduplication so concurrent reads for the same chunk issue only one Telegram RPC.
	flightMu sync.Mutex
	inflight map[chunkKey]*flightCall
}

type flightCall struct {
	done chan struct{}
	data []byte
	err  error
}

func NewChunkCache(maxBytes int64) *ChunkCache {
	if maxBytes < 0 {
		maxBytes = 0
	}
	return &ChunkCache{
		maxBytes: maxBytes,
		ll:       list.New(),
		items:    make(map[chunkKey]*list.Element),
		inflight: make(map[chunkKey]*flightCall),
	}
}

func (c *ChunkCache) Get(msgID int, chunkIdx int64) ([]byte, bool) {
	key := chunkKey{msgID: msgID, chunkIdx: chunkIdx}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		if ent, ok := el.Value.(*cacheEntry); ok {
			c.ll.MoveToFront(el)
			c.hits++
			return ent.data, true
		}
	}
	c.misses++
	return nil, false
}

func (c *ChunkCache) Put(msgID int, chunkIdx int64, data []byte) {
	size := int64(len(data))
	if size == 0 {
		return
	}
	key := chunkKey{msgID: msgID, chunkIdx: chunkIdx}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.maxBytes <= 0 || size > c.maxBytes {
		return
	}
	if el, ok := c.items[key]; ok {
		if ent, ok := el.Value.(*cacheEntry); ok {
			c.used -= int64(len(ent.data))
			ent.data = data
			c.used += size
			c.ll.MoveToFront(el)
			c.evictLocked()
			return
		}
	}
	el := c.ll.PushFront(&cacheEntry{key: key, data: data})
	c.items[key] = el
	c.used += size
	c.evictLocked()
}

// GetOrFetch checks the RAM cache first, deduplicates concurrent fetches for the same chunk,
// caches the result on success, and respects ctx cancellation while waiting without letting
// one canceled waiter poison other concurrent waiters sharing the same inflight fetch.
func (c *ChunkCache) GetOrFetch(ctx context.Context, msgID int, chunkIdx int64, fetch func() ([]byte, error)) ([]byte, error) {
	if data, ok := c.Get(msgID, chunkIdx); ok {
		return data, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	key := chunkKey{msgID: msgID, chunkIdx: chunkIdx}
	c.flightMu.Lock()
	call, ok := c.inflight[key]
	if !ok {
		call = &flightCall{done: make(chan struct{})}
		c.inflight[key] = call
		go func() {
			data, err := fetch()
			if err == nil && len(data) > 0 {
				c.Put(msgID, chunkIdx, data)
			}
			call.data = data
			call.err = err
			close(call.done)

			c.flightMu.Lock()
			delete(c.inflight, key)
			c.flightMu.Unlock()
		}()
	}
	c.flightMu.Unlock()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-call.done:
		return call.data, call.err
	}
}

func (c *ChunkCache) SetMaxBytes(maxBytes int64) {
	if maxBytes < 0 {
		maxBytes = 0
	}
	c.mu.Lock()
	c.maxBytes = maxBytes
	c.evictLocked()
	c.mu.Unlock()
}

func (c *ChunkCache) MaxBytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.maxBytes
}

// HasOrInflight reports whether the chunk is already in RAM or currently being fetched.
func (c *ChunkCache) HasOrInflight(msgID int, chunkIdx int64) bool {
	key := chunkKey{msgID: msgID, chunkIdx: chunkIdx}
	c.mu.Lock()
	_, cached := c.items[key]
	c.mu.Unlock()
	if cached {
		return true
	}
	c.flightMu.Lock()
	_, flying := c.inflight[key]
	c.flightMu.Unlock()
	return flying
}

func (c *ChunkCache) Clear() {
	c.mu.Lock()
	c.ll.Init()
	c.items = make(map[chunkKey]*list.Element)
	c.used = 0
	c.mu.Unlock()
}

func (c *ChunkCache) Stats() CacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return CacheStats{
		UsedBytes: c.used,
		MaxBytes:  c.maxBytes,
		Entries:   c.ll.Len(),
		Hits:      c.hits,
		Misses:    c.misses,
	}
}

func (c *ChunkCache) evictLocked() {
	for c.used > c.maxBytes {
		el := c.ll.Back()
		if el == nil {
			break
		}
		c.ll.Remove(el)
		if ent, ok := el.Value.(*cacheEntry); ok {
			delete(c.items, ent.key)
			c.used -= int64(len(ent.data))
		}
	}
}
