package imaging

import (
	"context"
	"strconv"
	"sync"

	"locless-vhd/proto"
	"locless-vhd/server/store"
)

// MaxReadCacheGiB limits the read cache the panel may set.
const MaxReadCacheGiB = 256

// readCacheLimit reads the setting; a bad or missing value turns the cache off.
func (m *Manager) readCacheLimit(ctx context.Context) int64 {
	v, err := m.store.Setting(ctx, store.SettingReadCache)
	if err != nil || v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > MaxReadCacheGiB {
		return 0
	}
	return int64(n) << 30
}

// readCache is the memory all deploys together may hold in blocks read ahead.
// The limit is a setting of the panel; 0 turns the reading ahead off.
type readCache struct {
	mu    sync.Mutex
	cond  *sync.Cond
	limit int64
	used  int64
}

func newReadCache() *readCache {
	c := &readCache{}
	c.cond = sync.NewCond(&c.mu)
	return c
}

// setLimit changes the limit; a deploy already waiting for room sees it at once.
func (c *readCache) setLimit(n int64) {
	c.mu.Lock()
	c.limit = max(n, 0)
	c.mu.Unlock()
	c.cond.Broadcast()
}

func (c *readCache) enabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.limit > 0
}

// take reserves n bytes, waiting while the cache is full. It gives up and returns
// false once stop says so or the cache is turned off.
func (c *readCache) take(n int64, stop func() bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for c.used+n > c.limit && c.limit > 0 && !stop() {
		c.cond.Wait()
	}
	if c.limit == 0 || stop() {
		return false
	}
	c.used += n
	return true
}

func (c *readCache) give(n int64) {
	if n == 0 {
		return
	}
	c.mu.Lock()
	c.used -= n
	c.mu.Unlock()
	c.cond.Broadcast()
}

// wake lets every waiting take look at its stop again.
func (c *readCache) wake() { c.cond.Broadcast() }

// prefetchBehind is how far behind the newest request a block read ahead may
// be before it is dropped: the agent has several requests in flight, so blocks
// arrive a little out of order, but one this far back was not wanted.
const prefetchBehind = 1024

// prefetcher reads the blocks of one deploy in the order of the list, ahead of
// what the agent asks for, into memory. The disk then reads long runs at its own
// pace instead of short ones whenever a request comes, and a request finds its
// blocks ready.
type prefetcher struct {
	cache   *readCache
	chain   *proto.Chain
	entries []proto.Entry

	mu      sync.Mutex
	ready   map[uint32]readyBlock
	front   int  // one past the highest number asked for so far
	next    int  // the next number to read ahead
	stopped bool // the deploy is over
}

type readyBlock struct {
	block proto.StoredBlock
	size  int64 // what it holds of the cache
}

func newPrefetcher(cache *readCache, chain *proto.Chain, entries []proto.Entry) *prefetcher {
	p := &prefetcher{cache: cache, chain: chain, entries: entries, ready: map[uint32]readyBlock{}}
	go p.run()
	return p
}

// take returns a block read ahead and forgets it: each block is sent once.
func (p *prefetcher) take(n uint32) (proto.StoredBlock, bool) {
	p.mu.Lock()
	b, ok := p.ready[n]
	if ok {
		delete(p.ready, n)
	}
	p.mu.Unlock()
	if ok {
		p.cache.give(b.size)
	}
	return b.block, ok
}

// has reports whether a block is ready in memory.
func (p *prefetcher) has(n uint32) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.ready[n]
	return ok
}

// asked moves the front to a request for numbers. Blocks far behind it are
// dropped, so a block nobody asks for cannot hold the cache for good.
func (p *prefetcher) asked(numbers []uint32) {
	var freed int64
	p.mu.Lock()
	for _, n := range numbers {
		p.front = max(p.front, int(n)+1)
	}
	for n, b := range p.ready {
		if int(n)+prefetchBehind < p.front {
			delete(p.ready, n)
			freed += b.size
		}
	}
	p.mu.Unlock()
	p.cache.give(freed)
}

func (p *prefetcher) stop() {
	var freed int64
	p.mu.Lock()
	p.stopped = true
	for _, b := range p.ready {
		freed += b.size
	}
	p.ready = nil
	p.mu.Unlock()
	p.cache.give(freed)
	p.cache.wake()
}

func (p *prefetcher) isStopped() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stopped
}

// run reads ahead until the end of the list or of the deploy. A read error only
// ends the reading ahead: the request reads the block itself and reports it.
func (p *prefetcher) run() {
	look := make([]proto.Entry, 0, 64)
	numbers := make([]uint32, 0, 64)
	for {
		p.mu.Lock()
		if p.stopped {
			p.mu.Unlock()
			return
		}
		// Blocks the agent asked for already are read by the request itself.
		p.next = max(p.next, p.front)
		start := p.next
		p.mu.Unlock()

		look, numbers = look[:0], numbers[:0]
		for i := start; i < len(p.entries) && len(look) < cap(look); i++ {
			if p.entries[i].Zero() {
				if len(look) > 0 {
					break // the agent never asks for a zero block, so a run ends here
				}
				start = i + 1
				continue
			}
			look = append(look, p.entries[i])
			numbers = append(numbers, uint32(i))
		}
		if len(look) == 0 {
			return // everything is read ahead or asked for
		}
		if !p.cache.take(readRun, p.isStopped) {
			return
		}
		run, err := p.chain.StoredRun(look, readRun)
		if err != nil {
			p.cache.give(readRun)
			return
		}
		var held int64
		p.mu.Lock()
		for i, b := range run {
			n := numbers[i]
			if p.stopped || int(n) < p.front {
				continue // asked for meanwhile: the request read it
			}
			size := int64(proto.BlockHeaderSize + len(b.Payload))
			p.ready[n] = readyBlock{b, size}
			held += size
		}
		p.next = int(numbers[len(run)-1]) + 1
		p.mu.Unlock()
		p.cache.give(readRun - held)
	}
}
