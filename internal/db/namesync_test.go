package db

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The rules under test, from SyncDisplayName's doc comment:
//
//  1. The FIRST time the bot sees a user's Telegram account name it records a
//     baseline and renames nobody.
//  2. If, at that first sighting, the stored display name already differs from
//     the account name, that user set their name by hand — syncing is switched
//     off for them and their name is left alone, forever.
//  3. Afterwards, a SEEN change of the account name is copied into the display
//     name, but only while syncing is on.
//
// Rule 2 is what protects the ~17k users who predate this feature, so it is the
// one most worth breaking the build over.

// syncState reads the three columns the rules turn on.
func syncState(t *testing.T, d *DB, ctx context.Context, uid string) (name string, sync bool, tgName string) {
	t.Helper()
	var tg *string
	err := d.pool.QueryRow(ctx,
		`SELECT name, name_sync, tg_name FROM users WHERE uid=$1`, uid).Scan(&name, &sync, &tg)
	noErr(t, err, "read sync state")
	if tg != nil {
		tgName = *tg
	}
	return
}

func TestSyncDisplayNameDefaultsOnForNewUsers(t *testing.T) {
	d, ctx := freshDB(t)

	_, err := d.AddUser(ctx, "new1", "Sara")
	noErr(t, err, "AddUser")

	on, err := d.GetNameSync(ctx, "new1")
	noErr(t, err, "GetNameSync")
	if !on {
		t.Fatal("a new user has name syncing OFF; it must default to ON")
	}
}

// Rules 1 + 3: a user whose display name matches their account name gets the
// baseline, then follows every later rename.
func TestSyncDisplayNameFollowsAccountRenames(t *testing.T) {
	d, ctx := freshDB(t)
	_, err := d.AddUser(ctx, "u1", "Sara")
	noErr(t, err, "AddUser")

	// First sighting: baseline recorded, nothing renamed.
	got, err := d.SyncDisplayName(ctx, "u1", "Sara")
	noErr(t, err, "first sighting")
	if got != "Sara" {
		t.Fatalf("first sighting returned %q; want Sara", got)
	}
	name, sync, tg := syncState(t, d, ctx, "u1")
	if name != "Sara" || !sync || tg != "Sara" {
		t.Fatalf("after the first sighting: name=%q sync=%v tg_name=%q; want Sara/true/Sara", name, sync, tg)
	}

	// The account is renamed. That is a SEEN change, so it is copied across.
	got, err = d.SyncDisplayName(ctx, "u1", "Sara Ahmadi")
	noErr(t, err, "rename")
	if got != "Sara Ahmadi" {
		t.Fatalf("rename returned %q; want Sara Ahmadi", got)
	}
	name, sync, tg = syncState(t, d, ctx, "u1")
	if name != "Sara Ahmadi" || !sync || tg != "Sara Ahmadi" {
		t.Fatalf("after a rename: name=%q sync=%v tg_name=%q; want Sara Ahmadi/true/Sara Ahmadi", name, sync, tg)
	}
}

// RULE 2 — the guarantee for existing users. A row whose display name was chosen
// by hand must survive the first sighting untouched, and must be opted out so no
// later rename touches it either.
func TestSyncDisplayNameNeverOverwritesAChosenName(t *testing.T) {
	d, ctx := freshDB(t)

	// An existing user: row created long ago, display name since customised, and
	// tg_name still NULL because this feature did not exist yet.
	const chosen = `<a href="https://t.me/mychannel">کانال من</a>`
	_, err := d.AddUser(ctx, "old1", "Reza")
	noErr(t, err, "AddUser")
	noErr(t, d.SetName(ctx, "old1", chosen), "SetName")

	got, err := d.SyncDisplayName(ctx, "old1", "Reza Mohammadi")
	noErr(t, err, "first sighting")

	name, sync, tg := syncState(t, d, ctx, "old1")
	if name != chosen {
		t.Fatalf("a hand-picked display name was overwritten with %q", name)
	}
	if sync {
		t.Fatal("a user with a diverged display name was left with syncing ON; a later rename would wipe their name")
	}
	if tg != "Reza Mohammadi" {
		t.Fatalf("tg_name = %q; the baseline must still be recorded so the toggle works later", tg)
	}
	if got != name {
		t.Fatalf("SyncDisplayName returned %q but the stored name is %q", got, name)
	}

	// And a later rename must still not touch it.
	_, err = d.SyncDisplayName(ctx, "old1", "Reza Tehrani")
	noErr(t, err, "later rename")
	name, _, _ = syncState(t, d, ctx, "old1")
	if name != chosen {
		t.Fatalf("a later rename overwrote an opted-out user's name with %q", name)
	}
}

