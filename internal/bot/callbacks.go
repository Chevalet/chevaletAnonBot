package bot

import (
	"log/slog"
	"strconv"
	"strings"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/PaulSonOfLars/gotgbot/v2/ext"
	"github.com/PaulSonOfLars/gotgbot/v2/ext/handlers"

	"github.com/aturzone/chevaletAnonBot/internal/db"
	"github.com/aturzone/chevaletAnonBot/internal/encoder"
)

// This file ports the callbacks from modules/start.py:
// answer, seen, alread_seen, block, unblock, report, report_confirm_yes,
// report_confirm_no, delete_msg_clbk, cancel, cancel_all, cancel_cmd, plus the
// no_callback handler from modules/other_msgs.py.

// answer ports start.answer: the "⌨️ ارسال جواب" button. It connects the user to
// the original sender and enters the sending state (Python 0).
func answer(b *Bot, tg *gotgbot.Bot, ctx *ext.Context, userid string) error {
	clbk := ctx.CallbackQuery
	if clbk == nil || clbk.Data == "" {
		return nil
	}
	// The reverse of the settings case: someone part-way through renaming themselves
	// who taps "answer" must not have their reply captured by the settings state.
	b.dropConversations(ctx)
	if _, err := clbk.Answer(tg, nil); err != nil {
		return err
	}
	parts := strings.Split(clbk.Data, "|")
	if len(parts) < 3 {
		return nil
	}
	token, targetMid := parts[1], parts[2]
	msg := ctx.EffectiveMessage

	dbctx, cancel := b.bg()
	defer cancel()

	targetUID, ok, err := b.resolveTargetUID(dbctx, msg, token, midAAD(targetMid))
	if err != nil {
		return err
	}
	if !ok {
		return handlers.EndConversation()
	}

	blocked, err := b.DB.IsBlocked(dbctx, targetUID, userid)
	if err != nil {
		return err
	}
	if blocked {
		_, _ = clbk.Answer(tg, &gotgbot.AnswerCallbackQueryOpts{Text: txtBlockedYou, ShowAlert: true})
		return handlers.EndConversation()
	}

	ud := b.ud(ctx)
	ud.d.targetUID = targetUID
	ud.d.replyTo = targetMid

	if _, err := msg.Reply(tg, txtAnswerPrompt, &gotgbot.SendMessageOpts{
		ParseMode:   "HTML",
		ReplyMarkup: *cancelMarkup(),
	}); err != nil {
		return err
	}
	return handlers.NextConversationState(stateSending)
}

