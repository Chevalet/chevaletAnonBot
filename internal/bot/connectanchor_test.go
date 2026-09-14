package bot

import (
	"strings"
	"testing"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/PaulSonOfLars/gotgbot/v2/ext"
	cqfilters "github.com/PaulSonOfLars/gotgbot/v2/ext/handlers/filters/callbackquery"
	msgfilters "github.com/PaulSonOfLars/gotgbot/v2/ext/handlers/filters/message"
)

// These cover the connect prompt's double life: a cancel button, and the anchor
// a sender replies to when they want to write again without reopening the link.

const testBotID = int64(7)

func anchorTestBot() *Bot {
	return &Bot{TG: &gotgbot.Bot{User: gotgbot.User{Id: testBotID, IsBot: true, Username: "anonbot"}}}
}

// kbMsg is a message from `from` carrying one row of callback buttons.
func kbMsg(from int64, data ...string) *gotgbot.Message {
	btns := make([]gotgbot.InlineKeyboardButton, 0, len(data))
	for _, d := range data {
		btns = append(btns, cb("x", d))
	}
	return &gotgbot.Message{
		From:        &gotgbot.User{Id: from},
		ReplyMarkup: &gotgbot.InlineKeyboardMarkup{InlineKeyboard: [][]gotgbot.InlineKeyboardButton{btns}},
	}
}

// replyCtx is an update whose message replies to `reply`.
func replyCtx(reply *gotgbot.Message) *ext.Context {
	return &ext.Context{EffectiveMessage: &gotgbot.Message{MessageId: 100, ReplyToMessage: reply}}
}

func TestConnectCancelMarkupCarriesTheLinkID(t *testing.T) {
	m, anchored := connectCancelMarkup("Ab3xY9zzqq")
	if !anchored {
		t.Fatal("a normal cid did not fit on the button; the anchor would never work")
	}
	if len(m.InlineKeyboard) != 1 || len(m.InlineKeyboard[0]) != 1 {
		t.Fatalf("shape = %v; want a single 1-button row", m.InlineKeyboard)
	}
	btn := m.InlineKeyboard[0][0]
	if btn.CallbackData != "cancel|Ab3xY9zzqq" {
		t.Errorf("data = %q; want cancel|Ab3xY9zzqq", btn.CallbackData)
	}
	// The label is what the user reads; only the data changed.
	if btn.Text != cancelButton().Text {
		t.Errorf("label = %q; want the same label as the plain cancel button (%q)", btn.Text, cancelButton().Text)
	}
	// Still a cancel button as far as the conversation fallback is concerned, so
	// the button keeps working even if the dedicated handler were ever removed.
	if !cqContains("cancel")(&gotgbot.CallbackQuery{Data: btn.CallbackData}) {
		t.Error("the anchored data no longer matches the conversation's cancel fallback")
	}
}

// A cid is user-chosen and MAX_CID_LENGTH is deployment-configured, so the data
// can outgrow Telegram's 64-byte limit. It must degrade to the plain button
// rather than making Telegram reject the whole connect prompt.
func TestConnectCancelMarkupFallsBackWhenItCannotFit(t *testing.T) {
	for _, c := range []struct{ name, cid string }{
		{"empty cid", ""},
		{"oversized cid", strings.Repeat("a", 58)},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, anchored := connectCancelMarkup(c.cid)
			if anchored {
				t.Fatalf("anchored = true for %s; the caller would advertise a shortcut that does not exist", c.name)
			}
			if got := m.InlineKeyboard[0][0].CallbackData; got != "cancel" {
				t.Errorf("data = %q; want the plain cancel", got)
			}
		})
	}
	// And the boundary just under it still anchors.
	if _, anchored := connectCancelMarkup(strings.Repeat("a", 57)); !anchored {
		t.Error("a 57-char cid (64 bytes with the prefix) should still fit")
	}
}

func TestConnectAnchorCid(t *testing.T) {
	b := anchorTestBot()

	cases := []struct {
		name  string
		reply *gotgbot.Message
		want  string
		ok    bool
	}{
		{"not a reply", nil, "", false},
		{"anchored connect prompt", kbMsg(testBotID, "cancel|Ab3xY9"), "Ab3xY9", true},
		{"anchor among other buttons", kbMsg(testBotID, "no-callback", "cancel|Ab3xY9"), "Ab3xY9", true},
		// The answer prompt's button carries no id: it is not an anchor.
		{"plain cancel is not an anchor", kbMsg(testBotID, "cancel"), "", false},
		{"empty cid is not an anchor", kbMsg(testBotID, "cancel|"), "", false},
		// A delivered anonymous message is isAnswer's business, not ours.
		{"delivered message keyboard", kbMsg(testBotID, "answer|tok|55", "oth|tok"), "", false},
		// Forged: someone else's message wearing the same button must not resolve.
		{"button on a non-bot message", kbMsg(999, "cancel|Ab3xY9"), "", false},
		{"no keyboard at all", &gotgbot.Message{From: &gotgbot.User{Id: testBotID}}, "", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := b.connectAnchorCid(replyCtx(c.reply))
			if ok != c.ok || got != c.want {
				t.Errorf("connectAnchorCid() = (%q,%v); want (%q,%v)", got, ok, c.want, c.ok)
			}
		})
	}
}

