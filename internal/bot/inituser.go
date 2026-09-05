package bot

import (
	"context"
	"errors"
	"log/slog"

	"github.com/PaulSonOfLars/gotgbot/v2"

	"github.com/aturzone/chevaletAnonBot/internal/encoder"
)

// errInitNoCID signals that initUser could not allocate a unique cid for the
// user after config.MaxTryAddCID attempts — the Go equivalent of the Python
// init_user returning False. prep turns it into the user-facing "couldn't make
// an anonymous link" reply (decorators.py:69-74).
var errInitNoCID = errors.New("init: could not allocate a unique cid")

// tgFullName is the user's Telegram account name as one string — the same
// "first last" shape the Python bot's full_name produced.
func tgFullName(u *gotgbot.User) string {
	name := u.FirstName
	if u.LastName != "" {
		name += " " + u.LastName
	}
	return name
}

// initUser mirrors modules/Global/user_init.py plus the chevaletid bootstrap
// from @prep_function: it upserts the user, ensures they have at least one cid,
// and ensures they have a chevaletid.
//
// Unlike the Python original (which called bot.get_chat(uid) on every update to
// fetch full_name) this uses the triggering user's name straight from the
// update — the same person, no extra API round-trip. add_user is an upsert that
// does nothing on conflict, so it only ever sets the name on first insert; the
// SyncDisplayName call below is what keeps it current afterwards, and it is the
// one place that decides whether a rename is wanted (see its doc comment — it
// deliberately does NOT rename a user whose display name they set themselves).
func (b *Bot) initUser(ctx context.Context, userid string, u *gotgbot.User) error {
	name := tgFullName(u)

	if _, err := b.DB.AddUser(ctx, userid, name); err != nil {
		return err
	}

	// Keep the display name in step with the account name. Best effort on purpose:
	// this is a convenience, and a hiccup here must never stop the user's update
	// from being handled. The statement writes no row in the common case.
	if _, serr := b.DB.SyncDisplayName(ctx, userid, name); serr != nil {
		slog.Warn("could not sync a display name", "err", serr)
	}

	cids, err := b.DB.GetCIDs(ctx, userid)
	if err != nil {
		return err
	}
	if len(cids) == 0 {
		ok, err := b.DB.AddCID(ctx, userid, encoder.GenerateCID(10))
		if err != nil {
			return err
		}
		if !ok {
			// every cid attempt collided (astronomically unlikely) — mirror Python
			// init_user returning False so prep can tell the user to retry.
			return errInitNoCID
		}
	}

	chev, err := b.DB.GetChevaletIDByUID(ctx, userid)
	if err != nil {
		return err
	}
	if chev == "" {
		if err := b.DB.SetChevaletID(ctx, userid, encoder.GenerateChevaletID()); err != nil {
			return err
		}
	}
	return nil
}
