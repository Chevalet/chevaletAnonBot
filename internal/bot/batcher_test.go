package bot

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// Tests for the per-conversation delivery queue in isolation (no DB, no Telegram):
// grouping (messages + replies + albums), caps, per-conversation isolation,
// ordering, reply context, shutdown flush and the disabled modes. Delivery behaviour
// is covered end-to-end in batch_box_integration_test.go.

type flushRecord struct {
	key   convKey
	units []queueUnit
}

type recorder struct {
	ch chan flushRecord
}

func newRecorder() *recorder { return &recorder{ch: make(chan flushRecord, 64)} }

func (r *recorder) flush(key convKey, units []queueUnit) {
	r.ch <- flushRecord{key: key, units: units}
}

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

func (r *recorder) expectNoFlush(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case rec := <-r.ch:
		t.Fatalf("unexpected flush: key=%+v units=%d", rec.key, len(rec.units))
	case <-time.After(d):
	}
}

func dummyMessage(id int64) *gotgbot.Message { return &gotgbot.Message{MessageId: id} }

func keyFor(a, b string) convKey { return convKey{senderUID: a, targetUID: b} }

func totalMsgs(units []queueUnit) int {
	n := 0
	for _, u := range units {
		n += len(u.msgs)
	}
	return n
}

func testBatcher(window, albumWindow time.Duration, rec *recorder) *batcher {
	return newBatcher(window, albumWindow, time.Second, 100, rec.flush)
}

// TestQueueGroupsBurst: several rapid messages to one conversation arrive as ONE
// flush holding all of them, in arrival order.
func TestQueueGroupsBurst(t *testing.T) {
	rec := newRecorder()
	q := testBatcher(60*time.Millisecond, 25*time.Millisecond, rec)
	defer q.Shutdown(time.Second)
	key := keyFor("sender", "target")

	for i := 1; i <= 3; i++ {
		q.AddSingle(key, batchItem{msg: dummyMessage(int64(i))}, batchReply{})
	}
	got := rec.next(t, 2*time.Second)
	if got.key != key {
		t.Fatalf("flush key = %+v; want %+v", got.key, key)
	}
	if len(got.units) != 3 {
		t.Fatalf("flush carried %d units; want 3 singles", len(got.units))
	}
	for i, u := range got.units {
		if u.kind != unitSingle || u.msgs[0].MessageId != int64(i+1) {
			t.Errorf("unit %d = %+v; want single message %d (arrival order)", i, u, i+1)
		}
	}
}

// TestQueueSeparatesPauses: a pause longer than the window ends the burst.
func TestQueueSeparatesPauses(t *testing.T) {
	rec := newRecorder()
	q := testBatcher(40*time.Millisecond, 20*time.Millisecond, rec)
	defer q.Shutdown(time.Second)
	key := keyFor("s", "t")

	q.AddSingle(key, batchItem{msg: dummyMessage(1)}, batchReply{})
	if got := rec.next(t, 2*time.Second); totalMsgs(got.units) != 1 {
		t.Fatalf("first flush carried %d messages; want 1", totalMsgs(got.units))
	}
	q.AddSingle(key, batchItem{msg: dummyMessage(2)}, batchReply{})
	got := rec.next(t, 2*time.Second)
	if totalMsgs(got.units) != 1 || got.units[0].msgs[0].MessageId != 2 {
		t.Fatalf("second flush = %+v; want the single second message", got.units)
	}
}

