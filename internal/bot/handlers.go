package bot

import (
	"errors"
	"log/slog"
	"strconv"
	"strings"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/PaulSonOfLars/gotgbot/v2/ext"
	"github.com/PaulSonOfLars/gotgbot/v2/ext/handlers"
	"github.com/PaulSonOfLars/gotgbot/v2/ext/handlers/conversation"
	"github.com/PaulSonOfLars/gotgbot/v2/ext/handlers/filters"
	cqfilters "github.com/PaulSonOfLars/gotgbot/v2/ext/handlers/filters/callbackquery"
	msgfilters "github.com/PaulSonOfLars/gotgbot/v2/ext/handlers/filters/message"
)

// registerHandlers wires the dispatcher in the same group-0 order as the Python
// main.py handler list, so exactly one handler processes each update and
// precedence matches the original (the conversation outranks the media-group and
// catch-all handlers; the catch-all is last).
//
// Phase 3 adds: no-callback, the /start conversation (start/answer/seen/block/
// unblock/report/delete callbacks + the state-0 send_msg + fallbacks), the
// media-group handler and the top-level delete handler, plus the other_messages
// catch-all. Phases 4/5 (settings, my_links, admin, AI, jobs) come later.
func (b *Bot) registerHandlers() {
	d := b.Dispatcher

	// errmore_handler — the "more" button on error reports in ERROR_CHAT_ID.
	// Registered OUTSIDE prep (it lives in a group, not a private chat) and before
	// the conversation so it is never shadowed.
	d.AddHandler(handlers.NewCallback(cqfilters.Prefix("errmore|"), b.errMore))

	// report action buttons — the accept/ban/message buttons under reports in
	// REPORT_CHAT_ID. Also OUTSIDE prep: that chat is a channel, which prep drops.
	d.AddHandler(handlers.NewCallback(cqfilters.Prefix("rpt|"), b.reportAction))

	// An admin's reply to a "write the message" prompt, in their own private chat.
	// Ahead of the conversations so the reply is not consumed as an anonymous
	// message; the filter only matches replies to the bot's own prompt.
	d.AddHandler(handlers.NewMessage(b.rptComposeFilter(botIDInt(b.Cfg.BotID)), b.topLevel(rptComposeReply)))

	// A user's reply to the "describe the bug" prompt. Ahead of the conversations
	// and the catch-all for the same reason as the admin compose handler.
	d.AddHandler(handlers.NewMessage(b.bugComposeFilter(botIDInt(b.Cfg.BotID)), b.topLevel(bugComposeReply)))

	// no_callback_handler — answers the spacer / "sent with link" buttons.
	d.AddHandler(handlers.NewCallback(cqfilters.Prefix("no-callback"), b.topLevel(noCallback)))

	// /menu panel buttons. Registered BEFORE the conversations so a user part-way
	// through a settings or my_links flow can still reach the menu — a conversation
	// state would otherwise get first refusal on the update. The "menu|" prefix is
	// deliberately free of every substring the conversations' cqContains filters
	// match on (see menu.go), so this cannot shadow them either.
	d.AddHandler(handlers.NewCallback(cqfilters.Prefix("menu|"), b.menuCallback))

	// A tap on the persistent bar arrives as an ordinary text message, so it must be
	// claimed BEFORE the start conversation's send state and before the catch-all —
	// otherwise the label would be delivered as somebody's anonymous message. The
	// filter matches only that exact text in a private chat.
	d.AddHandler(handlers.NewMessage(menuBarFilter, b.topLevel(menuCmd)))

	// start_cmd_handler — the per-user conversation.
	d.AddHandler(b.startConversation())

	// media_group_handler — subsequent media of a group.
	d.AddHandler(handlers.NewMessage(msgfilters.MediaGroup, b.topLevel(handleMedia)))

	// The "سایر" toggle on a delivered message: reveals/hides report+block in place.
	d.AddHandler(handlers.NewCallback(cqfilters.Prefix("oth"), b.topLevel(otherActions)))

	// delete_message_handler — the warning's "delete" button, standalone.
	d.AddHandler(handlers.NewCallback(cqfilters.Prefix("delete|"), b.topLevel(deleteMsgClbk)))

	// help + the "why multiple links" callback.
	b.command("help", cmdHelp)
	d.AddHandler(handlers.NewCallback(cqContains("more-links"), b.topLevel(moreLinks)))

	// my_links and settings conversations.
	d.AddHandler(b.myLinksConversation())
	d.AddHandler(b.settingsConversation())

	// The /menu panel entry point.
	b.command("menu", menuCmd)

	// remaining Phase 2/5 self-contained commands.
	b.command("privacy", cmdPrivacy)
	b.command("donate", cmdDonate)
	b.command("admin", adminCmd)
	b.command("admin_donate", adminDonateCmd)
	b.command("admin_reports", adminReportsCmd)
	b.command("admin_stats", adminStatsCmd)
	b.command("admin_menurich", adminMenuRichCmd)

	// The moderation panel's buttons. Outside prep like the other admin callback
	// paths, and admin-checked inside the handler.
	d.AddHandler(handlers.NewCallback(cqfilters.Prefix("am|"), b.adminMod))
	b.command("myuid", cmdMyUID)
	b.command("bug", cmdBug)

	// ai_input_message_handler — the GM-group AI input. Registered (like main.py)
	// just before the catch-all, WITHOUT prep, only when the AI feature is enabled
	// AND GM_GROUP_ID is set. AI is OFF by default (no AI_URL POST surface).
	if b.Cfg.AIEnabled {
		if gid, ok := b.gmGroupID(); ok {
			d.AddHandler(handlers.NewMessage(b.aiInputFilter(gid, botIDInt(b.Cfg.BotID)), b.aiInput))
		}
	}

	// other_messages_handler — the catch-all, registered LAST so the conversations
	// and media handler take precedence. The Python `& ~IsDirectMessageFilter`
	// (exclude business "direct messages" chats) is unnecessary here because prep
	// already restricts handling to private chats.
	d.AddHandler(handlers.NewMessage(msgfilters.All, b.topLevel(otherMessages)))
}

