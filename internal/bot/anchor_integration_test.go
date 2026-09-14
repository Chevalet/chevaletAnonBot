package bot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/PaulSonOfLars/gotgbot/v2/ext"

	"github.com/aturzone/chevaletAnonBot/internal/config"
	"github.com/aturzone/chevaletAnonBot/internal/db"
	"github.com/aturzone/chevaletAnonBot/internal/dynset"
	"github.com/aturzone/chevaletAnonBot/internal/encoder"
	"github.com/aturzone/chevaletAnonBot/internal/texts"
)

// End-to-end routing tests for the connect anchor: they drive the REAL
// dispatcher (every handler, in registration order) with synthetic updates,
// against a real PostgreSQL and a fake Bot API.
//
// Routing is the whole risk in this feature — which handler claims a reply to
// the connect prompt decides whether a message is delivered once, twice, or as a
// command. That cannot be tested by calling a function directly; it only shows
// up when the update goes through the dispatcher the way Telegram delivers it.
//
// Skipped unless BOT_TEST_DB is set — its OWN database, not the one the db
// package's tests use. Those drop the tables and assert on whole-table contents,
// so sharing one database makes both suites flaky when `go test ./...` runs the
// packages in parallel. Hence a separate name rather than reusing DB_NAME:
//
//	docker run -d --rm --name chevalet-pgtest \
//	  -e POSTGRES_USER=test -e POSTGRES_PASSWORD=test -e POSTGRES_DB=testdb \
//	  -p 55432:5432 postgres:16-alpine
//	docker exec chevalet-pgtest createdb -U test botit
//	DB_HOST=localhost DB_PORT=55432 BOT_TEST_DB=botit DB_USER=test DB_PASS=test \
//	  go test ./internal/bot/ -run TestAnchor

const fakeBotID = int64(123456)

type apiCall struct {
	method string
	params map[string]string
}

// fakeTG answers the Bot API plausibly and records every call.
type fakeTG struct {
	mu     sync.Mutex
	calls  []apiCall
	nextID atomic.Int64
	srv    *httptest.Server
}

func newFakeTG() *fakeTG {
	f := &fakeTG{}
	f.nextID.Store(1000)
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

func (f *fakeTG) handle(w http.ResponseWriter, r *http.Request) {
	method := path.Base(r.URL.Path)
	_ = r.ParseMultipartForm(1 << 20)
	params := map[string]string{}
	if r.MultipartForm != nil {
		for k, v := range r.MultipartForm.Value {
			if len(v) > 0 {
				params[k] = v[0]
			}
		}
	}
	f.mu.Lock()
	f.calls = append(f.calls, apiCall{method: method, params: params})
	f.mu.Unlock()

	id := f.nextID.Add(1)
	write := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		b, _ := json.Marshal(map[string]any{"ok": true, "result": v})
		_, _ = w.Write(b)
	}

	switch method {
	case "getMe":
		write(gotgbot.User{Id: fakeBotID, IsBot: true, FirstName: "test", Username: "chevalet_test_bot"})
	case "sendMessage":
		write(f.message(id, params))
	case "copyMessage":
		write(gotgbot.MessageId{MessageId: id})
	case "copyMessages":
		var src []int64
		_ = json.Unmarshal([]byte(params["message_ids"]), &src)
		out := make([]gotgbot.MessageId, 0, len(src))
		for range src {
			out = append(out, gotgbot.MessageId{MessageId: f.nextID.Add(1)})
		}
		write(out)
	case "getChat":
		cid, _ := strconv.ParseInt(params["chat_id"], 10, 64)
		write(gotgbot.ChatFullInfo{Id: cid, Type: "private"})
	case "getChatAdministrators":
		write([]gotgbot.ChatMember{})
	default:
		// editMessageText / editMessageReplyMarkup / answerCallbackQuery /
		// deleteMessage / setMyCommands … all accept a bare `true`.
		write(true)
	}
}

