package bot

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/PaulSonOfLars/gotgbot/v2/ext"
)

// /menu — a BotFather-style panel: ONE message that is edited in place as the user
// navigates, with a back button, rather than a trail of new messages.
//
// Four sections for everyone, a fifth for admins:
//
//	[ 🔗 my links ]  [ ⚙️ settings ]
//	[ 🆘 help     ]  [ 🙏 support  ]
//	[ 🛡 admin panel ]            (admins only)
//
// TWO DESIGN POINTS THAT MATTER:
//
// 1. "my links" and "settings" are ConversationHandlers with their own state
// machines. Rather than reimplement them, their buttons carry the callback data
// those conversations ALREADY accept as entry points (mylinks-menu / settings-menu
// — see myLinksConversation and settingsConversation). So tapping them enters the
// real conversation, and every existing sub-flow keeps working untouched. Anything
// else would have meant duplicating two state machines, which is how menus and the
// commands they wrap drift apart.
//
// 2. Every other menu callback is prefixed "menu|", which is checked against the
// conversations' cqContains filters so it cannot be swallowed by one. The handler
// is also registered BEFORE the conversations, so a user part-way through a
// settings flow can still tap the menu.

// menuVerb values. Kept free of the substrings the conversation entry points match
// on (settings-menu, mylinks-menu, add-link, what-is-cid, more-links, …).
const (
	menuMain     = "main"
	menuHelp     = "help"
	menuHelpText = "helptext"
	menuPrivacy  = "privacy"
	menuMyUID    = "myuid"
	menuBug      = "bug"
	menuBugSend  = "bugsend"
	menuDonate   = "donate"

	// Admin section.
	menuAdmin        = "admin"
	menuAdminStats   = "astats"
	menuAdminReports = "areports"
	menuAdminDonate  = "adonate"
	menuAdminDonOn   = "adon_on"
	menuAdminDonOff  = "adon_off"
	menuAdminBackup  = "abackup"
	menuAdminCount   = "acount"
	menuAdminCmds    = "acmds"
)

// menuTitle is the panel's own heading, shown on the main screen.
const menuTitle = "🏠 <b>منوی اصلی</b>\n\nیکی از گزینه‌ها رو انتخاب کن:"

// menuBarButton is the single button in the persistent bar next to the input box
// (a ReplyKeyboard). Deliberately ONE button: it opens the panel, and the panel
// holds everything else. A bar crowded with every option is what this replaces.
//
// Tapping it sends this exact text, which menuBarFilter matches to reopen the
// panel — so the label is effectively protocol and must match on both sides.
const menuBarButton = "🏠 منو"

// menuBarKeyboard is the bar.
//
// IsPersistent is deliberately FALSE. True pins the bar open with no way to put it
// away, which is wrong for a bot people mostly use to type: the keyboard icon next
// to the input box has to be able to collapse and reopen it. False is what every
// bot with a usable bottom bar does.
//
// ResizeKeyboard keeps it one row tall instead of a full keyboard's height.
func menuBarKeyboard() gotgbot.ReplyKeyboardMarkup {
	return gotgbot.ReplyKeyboardMarkup{
		Keyboard:              [][]gotgbot.KeyboardButton{{{Text: menuBarButton}}},
		IsPersistent:          false,
		ResizeKeyboard:        true,
		InputFieldPlaceholder: "پیامت رو بنویس…",
	}
}

// markMenuBarSent records that the bar has reached this user, for callers that
// attached it to a message of their own (see /start). Best effort: the only cost of
// failure is showing the install notice once more.
func (b *Bot) markMenuBarSent(userid string) {
	dbctx, cancel := b.bg()
	defer cancel()
	if err := b.DB.SetMenuBarSent(dbctx, userid); err != nil {
		slog.Warn("could not record that the menu bar was installed", "err", err)
	}
}

