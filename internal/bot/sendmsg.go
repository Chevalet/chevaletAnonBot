package bot

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/PaulSonOfLars/gotgbot/v2/ext"
	"github.com/PaulSonOfLars/gotgbot/v2/ext/handlers"

	"github.com/aturzone/chevaletAnonBot/internal/config"
)

// sendOutcome is the post-delete_notify_on_END result of send_msg_template:
// either re-enter the "composing" state (Python returned 0) or end the
// conversation (Python returned ConversationHandler.END).
type sendOutcome int

const (
	outcomeEnd sendOutcome = iota
	outcomeState0
)

// toState converts the outcome into the gotgbot conversation transition.
func (o sendOutcome) toState() error {
	if o == outcomeState0 {
		return handlers.NextConversationState(stateSending)
	}
	return handlers.EndConversation()
}

// sendMsg is the state-0 message handler (start.py send_msg). It is wrapped by
// delete_notify_on_END + prep_function in Python; the delete_notify_on_END
// behaviour lives inside sendMsgTemplate here.
func sendMsg(b *Bot, _ *gotgbot.Bot, ctx *ext.Context, userid string) error {
	outcome, err := b.sendMsgTemplate(ctx, userid)
	if err != nil {
		return err
	}
	return outcome.toState()
}

// sendMsgTemplate is the delete_notify_on_END wrapper around the core send: it
// maps the del sentinels to a conversation outcome (del+0 -> state 0, del+e /
// normal -> END).
//
// Faithful subtlety: the Python delete_notify_on_END ALSO tried to delete the
// pre-send "جواب جدید:" notify (wrapper_list[0]) on failure — but
// send_msg_template's `context.user_data.clear()` runs BEFORE the notify is
// appended, and the append targets `user_data.get("wrapper_list", [])`, i.e. a
// throwaway list (the key was just cleared). So the decorator's wrapper_list is
// always empty and that deletion is dead code (the IndexError is swallowed). We
// reproduce that exactly: the notify is never deleted here.
func (b *Bot) sendMsgTemplate(ctx *ext.Context, userid string) (sendOutcome, error) {
	sentinel, err := b.sendMsgCore(ctx, userid)
	if err != nil {
		return outcomeEnd, err
	}
	if sentinel == del0 {
		return outcomeState0, nil
	}
	return outcomeEnd, nil
}

