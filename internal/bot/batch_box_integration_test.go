package bot

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// End-to-end tests for the two new features, driven through the REAL dispatcher
// with the fake Bot API and a real PostgreSQL — the same approach as
// anchor_integration_test.go, because routing/ordering is exactly where batching
// and the message box can go wrong (and exactly what unit tests cannot see).
//
// Skipped unless DB_HOST and BOT_TEST_DB are set (see anchor_integration_test.go
// for the docker command).

// enableBatching installs a short window for ordinary messages AND albums (tests
// must not sleep for the production 800ms/600ms) and shuts the queue down at the end.
func (h *harness) enableBatching(window, maxWait time.Duration, maxItems int) {
	h.t.Helper()
	h.enableQueue(window, window, maxWait, maxItems)
}

// enableQueue configures the conversation delivery queue explicitly.
func (h *harness) enableQueue(window, albumWindow, maxWait time.Duration, maxItems int) {
	h.t.Helper()
	old := h.b.batcher
	if old != nil {
		old.Shutdown(time.Second)
	}
	h.b.Cfg.BatchWindow = window
	h.b.Cfg.AlbumWindow = albumWindow
	h.b.batcher = newBatcher(window, albumWindow, maxWait, maxItems, h.b.deliverUnits)
	h.t.Cleanup(func() { h.b.batcher.Shutdown(2 * time.Second) })
}

// startUser creates the user row (new_user keeps cids/chevaletids unique) by
// driving /start through the dispatcher, like a real first contact.
func (h *harness) startUser(uid int64) {
	h.t.Helper()
	h.send(uid, "/start", nil)
}

// setMessageBox turns the recipient's Message Box setting on/off in the database.
func (h *harness) setMessageBox(uid int64, on bool) {
	h.t.Helper()
	if err := h.b.DB.SetMessageBox(context.Background(), strconv.FormatInt(uid, 10), on); err != nil {
		h.t.Fatalf("SetMessageBox(%d, %v): %v", uid, on, err)
	}
}

// pendingBox returns the recipient's pending message-box rows.
func (h *harness) pendingBox(uid int64) int {
	h.t.Helper()
	n, err := h.b.DB.PendingBoxCount(context.Background(), strconv.FormatInt(uid, 10))
	if err != nil {
		h.t.Fatalf("PendingBoxCount: %v", err)
	}
	return n
}

// waitFor polls cond until it returns true or the timeout expires.
func (h *harness) waitFor(timeout time.Duration, what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %s", what)
}

// callsTo returns the recorded calls of method addressed to chatID since mark.
func (h *harness) callsTo(mark int, method string, chatID int64) []apiCall {
	want := strconv.FormatInt(chatID, 10)
	var out []apiCall
	for _, c := range h.tg.since(mark) {
		if c.method == method && c.params["chat_id"] == want {
			out = append(out, c)
		}
	}
	return out
}

// messagesWithText returns sendMessage calls addressed to chatID whose text
// contains sub.
func (h *harness) messagesWithText(mark int, chatID int64, sub string) []apiCall {
	var out []apiCall
	for _, c := range h.callsTo(mark, "sendMessage", chatID) {
		if strings.Contains(c.params["text"], sub) {
			out = append(out, c)
		}
	}
	return out
}

// boxNotification builds the synthetic "you have new messages" message from the
// recorded Bot API call, so a test can press its button exactly like a user.
func (h *harness) boxNotification(uid int64) *gotgbot.Message {
	h.t.Helper()
	var latest *apiCall
	for _, c := range h.callsTo(0, "sendMessage", uid) {
		if strings.Contains(c.params["text"], "پیام جدید") {
			cc := c
			latest = &cc
		}
	}
	if latest == nil {
		h.t.Fatalf("no message-box notification was sent to %d", uid)
	}
	var kb gotgbot.InlineKeyboardMarkup
	if err := json.Unmarshal([]byte(latest.params["reply_markup"]), &kb); err != nil {
		h.t.Fatalf("decode notification keyboard: %v", err)
	}
	return &gotgbot.Message{
		MessageId:   h.mid.Add(1),
		From:        &gotgbot.User{Id: fakeBotID, IsBot: true, Username: "chevalet_test_bot"},
		Chat:        gotgbot.Chat{Id: uid, Type: "private"},
		Text:        latest.params["text"],
		ReplyMarkup: &kb,
	}
}

// --- batching -----------------------------------------------------------------

// TestBatchDeliversBurstTogether: several rapid messages to one recipient arrive as
// one ordered delivery pass with ONE sender confirmation (with delete buttons that
// cover the whole burst), not one notification per message.
func TestBatchDeliversBurstTogether(t *testing.T) {
	h := newHarness(t)
	h.enableBatching(50*time.Millisecond, 500*time.Millisecond, 10)

	recipient := newUID()
	sender := newUID()
	h.startUser(recipient)
	prompt := h.connect(sender, h.linkOf(recipient))

	mark := h.tg.mark()
	h.send(sender, "سلام", prompt)
	h.send(sender, "خوبی؟", prompt)
	h.send(sender, "یه سوال داشتم", prompt)

	h.waitFor(3*time.Second, "the three copies to the recipient", func() bool {
		return len(h.callsTo(mark, "copyMessage", recipient)) == 3
	})

	copies := h.callsTo(mark, "copyMessage", recipient)
	var prev int64
	for i, c := range copies {
		id, _ := strconv.ParseInt(c.params["message_id"], 10, 64)
		if i > 0 && id <= prev {
			t.Errorf("copies are out of order: message_id %d after %d", id, prev)
		}
		prev = id
	}
	if got := h.callsTo(mark, "copyMessage", sender); len(got) != 0 {
		t.Errorf("sender's own chat received %d copies; want 0", len(got))
	}

	// Exactly one "sent" confirmation for the whole burst, carrying delete buttons.
	// (It is sent after the copies, so wait for it rather than racing the flusher.)
	h.waitFor(2*time.Second, "the sender confirmation", func() bool {
		return len(h.messagesWithText(mark, sender, txtSentToThem)) == 1
	})
	confirmations := h.messagesWithText(mark, sender, txtSentToThem)
	if !strings.Contains(confirmations[0].params["reply_markup"], `"delete|`) {
		t.Errorf("batch confirmation has no delete button: %q", confirmations[0].params["reply_markup"])
	}
}

