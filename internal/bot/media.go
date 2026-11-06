package bot

import (
	"log/slog"
	"sort"
	"strconv"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/PaulSonOfLars/gotgbot/v2/ext"

	"github.com/aturzone/chevaletAnonBot/internal/config"
)

// handleMedia is the top-level handler for media-group messages. The FIRST item of
// a gallery is buffered by send_msg (which records the conversation for the group);
// each subsequent item of the same group lands here and is appended to the same
// album unit in the conversation's delivery queue. The queue sends the COMPLETE
// gallery in one CopyMessages call after the debounce window — there is no
// delete-and-re-copy loop anymore.
func handleMedia(b *Bot, tg *gotgbot.Bot, ctx *ext.Context, userid string) error {
	msg := ctx.EffectiveMessage
	ud := b.ud(ctx)

	if msg.MediaGroupId != "" && ud.d.groupMsgs != nil &&
		msg.MediaGroupId == ud.d.mediaGroupID && ud.d.groupTargetUID != "" && !ud.d.groupImmediate {
		key := convKey{senderUID: userid, targetUID: ud.d.groupTargetUID}
		reply := batchReply{
			targetMid: ud.d.groupReplyTo,
			quoteText: ud.d.groupQuoteText,
			quotePos:  ud.d.groupQuotePos,
		}
		b.batcher.AddAlbumItem(key, msg.MediaGroupId, msg, ud.d.groupTargetCid, reply)
		ud.d.groupExpiration = nowSeconds() + config.ExpireAfter
		return nil
	}

	// Not part of a buffered gallery: the legacy path handles channel/external-reply
	// albums, a late item whose queue was already flushed, or an unrelated media
	// message.
	return b.handleMediaLegacy(tg, ctx, userid)
}

