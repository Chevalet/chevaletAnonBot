package db

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/aturzone/chevaletAnonBot/internal/config"
)

// These tests run against a real PostgreSQL. They are skipped unless DB_HOST is
// set. A throwaway instance is enough, e.g.:
//
//	docker run -d --rm --name chevalet-pgtest \
//	  -e POSTGRES_USER=test -e POSTGRES_PASSWORD=test -e POSTGRES_DB=testdb \
//	  -p 55432:5432 postgres:16-alpine
//	DB_HOST=localhost DB_PORT=55432 DB_NAME=testdb DB_USER=test DB_PASS=test \
//	  go test ./internal/db/...

func ptr(s string) *string { return &s }

func noErr(t *testing.T, err error, label string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: unexpected error: %v", label, err)
	}
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	host := os.Getenv("DB_HOST")
	if host == "" {
		t.Skip("DB_HOST not set; skipping db integration test")
	}
	port := 5432
	if v := os.Getenv("DB_PORT"); v != "" {
		port, _ = strconv.Atoi(v)
	}
	return &config.Config{
		DBHost:          host,
		DBPort:          port,
		DBName:          os.Getenv("DB_NAME"),
		DBUser:          os.Getenv("DB_USER"),
		DBPass:          os.Getenv("DB_PASS"),
		DefaultCIDLimit: 2,
		MaxNameLength:   100,
	}
}

func freshDB(t *testing.T) (*DB, context.Context) {
	t.Helper()
	cfg := testConfig(t)
	// SAFETY: these tests DROP and recreate tables. Running them with the bot's own
	// DB_NAME (e.g. after exporting the deployment .env) wipes production/test data.
	// Require a database whose name ends in _test, matching the isolation the bot's
	// end-to-end tests already use (BOT_TEST_DB), unless explicitly overridden.
	if !strings.HasSuffix(cfg.DBName, "_test") && os.Getenv("DB_ALLOW_DESTRUCTIVE") != "1" {
		t.Skipf("refusing destructive DB test against %q: DB_NAME must end with _test (or set DB_ALLOW_DESTRUCTIVE=1)", cfg.DBName)
	}
	ctx := context.Background()
	d, err := Connect(ctx, cfg)
	noErr(t, err, "connect")
	t.Cleanup(d.Close)
	for _, tbl := range []string{"users", "blocks", "cids", "reports"} {
		_, err := d.pool.Exec(ctx, "DROP TABLE IF EXISTS "+tbl+" CASCADE")
		noErr(t, err, "drop "+tbl)
	}
	noErr(t, d.MakeTables(ctx), "make tables")
	return d, ctx
}

