package bot

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/PaulSonOfLars/gotgbot/v2"

	"github.com/aturzone/chevaletAnonBot/internal/config"
)

// deliverBatch is the batcher's flush handler: it delivers one conversation's
// buffered burst. It runs outside the dispatcher and outside the per-user mutex
// (see batcher.go), so it must not touch userData; it reads everything it needs
// from the database and from the buffered messages themselves.
//
// Failure policy:
//   - a retryable Telegram failure is already queued durably by the rate-limited
//     client (see ratelimit.go/outbox.go) while the send is attempted, so a message
//     is never silently dropped here;
//   - a permanent failure (blocked bot, deleted account) is logged and skipped;
//   - the safe fallback for a database error while storing into the Message Box is
//     to deliver immediately — the old behaviour, never a lost message.
func (b *Bot) deliverBatch(key convKey, items []batchItem) {
	if len(items) == 0 {
		return
	}
	dbctx, cancel := context.WithTimeout(context.Background(), dbOpTimeout)
	defer cancel()

	senderID, err := strconv.ParseInt(key.senderUID, 10, 64)
	if err != nil {
		return
	}
	targetID, err := strconv.ParseInt(key.targetUID, 10, 64)
	if err != nil {
		return
	}
	last := items[len(items)-1].msg

	// The recipient may have blocked the sender while the burst was buffered.
	if blocked, berr := b.DB.IsBlocked(dbctx, key.targetUID, key.senderUID); berr == nil && blocked {
		if last != nil {
			_, _ = last.Reply(b.TG, txtBlockedYou, nil)
		}
		return
	}

	// Message Box: persist the whole burst, ping once, and do not deliver now.
	if b.boxEnabled(dbctx, key.targetUID) {
		boxItems := make([]boxIncoming, 0, len(items))
		for _, it := range items {
			boxItems = append(boxItems, boxIncoming{msg: it.msg, targetCid: it.targetCid})
		}
		if err := b.storeBoxIncoming(dbctx, key.targetUID, key.senderUID, boxItems); err == nil {
			if last != nil {
				_, _ = last.Reply(b.TG, txtSentToTheirBox, nil)
			}
			return
		} else {
			// Fall through to immediate delivery: a message must not be lost because
			// the box could not be written.
			slog.Warn("batch: message-box store failed; delivering directly", "err", err)
		}
	}

	// Recipient-side settings, read once for the whole burst.
	seen, err := b.DB.GetSeenStatus(dbctx, key.targetUID)
	if err != nil {
		slog.Warn("batch: could not read the seen setting", "err", err)
	}
	wpp, err := b.DB.GetWPP(dbctx, key.targetUID)
	if err != nil {
		slog.Warn("batch: could not read the preview setting", "err", err)
		wpp = true
	}
	targetCids, err := b.DB.GetCIDs(dbctx, key.targetUID)
	if err != nil {
		slog.Warn("batch: could not read the recipient's links", "err", err)
	}
	customTag, err := b.DB.GetCustomTag(dbctx, key.targetUID)
	if err != nil {
		slog.Warn("batch: could not read the custom tag", "err", err)
	}
	audioTag, err := b.DB.GetAudioTag(dbctx, key.targetUID)
	if err != nil {
		slog.Warn("batch: could not read the audio tag", "err", err)
	}
	anonEnabled, anonName, anonEmoji, err := b.DB.GetAnonSig(dbctx, key.senderUID)
	if err != nil {
		slog.Warn("batch: could not read the sender signature", "err", err)
	}
	sig := anonSignature(anonEnabled, anonName, anonEmoji)
	warning, err := b.DB.GetWarning(dbctx, key.senderUID)
	if err != nil {
		slog.Warn("batch: could not read the warning setting", "err", err)
		warning = true
	}
	donation := b.Dyn.DonationEnabled()
	donationLink := b.Dyn.DonationLink()

	copiedIDs := make([]string, 0, len(items))
	delivered := 0
	var lastErr error
	for _, it := range items {
		if it.msg == nil {
			continue
		}
		copiedID, cerr := b.copyBatchItem(dbctx, senderID, targetID, it, seen, wpp, targetCids,
			customTag, audioTag, sig, donation, donationLink)
		if cerr != nil {
			lastErr = cerr
			if permanentSendFailure(cerr) {
				slog.Info("batch: dropping an undeliverable message", "kind", kindOf(it.msg), "err", cerr)
			} else {
				// The rate-limited client queued it for retry; the rest of the burst
				// is still delivered now.
				slog.Info("batch: a delivery failed and was queued for retry", "kind", kindOf(it.msg))
			}
			continue
		}
		copiedIDs = append(copiedIDs, strconv.FormatInt(copiedID, 10))
		delivered++
	}

	if delivered == 0 {
		if lastErr != nil && last != nil && permanentSendFailure(lastErr) {
			_, _ = last.Reply(b.TG, txtMaybeBlockedOrCleared, nil)
		}
		return
	}

	// Count each delivered message for the daily stats (best effort, like the
	// immediate path).
	for i := 0; i < delivered; i++ {
		if cerr := b.DB.CountMessage(dbctx); cerr != nil {
			slog.Warn("could not count a delivered message", "err", cerr)
		}
	}

	b.sendBatchConfirmation(last, targetID, warning, copiedIDs)
}

