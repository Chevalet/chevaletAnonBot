package bot

// messagebox_ui.go is the Telegram-facing half of the "Message Box" feature:
// the settings page's on/off toggle, the /message_box command, and the
// notification's "open the box" button.
//
// The delivery side — holding a message instead of delivering it, claiming the
// user's pending messages and reporting the summary — lives in
// messagebox_core.go (openMessageBox). This file only wires that action to the
// UI, following the same shapes as the rest of the settings pages: the toggle is
// a toggleSpec rendered through settingsToggle (see settings.go), and the open
// button is a TOP-LEVEL callback registered ahead of the conversations so it
// works even while the user is mid-way through another flow.
//
// The feature is opt-in: the DB default is OFF, so until the user flips the
// settings toggle every message is delivered exactly as before.

import (
	"errors"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/PaulSonOfLars/gotgbot/v2/ext"
	"github.com/PaulSonOfLars/gotgbot/v2/ext/handlers"
)

const (
	// mboxSettingsPrefix is the settings page's callback prefix. The bare prefix
	// renders the page; "<prefix>activate" / "<prefix>deactivate" flip the toggle
	// (handled by settingsToggle). It must not be a prefix of (or share a
	// cqContains substring with) any conversation's filters — pinned by
	// TestMessageBoxCallbacksDoNotCollide.
	mboxSettingsPrefix = "mbox-settings|"

	// mboxOpenData is the callback data of the notification button. Registered
	// top-level in registerHandlers, before the conversations, so a tap opens the
	// box no matter what state the user's conversations are in.
	mboxOpenData = "mbox|open"
)

// The settings page's two buttons. Package-level so the tests can pin their
// callback data without driving the settingsToggle closure.
var (
	mboxActivateBtn   = cb(txtBoxBtnActivate, mboxSettingsPrefix+"activate")
	mboxDeactivateBtn = cb(txtBoxBtnDeactivate, mboxSettingsPrefix+"deactivate")
)

// boxNotificationMarkup is the keyboard that rides on the "new messages in your
// box" notification: one button that opens the box. messagebox_core.go uses this
// when it notifies a user of held messages, so the button's data and label stay
// in one place (and are pinned by TestBoxNotificationMarkup).
func boxNotificationMarkup() gotgbot.InlineKeyboardMarkup {
	return ikb(row(cb(txtBoxOpenButton, mboxOpenData)))
}

// messageBoxOpenBtn handles the notification's open button: acknowledge the tap,
// then hand off to openMessageBox (claim + deliver + summary).
//
// It is registered with b.prep(...) OUTSIDE any conversation, so it must swallow
// conversation state changes the way topLevel does — a state change returned here
// would otherwise reach the dispatcher's error hook as a spurious incident.
func messageBoxOpenBtn(b *Bot, tg *gotgbot.Bot, ctx *ext.Context, userid string) error {
	if ctx.CallbackQuery != nil {
		_, _ = ctx.CallbackQuery.Answer(tg, nil)
	}
	err := b.openMessageBox(tg, ctx, userid)
	var sc *handlers.ConversationStateChange
	if errors.As(err, &sc) {
		return nil
	}
	return err
}

// messageBoxCmd handles /message_box — the same action as tapping the
// notification button, for users who dismissed the notification. Registered via
// b.command, which already treats a conversation state change as a no-op.
func messageBoxCmd(b *Bot, tg *gotgbot.Bot, ctx *ext.Context, userid string) error {
	return b.openMessageBox(tg, ctx, userid)
}