// TestBatchDoesNotMergeAcrossConversations is the privacy-critical integration
// test: interleaved bursts from two senders to the same recipient (and one sender
// talking to two recipients) must never be delivered as someone else's batch.
func TestBatchDoesNotMergeAcrossConversations(t *testing.T) {
	h := newHarness(t)
	h.enableBatching(60*time.Millisecond, 600*time.Millisecond, 10)

	alice, bob, carol := newUID(), newUID(), newUID()
	h.startUser(bob)
	h.startUser(carol)

	// The conversation state only ever points at ONE target at a time, so the first
	// message after each /start uses that state and later ones go through the connect
	// anchor (the prompt's cancel button carries the cid) — exactly like a real user
	// switching between conversations.
	bobPrompt := h.connect(alice, h.linkOf(bob))
	h.send(alice, "seed bob", bobPrompt) // state: alice -> bob (this ends the conversation)
	carolPrompt := h.connect(alice, h.linkOf(carol))
	h.send(alice, "seed carol", carolPrompt) // state: alice -> carol
	bobAnchor := h.connect(alice, h.linkOf(bob))
	h.send(alice, "seed bob 2", bobAnchor)
	carolAnchor := h.connect(alice, h.linkOf(carol))
	h.send(alice, "seed carol 2", carolAnchor)

	// Let the seed messages flush before marking, so only the burst below is counted.
	// (Wait for the copies instead of sleeping: the delivery is paced by the outbound
	// rate limiter, so a fixed sleep is racy on a loaded machine.)
	h.waitFor(5*time.Second, "the seed batches to finish delivering", func() bool {
		return len(h.callsTo(0, "copyMessage", bob)) >= 2 && len(h.callsTo(0, "copyMessage", carol)) >= 2
	})

	// Now interleave two bursts for DIFFERENT conversations inside one batch window.
	mark := h.tg.mark()
	h.send(alice, "to bob 1", bobAnchor)     // anchor: alice -> bob
	h.send(alice, "to carol 1", carolAnchor) // anchor: alice -> carol
	h.send(alice, "to bob 2", bobAnchor)     // anchor: alice -> bob

	h.waitFor(3*time.Second, "alice's burst copies to bob", func() bool {
		return len(h.callsTo(mark, "copyMessage", bob)) >= 2
	})
	h.waitFor(3*time.Second, "alice's burst copy to carol", func() bool {
		return len(h.callsTo(mark, "copyMessage", carol)) >= 1
	})

	// Every copy to bob must have come from alice's chat, never from another user.
	aliceStr := strconv.FormatInt(alice, 10)
	for _, c := range h.callsTo(mark, "copyMessage", bob) {
		if got := c.params["from_chat_id"]; got != aliceStr {
			t.Errorf("copy to bob came from %s; want only alice %s", got, aliceStr)
		}
	}
	// Give any late flush a moment: the assertion below must be exact.
	time.Sleep(150 * time.Millisecond)
	if got := len(h.callsTo(mark, "copyMessage", carol)); got != 1 {
		t.Errorf("copies to carol = %d; want 1 (her own conversation)", got)
	}
}

// TestBatchFlushesBeforeAlbumPreservesOrder: a text and an album sent in the same
// burst are delivered in arrival order — text first, then the complete gallery.
func TestBatchFlushesBeforeAlbumPreservesOrder(t *testing.T) {
	h := newHarness(t)
	h.enableQueue(50*time.Millisecond, 50*time.Millisecond, time.Second, 10)

	recipient := newUID()
	sender := newUID()
	h.startUser(recipient)
	prompt := h.connect(sender, h.linkOf(recipient))

	mark := h.tg.mark()
	h.send(sender, "متن قبل از آلبوم", prompt)
	h.sendPhoto(sender, "album-"+strconv.FormatInt(sender, 10), prompt)
	h.sendPhoto(sender, "album-"+strconv.FormatInt(sender, 10), prompt)

	h.waitFor(3*time.Second, "the album copy", func() bool {
		return len(h.callsTo(mark, "copyMessages", recipient)) == 1
	})
	delta := h.callsTo(mark, "copyMessage", recipient)
	if len(delta) != 1 {
		t.Fatalf("copies to recipient = %d; want the single buffered text", len(delta))
	}
	// The album call must come after the text copy in the recorded order.
	var textIdx, albumIdx = -1, -1
	for i, c := range h.tg.since(mark) {
		switch c.method {
		case "copyMessage":
			if textIdx == -1 {
				textIdx = i
			}
		case "copyMessages":
			if albumIdx == -1 {
				albumIdx = i
			}
		}
	}
	if !(textIdx >= 0 && albumIdx >= 0 && textIdx < albumIdx) {
		t.Fatalf("order violated: text copy index %d, album index %d", textIdx, albumIdx)
	}
}

