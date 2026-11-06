// Package bot wires the Telegram side of ChevaletAnonBot on top of gotgbot:
// the Bot client, the long-polling Updater, the Dispatcher, the central error
// handler, and the prep middleware that mirrors the Python @prep_function.
package bot

import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/PaulSonOfLars/gotgbot/v2/ext"
	"github.com/PaulSonOfLars/gotgbot/v2/ext/handlers/conversation"

	"github.com/aturzone/chevaletAnonBot/internal/config"
	"github.com/aturzone/chevaletAnonBot/internal/db"
	"github.com/aturzone/chevaletAnonBot/internal/dynset"
	"github.com/aturzone/chevaletAnonBot/internal/encoder"
	"github.com/aturzone/chevaletAnonBot/internal/texts"
)

// dbOpTimeout bounds a single update's database work.
const dbOpTimeout = 30 * time.Second

// Bot bundles everything a handler needs.
type Bot struct {
	TG         *gotgbot.Bot
	Dispatcher *ext.Dispatcher
	Updater    *ext.Updater

	DB    *db.DB
	Cfg   *config.Config
	Texts *texts.Loader
	Dyn   *dynset.Settings

	// Tokens seals/opens the per-emission callback tokens that carry a user id in
	// button callback_data (replacing the keyless, linkable EncodeChevaletID for
	// NEW messages; see encoder/token.go). Keyed off BOT_TOKEN + TOKEN_KEYS.
	Tokens *encoder.TokenCipher

	users      *userStore
	aiQueue    *aiQueue
	admins     map[string]bool
	errReports *errReportStore

	// limiter paces every outbound Telegram call and owns the flood pause. See
	// ratelimit.go for why this lives at the BotClient layer.
	limiter *sendLimiter

	// batcher buffers ordinary outbound sends for a short debounce window so a
	// burst of messages from one sender reaches one recipient as a single delivery
	// pass. See batcher.go. nil is impossible after New; the field exists here so
	// Run can drain it on shutdown and tests can drive it directly.
	batcher *batcher

	// convStores holds the three ConversationHandlers' state storages so a stale
	// flow can be dropped when the user starts a different one. Without this, a
	// user part-way through composing a message who goes to settings and types a
	// new name has that text delivered as the message: the start conversation is
	// registered first, so its send state claims the update. See dropConversations.
	convStores []conversation.Storage

	// dbErrMu/lastDBErr throttle the "PostgreSQL ERROR" reports to ERROR_CHAT_ID:
	// during a brief DB outage every one of (10k users × concurrent) updates hits
	// the error path, and an unthrottled flood can itself trip Telegram 429s at
	// the worst possible moment.
	dbErrMu   sync.Mutex
	lastDBErr time.Time

	// bgWG tracks background goroutines (AI loop, GM/GN, an in-flight mass-msg) so
	// Run can drain them on shutdown before the DB pool is closed; runCtx is the
	// run-scoped context they derive from.
	bgWG   sync.WaitGroup
	runCtx context.Context
}

// goBG runs fn as a tracked background goroutine, awaited by Run on shutdown.
func (b *Bot) goBG(fn func()) {
	b.bgWG.Add(1)
	go func() {
		defer b.bgWG.Done()
		fn()
	}()
}

// waitBackground waits for tracked background work to finish, bounded by timeout
// so a long mass-broadcast can't hang shutdown indefinitely.
func (b *Bot) waitBackground(timeout time.Duration) {
	done := make(chan struct{})
	go func() { b.bgWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		slog.Warn("background work didn't finish within the shutdown grace period")
	}
}

// allowDBErrReport reports true at most once per 30s, so DB-outage error reports
// to ERROR_CHAT_ID are coalesced instead of flooding.
func (b *Bot) allowDBErrReport() bool {
	b.dbErrMu.Lock()
	defer b.dbErrMu.Unlock()
	now := time.Now()
	if !b.lastDBErr.IsZero() && now.Sub(b.lastDBErr) < 30*time.Second {
		return false
	}
	b.lastDBErr = now
	return true
}

