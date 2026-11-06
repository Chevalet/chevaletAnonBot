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

// deliverUnits is the queue's flush handler: it delivers one conversation's buffered
// items IN ORDER — ordinary messages and replies as individual copies with their own
// reply context and keyboard, each gallery as ONE media group (a single CopyMessages
// call) plus its keyboard carrier — then sends the sender ONE confirmation covering
// everything.
//
// It runs outside the dispatcher and per-user mutex, so it reads everything from the
// database and from the buffered messages. Failure policy: retryable Telegram errors
// are already queued durably by the rate-limited client; permanent ones are logged
// and skipped; a message-box store failure falls back to direct delivery.
func (b *Bot) deliverUnits(key convKey, units []queueUnit) {
	if len(units) == 0 {
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
	last := queueLastMessage(units)

	// The recipient may have blocked the sender while the burst was buffered.
	if blocked, berr := b.DB.IsBlocked(dbctx, key.targetUID, key.senderUID); berr == nil && blocked {
		if last != nil {
			_, _ = last.Reply(b.TG, txtBlockedYou, nil)
		}
		return
	}

	// Message Box: persist the whole queue (albums and replies included) and ping
	// ONCE with the total pending count — not once per message or album item.
	if b.boxEnabled(dbctx, key.targetUID) {
		if err := b.storeUnitsInBox(dbctx, key, units); err == nil {
			if last != nil {
				_, _ = last.Reply(b.TG, txtSentToTheirBox, nil)
			}
			return
		} else {
			slog.Warn("queue: message-box store failed; delivering directly", "err", err)
		}
	}

	// Recipient-side settings, read once for the whole queue.
	seen, err := b.DB.GetSeenStatus(dbctx, key.targetUID)
	if err != nil {
		slog.Warn("queue: could not read the seen setting", "err", err)
	}
	wpp, err := b.DB.GetWPP(dbctx, key.targetUID)
	if err != nil {
		slog.Warn("queue: could not read the preview setting", "err", err)
		wpp = true
	}
	targetCids, err := b.DB.GetCIDs(dbctx, key.targetUID)
	if err != nil {
		slog.Warn("queue: could not read the recipient's links", "err", err)
	}
	customTag, err := b.DB.GetCustomTag(dbctx, key.targetUID)
	if err != nil {
		slog.Warn("queue: could not read the custom tag", "err", err)
	}
	audioTag, err := b.DB.GetAudioTag(dbctx, key.targetUID)
	if err != nil {
		slog.Warn("queue: could not read the audio tag", "err", err)
	}
	anonEnabled, anonName, anonEmoji, err := b.DB.GetAnonSig(dbctx, key.senderUID)
	if err != nil {
		slog.Warn("queue: could not read the sender signature", "err", err)
	}
	warning, err := b.DB.GetWarning(dbctx, key.senderUID)
	if err != nil {
		slog.Warn("queue: could not read the warning setting", "err", err)
		warning = true
	}

	env := queueEnv{
		senderID: senderID, targetID: targetID, targetUID: key.targetUID,
		seen: seen, wpp: wpp, targetCids: targetCids,
		customTag: customTag, audioTag: audioTag,
		sig:      anonSignature(anonEnabled, anonName, anonEmoji),
		donation: b.Dyn.DonationEnabled(), donationLink: b.Dyn.DonationLink(),
	}

	var copiedIDs []string
	delivered := 0
	anyChannel := false
	var lastErr error
	for _, u := range units {
		if u.reply.channel {
			anyChannel = true
		}
		if u.kind == unitAlbum {
			ids, n, cerr := b.deliverQueuedAlbum(env, u)
			if cerr != nil {
				lastErr = cerr
				logQueueError("album", cerr)
				continue
			}
			copiedIDs = append(copiedIDs, ids...)
			delivered += n
			continue
		}
		copiedID, cerr := b.copyQueuedSingle(env, u)
		if cerr != nil {
			lastErr = cerr
			logQueueError("message", cerr)
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
	for i := 0; i < delivered; i++ {
		if cerr := b.DB.CountMessage(dbctx); cerr != nil {
			slog.Warn("could not count a delivered message", "err", cerr)
		}
	}
	b.sendBatchConfirmation(last, targetID, key.targetUID, warning || anyChannel, anyChannel, copiedIDs)
}

// logQueueError keeps the per-item failure handling in one place.
func logQueueError(kind string, err error) {
	if permanentSendFailure(err) {
		slog.Info("queue: dropping an undeliverable item", "kind", kind, "err", err)
		return
	}
	// The rate-limited client queued it for retry; the rest of the queue still goes.
	slog.Info("queue: a delivery failed and was queued for retry", "kind", kind)
}

// queueEnv is the per-flush settings snapshot shared by every item.
type queueEnv struct {
	senderID, targetID int64
	targetUID          string
	seen, wpp          bool
	targetCids         []string
	customTag          string
	audioTag           string
	sig                string
	donation           bool
	donationLink       string
}

// queueKeyboard builds the action keyboard for a delivered item, exactly like the
// immediate path: reply (+ seen), optionally the "sent with link N" row, and the
// donation row when an admin enabled it.
func (b *Bot) queueKeyboard(env queueEnv, msg *gotgbot.Message, targetCid string) (gotgbot.InlineKeyboardMarkup, error) {
	midStr := strconv.FormatInt(msg.MessageId, 10)
	tokenMid, err := b.Tokens.Seal(env.senderID, midAAD(midStr))
	if err != nil {
		return gotgbot.InlineKeyboardMarkup{}, err
	}
	tokenBlock, err := b.Tokens.Seal(env.senderID, blockAAD())
	if err != nil {
		return gotgbot.InlineKeyboardMarkup{}, err
	}
	keyboard := messageKeyboard(tokenMid, tokenBlock, msg.MessageId, env.seen)
	if len(env.targetCids) > 1 && targetCid != "" {
		if idx := indexOf(env.targetCids, targetCid); idx >= 0 {
			keyboard = append(keyboard, []gotgbot.InlineKeyboardButton{
				cb(fmt.Sprintf("ارسال شده با لینک %d (%s)", idx+1, targetCid), "no-callback"),
			})
		}
	}
	if env.donation {
		keyboard = append(keyboard, donationRow(env.donationLink))
	}
	return gotgbot.InlineKeyboardMarkup{InlineKeyboard: keyboard}, nil
}

// queueReplyTarget converts a queued reply context into CopyMessage reply
// parameters, or an inline link to the external message when the bot cannot reply
// there (not an admin) — mirroring the immediate path's fallback.
func (b *Bot) queueReplyTarget(env queueEnv, r batchReply) (*gotgbot.ReplyParameters, string) {
	if r.targetMid != "" {
		mid, err := strconv.ParseInt(r.targetMid, 10, 64)
		if err != nil {
			return nil, ""
		}
		rp := &gotgbot.ReplyParameters{MessageId: mid, ChatId: env.targetID}
		if r.quoteText != "" {
			rp.Quote = r.quoteText
			rp.QuotePosition = r.quotePos
		}
		return rp, ""
	}
	if r.externalChat != 0 {
		if _, err := b.TG.GetChatAdministrators(r.externalChat, nil); err != nil {
			return nil, externalMessageLink(r.externalChat, r.externalMid, r.externalUsername)
		}
		rp := &gotgbot.ReplyParameters{MessageId: r.externalMid, ChatId: r.externalChat}
		if r.quoteText != "" {
			rp.Quote = r.quoteText
			rp.QuotePosition = r.quotePos
		}
		return rp, ""
	}
	return nil, ""
}

// externalMessageLink renders a t.me link to a channel message (fallback when the
// bot is not an admin there and therefore cannot reply in place).
func externalMessageLink(chatID, mid int64, username string) string {
	if mid == 0 {
		return ""
	}
	if username != "" {
		return "https://t.me/" + username + "/" + strconv.FormatInt(mid, 10)
	}
	s := strconv.FormatInt(chatID, 10)
	if len(s) > 4 && s[:4] == "-100" {
		return "https://t.me/c/" + s[4:] + "/" + strconv.FormatInt(mid, 10)
	}
	return ""
}

// copyQueuedSingle copies one buffered message to the recipient: reply context,
// keyboard, link-preview preference, tags and the sender's anonymous signature.
func (b *Bot) copyQueuedSingle(env queueEnv, u queueUnit) (int64, error) {
	msg := u.msgs[0]
	replyMarkup, err := b.queueKeyboard(env, msg, u.targetCid)
	if err != nil {
		return 0, err
	}
	opts := &gotgbot.CopyMessageOpts{ParseMode: "HTML", ReplyMarkup: replyMarkup}
	repliedToLink := ""
	if rp, link := b.queueReplyTarget(env, u.reply); rp != nil || link != "" {
		if link != "" {
			repliedToLink = link
		} else {
			opts.ReplyParameters = rp
		}
	}
	mi, err := msg.Copy(b.TG, env.targetID, opts)
	if err != nil {
		return 0, err
	}
	copiedID := mi.MessageId

	tagPreview := msg.LinkPreviewOptions
	if !env.wpp && msg.Text != "" {
		preview := &gotgbot.LinkPreviewOptions{IsDisabled: true}
		_, _, _ = b.TG.EditMessageText(msg.OriginalHTML(), &gotgbot.EditMessageTextOpts{
			ChatId:             env.targetID,
			MessageId:          copiedID,
			ParseMode:          "HTML",
			ReplyMarkup:        replyMarkup,
			LinkPreviewOptions: preview,
		})
		tagPreview = preview
	}

	footer := b.queueFooter(env, msg, repliedToLink)
	if footer != "" {
		if !b.addTag(msg, "text", env.targetID, copiedID, replyMarkup, footer, tagPreview) {
			b.addTag(msg, "caption", env.targetID, copiedID, replyMarkup, footer, tagPreview)
		}
	}
	return copiedID, nil
}

// queueFooter renders the caption/text footer for an item: the receiver's tag
// (custom/audio), the channel-reply fallback link, plus the sender's signature.
// Returns "" when there is nothing to add, so an untagged message stays untouched.
func (b *Bot) queueFooter(env queueEnv, msg *gotgbot.Message, repliedToLink string) string {
	base := ""
	switch {
	case msg.Audio != nil && env.customTag == "":
		base = sanitizeUserHTML(env.audioTag)
	case env.customTag != "":
		base = sanitizeUserHTML(env.customTag)
	}
	link := ""
	if repliedToLink != "" {
		link = `<blockquote><a href="` + repliedToLink + `">ریپلای به این پیام</a></blockquote>`
	}
	if base == "" && link == "" && env.sig == "" {
		return ""
	}
	if base != "" {
		base += "\n"
	}
	return withSig(base+link, env.sig)
}

// deliverQueuedAlbum copies one complete gallery in a SINGLE CopyMessages call and
// attaches the action keyboard through a carrier message (albums cannot carry
// per-message markup). Returns the copied ids and the number of messages delivered.
func (b *Bot) deliverQueuedAlbum(env queueEnv, u queueUnit) ([]string, int, error) {
	msgs := dedupeSortMsgs(u.msgs)
	if len(msgs) == 0 {
		return nil, 0, nil
	}
	// A "gallery" of one item is just a message.
	if len(msgs) == 1 {
		id, err := b.copyQueuedSingle(env, queueUnit{kind: unitSingle, msgs: msgs, targetCid: u.targetCid, reply: u.reply})
		if err != nil {
			return nil, 0, err
		}
		return []string{strconv.FormatInt(id, 10)}, 1, nil
	}

	srcIDs := make([]int64, 0, len(msgs))
	for _, m := range msgs {
		srcIDs = append(srcIDs, m.MessageId)
	}
	sent, err := b.TG.CopyMessages(env.targetID, env.senderID, srcIDs, nil)
	if err != nil {
		return nil, 0, err
	}
	keyboard, err := b.queueKeyboard(env, msgs[0], u.targetCid)
	if err != nil {
		return nil, 0, err
	}

	// Carrier message: carries the keyboard and the reply context (CopyMessages has
	// no reply parameters of its own).
	carrierOpts := &gotgbot.SendMessageOpts{ParseMode: "HTML", ReplyMarkup: keyboard}
	repliedToLink := ""
	if rp, link := b.queueReplyTarget(env, u.reply); rp != nil || link != "" {
		if link != "" {
			repliedToLink = link
		} else {
			carrierOpts.ReplyParameters = rp
		}
	}
	if carrierOpts.ReplyParameters == nil && len(sent) > 0 {
		carrierOpts.ReplyParameters = &gotgbot.ReplyParameters{MessageId: sent[0].MessageId, ChatId: env.targetID}
	}
	carrierText := txtMediaUseThis
	if repliedToLink != "" {
		carrierText = `<blockquote><a href="` + repliedToLink + `">ریپلای به این پیام</a></blockquote>` + "\n" + carrierText
	}
	carrier, err := b.TG.SendMessage(env.targetID, carrierText, carrierOpts)
	if err != nil {
		// The gallery itself was delivered; a missing carrier only loses the buttons.
		slog.Warn("queue: gallery delivered but its keyboard carrier failed", "err", err)
	}

	// Caption tags (audio album -> audio tag, otherwise custom tag) + signature.
	tag := env.customTag
	mt := mediaType(msgs[0])
	if mt == "audio" {
		tag = env.audioTag
	}
	footer := withSig(sanitizeUserHTML(tag), env.sig)
	if footer == "" && repliedToLink != "" {
		footer = `<blockquote><a href="` + repliedToLink + `">ریپلای به این پیام</a></blockquote>`
	}
	if footer != "" {
		for _, idx := range selectTagTargets(mt, msgs) {
			if idx < len(sent) {
				b.addTag(msgs[idx], "caption", env.targetID, sent[idx].MessageId, keyboard, footer, nil)
			}
		}
	}

	ids := make([]string, 0, len(sent)+1)
	for _, sm := range sent {
		ids = append(ids, strconv.FormatInt(sm.MessageId, 10))
	}
	if carrier != nil {
		ids = append(ids, strconv.FormatInt(carrier.MessageId, 10))
	}
	return ids, len(msgs), nil
}

// storeUnitsInBox persists a whole queue into the recipient's Message Box and sends
// ONE notification with the total pending count — an album plus singles in the same
// burst is one notification, not one per item.
func (b *Bot) storeUnitsInBox(dbctx context.Context, key convKey, units []queueUnit) error {
	var items []boxIncoming
	for _, u := range units {
		msgs := dedupeSortMsgs(u.msgs)
		for i, msg := range msgs {
			it := boxIncoming{msg: msg, targetCid: u.targetCid}
			if u.reply.targetMid != "" {
				it.replyToChatID = parseUIDOrZero(key.targetUID)
				it.replyToMessageID = parseUIDOrZero(u.reply.targetMid)
				it.quoteText = u.reply.quoteText
				it.quotePosition = u.reply.quotePos
			}
			if u.reply.externalChat != 0 {
				it.replyToChatID = u.reply.externalChat
				it.replyToMessageID = u.reply.externalMid
				it.quoteText = u.reply.quoteText
				it.quotePosition = u.reply.quotePos
				it.externalReply = true
			}
			it.channelReply = u.reply.channel
			_ = i // reply metadata applies to the unit; keeping it on every row is harmless
			items = append(items, it)
		}
	}
	if len(items) == 0 {
		return nil
	}
	return b.storeBoxIncoming(dbctx, key.targetUID, key.senderUID, items)
}

// queueLastMessage returns the newest buffered message (for sender confirmation).
func queueLastMessage(units []queueUnit) *gotgbot.Message {
	for i := len(units) - 1; i >= 0; i-- {
		for j := len(units[i].msgs) - 1; j >= 0; j-- {
			if units[i].msgs[j] != nil {
				return units[i].msgs[j]
			}
		}
	}
	return nil
}

// sendBatchConfirmation sends the sender ONE "sent" notice for the whole queue, with
// delete buttons covering every copied message when the warning setting is on.
func (b *Bot) sendBatchConfirmation(last *gotgbot.Message, targetID int64, targetUID string, warning, extended bool, copiedIDs []string) {
	if last == nil {
		return
	}
	sentText := txtSentToThem
	timeout := config.DeletionTimeout
	if extended {
		timeout = config.DeletionTimeoutExtended
		if name, err := b.displayName(targetUID); err == nil {
			sentText = fmt.Sprintf("فرستادم به %s.\n<blockquote><b>%s</b></blockquote>\n", sanitizeUserHTML(name), txtWarnBugInner)
		}
	}
	if !warning || len(copiedIDs) == 0 {
		_, _ = last.Reply(b.TG, sentText, nil)
		return
	}
	deletionToken, err := b.Tokens.Seal(targetID, deleteAAD(copiedIDs))
	if err != nil {
		_, _ = last.Reply(b.TG, sentText, nil)
		return
	}
	parts := append([]string{deletionToken}, copiedIDs...)
	rows := packDeleteButtons(parts[0], parts[1:])
	warnText := sentText + "\n" + fmt.Sprintf(config.DeletionText, timeout)
	warnMsg, err := last.Reply(b.TG, warnText, &gotgbot.SendMessageOpts{
		ParseMode:   "HTML",
		ReplyMarkup: gotgbot.InlineKeyboardMarkup{InlineKeyboard: rows},
	})
	if err != nil {
		return
	}
	b.scheduleDeleteWarning(warnMsg, timeout)
}

// flushPendingBatch delivers any buffered queue of one conversation before an
// immediate send. A nil batcher means a Bot built without New (tests), where
// buffering is simply off.
func (b *Bot) flushPendingBatch(key convKey) {
	if b.batcher == nil {
		return
	}
	b.batcher.FlushKey(key)
}

// batcherSweepLoop is the insurance path for buffered items: the debounce timer is
// the primary flush, this loop catches a queue whose timer was somehow lost.
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
