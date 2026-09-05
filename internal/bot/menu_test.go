package bot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// menuCallbackData is every callback_data the panel can emit: what the real
// keyboards build, plus every verb the handler switches on even if no keyboard
// shows it today.
func menuCallbackData() []string {
	var out []string
	for _, kb := range []gotgbot.InlineKeyboardMarkup{
		mainMenuKeyboard(false),
		mainMenuKeyboard(true),
		adminPanelKeyboard(),
	} {
		for _, r := range kb.InlineKeyboard {
			for _, btn := range r {
				out = append(out, btn.CallbackData)
			}
		}
	}
	for _, verb := range append(userMenuVerbs(), adminMenuVerbs()...) {
		out = append(out, "menu|"+verb)
	}
	return out
}

func userMenuVerbs() []string {
	return []string{menuMain, menuHelp, menuHelpText, menuPrivacy, menuMyUID, menuBug, menuBugSend, menuDonate}
}

func adminMenuVerbs() []string {
	return []string{
		menuAdmin, menuAdminStats, menuAdminReports, menuAdminDonate,
		menuAdminDonOn, menuAdminDonOff, menuAdminBackup, menuAdminCount, menuAdminCmds,
	}
}

// conversationContainsFilters are the substrings the ConversationHandlers (and the
// standalone cqContains handlers) match on. A menu datum containing one would be
// swallowed by a conversation instead of reaching menuCallback — a button that
// silently does nothing.
var conversationContainsFilters = []string{
	"settings-menu", "what-is-formatting",
	"mylinks-menu", "what-is-cid", "add-link", "more-links",
	"rm-custom-tag", "rm-audio-tag", "anon-name-set", "anon-name-remove",
	"anon-name-noemoji",
}

// otherPrefixFilters are prefixes other handlers already claim.
var otherPrefixFilters = []string{
	"errmore|", "rpt|", "am|", "no-callback", "delete|",
	"reply-quote|", "media-settings|", "change-name|", "custom-tag|", "audio-tag|",
	"wpp|", "warning|", "easier-answer|", "channel-signature|", "seen-settings|",
	"anon-name|", "name-sync|", "unblock-all|", "unblock-me|", "ch-link", "rm-link",
	"answer|", "seen|", "report|", "block|", "unblock|", "cancel",
	"oth|", "othx|",
}

// TestMenuDataDoesNotCollide is the guarantee that the panel's buttons actually
// reach the panel. This is the failure mode that would be invisible in review: the
// button renders, the tap goes to another handler, nothing happens.
func TestMenuDataDoesNotCollide(t *testing.T) {
	for _, data := range menuCallbackData() {
		if data == "" {
			t.Error("a menu button has empty callback_data")
			continue
		}

		// The two deliberate hand-offs into the existing conversations.
		if data == "settings-menu" || data == "mylinks-menu" {
			continue
		}

		if !strings.HasPrefix(data, "menu|") {
			t.Errorf("callback_data %q is neither a conversation entry point nor menu|-prefixed", data)
			continue
		}
		for _, sub := range conversationContainsFilters {
			if strings.Contains(data, sub) {
				t.Errorf("menu data %q contains %q, so a ConversationHandler would swallow it", data, sub)
			}
		}
		for _, p := range otherPrefixFilters {
			if strings.HasPrefix(data, p) {
				t.Errorf("menu data %q starts with %q, which another handler claims", data, p)
			}
		}
		if len(data) > 64 {
			t.Errorf("callback_data %q is %d bytes (>64)", data, len(data))
		}
	}
}

// TestMenuHandsOffToRealConversationEntryPoints pins the two buttons that must
// match the conversations' entry-point data EXACTLY. If either string drifted, the
// button would silently do nothing instead of opening links or settings.
func TestMenuHandsOffToRealConversationEntryPoints(t *testing.T) {
	kb := mainMenuKeyboard(false)
	if len(kb.InlineKeyboard) < 1 || len(kb.InlineKeyboard[0]) != 2 {
		t.Fatalf("main menu's first row = %v; want two buttons", kb.InlineKeyboard)
	}
	if got := kb.InlineKeyboard[0][0].CallbackData; got != "mylinks-menu" {
		t.Errorf("my-links button data = %q; want mylinks-menu (myLinksConversation's entry point)", got)
	}
	if got := kb.InlineKeyboard[0][1].CallbackData; got != "settings-menu" {
		t.Errorf("settings button data = %q; want settings-menu (settingsConversation's entry point)", got)
	}
}

