package bot

import (
	"sync"
	"time"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// Batch buffering for ordinary anonymous messages.
//
// WHY: a person often sends one thought as several messages ("سلام" / "خوبی؟" /
// "یه سوال داشتم"). Forwarding each one the instant it arrives turns that into a
// wall of separate notifications for the recipient. This buffer holds those
// messages for a short debounce window and hands the whole burst to the delivery
// path at once, so the recipient gets one ordered batch (still one Telegram
// message per item — Telegram cannot merge arbitrary types into one message — but
// one logical delivery, one sender-side confirmation).
//
// WHAT IS NOT BATCHED, deliberately:
//   - answers (they are tied to a message id and must not lose their context),
//   - album items (media groups already have their own delivery path; see
//     media.go — "a gallery stays a gallery"),
//   - replies to channel posts, and anything already in the message-box path.
// When one of those arrives for a conversation that has a pending batch, the
// batch is FLUSHED FIRST (FlushKey) so a message can never be delivered before an
// older message of the same conversation.
//
// ISOLATION: the buffer key is (sender uid, target uid) — a batch can only ever
// contain messages of that one conversation. Flush detaches the slice under the
// mutex before doing any I/O, so a slow Telegram call can never let new messages
// of another conversation into a batch being delivered.
//
// MEMORY: the map holds at most one pending batch per active conversation, each
// capped in both item count (BatchMaxMessages) and age (BatchMaxWait), and both
// the timer and Shutdown stop/remove it. A message is never referenced after its
// batch has been flushed.

// convKey identifies one anonymous conversation. Both halves are required: the
// same recipient can be messaged by many senders, and one sender can talk to many
// recipients — merging either way would leak messages across conversations.
type convKey struct {
	senderUID string
	targetUID string
}

// batchItem is one buffered message. Only the message and the link it came in
// through are kept; everything derived (tokens, keyboards, tags, seen status) is
// computed at flush time, exactly as the immediate path does.
type batchItem struct {
	msg       *gotgbot.Message
	targetCid string
}

// pendingBatch is one conversation's open buffer.
type pendingBatch struct {
	items   []batchItem
	firstAt time.Time
	timer   *time.Timer
	// epoch is bumped on every timer (re)arm so a callback from a stopped timer
	// that happens to fire anyway can tell it is stale and do nothing.
	epoch uint64
}

// batchFlushFn receives a detached batch. It is called with the batcher's mutex
// released; it must do its own error handling (the batcher never re-queues).
type batchFlushFn func(key convKey, items []batchItem)

// batcher buffers immediate-send candidates and flushes them after a debounce.
type batcher struct {
	mu       sync.Mutex
	m        map[convKey]*pendingBatch
	window   time.Duration
	maxWait  time.Duration
	maxItems int
	flush    batchFlushFn

	// design seams for deterministic tests
	now   func() time.Time
	after func(time.Duration, func()) *time.Timer

	// wg tracks in-flight and pending flush executions so Shutdown can wait for
	// them; closed stops new timers and makes adds flush immediately.
	wg     sync.WaitGroup
	closed bool
}

// newBatcher builds a batcher. A zero/negative window disables buffering: Add
// flushes immediately, so callers need no special case for the disabled mode.
func newBatcher(window, maxWait time.Duration, maxItems int, flush batchFlushFn) *batcher {
	if maxItems < 1 {
		maxItems = 1
	}
	if maxWait < window {
		maxWait = window
	}
	return &batcher{
		m:        make(map[convKey]*pendingBatch),
		window:   window,
		maxWait:  maxWait,
		maxItems: maxItems,
		flush:    flush,
		now:      time.Now,
		after:    time.AfterFunc,
	}
}

// Enabled reports whether messages are being buffered at all.
func (b *batcher) Enabled() bool { return b.window > 0 }

// Add appends it to key's batch, (re)arming the debounce timer, and flushes at
// once when the batch hits a cap. Returns after flush, so the caller can be sure
// when a synchronous delivery has happened (tests use this too).
func (b *batcher) Add(key convKey, it batchItem) {
	if !b.Enabled() {
		b.runFlush(key, []batchItem{it})
		return
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		b.runFlush(key, []batchItem{it})
		return
	}
	p := b.m[key]
	if p == nil {
		p = &pendingBatch{firstAt: b.now()}
		b.m[key] = p
	}
	p.items = append(p.items, it)

	// Absolute cap: a sender who keeps typing must still be delivered to.
	if len(p.items) >= b.maxItems || b.now().Sub(p.firstAt) >= b.maxWait {
		items := b.detachLocked(key)
		b.mu.Unlock()
		b.runFlush(key, items)
		return
	}
	b.armLocked(key, p)
	b.mu.Unlock()
}

// armLocked (re)schedules the flush timer for p. Caller holds b.mu.
func (b *batcher) armLocked(key convKey, p *pendingBatch) {
	if p.timer != nil {
		p.timer.Stop()
	}
	p.epoch++
	epoch := p.epoch
	remaining := b.window
	if left := b.maxWait - b.now().Sub(p.firstAt); left < remaining {
		remaining = left
	}
	if remaining < 0 {
		remaining = 0
	}
	p.timer = b.after(remaining, func() {
		b.mu.Lock()
		cur := b.m[key]
		if cur != p || cur.epoch != epoch {
			b.mu.Unlock()
			return // superseded by a newer timer, or the batch was already flushed
		}
		items := b.detachLocked(key)
		b.mu.Unlock()
		b.runFlush(key, items)
	})
}

// detachLocked removes key's batch from the map and returns its items. The caller
// owns the items from here on; nothing else can append to them. Caller holds b.mu.
func (b *batcher) detachLocked(key convKey) []batchItem {
	p := b.m[key]
	if p == nil {
		return nil
	}
	if p.timer != nil {
		p.timer.Stop()
	}
	delete(b.m, key)
	return p.items
}

// FlushKey delivers key's pending batch immediately (if any), synchronously. The
// immediate paths (answer, album, channel reply, message-box store) call this
// first so ordering within one conversation is always preserved.
func (b *batcher) FlushKey(key convKey) {
	b.mu.Lock()
	items := b.detachLocked(key)
	b.mu.Unlock()
	if len(items) > 0 {
		b.runFlush(key, items)
	}
}

// runFlush invokes the flush callback, tracked by the shutdown waitgroup.
func (b *batcher) runFlush(key convKey, items []batchItem) {
	if len(items) == 0 {
		return
	}
	b.wg.Add(1)
	defer b.wg.Done()
	b.flush(key, items)
}

// Pending reports how many batches (not items) are buffered — for tests and a
// periodic self-check; not used on the hot path.
func (b *batcher) Pending() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.m)
}