// handleMediaLegacy is the original immediate album path, kept only for the rare
// external/channel-reply gallery. It accumulates the group and re-copies it on each
// arrival (the old behaviour); ordinary galleries never reach it.
func (b *Bot) handleMediaLegacy(tg *gotgbot.Bot, ctx *ext.Context, userid string) error {
	msg := ctx.EffectiveMessage
	ud := b.ud(ctx)

	slog.Debug("handleMedia", "kind", kindOf(msg), "groupId", msg.MediaGroupId,
		"haveGroup", len(ud.d.groupMsgs) > 0, "match", msg.MediaGroupId == ud.d.mediaGroupID)

	// Not part of a tracked group (or a different group) -> treat as a fresh
	// auto-reply attempt, else "didn't understand".
	if len(ud.d.groupMsgs) == 0 || msg.MediaGroupId != ud.d.mediaGroupID {
		ud.clear()
		handled, err := b.checkIfAutoreply(ctx, userid)
		if err != nil {
			return err
		}
		if handled {
			return nil
		}
		return b.otherMessagesTemplate(ctx)
	}

	// Defensive: group state must be intact.
	if ud.d.groupExpiration == 0 || len(ud.d.groupMsgs) == 0 {
		return b.otherMessagesTemplate(ctx)
	}

	// Expired window -> ask the user to start fresh. Note the Python comparison
	// uses an already-future expiration (set to now+EXPIRE_AFTER), reproduced in
	// nowSeconds()/groupExpiration verbatim.
	if nowSeconds()-ud.d.groupExpiration >= config.ExpireAfter {
		ud.clear()
		// reply quoting this message in the user's own chat.
		_, err := msg.Reply(tg, txtTooLate, &gotgbot.SendMessageOpts{
			ReplyParameters: &gotgbot.ReplyParameters{
				MessageId: msg.MessageId,
				ChatId:    ctx.EffectiveChat.Id,
			},
		})
		return err
	}

	// add this media to the group
	ud.d.groupMsgs = append(ud.d.groupMsgs, msg)

	targetUID := ud.d.groupTargetUID

	dbctx, cancel := b.bg()
	defer cancel()

	targetID, err := strconv.ParseInt(targetUID, 10, 64)
	if err != nil {
		return err
	}
	userID64, err := strconv.ParseInt(userid, 10, 64)
	if err != nil {
		return err
	}
	// Album items arrive as separate, concurrently-dispatched updates; prep
	// serialises them per-user but NOT in message-id order (whichever update grabs
	// the lock first is appended first), and Telegram may even redeliver one. So
	// sort + dedupe by message id before copying — tg.CopyMessages REQUIRES the
	// ids in strictly-increasing order (else "message identifiers must be in a
	// strictly increasing order").
	msgs := dedupeSortMsgs(ud.d.groupMsgs)
	ud.d.groupMsgs = msgs
	replyMarkup := *ud.d.groupReplyMarkup

	// delete the previously-copied set, then re-copy the whole group.
	if len(ud.d.sentMedias) > 0 {
		if ids, ok := parseIDs(ud.d.sentMedias); ok {
			_, _ = tg.DeleteMessages(targetID, ids, nil)
		}
		ud.d.sentMedias = nil
	}
	srcIDs := make([]int64, 0, len(msgs))
	for _, m := range msgs {
		srcIDs = append(srcIDs, m.MessageId)
	}
	sentMessages, err := tg.CopyMessages(targetID, userID64, srcIDs, nil)
	if err != nil {
		return err
	}
	for _, sm := range sentMessages {
		ud.d.sentMedias = append(ud.d.sentMedias, strconv.FormatInt(sm.MessageId, 10))
	}
	slog.Debug("media group copied to target", "type", mediaType(msgs[0]), "items", len(sentMessages))

	// decide which message(s) to tag and with which tag.
	mt := mediaType(msgs[0])
	var tag string
	if mt == "audio" {
		if tag, err = b.DB.GetAudioTag(dbctx, targetUID); err != nil {
			return err
		}
	} else {
		if tag, err = b.DB.GetCustomTag(dbctx, targetUID); err != nil {
			return err
		}
	}

	// The SENDER's optional anonymous nickname (see sendMsgCore), combined with the
	// receiver tag into one caption footer. sig is "" unless the sender enabled it.
	anonEnabled, anonName, anonEmoji, err := b.DB.GetAnonSig(dbctx, userid)
	if err != nil {
		return err
	}
	footer := withSig(sanitizeUserHTML(tag), anonSignature(anonEnabled, anonName, anonEmoji))

	// Parity with Python handle_media: it passes the raw tag straight to add_tag,
	// and when the target has no custom tag (NULL -> Python None) the
	// `og_text_html + "\n" + tag` concatenation raises a TypeError that add_tag
	// swallows — so the album captions are left UNTOUCHED and no reply markup is
	// placed on any media item; the keyboard reaches the target only via the
	// carrier message below. Go's GetCustomTag maps NULL -> "", so we reproduce
	// that no-op by tagging only when the footer is non-empty. (audio_tag has a
	// non-null default, so audio albums are always tagged, matching Python.) A
	// sender signature adds itself to the footer, so an opted-in sender's album is
	// captioned even when the receiver set no tag — the intended new behaviour.
	if footer != "" {
		for _, idx := range selectTagTargets(mt, msgs) {
			if idx < len(ud.d.sentMedias) {
				copiedID, _ := strconv.ParseInt(ud.d.sentMedias[idx], 10, 64)
				b.addTag(msgs[idx], "caption", targetID, copiedID, replyMarkup, footer, nil)
			}
		}
	}

	// send the reply-markup carrier message (replying to the first media).
	markupMsg, err := tg.SendMessage(targetID, txtMediaUseThis, &gotgbot.SendMessageOpts{
		ParseMode:       "HTML",
		ReplyParameters: &gotgbot.ReplyParameters{MessageId: sentMessages[0].MessageId, ChatId: targetID},
		ReplyMarkup:     replyMarkup,
	})
	if err != nil {
		return err
	}
	ud.d.sentMedias = append(ud.d.sentMedias, strconv.FormatInt(markupMsg.MessageId, 10))

	// delete the previous warning message (it referenced the now-stale copies).
	if ud.d.groupWarningMsgID != "" {
		if wid, e := strconv.ParseInt(ud.d.groupWarningMsgID, 10, 64); e == nil {
			_, _ = tg.DeleteMessage(userID64, wid, nil)
		}
	}

	// build the deletion id list (sent medias + notify), de-duplicated in order.
	tbd := append([]string{}, ud.d.sentMedias...)
	if ud.d.groupNotifyMsg != nil {
		tbd = append(tbd, strconv.FormatInt(ud.d.groupNotifyMsg.MessageId, 10))
	}
	tbd = dedupeOrdered(tbd)

	// Bind the deletable id set into the token (S-0001), matching how deleteMsgClbk
	// re-derives the aad from the harvested button ids.
	deleteToken, err := b.Tokens.Seal(targetID, deleteAAD(tbd))
	if err != nil {
		return err
	}
	deletionCallbackData := deleteToken
	for _, m := range tbd {
		deletionCallbackData += "|" + m
	}
	warnMsg, err := b.warningHandle(ctx, ud.d.groupWasChannelReply, targetUID, userid, deletionCallbackData)
	if err != nil {
		return err
	}
	if warnMsg != nil {
		ud.d.groupWarningMsgID = strconv.FormatInt(warnMsg.MessageId, 10)
	}
	ud.d.groupExpiration = nowSeconds() + config.ExpireAfter
	return nil
}

