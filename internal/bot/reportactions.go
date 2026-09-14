package bot

import (
	"errors"
	"html"
	"log/slog"
	"strconv"
	"strings"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/PaulSonOfLars/gotgbot/v2/ext"
	"github.com/PaulSonOfLars/gotgbot/v2/ext/handlers"
	"github.com/PaulSonOfLars/gotgbot/v2/ext/handlers/filters"
	msgfilters "github.com/PaulSonOfLars/gotgbot/v2/ext/handlers/filters/message"

	"github.com/aturzone/chevaletAnonBot/internal/db"
)

// Admin actions on a report: register the report, ban the reported user, or write
// to either party through the bot.
//
// WHY THESE BUTTONS ARE NOT IN THE REPORT CHANNEL:
// they were, and they did not work. Tapping a callback button on a channel post
// never reached the bot — the Telegram client did not even send it (no progress
// spinner, and zero arrivals in a handler that logs every callback before any
// check). The identical button in a private chat with the same handler works. So
// callback buttons are only used where they are proven to work: the channel post
// carries a LINK button, which needs no callback, and that link opens the admin's
// private chat where the real buttons live.
//
// A consequence worth knowing: the buttons now live in ONE admin's private chat,
// so a keyboard cannot represent shared state. "Who handled it" is therefore
// claimed in the database (db.ClaimReportCase) and stamped onto the channel
// message, which is both visible to everyone and safe against two admins acting
// at the same moment.
const (
	btnHandleReport   = "⚙️ بررسی این ریپورت"
	btnRptAccept      = "✅ ثبت ریپورت"
	btnRptBan         = "🚫 بن کردن"
	btnRptMsgReporter = "✉️ پیام به ریپورت‌کننده"
	btnRptMsgReported = "✉️ پیام به ریپورت‌شده"
)

// reportDeepLinkPrefix marks a /start payload as "open this report". It mirrors
// the existing UNBLOCK- prefix convention in start.go. The report id is
// alphanumeric (encoder.GenerateCID), so the whole payload is deep-link safe and
// well inside Telegram's 64-character limit.
const reportDeepLinkPrefix = "RPT-"

// Callback verbs, kept short because callback_data is capped at 64 bytes and each
// carries the report id plus a sealed uid token.
const (
	rptVerbAccept      = "a"
	rptVerbBan         = "b"
	rptVerbMsgReporter = "mr"
	rptVerbMsgReported = "md"
)

// composeMarker tags the prompt asking an admin for the text to send. The reply is
// matched against the message it replies TO, so a pending compose survives a
// restart — there is no in-memory state to lose. The token after it is the sealed
// target uid, so a raw id never appears even here.
const composeMarker = "ref:rpt:"

// reportActionKeyboard is shown in the admin's PRIVATE chat after they follow the
// channel's link button.
//
// Sealed tokens rather than raw uids, keeping the invariant that no real Telegram
// id goes into callback_data. A token is 31 chars: "rpt|md|" + 31 = 38 of 64.
func reportActionKeyboard(reporterToken, reportedToken string) gotgbot.InlineKeyboardMarkup {
	return ikb(
		row(
			cb(btnRptAccept, "rpt|"+rptVerbAccept+"|"+reportedToken),
			cb(btnRptBan, "rpt|"+rptVerbBan+"|"+reportedToken),
		),
		row(
			cb(btnRptMsgReporter, "rpt|"+rptVerbMsgReporter+"|"+reporterToken),
			cb(btnRptMsgReported, "rpt|"+rptVerbMsgReported+"|"+reportedToken),
		),
	)
}

// adminLabel is a short human name for the admin who acted.
func adminLabel(u gotgbot.User) string {
	if u.Username != "" {
		return "@" + u.Username
	}
	if u.FirstName != "" {
		return u.FirstName
	}
	return strconv.FormatInt(u.Id, 10)
}