// TestBatchDisabledDeliversImmediately: with no batcher (the BATCH_WINDOW_MS=0
// behaviour) each message is copied at once, exactly as before the feature.
func TestBatchDisabledDeliversImmediately(t *testing.T) {
	h := newHarness(t) // no enableBatching: nil batcher == disabled

	recipient := newUID()
	sender := newUID()
	h.startUser(recipient)
	prompt := h.connect(sender, h.linkOf(recipient))

	mark := h.tg.mark()
	h.send(sender, "a", prompt)
	h.send(sender, "b", prompt)

	if got := len(h.callsTo(mark, "copyMessage", recipient)); got != 2 {
		t.Fatalf("immediate copies = %d; want 2", got)
	}
}

// --- message box ---------------------------------------------------------------

// TestMessageBoxHoldsThenDelivers: with the box on, the message is NOT delivered on
// arrival; the recipient is notified; opening the box delivers it, tells the sender
// it was seen, and clears the row. A second open does nothing (idempotency).
func TestMessageBoxHoldsThenDelivers(t *testing.T) {
	h := newHarness(t)

	recipient := newUID()
	sender := newUID()
	h.startUser(recipient)
	prompt := h.connect(sender, h.linkOf(recipient))
	h.setMessageBox(recipient, true)

	mark := h.tg.mark()
	h.send(sender, "پیام داخل جعبه", prompt)

	if got := len(h.callsTo(mark, "copyMessage", recipient)); got != 0 {
		t.Fatalf("boxed message was delivered immediately (%d copies); want 0", got)
	}
	if got := len(h.messagesWithText(mark, recipient, txtBoxNewMessages)); got != 1 {
		t.Fatalf("notifications = %d; want 1", got)
	}
	if got := len(h.messagesWithText(mark, sender, txtSentToTheirBox)); got != 1 {
		t.Fatalf("sender confirmations = %d; want 1", got)
	}
	if got := h.pendingBox(recipient); got != 1 {
		t.Fatalf("pending box rows = %d; want 1", got)
	}

	notification := h.boxNotification(recipient)

	mark = h.tg.mark()
	h.press(recipient, notification, mboxOpenData)
	h.waitFor(2*time.Second, "the boxed message delivered", func() bool {
		return len(h.callsTo(mark, "copyMessage", recipient)) == 1
	})
	if got := len(h.messagesWithText(mark, sender, txtSeenSent)); got != 1 {
		t.Fatalf("seen notifications to sender = %d; want 1", got)
	}
	if got := h.pendingBox(recipient); got != 0 {
		t.Fatalf("pending box rows after open = %d; want 0", got)
	}

	// Second press: nothing left to deliver, and the rows must not resurrect.
	mark = h.tg.mark()
	h.press(recipient, notification, mboxOpenData)
	time.Sleep(100 * time.Millisecond)
	if got := len(h.callsTo(mark, "copyMessage", recipient)); got != 0 {
		t.Errorf("second open delivered %d copies; want 0 (idempotent)", got)
	}
	if got := len(h.callsTo(mark, "sendMessage", recipient)); got == 0 {
		t.Error("second open gave no feedback (expected the empty-box reply)")
	}
	if got := h.pendingBox(recipient); got != 0 {
		t.Errorf("pending box rows = %d after the second open; want 0", got)
	}
}

// TestMessageBoxOffByDefault: without opting in, delivery is byte-for-byte the old
// behaviour (no rows, no notification, immediate copy).
func TestMessageBoxOffByDefault(t *testing.T) {
	h := newHarness(t)

	recipient := newUID()
	sender := newUID()
	h.startUser(recipient)
	prompt := h.connect(sender, h.linkOf(recipient))

	mark := h.tg.mark()
	h.send(sender, "پیام معمولی", prompt)

	if got := len(h.callsTo(mark, "copyMessage", recipient)); got != 1 {
		t.Fatalf("immediate copies = %d; want 1 (box is OFF by default)", got)
	}
	if got := len(h.messagesWithText(mark, recipient, txtBoxNewMessages)); got != 0 {
		t.Errorf("notification sent while the box is off")
	}
	if got := h.pendingBox(recipient); got != 0 {
		t.Errorf("pending box rows = %d while the box is off; want 0", got)
	}
}

// TestMessageBoxCommandDeliversEverything: /message_box is the same action as the
// button, delivers all pending messages and reports the count for a command.
func TestMessageBoxCommandDeliversEverything(t *testing.T) {
	h := newHarness(t)

	recipient := newUID()
	sender := newUID()
	h.startUser(recipient)
	prompt := h.connect(sender, h.linkOf(recipient))
	h.setMessageBox(recipient, true)

	h.send(sender, "اول", prompt)
	h.send(sender, "دوم", prompt)
	if got := h.pendingBox(recipient); got != 2 {
		t.Fatalf("pending rows = %d; want 2", got)
	}

	mark := h.tg.mark()
	h.send(recipient, "/message_box", nil)

	h.waitFor(2*time.Second, "both messages delivered by the command", func() bool {
		return len(h.callsTo(mark, "copyMessage", recipient)) == 2
	})
	if got := h.messagesWithText(mark, recipient, "از جعبه تحویل داده شد"); len(got) != 1 {
		t.Fatalf("command summary messages = %d; want 1", len(got))
	}
	if got := h.pendingBox(recipient); got != 0 {
		t.Errorf("pending rows after command = %d; want 0", got)
	}
	if got := len(h.messagesWithText(mark, sender, txtSeenSent)); got != 2 {
		t.Errorf("seen notifications = %d; want 2 (one per delivered message)", got)
	}
}