func TestUsersAndSettings(t *testing.T) {
	d, ctx := freshDB(t)

	ins, err := d.AddUser(ctx, "u1", "Alice")
	noErr(t, err, "AddUser u1")
	if !ins {
		t.Fatal("AddUser u1: want inserted=true")
	}
	ins, err = d.AddUser(ctx, "u1", "Alice")
	noErr(t, err, "AddUser u1 again")
	if ins {
		t.Fatal("AddUser u1 again: want inserted=false")
	}

	// defaults
	name, err := d.GetName(ctx, "u1")
	noErr(t, err, "GetName")
	if name != "Alice" {
		t.Fatalf("GetName = %q; want Alice", name)
	}
	banned, err := d.IsBanned(ctx, "u1")
	noErr(t, err, "IsBanned")
	if banned {
		t.Fatal("IsBanned default = true; want false")
	}
	warning, err := d.GetWarning(ctx, "u1")
	noErr(t, err, "GetWarning")
	if !warning {
		t.Fatal("GetWarning default = false; want true")
	}
	seen, err := d.GetSeenStatus(ctx, "u1")
	noErr(t, err, "GetSeenStatus")
	if seen {
		t.Fatal("GetSeenStatus default = true; want false")
	}
	wpp, err := d.GetWPP(ctx, "u1")
	noErr(t, err, "GetWPP")
	if !wpp {
		t.Fatal("GetWPP default = false; want true")
	}
	limit, err := d.GetCIDLimit(ctx, "u1")
	noErr(t, err, "GetCIDLimit")
	if limit != 2 {
		t.Fatalf("GetCIDLimit default = %d; want 2", limit)
	}
	audio, err := d.GetAudioTag(ctx, "u1")
	noErr(t, err, "GetAudioTag")
	if audio != "[ناشناس]" {
		t.Fatalf("GetAudioTag default = %q; want [ناشناس]", audio)
	}
	custom, err := d.GetCustomTag(ctx, "u1")
	noErr(t, err, "GetCustomTag")
	if custom != "" {
		t.Fatalf("GetCustomTag default = %q; want empty", custom)
	}

	// setters
	noErr(t, d.SetName(ctx, "u1", "Bob"), "SetName")
	noErr(t, d.SetWarning(ctx, "u1", false), "SetWarning")
	noErr(t, d.SetSeenOption(ctx, "u1", true), "SetSeenOption")
	noErr(t, d.SetWPP(ctx, "u1", false), "SetWPP")
	noErr(t, d.SetCIDLimit(ctx, "u1", 5), "SetCIDLimit")
	noErr(t, d.SetCustomTag(ctx, "u1", ptr("mytag")), "SetCustomTag")
	custom, err = d.GetCustomTag(ctx, "u1")
	noErr(t, err, "GetCustomTag set")
	if custom != "mytag" {
		t.Fatalf("GetCustomTag = %q; want mytag", custom)
	}
	noErr(t, d.SetCustomTag(ctx, "u1", nil), "SetCustomTag clear") // -> NULL
	custom, err = d.GetCustomTag(ctx, "u1")
	noErr(t, err, "GetCustomTag cleared")
	if custom != "" {
		t.Fatalf("GetCustomTag after clear = %q; want empty", custom)
	}

	// anonymous nickname: defaults (opted out), setters, the combined getter, clear.
	en, nm, em, err := d.GetAnonSig(ctx, "u1")
	noErr(t, err, "GetAnonSig default")
	if en || nm != "" || em != "" {
		t.Fatalf("GetAnonSig default = (%v,%q,%q); want (false,\"\",\"\")", en, nm, em)
	}
	noErr(t, d.SetAnonName(ctx, "u1", ptr("روباه آبی")), "SetAnonName")
	noErr(t, d.SetAnonEmoji(ctx, "u1", ptr("🦊")), "SetAnonEmoji")
	noErr(t, d.SetAnonEnabled(ctx, "u1", true), "SetAnonEnabled")
	en, nm, em, err = d.GetAnonSig(ctx, "u1")
	noErr(t, err, "GetAnonSig set")
	if !en || nm != "روباه آبی" || em != "🦊" {
		t.Fatalf("GetAnonSig set = (%v,%q,%q); want (true,روباه آبی,🦊)", en, nm, em)
	}
	noErr(t, d.SetAnonName(ctx, "u1", nil), "SetAnonName clear")   // -> NULL
	noErr(t, d.SetAnonEmoji(ctx, "u1", nil), "SetAnonEmoji clear") // -> NULL
	noErr(t, d.SetAnonEnabled(ctx, "u1", false), "SetAnonEnabled off")
	en, nm, em, err = d.GetAnonSig(ctx, "u1")
	noErr(t, err, "GetAnonSig cleared")
	if en || nm != "" || em != "" {
		t.Fatalf("GetAnonSig cleared = (%v,%q,%q); want (false,\"\",\"\")", en, nm, em)
	}

	name, err = d.GetName(ctx, "u1")
	noErr(t, err, "GetName set")
	if name != "Bob" {
		t.Fatalf("GetName after set = %q; want Bob", name)
	}

	// chevaletid
	chev, err := d.GetChevaletIDByUID(ctx, "u1")
	noErr(t, err, "GetChevaletIDByUID initial")
	if chev != "" {
		t.Fatalf("GetChevaletIDByUID initial = %q; want empty", chev)
	}
	noErr(t, d.SetChevaletID(ctx, "u1", "chev1"), "SetChevaletID")
	chev, err = d.GetChevaletIDByUID(ctx, "u1")
	noErr(t, err, "GetChevaletIDByUID")
	if chev != "chev1" {
		t.Fatalf("GetChevaletIDByUID = %q; want chev1", chev)
	}
	uid, err := d.GetUIDByChevaletID(ctx, "chev1")
	noErr(t, err, "GetUIDByChevaletID")
	if uid != "u1" {
		t.Fatalf("GetUIDByChevaletID = %q; want u1", uid)
	}
	chevs, err := d.GetAllChevaletIDs(ctx)
	noErr(t, err, "GetAllChevaletIDs")
	if len(chevs) != 1 || chevs[0] != "chev1" {
		t.Fatalf("GetAllChevaletIDs = %v; want [chev1]", chevs)
	}

	// ban + status
	noErr(t, d.BanAction(ctx, "u1", true), "BanAction u1")
	banned, err = d.IsBanned(ctx, "u1")
	noErr(t, err, "IsBanned after ban")
	if !banned {
		t.Fatal("IsBanned after ban = false")
	}
	isBanned, cidLimit, err := d.UserStatus(ctx, "u1")
	noErr(t, err, "UserStatus")
	if !isBanned || cidLimit != 5 {
		t.Fatalf("UserStatus = (%v,%d); want (true,5)", isBanned, cidLimit)
	}

	// ban a user that never started the bot -> row auto-created
	noErr(t, d.BanAction(ctx, "u2", true), "BanAction u2")
	banned, err = d.IsBanned(ctx, "u2")
	noErr(t, err, "IsBanned u2")
	if !banned {
		t.Fatal("IsBanned u2 (auto-created) = false")
	}

	count, err := d.UserCount(ctx)
	noErr(t, err, "UserCount")
	if count != 2 {
		t.Fatalf("UserCount = %d; want 2", count)
	}

	// name truncation by rune (MAX_NAME_LENGTH=100), Persian text
	long := strings.Repeat("ب", 150)
	_, err = d.AddUser(ctx, "u3", long)
	noErr(t, err, "AddUser u3 long")
	name, err = d.GetName(ctx, "u3")
	noErr(t, err, "GetName u3")
	if got := len([]rune(name)); got != 100 {
		t.Fatalf("truncated name rune length = %d; want 100", got)
	}
}