// sendMsgCore ports handler_templates.send_msg_template (the function body,
// before the decorator). It returns one of delNone(""), del0, delE: "" means a
// plain END (success or an early bail), del0/delE carry the handle_target_send
// sentinels up to the wrapper.
func (b *Bot) sendMsgCore(ctx *ext.Context, userid string) (string, error) {
	msg := ctx.EffectiveMessage
	ud := b.ud(ctx)

	targetUID := ud.d.targetUID

	// Anti-spam, checked before anything is sent. Two limits: total output from this
	// account, and how much a SINGLE recipient can be made to receive — the second
	// is the one that protects a victim, since the harassment anonymity enables is
	// aimed at one inbox.
	//
	// warn is rate-limited separately: replying to every blocked attempt would cost
	// one API call per attempt, turning the anti-spam measure into its own flood.
	// When it is false the attempt is dropped silently, which is also the right
	// answer for an automated flooder — it gets nothing back to react to.
	allowed, warn := ud.allowSendTo(targetUID)
	if !allowed {
		slog.Info("send dropped: rate-limited",
			"userid", userid, "kind", kindOf(msg), "warned", warn)
		if warn {
			_ = b.replyText(ctx, txtTooFast)
		}
		ud.clear()
		return delNone, nil
	}

	targetCid := ud.d.targetCid
	targetMid := ud.d.replyTo
	wasChannelReply := ud.d.channelReply
	externalReply := msg.ExternalReply
	mediaGroupID := msg.MediaGroupId
	slog.Info("send attempt", "kind", kindOf(msg), "group", mediaGroupID != "",
		"answer", targetMid != "", "channelReply", wasChannelReply)
	ud.clear()

	dbctx, cancel := b.bg()
	defer cancel()

	// targetUID was resolved when the send flow was set up (start/answer, via
	// resolveTargetUID); an empty one means the conversation state expired/was lost.
	if targetUID == "" {
		return delNone, b.replyHTML(ctx, txtSessionExpired, false)
	}

	// the target cid (if any) must still resolve to a user
	if targetCid != "" {
		uid, err := b.DB.GetUIDByCID(dbctx, targetCid)
		if err != nil {
			return "", err
		}
		if uid == "" {
			return delNone, b.replyHTML(ctx, txtContactChangedLink, false)
		}
	}

	// if the user pressed "answer" but replied to a DIFFERENT message, cancel.
	if targetMid != "" {
		kind, mUID, mMid, aerr := b.isAnswer(ctx, false)
		if aerr != nil {
			return "", aerr
		}
		if kind == answerMatch {
			if !(mUID == targetUID && mMid == targetMid) {
				return delNone, b.replyHTML(ctx, txtSendingToAnother, false)
			}
		}
	}

	// blocked by the target?
	blocked, err := b.DB.IsBlocked(dbctx, targetUID, userid)
	if err != nil {
		return "", err
	}
	if blocked {
		return delNone, b.replyText(ctx, txtBlockedYou)
	}

	// Seal the SENDER's uid into opaque, unlinkable tokens for the message's
	// buttons — replaces the keyless EncodeChevaletID, which was reversible by any
	// recipient once the source is public (S-0002). Two sends -> two nonces -> two
	// unlinkable tokens; only the bot's key opens them.
	//
	// Two tokens, both non-nil aad (S-0001): tokenMid is bound to THIS message's id
	// and carries answer/seen/report (so a tampered id fails Open); tokenBlock is
	// bound to the constant block aad and carries block (no id to act on). A single
	// nil-sealed token would be a mid-agnostic skeleton key — see callbackaad.go.
	senderID, perr := strconv.ParseInt(userid, 10, 64)
	if perr != nil {
		return "", perr
	}
	midStr := strconv.FormatInt(msg.MessageId, 10)
	senderTokenMid, err := b.Tokens.Seal(senderID, midAAD(midStr))
	if err != nil {
		return "", err
	}
	senderTokenBlock, err := b.Tokens.Seal(senderID, blockAAD())
	if err != nil {
		return "", err
	}

	// reply / quote calculation (strings, like the Python original; converted to
	// int64 at the gotgbot boundary in copyToTarget).
	var replyToChatStr, replyToMidStr, quoteText string
	var quotePosition int64
	if externalReply != nil && externalReply.Chat != nil {
		replyToChatStr = strconv.FormatInt(externalReply.Chat.Id, 10)
		replyToMidStr = strconv.FormatInt(externalReply.MessageId, 10)
	} else if targetMid != "" {
		replyToChatStr = targetUID
		replyToMidStr = targetMid
	}
	if q := msg.Quote; q != nil {
		// Only honour a quote with no entities — Telegram rejects a quote that
		// carries formatting (Quote_text_invalid).
		if len(q.Entities) == 0 {
			quoteText = q.Text
			quotePosition = q.Position
		}
	}

	// if the bot is not an admin of the replied-to channel, fall back to an
	// inline link to that channel message instead of a real reply.
	repliedToLink := ""
	if externalReply != nil && externalReply.Chat != nil {
		if _, e := b.TG.GetChatAdministrators(externalReply.Chat.Id, nil); e != nil {
			if un := externalReply.Chat.Username; un != "" {
				repliedToLink = "https://t.me/" + un + "/" + replyToMidStr
			} else {
				// reply_to_chat[4:] strips the "-100" supergroup/channel prefix.
				repliedToLink = "https://t.me/c/" + replyToChatStr[4:] + "/" + replyToMidStr
			}
			replyToChatStr, replyToMidStr, quoteText, quotePosition = "", "", "", 0
		}
	}

	// keyboard: reply (+ optional seen), report/spacer/block.
	seen, err := b.DB.GetSeenStatus(dbctx, userid)
	if err != nil {
		return "", err
	}
	keyboard := messageKeyboard(senderTokenMid, senderTokenBlock, msg.MessageId, seen)

	targetCids, err := b.DB.GetCIDs(dbctx, targetUID)
	if err != nil {
		return "", err
	}

	var notifyMsg *gotgbot.Message
	if len(targetCids) > 1 && targetMid == "" {
		// a brand-new message (not an answer) to a user with several links:
		// show which link it came in through.
		if idx := indexOf(targetCids, targetCid); idx >= 0 {
			keyboard = append(keyboard, []gotgbot.InlineKeyboardButton{
				cb(fmt.Sprintf("ارسال شده با لینک %d (%s)", idx+1, targetCid), "no-callback"),
			})
		}
	} else if targetMid != "" && (externalReply != nil || (mediaGroupID != "" && externalReply == nil)) {
		// answering AND replied to an external/channel message (or a media-group
		// answer): send a "new answer:" notification first and reply to IT.
		nm, sentinel, nerr := b.sendNotif(ctx, targetUID, targetMid, externalReply != nil)
		if nerr != nil {
			return "", nerr
		}
		if sentinel != delNone {
			// FIX: the Python original used notify_msg.message_id unconditionally
			// right after this send and would crash (AttributeError) if the notify
			// was rejected. Honour the del sentinel here instead.
			return sentinel, nil
		}
		notifyMsg = nm
		replyToChatStr = ""
		replyToMidStr = strconv.FormatInt(nm.MessageId, 10)
		quoteText, quotePosition = "", 0
		// (Python appended notify_msg to a throwaway wrapper_list here; see the
		// note in sendMsgTemplate — the decorator's deletion is dead code.)
	}

	// Trailing donation row — only when an admin has switched it on, since the
	// button was removed by request. Both the flag and the link are runtime
	// settings (see /admin_donate), so turning it back on needs no redeploy.
	if b.Dyn.DonationEnabled() {
		keyboard = append(keyboard, donationRow(b.Dyn.DonationLink()))
	}
	replyMarkup := gotgbot.InlineKeyboardMarkup{InlineKeyboard: keyboard}

	// media groups: stash everything and wait for the rest of the group.
	if mediaGroupID != "" {
		ud.d.mediaGroupID = mediaGroupID
		ud.d.groupMsgs = []*gotgbot.Message{msg}
		ud.d.groupExpiration = nowSeconds() + config.ExpireAfter
		// target_uid / reply_to / channel_reply were reset by ud.clear() above.
		ud.d.groupTargetUID = targetUID
		ud.d.groupWasChannelReply = wasChannelReply
		ud.d.groupNotifyMsg = notifyMsg
		ud.d.groupReplyMarkup = &replyMarkup
		ud.d.groupWarningMsgID = ""
		return delNone, nil // END
	}

	targetID, err := strconv.ParseInt(targetUID, 10, 64)
	if err != nil {
		return "", err
	}

	// copy the message to the target.
	copiedID, sentinel, err := b.copyToTarget(ctx, targetID, &replyMarkup,
		replyToChatStr, replyToMidStr, quoteText, quotePosition, externalReply != nil)
	if err != nil {
		return "", err
	}
	if sentinel != delNone {
		ud.clear()
		return sentinel, nil
	}

	// remove the link preview on the copy if the target disabled wpp.
	wpp, err := b.DB.GetWPP(dbctx, targetUID)
	if err != nil {
		return "", err
	}
	if !wpp && msg.Text != "" {
		_, _, _ = b.TG.EditMessageText(msg.OriginalHTML(), &gotgbot.EditMessageTextOpts{
			ChatId:             targetID,
			MessageId:          copiedID,
			ParseMode:          "HTML",
			ReplyMarkup:        replyMarkup,
			LinkPreviewOptions: &gotgbot.LinkPreviewOptions{IsDisabled: true},
		}) // errors ignored, like the Python try/except pass
	}

	// "sent" + the deletion warning. The callback data carries a fresh sealed token
	// of the TARGET uid (so the sender can delete their message from the target's
	// chat) plus the copied (and notify) message ids; when notify is nil the Python
	// f-string produced the literal "None", which we reproduce.
	notifyPart := "None"
	if notifyMsg != nil {
		notifyPart = strconv.FormatInt(notifyMsg.MessageId, 10)
	}
	// Bind the deletable id set into the token (S-0001): deleteMsgClbk re-derives
	// the SAME aad by harvesting these ids back from the buttons, so a tampered id
	// set fails Open. The "None" slot is kept verbatim — deleteAAD hashes it too.
	delMids := []string{strconv.FormatInt(copiedID, 10), notifyPart}
	deleteToken, err := b.Tokens.Seal(targetID, deleteAAD(delMids))
	if err != nil {
		return "", err
	}
	deletionCallbackData := deleteToken + "|" + delMids[0] + "|" + delMids[1]
	if _, err := b.warningHandle(ctx, wasChannelReply, targetUID, userid, deletionCallbackData); err != nil {
		return "", err
	}

	// reply tag, custom tag, audio tag.
	customTag, err := b.DB.GetCustomTag(dbctx, targetUID)
	if err != nil {
		return "", err
	}

	// The SENDER's optional anonymous nickname, appended as the footer's last line
	// so the recipient can recognise a consistent pseudonym with no identity leak.
	// sig is "" unless the sender enabled it, so withSig keeps every default send
	// byte-for-byte identical to before this feature.
	anonEnabled, anonName, anonEmoji, err := b.DB.GetAnonSig(dbctx, userid)
	if err != nil {
		return "", err
	}
	sig := anonSignature(anonEnabled, anonName, anonEmoji)

	if repliedToLink != "" {
		repliedToLink = `<blockquote><a href="` + repliedToLink + `">ریپلای به این پیام</a></blockquote>`
	}
	// The tag edits below re-set the copy's link-preview options, so pass the
	// target's wpp choice through: for a wpp-off target keep the preview disabled
	// (matching the wpp edit above) instead of a tag edit reviving it.
	tagPreview := msg.LinkPreviewOptions
	if !wpp {
		tagPreview = &gotgbot.LinkPreviewOptions{IsDisabled: true}
	}
	switch {
	case msg.Audio != nil && customTag == "":
		audioTag, err := b.DB.GetAudioTag(dbctx, targetUID)
		if err != nil {
			return "", err
		}
		b.addTag(msg, "caption", targetID, copiedID, replyMarkup, withSig(sanitizeUserHTML(audioTag)+"\n"+repliedToLink, sig), tagPreview)
	case customTag != "":
		ct := sanitizeUserHTML(customTag)
		body := withSig(ct+"\n"+repliedToLink, sig)
		if !b.addTag(msg, "text", targetID, copiedID, replyMarkup, body, tagPreview) {
			b.addTag(msg, "caption", targetID, copiedID, replyMarkup, body, tagPreview)
		}
	case repliedToLink != "" || sig != "":
		body := withSig(repliedToLink, sig)
		if !b.addTag(msg, "text", targetID, copiedID, replyMarkup, body, tagPreview) {
			b.addTag(msg, "caption", targetID, copiedID, replyMarkup, body, tagPreview)
		}
	}

	ud.clear()
	return delNone, nil // END
}

