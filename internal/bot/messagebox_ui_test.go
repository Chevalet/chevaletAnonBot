package bot

// Tests for the Message Box UI half (messagebox_ui.go / settings toggle / command
// list / Texts template). Everything here is DB-free: the toggle's read/write
// behaviour is exercised by the db and core tests, this file only pins the
// Telegram wiring — the parts where a wrong callback string fails silently.

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/aturzone/chevaletAnonBot/internal/texts"
)

// settingsMenuData is every callback_data the settings main menu emits.
func settingsMenuData() []string {
	var out []string
	for _, r := range settingsMainMenu() {
		for _, btn := range r {
			out = append(out, btn.CallbackData)
		}
	}
	return out
}

// TestMessageBoxSettingsButtonOnTheSettingsMenu: the page must be reachable from
// the settings menu, with the data the conversation entry point is registered for.
func TestMessageBoxSettingsButtonOnTheSettingsMenu(t *testing.T) {
	var found bool
	for _, r := range settingsMainMenu() {
		for _, btn := range r {
			if btn.CallbackData != mboxSettingsPrefix {
				continue
			}
			found = true
			if !strings.Contains(btn.Text, "جعبه") {
				t.Errorf("message-box button text = %q; want it to mention جعبه", btn.Text)
			}
		}
	}
	if !found {
		t.Fatalf("no settings button carries %q, so the message-box page is unreachable", mboxSettingsPrefix)
	}
}

// TestMessageBoxCallbacksDoNotCollide is the settings-side twin of
// TestMenuDataDoesNotCollide: the new callback data must not be swallowed by an
// earlier handler (a conversation's cqContains filter, another prefix), must not
// shadow each other, and must actually be listed in menu_test.go's collision
// lists so future additions keep being checked.
func TestMessageBoxCallbacksDoNotCollide(t *testing.T) {
	datas := []string{mboxSettingsPrefix, mboxOpenData}
	for _, data := range datas {
		if data == "" {
			t.Fatal("empty message-box callback data")
		}
		if len(data) > 64 {
			t.Errorf("callback_data %q is %d bytes (>64)", data, len(data))
		}
		for _, sub := range conversationContainsFilters {
			if strings.Contains(data, sub) {
				t.Errorf("%q contains %q, so a conversation would swallow it", data, sub)
			}
		}
		for _, prefix := range otherPrefixFilters {
			if prefix == data {
				continue
			}
			if strings.HasPrefix(data, prefix) {
				t.Errorf("%q starts with %q, which another handler claims first", data, prefix)
			}
		}
		if !containsString(otherPrefixFilters, data) {
			t.Errorf("menu_test.go's otherPrefixFilters is missing %q — the collision lists must list the new prefixes", data)
		}
	}

	// The page prefix and the notification button must not shadow one another.
	if strings.HasPrefix(mboxOpenData, mboxSettingsPrefix) || strings.HasPrefix(mboxSettingsPrefix, mboxOpenData) {
		t.Errorf("%q and %q shadow each other", mboxSettingsPrefix, mboxOpenData)
	}

	// Both flip variants must route to the page handler: they share its prefix.
	for _, btn := range []string{mboxActivateBtn.CallbackData, mboxDeactivateBtn.CallbackData} {
		if !strings.HasPrefix(btn, mboxSettingsPrefix) {
			t.Errorf("toggle button data %q must start with %q", btn, mboxSettingsPrefix)
		}
	}
}

// TestBoxNotificationMarkup: the notification carries exactly one button, whose
// data opens the box and whose label comes from the shared text constant.
func TestBoxNotificationMarkup(t *testing.T) {
	// With no count the label is the plain button text.
	kb := boxNotificationMarkup(0)
	if len(kb.InlineKeyboard) != 1 || len(kb.InlineKeyboard[0]) != 1 {
		t.Fatalf("boxNotificationMarkup(0) has %d rows; want exactly one row with one button",
			len(kb.InlineKeyboard))
	}
	btn := kb.InlineKeyboard[0][0]
	if btn.CallbackData != mboxOpenData {
		t.Errorf("notification button data = %q; want %q", btn.CallbackData, mboxOpenData)
	}
	if btn.Text != txtBoxOpenButton {
		t.Errorf("notification button text = %q; want txtBoxOpenButton (%q)", btn.Text, txtBoxOpenButton)
	}
	if len(btn.CallbackData) > 64 {
		t.Errorf("notification button data is %d bytes (>64)", len(btn.CallbackData))
	}

	// The pending count must be visible on the button itself.
	for _, n := range []int{1, 2, 37} {
		kb := boxNotificationMarkup(n)
		label := kb.InlineKeyboard[0][0].Text
		if !strings.Contains(label, txtBoxOpenButton) || !strings.Contains(label, fmt.Sprintf("(%d)", n)) {
			t.Errorf("boxNotificationMarkup(%d) label = %q; want it to carry the count", n, label)
		}
	}
}