// seen ports start.seen: tells the sender their message was seen, then swaps the
// seen button to its "✅ seen done" form so it can't be triggered again.
func seen(b *Bot, tg *gotgbot.Bot, ctx *ext.Context, userid string) error {
	clbk := ctx.CallbackQuery
	if clbk == nil || clbk.Data == "" {
		return nil
	}
	parts := strings.Split(clbk.Data, "|")
	if len(parts) < 3 {
		return nil
	}
	token, targetMid := parts[1], parts[2]
	msg := ctx.EffectiveMessage

	dbctx, cancel := b.bg()
	defer cancel()

	targetUID, ok, err := b.resolveTargetUID(dbctx, msg, token, midAAD(targetMid))
	if err != nil {
		return err
	}
	if !ok {
		return handlers.EndConversation()
	}

	blocked, err := b.DB.IsBlocked(dbctx, targetUID, userid)
	if err != nil {
		return err
	}
	if blocked {
		_, _ = clbk.Answer(tg, &gotgbot.AnswerCallbackQueryOpts{Text: txtBlockedYou, ShowAlert: true})
		return handlers.EndConversation()
	}

	// send the "seen" notice to the target (handle_target_send wrapped).
	tid, err := strconv.ParseInt(targetUID, 10, 64)
	if err != nil {
		return err
	}
	mid, _ := strconv.ParseInt(targetMid, 10, 64)
	if _, serr := tg.SendMessage(tid, txtSeenSent, &gotgbot.SendMessageOpts{
		ParseMode:       "HTML",
		ReplyParameters: &gotgbot.ReplyParameters{MessageId: mid},
	}); serr != nil {
		// The Python seen_message ignored the del sentinel (the closure returned
		// it but the caller never used it), so a handled error is effectively
		// swallowed here too; an unhandled one propagates.
		if _, perr := b.classifyTargetSendErr(ctx, msg.ExternalReply != nil, serr); perr != nil {
			return perr
		}
	}

	_, _ = clbk.Answer(tg, &gotgbot.AnswerCallbackQueryOpts{Text: txtToldThemSeen})

	// rebuild the keyboard, swapping the seen button for SEEN_DONE.
	if msg.ReplyMarkup != nil {
		newKB := transformKeyboard(msg.ReplyMarkup.InlineKeyboard, func(btn gotgbot.InlineKeyboardButton) gotgbot.InlineKeyboardButton {
			if strings.HasPrefix(btn.CallbackData, "seen") {
				return cb(msgBtnSeenDone, "alread-seen")
			}
			return btn // verbatim — preserves the donation URL button (see transformKeyboard)
		})
		_, _, _ = msg.EditReplyMarkup(tg, &gotgbot.EditMessageReplyMarkupOpts{
			ReplyMarkup: gotgbot.InlineKeyboardMarkup{InlineKeyboard: newKB},
		}) // errors ignored, like the Python try/except pass
	}

	return handlers.EndConversation()
}

// alreadySeen ports start.alread_seen: a no-op popup for the SEEN_DONE button.
func alreadySeen(_ *Bot, tg *gotgbot.Bot, ctx *ext.Context, _ string) error {
	if ctx.CallbackQuery != nil {
		_, _ = ctx.CallbackQuery.Answer(tg, &gotgbot.AnswerCallbackQueryOpts{Text: txtAlreadySeenOnce})
	}
	return nil
}

// block ports start.block: the "🔒 بلاک" button.
func block(b *Bot, tg *gotgbot.Bot, ctx *ext.Context, userid string) error {
	clbk := ctx.CallbackQuery
	if clbk == nil || clbk.Data == "" {
		return nil
	}
	parts := strings.Split(clbk.Data, "|")
	if len(parts) < 2 {
		return nil
	}
	token := parts[1]
	msg := ctx.EffectiveMessage

	dbctx, cancel := b.bg()
	defer cancel()

	targetUID, ok, err := b.resolveTargetUID(dbctx, msg, token, blockAAD())
	if err != nil {
		return err
	}
	if !ok {
		return handlers.EndConversation()
	}

	swap := func() {
		if msg.ReplyMarkup == nil {
			return
		}
		newKB := transformKeyboard(msg.ReplyMarkup.InlineKeyboard, func(btn gotgbot.InlineKeyboardButton) gotgbot.InlineKeyboardButton {
			if strings.HasPrefix(btn.CallbackData, "block") {
				// Swap only the "block" PREFIX to "unblock". The Python original
				// used callback_data.replace("block","unblock") (replace-all),
				// which would corrupt the embedded chevaletid if it ever contained
				// the substring "block"; replacing just the prefix is the intended,
				// safe behaviour.
				return cb(msgBtnUnblock, "unblock|"+strings.TrimPrefix(btn.CallbackData, "block|"))
			}
			return btn // verbatim — preserves the donation URL button (see transformKeyboard)
		})
		_, _, _ = msg.EditReplyMarkup(tg, &gotgbot.EditMessageReplyMarkupOpts{
			ReplyMarkup: gotgbot.InlineKeyboardMarkup{InlineKeyboard: newKB},
		})
	}

	added, err := b.DB.AddBlock(dbctx, userid, targetUID)
	if err != nil {
		return err
	}
	if added {
		swap()
		if userid == targetUID {
			_, _ = clbk.Answer(tg, &gotgbot.AnswerCallbackQueryOpts{Text: txtBlockSelf, ShowAlert: true})
		} else {
			_, _ = clbk.Answer(tg, &gotgbot.AnswerCallbackQueryOpts{Text: txtBlockSuccess, ShowAlert: true})
		}
	} else {
		_, _ = clbk.Answer(tg, &gotgbot.AnswerCallbackQueryOpts{Text: txtAlreadyBlocked, ShowAlert: true})
		swap()
	}
	return nil
}

