package bot

import (
	"log/slog"
	"regexp"
	"strings"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/PaulSonOfLars/gotgbot/v2/ext"
	msgfilters "github.com/PaulSonOfLars/gotgbot/v2/ext/handlers/filters/message"

	"github.com/aturzone/chevaletAnonBot/internal/encoder"
)

// answerKind classifies handler_templates.is_answer's three return shapes:
// Python False (not a reply), END (a reply but not a valid answer), or the
// (target_cid, target_chid, target_mid) tuple.
type answerKind int

const (
	answerNone  answerKind = iota // Python: False
	answerEnd                     // Python: END
	answerMatch                   // Python: the answer tuple
)

// otherMessagesTemplate mirrors handler_templates.other_messages_template: the
// "didn't understand" reply, quoting the triggering message.
func (b *Bot) otherMessagesTemplate(ctx *ext.Context) error {
	slog.Debug("message not understood (catch-all)", "kind", kindOf(ctx.EffectiveMessage),
		"groupId", ctx.EffectiveMessage.MediaGroupId, "isReply", ctx.EffectiveMessage.ReplyToMessage != nil)
	return b.replyText(ctx, txtNotUnderstood)
}

// isAnswer ports handler_templates.is_answer. It inspects the replied-to
// message: if that is one of the bot's delivered messages (carrying an "answer|"
// button), it resolves the target user id and the original message id.
// warnWrongReply controls the "you must reply to the anonymous message itself"
// hint, exactly as the Python `_warn_wrong_reply` flag.
func (b *Bot) isAnswer(ctx *ext.Context, warnWrongReply bool) (answerKind, string, string, error) {
	msg := ctx.EffectiveMessage
	reply := msg.ReplyToMessage
	if reply == nil {
		return answerNone, "", "", nil
	}

	if reply.ReplyMarkup != nil && reply.From != nil && reply.From.Id == b.TG.User.Id {
		for _, row := range reply.ReplyMarkup.InlineKeyboard {
			for _, button := range row {
				data := button.CallbackData
				if data != "" && strings.HasPrefix(data, "answer|") {
					parts := strings.Split(data, "|")
					if len(parts) < 3 {
						continue // malformed; bot never produces this
					}
					token, targetMid := parts[1], parts[2]
					dbctx, cancel := b.bg()
					uid, ok, err := b.resolveTargetUID(dbctx, msg, token, midAAD(targetMid))
					cancel()
					if err != nil {
						return answerNone, "", "", err
					}
					if !ok {
						return answerEnd, "", "", nil
					}
					return answerMatch, uid, targetMid, nil
				}
			}
		}
	}

	if warnWrongReply {
		if e := b.replyText(ctx, txtWrongReply); e != nil {
			return answerNone, "", "", e
		}
	}
	return answerEnd, "", "", nil
}

// channelLinkRe builds the bare-link regex t.me/<bot>?start=(cid), matching the
// Python `pattern`. The bot username is lower-cased (as in the original) and the
// caller searches it against a lower-cased haystack.
func (b *Bot) channelLinkRe() *regexp.Regexp {
	user := strings.ToLower(b.TG.User.Username)
	return regexp.MustCompile(`t\.me/` + regexp.QuoteMeta(user) + `\?start=([A-Za-z0-9_-]+)`)
}

// findCID searches the lower-cased haystack with re but returns the cid sliced
// from the ORIGINAL text, preserving the cid's case — exactly what the Python
// code did with `match.span(1)` + `text[offset:end]`.
func findCID(re *regexp.Regexp, original string) (string, bool) {
	loc := re.FindStringSubmatchIndex(strings.ToLower(original))
	if len(loc) < 4 || loc[2] < 0 {
		return "", false
	}
	return original[loc[2]:loc[3]], true
}