// ensureMenuBar installs the bar once per user.
//
// A ReplyKeyboard has to ride along on some message and then persists for that chat
// forever, so this is a one-shot: the flag lives in the database (users
// .menu_bar_sent) because the alternative is either never reaching the ~17k users
// who predate the bar, or attaching a throwaway message to every single /menu.
//
// Best effort throughout — failing to install a convenience must not break /menu.
func (b *Bot) ensureMenuBar(tg *gotgbot.Bot, ctx *ext.Context, userid string) {
	dbctx, cancel := b.bg()
	defer cancel()

	sent, err := b.DB.MenuBarSent(dbctx, userid)
	if err != nil {
		slog.Warn("could not read the menu-bar flag", "err", err)
		return
	}
	if sent {
		return
	}
	if ctx.EffectiveChat == nil {
		return
	}
	if _, err := tg.SendMessage(ctx.EffectiveChat.Id,
		"👇 از این به بعد دکمه <b>منو</b> پایین صفحه همیشه در دسترسته.",
		&gotgbot.SendMessageOpts{ParseMode: "HTML", ReplyMarkup: menuBarKeyboard()}); err != nil {
		slog.Warn("could not install the menu bar", "err", err)
		return
	}
	if err := b.DB.SetMenuBarSent(dbctx, userid); err != nil {
		// Worst case the notice is shown again next time; better than losing the bar.
		slog.Warn("could not record that the menu bar was installed", "err", err)
	}
}

// menuBarFilter matches a tap on the bar: a private text message whose content is
// exactly the button label.
func menuBarFilter(m *gotgbot.Message) bool {
	return m != nil && m.Chat.Type == "private" && strings.TrimSpace(m.Text) == menuBarButton
}

// panelTextLimit is where editing stops being safe. Telegram's hard cap is 4096;
// privacy_safety.txt alone is ~3900, so a panel that grows a header would silently
// fail to render. Past this, send a fresh message instead of editing.
const panelTextLimit = 4000