// sendNotif ports the @handle_target_send send_notif closure: a "new answer:"
// message to the target, replying to target_mid. Returns the message, or a del
// sentinel when the send was rejected.
func (b *Bot) sendNotif(ctx *ext.Context, targetUID, targetMid string, externalReply bool) (*gotgbot.Message, string, error) {
	tid, err := strconv.ParseInt(targetUID, 10, 64)
	if err != nil {
		return nil, delNone, err
	}
	var opts *gotgbot.SendMessageOpts
	if targetMid != "" {
		mid, _ := strconv.ParseInt(targetMid, 10, 64)
		opts = &gotgbot.SendMessageOpts{ReplyParameters: &gotgbot.ReplyParameters{MessageId: mid}}
	}
	nm, serr := b.TG.SendMessage(tid, txtNotifNewAnswer, opts)
	if serr != nil {
		sentinel, perr := b.classifyTargetSendErr(ctx, externalReply, serr)
		return nil, sentinel, perr
	}
	return nm, delNone, nil
}

// copyToTarget ports the @handle_target_send copy_msg_to_target closure. Returns
// the copied message id, or a del sentinel when the copy was rejected.
func (b *Bot) copyToTarget(ctx *ext.Context, targetID int64, markup *gotgbot.InlineKeyboardMarkup,
	replyChatStr, replyMidStr, quoteText string, quotePos int64, externalReply bool) (int64, string, error) {

	opts := &gotgbot.CopyMessageOpts{ParseMode: "HTML", ReplyMarkup: *markup}
	if replyMidStr != "" {
		rp := &gotgbot.ReplyParameters{}
		rp.MessageId, _ = strconv.ParseInt(replyMidStr, 10, 64)
		if replyChatStr != "" {
			rp.ChatId, _ = strconv.ParseInt(replyChatStr, 10, 64)
		}
		if quoteText != "" {
			rp.Quote = quoteText
			rp.QuotePosition = quotePos
		}
		opts.ReplyParameters = rp
	}
	mi, cerr := ctx.EffectiveMessage.Copy(b.TG, targetID, opts)
	if cerr != nil {
		sentinel, perr := b.classifyTargetSendErr(ctx, externalReply, cerr)
		return 0, sentinel, perr
	}
	return mi.MessageId, delNone, nil
}