// findAuthorLink ports the find_author_link closure: it looks for
// "<signature> : t.me/<bot>?start=(cid)" case-insensitively in the ORIGINAL
// text and returns the original-case cid.
func (b *Bot) findAuthorLink(text, signature string) (string, bool) {
	if text == "" || signature == "" {
		return "", false
	}
	user := strings.ToLower(b.TG.User.Username)
	re := regexp.MustCompile(`(?i)` + regexp.QuoteMeta(signature) +
		`\s*:\s*(?:https?://)?t\.me/` + regexp.QuoteMeta(user) + `\?start=([A-Za-z0-9_-]+)`)
	loc := re.FindStringSubmatchIndex(text)
	if len(loc) < 4 || loc[2] < 0 {
		return "", false
	}
	return text[loc[2]:loc[3]], true
}

// authorSignature extracts external_reply.origin.author_signature (channels and
// anonymous group admins carry one), returning "" otherwise.
func authorSignature(origin gotgbot.MessageOrigin) string {
	switch o := origin.(type) {
	case gotgbot.MessageOriginChannel:
		return o.AuthorSignature
	case gotgbot.MessageOriginChat:
		return o.AuthorSignature
	default:
		return ""
	}
}

// isReplyToChannel ports handler_templates.is_reply_to_channel: when the user
// replied to a channel post, it finds the channel's anonymous /start link (in
// bio, pinned message text/caption entities, or pinned inline buttons), honours
// an author-specific link when the post had an author signature, and resolves it
// to a target. Returns answerNone (Python False, no external reply), answerEnd
// (Python END), or answerMatch with (target_cid, encoded_chid).
func (b *Bot) isReplyToChannel(ctx *ext.Context) (kind answerKind, targetCid, uid string, err error) {
	msg := ctx.EffectiveMessage
	externalReply := msg.ExternalReply
	if externalReply == nil || externalReply.Chat == nil {
		return answerNone, "", "", nil
	}

	channel, gerr := b.TG.GetChat(externalReply.Chat.Id, nil)
	if gerr != nil {
		if errForbidden(gerr) {
			// Python replied and returned False (NOT END), so the caller falls
			// through to the "didn't understand" template.
			return answerNone, "", "", b.replyText(ctx, txtChannelPrivateNotAdded)
		}
		return answerNone, "", "", gerr
	}
	bio := channel.Description
	pin := channel.PinnedMessage
	re := b.channelLinkRe()

	sig := authorSignature(externalReply.Origin)

	// author-specific link first
	if sig != "" {
		if cid, ok := b.findAuthorLink(bio, sig); ok {
			targetCid = cid
		} else if pin != nil && pin.Text != "" {
			if cid, ok := b.findAuthorLink(pin.Text, sig); ok {
				targetCid = cid
			}
		} else if pin != nil && pin.Caption != "" {
			if cid, ok := b.findAuthorLink(pin.Caption, sig); ok {
				targetCid = cid
			}
		}
	}

	// fallback to the generic link search
	if targetCid == "" {
		switch {
		case bio != "" && reMatches(re, bio):
			targetCid, _ = findCID(re, bio)
		case pin != nil && len(pin.Entities) > 0:
			targetCid = firstURLEntityCID(re, pin.ParseEntities())
		case pin != nil && len(pin.CaptionEntities) > 0:
			targetCid = firstURLEntityCID(re, pin.ParseCaptionEntities())
		case pin != nil && pin.ReplyMarkup != nil && len(pin.ReplyMarkup.InlineKeyboard) > 0:
			targetCid = firstURLButtonCID(re, pin.ReplyMarkup.InlineKeyboard)
		default:
			return answerEnd, "", "", b.replyText(ctx, txtChannelNoLinkInBioOrPin)
		}
	}

	if targetCid == "" {
		return answerEnd, "", "", b.replyText(ctx, txtNoAnonLinkFound)
	}

	dbctx, cancel := b.bg()
	defer cancel()

	targetUID, derr := b.DB.GetUIDByCID(dbctx, targetCid)
	if derr != nil {
		return answerNone, "", "", derr
	}
	if targetUID == "" {
		return answerEnd, "", "", b.replyText(ctx, txtLinkDeletedOrChanged)
	}

	// Ensure the target still has a chevaletid (mint if missing) — preserves the
	// original side effect for any legacy path that resolves by chevaletid.
	targetChid, derr := b.DB.GetChevaletIDByUID(dbctx, targetUID)
	if derr != nil {
		return answerNone, "", "", derr
	}
	if targetChid == "" {
		if derr := b.DB.SetChevaletID(dbctx, targetUID, encoder.GenerateChevaletID()); derr != nil {
			return answerNone, "", "", derr
		}
	}
	return answerMatch, targetCid, targetUID, nil
}