func TestCIDs(t *testing.T) {
	d, ctx := freshDB(t)
	_, err := d.AddUser(ctx, "u1", "Alice")
	noErr(t, err, "AddUser u1")

	ok, err := d.AddCID(ctx, "u1", "cidA")
	noErr(t, err, "AddCID cidA")
	if !ok {
		t.Fatal("AddCID cidA: want true")
	}
	ok, err = d.AddCID(ctx, "u1", "cidB")
	noErr(t, err, "AddCID cidB")
	if !ok {
		t.Fatal("AddCID cidB: want true")
	}
	cids, err := d.GetCIDs(ctx, "u1")
	noErr(t, err, "GetCIDs")
	if len(cids) != 2 || cids[0] != "cidA" || cids[1] != "cidB" {
		t.Fatalf("GetCIDs = %v; want [cidA cidB] in insertion order", cids)
	}

	// GetUIDByCID resolves an owner, and returns "" for an unknown cid.
	uid, err := d.GetUIDByCID(ctx, "cidA")
	noErr(t, err, "GetUIDByCID cidA")
	if uid != "u1" {
		t.Fatalf("GetUIDByCID cidA = %q; want u1", uid)
	}
	uid, err = d.GetUIDByCID(ctx, "nope")
	noErr(t, err, "GetUIDByCID nope")
	if uid != "" {
		t.Fatalf("GetUIDByCID nope = %q; want empty", uid)
	}

	noErr(t, d.SetCID(ctx, "cidA2", "cidA"), "SetCID")
	cids, err = d.GetCIDs(ctx, "u1")
	noErr(t, err, "GetCIDs after rename")
	if cids[0] != "cidA2" {
		t.Fatalf("GetCIDs after rename = %v; want first cidA2", cids)
	}

	noErr(t, d.RemoveCID(ctx, "u1", "cidB"), "RemoveCID")
	cids, err = d.GetCIDs(ctx, "u1")
	noErr(t, err, "GetCIDs after remove")
	if len(cids) != 1 || cids[0] != "cidA2" {
		t.Fatalf("GetCIDs after remove = %v; want [cidA2]", cids)
	}

	// collision: inserting an existing cid for another user must regenerate and
	// still succeed (proves the retry path the Python original had broken).
	_, err = d.AddUser(ctx, "u2", "Carol")
	noErr(t, err, "AddUser u2")
	ok, err = d.AddCID(ctx, "u2", "cidA2")
	noErr(t, err, "AddCID collision")
	if !ok {
		t.Fatal("AddCID collision: want true (regenerated)")
	}
	u2cids, err := d.GetCIDs(ctx, "u2")
	noErr(t, err, "GetCIDs u2")
	if len(u2cids) != 1 || u2cids[0] == "cidA2" {
		t.Fatalf("u2 cids = %v; want one regenerated cid != cidA2", u2cids)
	}

	all, err := d.GetAllCIDs(ctx)
	noErr(t, err, "GetAllCIDs")
	if len(all) != 2 {
		t.Fatalf("GetAllCIDs len = %d; want 2", len(all))
	}
}