// TestMessageBoxTextTemplate pins the settings page's Texts file: it must load,
// carry exactly one %s for settingsToggle to substitute, and say the things the
// feature contract promises (OFF by default, how to open, what opening does).
func TestMessageBoxTextTemplate(t *testing.T) {
	txt, err := texts.New("../../Texts").Get("settings/message_box")
	if err != nil {
		t.Fatalf("Texts/settings/message_box.txt does not load: %v", err)
	}
	if n := strings.Count(txt, "%s"); n != 1 {
		t.Errorf("template has %d %%s placeholders; settingsToggle substitutes exactly one", n)
	}
	for _, want := range []string{
		"وضعیت الان",   // the on/off line settingsToggle formats
		"/message_box", // the command that opens the box
		"خاموش",        // default OFF must be spelled out
		"جعبه",         // the feature is named the way the menu names it
	} {
		if !strings.Contains(txt, want) {
			t.Errorf("template does not mention %q", want)
		}
	}
	// The rendered page is an EditText payload; Telegram caps it at 4096 chars.
	if n := len([]rune(txt)); n > 4096 {
		t.Errorf("template is %d chars; an in-place edit must stay under 4096", n)
	}
	// fmtText must consume the placeholder (a stray one would render literally).
	if rendered := fmtText(txt, txtStateActive); strings.Contains(rendered, "%s") {
		t.Error("fmtText left a literal placeholder; the template has more than one substitution point")
	}
}

// TestMessageBoxCommandPublished: /message_box must be in the public command list
// (and therefore in the admin list) with a non-empty Persian description, without
// disturbing /menu's first position.
func TestMessageBoxCommandPublished(t *testing.T) {
	var found bool
	for _, c := range botCommands() {
		if c.Command == "message_box" {
			found = true
			if c.Description == "" {
				t.Error("/message_box has no description; Telegram would show a blank line")
			}
			if !strings.Contains(c.Description, "جعبه") {
				t.Errorf("/message_box description = %q; want it to mention جعبه", c.Description)
			}
		}
	}
	if !found {
		t.Fatal("botCommands() is missing /message_box")
	}
	if botCommands()[0].Command != "menu" {
		t.Errorf("/menu is no longer first in the command list; got /%s", botCommands()[0].Command)
	}
	var inAdmin bool
	for _, c := range adminBotCommands() {
		if c.Command == "message_box" {
			inAdmin = true
		}
	}
	if !inAdmin {
		t.Error("adminBotCommands() is missing /message_box (it is built from botCommands())")
	}
}

// TestMessageBoxHandlersRegistered pins the dispatcher wiring, which is else
// invisible without a live Bot: handlers.go must register the notification
// callback (top-level, ahead of the conversations so it works mid-flow), the
// settings entry point, and the /message_box command.
func TestMessageBoxHandlersRegistered(t *testing.T) {
	raw, err := os.ReadFile("handlers.go")
	if err != nil {
		t.Fatalf("read handlers.go: %v", err)
	}
	src := string(raw)

	for _, want := range []string{
		"handlers.NewCallback(cqfilters.Prefix(mboxOpenData), b.prep(messageBoxOpenBtn))",
		"handlers.NewCallback(cqfilters.Prefix(mboxSettingsPrefix), b.prep(messageBoxSettings))",
		`b.command("message_box", messageBoxCmd)`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("handlers.go is missing %q", want)
		}
	}

	// The notification button must be registered BEFORE the conversations.
	openIdx := strings.Index(src, "Prefix(mboxOpenData)")
	convIdx := strings.Index(src, "b.startConversation()")
	if openIdx < 0 || convIdx < 0 {
		t.Fatalf("could not locate the registrations (open=%d conv=%d)", openIdx, convIdx)
	}
	if openIdx > convIdx {
		t.Error("the mbox|open callback is registered after the conversations; a conversation state could swallow it")
	}
}

// TestMessageBoxToggleWiring pins the settings page's toggle contract without a
// DB: it must read and write through the message-box columns and render the
// shared "#/settings/message_box" template.
func TestMessageBoxToggleWiring(t *testing.T) {
	raw, err := os.ReadFile("settings.go")
	if err != nil {
		t.Fatalf("read settings.go: %v", err)
	}
	src := string(raw)
	for _, want := range []string{
		"func messageBoxSettings",
		"b.DB.GetMessageBox",
		"b.DB.SetMessageBox",
		`"settings/message_box"`,
		"mboxActivateBtn",
		"mboxDeactivateBtn",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("settings.go is missing %q in the message-box toggle", want)
		}
	}
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
