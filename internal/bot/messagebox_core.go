package bot

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/PaulSonOfLars/gotgbot/v2/ext"

	"github.com/aturzone/chevaletAnonBot/internal/db"
	"github.com/aturzone/chevaletAnonBot/internal/encoder"
)

// Message Box: an optional per-user mode in which incoming anonymous messages are
// not delivered immediately. They are persisted as delivery REFERENCES (sender
// chat + message id + reply/album metadata — never the message content) and the
// recipient gets one generic "you have new messages" notification with a button.
// Opening the box delivers everything that is pending, tells each original sender
// it was seen, and removes the rows — but only the rows whose delivery succeeded.
//
// The feature is OFF for every user until they turn it on from /settings. When it
// is off, none of this code runs on the delivery path: the pre-existing behaviour
// (deliver immediately, receiver presses "seen" themselves) is untouched.
//
// Privacy: the box stores exactly what the sender's side already needed to deliver
// (their uid, the source message id) plus the recipient uid. No message text or
// media is copied into our database. Nothing here logs a real uid.

const (
	// boxMaxPending bounds one recipient's pending queue (abuse/bloat guard). When
	// exceeded the OLDEST pending rows are dropped; the notification still tells the
	// user there is something to open.
	boxMaxPending = 500
	// boxDeliverLimit is how many rows one "open the box" pass processes. Telegram
	// rate limits make a bigger single pass pointless and slow.
	boxDeliverLimit = 50
	// boxStaleClaimAge is how old a 'delivering' claim must be before startup/open
	// treats it as abandoned by a crashed process and returns the rows to 'pending'.
	boxStaleClaimAge = 5 * time.Minute
)

const (
	txtBoxNewMessages = "📬 پیام جدید داری"
	// txtBoxNewMessagesMany is the plural form; %d is the pending count.
	txtBoxNewMessagesMany = "📬 %d پیام جدید داری"
	txtBoxEmpty           = "📭 جعبهٔ پیام‌هات خالیه."
	txtBoxPartial         = "بعضی پیام‌ها الان تحویل داده نشدن؛ بعداً دوباره با /message_box سر بزن."
	txtBoxFailed          = "الان نتونستم جعبهٔ پیام‌ها رو باز کنم؛ یه‌کم بعد دوباره امتحان کن."
	txtBoxDelivered       = "📬 %d پیام از جعبه تحویل داده شد."
	txtBoxGone            = "پیام‌هایی که توی جعبه بودن دیگه در دسترس نیستن (فرستنده پاکشون کرده)."
	txtSentToTheirBox     = "فرستادم به جعبهٔ پیام‌های مخاطبت؛ هر وقت بازشون کنه سین می‌شن."
)

// boxIncoming is one message that arrived for a recipient whose box is enabled.
// It is built at the delivery site (immediate path or batch flush) and turned into
// a single message_box row.
type boxIncoming struct {
	msg       *gotgbot.Message
	targetCid string

	replyToChatID    int64
	replyToMessageID int64
	quoteText        string
	quotePosition    int64
	externalReply    bool
	channelReply     bool
}

// boxEnabled reports whether uid currently wants the message-box behaviour. A DB
// error is treated as "off": the safe fallback in that case is the old, immediate
// delivery, never losing the message into a box we cannot read later.
func (b *Bot) boxEnabled(dbctx context.Context, uid string) bool {
	on, err := b.DB.GetMessageBox(dbctx, uid)
	if err != nil {
		slog.Warn("message box: could not read the setting; delivering directly", "err", err)
		return false
	}
	return on
}

// storeBoxIncoming persists items for recipient and sends the notification. It is
// called from the immediate delivery paths and from the batch flusher; the caller
// has already decided that the recipient's box is on.
//
// The recipient is told once (cooldown), never per message; the senders' side gets
// its own confirmation in the caller. Rows are committed BEFORE the notification is
// sent, so a crash between the two leaves the messages pending (recoverable) rather
// than announced-but-lost.
func (b *Bot) storeBoxIncoming(dbctx context.Context, recipientUID, senderUID string, items []boxIncoming) error {
	if len(items) == 0 {
		return nil
	}
	senderID, err := strconv.ParseInt(senderUID, 10, 64)
	if err != nil {
		return err
	}
	rows := make([]db.BoxMessage, 0, len(items))
	for _, it := range items {
		if it.msg == nil {
			continue
		}
		rows = append(rows, db.BoxMessage{
			RecipientUID:     recipientUID,
			SenderUID:        senderUID,
			SourceChatID:     senderID,
			SourceMessageID:  it.msg.MessageId,
			MediaGroupID:     it.msg.MediaGroupId,
			Kind:             kindOf(it.msg),
			TargetCID:        it.targetCid,
			ReplyToChatID:    it.replyToChatID,
			ReplyToMessageID: it.replyToMessageID,
			QuoteText:        it.quoteText,
			QuotePosition:    it.quotePosition,
			ExternalReply:    it.externalReply,
			ChannelReply:     it.channelReply,
		})
	}
	if len(rows) == 0 {
		return nil
	}
	if err := b.DB.AddBoxMessages(dbctx, rows); err != nil {
		return err
	}
	if dropped, err := b.DB.CapBoxMessages(dbctx, recipientUID, boxMaxPending); err != nil {
		slog.Warn("message box: could not enforce the pending cap", "err", err)
	} else if dropped > 0 {
		// No recipient uid in the log line: the count is the signal, the id is not ours.
		slog.Warn("message box: dropped the oldest pending messages over the cap", "count", dropped)
	}
	b.notifyNewBoxMessages(dbctx, recipientUID)
	return nil
}