// kindOf describes a message's media kind for diagnostic logging.
func kindOf(m *gotgbot.Message) string {
	switch {
	case m.Audio != nil:
		return "audio"
	case m.Voice != nil:
		return "voice"
	case len(m.Photo) > 0:
		return "photo"
	case m.Video != nil:
		return "video"
	case m.Document != nil:
		return "document"
	case m.Animation != nil:
		return "animation"
	case m.VideoNote != nil:
		return "video_note"
	case m.Sticker != nil:
		return "sticker"
	case m.Text != "":
		return "text"
	default:
		return "other"
	}
}

// mediaType returns the bot-relevant message type of m, mirroring the slices of
// telegram.helpers.effective_message_type that handle_media branches on.
func mediaType(m *gotgbot.Message) string {
	switch {
	case m.Audio != nil:
		return "audio"
	case len(m.Photo) > 0:
		return "photo"
	case m.Video != nil:
		return "video"
	default:
		return "other"
	}
}

// parseIDs converts a slice of decimal id strings to int64s; ok is false if any
// fail to parse.
func parseIDs(ss []string) ([]int64, bool) {
	out := make([]int64, 0, len(ss))
	for _, s := range ss {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// dedupeSortMsgs returns the messages sorted by ascending message id with
// duplicate ids removed — the order tg.CopyMessages requires. Album items arrive
// as concurrently-dispatched updates, so the accumulated slice can be unordered
// or hold a redelivered duplicate.
func dedupeSortMsgs(in []*gotgbot.Message) []*gotgbot.Message {
	out := make([]*gotgbot.Message, len(in))
	copy(out, in)
	sort.Slice(out, func(i, j int) bool { return out[i].MessageId < out[j].MessageId })
	dedup := out[:0]
	last := int64(-1)
	for _, m := range out {
		if m.MessageId == last {
			continue
		}
		dedup = append(dedup, m)
		last = m.MessageId
	}
	return dedup
}

// selectTagTargets returns the indices of the album items that should receive
// the tag caption, mirroring handle_media's selection: for a photo/video album,
// tag the single captioned item, ALL items when two-or-more are captioned, or the
// first item when none is captioned; for any other type (audio/document/voice/…)
// tag every item. handle_media only ever distinguishes photo/video from the rest
// (mediaType collapses everything else to "other"). Extracted from handleMedia so
// the parity-sensitive caption-count branch can be unit-tested.
func selectTagTargets(mt string, msgs []*gotgbot.Message) []int {
	if len(msgs) == 0 {
		return nil
	}
	if mt == "photo" || mt == "video" {
		var captioned []int
		for i, m := range msgs {
			if m.OriginalCaptionHTML() != "" {
				captioned = append(captioned, i)
			}
		}
		switch len(captioned) {
		case 0:
			return []int{0}
		case 1:
			return captioned
		default:
			return allIndices(len(msgs))
		}
	}
	return allIndices(len(msgs))
}

// allIndices returns [0,1,…,n-1].
func allIndices(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

// dedupeOrdered removes duplicates while preserving first-seen order.
func dedupeOrdered(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, x := range in {
		if _, ok := seen[x]; ok {
			continue
		}
		seen[x] = struct{}{}
		out = append(out, x)
	}
	return out
}