// unblock ports start.unblock: the "🔓 آنبلاک" button.
func unblock(b *Bot, tg *gotgbot.Bot, ctx *ext.Context, userid string) error {
	clbk := ctx.CallbackQuery
	if clbk == nil || clbk.Data == "" {
		return nil
	}
	parts := strings.Split(clbk.Data, "|")
	if len(parts) < 2 {
		return nil
	}
	token := parts[1]
	msg := ctx.EffectiveMessage

	dbctx, cancel := b.bg()
	defer cancel()

	targetUID, ok, err := b.resolveTargetUID(dbctx, msg, token, blockAAD())
	if err != nil {
		return err
	}
	if !ok {
		return handlers.EndConversation()
	}

	swap := func() {
		if msg.ReplyMarkup == nil {
			return
		}
		newKB := transformKeyboard(msg.ReplyMarkup.InlineKeyboard, func(btn gotgbot.InlineKeyboardButton) gotgbot.InlineKeyboardButton {
			if strings.HasPrefix(btn.CallbackData, "unblock") {
				// Swap only the "unblock" PREFIX (see the note in block()).
				return cb(msgBtnBlock, "block|"+strings.TrimPrefix(btn.CallbackData, "unblock|"))
			}
			return btn // verbatim — preserves the donation URL button (see transformKeyboard)
		})
		_, _, _ = msg.EditReplyMarkup(tg, &gotgbot.EditMessageReplyMarkupOpts{
			ReplyMarkup: gotgbot.InlineKeyboardMarkup{InlineKeyboard: newKB},
		})
	}

	removed, err := b.DB.RemoveBlock(dbctx, userid, targetUID)
	if err != nil {
		return err
	}
	// The Python remove_block always returned True (dead "wasn't blocked" branch);
	// RemoveBlock now reports the real affected-row count, so the "همین الانش بلاک
	// نیس" popup can finally fire when nothing was removed.
	if removed {
		swap()
		if userid == targetUID {
			_, _ = clbk.Answer(tg, &gotgbot.AnswerCallbackQueryOpts{Text: txtUnblockSelf})
		} else {
			_, _ = clbk.Answer(tg, &gotgbot.AnswerCallbackQueryOpts{Text: txtUnblockSuccess})
		}
	} else {
		_, _ = clbk.Answer(tg, &gotgbot.AnswerCallbackQueryOpts{Text: txtNotBlockedNow})
		swap()
	}
	return nil
}

// report ports start.report: shows the report confirmation prompt.
func report(b *Bot, tg *gotgbot.Bot, ctx *ext.Context, _ string) error {
	clbk := ctx.CallbackQuery
	if clbk == nil || clbk.Data == "" {
		return nil
	}
	if _, err := clbk.Answer(tg, nil); err != nil {
		return err
	}
	parts := strings.Split(clbk.Data, "|")
	if len(parts) < 3 {
		return nil
	}
	token, targetMid := parts[1], parts[2]
	msg := ctx.EffectiveMessage

	dbctx, cancel := b.bg()
	defer cancel()
	_, ok, err := b.resolveTargetUID(dbctx, msg, token, midAAD(targetMid))
	if err != nil {
		return err
	}
	if !ok {
		return handlers.EndConversation()
	}

	markup := gotgbot.InlineKeyboardMarkup{InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{
		cb(btnReportYes, "report_yes|"+token+"|"+targetMid),
		cb(btnReportNo, "report_no"),
	}}}
	_, err = msg.Reply(tg, txtReportConfirm, &gotgbot.SendMessageOpts{
		ParseMode:   "HTML",
		ReplyMarkup: markup,
	})
	return err
}

