package imaging

import (
	"bytes"
	"testing"
	"time"

	"locless-vhd/proto"
	"locless-vhd/server/store"
)

// deployBody fetches blocks the way the agent does and returns the response.
func deployBody(t *testing.T, m *Manager, task int64, numbers []uint32) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := m.DeployBlocks(task, numbers, &out); err != nil {
		t.Fatalf("DeployBlocks: %v", err)
	}
	return out.Bytes()
}

// TestReadCacheSendsTheSameBlocks: with the cache on, the agent gets the same
// bytes, blocks come from memory, and the end of the deploy gives the memory back.
func TestReadCacheSendsTheSameBlocks(t *testing.T) {
	m, s, _ := setup(t)
	blocks := map[uint32][]byte{}
	for pos := range uint32(30) { // the test partition holds 32
		blocks[pos] = block(byte(pos + 1))
	}
	clone(t, m, 1, blocks)

	var numbers []uint32
	plain := func() []byte {
		if _, err := m.StartDeploy(ctx, 2, "win11"); err != nil {
			t.Fatalf("StartDeploy: %v", err)
		}
		defer m.Abort(2)
		d, _ := m.deploy(2)
		numbers = numbers[:0]
		for i, e := range d.entries {
			if !e.Zero() {
				numbers = append(numbers, uint32(i))
			}
		}
		return deployBody(t, m, 2, numbers)
	}()

	if err := s.SetSetting(ctx, store.SettingReadCache, "1"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if _, err := m.StartDeploy(ctx, 3, "win11"); err != nil {
		t.Fatalf("StartDeploy: %v", err)
	}
	d, _ := m.deploy(3)
	if d.ahead == nil {
		t.Fatal("the cache is on, yet nothing reads ahead")
	}
	// Give the reading ahead the time the countdown of the agent would.
	deadline := time.Now().Add(5 * time.Second)
	for !d.ahead.has(numbers[len(numbers)-1]) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	var got []byte
	for i := 0; i < len(numbers); i += 7 { // batches, as the agent asks
		got = append(got, deployBody(t, m, 3, numbers[i:min(len(numbers), i+7)])...)
	}
	if !bytes.Equal(got, plain) {
		t.Fatal("the cache changed what was sent")
	}
	_, amounts := m.ServerStats(3)
	var cached uint64
	for _, a := range amounts {
		if a.Name == "отдано из кеша чтения" {
			cached = a.Value
		}
	}
	if cached == 0 {
		t.Fatalf("nothing came from the cache: %+v", amounts)
	}
	m.Abort(3)
	if m.cache.used != 0 {
		t.Fatalf("%d bytes still held after the deploy", m.cache.used)
	}
}

// TestReadCacheStaysWithinItsLimit: a reader that is never asked stops at the
// limit instead of reading the whole image into memory.
func TestReadCacheStaysWithinItsLimit(t *testing.T) {
	m, _, _ := setup(t)
	blocks := map[uint32][]byte{}
	for pos := range uint32(30) { // the test partition holds 32
		blocks[pos] = block(byte(pos + 1))
	}
	clone(t, m, 1, blocks)
	if _, err := m.StartDeploy(ctx, 2, "win11"); err != nil {
		t.Fatalf("StartDeploy: %v", err)
	}
	defer m.Abort(2)
	d, _ := m.deploy(2)
	limit := int64(readRun + proto.DefaultBlockSize)
	m.cache.setLimit(limit)
	d.ahead = newPrefetcher(m.cache, d.chain, d.entries)
	defer d.ahead.stop()
	time.Sleep(200 * time.Millisecond)
	m.cache.mu.Lock()
	used := m.cache.used
	m.cache.mu.Unlock()
	if used == 0 || used > limit {
		t.Fatalf("used %d of %d", used, limit)
	}
}
