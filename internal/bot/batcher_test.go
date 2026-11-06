package bot

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// These tests exercise the buffering/debounce machinery directly (no DB, no
// Telegram): grouping, caps, per-conversation isolation, ordering flush, shutdown
// flush and the disabled mode. The end-to-end delivery behaviour is covered by
// batch_box_integration_test.go.

// flushRecord is one batch handed to the flush callback.
type flushRecord struct {
	key   convKey
	items []batchItem
}

// recorder collects flush callbacks in a thread-safe channel for assertion.
type recorder struct {
	mu     sync.Mutex
	nextID int64
	ch     chan flushRecord
}

func newRecorder() *recorder { return &recorder{ch: make(chan flushRecord, 64)} }

func (r *recorder) flush(key convKey, items []batchItem) {
	r.ch <- flushRecord{key: key, items: items}
}

// next waits for one flush record.
func (r *recorder) next(t *testing.T, timeout time.Duration) flushRecord {
	t.Helper()
	select {
	case rec := <-r.ch:
		return rec
	case <-time.After(timeout):
		t.Fatalf("no flush within %s", timeout)
		return flushRecord{}
	}
}

// expectNoFlush asserts no flush arrives within d.
func (r *recorder) expectNoFlush(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case rec := <-r.ch:
		t.Fatalf("unexpected flush: key=%+v items=%d", rec.key, len(rec.items))
	case <-time.After(d):
	}
}

// dummyMessage builds a message with an id; the batcher never inspects content.
func dummyMessage(id int64) *gotgbot.Message {
	return &gotgbot.Message{MessageId: id}
}

func keyFor(a, b string) convKey { return convKey{senderUID: a, targetUID: b} }

func testBatcher(window, maxWait time.Duration, maxItems int, rec *recorder) *batcher {
	return newBatcher(window, maxWait, maxItems, rec.flush)
}

// TestBatcherGroupsBurst: several rapid messages to one conversation arrive as ONE
// flush holding all of them, in arrival order.
func TestBatcherGroupsBurst(t *testing.T) {
	rec := newRecorder()
	b := testBatcher(60*time.Millisecond, time.Second, 10, rec)
	defer b.Shutdown(time.Second)
	key := keyFor("sender", "target")

	for i := 1; i <= 3; i++ {
		b.Add(key, batchItem{msg: dummyMessage(int64(i))})
	}
	got := rec.next(t, 2*time.Second)
	if got.key != key {
		t.Fatalf("flush key = %+v; want %+v", got.key, key)
	}
	if len(got.items) != 3 {
		t.Fatalf("flush carried %d items; want 3", len(got.items))
	}
	for i, it := range got.items {
		if it.msg.MessageId != int64(i+1) {
			t.Errorf("item %d id = %d; want %d (arrival order)", i, it.msg.MessageId, i+1)
		}
	}
}

// TestBatcherSeparatesPauses: a pause longer than the window ends the batch; the
// next message becomes a new one (an intentional separate message is not merged).
func TestBatcherSeparatesPauses(t *testing.T) {
	rec := newRecorder()
	b := testBatcher(40*time.Millisecond, time.Second, 10, rec)
	defer b.Shutdown(time.Second)
	key := keyFor("s", "t")

	b.Add(key, batchItem{msg: dummyMessage(1)})
	if got := rec.next(t, 2*time.Second); len(got.items) != 1 {
		t.Fatalf("first flush carried %d items; want 1", len(got.items))
	}

	b.Add(key, batchItem{msg: dummyMessage(2)})
	got := rec.next(t, 2*time.Second)
	if len(got.items) != 1 || got.items[0].msg.MessageId != 2 {
		t.Fatalf("second flush = %+v; want the single second message", got.items)
	}
}