// TestMenuAdminSectionIsAdminOnly checks the fifth section is hidden from
// non-admins AND gated in the handler — a hidden button is not a permission check.
func TestMenuAdminSectionIsAdminOnly(t *testing.T) {
	user := mainMenuKeyboard(false)
	for _, r := range user.InlineKeyboard {
		for _, btn := range r {
			if strings.Contains(btn.CallbackData, "menu|"+menuAdmin) {
				t.Errorf("non-admin menu exposes the admin section: %q", btn.CallbackData)
			}
		}
	}
	if got := countButtons(user); got != 4 {
		t.Errorf("non-admin menu has %d buttons; want 4", got)
	}
	if got := countButtons(mainMenuKeyboard(true)); got != 5 {
		t.Errorf("admin menu has %d buttons; want 5 (the 5th is the admin panel)", got)
	}

	// Every admin screen must be recognised by the gate. One missing from
	// isAdminVerb would be reachable by anyone who guessed its data.
	for _, verb := range adminMenuVerbs() {
		if !isAdminVerb(verb) {
			t.Errorf("isAdminVerb(%q) = false; that screen would be open to everyone", verb)
		}
	}
	// And no user-facing verb may be gated, which would lock users out of it.
	for _, verb := range userMenuVerbs() {
		if isAdminVerb(verb) {
			t.Errorf("isAdminVerb(%q) = true; ordinary users would be refused", verb)
		}
	}
}

// TestMenuEveryScreenIsReachable walks the panel like a user: from the main menu,
// following buttons, every screen must be arrived at, and every screen must offer a
// way back. A dead end is the classic menu bug.
func TestMenuEveryScreenIsReachable(t *testing.T) {
	// Screens reachable by following buttons from the two main menus and the admin
	// panel. The help sub-screen's buttons are built inline in the handler, so its
	// children are listed explicitly here — that list is what the handler renders.
	reachable := map[string]bool{}
	for _, kb := range []gotgbot.InlineKeyboardMarkup{mainMenuKeyboard(true), adminPanelKeyboard()} {
		for _, r := range kb.InlineKeyboard {
			for _, btn := range r {
				reachable[strings.TrimPrefix(btn.CallbackData, "menu|")] = true
			}
		}
	}
	// Children of the help screen, and the bug page's own child.
	for _, v := range []string{menuHelpText, menuPrivacy, menuMyUID, menuBug, menuBugSend} {
		reachable[v] = true
	}
	// Child of the donate-settings screen.
	reachable[menuAdminDonOn] = true
	reachable[menuAdminDonOff] = true

	for _, verb := range append(userMenuVerbs(), adminMenuVerbs()...) {
		if !reachable[verb] {
			t.Errorf("screen %q is handled but no button leads to it — dead code or a missing button", verb)
		}
	}

	// Back navigation must exist on the admin panel and every sub-screen keyboard
	// the helpers build.
	if !hasData(adminPanelKeyboard(), "menu|"+menuMain) {
		t.Error("the admin panel has no way back to the main menu")
	}
	if !hasDataInRow(backRow(), "menu|"+menuMain) {
		t.Error("backRow does not point at the main menu")
	}
}

func countButtons(kb gotgbot.InlineKeyboardMarkup) int {
	n := 0
	for _, r := range kb.InlineKeyboard {
		n += len(r)
	}
	return n
}

func hasData(kb gotgbot.InlineKeyboardMarkup, data string) bool {
	for _, r := range kb.InlineKeyboard {
		if hasDataInRow(r, data) {
			return true
		}
	}
	return false
}

func hasDataInRow(r []gotgbot.InlineKeyboardButton, data string) bool {
	for _, btn := range r {
		if btn.CallbackData == data {
			return true
		}
	}
	return false
}

// TestMenuBarFilter is the safety test for the bar. The tap arrives as an ordinary
// text message and the handler is registered ahead of the send state, so a filter
// that matched too much would swallow real messages — including somebody's
// anonymous message, which would be lost silently.
func TestMenuBarFilter(t *testing.T) {
	priv := func(text string) *gotgbot.Message {
		return &gotgbot.Message{Text: text, Chat: gotgbot.Chat{Type: "private"}}
	}

	if !menuBarFilter(priv(menuBarButton)) {
		t.Errorf("the bar's own label %q does not match its filter", menuBarButton)
	}
	// Telegram clients can pad the text; a tap must still register.
	if !menuBarFilter(priv("  " + menuBarButton + " ")) {
		t.Error("a padded bar tap did not match")
	}

	// Everything else must fall through to the normal handlers.
	for _, text := range []string{
		"", "منو", "🏠", "/menu", "سلام",
		menuBarButton + " چیه",    // the label inside a longer message
		"میخوام " + menuBarButton, // …and at the end
		"🏠 منوی اصلی",             // a near-miss label
	} {
		if menuBarFilter(priv(text)) {
			t.Errorf("filter claimed %q; a real message would be eaten", text)
		}
	}

	// Not a private chat, and a nil message, must never match.
	if menuBarFilter(&gotgbot.Message{Text: menuBarButton, Chat: gotgbot.Chat{Type: "supergroup"}}) {
		t.Error("the bar filter matched outside a private chat")
	}
	if menuBarFilter(nil) {
		t.Error("the bar filter matched a nil message")
	}
}

