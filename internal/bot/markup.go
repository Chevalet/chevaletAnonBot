package bot

import (
	"strconv"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// This file ports modules/Global/reply_markups.py verbatim. The callback_data
// strings are reproduced exactly because the handlers match on them.

// MSG_BTN labels (the buttons under each delivered anonymous message).
const (
	msgBtnRepliedTo = "ریپلای شده به این پیام"
	msgBtnReply     = "⌨️ ارسال جواب"
	msgBtnSeen      = "✅ سین بزن"
	msgBtnSeenDone  = "☑️ سین زدم"
	msgBtnBlock     = "🔒 بلاک"
	msgBtnUnblock   = "🔓 آنبلاک"
	msgBtnReport    = "⚠️ ریپورت"
	msgBtnOther     = "⚙️ سایر"
	msgBtnOtherBack = "↩️ بستن"
)

// cb builds a callback-data button.
func cb(text, data string) gotgbot.InlineKeyboardButton {
	return gotgbot.InlineKeyboardButton{Text: text, CallbackData: data}
}

// urlBtn builds a URL button.
func urlBtn(text, url string) gotgbot.InlineKeyboardButton {
	return gotgbot.InlineKeyboardButton{Text: text, Url: url}
}

// cancelButton mirrors reply_markups.CANCEL_BUTTON.
func cancelButton() gotgbot.InlineKeyboardButton { return cb("بیخیالش", "cancel") }

// cancelMarkup is the single-row [CANCEL_BUTTON] keyboard used by /start connect
// and the answer prompt.
func cancelMarkup() *gotgbot.InlineKeyboardMarkup {
	return &gotgbot.InlineKeyboardMarkup{
		InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{cancelButton()}},
	}
}

// messageKeyboard builds the inline keyboard placed under every delivered
// anonymous message, mirroring the first two rows of reply_markup_keyboard in
// handler_templates.send_msg_template. mid is the sender's message id. When seen
// is true the "mark as seen" button is inserted at the front of the first row,
// exactly as the Python `reply_markup_keyboard[0].insert(0, ...)`.
//
// Two sealed tokens of the sender's uid are used (both unlinkable; see
// callbackaad.go). tokenMid is bound to mid via aad and carries the id-acting
// verbs (answer/seen/report) so a tampered id fails Open — the S-0001 fix.
// tokenBlock is bound to the constant blockAAD (non-nil) and carries block, which
// acts on no message id and is reused across the block<->unblock swap.
//
// The caller appends the optional "sent with link N (cid)" row and the donation
// row, matching the order the Python code assembles them.
func messageKeyboard(tokenMid, tokenBlock string, mid int64, seen bool) [][]gotgbot.InlineKeyboardButton {
	midStr := strconv.FormatInt(mid, 10)
	firstRow := []gotgbot.InlineKeyboardButton{
		cb(msgBtnReply, "answer|"+tokenMid+"|"+midStr),
	}
	if seen {
		firstRow = append([]gotgbot.InlineKeyboardButton{
			cb(msgBtnSeen, "seen|"+tokenMid+"|"+midStr),
		}, firstRow...)
	}
	// Report and block moved behind "سایر" so a delivered message carries two rows,
	// not four: the actions people use constantly stay visible, the rarer ones are
	// one tap away. otherActions expands it in place.
	firstRow = append(firstRow, cb(msgBtnOther, "oth|"+tokenBlock))

	return [][]gotgbot.InlineKeyboardButton{firstRow}
}

// otherActionsRow is the row "سایر" reveals.
func otherActionsRow(tokenMid, tokenBlock, midStr string, blocked bool) []gotgbot.InlineKeyboardButton {
	blockBtn := cb(msgBtnBlock, "block|"+tokenBlock)
	if blocked {
		// Read from the database rather than remembered in the button, so collapsing
		// and reopening cannot show "block" to someone who has already blocked.
		blockBtn = cb(msgBtnUnblock, "unblock|"+tokenBlock)
	}
	return []gotgbot.InlineKeyboardButton{
		cb(msgBtnReport, "report|"+tokenMid+"|"+midStr),
		blockBtn,
	}
}

// donationRow mirrors the trailing donation-link button row appended to every
// delivered message's keyboard. Only appended when an admin has enabled it
// (/admin_donate active) — it is off by default.
func donationRow(donationLink string) []gotgbot.InlineKeyboardButton {
	return []gotgbot.InlineKeyboardButton{urlBtn(btnDonation, donationLink)}
}

