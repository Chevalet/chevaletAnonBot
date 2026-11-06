package db

// Tests for the message_box DB layer. They hit a REAL PostgreSQL (skipped unless
// DB_HOST is set, like the other db integration tests) but are deliberately
// NON-DESTRUCTIVE: they never DROP or TRUNCATE a table, only insert rows with
// per-run unique uids, and delete exactly those rows in t.Cleanup. They only need
// the schema from MakeTables (which is CREATE ... IF NOT EXISTS).

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// boxTestDB connects to the configured test Postgres and ensures the schema
// exists WITHOUT dropping anything. Existing tests' destructive helpers (freshDB)
// are intentionally not used here.
func boxTestDB(t *testing.T) *DB {
	t.Helper()
	ctx := context.Background()
	d, err := Connect(ctx, testConfig(t))
	noErr(t, err, "connect")
	t.Cleanup(d.Close)
	noErr(t, d.MakeTables(ctx), "MakeTables")
	return d
}

// boxTestUID returns a uid unique to this test run, so parallel/repeated runs
// never collide with each other or with real data in the (throwaway) test DB.
func boxTestUID(t *testing.T, prefix string) string {
	t.Helper()
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// boxCleanup deletes only the rows this test created: queued messages addressed
// to any of uids and the unique user rows themselves.
func boxCleanup(t *testing.T, d *DB, uids ...string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		for _, uid := range uids {
			_, _ = d.pool.Exec(ctx,
				`DELETE FROM message_box WHERE recipient_uid=$1 OR sender_uid=$1`, uid)
			_, _ = d.pool.Exec(ctx, `DELETE FROM users WHERE uid=$1`, uid)
		}
	})
}

// boxIDs returns the ids of recipientUID's rows in insertion order (test helper
// for order assertions; it selects only that test's rows).
func boxIDs(t *testing.T, d *DB, recipientUID string) []int64 {
	t.Helper()
	rows, err := d.pool.Query(context.Background(),
		`SELECT id FROM message_box WHERE recipient_uid=$1 ORDER BY id`, recipientUID)
	noErr(t, err, "select ids")
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		noErr(t, rows.Scan(&id), "scan id")
		ids = append(ids, id)
	}
	noErr(t, rows.Err(), "rows err")
	return ids
}