// botIDInt parses the numeric bot id (BOT_TOKEN before the ':'); 0 on failure.
func botIDInt(botID string) int64 {
	n, _ := strconv.ParseInt(botID, 10, 64)
	return n
}

// settingsConversation builds the ConversationHandler ported from settings.py:
// state "0" = changing the display name, "1" = custom tag, "2" = audio tag.
func (b *Bot) settingsConversation() handlers.Conversation {
	entryPoints := []ext.Handler{
		handlers.NewCallback(cqfilters.Prefix("reply-quote|"), b.prep(replyQuote)),
		handlers.NewCallback(cqfilters.Prefix("media-settings|"), b.prep(mediaSettings)),
		handlers.NewCallback(cqfilters.Prefix("change-name|"), b.prep(changeName)),
		handlers.NewCallback(cqfilters.Prefix("custom-tag|"), b.prep(customTag)),
		handlers.NewCallback(cqfilters.Prefix("audio-tag|"), b.prep(audioTag)),
		handlers.NewCallback(cqfilters.Prefix("wpp|"), b.prep(wppClbk)),
		handlers.NewCallback(cqfilters.Prefix("warning|"), b.prep(warningClbk)),
		handlers.NewCallback(cqfilters.Prefix("easier-answer|"), b.prep(easierAnswer)),
		handlers.NewCallback(cqfilters.Prefix("channel-signature|"), b.prep(channelSignature)),
		handlers.NewCallback(cqfilters.Prefix("seen-settings|"), b.prep(seenSettings)),
		handlers.NewCallback(cqfilters.Prefix("anon-name|"), b.prep(anonName)),
		handlers.NewCallback(cqfilters.Prefix("anon-name-set"), b.prep(anonNameSet)),
		handlers.NewCallback(cqfilters.Prefix("anon-name-remove"), b.prep(removeAnonName)),
		handlers.NewCallback(cqfilters.Prefix("unblock-all|"), b.prep(unblockAll)),
		handlers.NewCallback(cqfilters.Prefix("unblock-me|"), b.prep(unblockMe)),
		handlers.NewCommand("settings", b.prep(settingsCmd)),
		handlers.NewCallback(cqContains("settings-menu"), b.prep(settingsCmd)),
		handlers.NewCallback(cqContains("what-is-formatting"), b.prep(whatIsFormatting)),
	}

	return handlers.NewConversation(
		entryPoints,
		map[string][]ext.Handler{
			"0": {handlers.NewMessage(textNotCommand, b.prep(updateName))},
			"1": {
				handlers.NewCallback(cqContains("rm-custom-tag"), b.prep(removeCustomTag)),
				handlers.NewMessage(textNotCommand, b.prep(updateCustomTag)),
			},
			"2": {
				handlers.NewCallback(cqContains("rm-audio-tag"), b.prep(removeAudioTag)),
				handlers.NewMessage(textNotCommand, b.prep(updateAudioTag)),
			},
			"3": {handlers.NewMessage(textNotCommand, b.prep(updateAnonName))},
			"4": {
				handlers.NewCallback(cqContains("anon-name-noemoji"), b.prep(anonSkipEmoji)),
				handlers.NewMessage(textNotCommand, b.prep(updateAnonEmoji)),
			},
		},
		&handlers.ConversationOpts{
			Fallbacks: []ext.Handler{
				handlers.NewCallback(cqContains("what-is-formatting"), b.prep(whatIsFormatting)),
				handlers.NewCallback(cqContains("settings-menu"), b.prep(settingsCmd)),
				handlers.NewCallback(cqContains("nvm-back-to-menu"), b.prep(settingsCmd)),
				handlers.NewCommand("cancel", b.prep(genericCancelCmd)),
				handlers.NewMessage(msgfilters.All, b.prep(settingsCancelAll)),
			},
			StateStorage: b.newConvStore(),
		},
	)
}

