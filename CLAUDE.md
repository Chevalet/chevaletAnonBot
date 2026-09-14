# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A Telegram bot for sending anonymous messages between users, written in Go
(`gotgbot/v2` + `pgx/v5`/PostgreSQL). The bot's user-facing text is Persian.

This is a **line-by-line port of an earlier Python bot** (which lives on the
`python` branch / a separate project). Behavior is intentionally faithful to
the original — see [`MIGRATION.md`](MIGRATION.md) for the full Python→Go
architecture mapping and the production cutover runbook. When touching a
handler, check whether a comment references the Python source module (e.g.
`decorators.py`, `start.py`) — that's the behavior to preserve.

## Commands

```sh
go build ./...
go vet ./...
gofmt -l .            # must print nothing; `gofmt -w .` to fix
go test ./...                    # encoder golden vectors + rest; DB tests skip without DB_HOST
go test -race -count=1 ./...     # what CI actually runs — run this before every PR
go test ./internal/encoder/...   # single package
go test ./internal/bot/ -run TestSendMsg   # single test

# End-to-end handler-routing tests (real dispatcher + fake Bot API). They need
# their OWN database, separate from the one the db tests drop tables in:
#   docker exec <pg> createdb -U <user> botit
#   DB_HOST=localhost DB_PORT=55432 BOT_TEST_DB=botit DB_USER=… DB_PASS=… \
#     go test ./internal/bot/ -run TestAnchor
go run ./cmd/bot      # needs a populated .env + reachable PostgreSQL (+ PROXY)
```

Makefile shortcuts: `make go-build`, `make go-vet`, `make go-fmt`, `make go-test`.
Docker workflow: `make up` / `make down` / `make rebuild` / `make logs` /
`make backup` / `make restore FILE=...` — run `make help` for the full list.

CI (`.github/workflows/ci.yml`) runs build, vet, gofmt-check, and
`go test -race -count=1 ./...` on every push. The DB integration test skips
itself automatically when `DB_HOST` is unset, so CI runs everything except
that one without needing a database.

Production deploys are **tag-driven**: pushing a `vX.Y.Z` tag deploys that exact
tag; pushing to `main` only runs the tests. Rollback is a `workflow_dispatch` run
with an older tag. Don't add a branch-push deploy back —
[`deploy/go/RELEASING.md`](deploy/go/RELEASING.md) explains the flow and why the
server-side forced command validates the tag itself.

To live-test end-to-end against a throwaway Postgres with a staging bot
token, use the isolated stack in [`deploy/go/`](deploy/go/README.md) (needs
`PROXY` — Telegram is generally unreachable without one).

## Architecture

### Anonymity mechanism (read this before touching ids)

Every Telegram user has a numeric user id, which the bot must never expose.
Each user gets a `chevaletid`, encoded with a custom reversible cipher
(`internal/encoder`) before it appears anywhere, including inline-button
`callback_data`. When user A messages user B, B's buttons carry A's *encoded*
id; pressing one makes the bot decode it, look up the real id in Postgres,
act, and re-encode before it's exposed again. Real ids never leave the DB.

**`internal/encoder` is a frozen compatibility contract — do not change its
output.** Buttons on already-delivered messages (including from the Python
bot era) embed ids encoded with this exact cipher; changing it breaks
reply/seen/block/report on every historical message. It's locked by an
exhaustive golden-vector test (`encoder_test.go`, ~6966 vectors covering every
key × alphabet character). If you must touch this package, the test must
still pass unmodified.

Separately, `encoder.TokenCipher` (`internal/encoder/token.go`) seals
per-emission callback tokens keyed off `BOT_TOKEN` (+ optional `TOKEN_KEYS`)
for newly-sent messages — this one is not a frozen contract, just the current
mechanism. The bot **fails closed** at startup if this cipher has no key
(`bot.New` returns an error) rather than silently emitting linkable tokens.

### Request flow

`cmd/bot/main.go` loads config → connects DB → ensures schema
(`CREATE TABLE IF NOT EXISTS`, so pointing at an existing DB preserves data)
→ builds the `bot.Bot` → starts the health listener → runs long polling until
SIGINT/SIGTERM.