// copyBatchItem delivers one buffered message exactly as the immediate path does:
// fresh sealed tokens, the same keyboard (reply/other, plus the optional "sent
// with link N" row and donation row), the target's link-preview preference, the
// receiver's custom/audio tag and the sender's anonymous signature.
func (b *Bot) copyBatchItem(dbctx context.Context, senderID, targetID int64, it batchItem,
	seen, wpp bool, targetCids []string, customTag, audioTag, sig string,
	donation bool, donationLink string) (int64, error) {

	msg := it.msg
	midStr := strconv.FormatInt(msg.MessageId, 10)
	tokenMid, err := b.Tokens.Seal(senderID, midAAD(midStr))
	if err != nil {
		return 0, err
	}
	tokenBlock, err := b.Tokens.Seal(senderID, blockAAD())
	if err != nil {
		return 0, err
	}

	keyboard := messageKeyboard(tokenMid, tokenBlock, msg.MessageId, seen)
	if len(targetCids) > 1 && it.targetCid != "" {
		if idx := indexOf(targetCids, it.targetCid); idx >= 0 {
			keyboard = append(keyboard, []gotgbot.InlineKeyboardButton{
				cb(fmt.Sprintf("ارسال شده با لینک %d (%s)", idx+1, it.targetCid), "no-callback"),
			})
		}
	}
	if donation {
		keyboard = append(keyboard, donationRow(donationLink))
	}
	replyMarkup := gotgbot.InlineKeyboardMarkup{InlineKeyboard: keyboard}

	mi, err := msg.Copy(b.TG, targetID, &gotgbot.CopyMessageOpts{ParseMode: "HTML", ReplyMarkup: replyMarkup})
	if err != nil {
		return 0, err
	}
	copiedID := mi.MessageId

	// Honour the recipient's "no link preview" choice, like the immediate path.
	tagPreview := msg.LinkPreviewOptions
	if !wpp && msg.Text != "" {
		preview := &gotgbot.LinkPreviewOptions{IsDisabled: true}
		_, _, _ = b.TG.EditMessageText(msg.OriginalHTML(), &gotgbot.EditMessageTextOpts{
			ChatId:             targetID,
			MessageId:          copiedID,
			ParseMode:          "HTML",
			ReplyMarkup:        replyMarkup,
			LinkPreviewOptions: preview,
		})
		tagPreview = preview
	}

	// Tags/signature: same rules and order as the immediate path.
	switch {
	case msg.Audio != nil && customTag == "":
		b.addTag(msg, "caption", targetID, copiedID, replyMarkup, withSig(sanitizeUserHTML(audioTag), sig), tagPreview)
	case customTag != "":
		body := withSig(sanitizeUserHTML(customTag), sig)
		if !b.addTag(msg, "text", targetID, copiedID, replyMarkup, body, tagPreview) {
			b.addTag(msg, "caption", targetID, copiedID, replyMarkup, body, tagPreview)
		}
	case sig != "":
		body := withSig("", sig)
		if !b.addTag(msg, "text", targetID, copiedID, replyMarkup, body, tagPreview) {
			b.addTag(msg, "caption", targetID, copiedID, replyMarkup, body, tagPreview)
		}
	}
	return copiedID, nil
}

// sendBatchConfirmation sends the sender ONE "sent" notice for the whole burst,
// with delete buttons covering every copied message when the warning setting is on.
// (The immediate path sends one notice per message; a batch is one delivery, so it
// gets one notice.)
func (b *Bot) sendBatchConfirmation(last *gotgbot.Message, targetID int64, warning bool, copiedIDs []string) {
	if last == nil {
		return
	}
	if !warning || len(copiedIDs) == 0 {
		_, _ = last.Reply(b.TG, txtSentToThem, nil)
		return
	}
	deletionToken, err := b.Tokens.Seal(targetID, deleteAAD(copiedIDs))
	if err != nil {
		_, _ = last.Reply(b.TG, txtSentToThem, nil)
		return
	}
	parts := append([]string{deletionToken}, copiedIDs...)
	rows := packDeleteButtons(parts[0], parts[1:])
	warnText := txtSentToThem + "\n" + fmt.Sprintf(config.DeletionText, config.DeletionTimeout)
	warnMsg, err := last.Reply(b.TG, warnText, &gotgbot.SendMessageOpts{
		ParseMode:   "HTML",
		ReplyMarkup: gotgbot.InlineKeyboardMarkup{InlineKeyboard: rows},
	})
	if err != nil {
		return
	}
	b.scheduleDeleteWarning(warnMsg, config.DeletionTimeout)
}

// flushPendingBatch delivers any buffered batch of one conversation before an
// immediate send. A nil batcher means a Bot built without New (tests), where
// buffering is simply off.
func (b *Bot) flushPendingBatch(key convKey) {
	if b.batcher == nil {
		return
	}
	b.batcher.FlushKey(key)
}

// batcherSweepLoop is the insurance path for buffered messages: the debounce timer
// is the primary flush, this loop catches a batch whose timer was somehow lost.
func (b *Bot) batcherSweepLoop(ctx context.Context) {
	if b.batcher == nil || !b.batcher.Enabled() {
		return
	}
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.batcher.Sweep()
		}
	}
}