// TestMessageBoxTransientFailureKeepsRows: a failing delivery must NOT delete the
// row; the next open retries and succeeds, and the sender is told seen exactly once.
func TestMessageBoxTransientFailureKeepsRows(t *testing.T) {
	h := newHarness(t)

	recipient := newUID()
	sender := newUID()
	h.startUser(recipient)
	prompt := h.connect(sender, h.linkOf(recipient))
	h.setMessageBox(recipient, true)

	h.send(sender, "پیام", prompt)
	notification := h.boxNotification(recipient)

	h.tg.failNext("copyMessage", 1, 500, "Internal Server Error")

	mark := h.tg.mark()
	h.press(recipient, notification, mboxOpenData)
	if got := h.pendingBox(recipient); got != 1 {
		t.Fatalf("pending rows after a transient failure = %d; want 1 (must not be lost)", got)
	}
	if got := len(h.messagesWithText(mark, sender, txtSeenSent)); got != 0 {
		t.Errorf("sender was told seen despite a failed delivery")
	}
	// Nothing was delivered in this pass, so the wording is the "could not open"
	// variant (txtBoxPartial is for a pass that delivered some but not all).
	if got := h.messagesWithText(mark, recipient, txtBoxFailed); len(got) != 1 {
		t.Errorf("failure reply = %d; want 1", len(got))
	}

	// Retry: now it succeeds.
	mark = h.tg.mark()
	h.press(recipient, notification, mboxOpenData)
	h.waitFor(2*time.Second, "the retried delivery", func() bool {
		return len(h.callsTo(mark, "copyMessage", recipient)) == 1
	})
	if got := h.pendingBox(recipient); got != 0 {
		t.Errorf("pending rows after the successful retry = %d; want 0", got)
	}
	if got := len(h.messagesWithText(mark, sender, txtSeenSent)); got != 1 {
		t.Errorf("seen notifications after retry = %d; want 1", got)
	}
}

// TestMessageBoxPermanentFailureDropsRow: a message that can never be copied (the
// sender deleted it) is removed instead of wedging the box forever.
func TestMessageBoxPermanentFailureDropsRow(t *testing.T) {
	h := newHarness(t)

	recipient := newUID()
	sender := newUID()
	h.startUser(recipient)
	prompt := h.connect(sender, h.linkOf(recipient))
	h.setMessageBox(recipient, true)

	h.send(sender, "پیام پاک‌شده", prompt)
	notification := h.boxNotification(recipient)

	h.tg.failNext("copyMessage", 1, 400, "Bad Request: message to copy not found")
	mark := h.tg.mark()
	h.press(recipient, notification, mboxOpenData)

	if got := h.pendingBox(recipient); got != 0 {
		t.Fatalf("pending rows after a permanent failure = %d; want 0 (dropped)", got)
	}
	if got := h.messagesWithText(mark, recipient, txtBoxGone); len(got) != 1 {
		t.Errorf("gone-message reply = %d; want 1", len(got))
	}
}

// TestMessageBoxAlbumReassemblesGallery: an album stored in the box must be
// delivered as ONE media group (copyMessages), not as unrelated single copies.
func TestMessageBoxAlbumReassemblesGallery(t *testing.T) {
	h := newHarness(t)

	recipient := newUID()
	sender := newUID()
	h.startUser(recipient)
	prompt := h.connect(sender, h.linkOf(recipient))
	h.setMessageBox(recipient, true)

	group := "boxalbum-" + strconv.FormatInt(sender, 10)
	h.sendPhoto(sender, group, prompt)
	h.sendPhoto(sender, group, prompt)

	if got := h.pendingBox(recipient); got != 2 {
		t.Fatalf("pending album rows = %d; want 2", got)
	}
	notification := h.boxNotification(recipient)

	mark := h.tg.mark()
	h.press(recipient, notification, mboxOpenData)
	h.waitFor(2*time.Second, "one album delivery", func() bool {
		return len(h.callsTo(mark, "copyMessages", recipient)) == 1
	})
	if got := len(h.callsTo(mark, "copyMessage", recipient)); got != 0 {
		t.Errorf("album was delivered as %d separate copies; want a single media group", got)
	}
	if got := h.pendingBox(recipient); got != 0 {
		t.Errorf("pending rows after the album delivery = %d; want 0", got)
	}
}

// TestMessageBoxIsolatedPerRecipient: opening one recipient's box must never
// deliver (or mark seen) another recipient's messages.
func TestMessageBoxIsolatedPerRecipient(t *testing.T) {
	h := newHarness(t)

	alice, bob, sender := newUID(), newUID(), newUID()
	h.startUser(alice)
	h.startUser(bob)
	h.setMessageBox(alice, true)
	h.setMessageBox(bob, true)

	alicePrompt := h.connect(sender, h.linkOf(alice))
	h.send(sender, "برای آلیس", alicePrompt)
	bobPrompt := h.connect(sender, h.linkOf(bob))
	h.send(sender, "برای باب", bobPrompt)

	if gotA, gotB := h.pendingBox(alice), h.pendingBox(bob); gotA != 1 || gotB != 1 {
		t.Fatalf("pending rows alice=%d bob=%d; want 1 each", gotA, gotB)
	}

	mark := h.tg.mark()
	h.press(alice, h.boxNotification(alice), mboxOpenData)
	h.waitFor(2*time.Second, "alice's delivery", func() bool {
		return len(h.callsTo(mark, "copyMessage", alice)) == 1
	})
	if got := len(h.callsTo(mark, "copyMessage", bob)); got != 0 {
		t.Errorf("bob received %d copies when alice opened her box; want 0", got)
	}
	if got := h.pendingBox(bob); got != 1 {
		t.Errorf("bob's pending rows = %d; want 1 (untouched)", got)
	}
	if got := len(h.messagesWithText(mark, sender, txtSeenSent)); got != 1 {
		t.Errorf("seen notifications = %d; want exactly 1 (alice's message only)", got)
	}
}

