package bot

import (
	"log/slog"
	"strconv"
	"time"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// Display-name syncing, read side. The write side lives in initUser (which sees
// every user's account name for free on every update they send) and the rules
// themselves live in db.SyncDisplayName.
//
// This file covers the case initUser cannot: showing SOMEBODY ELSE's name. When
// A opens B's anonymous link, the bot prints B's display name — but B may not
// have touched the bot since renaming their Telegram account, so nothing has had
// the chance to notice. Here the bot asks Telegram directly, just before it
// prints the name, so a rename cannot slip through.

const (
	// nameRefreshTTL is how stale a target's recorded account name may be before
	// displayName spends a Telegram call refreshing it.
	//
	// The bot's dispatcher is SERIAL (MaxRoutines=1), so this call sits in front of
	// every other user's updates — which is why it is rationed rather than made on
	// every view. Six hours makes it at most one extra call per target per six
	// hours, and a user active in the bot never reaches it at all: their own
	// updates keep the record fresh through initUser for free.
	nameRefreshTTL = 6 * time.Hour

	// nameRefreshTimeout bounds that call hard. It is a nicety on the way to
	// showing a name we ALREADY have, so it must never be the thing that makes the
	// bot look hung; on timeout the stored name is used.
	nameRefreshTimeout = 3 * time.Second
)

// displayName returns the name to show for userid, refreshing it from Telegram
// first when that user syncs their name and the bot's copy has gone stale.
//
// Errors from the refresh are swallowed: the stored name is always a usable
// answer, and failing to show a message because a convenience lookup failed
// would be a far worse outcome. Only a database error is returned, matching what
// the callers did when they read the name with GetName.
func (b *Bot) displayName(userid string) (string, error) {
	dbctx, cancel := b.bg()
	defer cancel()

	d, err := b.DB.GetDisplayName(dbctx, userid, nameRefreshTTL)
	if err != nil {
		return "", err
	}
	if !d.Sync || !d.Stale {
		return d.Name, nil
	}

	fresh, ok := b.fetchAccountName(userid)
	if !ok {
		return d.Name, nil
	}

	// Hand it to the same statement initUser uses, so the "first sighting does not
	// rename" and "diverged means they chose it" rules hold no matter which path
	// happens to observe a user's account name first.
	synced, serr := b.DB.SyncDisplayName(dbctx, userid, fresh)
	if serr != nil {
		slog.Warn("could not sync a display name before showing it", "err", serr)
		return d.Name, nil
	}
	if synced == "" {
		return d.Name, nil // nothing needed writing
	}
	return synced, nil
}

// fetchAccountName asks Telegram for a user's current account name. The bool
// reports whether the answer is usable — a failed lookup, or an account with no
// first name at all, must not overwrite a name the bot already has.
func (b *Bot) fetchAccountName(userid string) (string, bool) {
	id, err := strconv.ParseInt(userid, 10, 64)
	if err != nil {
		return "", false
	}
	chat, err := b.TG.GetChat(id, &gotgbot.GetChatOpts{
		RequestOpts: &gotgbot.RequestOpts{Timeout: nameRefreshTimeout},
	})
	if err != nil {
		// Expected in normal operation: the user may have blocked the bot, and the
		// call is also subject to the outbound rate limiter. Not an incident.
		slog.Info("could not refresh an account name", "err", err)
		return "", false
	}
	name := chat.FirstName
	if chat.LastName != "" {
		name += " " + chat.LastName
	}
	if name == "" {
		return "", false
	}
	return name, true
}