// handleReportCase serves /start RPT-<id>: it shows the report and its action
// buttons in the admin's private chat. Called from startCmd.
func (b *Bot) handleReportCase(tg *gotgbot.Bot, ctx *ext.Context, userid, arg string) error {
	reportID := strings.TrimPrefix(arg, reportDeepLinkPrefix)

	// Non-admins get the ordinary "didn't understand" reply: the link is public
	// once it is in a channel, so it must not confirm that a report id exists.
	if !b.isAdmin(userid) {
		slog.Warn("report case link followed by a non-admin", "actor", userid)
		if e := b.otherMessagesTemplate(ctx); e != nil {
			return e
		}
		return handlers.EndConversation()
	}

	dbctx, cancel := b.bg()
	defer cancel()

	c, err := b.DB.GetReportCase(dbctx, reportID)
	if errors.Is(err, db.ErrNoReportCase) {
		if e := b.replyText(ctx, "این ریپورت پیدا نشد. (ممکنه قبل از اضافه شدن این قابلیت ثبت شده باشه)"); e != nil {
			return e
		}
		return handlers.EndConversation()
	}
	if err != nil {
		return err
	}

	reporterToken, rerr := b.Tokens.Seal(mustInt64(c.ReporterID), nil)
	reportedToken, derr := b.Tokens.Seal(mustInt64(c.ReportedID), nil)
	if rerr != nil || derr != nil {
		return errors.Join(rerr, derr)
	}

	body := "⚙️ <b>بررسی ریپورت</b>\n" +
		"کد: <code>" + html.EscapeString(c.ReportID) + "</code>\n\n" +
		"ریپورت‌کننده: " + b.getLinkUsername(c.ReporterID) + "\n" +
		"ریپورت‌شده: " + b.getLinkUsername(c.ReportedID)
	if c.Handled {
		body += "\n\n✅ این ریپورت قبلا توسط <b>" + html.EscapeString(c.HandledBy) +
			"</b> بررسی شده (" + html.EscapeString(rptActionLabel(c.Action)) + ")." +
			"\nهنوز می‌تونی به طرفین پیام بدی."
	}

	_, err = ctx.EffectiveMessage.Reply(tg, body, &gotgbot.SendMessageOpts{
		ParseMode:   "HTML",
		ReplyMarkup: reportActionKeyboard(reporterToken, reportedToken),
	})
	if err != nil {
		return err
	}
	return handlers.EndConversation()
}

// rptActionLabel renders a stored action for humans.
func rptActionLabel(action string) string {
	switch action {
	case "ban":
		return "بن شد"
	case "report":
		return "ریپورت ثبت شد"
	default:
		return action
	}
}

// mustInt64 parses a uid stored in the DB; 0 on failure, which simply fails to
// open later rather than acting on the wrong person.
func mustInt64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// reportAction handles the four buttons. Registered outside prep so it also works
// if a report is ever surfaced somewhere prep filters out; it does its own admin
// check either way.
func (b *Bot) reportAction(tg *gotgbot.Bot, ctx *ext.Context) error {
	clbk := ctx.CallbackQuery
	if clbk == nil || clbk.Data == "" {
		return nil
	}
	actor := strconv.FormatInt(clbk.From.Id, 10)

	fields := strings.SplitN(clbk.Data, "|", 3)
	if len(fields) != 3 {
		slog.Warn("report action refused: malformed data", "data", clbk.Data)
		_, _ = clbk.Answer(tg, &gotgbot.AnswerCallbackQueryOpts{Text: "دکمه ناشناس.", ShowAlert: true})
		return nil
	}
	verb, token := fields[1], fields[2]
	slog.Info("report action received", "actor", actor, "admin", b.isAdmin(actor), "verb", verb)

	// Authority is ADMINS, checked server-side on every tap: the deep link is
	// public once it sits in a channel, and buttons outlive admin-list changes.
	if !b.isAdmin(actor) {
		slog.Warn("report action refused: not an admin", "actor", actor)
		_, _ = clbk.Answer(tg, &gotgbot.AnswerCallbackQueryOpts{
			Text:      "فقط ادمین‌ها می‌تونن از این دکمه‌ها استفاده کنن.",
			ShowAlert: true,
		})
		return nil
	}

	uid, ok := b.Tokens.Open(token, nil)
	if !ok {
		slog.Warn("report action refused: token did not open", "verb", verb)
		_, _ = clbk.Answer(tg, &gotgbot.AnswerCallbackQueryOpts{
			Text:      "این دکمه دیگه معتبر نیست.",
			ShowAlert: true,
		})
		return nil
	}
	uidStr := strconv.FormatInt(uid, 10)

	// The report id is in the message this keyboard is attached to, which is the
	// summary handleReportCase sent.
	reportID := parseReportIDFromSummary(ctx.EffectiveMessage)

	switch verb {
	case rptVerbAccept:
		return b.rptAccept(tg, ctx, clbk, reportID, uidStr)
	case rptVerbBan:
		return b.rptBan(tg, ctx, clbk, reportID, uidStr)
	case rptVerbMsgReporter:
		return b.rptAskCompose(tg, clbk, token, "reporter")
	case rptVerbMsgReported:
		return b.rptAskCompose(tg, clbk, token, "reported")
	default:
		slog.Warn("report action refused: unknown verb", "verb", verb)
		_, _ = clbk.Answer(tg, nil)
		return nil
	}
}