// TestQueueIsolatesConversations is the privacy-critical test.
func TestQueueIsolatesConversations(t *testing.T) {
	rec := newRecorder()
	q := testBatcher(50*time.Millisecond, 20*time.Millisecond, rec)
	defer q.Shutdown(time.Second)

	aX, bX, aY := keyFor("A", "X"), keyFor("B", "X"), keyFor("A", "Y")
	q.AddSingle(aX, batchItem{msg: dummyMessage(1)}, batchReply{})
	q.AddSingle(bX, batchItem{msg: dummyMessage(2)}, batchReply{})
	q.AddSingle(aY, batchItem{msg: dummyMessage(3)}, batchReply{})
	q.AddSingle(aX, batchItem{msg: dummyMessage(4)}, batchReply{})

	seen := map[convKey][]int64{}
	for i := 0; i < 3; i++ {
		got := rec.next(t, 2*time.Second)
		for _, u := range got.units {
			seen[got.key] = append(seen[got.key], u.msgs[0].MessageId)
		}
	}
	want := map[convKey][]int64{aX: {1, 4}, bX: {2}, aY: {3}}
	if len(seen) != len(want) {
		t.Fatalf("flush keys = %d; want %d (each conversation its own queue)", len(seen), len(want))
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

// TestQueueGroupsAlbum: the items of one media group become ONE unit (in order,
// deduped), keeping their position relative to surrounding messages.
func TestQueueGroupsAlbum(t *testing.T) {
	rec := newRecorder()
	q := testBatcher(10*time.Second, 10*time.Second, rec)
	key := keyFor("s", "t")

	q.AddSingle(key, batchItem{msg: dummyMessage(1)}, batchReply{})
	q.AddAlbumItem(key, "album-a", dummyMessage(50), "", batchReply{})
	q.AddAlbumItem(key, "album-a", dummyMessage(52), "", batchReply{})
	q.AddAlbumItem(key, "album-a", dummyMessage(51), "", batchReply{})
	q.AddAlbumItem(key, "album-a", dummyMessage(52), "", batchReply{}) // redelivered duplicate
	q.AddSingle(key, batchItem{msg: dummyMessage(2)}, batchReply{})
	q.FlushKey(key)

	got := rec.next(t, 2*time.Second)
	if len(got.units) != 3 {
		t.Fatalf("units = %d (%+v); want single, album, single", len(got.units), got.units)
	}
	if got.units[0].kind != unitSingle || got.units[0].msgs[0].MessageId != 1 {
		t.Errorf("unit 0 = %+v; want the first single", got.units[0])
	}
	album := got.units[1]
	if album.kind != unitAlbum || len(album.msgs) != 3 {
		t.Fatalf("album unit = %+v; want 3 deduped items", album)
	}
	if got.units[2].kind != unitSingle || got.units[2].msgs[0].MessageId != 2 {
		t.Errorf("unit 2 = %+v; want the trailing single", got.units[2])
	}
}

// TestQueueRetainsReplyContext: a batched reply must keep the message it replies to.
func TestQueueRetainsReplyContext(t *testing.T) {
	rec := newRecorder()
	q := testBatcher(10*time.Second, 10*time.Second, rec)
	key := keyFor("s", "t")

	q.AddSingle(key, batchItem{msg: dummyMessage(5), targetCid: "cid-1"}, batchReply{
		targetMid: "9001", quoteText: "quoted", quotePos: 4,
	})
	q.FlushKey(key)
	got := rec.next(t, 2*time.Second)
	if len(got.units) != 1 {
		t.Fatalf("units = %d; want 1", len(got.units))
	}
	r := got.units[0].reply
	if r.targetMid != "9001" || r.quoteText != "quoted" || r.quotePos != 4 {
		t.Fatalf("reply context = %+v; want it preserved", r)
	}
	if got.units[0].targetCid != "cid-1" {
		t.Errorf("targetCid = %q; want cid-1", got.units[0].targetCid)
	}
}

// TestQueueMaxItems: hitting the message cap flushes at once (album items count too).
func TestQueueMaxItems(t *testing.T) {
	rec := newRecorder()
	q := newBatcher(10*time.Second, 10*time.Second, 10*time.Second, 2, rec.flush)
	defer q.Shutdown(time.Second)
	key := keyFor("s", "t")
	q.AddSingle(key, batchItem{msg: dummyMessage(1)}, batchReply{})
	q.AddSingle(key, batchItem{msg: dummyMessage(2)}, batchReply{}) // cap of 2 -> flush
	got := rec.next(t, time.Second)
	if totalMsgs(got.units) != 2 {
		t.Fatalf("cap flush carried %d messages; want 2", totalMsgs(got.units))
	}
}

// TestQueueMaxWait: a sender who keeps typing is still delivered once the absolute
// cap passes.
func TestQueueMaxWait(t *testing.T) {
	rec := newRecorder()
	q := newBatcher(80*time.Millisecond, 80*time.Millisecond, 160*time.Millisecond, 100, rec.flush)
	defer q.Shutdown(time.Second)
	key := keyFor("s", "t")

	start := time.Now()
	addDone := make(chan struct{})
	go func() {
		defer close(addDone)
		for i := 1; i <= 6; i++ {
			q.AddSingle(key, batchItem{msg: dummyMessage(int64(i))}, batchReply{})
			time.Sleep(40 * time.Millisecond)
		}
	}()
	got := rec.next(t, 2*time.Second)
	if elapsed := time.Since(start); elapsed > 160*time.Millisecond+300*time.Millisecond {
		t.Fatalf("flush took %s; must not exceed max-wait 160ms (plus slack)", elapsed)
	}
	if totalMsgs(got.units) == 0 {
		t.Fatal("flush carried no messages")
	}
	<-addDone
}

// TestQueueFlushKeyIsSynchronous: an immediate send flushes the queue first and a
// later item starts a new one.
func TestQueueFlushKeyIsSynchronous(t *testing.T) {
	rec := newRecorder()
	q := testBatcher(10*time.Second, 10*time.Second, rec)
	defer q.Shutdown(time.Second)
	key := keyFor("s", "t")

	q.AddSingle(key, batchItem{msg: dummyMessage(1)}, batchReply{})
	q.AddSingle(key, batchItem{msg: dummyMessage(2)}, batchReply{})
	q.FlushKey(key)
	select {
	case got := <-rec.ch:
		if totalMsgs(got.units) != 2 {
			t.Fatalf("FlushKey flushed %d messages; want 2", totalMsgs(got.units))
		}
	default:
		t.Fatal("FlushKey returned without flushing")
	}

	q.AddSingle(key, batchItem{msg: dummyMessage(3)}, batchReply{})
	rec.expectNoFlush(t, 40*time.Millisecond)
	q.FlushKey(key)
	if got := <-rec.ch; totalMsgs(got.units) != 1 || got.units[0].msgs[0].MessageId != 3 {
		t.Fatalf("post-flush queue = %+v; want the single new message", got.units)
	}
}

// TestQueueShutdownFlushes: a queue pending at shutdown is delivered, not lost.
func TestQueueShutdownFlushes(t *testing.T) {
	rec := newRecorder()
	q := testBatcher(10*time.Second, 10*time.Second, rec)
	key := keyFor("s", "t")
	q.AddSingle(key, batchItem{msg: dummyMessage(1)}, batchReply{})
	q.AddSingle(key, batchItem{msg: dummyMessage(2)}, batchReply{})
	q.Shutdown(2 * time.Second)
	got := rec.next(t, time.Second)
	if totalMsgs(got.units) != 2 {
		t.Fatalf("shutdown flush carried %d messages; want 2", totalMsgs(got.units))
	}
}

// TestQueueDisabledWindowsFlushImmediately: BATCH_WINDOW_MS=0 / ALBUM_WINDOW_MS=0
// are the documented escape hatches and must preserve immediate delivery.
func TestQueueDisabledWindowsFlushImmediately(t *testing.T) {
	rec := newRecorder()
	q := newBatcher(0, 0, time.Second, 10, rec.flush)
	q.AddSingle(keyFor("s", "t"), batchItem{msg: dummyMessage(1)}, batchReply{})
	if got := rec.next(t, time.Second); totalMsgs(got.units) != 1 {
		t.Fatalf("disabled singles flushed %d; want 1", totalMsgs(got.units))
	}
	q.AddAlbumItem(keyFor("s", "t"), "a", dummyMessage(2), "", batchReply{})
	if got := rec.next(t, time.Second); len(got.units) != 1 {
		t.Fatalf("disabled albums flushed %d units; want 1", len(got.units))
	}
}

// TestQueueSweepCatchesLostTimers: a queue whose timer never fires is still flushed.
func TestQueueSweepCatchesLostTimers(t *testing.T) {
	rec := newRecorder()
	q := testBatcher(10*time.Millisecond, 10*time.Millisecond, rec)
	q.after = func(time.Duration, func()) *time.Timer { return time.NewTimer(time.Hour) }
	defer q.Shutdown(time.Second)
	key := keyFor("s", "t")
	q.AddSingle(key, batchItem{msg: dummyMessage(1)}, batchReply{})
	q.AddSingle(key, batchItem{msg: dummyMessage(2)}, batchReply{})

	time.Sleep(q.maxWait + q.window + q.albumWnd + 50*time.Millisecond)
	q.Sweep()
	got := rec.next(t, time.Second)
	if totalMsgs(got.units) != 2 {
		t.Fatalf("sweep flush carried %d; want 2", totalMsgs(got.units))
	}
}

// TestQueueNoCrossConversationAfterDetach uses many conversations concurrently under
// -race: every flushed unit must belong to the key it was flushed under.
func TestQueueNoCrossConversationAfterDetach(t *testing.T) {
	rec := newRecorder()
	q := testBatcher(15*time.Millisecond, 15*time.Millisecond, rec)
	defer q.Shutdown(2 * time.Second)

	const conversations = 20
	var mu sync.Mutex
	seen := make(map[convKey]bool)
	for i := 0; i < conversations; i++ {
		key := keyFor(fmt.Sprintf("s%d", i), fmt.Sprintf("t%d", i))
		q.AddSingle(key, batchItem{msg: dummyMessage(int64(i * 10))}, batchReply{})
		q.AddSingle(key, batchItem{msg: dummyMessage(int64(i*10 + 1))}, batchReply{})
	}

	deadline := time.After(3 * time.Second)
	batches := 0
	for batches < conversations {
		select {
		case got := <-rec.ch:
			batches++
			mu.Lock()
			owner := 0
			fmt.Sscanf(got.key.senderUID, "s%d", &owner)
			for _, u := range got.units {
				for _, m := range u.msgs {
					id := int(m.MessageId)
					if id != owner*10 && id != owner*10+1 {
						mu.Unlock()
						t.Fatalf("conversation %+v received foreign message id %d", got.key, id)
					}
				}
			}
			seen[got.key] = true
			mu.Unlock()
		case <-deadline:
			t.Fatalf("only %d/%d batches flushed", batches, conversations)
		}
	}
	if len(seen) != conversations {
		t.Fatalf("distinct conversations flushed = %d; want %d", len(seen), conversations)
	}
}