// TestBatchRespectsMessageBox: when the recipient's box is on, a buffered burst is
// stored as rows (one notification) instead of being delivered, and opening it later
// delivers the whole burst.
func TestBatchRespectsMessageBox(t *testing.T) {
	h := newHarness(t)
	h.enableBatching(50*time.Millisecond, 500*time.Millisecond, 10)

	recipient := newUID()
	sender := newUID()
	h.startUser(recipient)
	prompt := h.connect(sender, h.linkOf(recipient))
	h.setMessageBox(recipient, true)

	mark := h.tg.mark()
	h.send(sender, "یک", prompt)
	h.send(sender, "دو", prompt)
	h.send(sender, "سه", prompt)

	h.waitFor(3*time.Second, "the burst stored in the box", func() bool {
		return h.pendingBox(recipient) == 3
	})
	if got := len(h.callsTo(mark, "copyMessage", recipient)); got != 0 {
		t.Fatalf("buffered messages were delivered despite the box being on (%d copies)", got)
	}
	h.waitFor(2*time.Second, "the burst notification", func() bool {
		return len(h.messagesWithText(mark, recipient, "پیام جدید")) == 1
	})

	notification := h.boxNotification(recipient)
	mark = h.tg.mark()
	h.press(recipient, notification, mboxOpenData)
	h.waitFor(2*time.Second, "the whole burst delivered from the box", func() bool {
		return len(h.callsTo(mark, "copyMessage", recipient)) == 3
	})
	if got := h.pendingBox(recipient); got != 0 {
		t.Errorf("pending rows after opening = %d; want 0", got)
	}
}

// TestMessageBoxCommandWorksMidConversation: /message_box is registered ahead of
// the conversations, so a user who is mid-way through composing a message can still
// open their box (the start conversation's catch-all must not swallow the command).
func TestMessageBoxCommandWorksMidConversation(t *testing.T) {
	h := newHarness(t)

	recipient := newUID()
	sender := newUID()
	h.startUser(recipient)
	h.connect(sender, h.linkOf(recipient))
	h.setMessageBox(recipient, true)

	sendPrompt := h.connect(sender, h.linkOf(recipient))
	h.send(sender, "داخل جعبه", sendPrompt)
	if got := h.pendingBox(recipient); got != 1 {
		t.Fatalf("pending rows = %d; want 1", got)
	}

	// Put the recipient into the composing state for their own conversation, then
	// ask for the box: the command must win over the conversation's fallback.
	h.connect(recipient, h.linkOf(sender))
	mark := h.tg.mark()
	h.send(recipient, "/message_box", nil)

	h.waitFor(2*time.Second, "the boxed message delivered mid-conversation", func() bool {
		return len(h.callsTo(mark, "copyMessage", recipient)) == 1
	})
	if got := h.pendingBox(recipient); got != 0 {
		t.Errorf("pending rows after /message_box = %d; want 0", got)
	}
	if got := h.messagesWithText(mark, recipient, "کنسل"); len(got) != 0 {
		t.Errorf("the command was swallowed as a cancel: %q", got[0].params["text"])
	}
}

// --- follow-up messages after the conversation state ends (Hamid's bug) --------
//
// The conversation state ends as soon as a message is sent (Python behaviour), so a
// second message typed straight after the first used to fall through to the
// catch-all and the sender got "متوجه نشدم" instead of their message being
// delivered. CONTINUE_WINDOW closes that hole; these tests pin it.

// TestFollowupPlainMessagesBothDeliver: after sending, a plain (non-reply) message
// typed immediately is delivered to the same recipient — and batched with the first
// when batching is on.
func TestFollowupPlainMessagesBothDeliver(t *testing.T) {
	h := newHarness(t)
	h.enableBatching(50*time.Millisecond, 500*time.Millisecond, 10)

	recipient := newUID()
	sender := newUID()
	h.startUser(recipient)
	h.connect(sender, h.linkOf(recipient))

	mark := h.tg.mark()
	h.send(sender, "پیام اول", nil)
	h.send(sender, "پیام دوم", nil) // plain follow-up, NOT a reply to the prompt

	h.waitFor(3*time.Second, "both follow-up copies", func() bool {
		return len(h.callsTo(mark, "copyMessage", recipient)) == 2
	})
	if got := h.messagesWithText(mark, sender, txtNotUnderstood); len(got) != 0 {
		t.Fatalf("the follow-up was rejected as '%s' instead of delivered", txtNotUnderstood)
	}
}

// TestFollowupWithoutBatchingBothDeliver: the same guarantee with batching off.
func TestFollowupWithoutBatchingBothDeliver(t *testing.T) {
	h := newHarness(t) // batching disabled

	recipient := newUID()
	sender := newUID()
	h.startUser(recipient)
	h.connect(sender, h.linkOf(recipient))

	mark := h.tg.mark()
	h.send(sender, "اول", nil)
	h.send(sender, "دوم", nil)

	if got := len(h.callsTo(mark, "copyMessage", recipient)); got != 2 {
		t.Fatalf("copies = %d; want 2 (both messages delivered)", got)
	}
}

// TestFollowupExpires: after the window passes the shortcut must not fire, so an
// old conversation cannot silently capture a later message.
func TestFollowupExpires(t *testing.T) {
	h := newHarness(t)
	h.b.Cfg.ContinueWindow = 80 * time.Millisecond
	defer func() { h.b.Cfg.ContinueWindow = 6 * time.Second }()

	recipient := newUID()
	sender := newUID()
	h.startUser(recipient)
	h.connect(sender, h.linkOf(recipient))

	h.send(sender, "اول", nil)
	time.Sleep(250 * time.Millisecond) // well past the 80ms window

	mark := h.tg.mark()
	h.send(sender, "بعداً", nil)
	if got := len(h.callsTo(mark, "copyMessage", recipient)); got != 0 {
		t.Fatalf("a message outside the continue window was delivered to the old target (%d copies)", got)
	}
	if got := h.messagesWithText(mark, sender, txtNotUnderstood); len(got) != 1 {
		t.Fatalf("expected the ordinary 'didn't understand' reply once, got %d", len(got))
	}
}

