package db

import (
	"context"
	"time"
)

// AddUser upserts a user with the Python defaults (not banned, warning on, no
// seen, wpp on, default cid_limit, no custom_tag, default audio_tag, no
// chevaletid). Returns true if a new row was inserted, false if uid existed.
// Mirrors DBHandler.add_user.
func (db *DB) AddUser(ctx context.Context, uid, name string) (bool, error) {
	name = truncateRunes(name, db.maxNameLength)
	tag, err := db.pool.Exec(ctx,
		`INSERT INTO users
			(uid, name, is_banned, warning, seen_option, wpp, cid_limit, custom_tag, audio_tag, chevaletid)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		 ON CONFLICT (uid) DO NOTHING`,
		uid, name, false, true, false, true, db.defaultCIDLimit, nil, db.defaultAudioTag, nil,
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// IsBanned mirrors DBHandler.is_banned.
func (db *DB) IsBanned(ctx context.Context, uid string) (bool, error) {
	return db.queryBool(ctx, `SELECT is_banned FROM users WHERE uid=$1`, uid)
}

// BanAction sets/clears a user's ban flag, first ensuring the row exists (so an
// admin can ban a uid that never started the bot). Mirrors DBHandler.ban_action.
func (db *DB) BanAction(ctx context.Context, uid string, ban bool) error {
	_, _ = db.AddUser(ctx, uid, "-") // ignore error, matching the Python try/except
	_, err := db.pool.Exec(ctx, `UPDATE users SET is_banned=$1 WHERE uid=$2`, ban, uid)
	return err
}

// GetAllUIDs returns every user's uid. Mirrors DBHandler.get_all_uids.
func (db *DB) GetAllUIDs(ctx context.Context) ([]string, error) {
	return db.queryStrings(ctx, `SELECT uid FROM users`)
}

// GetName mirrors DBHandler.get_name ("" when missing).
func (db *DB) GetName(ctx context.Context, uid string) (string, error) {
	return db.queryOptString(ctx, `SELECT name FROM users WHERE uid=$1`, uid)
}

// GetUIDByChevaletID mirrors DBHandler.get_uid_by_chevaletid.
func (db *DB) GetUIDByChevaletID(ctx context.Context, chevaletid string) (string, error) {
	return db.queryOptString(ctx, `SELECT uid FROM users WHERE chevaletid=$1`, chevaletid)
}

// GetChevaletIDByUID mirrors DBHandler.get_chevaletid_by_uid ("" when missing or NULL).
func (db *DB) GetChevaletIDByUID(ctx context.Context, uid string) (string, error) {
	return db.queryOptString(ctx, `SELECT chevaletid FROM users WHERE uid=$1`, uid)
}

// GetAllChevaletIDs returns every non-NULL chevaletid. Mirrors
// DBHandler.get_all_chevaletids (used for cid uniqueness checks).
func (db *DB) GetAllChevaletIDs(ctx context.Context) ([]string, error) {
	return db.queryStrings(ctx, `SELECT chevaletid FROM users WHERE chevaletid IS NOT NULL`)
}

// GetCIDLimit mirrors DBHandler.get_cid_limit.
func (db *DB) GetCIDLimit(ctx context.Context, uid string) (int, error) {
	var n int
	err := db.pool.QueryRow(ctx, `SELECT cid_limit FROM users WHERE uid=$1`, uid).Scan(&n)
	return n, err
}

// GetWarning mirrors DBHandler.get_warning.
func (db *DB) GetWarning(ctx context.Context, uid string) (bool, error) {
	return db.queryBool(ctx, `SELECT warning FROM users WHERE uid=$1`, uid)
}

// GetSeenStatus mirrors DBHandler.get_seen_status.
func (db *DB) GetSeenStatus(ctx context.Context, uid string) (bool, error) {
	return db.queryBool(ctx, `SELECT seen_option FROM users WHERE uid=$1`, uid)
}

// GetWPP mirrors DBHandler.get_wpp.
func (db *DB) GetWPP(ctx context.Context, uid string) (bool, error) {
	return db.queryBool(ctx, `SELECT wpp FROM users WHERE uid=$1`, uid)
}

// GetCustomTag mirrors DBHandler.get_custom_tag ("" when NULL/missing).
func (db *DB) GetCustomTag(ctx context.Context, uid string) (string, error) {
	return db.queryOptString(ctx, `SELECT custom_tag FROM users WHERE uid=$1`, uid)
}

// GetAudioTag mirrors DBHandler.get_audio_tag ("" when NULL/missing).
func (db *DB) GetAudioTag(ctx context.Context, uid string) (string, error) {
	return db.queryOptString(ctx, `SELECT audio_tag FROM users WHERE uid=$1`, uid)
}

// SetName mirrors DBHandler.set_name.
func (db *DB) SetName(ctx context.Context, uid, name string) error {
	_, err := db.pool.Exec(ctx, `UPDATE users SET name=$1 WHERE uid=$2`, name, uid)
	return err
}

// SetCIDLimit mirrors DBHandler.set_cid_limit.
func (db *DB) SetCIDLimit(ctx context.Context, uid string, cidLimit int) error {
	_, err := db.pool.Exec(ctx, `UPDATE users SET cid_limit=$1 WHERE uid=$2`, cidLimit, uid)
	return err
}

// SetWarning mirrors DBHandler.set_warning.
func (db *DB) SetWarning(ctx context.Context, uid string, warning bool) error {
	_, err := db.pool.Exec(ctx, `UPDATE users SET warning=$1 WHERE uid=$2`, warning, uid)
	return err
}

// SetSeenOption mirrors DBHandler.set_seen_option.
func (db *DB) SetSeenOption(ctx context.Context, uid string, seen bool) error {
	_, err := db.pool.Exec(ctx, `UPDATE users SET seen_option=$1 WHERE uid=$2`, seen, uid)
	return err
}

// SetWPP mirrors DBHandler.set_wpp.
func (db *DB) SetWPP(ctx context.Context, uid string, wpp bool) error {
	_, err := db.pool.Exec(ctx, `UPDATE users SET wpp=$1 WHERE uid=$2`, wpp, uid)
	return err
}

// SetCustomTag mirrors DBHandler.set_custom_tag. A nil tag clears the column
// (stores NULL), matching set_custom_tag(uid, None).
func (db *DB) SetCustomTag(ctx context.Context, uid string, tag *string) error {
	_, err := db.pool.Exec(ctx, `UPDATE users SET custom_tag=$1 WHERE uid=$2`, tag, uid)
	return err
}

// SetAudioTag mirrors DBHandler.set_audio_tag. A nil tag stores NULL.
func (db *DB) SetAudioTag(ctx context.Context, uid string, tag *string) error {
	_, err := db.pool.Exec(ctx, `UPDATE users SET audio_tag=$1 WHERE uid=$2`, tag, uid)
	return err
}

// SetChevaletID mirrors DBHandler.set_chevaletid (which always returned True;
// here a real failure surfaces as a non-nil error instead).
func (db *DB) SetChevaletID(ctx context.Context, uid, chevaletid string) error {
	_, err := db.pool.Exec(ctx, `UPDATE users SET chevaletid=$1 WHERE uid=$2`, chevaletid, uid)
	return err
}

// UserStatus returns (is_banned, cid_limit). Mirrors DBHandler.user_status.
func (db *DB) UserStatus(ctx context.Context, uid string) (isBanned bool, cidLimit int, err error) {
	err = db.pool.QueryRow(ctx,
		`SELECT is_banned, cid_limit FROM users WHERE uid=$1`, uid,
	).Scan(&isBanned, &cidLimit)
	return
}

// UserCount mirrors DBHandler.user_count (also used by the hourly DB health check).
func (db *DB) UserCount(ctx context.Context) (int, error) {
	var n int
	err := db.pool.QueryRow(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// --- anonymous nickname (optional per-sender signature) ----------------------
//
// anon_name / anon_emoji are the user-chosen pseudonym and its optional leading
// emoji; anon_enabled is the on/off switch. All three default to unset/FALSE, so
// a user is opted OUT until they configure and enable it. Reads go through
// GetAnonSig (a single round-trip); the three setters write each column.

// GetAnonSig fetches all three nickname fields in a single round-trip for the
// hot send path. A missing row or NULL name/emoji map to the zero values (the
// send path always runs for an initialised user, so a missing row is only a
// defensive case). enabled reflects anon_enabled verbatim.
func (db *DB) GetAnonSig(ctx context.Context, uid string) (enabled bool, name, emoji string, err error) {
	var n, e *string
	err = db.pool.QueryRow(ctx,
		`SELECT anon_enabled, anon_name, anon_emoji FROM users WHERE uid=$1`, uid,
	).Scan(&enabled, &n, &e)
	if IsNoRows(err) {
		return false, "", "", nil
	}
	if err != nil {
		return false, "", "", err
	}
	if n != nil {
		name = *n
	}
	if e != nil {
		emoji = *e
	}
	return enabled, name, emoji, nil
}

// SetAnonName sets/clears the nickname. A nil name stores NULL.
func (db *DB) SetAnonName(ctx context.Context, uid string, name *string) error {
	_, err := db.pool.Exec(ctx, `UPDATE users SET anon_name=$1 WHERE uid=$2`, name, uid)
	return err
}

// SetAnonEmoji sets/clears the leading emoji. A nil emoji stores NULL.
func (db *DB) SetAnonEmoji(ctx context.Context, uid string, emoji *string) error {
	_, err := db.pool.Exec(ctx, `UPDATE users SET anon_emoji=$1 WHERE uid=$2`, emoji, uid)
	return err
}

// SetAnonEnabled turns the nickname signature on or off.
func (db *DB) SetAnonEnabled(ctx context.Context, uid string, enabled bool) error {
	_, err := db.pool.Exec(ctx, `UPDATE users SET anon_enabled=$1 WHERE uid=$2`, enabled, uid)
	return err
}

// MenuBarSent reports whether the persistent "🏠 منو" bar has already been
// installed in this user's chat. See the column's comment in MakeTables: a
// ReplyKeyboard can only arrive attached to a message, so sending it is a one-time
// event that has to be remembered.
func (db *DB) MenuBarSent(ctx context.Context, uid string) (bool, error) {
	var sent bool
	err := db.pool.QueryRow(ctx,
		`SELECT COALESCE(menu_bar_sent, FALSE) FROM users WHERE uid=$1`, uid).Scan(&sent)
	return sent, err
}

// SetMenuBarSent marks the bar as installed for this user.
func (db *DB) SetMenuBarSent(ctx context.Context, uid string) error {
	_, err := db.pool.Exec(ctx,
		`UPDATE users SET menu_bar_sent = TRUE WHERE uid=$1`, uid)
	return err
}

// --- display-name syncing ----------------------------------------------------
//
// A user's display name is what OTHERS see when they open that user's anonymous
// link. It was set once, from their Telegram account name, when they first
// started the bot — so renaming the account left a stale name behind forever
// unless the user went into /settings and retyped it. name_sync fixes that.
//
// THE RULE THAT PROTECTS EXISTING USERS. The column defaults to TRUE, but a
// TRUE flag alone never renames anybody: SyncDisplayName only copies the account
// name across once it has SEEN that name change. On the very first sighting of a
// user's account name it records a baseline and renames nothing — and if the
// stored display name already differs from the account name at that moment, it
// turns the sync OFF for that user, because a diverged name is the signature of
// somebody who set it deliberately (the rename page tells users they can format
// it and link it to their channel). Every user who predates the feature
// therefore keeps exactly the name they have.

// nameSyncMinInterval bounds how often the baseline timestamp is rewritten. The
// sync runs on EVERY update (see initUser), so without this the statement would
// write a row every time instead of only when something actually moved. An hour
// is far below the staleness window a reader cares about (see GetDisplayName)
// and far above the update rate of any one user.
const nameSyncMinInterval = time.Hour

// SyncDisplayName records uid's current Telegram account name and, when that
// user has syncing on and the name has actually CHANGED since the last sighting,
// copies it into their display name.
//
// It is one statement on purpose: every branch of the rule reads the row's
// pre-update values, so the "first sighting" and "already diverged" cases cannot
// race each other, and the common case (nothing changed, seen recently) matches
// no row and writes nothing — the same trick TouchUser uses.
//
// Returns the display name now in force, or "" when the statement was a no-op
// and the caller's existing copy is still current.
func (db *DB) SyncDisplayName(ctx context.Context, uid, tgName string) (string, error) {
	tgName = truncateRunes(tgName, db.maxNameLength)

	var name string
	err := db.pool.QueryRow(ctx,
		`UPDATE users SET
		     tg_name    = $2,
		     tg_name_at = now(),
		     -- First sighting of this user's account name AND their display name
		     -- already differs from it: they chose that name. Opt them out.
		     name_sync = CASE
		         WHEN tg_name IS NULL AND name <> $2 THEN FALSE
		         ELSE name_sync END,
		     -- Rename only on a SEEN change: tg_name must already be recorded (so
		     -- this is not the first sighting) and must actually differ.
		     name = CASE
		         WHEN tg_name IS NOT NULL AND name_sync AND tg_name <> $2 THEN $2
		         ELSE name END
		   WHERE uid = $1
		     AND (tg_name IS DISTINCT FROM $2
		          OR tg_name_at IS NULL
		          OR tg_name_at < now() - $3::interval)
		 RETURNING name`,
		uid, tgName, nameSyncMinInterval.String(),
	).Scan(&name)
	if IsNoRows(err) {
		return "", nil // nothing needed writing
	}
	if err != nil {
		return "", err
	}
	return name, nil
}

// DisplayName is what a send path needs to render a target's name — and to
// decide whether it is worth one Telegram call to refresh it first.
type DisplayName struct {
	Name string
	// Sync is the user's name_sync setting. False means the name is theirs and
	// must be shown as-is.
	Sync bool
	// Stale is true when this user's account name has never been recorded, or was
	// last looked at longer ago than the caller's TTL. Only then is a refresh
	// worth an API round-trip.
	Stale bool
}

// GetDisplayName fetches a target's display name together with everything needed
// to decide on refreshing it, in a single round-trip. ttl is how old the last
// sighting may be before Stale is reported.
//
// A missing row yields the zero value with no error: the send paths only ever
// ask about an initialised user, so this is defensive.
func (db *DB) GetDisplayName(ctx context.Context, uid string, ttl time.Duration) (DisplayName, error) {
	var d DisplayName
	err := db.pool.QueryRow(ctx,
		`SELECT name,
		        COALESCE(name_sync, FALSE),
		        (tg_name_at IS NULL OR tg_name_at < now() - $2::interval)
		   FROM users WHERE uid = $1`,
		uid, ttl.String(),
	).Scan(&d.Name, &d.Sync, &d.Stale)
	if IsNoRows(err) {
		return DisplayName{}, nil
	}
	return d, err
}

// GetNameSync reports whether uid has display-name syncing on.
func (db *DB) GetNameSync(ctx context.Context, uid string) (bool, error) {
	return db.queryBool(ctx, `SELECT name_sync FROM users WHERE uid=$1`, uid)
}

// SetNameSync turns display-name syncing on or off for uid.
func (db *DB) SetNameSync(ctx context.Context, uid string, on bool) error {
	_, err := db.pool.Exec(ctx, `UPDATE users SET name_sync=$1 WHERE uid=$2`, on, uid)
	return err
}

// ApplyNameSync copies the last-seen account name into the display name right
// now, for a user who has syncing on.
//
// Turning the setting on is a request for the two to match, so it has to take
// effect immediately rather than at the user's next account rename — otherwise
// the toggle appears to do nothing. Returns the display name in force
// afterwards (unchanged when there was nothing to apply).
func (db *DB) ApplyNameSync(ctx context.Context, uid string) (string, error) {
	var name string
	err := db.pool.QueryRow(ctx,
		`UPDATE users SET name = tg_name
		   WHERE uid = $1 AND name_sync AND tg_name IS NOT NULL AND name <> tg_name
		 RETURNING name`, uid).Scan(&name)
	if IsNoRows(err) {
		// Nothing to apply (syncing off, never seen, or already equal) — report the
		// name as it stands so the caller can render it either way.
		return db.GetName(ctx, uid)
	}
	if err != nil {
		return "", err
	}
	return name, nil
}