func TestBlocks(t *testing.T) {
	d, ctx := freshDB(t)

	ok, err := d.AddBlock(ctx, "a", "b")
	noErr(t, err, "AddBlock a->b")
	if !ok {
		t.Fatal("AddBlock a->b: want true")
	}
	ok, err = d.AddBlock(ctx, "a", "b")
	noErr(t, err, "AddBlock a->b again")
	if ok {
		t.Fatal("AddBlock a->b again: want false")
	}
	blocked, err := d.IsBlocked(ctx, "a", "b")
	noErr(t, err, "IsBlocked a->b")
	if !blocked {
		t.Fatal("IsBlocked a->b: want true")
	}
	blocked, err = d.IsBlocked(ctx, "b", "a")
	noErr(t, err, "IsBlocked b->a")
	if blocked {
		t.Fatal("IsBlocked b->a: want false")
	}
	ok, err = d.RemoveBlock(ctx, "a", "b")
	noErr(t, err, "RemoveBlock a->b")
	if !ok {
		t.Fatal("RemoveBlock a->b: want true (row existed)")
	}
	ok, err = d.RemoveBlock(ctx, "a", "b")
	noErr(t, err, "RemoveBlock a->b again")
	if ok {
		t.Fatal("RemoveBlock a->b again: want false (nothing to remove)")
	}

	_, err = d.AddBlock(ctx, "a", "b")
	noErr(t, err, "AddBlock a->b 2")
	_, err = d.AddBlock(ctx, "a", "c")
	noErr(t, err, "AddBlock a->c")
	noErr(t, d.UnblockAll(ctx, "a"), "UnblockAll")
	blocked, err = d.IsBlocked(ctx, "a", "b")
	noErr(t, err, "IsBlocked a->b after unblockall")
	if blocked {
		t.Fatal("IsBlocked a->b after UnblockAll: want false")
	}
	blocked, err = d.IsBlocked(ctx, "a", "c")
	noErr(t, err, "IsBlocked a->c after unblockall")
	if blocked {
		t.Fatal("IsBlocked a->c after UnblockAll: want false")
	}
}

func TestReports(t *testing.T) {
	d, ctx := freshDB(t)

	n, err := d.AddReportID(ctx, "r1")
	noErr(t, err, "AddReportID r1")
	if n != 1 {
		t.Fatalf("AddReportID r1 = %d; want 1", n)
	}
	n, err = d.AddReportID(ctx, "r1")
	noErr(t, err, "AddReportID r1 again")
	if n != 2 {
		t.Fatalf("AddReportID r1 again = %d; want 2", n)
	}
	n, err = d.AddReportID(ctx, "r2")
	noErr(t, err, "AddReportID r2")
	if n != 1 {
		t.Fatalf("AddReportID r2 = %d; want 1", n)
	}
	n, err = d.GetReportID(ctx, "r1")
	noErr(t, err, "GetReportID r1")
	if n != 2 {
		t.Fatalf("GetReportID r1 = %d; want 2", n)
	}

	all, err := d.GetAllReports(ctx)
	noErr(t, err, "GetAllReports")
	if all["r1"] != 2 || all["r2"] != 1 {
		t.Fatalf("GetAllReports = %v; want r1:2 r2:1", all)
	}

	n, err = d.DelReportID(ctx, "r1")
	noErr(t, err, "DelReportID r1")
	if n != 2 {
		t.Fatalf("DelReportID r1 = %d; want 2", n)
	}
	n, err = d.GetReportID(ctx, "r1")
	noErr(t, err, "GetReportID r1 after del")
	if n != 0 {
		t.Fatalf("GetReportID r1 after del = %d; want 0", n)
	}
	n, err = d.DelReportID(ctx, "missing")
	noErr(t, err, "DelReportID missing")
	if n != 0 {
		t.Fatalf("DelReportID missing = %d; want 0", n)
	}
}