// warningHandle ports handler_templates._warning_handle: it sends the "sent"
// confirmation, and (when warnings are on, or it was a channel reply) attaches
// "delete" buttons and schedules the countdown auto-edit. Returns the warning
// message (or nil when no warning was shown).
func (b *Bot) warningHandle(ctx *ext.Context, wasChannelReply bool, targetUID, userid, deletionCallbackData string) (*gotgbot.Message, error) {
	msg := ctx.EffectiveMessage
	dbctx, cancel := b.bg()
	defer cancel()

	deletionTimeout := config.DeletionTimeout
	var sentText string
	if wasChannelReply {
		deletionTimeout = config.DeletionTimeoutExtended
		// displayName rather than GetName, for the same reason as the connect
		// message: this prints somebody else's name, so it is a place a stale one
		// would show. See namesync.go.
		name, err := b.displayName(targetUID)
		if err != nil {
			return nil, err
		}
		// The bug text is a trusted constant. The display name is user-controlled,
		// so strip injected <a> links from it before embedding it (HTML parse mode)
		// — keeps the name + safe formatting, kills the phishing vector.
		sentText = fmt.Sprintf("فرستادم به %s.\n<blockquote><b>%s</b></blockquote>\n", sanitizeUserHTML(name), txtWarnBugInner)
	} else {
		sentText = txtSentToThem
	}

	// Count the delivery for the daily stats. Reached only once the message is
	// actually through, so the figure means "delivered", not "attempted". Best
	// effort: a counter must never fail a send that already succeeded.
	if cerr := b.DB.CountMessage(dbctx); cerr != nil {
		slog.Warn("could not count a delivered message", "err", cerr)
	}

	showWarning := wasChannelReply
	if !showWarning {
		w, err := b.DB.GetWarning(dbctx, userid)
		if err != nil {
			return nil, err
		}
		showWarning = w
	}

	if !showWarning {
		return nil, b.replyText(ctx, sentText)
	}

	// pack the message ids into as few "delete" buttons as fit in 64 bytes, two
	// buttons per row (the historical callback_data layout deleteMsgClbk parses).
	parts := strings.Split(deletionCallbackData, "|")
	rows := packDeleteButtons(parts[0], parts[1:])

	// Python applied `% deletion_timeout` to the whole "{sent_text}\n{DELETION_TEXT}"
	// string; we only format DELETION_TEXT so a literal '%' in the target's
	// display name can never break the formatting.
	warnText := sentText + "\n" + fmt.Sprintf(config.DeletionText, deletionTimeout)
	warnMsg, err := msg.Reply(b.TG, warnText, &gotgbot.SendMessageOpts{
		ParseMode:   "HTML",
		ReplyMarkup: gotgbot.InlineKeyboardMarkup{InlineKeyboard: rows},
	})
	if err != nil {
		return nil, err
	}
	b.scheduleDeleteWarning(warnMsg, deletionTimeout)
	return warnMsg, nil
}