// A cid can be renamed to anything in AllowedCIDChars — including the exact
// strings the ConversationHandlers match with cqContains. That is why the anchor
// handler is registered ahead of them with a PREFIX filter: whatever the cid
// spells, the tap still reaches cancelConnect. If the data shape ever changes so
// the prefix stops covering it, this fails.
func TestConnectAnchorDataAlwaysReachesItsHandler(t *testing.T) {
	claims := cqfilters.Prefix(connectAnchorPrefix)

	hostileCids := append([]string{"cancel", "start", "menu", "oth", "delete"}, conversationContainsFilters...)
	for _, cid := range hostileCids {
		m, anchored := connectCancelMarkup(cid)
		if !anchored {
			t.Fatalf("cid %q did not anchor", cid)
		}
		data := m.InlineKeyboard[0][0].CallbackData
		if !claims(&gotgbot.CallbackQuery{Data: data}) {
			t.Errorf("data %q is not claimed by the Prefix(%q) handler", data, connectAnchorPrefix)
		}
		if len(data) > 64 {
			t.Errorf("data %q is %d bytes (>64)", data, len(data))
		}
	}

	// The legacy button must NOT be swallowed by the new handler: it has to keep
	// falling through to the conversation, which is what ends the conversation.
	if claims(&gotgbot.CallbackQuery{Data: "cancel"}) {
		t.Error("the plain cancel button is claimed by the anchor handler; it would no longer end the conversation")
	}
}

// Requirement: a reply to the anchor that STARTS with a command is a command,
// not something to deliver anonymously. checkIfAutoreply gates on exactly this
// filter, so pin what it considers a command.
func TestCommandReplyToAnchorIsNotAnAnonymousMessage(t *testing.T) {
	cmd := func(text string, ents ...gotgbot.MessageEntity) *gotgbot.Message {
		return &gotgbot.Message{Text: text, Entities: ents}
	}
	botCmd := func(off, l int64) gotgbot.MessageEntity {
		return gotgbot.MessageEntity{Type: "bot_command", Offset: off, Length: l}
	}

	cases := []struct {
		name string
		msg  *gotgbot.Message
		want bool
	}{
		{"plain command", cmd("/menu", botCmd(0, 5)), true},
		{"command with args", cmd("/start abc", botCmd(0, 6)), true},
		{"unknown command", cmd("/whatever", botCmd(0, 9)), true},
		{"ordinary text", cmd("سلام"), false},
		// Only a message that STARTS with one counts — a slash mid-sentence is
		// somebody's message, and must still be delivered.
		{"command not at the start", cmd("ping /menu", botCmd(5, 5)), false},
		{"no entities", cmd("/notacommand"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := msgfilters.Command(c.msg); got != c.want {
				t.Errorf("Command(%q) = %v; want %v", c.msg.Text, got, c.want)
			}
		})
	}
}

// The connect prompt prints the TARGET's display name, which that target chooses.
// The two compose filters are registered ahead of everything and match a bare
// substring of the replied-to message's text, so a name spelling their marker
// diverted every reply to that prompt — for the bug filter, straight into the
// report channel instead of to the target. Both must now ignore an anchor.
func TestComposeFiltersIgnoreAConnectAnchor(t *testing.T) {
	b := anchorTestBot()

	// exactly what startConnect builds, with a display name chosen to attack.
	prompt := func(name string) *gotgbot.Message {
		return &gotgbot.Message{
			MessageId:   5,
			From:        &gotgbot.User{Id: testBotID},
			Text:        "\n\u0628\u0647 " + name + " \u0648\u0635\u0644 \u0634\u062f\u06cc. \u067e\u06cc\u0627\u0645\u062a\u0648 \u0628\u0641\u0631\u0633\u062a\n" + txtConnectAnchorHint + "\n" + txtConnectBody,
			ReplyMarkup: &gotgbot.InlineKeyboardMarkup{InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{cb(btnCancel, "cancel|Ab3xY9")}}},
		}
	}
	replyTo := func(m *gotgbot.Message) *gotgbot.Message {
		return &gotgbot.Message{
			MessageId:      6,
			Chat:           gotgbot.Chat{Type: "private"},
			Text:           "\u0633\u0644\u0627\u0645",
			ReplyToMessage: m,
		}
	}

	for _, c := range []struct {
		name    string
		filter  func(*gotgbot.Message) bool
		hostile string
	}{
		{"bug filter", b.bugComposeFilter(testBotID), bugMarker},
		{"report compose filter", b.rptComposeFilter(testBotID), composeMarker + "x:y"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.filter(replyTo(prompt(c.hostile))) {
				t.Errorf("a reply to the connect prompt is claimed by the %s; the message never reaches the target", c.name)
			}
			// An ordinary name is unaffected either way.
			if c.filter(replyTo(prompt("\u0639\u0644\u06cc"))) {
				t.Errorf("the %s claims a reply to an ordinary connect prompt", c.name)
			}
		})
	}

	// The real prompts still work: same marker, no anchor button on them.
	realBug := &gotgbot.Message{
		MessageId: 9,
		From:      &gotgbot.User{Id: testBotID},
		Text:      "\u0645\u0634\u06a9\u0644\u062a \u0631\u0648 \u0628\u0646\u0648\u06cc\u0633\n" + bugMarker,
	}
	if !b.bugComposeFilter(testBotID)(replyTo(realBug)) {
		t.Error("the bug compose flow stopped matching its own prompt")
	}
	realRpt := &gotgbot.Message{
		MessageId: 10,
		From:      &gotgbot.User{Id: testBotID},
		Text:      "\u0645\u062a\u0646 \u067e\u06cc\u0627\u0645 \u0631\u0648 \u0628\u0646\u0648\u06cc\u0633\n" + composeMarker + "reported:tok",
	}
	if !b.rptComposeFilter(testBotID)(replyTo(realRpt)) {
		t.Error("the admin compose flow stopped matching its own prompt")
	}
}