// Sweep flushes batches whose timer can no longer be trusted (it fires only if a
// timer was lost somehow). Cheap insurance against a stuck batch; called every few
// seconds by a background loop.
func (b *batcher) Sweep() {
	b.mu.Lock()
	cutoff := b.now().Add(-(b.maxWait + b.window))
	var due []convKey
	for key, p := range b.m {
		if p.firstAt.Before(cutoff) {
			due = append(due, key)
		}
	}
	type detached struct {
		key   convKey
		items []batchItem
	}
	var out []detached
	for _, key := range due {
		out = append(out, detached{key: key, items: b.detachLocked(key)})
	}
	b.mu.Unlock()
	for _, d := range out {
		b.runFlush(d.key, d.items)
	}
}

// Shutdown flushes every pending batch and waits (bounded) for in-flight flushes.
// Called from Run after polling has stopped and before the DB pool closes, so a
// message that was being buffered when the process got SIGTERM is still delivered.
func (b *batcher) Shutdown(timeout time.Duration) {
	b.mu.Lock()
	b.closed = true
	type detached struct {
		key   convKey
		items []batchItem
	}
	out := make([]detached, 0, len(b.m))
	for key := range b.m {
		out = append(out, detached{key: key, items: b.detachLocked(key)})
	}
	b.mu.Unlock()

	for _, d := range out {
		b.runFlush(d.key, d.items)
	}

	done := make(chan struct{})
	go func() { b.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}