// myLinksConversation builds the ConversationHandler ported from my_links.py:
// state "0" = waiting for the new cid while renaming a link.
func (b *Bot) myLinksConversation() handlers.Conversation {
	entryPoints := []ext.Handler{
		handlers.NewCallback(cqfilters.Prefix("ch-link"), b.prep(changeLink)),
		handlers.NewCallback(cqContains("add-link"), b.prep(addLink)),
		handlers.NewCallback(cqfilters.Prefix("rm-link"), b.prep(removeLink)),
		handlers.NewCommand("my_links", b.prep(myLinksCmd)),
		handlers.NewCallback(cqContains("mylinks-menu"), b.prep(myLinksCmd)),
		handlers.NewCallback(cqContains("what-is-cid"), b.prep(whatIsCid)),
	}

	return handlers.NewConversation(
		entryPoints,
		map[string][]ext.Handler{
			"0": {handlers.NewMessage(textNotCommand, b.prep(updateCid))},
		},
		&handlers.ConversationOpts{
			Fallbacks: []ext.Handler{
				handlers.NewCallback(cqContains("what-is-cid"), b.prep(whatIsCid)),
				handlers.NewCallback(cqContains("mylinks-menu"), b.prep(myLinksCmd)),
				handlers.NewCommand("cancel", b.prep(genericCancelCmd)),
				handlers.NewMessage(msgfilters.All, b.prep(othersWhileSending)),
			},
			StateStorage: b.newConvStore(),
		},
	)
}

// startConversation builds the ConversationHandler ported from start.py
// (start_cmd_handler): per-user, entry points + state "0" (composing) +
// fallbacks. Entry points and state-0 share the same callback set; state 0 adds
// the send_msg message handler.
func (b *Bot) startConversation() handlers.Conversation {
	callbacks := func() []ext.Handler {
		return []ext.Handler{
			handlers.NewCommand("start", b.prep(startCmd)),
			handlers.NewCallback(cqfilters.Prefix("answer|"), b.prep(answer)),
			handlers.NewCallback(cqfilters.Prefix("seen|"), b.prep(seen)),
			handlers.NewCallback(cqContains("alread-seen"), b.prep(alreadySeen)),
			handlers.NewCallback(cqfilters.Prefix("report|"), b.prep(report)),
			handlers.NewCallback(cqfilters.Prefix("report_yes|"), b.prep(reportConfirmYes)),
			handlers.NewCallback(cqfilters.Prefix("report_no"), b.prep(reportConfirmNo)),
			handlers.NewCallback(cqfilters.Prefix("block|"), b.prep(block)),
			handlers.NewCallback(cqfilters.Prefix("unblock|"), b.prep(unblock)),
		}
	}

	entryPoints := callbacks()
	state0 := append(callbacks(),
		handlers.NewMessage(notCommand, b.prep(sendMsg)),
	)

	return handlers.NewConversation(
		entryPoints,
		map[string][]ext.Handler{stateSending: state0},
		&handlers.ConversationOpts{
			Fallbacks: []ext.Handler{
				handlers.NewCallback(cqfilters.Prefix("delete|"), b.prep(deleteMsgClbk)),
				handlers.NewCallback(cqContains("cancel"), b.prep(cancel)),
				handlers.NewCommand("cancel", b.prep(cancelCmd)),
				handlers.NewMessage(msgfilters.All, b.prep(cancelAll)),
			},
			// KeyStrategySenderAndChat == PTB's per_user + per_chat (the default the
			// Python ConversationHandlers used). This keys a user's private-chat
			// conversation to that chat, so their messages in the GM group are NOT
			// captured by it and instead reach the AI handler. (Sender-only keying
			// would have mis-routed GM-group replies into a stale private state.)
			StateStorage: b.newConvStore(),
		},
	)
}

// notCommand mirrors PTB's `filters.ALL & ~filters.COMMAND` for the state-0
// send_msg handler: any message that is not a bot command.
func notCommand(m *gotgbot.Message) bool { return !msgfilters.Command(m) }

// textNotCommand mirrors PTB's `filters.TEXT & ~filters.COMMAND` for the
// settings/my_links text-input states.
func textNotCommand(m *gotgbot.Message) bool { return msgfilters.Text(m) && !msgfilters.Command(m) }

