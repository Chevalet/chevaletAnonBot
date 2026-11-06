package bot

import (
	"sync"
	"time"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// Per-conversation delivery queue.
//
// WHY: a person often sends one thought as several messages ("سلام" / "خوبی؟" /
// "یه سوال داشتم"), sometimes mixed with photos/an albums, sometimes as replies.
// Forwarding each one the instant it arrives turns that into a wall of separate
// notifications for the recipient, and a gallery used to be copied item-by-item
// (deleting and re-sending the previous copy on every arrival). This queue holds a
// conversation's outgoing items for a short debounce window and delivers them in
// ONE ordered pass:
//
//   - ordinary messages and replies are copied in order, each with its own reply
//     context and keyboard;
//   - an album (media group) is collected COMPLETELY and sent once with a single
//     CopyMessages, rather than being re-copied on every item;
//   - the sender gets one confirmation for the whole burst, with delete buttons
//     covering everything that was copied.
//
// ISOLATION: the queue key is (sender uid, target uid) — a queue can only ever hold
// one conversation's items. Flush detaches the units under the mutex before doing
// any I/O, so new items (of this or any other conversation) can never leak into a
// delivery that is already running.
//
// MEMORY: at most one pending queue per active conversation, capped in item count
// and age; timers are stopped on flush/shutdown and nothing references the buffered
// messages after delivery.

// convKey identifies one anonymous conversation. Both halves are required: the
// same recipient can be messaged by many senders, and one sender can talk to many
// recipients — merging either way would leak messages across conversations.
type convKey struct {
	senderUID string
	targetUID string
}

// batchItem is one buffered ordinary message plus the link it arrived through.
type batchItem struct {
	msg       *gotgbot.Message
	targetCid string
}

// batchReply is the reply context of a queued item.
//
//   - targetMid: a normal answer — the message the sender replied to, in the
//     target's chat (that is the id the original Python flow used);
//   - externalChat/externalMid: a reply to a message in an external channel/group;
//   - channel: the reply-to-channel flow (bot may need an inline link because it is
//     not an admin of that channel).
type batchReply struct {
	targetMid        string
	externalChat     int64
	externalMid      int64
	externalUsername string
	quoteText        string
	quotePos         int64
	channel          bool
}

// queueUnitKind distinguishes a single message from an album.
type queueUnitKind int

const (
	unitSingle queueUnitKind = iota
	unitAlbum
)

// queueUnit is one delivery unit: either one message or one complete album.
type queueUnit struct {
	kind      queueUnitKind
	albumID   string
	msgs      []*gotgbot.Message // single: exactly one; album: the group's items
	targetCid string
	reply     batchReply
}

// pendingQueue is one conversation's open buffer.
type pendingQueue struct {
	units   []queueUnit
	albumAt map[string]int // media_group_id -> index in units
	firstAt time.Time
	timer   *time.Timer
	// epoch guards against a stopped timer's callback firing late.
	epoch uint64
}

// queueFlushFn receives a detached queue. Called with the queue mutex released.
type queueFlushFn func(key convKey, units []queueUnit)

// batcher is the per-conversation delivery queue (kept under the historical name
// so the wiring in Bot stays small).
type batcher struct {
	mu       sync.Mutex
	m        map[convKey]*pendingQueue
	window   time.Duration // ordinary messages + replies
	albumWnd time.Duration // an album waits this long after its last item
	maxWait  time.Duration
	maxItems int
	flush    queueFlushFn
	now      func() time.Time
	after    func(time.Duration, func()) *time.Timer
	wg       sync.WaitGroup
	closed   bool
}

// newBatcher builds the queue. A zero window disables buffering for that kind: Add
// flushes immediately, so callers need no special case for the disabled mode.
func newBatcher(window, albumWindow, maxWait time.Duration, maxItems int, flush queueFlushFn) *batcher {
	if maxItems < 1 {
		maxItems = 1
	}
	if albumWindow <= 0 {
		albumWindow = window
	}
	if maxWait < window {
		maxWait = window
	}
	return &batcher{
		m:        make(map[convKey]*pendingQueue),
		window:   window,
		albumWnd: albumWindow,
		maxWait:  maxWait,
		maxItems: maxItems,
		flush:    flush,
		now:      time.Now,
		after:    time.AfterFunc,
	}
}

// Enabled reports whether messages are being buffered at all.
func (b *batcher) Enabled() bool { return b.window > 0 || b.albumWnd > 0 }

// Add queues one ordinary message (no reply context). Kept for callers/tests that
// have no reply.
func (b *batcher) Add(key convKey, it batchItem) {
	b.AddSingle(key, it, batchReply{})
}

// AddSingle queues one ordinary message or a reply. A zero/negative ordinary-message
// window delivers immediately (the BATCH_WINDOW_MS=0 escape hatch).
func (b *batcher) AddSingle(key convKey, it batchItem, reply batchReply) {
	u := queueUnit{kind: unitSingle, msgs: []*gotgbot.Message{it.msg}, targetCid: it.targetCid, reply: reply}
	if b.window <= 0 {
		b.runFlush(key, []queueUnit{u})
		return
	}
	b.addUnit(key, u, b.window)
}

// AddAlbumItem queues one item of a media group. Items with the same media_group_id
// join the same album unit until the queue is flushed, so the gallery is copied
// exactly once, in one CopyMessages call.
func (b *batcher) AddAlbumItem(key convKey, albumID string, msg *gotgbot.Message, targetCid string, reply batchReply) {
	if b.albumWnd <= 0 {
		// No album window: galleries are not buffered (each item delivered on its own).
		b.runFlush(key, []queueUnit{{
			kind: unitAlbum, albumID: albumID, msgs: []*gotgbot.Message{msg},
			targetCid: targetCid, reply: reply,
		}})
		return
	}
	b.mu.Lock()
	if !b.Enabled() || b.closed {
		b.mu.Unlock()
		b.runFlush(key, []queueUnit{{
			kind: unitAlbum, albumID: albumID, msgs: []*gotgbot.Message{msg},
			targetCid: targetCid, reply: reply,
		}})
		return
	}
	p := b.m[key]
	if p == nil {
		p = &pendingQueue{firstAt: b.now(), albumAt: make(map[string]int)}
		b.m[key] = p
	}
	if p.albumAt == nil {
		p.albumAt = make(map[string]int)
	}
	if idx, ok := p.albumAt[albumID]; ok && idx < len(p.units) {
		// Same gallery: append. Avoid buffering a redelivered duplicate item.
		dup := false
		for _, existing := range p.units[idx].msgs {
			if existing.MessageId == msg.MessageId {
				dup = true
				break
			}
		}
		if !dup {
			p.units[idx].msgs = append(p.units[idx].msgs, msg)
		}
		if p.units[idx].targetCid == "" {
			p.units[idx].targetCid = targetCid
		}
	} else {
		p.albumAt[albumID] = len(p.units)
		p.units = append(p.units, queueUnit{
			kind: unitAlbum, albumID: albumID, msgs: []*gotgbot.Message{msg},
			targetCid: targetCid, reply: reply,
		})
	}
	if b.overCapLocked(p) {
		units := b.detachLocked(key)
		b.mu.Unlock()
		b.runFlush(key, units)
		return
	}
	b.armLocked(key, p, b.albumWnd)
	b.mu.Unlock()
}

// addUnit appends a single-message unit.
func (b *batcher) addUnit(key convKey, u queueUnit, window time.Duration) {
	if !b.Enabled() {
		b.runFlush(key, []queueUnit{u})
		return
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		b.runFlush(key, []queueUnit{u})
		return
	}
	p := b.m[key]
	if p == nil {
		p = &pendingQueue{firstAt: b.now(), albumAt: make(map[string]int)}
		b.m[key] = p
	}
	p.units = append(p.units, u)
	if b.overCapLocked(p) {
		units := b.detachLocked(key)
		b.mu.Unlock()
		b.runFlush(key, units)
		return
	}
	b.armLocked(key, p, window)
	b.mu.Unlock()
}

// overCapLocked reports whether the queue must flush at once (item cap or the
// absolute max-wait has passed). Caller holds b.mu.
func (b *batcher) overCapLocked(p *pendingQueue) bool {
	if b.now().Sub(p.firstAt) >= b.maxWait {
		return true
	}
	n := 0
	for _, u := range p.units {
		n += len(u.msgs)
	}
	return n >= b.maxItems
}

// armLocked (re)schedules the flush timer. Caller holds b.mu.
func (b *batcher) armLocked(key convKey, p *pendingQueue, window time.Duration) {
	if p.timer != nil {
		p.timer.Stop()
	}
	p.epoch++
	epoch := p.epoch
	remaining := window
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
			return
		}
		units := b.detachLocked(key)
		b.mu.Unlock()
		b.runFlush(key, units)
	})
}