// packDeleteButtons greedily packs message ids into "delete" buttons whose
// callback_data ("delete|<token>|<mid>|<mid>…") stays within Telegram's 64-byte
// limit, then groups the buttons two-per-row. Each id appears exactly once across
// the buttons, in order. Mirrors handler_templates._warning_handle's packing loop
// (the layout deleteMsgClbk re-parses), extracted so it can be unit-tested.
func packDeleteButtons(token string, mids []string) [][]gotgbot.InlineKeyboardButton {
	kbTemplate := func() []string { return []string{"delete", token} }
	isValid := func(rm []string) bool { return len(strings.Join(rm, "|")) <= 64 }

	var buttons []gotgbot.InlineKeyboardButton
	tempKB := kbTemplate()
	for _, mid := range mids {
		candidate := append(append([]string{}, tempKB...), mid)
		if isValid(candidate) {
			tempKB = candidate
		} else {
			buttons = append(buttons, cb(btnDelete, strings.Join(tempKB, "|")))
			tempKB = append(kbTemplate(), mid)
		}
	}
	if !equalStringSlices(tempKB, kbTemplate()) {
		buttons = append(buttons, cb(btnDelete, strings.Join(tempKB, "|")))
	}

	var rows [][]gotgbot.InlineKeyboardButton
	for i := 0; i < len(buttons); i += 2 {
		end := i + 2
		if end > len(buttons) {
			end = len(buttons)
		}
		rows = append(rows, buttons[i:end])
	}
	return rows
}