// message echoes the request back as a Message — including reply_markup, so a
// test can reply to the keyboard the bot actually sent.
func (f *fakeTG) message(id int64, p map[string]string) gotgbot.Message {
	m := gotgbot.Message{
		MessageId: id,
		From:      &gotgbot.User{Id: fakeBotID, IsBot: true, Username: "chevalet_test_bot"},
		Date:      time.Now().Unix(),
		Text:      p["text"],
	}
	if cid, err := strconv.ParseInt(p["chat_id"], 10, 64); err == nil {
		m.Chat = gotgbot.Chat{Id: cid, Type: "private"}
	}
	if rm := p["reply_markup"]; rm != "" {
		var kb gotgbot.InlineKeyboardMarkup
		if json.Unmarshal([]byte(rm), &kb) == nil && len(kb.InlineKeyboard) > 0 {
			m.ReplyMarkup = &kb
		}
	}
	return m
}

// since returns the calls recorded after mark.
func (f *fakeTG) since(mark int) []apiCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]apiCall, len(f.calls[mark:]))
	copy(out, f.calls[mark:])
	return out
}

func (f *fakeTG) mark() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type harness struct {
	t   *testing.T
	b   *Bot
	tg  *fakeTG
	upd atomic.Int64
	mid atomic.Int64
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	host, name := os.Getenv("DB_HOST"), os.Getenv("BOT_TEST_DB")
	if host == "" || name == "" {
		t.Skip("DB_HOST/BOT_TEST_DB not set; skipping bot integration test")
	}
	port := 5432
	if v := os.Getenv("DB_PORT"); v != "" {
		port, _ = strconv.Atoi(v)
	}
	cfg := &config.Config{
		BotToken:        "123456:TESTTOKEN",
		BotID:           "123456",
		DBHost:          host,
		DBPort:          port,
		DBName:          name,
		DBUser:          os.Getenv("DB_USER"),
		DBPass:          os.Getenv("DB_PASS"),
		DefaultCIDLimit: 2,
		MaxNameLength:   100,
		MaxCIDLength:    30,
		MinCIDLength:    5,
		DynsetPath:      filepath.Join(t.TempDir(), "dynamic_settings.json"),
		ReportChatID:    "-1001000000001",
		ErrorChatID:     "-1001000000002",
	}

	ctx := context.Background()
	database, err := db.Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(database.Close)
	if err := database.MakeTables(ctx); err != nil {
		t.Fatalf("make tables: %v", err)
	}

	tg := newFakeTG()
	t.Cleanup(tg.srv.Close)

	// Mirrors bot.New, with the API pointed at the fake server.
	limiter := newSendLimiter()
	lc := &limitedClient{
		inner: &gotgbot.BaseBotClient{
			DefaultRequestOpts: &gotgbot.RequestOpts{Timeout: 10 * time.Second, APIURL: tg.srv.URL},
		},
		limiter: limiter,
	}
	bot, err := gotgbot.NewBot(cfg.BotToken, &gotgbot.BotOpts{BotClient: lc})
	if err != nil {
		t.Fatalf("new bot: %v", err)
	}
	b := &Bot{
		TG:         bot,
		DB:         database,
		Cfg:        cfg,
		Texts:      texts.New("../../Texts"),
		Dyn:        dynset.New(cfg.DynsetPath, "", "", ""),
		Tokens:     encoder.NewTokenCipher(cfg.BotToken),
		users:      newUserStore(),
		aiQueue:    newAIQueue(),
		admins:     map[string]bool{},
		errReports: newErrReportStore(50),
		limiter:    limiter,
	}
	lc.onRetryable = b.enqueueFailedSend
	b.Dispatcher = ext.NewDispatcher(&ext.DispatcherOpts{
		Error:       b.onError,
		Panic:       b.onPanic,
		MaxRoutines: 1,
	})
	b.registerHandlers()

	h := &harness{t: t, b: b, tg: tg}
	h.mid.Store(1)
	return h
}

// uniqueUID keeps every test's users disjoint, so tests share one database
// without a truncate between them.
var uidSeq atomic.Int64

func newUID() int64 { return 900000000 + time.Now().UnixNano()%100000000 + uidSeq.Add(1)*1000 }

