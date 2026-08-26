package bot

import (
	"strings"
	"testing"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// The whole point of the rich renderer is that a menu screen survives the
// translation intact. A button that loses its callback_data, or a row that
// collapses, is a dead menu — and unlike a classic keyboard there is no visible
// clue that anything went wrong.
func TestRichHTMLKeepsEveryButtonAndRow(t *testing.T) {
	kb := ikb(
		row(cb("لینک‌های من", "mylinks-menu"), cb("تنظیمات", "settings-menu")),
		row(cb("راهنما", "menu|help")),
	)
	got := richHTML("عنوان", kb)

	if n := strings.Count(got, "<tg-button-row>"); n != 2 {
		t.Errorf("expected 2 button rows, got %d in %q", n, got)
	}
	if n := strings.Count(got, "<tg-button "); n != 3 {
		t.Errorf("expected 3 buttons, got %d in %q", n, got)
	}
	for _, data := range []string{"mylinks-menu", "settings-menu", "menu|help"} {
		if !strings.Contains(got, `data="`+data+`"`) {
			t.Errorf("callback data %q missing from %q", data, got)
		}
	}
	// The two conversation entry points are matched by substring elsewhere; if the
	// renderer ever mangled them the conversations would stop being reachable.
	if !strings.Contains(got, "لینک‌های من") {
		t.Error("button label was lost")
	}
}

// A blank line is a paragraph separator in the panel text, not an empty
// paragraph — otherwise every screen gains vertical gaps it was not written with.
func TestRichHTMLParagraphs(t *testing.T) {
	got := richHTML("first\n\nsecond", gotgbot.InlineKeyboardMarkup{})
	if n := strings.Count(got, "<p>"); n != 2 {
		t.Errorf("expected 2 paragraphs, got %d in %q", n, got)
	}
	if strings.Contains(got, "<p></p>") {
		t.Errorf("blank line became an empty paragraph: %q", got)
	}
}

// A quote inside an attribute would terminate it early, so a button could end up
// carrying different callback data than intended. Never trust it to "not happen".
func TestRichButtonEscapesAttributes(t *testing.T) {
	got := richButton(cb("x", `a"b&c<d`))
	if strings.Contains(got, `data="a"b`) {
		t.Errorf("attribute was not escaped: %q", got)
	}
	for _, want := range []string{"&quot;", "&amp;", "&lt;"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in escaped output %q", want, got)
		}
	}
}

// URL buttons render as url-type tags. The panel has none today, but the report
// and donation flows do, and a silently dropped link button is a dead end.
func TestRichButtonURL(t *testing.T) {
	got := richButton(gotgbot.InlineKeyboardButton{Text: "حمایت", Url: "https://t.me/chevalet_studio"})
	if !strings.Contains(got, `type="url"`) || !strings.Contains(got, "https://t.me/chevalet_studio") {
		t.Errorf("url button rendered wrong: %q", got)
	}
}

// A button kind we cannot translate must produce nothing rather than a malformed
// tag: an absent button is obvious, a broken one silently does nothing when tapped.
func TestRichButtonUnsupportedKindIsDropped(t *testing.T) {
	if got := richButton(gotgbot.InlineKeyboardButton{Text: "switch"}); got != "" {
		t.Errorf("expected an untranslatable button to be dropped, got %q", got)
	}
}

// Persian panels must be laid out right-to-left; without is_rtl Telegram orders
// the blocks the wrong way round.
func TestRichParamsIsRTL(t *testing.T) {
	p := richParams("متن", gotgbot.InlineKeyboardMarkup{})
	if p["is_rtl"] != true {
		t.Errorf("is_rtl must be set for the Persian panel, got %v", p["is_rtl"])
	}
	if _, ok := p["html"].(string); !ok {
		t.Error("html field must be a string")
	}
}

// An empty row must not emit an empty <tg-button-row>, which Telegram rejects.
func TestRichHTMLSkipsEmptyRows(t *testing.T) {
	kb := gotgbot.InlineKeyboardMarkup{InlineKeyboard: [][]gotgbot.InlineKeyboardButton{
		{},
		row(cb("ok", "menu|main")),
	}}
	got := richHTML("t", kb)
	if n := strings.Count(got, "<tg-button-row>"); n != 1 {
		t.Errorf("expected the empty row to be skipped, got %d rows in %q", n, got)
	}
}