// Syncing off means off, even for a user who never diverged.
func TestSyncDisplayNameRespectsTheOffSwitch(t *testing.T) {
	d, ctx := freshDB(t)
	_, err := d.AddUser(ctx, "u2", "Ali")
	noErr(t, err, "AddUser")
	_, err = d.SyncDisplayName(ctx, "u2", "Ali") // baseline
	noErr(t, err, "baseline")
	noErr(t, d.SetNameSync(ctx, "u2", false), "SetNameSync off")

	_, err = d.SyncDisplayName(ctx, "u2", "Ali Karimi")
	noErr(t, err, "rename while off")

	name, _, tg := syncState(t, d, ctx, "u2")
	if name != "Ali" {
		t.Fatalf("name = %q; syncing was off, so it must still be Ali", name)
	}
	// The baseline still moves, so turning the toggle back on is immediately
	// right rather than needing another rename first.
	if tg != "Ali Karimi" {
		t.Fatalf("tg_name = %q; want Ali Karimi even while syncing is off", tg)
	}
}

// Turning the toggle back on applies the account name straight away — otherwise
// it would look like it did nothing until the user's next rename.
func TestApplyNameSyncTakesEffectImmediately(t *testing.T) {
	d, ctx := freshDB(t)
	_, err := d.AddUser(ctx, "u3", "Nima")
	noErr(t, err, "AddUser")
	_, err = d.SyncDisplayName(ctx, "u3", "Nima")
	noErr(t, err, "baseline")
	noErr(t, d.SetNameSync(ctx, "u3", false), "off")
	_, err = d.SyncDisplayName(ctx, "u3", "Nima Rad") // baseline moves, name does not
	noErr(t, err, "rename while off")

	noErr(t, d.SetNameSync(ctx, "u3", true), "on")
	got, err := d.ApplyNameSync(ctx, "u3")
	noErr(t, err, "ApplyNameSync")
	if got != "Nima Rad" {
		t.Fatalf("ApplyNameSync returned %q; want Nima Rad", got)
	}
	name, _, _ := syncState(t, d, ctx, "u3")
	if name != "Nima Rad" {
		t.Fatalf("name = %q after turning syncing on; want Nima Rad", name)
	}

	// Applying again is a no-op that still reports the name, not an error.
	got, err = d.ApplyNameSync(ctx, "u3")
	noErr(t, err, "ApplyNameSync again")
	if got != "Nima Rad" {
		t.Fatalf("second ApplyNameSync returned %q; want Nima Rad", got)
	}
}

// The statement runs on every update, so the common case must write nothing.
func TestSyncDisplayNameIsANoOpWhenNothingMoved(t *testing.T) {
	d, ctx := freshDB(t)
	_, err := d.AddUser(ctx, "u4", "Omid")
	noErr(t, err, "AddUser")
	_, err = d.SyncDisplayName(ctx, "u4", "Omid")
	noErr(t, err, "baseline")

	got, err := d.SyncDisplayName(ctx, "u4", "Omid")
	noErr(t, err, "repeat")
	if got != "" {
		t.Fatalf("a repeat sync returned %q; want the empty string, which signals that no row was written", got)
	}
}

// GetDisplayName is what the send paths use to decide whether refreshing a name
// is worth a Telegram call.
func TestGetDisplayNameReportsStaleness(t *testing.T) {
	d, ctx := freshDB(t)
	_, err := d.AddUser(ctx, "u5", "Bita")
	noErr(t, err, "AddUser")

	// Never looked at: stale, so the first viewer refreshes it.
	got, err := d.GetDisplayName(ctx, "u5", time.Hour)
	noErr(t, err, "GetDisplayName before any sighting")
	if got.Name != "Bita" || !got.Sync || !got.Stale {
		t.Fatalf("before any sighting: %+v; want Bita/sync=true/stale=true", got)
	}

	_, err = d.SyncDisplayName(ctx, "u5", "Bita")
	noErr(t, err, "baseline")

	// Just looked at: not stale, so no call is spent.
	got, err = d.GetDisplayName(ctx, "u5", time.Hour)
	noErr(t, err, "GetDisplayName after a sighting")
	if got.Stale {
		t.Error("a name recorded a moment ago reports stale; every view would spend a Telegram call")
	}

	// A zero TTL makes everything stale again.
	got, err = d.GetDisplayName(ctx, "u5", 0)
	noErr(t, err, "GetDisplayName with a zero ttl")
	if !got.Stale {
		t.Error("nothing is stale at ttl=0")
	}

	// A user who does not exist is the zero value, not an error — the send paths
	// only ask about initialised users, so this is purely defensive.
	got, err = d.GetDisplayName(ctx, "nobody", time.Hour)
	noErr(t, err, "GetDisplayName for a missing user")
	if got.Name != "" || got.Sync {
		t.Fatalf("a missing user gave %+v; want the zero value", got)
	}
}

// A long account name must not blow past the name column.
func TestSyncDisplayNameTruncates(t *testing.T) {
	d, ctx := freshDB(t)
	_, err := d.AddUser(ctx, "u6", "x")
	noErr(t, err, "AddUser")
	_, err = d.SyncDisplayName(ctx, "u6", "x")
	noErr(t, err, "baseline")

	got, err := d.SyncDisplayName(ctx, "u6", strings.Repeat("ط", 300))
	noErr(t, err, "long rename")
	if r := []rune(got); len(r) != 100 { // testConfig sets MaxNameLength=100
		t.Fatalf("synced name is %d runes; want it truncated to 100", len(r))
	}
}