// TestFollowupDisabled: CONTINUE_WINDOW_MS=0 restores the old behaviour exactly.
func TestFollowupDisabled(t *testing.T) {
	h := newHarness(t)
	h.b.Cfg.ContinueWindow = 0
	defer func() { h.b.Cfg.ContinueWindow = 6 * time.Second }()

	recipient := newUID()
	sender := newUID()
	h.startUser(recipient)
	h.connect(sender, h.linkOf(recipient))

	h.send(sender, "اول", nil)
	mark := h.tg.mark()
	h.send(sender, "دوم", nil)
	if got := len(h.callsTo(mark, "copyMessage", recipient)); got != 0 {
		t.Fatalf("continuation fired while disabled (%d copies)", got)
	}
}

// --- message-box notifications: every new message, with the pending count -------

// TestBoxNotifiesEveryMessageWithCount: each store event pings the recipient, and
// the button carries the current pending count.
func TestBoxNotifiesEveryMessageWithCount(t *testing.T) {
	h := newHarness(t) // batching off: each message is its own store event

	recipient := newUID()
	sender := newUID()
	h.startUser(recipient)
	h.connect(sender, h.linkOf(recipient))
	h.setMessageBox(recipient, true)

	h.send(sender, "یک", nil)
	h.send(sender, "دو", nil) // continuation path -> second store event

	h.waitFor(2*time.Second, "two box notifications", func() bool {
		return len(h.messagesWithText(0, recipient, "پیام جدید")) == 2
	})
	if got := h.pendingBox(recipient); got != 2 {
		t.Fatalf("pending rows = %d; want 2", got)
	}

	notifs := h.messagesWithText(0, recipient, "پیام جدید")
	var kb gotgbot.InlineKeyboardMarkup
	if err := json.Unmarshal([]byte(notifs[len(notifs)-1].params["reply_markup"]), &kb); err != nil {
		t.Fatalf("decode latest notification keyboard: %v", err)
	}
	label := kb.InlineKeyboard[0][0].Text
	if !strings.Contains(label, "(2)") {
		t.Fatalf("latest notification button label = %q; want the pending count (2)", label)
	}

	// Opening the box still delivers both and clears the count.
	notification := h.boxNotification(recipient)
	mark := h.tg.mark()
	h.press(recipient, notification, mboxOpenData)
	h.waitFor(2*time.Second, "both boxed messages delivered", func() bool {
		return len(h.callsTo(mark, "copyMessage", recipient)) == 2
	})
	if got := h.pendingBox(recipient); got != 0 {
		t.Errorf("pending rows after opening = %d; want 0", got)
	}
}

// TestBoxBurstNotifiesOnceWithCount: when batching groups a burst, it produces ONE
// store event and therefore one notification — carrying the burst size.
func TestBoxBurstNotifiesOnceWithCount(t *testing.T) {
	h := newHarness(t)
	h.enableBatching(50*time.Millisecond, 500*time.Millisecond, 10)

	recipient := newUID()
	sender := newUID()
	h.startUser(recipient)
	prompt := h.connect(sender, h.linkOf(recipient))
	h.setMessageBox(recipient, true)

	mark := h.tg.mark()
	h.send(sender, "یک", nil)
	h.send(sender, "دو", prompt)
	h.send(sender, "سه", prompt)

	h.waitFor(3*time.Second, "the burst stored", func() bool {
		return h.pendingBox(recipient) == 3
	})
	h.waitFor(2*time.Second, "the burst notification", func() bool {
		return len(h.messagesWithText(mark, recipient, "پیام جدید")) >= 1
	})
	notifs := h.messagesWithText(mark, recipient, "پیام جدید")
	if len(notifs) != 1 {
		t.Fatalf("notifications for a batched burst = %d; want 1 (with the count)", len(notifs))
	}
	var kb gotgbot.InlineKeyboardMarkup
	if err := json.Unmarshal([]byte(notifs[0].params["reply_markup"]), &kb); err != nil {
		t.Fatalf("decode notification keyboard: %v", err)
	}
	if !strings.Contains(kb.InlineKeyboard[0][0].Text, "(3)") {
		t.Errorf("burst notification button = %q; want it to show (3)", kb.InlineKeyboard[0][0].Text)
	}
}

// --- /start now opens the main menu, exactly like /menu -------------------------

// TestStartOpensMainMenuLikeMenu: both commands must produce the same menu message
// (short text + the main keyboard), while /start <cid> keeps connecting.
func TestStartOpensMainMenuLikeMenu(t *testing.T) {
	h := newHarness(t)

	uid := newUID()
	h.send(uid, "/start", nil)
	startMark := h.tg.mark()

	// The /menu that follows is a fresh update; compare what each command sent.
	h.send(uid, "/menu", nil)
	menuCalls := h.messagesWithText(startMark, uid, "")
	if len(menuCalls) == 0 {
		t.Fatalf("no message after /menu")
	}

	// Both menu messages: same text and same keyboard as the /menu one.
	var startCall, menuCall *apiCall
	calls := h.callsTo(0, "sendMessage", uid)
	for i := range calls {
		if strings.Contains(calls[i].params["text"], "منوی اصلی") {
			cc := calls[i]
			if startCall == nil {
				startCall = &cc
			} else {
				menuCall = &cc
			}
		}
	}
	if startCall == nil || menuCall == nil {
		t.Fatalf("expected two main-menu messages (from /start and /menu), got start=%v menu=%v", startCall, menuCall)
	}
	if startCall.params["text"] != menuCall.params["text"] {
		t.Errorf("/start menu text %q != /menu menu text %q", startCall.params["text"], menuCall.params["text"])
	}
	if startCall.params["reply_markup"] != menuCall.params["reply_markup"] {
		t.Errorf("/start and /menu produced different main keyboards")
	}
	if !strings.Contains(startCall.params["reply_markup"], "settings-menu") {
		t.Errorf("/start did not produce the main menu keyboard: %q", startCall.params["reply_markup"])
	}
}

