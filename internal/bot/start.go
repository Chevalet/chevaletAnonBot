package bot

import (
	"strconv"
	"strings"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/PaulSonOfLars/gotgbot/v2/ext"
	"github.com/PaulSonOfLars/gotgbot/v2/ext/handlers"

	"github.com/aturzone/chevaletAnonBot/internal/encoder"
)

// startCmd ports start.start_cmd: /start (help text), /start <cid> (connect to a
// target), and /start UNBLOCK-<uid> (unblock a user). It is the conversation's
// primary entry point; the connect path enters the sending state (Python 0).
func startCmd(b *Bot, tg *gotgbot.Bot, ctx *ext.Context, userid string) error {
	msg := ctx.EffectiveMessage
	splitText := strings.Fields(msg.Text)

	if len(splitText) == 1 {
		// /start with no argument and /menu are the SAME entry point: both open the
		// main menu (short text + the panel + the persistent bar). /start is the URL
		// Telegram itself offers and the first thing a new user taps, so it must not
		// land on a long help page while /menu lands on the menu. The help page is
		// still one tap away from the menu.
		if err := menuCmd(b, tg, ctx, userid); err != nil {
			return err
		}
		return handlers.EndConversation()
	}

	arg := splitText[1]
	if strings.HasPrefix(arg, "UNBLOCK-") {
		return b.startUnblock(tg, ctx, userid, arg)
	}
	// The report channel's "handle this report" link lands here. Checked before
	// startConnect so a report id is never mistaken for someone's anonymous cid.
	if strings.HasPrefix(arg, reportDeepLinkPrefix) {
		return b.handleReportCase(tg, ctx, userid, arg)
	}
	// Likewise the error channel's "more detail" link.
	if strings.HasPrefix(arg, errDeepLinkPrefix) {
		return b.handleErrReport(tg, ctx, userid, arg)
	}
	return b.startConnect(tg, ctx, userid, arg)
}

// startUnblock ports the /start UNBLOCK-<uid> branch.
func (b *Bot) startUnblock(tg *gotgbot.Bot, ctx *ext.Context, userid, arg string) error {
	msg := ctx.EffectiveMessage

	// Python: target_uid = arg.split("-", 1)[-1] -> everything after the first '-'.
	targetUID := ""
	if i := strings.IndexByte(arg, '-'); i >= 0 {
		targetUID = arg[i+1:]
	}
	if _, err := strconv.ParseInt(targetUID, 10, 64); err != nil || targetUID == "" {
		if e := b.replyText(ctx, txtWrongLink); e != nil {
			return e
		}
		return handlers.EndConversation()
	}

	dbctx, cancel := b.bg()
	defer cancel()

	blocked, err := b.DB.IsBlocked(dbctx, userid, targetUID)
	if err != nil {
		return err
	}
	if blocked {
		if _, err := b.DB.RemoveBlock(dbctx, userid, targetUID); err != nil {
			return err
		}
		tid, err := strconv.ParseInt(targetUID, 10, 64)
		if err != nil {
			return err
		}
		// non-nil block aad — matches how block/unblock resolve (S-0001).
		token, err := b.Tokens.Seal(tid, blockAAD())
		if err != nil {
			return err
		}
		text := "این یوزر برات آنبلاک شد:\n" + b.getUsername(targetUID) + " | " + hrefUser(targetUID, "")
		markup := gotgbot.InlineKeyboardMarkup{InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{
			cb(btnBlockAgain, "block|"+token),
		}}}
		if _, err := msg.Reply(tg, text, &gotgbot.SendMessageOpts{ParseMode: "HTML", ReplyMarkup: markup}); err != nil {
			return err
		}
	} else {
		if e := b.replyText(ctx, txtNotBlockedAnyway); e != nil {
			return e
		}
	}
	return handlers.EndConversation()
}

// startConnect ports the /start <cid> branch.
func (b *Bot) startConnect(tg *gotgbot.Bot, ctx *ext.Context, userid, targetCid string) error {
	msg := ctx.EffectiveMessage

	dbctx, cancel := b.bg()
	defer cancel()

	targetUID, err := b.DB.GetUIDByCID(dbctx, targetCid)
	if err != nil {
		return err
	}
	if targetUID == "" {
		if e := b.replyText(ctx, txtLinkDeletedOrChanged); e != nil {
			return e
		}
		return handlers.EndConversation()
	}

	targetChid, err := b.DB.GetChevaletIDByUID(dbctx, targetUID)
	if err != nil {
		return err
	}
	if targetChid == "" {
		targetChid = encoder.GenerateChevaletID()
		// set_chevaletid always succeeded in Python (its failure branch was dead
		// code); a real DB failure propagates to the central error handler.
		if err := b.DB.SetChevaletID(dbctx, targetUID, targetChid); err != nil {
			return err
		}
	}

	blocked, err := b.DB.IsBlocked(dbctx, targetUID, userid)
	if err != nil {
		return err
	}
	if blocked {
		if e := b.replyText(ctx, txtBlockedYou); e != nil {
			return e
		}
		return handlers.EndConversation()
	}

	banned, err := b.DB.IsBanned(dbctx, targetUID)
	if err != nil {
		return err
	}
	if banned {
		if e := b.replyText(ctx, txtUserBanned); e != nil {
			return e
		}
		return handlers.EndConversation()
	}

	// displayName, not GetName: the target may have renamed their Telegram account
	// since they last touched the bot, and this line is where somebody else reads
	// their name. See namesync.go — it is a no-op unless that user syncs their name
	// and the bot's copy has gone stale.
	name, err := b.displayName(targetUID)
	if err != nil {
		return err
	}

	ud := b.ud(ctx)
	ud.d.targetCid = targetCid
	ud.d.targetUID = targetUID
	ud.d.replyTo = ""

	selfPrefix := ""
	if targetUID == userid {
		selfPrefix = txtConnectSelf
	}
	// The cancel button carries targetCid, which turns this prompt into a reusable
	// anchor: once the message is sent the conversation ends, and replying to this
	// message is how the sender writes again without going back through the link
	// (connectAnchorCid resolves it). Only advertise that when the id actually fit
	// on the button.
	markup, anchored := connectCancelMarkup(targetCid)
	hint := ""
	if anchored {
		hint = txtConnectAnchorHint + "\n"
	}
	text := selfPrefix + "\nبه " + sanitizeUserHTML(name) + " وصل شدی. پیامتو بفرست\n" + hint + "\n" + txtConnectBody
	if _, err := msg.Reply(tg, text, &gotgbot.SendMessageOpts{
		ParseMode:   "HTML",
		ReplyMarkup: *markup,
	}); err != nil {
		return err
	}
	return handlers.NextConversationState(stateSending)
}