`internal/bot/bot.go` builds the gotgbot `Bot`/`Dispatcher`/`Updater`. The
dispatcher runs with `MaxRoutines: 1` — updates are processed strictly
sequentially and in order, matching the Python bot's `concurrent_updates=False`
that the conversation state machine and media-group/album handling depend on.
Background jobs (AI loop, greetings, hourly DB check) run concurrently outside
this serialization via `goBG`/`bgWG`, and are drained on shutdown before the
DB pool closes.

Every handler is wrapped by `prep` (`internal/bot/prep.go`), the middleware
mirroring the Python `@prep_function` decorator: filters out edited
messages/non-private chats/the GM group, initializes the user row (creating a
`cid`/`chevaletid` on first contact), rejects banned users, serializes a given
user's updates via a per-user mutex, then dispatches. `handleErr` maps DB and
network errors to best-effort user replies and throttled error-channel
reports; anything else reaches the central `onError`/`onPanic` hooks, which
file a tracking-coded incident to `ERROR_CHAT_ID` (paged detail behind a "more"
button) and reply the code to the user.

`internal/bot/handlers.go` registers everything in the same order as the
Python `main.py` handler list (order encodes precedence — the conversation
handlers outrank the catch-all, which is always last). The three
`ConversationHandler`s (`start`, `settings`, `my_links`) are per-user
in-memory state machines keyed by `KeyStrategySenderAndChat`, ported from the
Python bot's `ConversationHandler` states.

`/menu` (`internal/bot/menu.go`) is the BotFather-style panel users see: one
message edited in place, four sections plus an admin-only fifth. Its "my links"
and "settings" buttons carry the callback data those two conversations already
accept as **entry points** (`mylinks-menu` / `settings-menu`) rather than
reimplementing them — so don't "tidy" that data. Every other panel button is
`menu|`-prefixed, registered *before* the conversations, and deliberately free of
the substrings their `cqContains` filters match; `menu_test.go` asserts that, so a
colliding button fails the build instead of silently doing nothing.

The Telegram command list is published at startup by `setCommands`
(`background.go`) in two scopes: `botCommands()` goes to everyone with `/menu`
first, and `adminBotCommands()` goes to each configured admin's own chat via
`BotCommandScopeChat` so the admin commands are visible only to them. Access is
enforced at the handlers (each admin handler checks `isAdmin`), not by the list.
`menu_test.go` pins `/menu` to the front and asserts no admin command leaks into
the public list.

### Package layout

```
cmd/bot/            main entrypoint
internal/bot/       handlers, conversations, jobs — the bot logic (largest package)
internal/db/        PostgreSQL layer (pgx) — users, blocks, cids, reports
internal/encoder/   the chevaletid cipher (frozen) + the callback TokenCipher
internal/config/    env configuration (.env loading)
internal/texts/     Texts/*.txt loader (Persian message templates)
internal/health/    TCP health listener
internal/dynset/    dynamic_settings.json — runtime-tunable AI settings, hot-reloaded
Texts/              Persian message templates, loaded at runtime (edit without recompiling)
deploy/go/          isolated staging stack (throwaway Postgres + staging bot token)
```

Within `internal/bot`, filenames generally mirror the Python module they port
(e.g. `admin.go` ~ `admin.py`, `aichat.go`/`aiqueue.go` ~ the AI auto-reply
flow, `callbacks.go` ~ shared callback plumbing, `background.go`/`jobs.go` ~
the greeting/health background jobs).

### Configuration

All config is env vars loaded via `.env` (`internal/config`). See
[`.env.example`](.env.example) for the full list; key ones are `BOT_TOKEN`,
`PROXY` (outbound proxy for Telegram — required in most deployment
environments), `ADMINS` (`|`-separated uids), `DB_HOST`/`DB_PORT`/`DB_NAME`/
`DB_USER`/`DB_PASS`, `REPORT_CHAT_ID`/`ERROR_CHAT_ID`, `GM_GROUP_ID`/
`GM_GROUP_TOPIC_ID`, `AI_URL`/`AI_SESSION_ID`/`AI_INTERVAL`, `HEALTH_PORT`.

## Conventions

- Commit prefixes: `feat`, `fix`, `maint` (refactors/deps/chores), `docs`,
  `ci`. Imperative subject line.
- Match the surrounding package's naming, comment density, and idioms —
  comments in this codebase frequently explain *why* (often citing the
  original Python behavior being preserved), not what.
- If a change alters user-visible behavior from the Python original, call
  that out explicitly rather than folding it in silently.