// --- replies are batched too (Hamid round 3) -----------------------------------

// deliveredMessageTo rebuilds the bot's delivered message in a recipient's chat from
// the recorded CopyMessage call (chat id + keyboard). A reply to it exercises the
// real answer flow exactly like a user tapping the keyboard.
func (h *harness) deliveredMessageTo(uid int64) *gotgbot.Message {
	h.t.Helper()
	var latest *apiCall
	for _, c := range h.callsTo(0, "copyMessage", uid) {
		cc := c
		latest = &cc
	}
	if latest == nil {
		h.t.Fatalf("no delivered message recorded for %d", uid)
	}
	var kb gotgbot.InlineKeyboardMarkup
	if err := json.Unmarshal([]byte(latest.params["reply_markup"]), &kb); err != nil {
		h.t.Fatalf("decode delivered keyboard: %v", err)
	}
	return &gotgbot.Message{
		MessageId:   h.mid.Add(1),
		From:        &gotgbot.User{Id: fakeBotID, IsBot: true, Username: "chevalet_test_bot"},
		Chat:        gotgbot.Chat{Id: uid, Type: "private"},
		Text:        "delivered",
		ReplyMarkup: &kb,
	}
}

// TestRepliesAreBatched: several quick replies to delivered messages go to the
// original sender as ONE delivery pass (two copies, one confirmation), instead of
// one by one.
func TestRepliesAreBatched(t *testing.T) {
	h := newHarness(t)
	h.enableBatching(50*time.Millisecond, 500*time.Millisecond, 10)

	recipient := newUID()
	sender := newUID()
	h.startUser(recipient)
	prompt := h.connect(sender, h.linkOf(recipient))
	h.send(sender, "پیام اولیه", prompt)
	h.waitFor(3*time.Second, "the delivered message", func() bool {
		return len(h.callsTo(0, "copyMessage", recipient)) >= 1
	})
	delivered := h.deliveredMessageTo(recipient)

	mark := h.tg.mark()
	h.send(recipient, "جواب اول", delivered)
	h.send(recipient, "جواب دوم", delivered)

	h.waitFor(3*time.Second, "both replies delivered to the sender", func() bool {
		return len(h.callsTo(mark, "copyMessage", sender)) == 2
	})
	replies := h.callsTo(mark, "copyMessage", sender)
	senderStr := strconv.FormatInt(sender, 10)
	for i, c := range replies {
		if c.params["from_chat_id"] != strconv.FormatInt(recipient, 10) {
			t.Errorf("reply %d came from %s; want the recipient's chat", i, c.params["from_chat_id"])
		}
		if !strings.Contains(c.params["reply_parameters"], senderStr) {
			t.Errorf("reply %d does not quote the sender's message: %s", i, c.params["reply_parameters"])
		}
	}
	for _, c := range h.callsTo(mark, "copyMessage", sender) {
		if c.params["reply_parameters"] == "" {
			t.Errorf("a batched reply lost its reply context")
		}
	}
	h.waitFor(2*time.Second, "the batch confirmation", func() bool {
		return len(h.messagesWithText(mark, recipient, txtSentToThem)) == 1
	})
	if got := len(h.messagesWithText(mark, recipient, txtSentToThem)); got != 1 {
		t.Errorf("reply confirmations to the replier = %d; want 1 for the batch", got)
	}
	if got := h.messagesWithText(mark, recipient, txtNotUnderstood); len(got) != 0 {
		t.Errorf("a reply was rejected as 'didn't understand'")
	}
}

// --- gallery: one copy pass, and one box notification for album+extra ------------

// TestGalleryCopiedOnceWithoutDeleteLoop: an album is copied in exactly one
// CopyMessages call; the old delete-and-re-copy loop produces no deleteMessages.
func TestGalleryCopiedOnceWithoutDeleteLoop(t *testing.T) {
	h := newHarness(t)
	h.enableQueue(0, 40*time.Millisecond, 2*time.Second, 10) // singles immediate, albums buffered

	recipient := newUID()
	sender := newUID()
	h.startUser(recipient)
	prompt := h.connect(sender, h.linkOf(recipient))

	mark := h.tg.mark()
	h.sendPhoto(sender, "g-once", prompt)
	h.sendPhoto(sender, "g-once", prompt)
	h.sendPhoto(sender, "g-once", prompt)

	h.waitFor(3*time.Second, "the gallery", func() bool {
		return len(h.callsTo(mark, "copyMessages", recipient)) == 1
	})
	time.Sleep(120 * time.Millisecond)
	if got := len(h.callsTo(mark, "copyMessages", recipient)); got != 1 {
		t.Errorf("copyMessages calls = %d; want exactly 1", got)
	}
	if got := len(h.callsTo(mark, "copyMessage", recipient)); got != 0 {
		t.Errorf("gallery produced %d single copies; want 0", got)
	}
	if got := len(h.callsTo(mark, "deleteMessages", recipient)); got != 0 {
		t.Errorf("gallery produced %d deleteMessages; want 0 (no re-copy loop)", got)
	}
}

