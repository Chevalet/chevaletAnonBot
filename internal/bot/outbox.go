package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"time"
)

// The retry worker behind the durable outbox.
//
// Sends remain synchronous, so nothing about the user-visible flow changes: the
// handler still gets the message id back and still builds the buttons and the
// "sent" confirmation from it. This only catches the sends that FAIL for a reason
// that may succeed later, which previously were simply lost — the user was told to
// try again, or in the restart case told nothing at all.
//
// Only retryable failures are enqueued. A 403, a missing message, or "not enough
// rights" will fail identically forever; retrying those would burn send budget and
// never deliver.
const (
	outboxTick     = 5 * time.Second
	outboxBatch    = 15 // per tick, so a backlog drains without monopolising the rate budget
	outboxMaxTries = 8  // with the DB's exponential backoff this spans a few hours
)

// retryableMethods are the calls worth re-attempting later.
//
// Message-producing calls only. An edit or a callback answer is tied to a moment
// that has passed — Telegram rejects a stale callback query outright, and an edit
// replayed minutes later would overwrite whatever the user has since done.
var retryableMethods = map[string]bool{
	"sendMessage":    true,
	"copyMessage":    true,
	"copyMessages":   true,
	"sendPhoto":      true,
	"sendVideo":      true,
	"sendAudio":      true,
	"sendVoice":      true,
	"sendDocument":   true,
	"sendAnimation":  true,
	"sendSticker":    true,
	"sendMediaGroup": true,
}

// enqueueFailedSend is handed to the limiter's client, which calls it when a send
// fails in a way worth retrying.
func (b *Bot) enqueueFailedSend(method string, params map[string]any) {
	if !retryableMethods[method] {
		return
	}
	chatID := chatIDFromParams(params)
	if chatID == 0 {
		return // nowhere to deliver it
	}
	raw, err := json.Marshal(params)
	if err != nil {
		slog.Warn("outbox: could not serialise a failed send", "method", method, "err", err)
		return
	}

	dbctx, cancel := b.bg()
	defer cancel()
	if err := b.DB.EnqueueSend(dbctx, chatID, method, raw); err != nil {
		slog.Warn("outbox: could not queue a failed send", "method", method, "err", err)
		return
	}
	slog.Info("outbox: queued a failed send for retry", "method", method, "chat", chatID)
}

// outboxLoop drains the queue until ctx is cancelled.
func (b *Bot) outboxLoop(ctx context.Context) {
	t := time.NewTicker(outboxTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.drainOutbox(ctx)
		}
	}
}

// drainOutbox attempts one batch.
func (b *Bot) drainOutbox(ctx context.Context) {
	// While Telegram has us paused there is no point claiming work: every attempt
	// would fail and burn an attempt counter.
	if d := b.limiter.floodWaitRemaining(); d > 0 {
		return
	}

	dbctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
	defer cancel()

	if n, err := b.DB.DropExhaustedSends(dbctx, outboxMaxTries); err != nil {
		slog.Warn("outbox: could not drop exhausted items", "err", err)
	} else if n > 0 {
		// Worth saying out loud: these are messages that will never arrive.
		slog.Warn("outbox: gave up on undeliverable messages", "count", n, "after_attempts", outboxMaxTries)
	}

	items, err := b.DB.ClaimDueSends(dbctx, outboxBatch)
	if err != nil {
		slog.Warn("outbox: could not claim due sends", "err", err)
		return
	}
	for _, it := range items {
		params, err := decodeOutboxParams(it.Params)
		if err != nil {
			// Unparseable: it can never be sent, so stop holding it.
			slog.Warn("outbox: dropping an unreadable item", "id", it.ID, "err", err)
			_ = b.DB.DeleteSend(dbctx, it.ID)
			continue
		}

		// Straight through the same client, so the retry is paced like any other send.
		if _, err := b.TG.RequestWithContext(ctx, it.Method, params, nil); err != nil {
			if permanentSendFailure(err) {
				slog.Info("outbox: dropping an undeliverable item", "id", it.ID, "method", it.Method, "err", err)
				_ = b.DB.DeleteSend(dbctx, it.ID)
				continue
			}
			// Still failing for a transient reason: ClaimDueSends already scheduled the
			// next attempt, so leave it be.
			slog.Info("outbox: retry failed, will try again", "id", it.ID, "attempts", it.Attempts)
			continue
		}
		if err := b.DB.DeleteSend(dbctx, it.ID); err != nil {
			// Delivered but not removed. Logged loudly because the next tick would
			// deliver it a second time.
			slog.Error("outbox: delivered but could not delete the item", "id", it.ID, "err", err)
			continue
		}
		slog.Info("outbox: delivered a queued message", "method", it.Method, "attempts", it.Attempts)
	}
}

// decodeOutboxParams reconstructs the stored request params for a retry.
//
// Decode with UseNumber. The stored JSON was produced from gotgbot's typed params
// map, so every numeric field (chat_id, message_id, …) is a JSON NUMBER. Decoding
// into map[string]any without UseNumber turns those into float64, and gotgbot
// renders a float64 with fmt.Sprint — chat 1234567890 became "1.23456789e+09",
// Telegram answered "chat not found", and the retry (the whole point of this
// queue) never delivered anything. json.Number keeps the original digits for
// scalars AND for arrays like message_ids, which gotgbot re-marshals verbatim.
func decodeOutboxParams(raw []byte) (map[string]any, error) {
	var params map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&params); err != nil {
		return nil, err
	}
	return params, nil
}

// retryableSendFailure reports whether a failed send is worth queueing.
func retryableSendFailure(err error) bool {
	if err == nil {
		return false
	}
	if _, isFlood := errTooManyRequests(err); isFlood {
		return true
	}
	if isNetworkError(err) {
		return true
	}
	return false
}

// permanentSendFailure reports whether retrying is pointless: the user blocked the
// bot, the chat is gone, the bot may not post there, or the source message the copy
// referred to no longer exists.
func permanentSendFailure(err error) bool {
	switch {
	case err == nil:
		return false
	case errForbidden(err), errNoSendRights(err), errMessageIDInvalid(err):
		return true
	case descContains(err, "message to copy not found"),
		descContains(err, "chat not found"),
		descContains(err, "user is deactivated"):
		return true
	}
	return false
}

// outboxStatus is a one-line summary for /admin_stats: how many deliveries are
// outstanding and how long the oldest has waited. A depth that grows, or an age
// that climbs, is the signal that deliveries are not draining.
func (b *Bot) outboxStatus() string {
	dbctx, cancel := b.bg()
	defer cancel()

	depth, err := b.DB.OutboxDepth(dbctx)
	if err != nil {
		return ""
	}
	if depth == 0 {
		return "• صف ارسال: خالی ✅"
	}
	age, _ := b.DB.OldestOutboxAge(dbctx)
	return "• صف ارسال: <b>" + itoaInt(depth) + "</b> پیام در انتظار (قدیمی‌ترین: " + age.Round(time.Second).String() + ")"
}

func itoaInt(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