// reportConfirmYes ports start.report_confirm_yes: forwards the reported message
// to the report channel and acknowledges to the reporter.
func reportConfirmYes(b *Bot, tg *gotgbot.Bot, ctx *ext.Context, userid string) error {
	clbk := ctx.CallbackQuery
	if clbk == nil || clbk.Data == "" {
		return nil
	}
	if _, err := clbk.Answer(tg, nil); err != nil {
		return err
	}
	parts := strings.Split(clbk.Data, "|")
	if len(parts) < 3 {
		return nil
	}
	token, targetMid := parts[1], parts[2]
	msg := ctx.EffectiveMessage

	dbctx, cancel := b.bg()
	defer cancel()
	targetUID, ok, err := b.resolveTargetUID(dbctx, msg, token, midAAD(targetMid))
	if err != nil {
		return err
	}
	if !ok {
		return handlers.EndConversation()
	}

	// delete the confirmation message
	_, _ = msg.Delete(tg, nil)

	reportChatID, perr := strconv.ParseInt(b.Cfg.ReportChatID, 10, 64)
	if perr != nil {
		return perr
	}
	targetID, perr := strconv.ParseInt(targetUID, 10, 64)
	if perr != nil {
		return perr
	}
	userID64, perr := strconv.ParseInt(userid, 10, 64)
	if perr != nil {
		return perr
	}

	// report id: an opaque short token shown to the reporter (shortuuid.uuid()).
	reportID := encoder.GenerateCID(20)

	header := "id: <code>" + reportID + "</code>\n" +
		"reporter: " + b.getLinkUsername(userid) + "\n" +
		"reported: " + b.getLinkUsername(targetUID) + "\n" +
		"\n----------------\n❇️ COPY: <code>" + targetUID + "</code>\n------------\n" +
		"message:"
	// A LINK button, not callback buttons: tapping a callback button on a channel
	// post never reaches the bot (the client does not even send it — verified
	// against this very channel), so the actions live in the admin's private chat,
	// which this link opens. See handleReportCase.
	headerOpts := &gotgbot.SendMessageOpts{ParseMode: "HTML"}
	if botUser := b.TG.User.Username; botUser != "" {
		headerOpts.ReplyMarkup = ikb(row(urlBtn(
			btnHandleReport, "https://t.me/"+botUser+"?start="+reportDeepLinkPrefix+reportID)))
	}
	firstMessage, err := tg.SendMessage(reportChatID, header, headerOpts)
	if err != nil {
		return err
	}

	// Record the pair behind the report so the private-chat actions know who to act
	// on, and remember the channel message so it can be stamped once handled. A
	// failure here must not lose the report itself, which is already posted — the
	// admin can still fall back to /admin ban and /admin report add.
	if cerr := b.DB.AddReportCase(dbctx, db.ReportCase{
		ReportID:      reportID,
		ReporterID:    userid,
		ReportedID:    targetUID,
		ChannelChatID: reportChatID,
		ChannelMsgID:  firstMessage.MessageId,
	}); cerr != nil {
		slog.Error("could not record the report case; admin buttons will not work for it",
			"report_id", reportID, "err", cerr)
	}

	targetMid64, _ := strconv.ParseInt(targetMid, 10, 64)
	if _, cerr := tg.CopyMessage(reportChatID, targetID, targetMid64, &gotgbot.CopyMessageOpts{
		ReplyParameters: &gotgbot.ReplyParameters{MessageId: firstMessage.MessageId, AllowSendingWithoutReply: true},
	}); cerr != nil {
		// Fall back to forwarding the message from the reporter's own chat.
		srcMid := msg.MessageId
		if msg.ReplyToMessage != nil {
			srcMid = msg.ReplyToMessage.MessageId
		}
		if originalMsg, ferr := tg.ForwardMessage(reportChatID, userID64, srcMid, nil); ferr == nil {
			_, _ = tg.SendMessage(reportChatID, txtReportedFromTargetChat, &gotgbot.SendMessageOpts{
				ReplyParameters: &gotgbot.ReplyParameters{MessageId: originalMsg.MessageId},
			})
		}
	}

	_, err = tg.SendMessage(userID64, "ریپورت شد.\nکد پیگیری: <code>"+reportID+"</code>",
		&gotgbot.SendMessageOpts{ParseMode: "HTML"})
	if err != nil {
		return err
	}
	return handlers.EndConversation()
}

