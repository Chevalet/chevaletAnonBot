package bot

import (
	"strings"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/PaulSonOfLars/gotgbot/v2/ext"
)

// /admin_menurich — the switch for the embedded-button (rich) /menu panel.
//
//	/admin_menurich          -> show the current state
//	/admin_menurich on       -> draw the panel with buttons inside the text
//	/admin_menurich off      -> classic inline keyboard (the default)
//
// This exists as a command rather than a build-time choice because it is the
// rollback path. Embedded buttons need a Bot API 10.3-era client and a rich
// message has no fallback for older ones (see richmenu.go), so if reports come in
// that the menu looks empty, an admin turns it off in one message and every user
// is back on the classic keyboard immediately — no redeploy, no restart.
func adminMenuRichCmd(b *Bot, _ *gotgbot.Bot, ctx *ext.Context, userid string) error {
	if !b.isAdmin(userid) {
		// Same as the other admin commands: a non-admin must not learn it exists.
		return b.otherMessagesTemplate(ctx)
	}

	fields := strings.Fields(ctx.EffectiveMessage.Text)
	if len(fields) > 1 {
		switch strings.ToLower(fields[1]) {
		case "on", "active", "enable":
			b.Dyn.SetMenuRichEnabled(true)
		case "off", "deactive", "disable":
			b.Dyn.SetMenuRichEnabled(false)
		default:
			return b.replyHTML(ctx, "wrong syntax.\n\n"+b.menuRichStatus(), false)
		}
	}
	return b.replyHTML(ctx, b.menuRichStatus(), false)
}

// menuRichStatus renders the current state plus the warning an admin needs before
// turning it on.
func (b *Bot) menuRichStatus() string {
	state := "❌ off — classic inline keyboard (default)"
	if b.Dyn.MenuRichEnabled() {
		state = "✅ on — buttons embedded in the message text"
	}
	return "menu style: " + state +
		"\n\n⚠️ embedded buttons need a recent Telegram client." +
		"\nOlder clients cannot render a rich message at all (it carries no" +
		" plain-text fallback), so they would see a menu with nothing to tap." +
		"\n\n<code>/admin_menurich on|off</code>"
}