// parseReportIDFromSummary pulls the report code out of the summary message.
// Empty when it cannot be found, which downgrades the action to "no claim" rather
// than refusing it — acting is more useful than a perfect audit trail.
func parseReportIDFromSummary(msg *gotgbot.Message) string {
	if msg == nil {
		return ""
	}
	const marker = "کد: "
	i := strings.Index(msg.Text, marker)
	if i < 0 {
		return ""
	}
	rest := msg.Text[i+len(marker):]
	end := len(rest)
	for j := 0; j < len(rest); j++ {
		c := rest[j]
		alnum := (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
		if !alnum {
			end = j
			break
		}
	}
	return rest[:end]
}

// claim takes the case for this admin, or explains who already has it.
func (b *Bot) claim(tg *gotgbot.Bot, clbk *gotgbot.CallbackQuery, reportID, action string) bool {
	if reportID == "" {
		return true // no case to claim; let the action through
	}
	dbctx, cancel := b.bg()
	defer cancel()

	won, holder, held, err := b.DB.ClaimReportCase(dbctx, reportID, action, adminLabel(clbk.From))
	if err != nil {
		slog.Warn("could not claim report case; allowing the action", "report_id", reportID, "err", err)
		return true
	}
	if !won {
		_, _ = clbk.Answer(tg, &gotgbot.AnswerCallbackQueryOpts{
			Text:      "این ریپورت قبلا توسط " + holder + " بررسی شده (" + rptActionLabel(held) + ").",
			ShowAlert: true,
		})
		return false
	}
	return true
}

// rptAccept records one report against the reported user, as
// `/admin report add <uid>` did by hand, and reports the new total.
func (b *Bot) rptAccept(tg *gotgbot.Bot, ctx *ext.Context, clbk *gotgbot.CallbackQuery, reportID, uid string) error {
	if !b.claim(tg, clbk, reportID, "report") {
		return nil
	}
	dbctx, cancel := b.bg()
	defer cancel()

	count, err := b.DB.AddReportID(dbctx, uid)
	if err != nil {
		return err
	}
	slog.Info("report registered by admin", "target", uid, "total", count, "actor", clbk.From.Id)
	_, _ = clbk.Answer(tg, &gotgbot.AnswerCallbackQueryOpts{
		Text:      "ثبت شد. تعداد ریپورت‌های این کاربر: " + strconv.Itoa(count),
		ShowAlert: true,
	})
	b.rptStamp(tg, ctx, reportID, "✅ ثبت شد ("+strconv.Itoa(count)+") — "+adminLabel(clbk.From))
	return nil
}

// rptBan bans the reported user and tells them — the one notification sent here,
// since they discover it anyway when the bot stops answering. A merely-registered
// report stays silent, because warning someone mostly invites a fresh account.
func (b *Bot) rptBan(tg *gotgbot.Bot, ctx *ext.Context, clbk *gotgbot.CallbackQuery, reportID, uid string) error {
	if !b.claim(tg, clbk, reportID, "ban") {
		return nil
	}
	dbctx, cancel := b.bg()
	defer cancel()

	if err := b.DB.BanAction(dbctx, uid, true); err != nil {
		return err
	}
	slog.Info("user banned by admin from report", "target", uid, "actor", clbk.From.Id)
	_, _ = clbk.Answer(tg, &gotgbot.AnswerCallbackQueryOpts{Text: "کاربر بن شد.", ShowAlert: true})

	// Best effort: a user who never started the bot, or blocked it, cannot be told
	// — that must not fail a ban which is already committed.
	if uid64, perr := strconv.ParseInt(uid, 10, 64); perr == nil {
		_, _ = tg.SendMessage(uid64, txtBannedNotice, &gotgbot.SendMessageOpts{ParseMode: "HTML"})
	}
	b.rptStamp(tg, ctx, reportID, "🚫 بن شد — "+adminLabel(clbk.From))
	return nil
}

// rptStamp records the outcome in two places: a line under the admin's own
// summary, and the ORIGINAL channel message, so everyone watching the report
// channel sees it was handled and by whom without needing the buttons to work
// there.
func (b *Bot) rptStamp(tg *gotgbot.Bot, ctx *ext.Context, reportID, label string) {
	if msg := ctx.EffectiveMessage; msg != nil {
		if _, _, err := msg.EditText(tg, msg.Text+"\n\n"+label, &gotgbot.EditMessageTextOpts{
			ReplyMarkup: gotgbot.InlineKeyboardMarkup{InlineKeyboard: msg.ReplyMarkup.InlineKeyboard},
		}); err != nil {
			slog.Warn("could not stamp the admin's summary", "err", err)
		}
	}
	if reportID == "" {
		return
	}
	dbctx, cancel := b.bg()
	defer cancel()
	c, err := b.DB.GetReportCase(dbctx, reportID)
	if err != nil || c.ChannelChatID == 0 || c.ChannelMsgID == 0 {
		return
	}
	// Relabel the channel's button with the outcome, keeping it a LINK: a callback
	// button here would be inert (channel taps never arrive) yet still look
	// tappable, and an admin may well want to reopen the case afterwards to message
	// either party. So the label carries the status and the button still works.
	if _, _, err := tg.EditMessageReplyMarkup(&gotgbot.EditMessageReplyMarkupOpts{
		ChatId:    c.ChannelChatID,
		MessageId: c.ChannelMsgID,
		ReplyMarkup: ikb(row(urlBtn(label,
			"https://t.me/"+b.TG.User.Username+"?start="+reportDeepLinkPrefix+c.ReportID))),
	}); err != nil {
		slog.Warn("could not stamp the report channel message",
			"chat", c.ChannelChatID, "msg", c.ChannelMsgID, "err", err)
	}
}

// rptAskCompose asks the admin, in their own private chat, for the text to send.
// The prompt carries the sealed target uid and the reply is matched against the
// prompt, so nothing is held in memory and a restart mid-compose is harmless.
func (b *Bot) rptAskCompose(tg *gotgbot.Bot, clbk *gotgbot.CallbackQuery, token, role string) error {
	who := "ریپورت‌کننده"
	if role == "reported" {
		who = "کاربر ریپورت‌شده"
	}

	prompt := "✉️ <b>ارسال پیام به " + who + "</b>\n\n" +
		"متن پیام رو در جواب همین پیام بنویس. پیام با اسم بات براش فرستاده میشه.\n" +
		"برای انصراف /cancel رو بفرست.\n\n" +
		"<code>" + composeMarker + role + ":" + token + "</code>"

	if _, err := tg.SendMessage(clbk.From.Id, prompt, &gotgbot.SendMessageOpts{
		ParseMode:   "HTML",
		ReplyMarkup: gotgbot.ForceReply{ForceReply: true, InputFieldPlaceholder: "متن پیام…"},
	}); err != nil {
		_, _ = clbk.Answer(tg, &gotgbot.AnswerCallbackQueryOpts{
			Text:      "نشد برات پیام خصوصی بفرستم. اول یه /start به بات بده بعد دوباره امتحان کن.",
			ShowAlert: true,
		})
		return nil
	}
	_, _ = clbk.Answer(tg, &gotgbot.AnswerCallbackQueryOpts{Text: "متن پیام رو بنویس ⤴️"})
	return nil
}

// rptComposeFilter matches an admin's reply to a compose prompt: a private text
// message replying to a bot message carrying the marker. Narrow on purpose — it is
// registered ahead of the conversations, so it must match nothing else.
func (b *Bot) rptComposeFilter(botID int64) filters.Message {
	return func(m *gotgbot.Message) bool {
		return m.Chat.Type == "private" &&
			msgfilters.Text(m) &&
			m.ReplyToMessage != nil &&
			m.ReplyToMessage.From != nil &&
			m.ReplyToMessage.From.Id == botID &&
			strings.Contains(m.ReplyToMessage.Text, composeMarker) &&
			// Never a connect prompt: that message prints the TARGET's display name,
			// so a name spelling the marker would divert their contacts' messages
			// here. See isConnectAnchor.
			!isConnectAnchor(m.ReplyToMessage)
	}
}

// rptComposeReply delivers what the admin wrote to the party the prompt names.
func rptComposeReply(b *Bot, tg *gotgbot.Bot, ctx *ext.Context, userid string) error {
	msg := ctx.EffectiveMessage
	if msg == nil || msg.ReplyToMessage == nil {
		return nil
	}
	// Re-check authority: the prompt may be old and the admin list may have changed.
	if !b.isAdmin(userid) {
		return nil
	}
	if strings.TrimSpace(msg.Text) == "/cancel" {
		return b.replyText(ctx, "لغو شد.")
	}

	role, token, ok := parseComposeRef(msg.ReplyToMessage.Text)
	if !ok {
		return nil
	}
	uid, ok := b.Tokens.Open(token, nil)
	if !ok {
		return b.replyText(ctx, "این درخواست دیگه معتبر نیست.")
	}

	body := "📩 <b>پیام از طرف پشتیبانی</b>\n\n" + html.EscapeString(msg.Text)
	if _, err := tg.SendMessage(uid, body, &gotgbot.SendMessageOpts{ParseMode: "HTML"}); err != nil {
		// A user who blocked the bot or never started it is expected here, not a bug.
		slog.Info("admin message to a report party could not be delivered", "err", err)
		return b.replyText(ctx, "نشد پیام رو بفرستم — احتمالا کاربر بات رو بلاک کرده یا هیچوقت استارت نزده.")
	}
	slog.Info("admin messaged a report party", "role", role, "actor", userid)

	who := "ریپورت‌کننده"
	if role == "reported" {
		who = "کاربر ریپورت‌شده"
	}
	return b.replyText(ctx, "✅ پیام برای "+who+" فرستاده شد.")
}

// parseComposeRef pulls the role and sealed token back out of a prompt.
//
// The token is cut at the first character that cannot appear in base64url rather
// than at whitespace. Telegram strips HTML from Message.Text (tags arrive as
// entities), so the marker ends the text cleanly in practice — but if that ever
// stopped holding, a trailing "</code>" would ride along inside the token, Open
// would fail, and the compose flow would break silently.
func parseComposeRef(promptText string) (role, token string, ok bool) {
	i := strings.Index(promptText, composeMarker)
	if i < 0 {
		return "", "", false
	}
	rest := promptText[i+len(composeMarker):]

	role, rest, found := strings.Cut(rest, ":")
	if !found || (role != "reporter" && role != "reported") {
		return "", "", false
	}
	end := len(rest)
	for j := 0; j < len(rest); j++ {
		if !isBase64URLChar(rest[j]) {
			end = j
			break
		}
	}
	token = rest[:end]
	if token == "" {
		return "", "", false
	}
	return role, token, true
}

// isBase64URLChar reports whether c can appear in a RawURLEncoding token.
func isBase64URLChar(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
		(c >= '0' && c <= '9') || c == '-' || c == '_'
}