// reMatches reports whether re matches the lower-cased haystack (the Python code
// searched bio.lower()).
func reMatches(re *regexp.Regexp, haystack string) bool {
	return re.MatchString(strings.ToLower(haystack))
}

// firstURLEntityCID returns the cid from the first "url" entity whose text
// contains the link, mirroring the Python loop over pin.entities / caption
// entities.
func firstURLEntityCID(re *regexp.Regexp, entities []gotgbot.ParsedMessageEntity) string {
	for _, e := range entities {
		if e.Type == "url" {
			if cid, ok := findCID(re, e.Text); ok {
				return cid
			}
		}
	}
	return ""
}

// firstURLButtonCID returns the cid from the first inline button whose URL
// contains the link, mirroring the Python loop over pin.reply_markup.
func firstURLButtonCID(re *regexp.Regexp, keyboard [][]gotgbot.InlineKeyboardButton) string {
	for _, row := range keyboard {
		for _, col := range row {
			if col.Url != "" {
				if cid, ok := findCID(re, col.Url); ok {
					return cid
				}
			}
		}
	}
	return ""
}

// anchorCidOf pulls the link id off a message's cancel button, identifying it as
// a connect prompt. The id comes from the keyboard Telegram echoes back, never
// from anything a user typed.
//
// ok is false for everything else, including the answer prompt's plain "cancel"
// button — that one carries no id and is not an anchor.
func anchorCidOf(m *gotgbot.Message) (string, bool) {
	if m == nil || m.ReplyMarkup == nil {
		return "", false
	}
	for _, r := range m.ReplyMarkup.InlineKeyboard {
		for _, btn := range r {
			if cid, found := strings.CutPrefix(btn.CallbackData, connectAnchorPrefix); found && cid != "" {
				return cid, true
			}
		}
	}
	return "", false
}

// isConnectAnchor reports whether a message is a connect prompt.
//
// The two compose filters (bug report, admin report reply) are registered ahead
// of everything and claim a reply by searching the replied-to message's TEXT for
// a marker. A connect prompt prints the target's display name, which that target
// chooses — so a name containing "ref:bug" made every reply to their prompt land
// in the report channel instead of being delivered. Those filters now exclude an
// anchor, which their own prompts (ForceReply, no inline keyboard) can never be.
func isConnectAnchor(m *gotgbot.Message) bool {
	_, ok := anchorCidOf(m)
	return ok
}

// connectAnchorCid reads a target's link id back off the message this one
// replies to, when that message is a connect prompt THIS bot sent (its cancel
// button carries "cancel|<cid>", see connectCancelMarkup).
func (b *Bot) connectAnchorCid(ctx *ext.Context) (string, bool) {
	reply := ctx.EffectiveMessage.ReplyToMessage
	if reply == nil || reply.From == nil || reply.From.Id != b.TG.User.Id {
		return "", false
	}
	return anchorCidOf(reply)
}