// TestBatcherIsolatesConversations is the privacy-critical test: interleaved
// messages from different senders/targets must never share a flush.
func TestBatcherIsolatesConversations(t *testing.T) {
	rec := newRecorder()
	b := testBatcher(50*time.Millisecond, time.Second, 10, rec)
	defer b.Shutdown(time.Second)

	aX := keyFor("A", "X")
	bX := keyFor("B", "X") // same recipient, different sender
	aY := keyFor("A", "Y") // same sender, different recipient

	// Interleave so that a naive single queue would merge all three.
	b.Add(aX, batchItem{msg: dummyMessage(1)})
	b.Add(bX, batchItem{msg: dummyMessage(2)})
	b.Add(aY, batchItem{msg: dummyMessage(3)})
	b.Add(aX, batchItem{msg: dummyMessage(4)})

	seen := map[convKey][]int64{}
	for i := 0; i < 3; i++ {
		got := rec.next(t, 2*time.Second)
		for _, it := range got.items {
			seen[got.key] = append(seen[got.key], it.msg.MessageId)
		}
	}
	want := map[convKey][]int64{
		aX: {1, 4},
		bX: {2},
		aY: {3},
	}
	if len(seen) != len(want) {
		t.Fatalf("flush keys = %d; want %d (each conversation its own batch)", len(seen), len(want))
	}
	for k, ids := range want {
		gotIDs := seen[k]
		if len(gotIDs) != len(ids) {
			t.Fatalf("conversation %+v got ids %v; want %v", k, gotIDs, ids)
		}
		for i := range ids {
			if gotIDs[i] != ids[i] {
				t.Errorf("conversation %+v ids = %v; want %v", k, gotIDs, ids)
			}
		}
	}
}

// TestBatcherMaxItems: hitting the item cap flushes at once.
func TestBatcherMaxItems(t *testing.T) {
	rec := newRecorder()
	b := testBatcher(10*time.Second, 10*time.Second, 2, rec)
	defer b.Shutdown(time.Second)
	key := keyFor("s", "t")

	b.Add(key, batchItem{msg: dummyMessage(1)})
	b.Add(key, batchItem{msg: dummyMessage(2)}) // cap of 2 -> immediate flush
	got := rec.next(t, time.Second)
	if len(got.items) != 2 {
		t.Fatalf("cap flush carried %d items; want 2", len(got.items))
	}
}

// TestBatcherMaxWait: a sender who keeps typing must still be delivered once the
// absolute cap passes, even though every message resets the debounce window.
func TestBatcherMaxWait(t *testing.T) {
	rec := newRecorder()
	b := testBatcher(80*time.Millisecond, 160*time.Millisecond, 100, rec)
	defer b.Shutdown(time.Second)
	key := keyFor("s", "t")

	start := time.Now()
	addDone := make(chan struct{})
	go func() {
		defer close(addDone)
		for i := 1; i <= 6; i++ {
			b.Add(key, batchItem{msg: dummyMessage(int64(i))})
			time.Sleep(40 * time.Millisecond) // reset the 80ms window every time
		}
	}()
	var got flushRecord
	select {
	case got = <-rec.ch:
	case <-time.After(2 * time.Second):
		t.Fatal("no flush within 2s despite the max-wait cap")
	}
	elapsed := time.Since(start)
	if elapsed > b.maxWait+300*time.Millisecond {
		t.Fatalf("flush took %s; must not exceed max-wait %s (plus slack)", elapsed, b.maxWait)
	}
	if len(got.items) == 0 {
		t.Fatal("flush carried no items")
	}
	<-addDone
}

// TestBatcherFlushKeyIsSynchronousAndOrdered: an immediate send (answer/album)
// flushes the conversation's buffered burst first, and later messages start a new
// batch.
func TestBatcherFlushKeyIsSynchronous(t *testing.T) {
	rec := newRecorder()
	b := testBatcher(10*time.Second, 10*time.Second, 10, rec)
	defer b.Shutdown(time.Second)
	key := keyFor("s", "t")

	b.Add(key, batchItem{msg: dummyMessage(1)})
	b.Add(key, batchItem{msg: dummyMessage(2)})
	b.FlushKey(key) // must have flushed by the time it returns
	select {
	case got := <-rec.ch:
		if len(got.items) != 2 {
			t.Fatalf("FlushKey flushed %d items; want 2", len(got.items))
		}
	default:
		t.Fatal("FlushKey returned without flushing")
	}

	b.Add(key, batchItem{msg: dummyMessage(3)})
	rec.expectNoFlush(t, 40*time.Millisecond) // window is 10s: it must still be buffered
	b.FlushKey(key)
	select {
	case got := <-rec.ch:
		if len(got.items) != 1 || got.items[0].msg.MessageId != 3 {
			t.Fatalf("post-flush batch = %v; want the single new message", got.items)
		}
	default:
		t.Fatal("second FlushKey did not flush the new batch")
	}
}

