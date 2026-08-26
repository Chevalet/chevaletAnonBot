package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/PaulSonOfLars/gotgbot/v2/ext"
	"github.com/PaulSonOfLars/gotgbot/v2/ext/handlers"
)

// Handler is a prepared handler: it receives the resolved userid in addition to
// the usual gotgbot arguments, mirroring the Python signature
// (update, context, message, userid, bot, dbh) — here `b` carries the db handle
// and config, so dbh/bot are reachable through it.
//
// The returned error is either a genuine error (routed to the central error
// hook) OR a conversation state change produced by handlers.NextConversationState
// / handlers.EndConversation. prep passes both kinds straight through; only the
// enclosing ConversationHandler interprets the latter (see registerHandlers).
type Handler func(b *Bot, tg *gotgbot.Bot, ctx *ext.Context, userid string) error

// ignoredChat reports whether an update from this chat must be dropped before
// any handler sees it.
//
// It rejects exactly the chats the Python prep rejected (decorators.py:21-31):
// legacy "group"/"channel" chats and the GM group (whose AI flow has its own
// un-prepped handler). Private chats AND ordinary supergroups are allowed —
// Python does NOT reject type=="supergroup", so the live bot still answers
// commands and the catch-all there; an earlier "private only" gate silently
// dropped those, a parity gap. (Python's extra my_chat_member check is moot:
// that update type isn't in AllowedUpdates.)
//
// It additionally rejects a channel's "direct messages" chat, which has no
// Python counterpart because the feature did not exist then. Such a chat is a
// SUPERGROUP, so the rule above deliberately lets it through: once this bot is
// added to a channel that has direct messages enabled, every message users send
// to the channel owner arrives here, and the catch-all would answer it as if the
// sender were talking to the bot — putting bot replies into someone else's
// inbox. chat.is_direct_messages is the official flag for this, and the right
// one to branch on: it lives on the Chat, so it is set for EVERY update type
// (message and callback_query alike), whereas the per-message
// direct_messages_topic exists only on a Message. There is no way to filter
// these out further upstream — allowed_updates selects update types, not chats.
func ignoredChat(ec *gotgbot.Chat, gmGroupID string) bool {
	if ec == nil {
		return false
	}
	if ec.Type == "channel" || ec.Type == "group" {
		return true
	}
	if ec.IsDirectMessages {
		return true
	}
	return gmGroupID != "" && strconv.FormatInt(ec.Id, 10) == gmGroupID
}

// prep wraps a Handler with the middleware from modules/Global/decorators.py
// (@prep_function): it filters out non-private and edited updates, initialises
// the user (row, a cid, a chevaletid), rejects banned users, then dispatches.
func (b *Bot) prep(fn Handler) handlers.Response {
	return func(tg *gotgbot.Bot, ctx *ext.Context) error {
		// Ignore edited messages (Python bailed on update.edited_message).
		if ctx.Update.EditedMessage != nil {
			return nil
		}
		if ignoredChat(ctx.EffectiveChat, b.Cfg.GMGroupID) {
			return nil
		}
		if ctx.EffectiveUser == nil {
			return nil
		}

		userid := strconv.FormatInt(ctx.EffectiveUser.Id, 10)

		// Serialise this user's updates (mirrors PTB's effectively per-user
		// processing and keeps media-group state race-free). Released when the
		// handler returns.
		ud := b.users.get(ctx.EffectiveUser.Id)
		ud.mu.Lock()
		defer ud.mu.Unlock()

		dbctx, cancel := context.WithTimeout(context.Background(), dbOpTimeout)
		defer cancel()

		if err := b.initUser(dbctx, userid, ctx.EffectiveUser); err != nil {
			// init_user returning False (a unique cid couldn't be allocated):
			// tell the user, wipe their state, end the conversation —
			// decorators.py:69-74.
			if errors.Is(err, errInitNoCID) {
				ud.clear()
				if ctx.EffectiveMessage != nil {
					_, _ = ctx.EffectiveMessage.Reply(tg, txtInitNoCID, nil)
				}
				return handlers.EndConversation()
			}
			return b.handleErr(tg, ctx, err)
		}

		banned, err := b.DB.IsBanned(dbctx, userid)
		if err != nil {
			return b.handleErr(tg, ctx, err)
		}
		if banned {
			return nil // banned users are silently ignored, as in the original
		}

		// Record activity for the daily stats. Best effort on purpose: this is a
		// reporting nicety, and a hiccup here must never stop a user's message from
		// being handled. It writes at most once per user per day (see TouchUser).
		if terr := b.DB.TouchUser(dbctx, userid); terr != nil {
			slog.Warn("could not record user activity", "err", terr)
		}

		return b.handleErr(tg, ctx, fn(b, tg, ctx, userid))
	}
}