// New builds the Telegram bot, dispatcher and updater, and registers handlers.
func New(cfg *config.Config, database *db.DB, txt *texts.Loader) (*Bot, error) {
	limiter := newSendLimiter()

	botOpts := &gotgbot.BotOpts{
		RequestOpts: &gotgbot.RequestOpts{Timeout: 10 * time.Second},
	}
	if cfg.Proxy != "" {
		u, err := url.Parse(cfg.Proxy)
		if err != nil {
			return nil, fmt.Errorf("bot: invalid PROXY %q: %w", cfg.Proxy, err)
		}
		botOpts.BotClient = &gotgbot.BaseBotClient{
			Client: http.Client{
				Transport: &http.Transport{Proxy: http.ProxyURL(u)},
			},
			DefaultRequestOpts: &gotgbot.RequestOpts{Timeout: 30 * time.Second},
		}
	}

	// Every API call goes through the limiter — including the ones a future change
	// adds, which is the point of doing it here rather than per call site.
	base := botOpts.BotClient
	if base == nil {
		base = &gotgbot.BaseBotClient{DefaultRequestOpts: botOpts.RequestOpts}
	}
	lc := &limitedClient{inner: base, limiter: limiter}
	botOpts.BotClient = lc

	tg, err := gotgbot.NewBot(cfg.BotToken, botOpts)
	if err != nil {
		return nil, fmt.Errorf("bot: new bot: %w", err)
	}

	// Callback-token cipher, keyed off BOT_TOKEN (+ any TOKEN_KEYS). Fail closed:
	// without a key the bot would emit unprotected/linkable tokens, so refuse to
	// start rather than silently regress anonymity. (BOT_TOKEN is required, so this
	// only trips on a misconfigured build.)
	tokens := encoder.NewTokenCipher(cfg.BotToken, cfg.TokenKeys...)
	if !tokens.Ready() {
		return nil, fmt.Errorf("bot: callback-token cipher has no key (BOT_TOKEN missing?)")
	}

	b := &Bot{
		TG:         tg,
		DB:         database,
		Cfg:        cfg,
		Texts:      txt,
		Dyn:        dynset.New(cfg.DynsetPath, cfg.AIURL, cfg.AISessionID, cfg.DonationLink),
		Tokens:     tokens,
		users:      newUserStore(),
		aiQueue:    newAIQueue(),
		admins:     make(map[string]bool, len(cfg.Admins)),
		errReports: newErrReportStore(50),
		limiter:    limiter,
	}
	// Wired after b exists: the flush path is a method on b.
	b.batcher = newBatcher(cfg.BatchWindow, cfg.AlbumWindow, cfg.BatchMaxWait, cfg.BatchMaxMessages, b.deliverUnits)
	for _, a := range cfg.Admins {
		b.admins[a] = true
	}
	// Wired after b exists: a failed send is queued durably instead of lost.
	lc.onRetryable = b.enqueueFailedSend

	b.Dispatcher = ext.NewDispatcher(&ext.DispatcherOpts{
		Error: b.onError,
		Panic: b.onPanic,
		// Process updates strictly sequentially and IN ORDER (limiter of 1,
		// acquired before each goroutine is spawned). This matches the Python
		// bot's PTB default (concurrent_updates=False) that the conversation FSM
		// and media-group/album state machine were designed around: gotgbot's
		// default 50-way concurrent dispatch would otherwise let two updates from
		// the SAME user race the conversation-state read and the media-group
		// stash (e.g. a second album item re-entering the send path and clearing
		// the group → lost album). Background jobs stay concurrent.
		MaxRoutines: 1,
	})
	b.Updater = ext.NewUpdater(b.Dispatcher, &ext.UpdaterOpts{})

	b.registerHandlers()
	return b, nil
}