// send pushes a text message from uid through the dispatcher.
func (h *harness) send(uid int64, text string, replyTo *gotgbot.Message) {
	h.t.Helper()
	m := &gotgbot.Message{
		MessageId:      h.mid.Add(1),
		From:           &gotgbot.User{Id: uid, IsBot: false, FirstName: "u" + strconv.FormatInt(uid, 10)},
		Chat:           gotgbot.Chat{Id: uid, Type: "private"},
		Date:           time.Now().Unix(),
		Text:           text,
		ReplyToMessage: replyTo,
	}
	if strings.HasPrefix(text, "/") {
		cmd := strings.Fields(text)[0]
		m.Entities = []gotgbot.MessageEntity{{Type: "bot_command", Offset: 0, Length: int64(len(cmd))}}
	}
	h.process(&gotgbot.Update{UpdateId: h.upd.Add(1), Message: m})
}

// sendPhoto pushes one photo of an album from uid through the dispatcher.
func (h *harness) sendPhoto(uid int64, group string, replyTo *gotgbot.Message) {
	h.t.Helper()
	h.process(&gotgbot.Update{UpdateId: h.upd.Add(1), Message: &gotgbot.Message{
		MessageId:      h.mid.Add(1),
		From:           &gotgbot.User{Id: uid, IsBot: false, FirstName: "u"},
		Chat:           gotgbot.Chat{Id: uid, Type: "private"},
		Date:           time.Now().Unix(),
		MediaGroupId:   group,
		Photo:          []gotgbot.PhotoSize{{FileId: "f" + group, FileUniqueId: "fu" + group, Width: 1, Height: 1}},
		ReplyToMessage: replyTo,
	}})
}

// press pushes a callback query on the given message.
func (h *harness) press(uid int64, on *gotgbot.Message, data string) {
	h.t.Helper()
	msg := *on
	msg.Chat = gotgbot.Chat{Id: uid, Type: "private"}
	h.process(&gotgbot.Update{
		UpdateId: h.upd.Add(1),
		CallbackQuery: &gotgbot.CallbackQuery{
			Id:      strconv.FormatInt(h.upd.Load(), 10),
			From:    gotgbot.User{Id: uid, IsBot: false, FirstName: "u"},
			Message: msg,
			Data:    data,
		},
	})
}

func (h *harness) process(u *gotgbot.Update) {
	h.t.Helper()
	if err := h.b.Dispatcher.ProcessUpdate(h.b.TG, u, nil); err != nil {
		h.t.Fatalf("dispatch: %v", err)
	}
}

// linkOf returns a user's first cid (their anonymous link id).
func (h *harness) linkOf(uid int64) string {
	h.t.Helper()
	ctx := context.Background()
	cids, err := h.b.DB.GetCIDs(ctx, strconv.FormatInt(uid, 10))
	if err != nil || len(cids) == 0 {
		h.t.Fatalf("no cid for %d: %v", uid, err)
	}
	return cids[0]
}

// connect runs /start <cid> and returns the prompt message the bot replied with.
func (h *harness) connect(sender int64, cid string) *gotgbot.Message {
	h.t.Helper()
	mark := h.tg.mark()
	h.send(sender, "/start "+cid, nil)
	for _, c := range h.tg.since(mark) {
		if c.method == "sendMessage" && strings.Contains(c.params["text"], "وصل شدی") {
			var kb gotgbot.InlineKeyboardMarkup
			_ = json.Unmarshal([]byte(c.params["reply_markup"]), &kb)
			return &gotgbot.Message{
				MessageId:   h.mid.Add(1),
				From:        &gotgbot.User{Id: fakeBotID, IsBot: true, Username: "chevalet_test_bot"},
				Chat:        gotgbot.Chat{Id: sender, Type: "private"},
				Text:        c.params["text"],
				ReplyMarkup: &kb,
			}
		}
	}
	h.t.Fatalf("no connect prompt was sent")
	return nil
}

// copies counts deliveries to the target since mark.
func (h *harness) copies(mark int, target int64) int {
	n := 0
	want := strconv.FormatInt(target, 10)
	for _, c := range h.tg.since(mark) {
		if c.method == "copyMessage" && c.params["chat_id"] == want {
			n++
		}
	}
	return n
}