// cqContains builds a callback-query filter matching data that CONTAINS sub,
// reproducing the Python CallbackQueryHandler patterns that used an unanchored
// regex (e.g. r"cancel", r"alread-seen").
func cqContains(sub string) filters.CallbackQuery {
	return func(cq *gotgbot.CallbackQuery) bool {
		return strings.Contains(cq.Data, sub)
	}
}

// replyHTML replies to the triggering message in HTML, optionally disabling the
// link preview (the Python handlers pass disable_web_page_preview=True).
func (b *Bot) replyHTML(ctx *ext.Context, text string, disablePreview bool) error {
	opts := &gotgbot.SendMessageOpts{ParseMode: "HTML"}
	if disablePreview {
		opts.LinkPreviewOptions = &gotgbot.LinkPreviewOptions{IsDisabled: true}
	}
	_, err := ctx.EffectiveMessage.Reply(b.TG, text, opts)
	return err
}

// cmdHelp mirrors help.help_cmd: the start/help text with the donation link.
func cmdHelp(b *Bot, _ *gotgbot.Bot, ctx *ext.Context, _ string) error {
	txt, err := b.Texts.Get("start_help")
	if err != nil {
		return err
	}
	// b.Dyn, not b.Cfg: an admin who changes the donation link with /admin_donate
	// must change it everywhere it appears, otherwise the button and the texts
	// would point at two different places.
	txt = strings.ReplaceAll(txt, "%s", b.Dyn.DonationLink())
	return b.replyHTML(ctx, txt, true)
}

// cmdPrivacy mirrors privacy.privacy_cmd: the privacy/safety text. Python called
// reply_html WITHOUT disable_web_page_preview, so the link preview stays enabled.
func cmdPrivacy(b *Bot, _ *gotgbot.Bot, ctx *ext.Context, _ string) error {
	txt, err := b.Texts.Get("privacy_safety")
	if err != nil {
		return err
	}
	return b.replyHTML(ctx, txt, false)
}

// cmdDonate mirrors donate.donate_cmd: the donation text with the donation link.
// Python called reply_html WITHOUT disable_web_page_preview, so the donation
// link's preview card is shown.
func cmdDonate(b *Bot, _ *gotgbot.Bot, ctx *ext.Context, _ string) error {
	txt, err := b.Texts.Get("donate")
	if err != nil {
		return err
	}
	txt = strings.ReplaceAll(txt, "%s", b.Dyn.DonationLink())
	return b.replyHTML(ctx, txt, false)
}

// cmdMyUID mirrors myuid.myuid_cmd: replies with the user's Telegram id.
func cmdMyUID(b *Bot, _ *gotgbot.Bot, ctx *ext.Context, userid string) error {
	_, err := ctx.EffectiveMessage.Reply(b.TG, userid, nil)
	return err
}

// cmdBug mirrors warn_bug.warn_bug_reply_to_channel: the channel-reply warning,
// with a small-media link preview pointing at the relevant Telegram bug.
func cmdBug(b *Bot, _ *gotgbot.Bot, ctx *ext.Context, _ string) error {
	txt, err := b.Texts.Get("warn_reply_to_channel")
	if err != nil {
		return err
	}
	_, err = ctx.EffectiveMessage.Reply(b.TG, txt, &gotgbot.SendMessageOpts{
		ParseMode: "HTML",
		LinkPreviewOptions: &gotgbot.LinkPreviewOptions{
			Url:              "https://bugs.telegram.org/c/47222",
			PreferSmallMedia: true,
		},
	})
	return err
}

// newConvStore creates a conversation state storage AND registers it, so the menu
// (and any other flow switch) can drop a stale conversation.
//
// Why this is needed: the three ConversationHandlers are independent state
// machines, and handler order decides which one claims a plain text message. The
// start conversation is registered first, so a user who was part-way through
// composing a message and then went to settings to change their name had that name
// delivered as the message — to the wrong person, silently. Whoever begins a new
// flow now clears the others first.
func (b *Bot) newConvStore() conversation.Storage {
	s := conversation.NewInMemoryStorage(conversation.KeyStrategySenderAndChat)
	b.convStores = append(b.convStores, s)
	return s
}

// dropConversations ends every pending conversation for this user+chat.
//
// Safe to call from inside a conversation handler: the framework applies whatever
// state that handler returns after it finishes, so clearing first only discards
// what was already there.
//
// KeyStrategySenderAndChat derives the key from the sender and chat, both of which
// a callback query carries — so this targets the right user whether it was reached
// from a tap or a command.
func (b *Bot) dropConversations(ctx *ext.Context) {
	for _, s := range b.convStores {
		if err := s.Delete(ctx); err != nil && !errors.Is(err, conversation.ErrKeyNotFound) {
			slog.Warn("could not drop a pending conversation", "err", err)
		}
	}
}
