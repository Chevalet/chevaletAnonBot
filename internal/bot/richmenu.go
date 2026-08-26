package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// Rendering the /menu panel with Bot API 10.3 "rich messages", where the buttons
// are embedded INSIDE the message text (<tg-button>) instead of hanging under it
// as an inline keyboard.
//
// Why this is written by hand rather than with typed library calls: no gotgbot
// release implements Bot API 10.3 yet (rc.36, the newest, stops at 10.2), so
// there is no SendRichMessage method and no InputRichMessage.blocks button type
// to build. That turns out not to matter — InputRichMessage.html is parsed
// SERVER-SIDE, so the <tg-button> tags never need library support; only the
// request does. Routing it through b.TG.RequestWithContext keeps it behind the
// same BotClient as every other call, so the rate limiter, the flood pause and
// the outbox all still apply. Nothing here bypasses them.
//
// THE COMPATIBILITY CATCH, and why this is behind a runtime flag:
// a rich message carries NO fallback. Verified against the live API — the
// returned Message has no `text` and no `reply_markup`; the buttons exist only
// inside rich_message.blocks. A client too old to understand rich messages
// therefore has nothing to render, not even the button labels as plain text.
// For the main menu — the bot's whole navigation — that would strand those users
// completely. So this renderer is OFF by default and an admin turns it on with
// /admin_menurich; see dynset.MenuRichEnabled.

// richMenuMethod names, kept as constants so a typo cannot silently fall back to
// a plain send.
const (
	methodSendRichMessage = "sendRichMessage"
	methodEditMessageText = "editMessageText"
)

// richHTML converts a panel screen (HTML body + inline keyboard) into the html
// field of an InputRichMessage.
//
// The body is already HTML in the subset Telegram's classic parse_mode accepts
// (<b>, <code>, <a>, newlines). Rich HTML is a superset for those tags, but it is
// block-structured: a bare newline is not a line break, so the text is wrapped in
// <p> per line to keep the paragraphing the screens were written for.
func richHTML(text string, kb gotgbot.InlineKeyboardMarkup) string {
	var sb strings.Builder
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue // blank lines are paragraph separators, not empty paragraphs
		}
		sb.WriteString("<p>")
		sb.WriteString(line)
		sb.WriteString("</p>")
	}
	for _, r := range kb.InlineKeyboard {
		if len(r) == 0 {
			continue
		}
		sb.WriteString("<tg-button-row>")
		for _, btn := range r {
			sb.WriteString(richButton(btn))
		}
		sb.WriteString("</tg-button-row>")
	}
	return sb.String()
}

// richButton renders one inline-keyboard button as a <tg-button>.
//
// Only the button kinds the panel actually uses are translated. Anything else
// returns "" rather than a broken tag: a button that silently does nothing is
// worse than one that is absent, and the caller (richMenuSend/richMenuEdit) falls
// back to the classic keyboard whenever the API rejects what we built.
func richButton(btn gotgbot.InlineKeyboardButton) string {
	label := btn.Text
	switch {
	case btn.CallbackData != "":
		return fmt.Sprintf(`<tg-button type="callback_data" data="%s">%s</tg-button>`,
			attrEscape(btn.CallbackData), label)
	case btn.Url != "":
		return fmt.Sprintf(`<tg-button type="url" url="%s">%s</tg-button>`,
			attrEscape(btn.Url), label)
	}
	return ""
}

// attrEscape escapes a value going into an HTML attribute. callback_data is
// bot-generated (menu verbs, sealed tokens), but escaping is not optional: a
// stray quote would end the attribute early and change which button the user
// actually taps.
func attrEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		`"`, "&quot;",
		"'", "&#39;",
		"<", "&lt;",
		">", "&gt;",
	)
	return r.Replace(s)
}

// richParams builds the InputRichMessage. IsRtl is always true: every panel
// screen is Persian, and without it Telegram lays the blocks out left-to-right.
func richParams(text string, kb gotgbot.InlineKeyboardMarkup) map[string]any {
	return map[string]any{
		"is_rtl": true,
		"html":   richHTML(text, kb),
	}
}

// richMenuSend posts a panel as a rich message. It returns the new message id,
// or an error if the API refused it — the caller then falls back to a classic
// send, so a rejected rich message never costs the user their menu.
func (b *Bot) richMenuSend(ctx context.Context, chatID int64, text string, kb gotgbot.InlineKeyboardMarkup) (int64, error) {
	raw, err := b.TG.RequestWithContext(ctx, methodSendRichMessage, map[string]any{
		"chat_id":      chatID,
		"rich_message": richParams(text, kb),
	}, nil)
	if err != nil {
		return 0, err
	}
	var m struct {
		MessageId int64 `json:"message_id"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		// Delivered, but we cannot read the id back. Reported as an error so the
		// caller does not treat an unknown message as an editable panel.
		return 0, fmt.Errorf("richMenuSend: decode result: %w", err)
	}
	return m.MessageId, nil
}

// richMenuEdit replaces a panel in place with rich content. editMessageText grew
// a rich_message parameter in the same API version, so the one-screen panel keeps
// working exactly as it does with a classic keyboard.
func (b *Bot) richMenuEdit(ctx context.Context, chatID, msgID int64, text string, kb gotgbot.InlineKeyboardMarkup) error {
	_, err := b.TG.RequestWithContext(ctx, methodEditMessageText, map[string]any{
		"chat_id":      chatID,
		"message_id":   msgID,
		"rich_message": richParams(text, kb),
	}, nil)
	return err
}