func (h *harness) sentTexts(mark int) string {
	var sb strings.Builder
	for _, c := range h.tg.since(mark) {
		if c.method == "sendMessage" {
			sb.WriteString(c.params["text"])
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// The core of the feature request: the prompt is reusable, and a reply to it
// while the bot is ALREADY waiting must still deliver exactly one message.
func TestAnchorReplyDeliversExactlyOnce(t *testing.T) {
	h := newHarness(t)
	sender, target := newUID(), newUID()

	h.send(target, "/start", nil) // create the target + their link
	cid := h.linkOf(target)

	prompt := h.connect(sender, cid)

	// The button must carry the link id, else nothing below can work.
	var data string
	for _, r := range prompt.ReplyMarkup.InlineKeyboard {
		for _, b := range r {
			data = b.CallbackData
		}
	}
	if data != "cancel|"+cid {
		t.Fatalf("cancel button data = %q; want cancel|%s", data, cid)
	}

	// 1. The bot is still waiting for the message AND the user replies to the
	//    prompt — the double-send case. Exactly one delivery.
	mark := h.tg.mark()
	h.send(sender, "سلام اول", prompt)
	if got := h.copies(mark, target); got != 1 {
		t.Fatalf("reply while composing delivered %d copies; want exactly 1", got)
	}

	// 2. The conversation has ended. Replying to the same prompt again must send
	//    again — the point of the feature.
	mark = h.tg.mark()
	h.send(sender, "سلام دوم", prompt)
	if got := h.copies(mark, target); got != 1 {
		t.Fatalf("reply after the conversation ended delivered %d copies; want 1", got)
	}

	// 3. And again, so the anchor is not single-use.
	mark = h.tg.mark()
	h.send(sender, "سلام سوم", prompt)
	if got := h.copies(mark, target); got != 1 {
		t.Fatalf("third reply delivered %d copies; want 1", got)
	}

	// A message NOT replying to anything, once the conversation is over, is not
	// silently delivered to the old target.
	mark = h.tg.mark()
	h.send(sender, "یه پیام بی ربط", nil)
	if got := h.copies(mark, target); got != 0 {
		t.Errorf("a stray message delivered %d copies; want 0", got)
	}
}

// Second requirement: a reply that starts with a command is the command.
func TestAnchorReplyWithCommandIsNotDelivered(t *testing.T) {
	h := newHarness(t)
	sender, target := newUID(), newUID()
	h.send(target, "/start", nil)
	cid := h.linkOf(target)
	prompt := h.connect(sender, cid)

	// Clear the waiting state first, so this tests the anchor path itself.
	h.send(sender, "پیام اول", prompt)

	for _, cmd := range []string{"/menu", "/help", "/myuid", "/settings", "/my_links", "/cancel", "/notarealcommand"} {
		t.Run(cmd, func(t *testing.T) {
			mark := h.tg.mark()
			h.send(sender, cmd, prompt)
			if got := h.copies(mark, target); got != 0 {
				t.Errorf("%q replying to the prompt was delivered to the target (%d copies)", cmd, got)
			}
			if len(h.tg.since(mark)) == 0 {
				t.Errorf("%q produced no response at all", cmd)
			}
		})
	}

	// A slash mid-sentence is somebody's message, not a command: still delivered.
	mark := h.tg.mark()
	h.send(sender, "ببین /menu رو زدم نشد", prompt)
	if got := h.copies(mark, target); got != 1 {
		t.Errorf("a message merely containing a command was not delivered (%d copies)", got)
	}
}

// The anchor is a shortcut past the link, never past its gates.
func TestAnchorHonoursLinkChangesAndBlocks(t *testing.T) {
	ctx := context.Background()

	t.Run("renamed link stops working", func(t *testing.T) {
		h := newHarness(t)
		sender, target := newUID(), newUID()
		h.send(target, "/start", nil)
		cid := h.linkOf(target)
		prompt := h.connect(sender, cid)
		h.send(sender, "اولی", prompt)

		if err := h.b.DB.SetCID(ctx, cid+"x2", cid); err != nil {
			t.Fatalf("rename cid: %v", err)
		}
		mark := h.tg.mark()
		h.send(sender, "بعد از تغییر لینک", prompt)
		if got := h.copies(mark, target); got != 0 {
			t.Errorf("a revoked link still delivered %d copies", got)
		}
		if !strings.Contains(h.sentTexts(mark), "پاک یا عوض کرده") {
			t.Errorf("the sender was not told the link changed; got %q", h.sentTexts(mark))
		}
	})

	t.Run("a block stops the anchor", func(t *testing.T) {
		h := newHarness(t)
		sender, target := newUID(), newUID()
		h.send(target, "/start", nil)
		cid := h.linkOf(target)
		prompt := h.connect(sender, cid)
		h.send(sender, "اولی", prompt)

		if _, err := h.b.DB.AddBlock(ctx, strconv.FormatInt(target, 10), strconv.FormatInt(sender, 10)); err != nil {
			t.Fatalf("add block: %v", err)
		}
		mark := h.tg.mark()
		h.send(sender, "بعد از بلاک", prompt)
		if got := h.copies(mark, target); got != 0 {
			t.Errorf("a blocked sender still delivered %d copies", got)
		}
		if !strings.Contains(h.sentTexts(mark), "بلاکت کرده") {
			t.Errorf("the sender was not told they are blocked; got %q", h.sentTexts(mark))
		}
	})
}

// Pressing cancel must still cancel — including when no conversation is left,
// which is the state the anchor leaves behind.
func TestAnchorCancelButtonStillCancels(t *testing.T) {
	h := newHarness(t)
	sender, target := newUID(), newUID()
	h.send(target, "/start", nil)
	cid := h.linkOf(target)
	prompt := h.connect(sender, cid)

	mark := h.tg.mark()
	h.press(sender, prompt, "cancel|"+cid)
	var edited bool
	for _, c := range h.tg.since(mark) {
		if c.method == "editMessageText" && strings.Contains(c.params["text"], "چشم بهم بزنی") {
			edited = true
		}
	}
	if !edited {
		t.Fatalf("the cancel button did not edit the prompt away; calls: %+v", h.tg.since(mark))
	}

	// And the pending send is gone: a plain message is no longer delivered.
	mark = h.tg.mark()
	h.send(sender, "بعد از کنسل", nil)
	if got := h.copies(mark, target); got != 0 {
		t.Errorf("a message after cancel was still delivered (%d copies)", got)
	}
}

// An album is several updates that must become ONE delivery. Sending one through
// the anchor exercises the media-group path from outside any conversation, which
// is where a stray extra send would show up.
func TestAnchorAlbumIsDeliveredOnce(t *testing.T) {
	h := newHarness(t)
	sender, target := newUID(), newUID()
	h.send(target, "/start", nil)
	cid := h.linkOf(target)
	prompt := h.connect(sender, cid)
	h.send(sender, "\u067e\u06cc\u0627\u0645 \u0627\u0648\u0644", prompt) // end the conversation

	mark := h.tg.mark()
	h.sendPhoto(sender, "GRP1", prompt)
	h.sendPhoto(sender, "GRP1", prompt)
	h.sendPhoto(sender, "GRP1", prompt)

	// The first item is stashed; each later one re-copies the whole group after
	// deleting the previous copy, so the LAST copyMessages call is the album.
	want := strconv.FormatInt(target, 10)
	var lastCount int
	var copyCalls, singleCopies int
	for _, c := range h.tg.since(mark) {
		if c.params["chat_id"] != want {
			continue
		}
		switch c.method {
		case "copyMessages":
			copyCalls++
			var ids []int64
			_ = json.Unmarshal([]byte(c.params["message_ids"]), &ids)
			lastCount = len(ids)
		case "copyMessage":
			singleCopies++
		}
	}
	if copyCalls == 0 {
		t.Fatalf("the album was never delivered through the anchor")
	}
	if lastCount != 3 {
		t.Errorf("the delivered album holds %d items; want 3", lastCount)
	}
	// The first item must not ALSO arrive as its own standalone message.
	if singleCopies != 0 {
		t.Errorf("the album also produced %d single-message copies; an item was delivered twice", singleCopies)
	}
}