// TestMenuBarKeyboard pins the bar to ONE button — the whole point of the redesign
// was a single button, not a bar crowded with options.
func TestMenuBarKeyboard(t *testing.T) {
	kb := menuBarKeyboard()
	if len(kb.Keyboard) != 1 || len(kb.Keyboard[0]) != 1 {
		t.Fatalf("bar layout = %v; want exactly one button", kb.Keyboard)
	}
	if kb.Keyboard[0][0].Text != menuBarButton {
		t.Errorf("bar button = %q; want %q (must equal what the filter matches)",
			kb.Keyboard[0][0].Text, menuBarButton)
	}
	// MUST be false: true pins the bar open with no way to put it away, which is
	// wrong for a bot people mostly use to type. False lets the keyboard icon next
	// to the input box collapse and reopen it.
	if kb.IsPersistent {
		t.Error("the bar is persistent, so the user cannot hide it")
	}
	if !kb.ResizeKeyboard {
		t.Error("the bar is not resized, so it takes a full keyboard's height")
	}
}

// TestSettingsAndLinksCanReachTheMenu covers the gap that was reported: entering
// settings from the panel left no way back.
func TestSettingsAndLinksCanReachTheMenu(t *testing.T) {
	home := "menu|" + menuMain

	if !hasData(ikb(settingsMainMenu()...), home) {
		t.Error("the settings menu has no way back to /menu — a dead end")
	}
	if !hasData(ikb(mylinksDefaultMenu()...), home) {
		t.Error("the my_links menu has no way back to /menu — a dead end")
	}
}

// TestModerationScreensHaveAnExit checks the moderation list is not a dead end
// either, on both the /admin_reports path and the panel path.
func TestModerationScreensHaveAnExit(t *testing.T) {
	if modBackToPanel().CallbackData != "menu|"+menuAdmin {
		t.Errorf("the moderation exit points at %q; want the admin panel",
			modBackToPanel().CallbackData)
	}
}

// TestCommandListLeadsWithMenu pins the shape of the published command list.
// /menu is the entry to everything else, so it must be the first thing a user
// sees in the autocomplete without scrolling; an empty list would take the blue
// "Menu" button away again and strand anyone who collapsed the 🏠 منو bar.
func TestCommandListLeadsWithMenu(t *testing.T) {
	got := botCommands()
	if len(got) == 0 {
		t.Fatal("botCommands() is empty; Telegram would show no command list and no blue Menu button")
	}
	if got[0].Command != "menu" {
		t.Errorf("the command list starts with /%s; /menu must be first", got[0].Command)
	}
	for _, c := range got {
		if c.Description == "" {
			t.Errorf("/%s has no description; Telegram shows the list with a blank line", c.Command)
		}
	}
}

// TestPublicCommandsHideTheAdminSurface guards the reason the admin commands are
// published per-admin instead of in the default scope: the default list reaches
// every user, so an admin entry slipping in advertises the moderation surface to
// all of them.
func TestPublicCommandsHideTheAdminSurface(t *testing.T) {
	for _, c := range botCommands() {
		if strings.HasPrefix(c.Command, "admin") {
			t.Errorf("/%s is in the PUBLIC command list; admin commands belong in adminBotCommands only", c.Command)
		}
	}
}

// TestAdminCommandsExtendTheUserOnes checks an admin still gets the ordinary
// commands. Their scoped list REPLACES the default one in their chat rather than
// adding to it, so building it from anything but botCommands() would silently
// take /menu and friends away from exactly the people who use them most.
func TestAdminCommandsExtendTheUserOnes(t *testing.T) {
	admin := adminBotCommands()
	has := func(name string) bool {
		for _, c := range admin {
			if c.Command == name {
				return true
			}
		}
		return false
	}
	for _, c := range botCommands() {
		if !has(c.Command) {
			t.Errorf("an admin's command list is missing /%s, which every user has", c.Command)
		}
	}
	if !has("admin") {
		t.Error("an admin's command list has no /admin")
	}
	if len(admin) <= len(botCommands()) {
		t.Error("adminBotCommands() adds nothing to the public list")
	}
}