// Run starts long polling and blocks until ctx is cancelled, then stops cleanly.
func (b *Bot) Run(ctx context.Context) error {
	b.runCtx = ctx
	if err := b.Updater.StartPolling(b.TG, &ext.PollingOpts{
		GetUpdatesOpts: &gotgbot.GetUpdatesOpts{
			// Set AllowedUpdates EXPLICITLY. Telegram remembers the last
			// allowed_updates passed to getUpdates and reuses it whenever the
			// parameter is omitted; if anything ever called getUpdates with a list
			// that excluded callback_query, polling without this would silently stop
			// delivering button presses. Listing exactly what the bot handles makes
			// it deterministic (and immune to that gotcha).
			AllowedUpdates: []string{"message", "edited_message", "callback_query"},
			Timeout:        9, // long-poll seconds
			RequestOpts: &gotgbot.RequestOpts{
				Timeout: 11 * time.Second, // slightly above the long-poll timeout
			},
		},
	}); err != nil {
		return fmt.Errorf("bot: start polling: %w", err)
	}

	slog.Info("bot polling", "username", b.TG.User.Username)

	// Start the background jobs (set_commands, AI responder, GM/GN greetings,
	// hourly DB check); they stop when ctx is cancelled.
	b.startBackground(ctx)

	<-ctx.Done()
	slog.Info("stopping updater")
	err := b.Updater.Stop()
	// Deliver whatever was buffered right before SIGTERM. Polling has stopped, so no
	// new messages can be added; this runs before the DB pool is closed. Bounded by
	// a deadline so a hung Telegram call cannot block shutdown forever.
	if b.batcher != nil {
		b.batcher.Shutdown(10 * time.Second)
	}
	// Drain tracked background work before main closes the DB pool, so nothing
	// uses a closed pool (bounded — see waitBackground).
	b.waitBackground(10 * time.Second)
	return err
}

// isAdmin reports whether a uid is in the configured ADMINS list.
func (b *Bot) isAdmin(uid string) bool { return b.admins[uid] }

// onError is the central dispatcher error handler, mirroring
// modules/Global/error_handler.py: it logs, sends a diagnostic report to
// ERROR_CHAT_ID, and replies to the user with a tracking code.
//
// To keep the error channel readable, the report is sent as a compact SUMMARY
// (the tracking code + the error) with a "🔎 more" button; the full detail — the
// triggering update dumped as pretty-printed JSON, paged into <pre> blocks under
// Telegram's length limit — is revealed one page at a time on demand by errMore,
// not dumped all at once. The complete detail is ALSO written to the process
// logs, so it survives even after the in-memory pages are evicted. (A Go error
// carries no stack trace here — unlike a Python exception's __traceback__ — so
// the error string stands in for the traceback and the update dump is the
// primary diagnostic.)
func (b *Bot) onError(tg *gotgbot.Bot, ctx *ext.Context, err error) ext.DispatcherAction {
	code := encoder.GenerateCID(8)

	// Build the update dump once; it goes both to the logs and to the paged report.
	// REDACTED — see redactUpdate. This is an anonymous-messaging bot, and the raw
	// dump carried users' message bodies into the error channel and the logs.
	var updateJSON string
	if ctx != nil && ctx.Update != nil {
		updateJSON = redactUpdate(ctx.Update)
	}
	slog.Error("handler error", "code", code, "err", err, "update", updateJSON)

	var pages []string
	if updateJSON != "" {
		// Page the update dump (escaped) so each page fits in one message under
		// the <pre></pre> wrapper; split on rune boundaries so a multi-byte
		// character is never cut in half.
		pages = chunkString(html.EscapeString(updateJSON), 3500)
	}
	summary := fmt.Sprintf("⚠️ <b>خطایی هنگام پردازش یک آپدیت رخ داد</b>\nکد پیگیری: <code>%s</code>\n\n<pre>%s</pre>",
		code, html.EscapeString(truncate(fmt.Sprintf("%v", err), 600)))
	b.reportIncident(tg, ctx, code, summary, pages)
	return ext.DispatcherActionNoop
}