// notifyNewBoxMessages pings the recipient that the box has content. It is called
// on EVERY store event (one per incoming message, or one per burst when batching
// grouped the burst), and both the text and the button carry the CURRENT pending
// count so the recipient can see how much is waiting without opening the box.
//
// There is deliberately no cooldown: a user with the box on who keeps receiving
// messages must keep being told. Batching is what keeps a rapid burst from becoming
// one ping per message — the whole burst is stored in one call and yields one ping.
//
// Best effort — a failed ping only costs the notification, the messages stay
// reachable through /message_box.
func (b *Bot) notifyNewBoxMessages(dbctx context.Context, recipientUID string) {
	n, err := b.DB.PendingBoxCount(dbctx, recipientUID)
	if err != nil || n == 0 {
		return
	}
	recipientID, err := strconv.ParseInt(recipientUID, 10, 64)
	if err != nil {
		return
	}
	text := txtBoxNewMessages
	if n > 1 {
		text = fmt.Sprintf(txtBoxNewMessagesMany, n)
	}
	if _, err := b.TG.SendMessage(recipientID, text, &gotgbot.SendMessageOpts{
		ParseMode:   "HTML",
		ReplyMarkup: boxNotificationMarkup(n),
	}); err != nil {
		slog.Warn("message box: notification failed", "err", err)
	}
}

// resetStaleBoxClaims returns rows abandoned by a crashed open pass to pending.
// Called once at startup.
func (b *Bot) resetStaleBoxClaims() {
	dbctx, cancel := b.bg()
	defer cancel()
	if n, err := b.DB.ResetStaleBoxClaims(dbctx, boxStaleClaimAge); err != nil {
		slog.Warn("message box: could not reset stale claims", "err", err)
	} else if n > 0 {
		slog.Info("message box: returned stale claims to pending", "count", n)
	}
}

// boxReplyParams rebuilds the reply context a stored message was delivered with.
// ChatId 0 means "the destination chat" (the recipient's own), which is how the
// original answer flow addressed a reply to the receiver's earlier message.
func boxReplyParams(row db.BoxMessage) *gotgbot.ReplyParameters {
	if row.ReplyToMessageID == 0 {
		return nil
	}
	rp := &gotgbot.ReplyParameters{MessageId: row.ReplyToMessageID}
	if row.ReplyToChatID != 0 && row.ReplyToChatID != parseUIDOrZero(row.RecipientUID) {
		rp.ChatId = row.ReplyToChatID
	}
	if row.QuoteText != "" {
		rp.Quote = row.QuoteText
		rp.QuotePosition = row.QuotePosition
	}
	return rp
}

func parseUIDOrZero(uid string) int64 {
	n, _ := strconv.ParseInt(uid, 10, 64)
	return n
}

// boxKeyboard builds the action keyboard for a message delivered FROM the box.
// It seals the SENDER's uid exactly like the immediate path does, using the
// sender-side message id as the action target (so "reply" replies to the sender's
// original message in the sender's chat). The "seen" button is deliberately
// omitted: opening the box already told the sender the message was seen.
func (b *Bot) boxKeyboard(senderUID string, sourceMessageID int64) (gotgbot.InlineKeyboardMarkup, error) {
	senderID, err := strconv.ParseInt(senderUID, 10, 64)
	if err != nil {
		return gotgbot.InlineKeyboardMarkup{}, err
	}
	midStr := strconv.FormatInt(sourceMessageID, 10)
	tokenMid, err := b.Tokens.Seal(senderID, midAAD(midStr))
	if err != nil {
		return gotgbot.InlineKeyboardMarkup{}, err
	}
	tokenBlock, err := b.Tokens.Seal(senderID, blockAAD())
	if err != nil {
		return gotgbot.InlineKeyboardMarkup{}, err
	}
	return gotgbot.InlineKeyboardMarkup{
		InlineKeyboard: messageKeyboard(tokenMid, tokenBlock, sourceMessageID, false),
	}, nil
}