// TestBatcherShutdownFlushes: a batch pending at shutdown is delivered, not lost.
func TestBatcherShutdownFlushes(t *testing.T) {
	rec := newRecorder()
	b := testBatcher(10*time.Second, 10*time.Second, 10, rec)
	key := keyFor("s", "t")
	b.Add(key, batchItem{msg: dummyMessage(1)})
	b.Add(key, batchItem{msg: dummyMessage(2)})
	b.Shutdown(2 * time.Second)
	got := rec.next(t, time.Second)
	if len(got.items) != 2 {
		t.Fatalf("shutdown flush carried %d items; want 2", len(got.items))
	}
}

// TestBatcherDisabledFlushesImmediately: window=0 is the documented escape hatch
// (BATCH_WINDOW_MS=0) and must preserve the old immediate behaviour.
func TestBatcherDisabledFlushesImmediately(t *testing.T) {
	rec := newRecorder()
	b := testBatcher(0, 0, 10, rec)
	if b.Enabled() {
		t.Fatal("window 0 must disable buffering")
	}
	b.Add(keyFor("s", "t"), batchItem{msg: dummyMessage(1)})
	got := rec.next(t, time.Second)
	if len(got.items) != 1 {
		t.Fatalf("disabled batcher flushed %d items; want 1", len(got.items))
	}
}

// TestBatcherSweepCatchesLostTimers documents the insurance path: a batch whose
// timer never fires is still flushed by the periodic sweep once it is old enough.
func TestBatcherSweepCatchesLostTimers(t *testing.T) {
	rec := newRecorder()
	b := testBatcher(10*time.Millisecond, 20*time.Millisecond, 10, rec)
	// Replace the timer with one that never fires, simulating a lost timer.
	b.after = func(time.Duration, func()) *time.Timer {
		return time.NewTimer(time.Hour) // stopped by detachLocked; never invokes fn
	}
	defer b.Shutdown(time.Second)
	key := keyFor("s", "t")
	b.Add(key, batchItem{msg: dummyMessage(1)})
	b.Add(key, batchItem{msg: dummyMessage(2)})

	time.Sleep(b.maxWait + b.window + 50*time.Millisecond)
	b.Sweep()
	got := rec.next(t, time.Second)
	if len(got.items) != 2 {
		t.Fatalf("sweep flush carried %d items; want 2", len(got.items))
	}
}

// TestBatcherNoCrossConversationAfterDetach uses many conversations concurrently
// under -race: whatever the scheduling, every flushed item must belong to the
// key it was flushed under (nothing is read from shared mutable state).
func TestBatcherNoCrossConversationAfterDetach(t *testing.T) {
	rec := newRecorder()
	b := testBatcher(15*time.Millisecond, 200*time.Millisecond, 10, rec)
	defer b.Shutdown(2 * time.Second)

	const conversations = 20
	perConv := map[convKey][]int64{}
	var mu sync.Mutex
	for i := 0; i < conversations; i++ {
		key := keyFor(fmt.Sprintf("s%d", i), fmt.Sprintf("t%d", i))
		perConv[key] = nil
		b.Add(key, batchItem{msg: dummyMessage(int64(i * 10))})
		b.Add(key, batchItem{msg: dummyMessage(int64(i*10 + 1))})
	}

	deadline := time.After(3 * time.Second)
	batches := 0
	for batches < conversations {
		select {
		case got := <-rec.ch:
			batches++
			mu.Lock()
			// Every item flushed under a key must be one of that key's own messages:
			// the id encodes the conversation index (i*10 / i*10+1).
			owner := 0
			fmt.Sscanf(got.key.senderUID, "s%d", &owner)
			for _, it := range got.items {
				id := it.msg.MessageId
				if int(id) != owner*10 && int(id) != owner*10+1 {
					mu.Unlock()
					t.Fatalf("conversation %+v received foreign message id %d", got.key, id)
				}
			}
			mu.Unlock()
		case <-deadline:
			t.Fatalf("only %d/%d batches flushed", batches, conversations)
		}
	}
}
