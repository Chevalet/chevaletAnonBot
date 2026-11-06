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

// enableBatching installs a batcher with a short window (tests must not sleep for
// the production 800ms) and shuts it down at the end of the test.
func (h *harness) enableBatching(window, maxWait time.Duration, maxItems int) {
	h.t.Helper()
	h.b.batcher = newBatcher(window, maxWait, maxItems, h.b.deliverBatch)
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
		if strings.Contains(c.params["text"], txtBoxNewMessages) {
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
	time.Sleep(200 * time.Millisecond)

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
	if got := len(h.callsTo(mark, "copyMessage", carol)); got != 1 {
		t.Errorf("copies to carol = %d; want 1 (her own conversation)", got)
	}
}

// TestBatchFlushesBeforeAlbumPreservesOrder: a buffered text must be delivered
// BEFORE an immediately-handled album, even though the album is not batched.
func TestBatchFlushesBeforeAlbumPreservesOrder(t *testing.T) {
	h := newHarness(t)
	h.enableBatching(10*time.Second, 10*time.Second, 10) // never timed out during the test

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
	if got := len(h.messagesWithText(mark, recipient, txtBoxNewMessages)); got != 1 {
		t.Fatalf("notifications = %d; want 1 for the whole burst", got)
	}

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