// notifyBoxSeen tells the original sender that their stored message was viewed.
// It replies to the sender's OWN message (same chat), which is both the correct
// thread and what makes the notice meaningful.
func (b *Bot) notifyBoxSeen(tg *gotgbot.Bot, senderUID string, sourceMessageID int64) {
	senderID, err := strconv.ParseInt(senderUID, 10, 64)
	if err != nil {
		return
	}
	_, _ = tg.SendMessage(senderID, txtSeenSent, &gotgbot.SendMessageOpts{
		ParseMode:       "HTML",
		ReplyParameters: &gotgbot.ReplyParameters{MessageId: sourceMessageID},
	})
}

// boxDelivery is one unit delivered when the box is opened: a single message, or a
// whole media group reassembled from consecutive rows with the same media_group_id
// (so a gallery stays a gallery, exactly as on the immediate path).
type boxDelivery struct {
	rows  []db.BoxMessage
	album bool
}

// groupBoxRows splits claimed rows (ordered by id) into deliveries. Consecutive
// rows with the same non-empty media_group_id and the same sender form one album.
func groupBoxRows(rows []db.BoxMessage) []boxDelivery {
	var out []boxDelivery
	i := 0
	for i < len(rows) {
		row := rows[i]
		if row.MediaGroupID == "" {
			out = append(out, boxDelivery{rows: rows[i : i+1]})
			i++
			continue
		}
		j := i + 1
		for j < len(rows) && rows[j].MediaGroupID == row.MediaGroupID && rows[j].SenderUID == row.SenderUID {
			j++
		}
		group := append([]db.BoxMessage(nil), rows[i:j]...)
		sort.Slice(group, func(a, c int) bool { return group[a].SourceMessageID < group[c].SourceMessageID })
		out = append(out, boxDelivery{rows: group, album: len(group) > 1})
		i = j
	}
	return out
}

// deliverBoxDelivery sends one delivery unit to the recipient and reports whether it
// was delivered, permanently undeliverable, or hit a transient error. Permanent
// (message deleted / sender deactivated / bot blocked) is reported so the row can be
// dropped; transient keeps the row pending for a later pass.
func (b *Bot) deliverBoxDelivery(tg *gotgbot.Bot, recipientID int64, d boxDelivery) (delivered, permanent bool, err error) {
	first := d.rows[0]
	kb, kerr := b.boxKeyboard(first.SenderUID, first.SourceMessageID)
	if kerr != nil {
		return false, false, kerr
	}

	if d.album {
		srcIDs := make([]int64, 0, len(d.rows))
		for _, r := range d.rows {
			srcIDs = append(srcIDs, r.SourceMessageID)
		}
		copied, cerr := tg.CopyMessages(recipientID, first.SourceChatID, srcIDs, nil)
		if cerr != nil {
			return false, permanentSendFailure(cerr), cerr
		}
		if len(copied) == 0 {
			return true, false, nil
		}
		// Album copies cannot carry reply markup; send the carrier message the
		// immediate media path also uses, replying to the first copied item.
		if _, serr := tg.SendMessage(recipientID, txtMediaUseThis, &gotgbot.SendMessageOpts{
			ParseMode:       "HTML",
			ReplyParameters: &gotgbot.ReplyParameters{MessageId: copied[0].MessageId, ChatId: recipientID},
			ReplyMarkup:     kb,
		}); serr != nil {
			return true, false, nil // the album itself is delivered; the carrier is a nice-to-have
		}
		return true, false, nil
	}

	opts := &gotgbot.CopyMessageOpts{ParseMode: "HTML", ReplyMarkup: kb}
	if rp := boxReplyParams(first); rp != nil {
		opts.ReplyParameters = rp
	}
	_, cerr := tg.CopyMessage(recipientID, first.SourceChatID, first.SourceMessageID, opts)
	if cerr != nil && opts.ReplyParameters != nil && descContains(cerr, "message to be replied not found") {
		// The message we replied to is gone; deliver the content without the reply
		// context rather than losing it.
		opts.ReplyParameters = nil
		_, cerr = tg.CopyMessage(recipientID, first.SourceChatID, first.SourceMessageID, opts)
	}
	if cerr != nil {
		return false, permanentSendFailure(cerr), cerr
	}
	return true, false, nil
}

