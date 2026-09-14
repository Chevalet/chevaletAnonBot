package bot

import (
	"html"
	"log/slog"
	"strconv"
	"strings"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/PaulSonOfLars/gotgbot/v2/ext"
	"github.com/PaulSonOfLars/gotgbot/v2/ext/handlers/filters"
	msgfilters "github.com/PaulSonOfLars/gotgbot/v2/ext/handlers/filters/message"
)

// User-submitted bug reports, reached from /menu → راهنما → گزارش باگ.
//
// That page used to only explain a Telegram quirk while being labelled "report a
// bug", which reads as an invitation to send one. It now collects them and posts
// them to REPORT_CHAT_ID, alongside the message reports admins already handle
// there.
//
// Stateless, the same way the admin compose flow is: the bot sends a ForceReply
// prompt carrying a marker, and the reply is recognised by the message it replies
// TO. Nothing is held in memory, so a restart mid-report loses nothing.
const bugMarker = "ref:bug"

// bugComposeFilter matches a reply to the bug prompt. Narrow on purpose — it is
// registered ahead of the send state, so anything looser would swallow a user's
// anonymous message.
func (b *Bot) bugComposeFilter(botID int64) filters.Message {
	return func(m *gotgbot.Message) bool {
		return m != nil && m.Chat.Type == "private" &&
			msgfilters.Text(m) &&
			m.ReplyToMessage != nil &&
			m.ReplyToMessage.From != nil &&
			m.ReplyToMessage.From.Id == botID &&
			strings.Contains(m.ReplyToMessage.Text, bugMarker) &&
			// Never a connect prompt: that message prints the TARGET's display name,
			// so a name spelling the marker would divert their contacts' messages
			// here. See isConnectAnchor.
			!isConnectAnchor(m.ReplyToMessage)
	}
}

// bugComposeReply forwards what the user wrote to the admin channel.
func bugComposeReply(b *Bot, tg *gotgbot.Bot, ctx *ext.Context, userid string) error {
	msg := ctx.EffectiveMessage
	if msg == nil {
		return nil
	}
	if strings.TrimSpace(msg.Text) == "/cancel" {
		return b.replyText(ctx, "لغو شد.")
	}
	if b.Cfg.ReportChatID == "" {
		return b.replyText(ctx, "الان امکان ثبت گزارش نیست، بعدا امتحان کن.")
	}
	chatID, perr := strconv.ParseInt(b.Cfg.ReportChatID, 10, 64)
	if perr != nil {
		return perr
	}

	// The reporter is identified so an admin can follow up — the same information
	// the channel already carries for message reports. The body is escaped because
	// it is user-authored and the channel post is HTML.
	body := "🐞 <b>گزارش باگ از کاربر</b>\n" +
		"از طرف: " + b.getLinkUsername(userid) + "\n\n" +
		html.EscapeString(msg.Text)

	if _, err := tg.SendMessage(chatID, body, &gotgbot.SendMessageOpts{ParseMode: "HTML"}); err != nil {
		slog.Warn("could not deliver a user bug report", "err", err)
		return b.replyText(ctx, "نشد گزارش رو بفرستم، چند لحظه بعد دوباره امتحان کن.")
	}
	slog.Info("user bug report delivered", "from", userid)
	return b.replyText(ctx, "✅ ممنون! گزارشت برای تیم فرستاده شد.")
}