// mainMenuKeyboard builds the top level. The admin row only exists for admins —
// a non-admin must not see that there is a fifth section.
func mainMenuKeyboard(isAdmin bool) gotgbot.InlineKeyboardMarkup {
	rows := [][]gotgbot.InlineKeyboardButton{
		{
			// Straight into the existing conversations (see the note above).
			cb("🔗 لینک‌های من", "mylinks-menu"),
			cb("⚙️ تنظیمات و قابلیت‌ها", "settings-menu"),
		},
		{
			cb("🆘 راهنما", "menu|"+menuHelp),
			cb("🙏 حمایت مالی", "menu|"+menuDonate),
		},
	}
	if isAdmin {
		rows = append(rows, row(cb("🛡 پنل ادمین", "menu|"+menuAdmin)))
	}
	return gotgbot.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// backRow returns to the main menu.
func backRow() []gotgbot.InlineKeyboardButton {
	return row(cb("↩️ برگشت به منو", "menu|"+menuMain))
}

// backRowTo returns to a NAMED screen. Sub-pages under راهنما used backRow, so
// "back" from privacy or the bug page jumped past the help menu to the top —
// losing the user's place instead of returning where they came from.
func backRowTo(verb, label string) []gotgbot.InlineKeyboardButton {
	return row(cb(label, "menu|"+verb))
}

// menuCmd handles /menu and a tap on the bar. It installs the bar first (once
// ever), so a user who arrives by typing /menu ends up with the button too.
func menuCmd(b *Bot, tg *gotgbot.Bot, ctx *ext.Context, userid string) error {
	// Opening the menu abandons whatever the user was part-way through, so drop it
	// rather than leaving a send state armed to swallow their next message.
	b.dropConversations(ctx)
	b.ensureMenuBar(tg, ctx, userid)
	kb := mainMenuKeyboard(b.isAdmin(userid))

	// The rich panel is a standalone message rather than a reply. Nothing is lost:
	// the panel is the only thing on screen the user is looking at, and quoting
	// their own "/menu" adds noise. Falls back to the classic reply on refusal.
	if b.Dyn.MenuRichEnabled() && ctx.EffectiveChat != nil {
		rctx, cancel := b.bg()
		_, err := b.richMenuSend(rctx, ctx.EffectiveChat.Id, menuTitle, kb)
		cancel()
		if err == nil {
			return nil
		}
		slog.Warn("rich menu send failed; falling back to the classic keyboard", "err", err)
	}

	_, err := ctx.EffectiveMessage.Reply(tg, menuTitle, &gotgbot.SendMessageOpts{
		ParseMode:   "HTML",
		ReplyMarkup: kb,
	})
	return err
}

// menuCallback handles every "menu|" button. Registered outside prep, before the
// conversations, and it resolves the acting user itself.
func (b *Bot) menuCallback(tg *gotgbot.Bot, ctx *ext.Context) error {
	clbk := ctx.CallbackQuery
	if clbk == nil || clbk.Data == "" {
		return nil
	}
	userid := strconv.FormatInt(clbk.From.Id, 10)
	verb := strings.TrimPrefix(clbk.Data, "menu|")
	admin := b.isAdmin(userid)

	// Any menu navigation means the user has moved on from whatever flow they were
	// in; see dropConversations for what goes wrong otherwise.
	b.dropConversations(ctx)

	// Every admin screen is gated here, not just hidden from the keyboard: the
	// button data is guessable, and hiding a button is not a permission check.
	//
	// Gated on isAdminVerb alone. An earlier version also required the verb to start
	// with "a", which happens to be true of all of them today — and would have
	// silently skipped the check for any admin verb added later that did not.
	if isAdminVerb(verb) && !admin {
		slog.Warn("menu admin screen requested by a non-admin", "actor", userid, "verb", verb)
		_, _ = clbk.Answer(tg, &gotgbot.AnswerCallbackQueryOpts{
			Text:      "فقط ادمین‌ها.",
			ShowAlert: true,
		})
		return nil
	}

	switch verb {
	case menuMain:
		_, _ = clbk.Answer(tg, nil)
		return b.panelEdit(tg, ctx, menuTitle, mainMenuKeyboard(admin))

	case menuHelp:
		_, _ = clbk.Answer(tg, nil)
		return b.panelEdit(tg, ctx, "🆘 <b>راهنما</b>\n\nچی میخوای بدونی؟", ikb(
			row(cb("📖 راهنمای کامل", "menu|"+menuHelpText)),
			row(cb("🔒 حریم خصوصی", "menu|"+menuPrivacy)),
			row(cb("🆔 آیدی عددی من", "menu|"+menuMyUID), cb("🐞 گزارش باگ", "menu|"+menuBug)),
			backRow(),
		))

	case menuHelpText:
		_, _ = clbk.Answer(tg, nil)
		txt, err := b.Texts.Get("start_help")
		if err != nil {
			return err
		}
		txt = strings.ReplaceAll(txt, "%s", b.Dyn.DonationLink())
		return b.panelEdit(tg, ctx, txt, ikb(backRowTo(menuHelp, "↩️ برگشت به راهنما")))

	case menuPrivacy:
		_, _ = clbk.Answer(tg, nil)
		txt, err := b.Texts.Get("privacy_safety")
		if err != nil {
			return err
		}
		// Deliberately no added heading: this text is ~3900 chars and a header could
		// push the edit past Telegram's 4096 limit.
		return b.panelEdit(tg, ctx, txt, ikb(backRowTo(menuHelp, "↩️ برگشت به راهنما")))

	case menuDonate:
		_, _ = clbk.Answer(tg, nil)
		txt, err := b.Texts.Get("donate")
		if err != nil {
			return err
		}
		txt = strings.ReplaceAll(txt, "%s", b.Dyn.DonationLink())
		return b.panelEdit(tg, ctx, txt, ikb(backRow()))

	case menuMyUID:
		_, _ = clbk.Answer(tg, nil)
		return b.panelEdit(tg, ctx,
			"🆔 آیدی عددی تو:\n<code>"+userid+"</code>", ikb(backRow()))

	case menuBug:
		_, _ = clbk.Answer(tg, nil)
		// Same text /bug serves (warn_reply_to_channel), so the two can never drift.
		txt, err := b.Texts.Get("warn_reply_to_channel")
		if err != nil {
			return err
		}
		// The page used to only EXPLAIN a Telegram bug while being labelled "report a
		// bug", which reads as an invitation to report one. Now it is both.
		return b.panelEdit(tg, ctx, txt, ikb(
			row(cb("📨 گزارش باگ به ما", "menu|"+menuBugSend)),
			backRowTo(menuHelp, "↩️ برگشت به راهنما"),
		))

	case menuBugSend:
		prompt := "📨 <b>گزارش باگ</b>\n\n" +
			"مشکل رو در جواب همین پیام بنویس تا برای تیم فرستاده بشه.\n" +
			"برای انصراف /cancel رو بفرست.\n\n" +
			"<code>" + bugMarker + "</code>"
		if _, err := tg.SendMessage(clbk.From.Id, prompt, &gotgbot.SendMessageOpts{
			ParseMode:   "HTML",
			ReplyMarkup: gotgbot.ForceReply{ForceReply: true, InputFieldPlaceholder: "توضیح باگ…"},
		}); err != nil {
			_, _ = clbk.Answer(tg, &gotgbot.AnswerCallbackQueryOpts{
				Text: "نشد الان بگیرم، بعدا امتحان کن.", ShowAlert: true})
			return nil
		}
		_, _ = clbk.Answer(tg, &gotgbot.AnswerCallbackQueryOpts{Text: "توضیح باگ رو بنویس ⤴️"})
		return nil

	// ---------------------------------------------------------------- admin
	case menuAdmin:
		_, _ = clbk.Answer(tg, nil)
		return b.panelEdit(tg, ctx, adminPanelTitle, adminPanelKeyboard())

	case menuAdminStats:
		_, _ = clbk.Answer(tg, nil)
		dbctx, cancel := b.bg()
		defer cancel()
		s, err := b.DB.GetStats(dbctx, 7)
		if err != nil {
			return err
		}
		return b.panelEdit(tg, ctx, formatStats(s), ikb(
			row(cb("🔄 بروزرسانی", "menu|"+menuAdminStats)),
			row(cb("↩️ پنل ادمین", "menu|"+menuAdmin)),
		))

	case menuAdminReports:
		_, _ = clbk.Answer(tg, nil)
		// modList carries its own route back to the panel, so nothing is appended
		// here — otherwise the button would appear twice on this path and be missing
		// on the /admin_reports one.
		text, kb, err := b.modList(false, 0)
		if err != nil {
			return err
		}
		return b.panelEdit(tg, ctx, text, kb)

	case menuAdminDonate:
		_, _ = clbk.Answer(tg, nil)
		return b.panelEdit(tg, ctx, b.donateStatus(), b.adminDonateKeyboard())

	case menuAdminDonOn, menuAdminDonOff:
		b.Dyn.SetDonationEnabled(verb == menuAdminDonOn)
		slog.Info("donation button toggled from the menu", "on", verb == menuAdminDonOn, "actor", userid)
		_, _ = clbk.Answer(tg, &gotgbot.AnswerCallbackQueryOpts{Text: "ذخیره شد ✅"})
		return b.panelEdit(tg, ctx, b.donateStatus(), b.adminDonateKeyboard())

	case menuAdminCount:
		_, _ = clbk.Answer(tg, nil)
		dbctx, cancel := b.bg()
		defer cancel()
		n, err := b.DB.UserCount(dbctx)
		if err != nil {
			return err
		}
		return b.panelEdit(tg, ctx, fmt.Sprintf("👥 تعداد کل کاربران: <b>%d</b>", n), ikb(
			row(cb("↩️ پنل ادمین", "menu|"+menuAdmin)),
		))

	case menuAdminBackup:
		// Sends a document, so it cannot be an in-place edit; the panel stays put.
		_, _ = clbk.Answer(tg, &gotgbot.AnswerCallbackQueryOpts{Text: "دارم بکاپ رو می‌فرستم…"})
		if err := b.adminBackup(tg, ctx); err != nil {
			_, _ = clbk.Answer(tg, &gotgbot.AnswerCallbackQueryOpts{
				Text:      "بکاپی پیدا نشد.",
				ShowAlert: true,
			})
			slog.Warn("menu backup failed", "err", err)
		}
		return nil

	case menuAdminCmds:
		_, _ = clbk.Answer(tg, nil)
		txt, err := b.Texts.Get("admin")
		if err != nil {
			return err
		}
		return b.panelEdit(tg, ctx, txt, ikb(row(cb("↩️ پنل ادمین", "menu|"+menuAdmin))))

	default:
		_, _ = clbk.Answer(tg, nil)
		return nil
	}
}

const adminPanelTitle = "🛡 <b>پنل ادمین</b>\n\nگزینه‌های مدیریتی:"

// adminPanelKeyboard is the fifth section. Everything that works as a button is a
// button; the commands that need typed arguments (ban by uid, send-msg, cid/link
// tools, AI settings) are one tap away under "دستورات ادمین" rather than being
// pretended into buttons they cannot be.
func adminPanelKeyboard() gotgbot.InlineKeyboardMarkup {
	return ikb(
		row(cb("📊 آمار روزانه", "menu|"+menuAdminStats)),
		row(cb("⚠️ ریپورت‌ها و بن‌ها", "menu|"+menuAdminReports)),
		row(cb("❤️ دکمه دونیشن", "menu|"+menuAdminDonate), cb("👥 تعداد کاربران", "menu|"+menuAdminCount)),
		row(cb("💾 گرفتن بکاپ", "menu|"+menuAdminBackup)),
		row(cb("📋 همه دستورات ادمین", "menu|"+menuAdminCmds)),
		backRow(),
	)
}

// adminDonateKeyboard offers the toggle that matches the current state, so the
// visible button is always the action rather than the status.
func (b *Bot) adminDonateKeyboard() gotgbot.InlineKeyboardMarkup {
	toggle := cb("✅ فعال کردن", "menu|"+menuAdminDonOn)
	if b.Dyn.DonationEnabled() {
		toggle = cb("❌ غیرفعال کردن", "menu|"+menuAdminDonOff)
	}
	return ikb(
		row(toggle),
		row(cb("↩️ پنل ادمین", "menu|"+menuAdmin)),
	)
}

// isAdminVerb reports whether a verb belongs to the admin section. Explicit list
// rather than a prefix test, so a future user-facing verb starting with "a" cannot
// accidentally become admin-gated (or worse, an admin one slip through).
func isAdminVerb(verb string) bool {
	switch verb {
	case menuAdmin, menuAdminStats, menuAdminReports, menuAdminDonate,
		menuAdminDonOn, menuAdminDonOff, menuAdminBackup, menuAdminCount, menuAdminCmds:
		return true
	}
	return false
}

// panelEdit replaces the panel in place, which is what makes this feel like one
// screen rather than a growing chat.
//
// Falls back to a NEW message when the text is too long to edit safely, or when the
// edit fails (the panel may be too old to edit, or the callback may have arrived
// from a message the bot cannot modify). Losing the in-place feel is better than
// showing the user nothing.
func (b *Bot) panelEdit(tg *gotgbot.Bot, ctx *ext.Context, text string, kb gotgbot.InlineKeyboardMarkup) error {
	msg := ctx.EffectiveMessage
	if msg == nil || len([]rune(text)) > panelTextLimit {
		return b.panelSend(tg, ctx, text, kb)
	}
	// Embedded-button rendering, when an admin has switched it on. A failure here
	// is not fatal: we fall through to the classic edit below, so a screen the
	// rich parser rejects still reaches the user as a normal keyboard.
	if b.Dyn.MenuRichEnabled() {
		rctx, cancel := b.bg()
		err := b.richMenuEdit(rctx, msg.Chat.Id, msg.MessageId, text, kb)
		cancel()
		if err == nil {
			return nil
		}
		if errMessageNotModified(err) {
			return nil
		}
		slog.Warn("rich menu edit failed; falling back to the classic keyboard", "err", err)
	}

	if _, _, err := msg.EditText(tg, text, &gotgbot.EditMessageTextOpts{
		ParseMode:          "HTML",
		LinkPreviewOptions: &gotgbot.LinkPreviewOptions{IsDisabled: true},
		ReplyMarkup:        kb,
	}); err != nil {
		if errMessageNotModified(err) {
			return nil // the user re-tapped the screen they are already on
		}
		slog.Warn("menu panel edit failed; sending a new panel", "err", err)
		return b.panelSend(tg, ctx, text, kb)
	}
	return nil
}

// panelSend posts a fresh panel.
func (b *Bot) panelSend(tg *gotgbot.Bot, ctx *ext.Context, text string, kb gotgbot.InlineKeyboardMarkup) error {
	chatID := int64(0)
	if ctx.EffectiveChat != nil {
		chatID = ctx.EffectiveChat.Id
	} else if ctx.CallbackQuery != nil {
		chatID = ctx.CallbackQuery.From.Id
	}
	if chatID == 0 {
		return nil
	}

	// Same deal as panelEdit: try rich, fall back to classic on any refusal.
	if b.Dyn.MenuRichEnabled() {
		rctx, cancel := b.bg()
		_, err := b.richMenuSend(rctx, chatID, text, kb)
		cancel()
		if err == nil {
			return nil
		}
		slog.Warn("rich menu send failed; falling back to the classic keyboard", "err", err)
	}

	_, err := tg.SendMessage(chatID, text, &gotgbot.SendMessageOpts{
		ParseMode:          "HTML",
		LinkPreviewOptions: &gotgbot.LinkPreviewOptions{IsDisabled: true},
		ReplyMarkup:        kb,
	})
	return err
}