// onPanic is the dispatcher panic handler. gotgbot recovers handler panics to
// keep the single bot process alive, but WITHOUT this hook a recovered panic is
// only returned to the updater and logged — it never reaches ERROR_CHAT_ID and
// the user gets no tracking code (worse than the Python error_handler, which
// reported exceptions). We route it through the same incident report as onError,
// paging the STACK TRACE (which a plain error doesn't carry).
func (b *Bot) onPanic(tg *gotgbot.Bot, ctx *ext.Context, r any) {
	code := encoder.GenerateCID(8)
	stack := string(debug.Stack())
	slog.Error("handler panic", "code", code, "panic", fmt.Sprintf("%v", r), "stack", stack)

	pages := chunkString(html.EscapeString(stack), 3500)
	summary := fmt.Sprintf("🛑 <b>panic هنگام پردازش یک آپدیت</b>\nکد پیگیری: <code>%s</code>\n\n<pre>%s</pre>",
		code, html.EscapeString(truncate(fmt.Sprintf("%v", r), 600)))
	b.reportIncident(tg, ctx, code, summary, pages)
}

// reportIncident files a tracked incident: it stores the detail pages under the
// tracking code, sends a compact summary to ERROR_CHAT_ID with a "🔎 more" button
// that pages the detail on demand, and replies to the triggering user with the
// code. Shared by onError (the update dump) and onPanic (the stack trace).
func (b *Bot) reportIncident(tg *gotgbot.Bot, ctx *ext.Context, code, summary string, detailPages []string) {
	if b.Cfg.ErrorChatID != "" {
		if chatID, perr := strconv.ParseInt(b.Cfg.ErrorChatID, 10, 64); perr == nil {
			b.errReports.put(code, detailPages)
			opts := &gotgbot.SendMessageOpts{ParseMode: "HTML"}
			// A link button, not a callback one: ERROR_CHAT_ID is a channel and
			// channel callback buttons never reach the bot, so the old "more" button
			// did nothing. The link opens the detail in the admin's private chat.
			if len(detailPages) > 0 && b.TG.User.Username != "" {
				opts.ReplyMarkup = moreLinkButton(b.TG.User.Username, code, len(detailPages))
			}
			_, _ = tg.SendMessage(chatID, summary, opts)
		}
	}
	if ctx != nil && ctx.EffectiveMessage != nil {
		_, _ = ctx.EffectiveMessage.Reply(tg,
			fmt.Sprintf("خطایی رخ داد. کد پیگیری: <code>%s</code>", code),
			&gotgbot.SendMessageOpts{ParseMode: "HTML"},
		)
	}
}

// reportToErrorChat sends a plain diagnostic line to ERROR_CHAT_ID, best-effort
// (used by handleErr's database-error branch, mirroring decorators.py's
// "PostgreSQL ERROR: ..." notice). A no-op when ERROR_CHAT_ID is unset/invalid.
func (b *Bot) reportToErrorChat(tg *gotgbot.Bot, text string) {
	if b.Cfg.ErrorChatID == "" {
		return
	}
	chatID, err := strconv.ParseInt(b.Cfg.ErrorChatID, 10, 64)
	if err != nil {
		return
	}
	_, _ = tg.SendMessage(chatID, text, nil)
}

// --- Telegram flood-wait handling -------------------------------------------
//
// A 429 means the bot is over Telegram's send rate. Continuing to send while a
// retry_after is pending is what turns a short wait into a long one, so the wait
// is recorded once and every outbound path checks it first. That is the difference
// between a 90-second pause and the 607-second ban that was observed.

// noteFloodWait records a Telegram-imposed wait so every outbound call backs off.
func (b *Bot) noteFloodWait(seconds int64) { b.limiter.noteFloodWait(seconds) }

// allowFloodReport throttles the admin notice to one per two minutes, so a flood
// does not produce a flood of reports about the flood.
func (b *Bot) allowFloodReport() bool { return b.limiter.allowReport() }