// sendViaConnectAnchor sends this message through the link id taken off a
// connect prompt, re-resolving it exactly as a fresh /start <cid> would: same
// link-still-valid, blocked and banned checks in the same order, so a target who
// has since renamed the link, blocked the sender or been banned is as
// unreachable through the anchor as through the link itself. The anchor is a
// shortcut past the link, never past its gates.
func (b *Bot) sendViaConnectAnchor(ctx *ext.Context, userid, targetCid string) (bool, error) {
	dbctx, cancel := b.bg()
	defer cancel()

	targetUID, err := b.DB.GetUIDByCID(dbctx, targetCid)
	if err != nil {
		return false, err
	}
	if targetUID == "" {
		return true, b.replyText(ctx, txtLinkDeletedOrChanged)
	}

	// Same side effect as startConnect: mint a chevaletid if the target has none,
	// for any legacy path that still resolves by one.
	targetChid, err := b.DB.GetChevaletIDByUID(dbctx, targetUID)
	if err != nil {
		return false, err
	}
	if targetChid == "" {
		if err := b.DB.SetChevaletID(dbctx, targetUID, encoder.GenerateChevaletID()); err != nil {
			return false, err
		}
	}

	blocked, err := b.DB.IsBlocked(dbctx, targetUID, userid)
	if err != nil {
		return false, err
	}
	if blocked {
		return true, b.replyText(ctx, txtBlockedYou)
	}

	banned, err := b.DB.IsBanned(dbctx, targetUID)
	if err != nil {
		return false, err
	}
	if banned {
		return true, b.replyText(ctx, txtUserBanned)
	}

	ud := b.ud(ctx)
	ud.d.targetCid = targetCid
	ud.d.targetUID = targetUID
	ud.d.replyTo = ""
	ud.d.channelReply = false
	if _, e := b.sendMsgTemplate(ctx, userid); e != nil {
		return false, e
	}
	return true, nil
}

// checkIfAutoreply ports handler_templates.check_if_autoreply: it tries the
// private-reply path (is_answer) then the channel-reply path (is_reply_to_channel),
// and on a match stashes the target and runs send_msg_template. handled=true is
// the Python END return (something was handled); false falls through.
func (b *Bot) checkIfAutoreply(ctx *ext.Context, userid string) (handled bool, err error) {
	// A reply to a connect prompt, whose cancel button carries the target's link
	// id: the prompt doubles as a reusable anchor, so replying to it sends through
	// that link again instead of the sender having to reopen it.
	//
	// Checked BEFORE isAnswer, which sees a bot message with no "answer|" button
	// and would answer the reply with "reply to the anonymous message itself".
	//
	// Note this runs only from the catch-all (and handleMedia), i.e. only when no
	// conversation state claimed the update. That is what stops the double send:
	// while the bot is still waiting for the message, the start conversation's
	// send state has first refusal on it and this path is never reached, so a
	// reply to the prompt the sender just received is delivered exactly once.
	if targetCid, ok := b.connectAnchorCid(ctx); ok {
		// A command is a command, even in a reply. Fall through so its own handler
		// — or, for one the bot does not know, the "didn't understand" reply — runs
		// instead of the command being delivered to the target as a message.
		if msgfilters.Command(ctx.EffectiveMessage) {
			return false, nil
		}
		return b.sendViaConnectAnchor(ctx, userid, targetCid)
	}

	// private reply
	kind, targetUID, mid, err := b.isAnswer(ctx, true)
	if err != nil {
		return false, err
	}
	if kind == answerEnd {
		return true, nil
	}
	if kind == answerMatch {
		ud := b.ud(ctx)
		ud.d.targetCid = ""
		ud.d.targetUID = targetUID
		ud.d.replyTo = mid
		ud.d.channelReply = false
		if _, e := b.sendMsgTemplate(ctx, userid); e != nil {
			return false, e
		}
		return true, nil
	}

	// channel reply
	ckind, cCid, cUID, err := b.isReplyToChannel(ctx)
	if err != nil {
		return false, err
	}
	if ckind == answerEnd {
		return true, nil
	}
	if ckind == answerMatch {
		ud := b.ud(ctx)
		ud.d.targetCid = cCid
		ud.d.targetUID = cUID
		ud.d.replyTo = ""
		ud.d.channelReply = true
		if _, e := b.sendMsgTemplate(ctx, userid); e != nil {
			return false, e
		}
		// Python resets channel_reply to None after the send.
		ud.d.channelReply = false
		return true, nil
	}

	return false, nil
}