// detachLocked removes key's queue from the map and returns its units. Caller holds
// b.mu; the caller owns the units afterwards.
func (b *batcher) detachLocked(key convKey) []queueUnit {
	p := b.m[key]
	if p == nil {
		return nil
	}
	if p.timer != nil {
		p.timer.Stop()
	}
	delete(b.m, key)
	return p.units
}

// FlushKey delivers key's pending queue immediately and synchronously.
func (b *batcher) FlushKey(key convKey) {
	b.mu.Lock()
	units := b.detachLocked(key)
	b.mu.Unlock()
	if len(units) > 0 {
		b.runFlush(key, units)
	}
}

// runFlush invokes the flush callback, tracked by the shutdown waitgroup.
func (b *batcher) runFlush(key convKey, units []queueUnit) {
	if len(units) == 0 {
		return
	}
	b.wg.Add(1)
	defer b.wg.Done()
	b.flush(key, units)
}

// Pending reports how many conversation queues are buffered.
func (b *batcher) Pending() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.m)
}

// Sweep flushes queues whose timer can no longer be trusted (insurance against a
// lost timer). Called every few seconds by a background loop.
func (b *batcher) Sweep() {
	b.mu.Lock()
	cutoff := b.now().Add(-(b.maxWait + b.albumWnd + b.window))
	type detached struct {
		key   convKey
		units []queueUnit
	}
	var out []detached
	for key, p := range b.m {
		if p.firstAt.Before(cutoff) {
			out = append(out, detached{key: key, units: b.detachLocked(key)})
		}
	}
	b.mu.Unlock()
	for _, d := range out {
		b.runFlush(d.key, d.units)
	}
}

// Shutdown flushes every pending queue and waits (bounded) for in-flight flushes.
// Called from Run after polling has stopped, so a message being buffered when the
// process got SIGTERM is still delivered.
func (b *batcher) Shutdown(timeout time.Duration) {
	b.mu.Lock()
	b.closed = true
	type detached struct {
		key   convKey
		units []queueUnit
	}
	out := make([]detached, 0, len(b.m))
	for key := range b.m {
		out = append(out, detached{key: key, units: b.detachLocked(key)})
	}
	b.mu.Unlock()
	for _, d := range out {
		b.runFlush(d.key, d.units)
	}
	done := make(chan struct{})
	go func() { b.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}