// openMessageBox is the single delivery path shared by the /message_box command,
// the notification button, and the in-chat entry point. It is idempotent: rows are
// CLAIMED in the database, delivered, and only then deleted — a second click (or a
// concurrent one) finds nothing to claim, a crash returns the claim to pending, and
// a transient failure leaves the undelivered rows pending for the next attempt.
func (b *Bot) openMessageBox(tg *gotgbot.Bot, ctx *ext.Context, userid string) error {
	dbctx, cancel := b.bg()
	defer cancel()

	if _, err := b.DB.ResetStaleBoxClaims(dbctx, boxStaleClaimAge); err != nil {
		slog.Warn("message box: stale claim reset failed", "err", err)
	}

	claim := encoder.GenerateCID(16)
	rows, err := b.DB.ClaimBoxMessages(dbctx, userid, boxDeliverLimit, claim)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return b.replyText(ctx, txtBoxEmpty)
	}

	recipientID, err := strconv.ParseInt(userid, 10, 64)
	if err != nil {
		return err
	}

	delivered, skipped := 0, 0
	transient := false
	for _, d := range groupBoxRows(rows) {
		ok, permanent, derr := b.deliverBoxDelivery(tg, recipientID, d)
		if !ok {
			ids := boxRowIDs(d.rows)
			if permanent {
				// Nothing can ever deliver these (message deleted, sender deactivated,
				// bot blocked); drop them so they cannot wedge the queue forever.
				slog.Warn("message box: dropping undeliverable messages", "count", len(d.rows))
				_ = b.DB.DeleteBoxMessages(dbctx, ids)
				skipped += len(d.rows)
				continue
			}
			slog.Warn("message box: transient delivery failure, keeping the rows pending", "err", derr)
			transient = true
			break
		}
		// Count + remove only what actually reached the recipient. The delete is
		// per delivery, so a crash mid-pass can never re-deliver earlier groups.
		for range d.rows {
			if cerr := b.DB.CountMessage(dbctx); cerr != nil {
				slog.Warn("could not count a delivered message", "err", cerr)
			}
			delivered++
		}
		if derr := b.DB.DeleteBoxMessages(dbctx, boxRowIDs(d.rows)); derr != nil {
			slog.Error("message box: delivered but could not delete the rows", "err", derr)
			transient = true
			break
		}
		b.notifyBoxSeen(tg, d.rows[0].SenderUID, d.rows[0].SourceMessageID)
	}

	// Anything still claimed (the current + later groups) goes back to pending.
	if transient {
		if rerr := b.DB.ReleaseBoxClaim(dbctx, claim); rerr != nil {
			slog.Error("message box: could not release the claim", "err", rerr)
		}
		if delivered == 0 {
			return b.replyText(ctx, txtBoxFailed)
		}
		return b.replyText(ctx, txtBoxPartial)
	}
	if delivered == 0 {
		if skipped > 0 {
			return b.replyText(ctx, txtBoxGone)
		}
		return b.replyText(ctx, txtBoxEmpty)
	}

	// A command deserves a plain confirmation; a button tap already shows the
	// delivered messages themselves, so it stays quiet.
	if ctx.CallbackQuery == nil {
		return b.replyText(ctx, fmtBoxDelivered(delivered))
	}
	return nil
}

// fmtBoxDelivered renders the /message_box success line.
func fmtBoxDelivered(n int) string { return fmt.Sprintf(txtBoxDelivered, n) }

func boxRowIDs(rows []db.BoxMessage) []int64 {
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids
}

// quoteTextForBox / quotePositionForBox capture the sender's quoting of a reply
// target. Mirrors the immediate path's rule: only a quote WITHOUT entities is
// honoured (Telegram rejects a quote that carries formatting).
func quoteTextForBox(m *gotgbot.Message) string {
	if m == nil || m.Quote == nil || len(m.Quote.Entities) > 0 {
		return ""
	}
	return m.Quote.Text
}

func quotePositionForBox(m *gotgbot.Message) int64 {
	if m == nil || m.Quote == nil || len(m.Quote.Entities) > 0 {
		return 0
	}
	return m.Quote.Position
}

// boxReplyChat is the chat id the stored message replied to: an external
// channel/group message when present, otherwise the recipient's own chat (the
// normal "answer" case — the receiver's earlier message lives there).
func boxReplyChat(targetUID, targetMid string, externalReply *gotgbot.ExternalReplyInfo) int64 {
	if externalReply != nil && externalReply.Chat != nil {
		return externalReply.Chat.Id
	}
	if targetMid != "" {
		n, _ := strconv.ParseInt(targetUID, 10, 64)
		return n
	}
	return 0
}

// boxReplyMID is the message id the stored message replied to.
func boxReplyMID(targetMid string, externalReply *gotgbot.ExternalReplyInfo) int64 {
	if externalReply != nil {
		return externalReply.MessageId
	}
	if targetMid != "" {
		n, _ := strconv.ParseInt(targetMid, 10, 64)
		return n
	}
	return 0
}