// boxRowCount counts ALL rows for a recipient, in any status, so tests can prove
// deletes actually removed the rows rather than only moved them out of 'pending'.
func boxRowCount(t *testing.T, d *DB, recipientUID string) int {
	t.Helper()
	var n int
	noErr(t, d.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM message_box WHERE recipient_uid=$1`, recipientUID).Scan(&n),
		"count rows")
	return n
}

// TestMessageBoxToggleAndDefault covers the users.message_box setting: OFF for a
// fresh user row, then the set/get toggle both ways.
func TestMessageBoxToggleAndDefault(t *testing.T) {
	d := boxTestDB(t)
	ctx := context.Background()
	uid := boxTestUID(t, "box-user")
	boxCleanup(t, d, uid)

	inserted, err := d.AddUser(ctx, uid, "Box User")
	noErr(t, err, "AddUser")
	if !inserted {
		t.Fatalf("AddUser(%q) reported the row already existed", uid)
	}

	// HARD REQUIREMENT: the feature is off by default.
	if on, err := d.GetMessageBox(ctx, uid); err != nil {
		t.Fatalf("GetMessageBox default: %v", err)
	} else if on {
		t.Fatal("GetMessageBox default = true; want false (message box must be OFF by default)")
	}

	noErr(t, d.SetMessageBox(ctx, uid, true), "SetMessageBox(true)")
	if on, err := d.GetMessageBox(ctx, uid); err != nil || !on {
		t.Fatalf("GetMessageBox after enable = (%v,%v); want (true,nil)", on, err)
	}
	noErr(t, d.SetMessageBox(ctx, uid, false), "SetMessageBox(false)")
	if on, err := d.GetMessageBox(ctx, uid); err != nil || on {
		t.Fatalf("GetMessageBox after disable = (%v,%v); want (false,nil)", on, err)
	}
}

// TestMessageBoxAddCountClaimOrder covers queueing + counting + claim semantics:
// only pending rows are claimable, claimed rows come back oldest-first with every
// field round-tripped, and a second claim of the same rows returns nothing.
func TestMessageBoxAddCountClaimOrder(t *testing.T) {
	d := boxTestDB(t)
	ctx := context.Background()
	recipient := boxTestUID(t, "box-recv")
	sender := boxTestUID(t, "box-send")
	boxCleanup(t, d, sender)

	// Empty batch is a no-op, not an error.
	noErr(t, d.AddBoxMessages(ctx, nil), "AddBoxMessages(nil)")

	// First row: every field populated (to prove the full round-trip).
	full := BoxMessage{
		RecipientUID:     recipient,
		SenderUID:        sender,
		SourceChatID:     1001,
		SourceMessageID:  2002,
		MediaGroupID:     "mg-1",
		Kind:             "media",
		TargetCID:        "cid-target",
		ReplyToChatID:    3003,
		ReplyToMessageID: 4004,
		QuoteText:        "quoted text",
		QuotePosition:    7,
		ExternalReply:    true,
		ChannelReply:     true,
	}
	// Second/third rows: optional fields left empty (must come back as "").
	sparse := BoxMessage{
		RecipientUID:    recipient,
		SenderUID:       sender,
		SourceChatID:    1001,
		SourceMessageID: 2003,
		Kind:            "text",
	}
	third := sparse
	third.SourceMessageID = 2004

	noErr(t, d.AddBoxMessages(ctx, []BoxMessage{full, sparse, third}), "AddBoxMessages")

	n, err := d.PendingBoxCount(ctx, recipient)
	noErr(t, err, "PendingBoxCount")
	if n != 3 {
		t.Fatalf("PendingBoxCount = %d; want 3", n)
	}

	// Claim two: only pending, oldest first, full field fidelity.
	got, err := d.ClaimBoxMessages(ctx, recipient, 2, "claim-a")
	noErr(t, err, "ClaimBoxMessages(2)")
	if len(got) != 2 {
		t.Fatalf("claim returned %d rows; want 2", len(got))
	}
	if got[0].ID >= got[1].ID {
		t.Fatalf("claim not ordered by id: %d then %d", got[0].ID, got[1].ID)
	}
	want := full
	want.ID = got[0].ID
	if got[0] != want {
		t.Fatalf("claimed full row mismatch:\n got %+v\nwant %+v", got[0], want)
	}
	if got[1].RecipientUID != recipient || got[1].Kind != "text" ||
		got[1].MediaGroupID != "" || got[1].TargetCID != "" ||
		got[1].QuoteText != "" || got[1].ExternalReply || got[1].ChannelReply {
		t.Fatalf("claimed sparse row round-trip mismatch: %+v", got[1])
	}

	// Claiming again returns only what is still pending (the third row).
	rest, err := d.ClaimBoxMessages(ctx, recipient, 10, "claim-b")
	noErr(t, err, "ClaimBoxMessages(rest)")
	if len(rest) != 1 || rest[0].SourceMessageID != 2004 {
		t.Fatalf("second claim = %+v; want exactly the remaining row (source_message_id 2004)", rest)
	}

	// Nothing pending now, so a third claim is empty.
	none, err := d.ClaimBoxMessages(ctx, recipient, 10, "claim-c")
	noErr(t, err, "ClaimBoxMessages(empty)")
	if len(none) != 0 {
		t.Fatalf("claim with nothing pending returned %d rows; want 0", len(none))
	}
	if n, err := d.PendingBoxCount(ctx, recipient); err != nil || n != 0 {
		t.Fatalf("PendingBoxCount after claims = (%d,%v); want (0,nil)", n, err)
	}

	// limit<=0 is an early no-op (and must not claim anything).
	if zero, err := d.ClaimBoxMessages(ctx, recipient, 0, "claim-zero"); err != nil || zero != nil {
		t.Fatalf("ClaimBoxMessages(limit=0) = (%v,%v); want (nil,nil)", zero, err)
	}
}

// TestMessageBoxReleaseClaim covers returning in-flight rows to pending after a
// failed/lost delivery.
func TestMessageBoxReleaseClaim(t *testing.T) {
	d := boxTestDB(t)
	ctx := context.Background()
	recipient := boxTestUID(t, "box-rel")
	sender := boxTestUID(t, "box-send")
	boxCleanup(t, d, sender)

	item := BoxMessage{RecipientUID: recipient, SenderUID: sender, SourceChatID: 1,
		SourceMessageID: 2, Kind: "text"}
	noErr(t, d.AddBoxMessages(ctx, []BoxMessage{item, item}), "AddBoxMessages")

	got, err := d.ClaimBoxMessages(ctx, recipient, 5, "release-claim")
	noErr(t, err, "ClaimBoxMessages")
	if len(got) != 2 {
		t.Fatalf("claim returned %d rows; want 2", len(got))
	}
	if n, _ := d.PendingBoxCount(ctx, recipient); n != 0 {
		t.Fatalf("pending while delivering = %d; want 0", n)
	}

	noErr(t, d.ReleaseBoxClaim(ctx, "release-claim"), "ReleaseBoxClaim")
	if n, err := d.PendingBoxCount(ctx, recipient); err != nil || n != 2 {
		t.Fatalf("pending after release = (%d,%v); want (2,nil)", n, err)
	}
	// The rows are claimable again under a new token.
	if got, err := d.ClaimBoxMessages(ctx, recipient, 5, "release-claim-2"); err != nil || len(got) != 2 {
		t.Fatalf("re-claim after release = (%d rows,%v); want (2,nil)", len(got), err)
	}
	// Releasing an unknown/empty claim is a harmless no-op.
	noErr(t, d.ReleaseBoxClaim(ctx, "no-such-claim"), "ReleaseBoxClaim(unknown)")
	noErr(t, d.ReleaseBoxClaim(ctx, ""), "ReleaseBoxClaim(empty)")
}

// TestMessageBoxDelete covers removing delivered rows and the empty-slice no-op.
func TestMessageBoxDelete(t *testing.T) {
	d := boxTestDB(t)
	ctx := context.Background()
	recipient := boxTestUID(t, "box-del")
	sender := boxTestUID(t, "box-send")
	boxCleanup(t, d, sender)

	item := BoxMessage{RecipientUID: recipient, SenderUID: sender, SourceChatID: 1,
		SourceMessageID: 2, Kind: "text"}
	noErr(t, d.AddBoxMessages(ctx, []BoxMessage{item, item, item}), "AddBoxMessages")

	got, err := d.ClaimBoxMessages(ctx, recipient, 2, "del-claim")
	noErr(t, err, "ClaimBoxMessages")
	ids := []int64{got[0].ID, got[1].ID}
	noErr(t, d.DeleteBoxMessages(ctx, ids), "DeleteBoxMessages")
	if n := boxRowCount(t, d, recipient); n != 1 {
		t.Fatalf("rows after delete = %d; want 1 (3 added, 2 deleted)", n)
	}

	noErr(t, d.DeleteBoxMessages(ctx, nil), "DeleteBoxMessages(nil)") // no-op
	// Deleting an unknown id silently affects no rows.
	noErr(t, d.DeleteBoxMessages(ctx, []int64{got[0].ID + 1_000_000}), "DeleteBoxMessages(unknown)")
}

// TestMessageBoxResetStaleClaims covers crash recovery: delivering rows older
// than the cutoff go back to pending, while recently created ones are untouched.
func TestMessageBoxResetStaleClaims(t *testing.T) {
	d := boxTestDB(t)
	ctx := context.Background()
	recipient := boxTestUID(t, "box-stale")
	sender := boxTestUID(t, "box-send")
	boxCleanup(t, d, sender)

	item := BoxMessage{RecipientUID: recipient, SenderUID: sender, SourceChatID: 1,
		SourceMessageID: 2, Kind: "text"}
	noErr(t, d.AddBoxMessages(ctx, []BoxMessage{item, item}), "AddBoxMessages")
	if _, err := d.ClaimBoxMessages(ctx, recipient, 5, "stale-claim"); err != nil {
		t.Fatalf("ClaimBoxMessages: %v", err)
	}

	// Age only this test's rows, then reset with a shorter cutoff: both recovered.
	_, err := d.pool.Exec(ctx,
		`UPDATE message_box SET created_at = now() - interval '10 minutes' WHERE recipient_uid=$1`,
		recipient)
	noErr(t, err, "age rows")
	n, err := d.ResetStaleBoxClaims(ctx, 5*time.Minute)
	noErr(t, err, "ResetStaleBoxClaims")
	if n != 2 {
		t.Fatalf("ResetStaleBoxClaims = %d; want 2", n)
	}
	if got, _ := d.PendingBoxCount(ctx, recipient); got != 2 {
		t.Fatalf("pending after stale reset = %d; want 2", got)
	}

	// A FRESH claim of those (now 10-minute-old) rows must NOT be reset by a cutoff
	// that only considers rows older than an hour.
	if _, err := d.ClaimBoxMessages(ctx, recipient, 5, "fresh-claim"); err != nil {
		t.Fatalf("ClaimBoxMessages(fresh): %v", err)
	}
	n, err = d.ResetStaleBoxClaims(ctx, time.Hour)
	noErr(t, err, "ResetStaleBoxClaims(fresh claim)")
	if n != 0 {
		t.Fatalf("ResetStaleBoxClaims reset %d freshly claimed rows (olderThan=1h, rows=10m old); want 0", n)
	}
	// olderThan<=0 is a no-op.
	if n, err := d.ResetStaleBoxClaims(ctx, 0); err != nil || n != 0 {
		t.Fatalf("ResetStaleBoxClaims(0) = (%d,%v); want (0,nil)", n, err)
	}
}

// TestMessageBoxCap covers the anti-flood cap: keep at most max pending rows,
// dropping the OLDEST (the newest waiting messages survive), and clear-all at 0.
func TestMessageBoxCap(t *testing.T) {
	d := boxTestDB(t)
	ctx := context.Background()
	recipient := boxTestUID(t, "box-cap")
	sender := boxTestUID(t, "box-send")
	boxCleanup(t, d, sender)

	var items []BoxMessage
	for i := 0; i < 5; i++ {
		items = append(items, BoxMessage{RecipientUID: recipient, SenderUID: sender,
			SourceChatID: 1, SourceMessageID: int64(100 + i), Kind: "text"})
	}
	noErr(t, d.AddBoxMessages(ctx, items), "AddBoxMessages")
	all := boxIDs(t, d, recipient)
	if len(all) != 5 {
		t.Fatalf("inserted %d rows; want 5", len(all))
	}

	// Cap to 2: the three oldest go, the two newest stay.
	dropped, err := d.CapBoxMessages(ctx, recipient, 2)
	noErr(t, err, "CapBoxMessages(2)")
	if dropped != 3 {
		t.Fatalf("CapBoxMessages(2) dropped %d; want 3", dropped)
	}
	if n, err := d.PendingBoxCount(ctx, recipient); err != nil || n != 2 {
		t.Fatalf("pending after cap = (%d,%v); want (2,nil)", n, err)
	}
	kept := boxIDs(t, d, recipient)
	if len(kept) != 2 || kept[0] != all[3] || kept[1] != all[4] {
		t.Fatalf("cap kept ids %v; want the newest two %v", kept, all[3:])
	}

	// Negative max is a no-op.
	if n, err := d.CapBoxMessages(ctx, recipient, -1); err != nil || n != 0 {
		t.Fatalf("CapBoxMessages(-1) = (%d,%v); want (0,nil)", n, err)
	}
	// max=0 clears the remaining pending rows.
	if n, err := d.CapBoxMessages(ctx, recipient, 0); err != nil || n != 2 {
		t.Fatalf("CapBoxMessages(0) = (%d,%v); want (2,nil)", n, err)
	}
	if n := boxRowCount(t, d, recipient); n != 0 {
		t.Fatalf("rows after cap(0) = %d; want 0", n)
	}
}

// TestMessageBoxCapIgnoresDelivering proves the cap only touches pending rows: a
// batch already claimed (being delivered right now) is not deleted underneath the
// worker.
func TestMessageBoxCapIgnoresDelivering(t *testing.T) {
	d := boxTestDB(t)
	ctx := context.Background()
	recipient := boxTestUID(t, "box-capd")
	sender := boxTestUID(t, "box-send")
	boxCleanup(t, d, sender)

	var items []BoxMessage
	for i := 0; i < 3; i++ {
		items = append(items, BoxMessage{RecipientUID: recipient, SenderUID: sender,
			SourceChatID: 1, SourceMessageID: int64(200 + i), Kind: "tag"})
	}
	noErr(t, d.AddBoxMessages(ctx, items), "AddBoxMessages")
	claimed, err := d.ClaimBoxMessages(ctx, recipient, 1, "cap-claim")
	noErr(t, err, "ClaimBoxMessages")
	if len(claimed) != 1 {
		t.Fatalf("claimed %d rows; want 1", len(claimed))
	}

	// Only 2 rows are pending; capping to 1 drops 1 and must leave the claimed row.
	dropped, err := d.CapBoxMessages(ctx, recipient, 1)
	noErr(t, err, "CapBoxMessages(1)")
	if dropped != 1 {
		t.Fatalf("CapBoxMessages(1) dropped %d; want 1 (of 2 pending)", dropped)
	}
	if n := boxRowCount(t, d, recipient); n != 2 {
		t.Fatalf("rows after cap = %d; want 2 (1 delivering + 1 pending)", n)
	}
}