// addTag ports handler_templates.add_tag: it appends a tag to the copied
// message's text or caption (edit_what is "text" or "caption"). It returns true
// on success and false on any error (Python returned True / None).
//
// preview is the link-preview option for the text edit (ignored for a caption).
// Callers pass the target's effective choice so this edit does not revive a
// preview an earlier wpp edit disabled; for a wpp-on target it is just the
// message's own options, so the behaviour is unchanged.
func (b *Bot) addTag(msg *gotgbot.Message, editWhat string, targetID, copiedID int64, markup gotgbot.InlineKeyboardMarkup, tag string, preview *gotgbot.LinkPreviewOptions) bool {
	if editWhat == "caption" {
		caption := msg.OriginalCaptionHTML() + "\n" + tag
		_, _, err := b.TG.EditMessageCaption(&gotgbot.EditMessageCaptionOpts{
			Caption:               caption,
			ChatId:                targetID,
			MessageId:             copiedID,
			ParseMode:             "HTML",
			ReplyMarkup:           markup,
			ShowCaptionAboveMedia: msg.ShowCaptionAboveMedia,
		})
		return err == nil
	}
	text := msg.OriginalHTML() + "\n" + tag
	_, _, err := b.TG.EditMessageText(text, &gotgbot.EditMessageTextOpts{
		ChatId:             targetID,
		MessageId:          copiedID,
		ParseMode:          "HTML",
		ReplyMarkup:        markup,
		LinkPreviewOptions: preview,
	})
	return err == nil
}