// reportConfirmNo ports start.report_confirm_no: cancels the report.
func reportConfirmNo(_ *Bot, tg *gotgbot.Bot, ctx *ext.Context, _ string) error {
	clbk := ctx.CallbackQuery
	if clbk == nil || clbk.Data == "" {
		return nil
	}
	_, _ = clbk.Answer(tg, &gotgbot.AnswerCallbackQueryOpts{Text: txtReportCancelled})
	_, _ = ctx.EffectiveMessage.Delete(tg, nil)
	return handlers.EndConversation()
}

// deleteMsgClbk ports start.delete_msg_clbk: deletes the message(s) the warning's
// "delete" button references from the target's chat, then tidies up.
func deleteMsgClbk(b *Bot, tg *gotgbot.Bot, ctx *ext.Context, _ string) error {
	clbk := ctx.CallbackQuery
	if clbk == nil || clbk.Data == "" {
		return nil
	}
	parts := strings.Split(clbk.Data, "|")
	if len(parts) < 2 {
		return nil
	}
	token := parts[1]
	msg := ctx.EffectiveMessage

	// Harvest the message ids from EVERY button (callback_data fields 2+) FIRST:
	// they are the aad the delete token was sealed under (S-0001), so a tampered
	// id set makes resolveTargetUID fail below. deleteAAD dedups + sorts, so the
	// harvest order and how the ids were packed across buttons don't matter.
	seen := map[string]struct{}{}
	var harvested []string
	if msg.ReplyMarkup != nil {
		for _, row := range msg.ReplyMarkup.InlineKeyboard {
			for _, btn := range row {
				flds := strings.Split(btn.CallbackData, "|")
				if len(flds) > 2 {
					for _, m := range flds[2:] {
						if _, dup := seen[m]; !dup {
							seen[m] = struct{}{}
							harvested = append(harvested, m)
						}
					}
				}
			}
		}
	}

	dbctx, cancel := b.bg()
	defer cancel()
	targetUID, ok, err := b.resolveTargetUID(dbctx, msg, token, deleteAAD(harvested))
	if err != nil {
		return err
	}
	if !ok {
		return handlers.EndConversation()
	}
	if targetUID == "" {
		// Python asserted target_uid; a missing one is an unexpected state.
		return handlers.EndConversation()
	}
	targetID, perr := strconv.ParseInt(targetUID, 10, 64)
	if perr != nil {
		return perr
	}

	for m := range seen {
		if mid, e := strconv.ParseInt(m, 10, 64); e == nil {
			_, _ = tg.DeleteMessage(targetID, mid, nil) // "None" and bad ids fail silently
		}
	}

	// delete the warning message itself
	_, _ = msg.Delete(tg, nil)

	// confirm to the user, quoting their original message
	if msg.ReplyToMessage != nil {
		_, _ = msg.Reply(tg, txtDeletedForThem, &gotgbot.SendMessageOpts{
			ReplyParameters: &gotgbot.ReplyParameters{
				MessageId:                msg.ReplyToMessage.MessageId,
				AllowSendingWithoutReply: true,
			},
		})
	}
	return handlers.EndConversation()
}