// TestHelpTextMentionsTheMenuCommand pins /menu into the guide. The bar can be
// collapsed, so the guide has to name a way in that survives that.
func TestHelpTextMentionsTheMenuCommand(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "Texts", "start_help.txt"))
	if err != nil {
		t.Fatalf("reading Texts/start_help.txt: %v", err)
	}
	if !strings.Contains(string(raw), "/menu") {
		t.Error("the guide never mentions /menu, so a user whose bar is collapsed is told nothing")
	}
}

// TestStartGuideMentionsTheBar guards the guide text against pointing at UI that no
// longer exists. It used to say "use the menu at the bottom left", which was the
// blue button — removing that button made the instruction actively wrong.
func TestStartGuideMentionsTheBar(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "Texts", "start_help.txt"))
	if err != nil {
		t.Fatalf("reading Texts/start_help.txt: %v", err)
	}
	txt := string(raw)

	if strings.Contains(txt, "پایین سمت چپ") {
		t.Error("the guide still points at the bottom-left menu, which no longer exists")
	}
	if !strings.Contains(txt, menuBarButton) {
		t.Errorf("the guide does not mention the %q button, so a new user is not told how to navigate", menuBarButton)
	}
	// And it must say how to get the bar back if it has been collapsed, since the
	// bar is now hideable.
	if !strings.Contains(txt, "کیبورد") {
		t.Error("the guide does not explain how to reopen the bar after hiding it")
	}
}

// TestHelpSubPagesReturnToHelp covers the reported navigation bug: back from a help
// sub-page jumped to the MAIN menu instead of the help menu it came from.
func TestHelpSubPagesReturnToHelp(t *testing.T) {
	r := backRowTo(menuHelp, "↩️ برگشت به راهنما")
	if len(r) != 1 || r[0].CallbackData != "menu|"+menuHelp {
		t.Fatalf("backRowTo(help) = %+v; want a single button targeting menu|%s", r, menuHelp)
	}
	// The generic back row must still exist for screens that DO belong at the top.
	if backRow()[0].CallbackData != "menu|"+menuMain {
		t.Errorf("backRow no longer targets the main menu: %q", backRow()[0].CallbackData)
	}
}

// TestNameSyncButtonReachesItsHandler covers the same invisible failure mode as
// TestMenuDataDoesNotCollide, for the settings side: "🔄 هم‌گام‌سازی نام با
// اکانت" is registered with Prefix("name-sync|"), and it sits next to
// "anon-name|" and "change-name|" — three name-ish buttons whose filters must not
// overlap, or one of them silently opens another one's page.
func TestNameSyncButtonReachesItsHandler(t *testing.T) {
	const data = "name-sync|"

	var found bool
	for _, r := range settingsMainMenu() {
		for _, btn := range r {
			if btn.CallbackData == data {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("no settings button carries %q, so the name-sync page is unreachable", data)
	}

	// Nothing registered ahead of it may claim its data...
	for _, p := range otherPrefixFilters {
		if p == data {
			continue
		}
		if strings.HasPrefix(data, p) {
			t.Errorf("%q starts with %q, which another handler claims first", data, p)
		}
	}
	for _, sub := range conversationContainsFilters {
		if strings.Contains(data, sub) {
			t.Errorf("%q contains %q, so a cqContains handler would swallow it", data, sub)
		}
	}
	// ...and it must not claim anybody else's, in either direction.
	for _, other := range otherPrefixFilters {
		if other != data && strings.HasPrefix(other, data) {
			t.Errorf("%q would swallow %q", data, other)
		}
	}

	// Both flip variants have to route to the same page, so they must share the
	// prefix the handler is registered under.
	for _, k := range []string{"name-sync-activate", "name-sync-deactivate"} {
		got := settingsButtons[k].CallbackData
		if !strings.HasPrefix(got, data) {
			t.Errorf("settingsButtons[%q] is %q; it must start with %q to reach the handler", k, got, data)
		}
	}
}

// TestNameSyncIsSeparateFromTheAnonNickname guards the distinction users are
// most likely to conflate, and that the settings page spells out: the display
// name is what somebody sees when messaging YOU, the anonymous nickname is a
// signature on what you SEND. They are different settings on different columns,
// so their buttons must stay different too.
func TestNameSyncIsSeparateFromTheAnonNickname(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range settingsMainMenu() {
		for _, btn := range r {
			if btn.CallbackData == "" {
				t.Errorf("settings button %q has no callback_data", btn.Text)
				continue
			}
			if seen[btn.CallbackData] {
				t.Errorf("two settings buttons share callback_data %q", btn.CallbackData)
			}
			seen[btn.CallbackData] = true
		}
	}
	for _, want := range []string{"name-sync|", "anon-name|", "change-name|"} {
		if !seen[want] {
			t.Errorf("the settings menu lost the %q button", want)
		}
	}
}