// ikb assembles an InlineKeyboardMarkup from rows.
func ikb(rows ...[]gotgbot.InlineKeyboardButton) gotgbot.InlineKeyboardMarkup {
	return gotgbot.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// row groups buttons into one keyboard row.
func row(btns ...gotgbot.InlineKeyboardButton) []gotgbot.InlineKeyboardButton {
	return btns
}

// settingsMainMenu mirrors SETTINGS_MARKUP["main-menu-set"].
func settingsMainMenu() [][]gotgbot.InlineKeyboardButton {
	return [][]gotgbot.InlineKeyboardButton{
		{cb("⌨️ ارسال پیام بدون لینک | ریپلای به کانال", "easier-answer|")},
		{cb("✍️ ارسال به ادمین خاص کانال", "channel-signature|")},
		{cb("🔗 پیشنمایشِ لینک", "wpp|"), cb("👌 ارسال پیامهای پیوسته", "media-settings|")},
		{cb("🖋 ریپلای به بخشی از پیام", "reply-quote|"), cb("👀 نمایش دکمه سین زدن", "seen-settings|")},
		{cb("⚠️ اخطار پاک سازی پیام", "warning|"), cb("📛 تغییر نام نمایشی", "change-name|")},
		// Its own row, directly under the rename button it belongs with: the label
		// is long enough that sharing a row truncates it on a narrow screen.
		{cb("🔄 هم‌گام‌سازی نام با اکانت", "name-sync|")},
		{cb("#️⃣ تگ آهنگ", "audio-tag|"), cb("#️⃣ تگ دلخواه", "custom-tag|")},
		{cb("🎭 نام مستعار ناشناس", "anon-name|")},
		{cb("🚫 آنبلاک شدن خودت", "unblock-me|"), cb("🚫 آنبلاک همه", "unblock-all|")},
		// Settings is a page of /menu now, so it needs a way home. settingsCmd ends
		// the conversation before this is shown, so leaving via the panel strands no
		// state. Handled by menuCallback, which is registered ahead of the
		// conversations.
		{cb("🏠 برگشت به منو", "menu|"+menuMain)},
	}
}

// settingsButtons mirrors the single-button entries of SETTINGS_MARKUP.
var settingsButtons = map[string]gotgbot.InlineKeyboardButton{
	"formatting":           cb("❔قالب بندی چیه", "what-is-formatting"),
	"back-to-menu":         cb("↪️ بازگشت به منوی اصلی", "settings-menu"),
	"nvm-back-to-menu":     cb("↪️ بیخیالش برگرد منوی اصلی", "settings-menu"),
	"wpp-activate":         cb("✅ برگشت به حالت پیشفرض", "wpp|activate"),
	"wpp-deactivate":       cb("❌ غیرفعال سازی اجباری", "wpp|deactivate"),
	"warning-activate":     cb("✅ فعالسازی", "warning|activate"),
	"warning-deactivate":   cb("❌ غیرفعالسازی", "warning|deactivate"),
	"seen-activate":        cb("✅ فعالسازی", "seen-settings|activate"),
	"seen-deactivate":      cb("❌ غیرفعالسازی", "seen-settings|deactivate"),
	"remove-custom-tag":    cb("🗑 پاک کردن تگ دلخواه", "rm-custom-tag"),
	"remove-audio-tag":     cb("🗑 پاک کردن تگ آهنگ", "rm-audio-tag"),
	"anon-name-set":        cb("✏️ تنظیم / تغییر نام مستعار", "anon-name-set"),
	"anon-name-remove":     cb("🗑 حذف نام مستعار", "anon-name-remove"),
	"anon-name-noemoji":    cb("بدون ایموجی", "anon-name-noemoji"),
	"name-sync-activate":   cb("✅ فعالسازی", "name-sync|activate"),
	"name-sync-deactivate": cb("❌ غیرفعالسازی", "name-sync|deactivate"),
}

// mylinksDefaultMenu mirrors MYLINKS_MARKUP["default-set"].
func mylinksDefaultMenu() [][]gotgbot.InlineKeyboardButton {
	return [][]gotgbot.InlineKeyboardButton{
		{cb("➕ اضافه کردن لینک جدید", "add-link")},
		{cb("🔧 شخصی سازی لینک", "ch-link"), cb("❌ حذف کردن لینک", "rm-link")},
		{cb("❔چرا چندتا لینک داشته باشم", "more-links")},
		// Same as the settings menu: my_links is a page of /menu, and myLinksCmd has
		// ended the conversation by the time this shows.
		{cb("🏠 برگشت به منو", "menu|"+menuMain)},
	}
}

// mylinksButtons mirrors the single-button entries of MYLINKS_MARKUP.
var mylinksButtons = map[string]gotgbot.InlineKeyboardButton{
	"back-to-menu":      cb("↪️ برگشت به منوی اصلی", "mylinks-menu"),
	"what-is-cid":       cb("❔آیدی لینک چیه", "what-is-cid|"),
	"what-is-customize": cb("❔شخصی سازی لینک چیه", "what-is-cid|"),
}