// handleErr mirrors the except blocks of @prep_function, which wrapped every
// handler. In order, it:
//   - lets conversation state changes through untouched;
//   - swallows the benign Telegram errors PTB ignored (an old callback query, a
//     vanished reply target, a Forbidden from a user who blocked the bot, or a
//     "message is not modified" from re-triggering a menu already showing);
//   - on a database error, replies the "database problem" notice to the user and
//     reports it to ERROR_CHAT_ID (decorators.py `except psycopg2.Error`);
//   - on a Telegram transport/network error, replies the "internet problem"
//     notice (decorators.py `except NetworkError`);
//   - propagates everything else to the central error hook (onError), which —
//     like the global PTB error_handler — notifies ERROR_CHAT_ID and replies a
//     tracking code.
//
// The user-facing replies are best-effort (ignored on failure), exactly like the
// Python `try: reply except: pass`.
func (b *Bot) handleErr(tg *gotgbot.Bot, ctx *ext.Context, err error) error {
	if err == nil {
		return nil
	}
	var sc *handlers.ConversationStateChange
	if errors.As(err, &sc) {
		return err
	}
	switch {
	case errQueryTooOld(err):
		return nil
	case errReplyNotFound(err):
		return nil
	case errMessageNotModified(err): // user re-triggered a menu/state they were already on
		return nil
	case errForbidden(err): // covers "bot was blocked by the user" / "not a member"
		return nil
	case errNoSendRights(err):
		// The bot is in a group where it may not post. A permission fact about that
		// chat, not a bug: logged once at INFO so it is discoverable, never filed as
		// an incident (which would report every single message in that group).
		slog.Info("cannot reply in this chat: no send rights", "chat", chatIDOf(ctx))
		return nil
	case errors.Is(err, errFloodPaused):
		// OUR pause, honouring Telegram's. Must NOT call noteFloodWait: recording a
		// pause because we are already paused is the loop that muted the bot.
		slog.Warn("send skipped: honouring a Telegram flood wait")
		if ctx.EffectiveMessage != nil {
			_, _ = ctx.EffectiveMessage.Reply(tg, txtTooManyRequests, nil)
		}
		return nil

	case floodSeconds(err) > 0 || isFloodErr(err):
		// A Telegram 429 is load, not a bug. Filing it as an incident was actively
		// harmful: each incident posts to ERROR_CHAT_ID and replies a tracking code,
		// so a rate limit produced two MORE sends per occurrence on a bot already
		// over its limit — 321 of them in nine hours. Record the wait so outbound
		// paths stop pushing, tell the user plainly, and notify admins at most once
		// every two minutes.
		secs := floodSeconds(err)
		b.noteFloodWait(secs)
		slog.Warn("telegram rate limit", "retry_after_s", secs)
		if ctx.EffectiveMessage != nil {
			_, _ = ctx.EffectiveMessage.Reply(tg, txtTooManyRequests, nil)
		}
		if b.allowFloodReport() {
			b.reportToErrorChat(tg, fmt.Sprintf(
				"⏳ Telegram rate limit (429). retry_after=%ds — sending is paused until it clears.", secs))
		}
		return nil

	case isDBError(err):
		if ctx.EffectiveMessage != nil {
			_, _ = ctx.EffectiveMessage.Reply(tg, txtDBProblem, nil)
		}
		// Throttle the channel report (coalesce a DB-outage flood); the per-update
		// user reply above is unaffected.
		if b.allowDBErrReport() {
			b.reportToErrorChat(tg, "PostgreSQL ERROR: "+err.Error())
		}
		return nil
	case isNetworkError(err):
		if ctx.EffectiveMessage != nil {
			_, _ = ctx.EffectiveMessage.Reply(tg, txtNetworkProblem, nil)
		}
		return nil
	default:
		return err
	}
}

// chatIDOf is the chat an update came from, 0 if unknown.
func chatIDOf(ctx *ext.Context) int64 {
	if ctx != nil && ctx.EffectiveChat != nil {
		return ctx.EffectiveChat.Id
	}
	return 0
}

// command registers a private-chat command handler wrapped with prep. Used for
// the self-contained Phase 2 commands; conversation entry points use the
// builders below instead so their state changes are honoured.
func (b *Bot) command(name string, fn Handler) {
	b.Dispatcher.AddHandler(handlers.NewCommand(name, b.topLevel(fn)))
}

// topLevel adapts a Handler for registration OUTSIDE a ConversationHandler. Some
// handlers (e.g. delete_msg_clbk) are reused both inside the conversation —
// where their handlers.EndConversation()/NextConversationState return values
// matter — and standalone, where PTB simply ignored that return. A conversation
// state change leaking to the dispatcher would be mistaken for a real error, so
// we swallow it here, exactly reproducing PTB's "return value ignored outside a
// conversation" semantics.
func (b *Bot) topLevel(fn Handler) handlers.Response {
	inner := b.prep(fn)
	return func(tg *gotgbot.Bot, ctx *ext.Context) error {
		err := inner(tg, ctx)
		var sc *handlers.ConversationStateChange
		if errors.As(err, &sc) {
			return nil
		}
		return err
	}
}

// Conversation state names. The Python ConversationHandler used the integer
// state 0 for "currently composing a message to a target"; we keep that label.
const stateSending = "0"