// cancel ports start.cancel: the CANCEL_BUTTON. Edits the prompt away and
// schedules its deletion.
func cancel(b *Bot, tg *gotgbot.Bot, ctx *ext.Context, _ string) error {
	ud := b.ud(ctx)
	ud.clear()
	msg := ctx.EffectiveMessage
	if _, _, err := msg.EditText(tg, txtCancelGone, nil); err != nil {
		// matches Python which did not guard edit_text; a benign error is
		// swallowed by handleErr.
		return err
	}
	b.scheduleDeleteMessage(msg)
	return handlers.EndConversation()
}

// cancelConnect is the connect prompt's cancel button, whose data carries the
// target's link id ("cancel|<cid>", see connectCancelMarkup).
//
// It is registered OUTSIDE the conversations and AHEAD of them, because that id
// is user-chosen text: a link renamed to something like "alread-seen" or
// "add-link" would otherwise be matched by one of the conversations' cqContains
// filters, and the button would quietly do the wrong thing. A Prefix("cancel|")
// handler registered first cannot be shadowed by any of them, whatever the cid
// spells. Plain "cancel" is untouched and still reaches the fallback below.
//
// Being outside a conversation it cannot end one by returning (topLevel swallows
// that), so it drops the pending state explicitly — the same pattern menuCallback
// uses.
func cancelConnect(b *Bot, tg *gotgbot.Bot, ctx *ext.Context, userid string) error {
	b.dropConversations(ctx)
	return cancel(b, tg, ctx, userid)
}

// cancelAll ports start.cancel_all: the conversation fallback for any stray
// message while composing.
func cancelAll(b *Bot, _ *gotgbot.Bot, ctx *ext.Context, _ string) error {
	b.ud(ctx).clear()
	if err := b.replyText(ctx, txtCancelledMidSend); err != nil {
		return err
	}
	return handlers.EndConversation()
}

// cancelCmd ports start.cancel_cmd: the /cancel fallback inside the conversation.
func cancelCmd(b *Bot, _ *gotgbot.Bot, ctx *ext.Context, _ string) error {
	b.ud(ctx).clear()
	if err := b.replyText(ctx, txtCancelledAll); err != nil {
		return err
	}
	return handlers.EndConversation()
}

// noCallback ports other_msgs.no_callback: answers the spacer / "sent with link"
// buttons with nothing.
func noCallback(_ *Bot, tg *gotgbot.Bot, ctx *ext.Context, _ string) error {
	if ctx.CallbackQuery != nil {
		_, _ = ctx.CallbackQuery.Answer(tg, nil)
	}
	return nil
}

// transformKeyboard rebuilds an inline keyboard, applying fn to every button.
// Used by seen/block/unblock to swap a single button in place while preserving
// the rest.
//
// Faithful fix: the Python list comprehensions rebuilt EVERY button as
// InlineKeyboardButton(text, callback_data=...), dropping other button types —
// so the trailing donation URL button became invalid (no url, no callback_data)
// and edit_reply_markup silently failed (caught by `except: pass`), meaning the
// seen/block/unblock button never actually changed on delivered messages (the
// DB action still happened). Here the pass-through fns return the button
// verbatim, so the swap works as intended.
func transformKeyboard(kb [][]gotgbot.InlineKeyboardButton, fn func(gotgbot.InlineKeyboardButton) gotgbot.InlineKeyboardButton) [][]gotgbot.InlineKeyboardButton {
	out := make([][]gotgbot.InlineKeyboardButton, 0, len(kb))
	for _, row := range kb {
		newRow := make([]gotgbot.InlineKeyboardButton, 0, len(row))
		for _, btn := range row {
			newRow = append(newRow, fn(btn))
		}
		out = append(out, newRow)
	}
	return out
}