// TestBoxAlbumPlusSingleNotifiesOnce: an album (2 photos) plus one standalone photo
// sent in one burst produces ONE message-box notification carrying the total (3) —
// not one per item.
func TestBoxAlbumPlusSingleNotifiesOnce(t *testing.T) {
	h := newHarness(t)
	h.enableQueue(60*time.Millisecond, 60*time.Millisecond, 600*time.Millisecond, 10)

	recipient := newUID()
	sender := newUID()
	h.startUser(recipient)
	prompt := h.connect(sender, h.linkOf(recipient))
	h.setMessageBox(recipient, true)

	mark := h.tg.mark()
	h.sendPhoto(sender, "box-g", prompt) // album item 1
	h.sendPhoto(sender, "box-g", prompt) // album item 2
	h.sendPhoto(sender, "", prompt)      // standalone photo (no media group)

	h.waitFor(3*time.Second, "all three stored in the box", func() bool {
		return h.pendingBox(recipient) == 3
	})
	h.waitFor(2*time.Second, "the box notification", func() bool {
		return len(h.messagesWithText(mark, recipient, "پیام جدید")) >= 1
	})
	time.Sleep(120 * time.Millisecond) // any (buggy) extra notification would land now
	notifs := h.messagesWithText(mark, recipient, "پیام جدید")
	if len(notifs) != 1 {
		t.Fatalf("box notifications = %d; want exactly 1 (with the total count)", len(notifs))
	}
	var kb gotgbot.InlineKeyboardMarkup
	if err := json.Unmarshal([]byte(notifs[0].params["reply_markup"]), &kb); err != nil {
		t.Fatalf("decode notification keyboard: %v", err)
	}
	if label := kb.InlineKeyboard[0][0].Text; !strings.Contains(label, "(3)") {
		t.Errorf("notification button = %q; want the total count (3)", label)
	}

	// Opening the box: album stays a gallery, single photo arrives as itself.
	notification := h.boxNotification(recipient)
	mark = h.tg.mark()
	h.press(recipient, notification, mboxOpenData)
	h.waitFor(3*time.Second, "the box contents delivered", func() bool {
		return len(h.callsTo(mark, "copyMessages", recipient)) == 1 &&
			len(h.callsTo(mark, "copyMessage", recipient)) == 1
	})
	if got := h.pendingBox(recipient); got != 0 {
		t.Errorf("pending rows after opening = %d; want 0", got)
	}
}

// TestReplyThenPlainBatchTogether: a reply and a plain message typed straight after
// it (same conversation) are delivered as one batch in arrival order — the reply
// with its reply context, the plain message without.
func TestReplyThenPlainBatchTogether(t *testing.T) {
	h := newHarness(t)
	h.enableBatching(50*time.Millisecond, 500*time.Millisecond, 10)

	recipient := newUID()
	sender := newUID()
	h.startUser(recipient)
	prompt := h.connect(sender, h.linkOf(recipient))
	h.send(sender, "پیام اولیه", prompt)
	h.waitFor(3*time.Second, "the delivered message", func() bool {
		return len(h.callsTo(0, "copyMessage", recipient)) >= 1
	})
	delivered := h.deliveredMessageTo(recipient)

	mark := h.tg.mark()
	h.send(recipient, "جواب", delivered)
	h.send(recipient, "و یه پیام دیگه", nil) // continuation: same recipient

	h.waitFor(3*time.Second, "both items delivered to the sender", func() bool {
		return len(h.callsTo(mark, "copyMessage", sender)) == 2
	})
	replies := h.callsTo(mark, "copyMessage", sender)
	if len(replies) != 2 {
		t.Fatalf("copies = %d; want 2", len(replies))
	}
	if replies[0].params["reply_parameters"] == "" {
		t.Errorf("the first (reply) copy lost its reply context")
	}
	if replies[1].params["reply_parameters"] != "" {
		t.Errorf("the plain follow-up unexpectedly carries reply context: %s", replies[1].params["reply_parameters"])
	}
}

// TestAlbumReplyIsBatched: an album sent as a reply still becomes one gallery plus a
// carrier that replies to the original message.
func TestAlbumReplyIsBatched(t *testing.T) {
	h := newHarness(t)
	h.enableQueue(50*time.Millisecond, 50*time.Millisecond, time.Second, 10)

	recipient := newUID()
	sender := newUID()
	h.startUser(recipient)
	prompt := h.connect(sender, h.linkOf(recipient))
	h.send(sender, "پیام اولیه", prompt)
	h.waitFor(3*time.Second, "the delivered message", func() bool {
		return len(h.callsTo(0, "copyMessage", recipient)) >= 1
	})
	delivered := h.deliveredMessageTo(recipient)

	mark := h.tg.mark()
	h.sendPhoto(recipient, "reply-album", delivered)
	h.sendPhoto(recipient, "reply-album", delivered)

	h.waitFor(3*time.Second, "the reply album", func() bool {
		return len(h.callsTo(mark, "copyMessages", sender)) == 1
	})
	if got := len(h.callsTo(mark, "copyMessage", sender)); got != 0 {
		t.Errorf("album reply produced %d standalone copies; want 0", got)
	}
	// The carrier message must carry the reply context (sender's message id quoted).
	h.waitFor(2*time.Second, "the album carrier", func() bool {
		return len(h.messagesWithText(mark, sender, txtMediaUseThis)) == 1
	})
	carriers := h.messagesWithText(mark, sender, txtMediaUseThis)
	if len(carriers) != 1 {
		t.Fatalf("album carrier messages = %d; want 1", len(carriers))
	}
	if !strings.Contains(carriers[0].params["reply_parameters"], strconv.FormatInt(sender, 10)) {
		t.Errorf("album carrier lost its reply context: %s", carriers[0].params["reply_parameters"])
	}
}
