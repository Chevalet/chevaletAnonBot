package bot

import (
	"sync"
	"time"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/PaulSonOfLars/gotgbot/v2/ext"
)

// userData is the Go equivalent of python-telegram-bot's per-user
// context.user_data dict. The Python conversation flow (start/answer/send,
// media groups) stashed all of its cross-update state there; we keep the same
// fields here, typed explicitly.
//
// Each userData carries its own mutex. prep locks it for the whole duration of
// an update's handler so that two updates from the SAME user never race on
// these fields (gotgbot dispatches updates concurrently, up to MaxRoutines).
// Different users still proceed in parallel. This is a little stricter than
// PTB (which did not lock user_data) but is required for memory safety in Go
// and, helpfully, serialises the rapid-fire messages of a media group.
type userData struct {
	mu sync.Mutex
	d  convData

	// sendTimes: unix-nano timestamps of this user's recent anonymous sends, for
	// the outbound rate limit. Deliberately NOT part of convData, so clear()/
	// /cancel can't reset it (a flooder mustn't dodge the limit by cancelling).
	// Guarded by mu (prep holds it for the whole handler).
	sendTimes []int64

	// perTarget: recent send times PER RECIPIENT. The global limit above bounds
	// total output, but the harassment vector on an anonymous bot is one person
	// flooding ONE victim — 40/min all aimed at the same inbox is still 40 messages
	// they cannot stop. Keyed by target uid, same nano timestamps, same lifetime as
	// sendTimes so cancelling cannot reset it.
	perTarget map[string][]int64

	// lastWarn: when the user was last told to slow down. A flooder tripping the
	// limit hundreds of times must not get hundreds of replies — each one is an API
	// call, so warning on every trip turns the anti-spam measure into its own flood.
	lastWarn int64

	// lastTarget*: the conversation this user most recently sent an anonymous
	// message to, and when. A plain follow-up message inside ContinueWindow goes to
	// the same recipient instead of falling through to the catch-all — the
	// conversation state itself ends as soon as a message is sent. In memory only,
	// never logged, wiped when it expires (see continueTarget).
	// Guarded by mu (prep holds it for the whole handler).
	lastTargetUID string
	lastTargetCid string
	lastTargetAt  time.Time

	// lastAccess: when this entry was last fetched. The userStore sweep evicts
	// long-idle entries to bound memory under a large/hostile population.
	// Guarded by the userStore mutex (set in get, read in sweep).
	lastAccess time.Time
}

// sendRateMax / sendRateWindow bound one user's anonymous sends. Generous by
// design — a human composing distinct messages never approaches 40/min, but it
// caps an automated flood (the anonymous-harassment amplifier). The Python
// original had no limit.
const (
	sendRateMax    = 40
	sendRateWindow = time.Minute

	// perTargetMax bounds messages to ONE recipient in the same window. A human
	// conversation never needs this many in a minute; a flood aimed at one person
	// does. This is the limit that actually protects a victim.
	perTargetMax = 8

	// warnCooldown is how often a rate-limited user is told to slow down. Replying
	// to every blocked attempt would cost one API call per attempt, which is exactly
	// the load the limit exists to prevent.
	warnCooldown = 30 * time.Second
)

// prune drops timestamps older than the window and returns what is left.
func prune(times []int64, cutoff int64) []int64 {
	kept := times[:0]
	for _, t := range times {
		if t >= cutoff {
			kept = append(kept, t)
		}
	}
	return kept
}

// allowSendTo reports whether this user may send another anonymous message to
// target right now, and whether the caller should say so.
//
// Two limits, because they catch different abuse:
//   - the global one bounds total output from this account;
//   - the per-target one bounds what a single VICTIM can be made to receive, which
//     is the harassment the anonymity makes possible in the first place.
//
// warn is true at most once per warnCooldown. A flooder that trips the limit two
// hundred times must not receive two hundred replies: each is an API call, and
// answering every attempt would make the anti-spam measure a flood of its own.
//
// The caller holds u.mu (prep does).
func (u *userData) allowSendTo(target string) (allowed, warn bool) {
	now := time.Now().UnixNano()
	cutoff := now - int64(sendRateWindow)

	u.sendTimes = prune(u.sendTimes, cutoff)

	if u.perTarget == nil {
		u.perTarget = make(map[string][]int64)
	}
	// Prune every target, not just this one, so an abandoned conversation cannot
	// keep its slice alive for the lifetime of the process.
	for t, times := range u.perTarget {
		if kept := prune(times, cutoff); len(kept) == 0 {
			delete(u.perTarget, t)
		} else {
			u.perTarget[t] = kept
		}
	}

	overGlobal := len(u.sendTimes) >= sendRateMax
	overTarget := target != "" && len(u.perTarget[target]) >= perTargetMax

	if overGlobal || overTarget {
		if now-u.lastWarn >= int64(warnCooldown) {
			u.lastWarn = now
			return false, true
		}
		return false, false
	}

	u.sendTimes = append(u.sendTimes, now)
	if target != "" {
		u.perTarget[target] = append(u.perTarget[target], now)
	}
	return true, false
}

// convData holds exactly the keys the Python handlers stored in user_data.
// Empty string / nil / false / zero stand in for Python's missing-key / None.
type convData struct {
	// single-message send flow (set by start_cmd / answer, read by send_msg_template)
	targetUID    string // "target_uid": the resolved target user id (server-side only)
	targetCid    string // "target_cid": cid, or "" when not a /start send
	replyTo      string // "reply_to": target message id, or "" when not an answer
	channelReply bool   // "channel_reply": True only for reply-to-channel sends

	// media-group send flow (spans several updates)
	mediaGroupID         string                        // "media_group_id"
	groupMsgs            []*gotgbot.Message            // "group_msgs"
	groupExpiration      float64                       // "group_expiration": unix seconds, 0 = unset
	groupTargetUID       string                        // "group_target_uid": resolved target user id
	groupWasChannelReply bool                          // "group_was_channel_reply"
	groupNotifyMsg       *gotgbot.Message              // "group_notify_msg"
	groupReplyMarkup     *gotgbot.InlineKeyboardMarkup // "group_reply_markup"
	groupWarningMsgID    string                        // "group_warning_msg_id"
	// groupBoxOn marks an album whose recipient has the Message Box enabled: the
	// album's first item was stored by send_msg and the remaining items must be
	// stored (not copied) by handle_media. groupBoxTargetCid keeps the link id the
	// album arrived through for the stored rows.
	groupBoxOn        bool
	groupBoxTargetCid string

	// "sent_medias": message ids already copied to the target for this group.
	sentMedias []string

	// settings conversation (settings.py): the menu message id to delete after a
	// name/tag update.
	ogMID int64 // "og_mid"

	// anonymous-nickname flow: the nickname typed in state "3", held here until
	// the emoji step (state "4") completes so name+emoji+enabled commit together.
	// Staging it (instead of writing on state "3") means aborting at the emoji
	// step leaves any existing live signature untouched, and clear() truly cancels.
	anonPendingName string

	// my_links conversation (my_links.py): the cid being renamed and the links
	// message id to refresh.
	chosenCid string // "chosen_cid"
	linksMID  int64  // "links_mid"
}

// clear mirrors context.user_data.clear(). It resets every stashed value but
// deliberately leaves the mutex untouched (prep still holds it).
func (u *userData) clear() { u.d = convData{} }

// userStore maps each Telegram user id to their userData. The store mutex only
// guards get-or-create of the map; the per-user mutex inside userData guards
// the fields themselves.
type userStore struct {
	mu sync.Mutex
	m  map[int64]*userData
}

func newUserStore() *userStore { return &userStore{m: make(map[int64]*userData)} }

func (s *userStore) get(uid int64) *userData {
	s.mu.Lock()
	defer s.mu.Unlock()
	ud := s.m[uid]
	if ud == nil {
		ud = &userData{}
		s.m[uid] = ud
	}
	ud.lastAccess = time.Now()
	return ud
}

// sweep evicts entries idle longer than idle that aren't currently in use (a
// successful TryLock proves no handler holds them), bounding memory under a
// large or hostile user population. Returns how many were evicted.
func (s *userStore) sweep(idle time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-idle)
	n := 0
	for uid, ud := range s.m {
		if ud.lastAccess.After(cutoff) {
			continue
		}
		if ud.mu.TryLock() {
			delete(s.m, uid)
			ud.mu.Unlock()
			n++
		}
	}
	return n
}

// ud returns the locked-by-prep userData for the effective user of this update.
// Handlers call this to reach the cross-update state; prep has already taken
// the lock for the duration of the handler.
func (b *Bot) ud(ctx *ext.Context) *userData {
	return b.users.get(ctx.EffectiveUser.Id)
}

// noteSentTarget records the conversation a plain anonymous message was accepted
// for, so a follow-up typed before ContinueWindow expires is routed to the same
// recipient (see continueTarget). The caller holds u.mu (prep does).
func (u *userData) noteSentTarget(targetUID, targetCid string) {
	u.lastTargetUID = targetUID
	u.lastTargetCid = targetCid
	u.lastTargetAt = time.Now()
}

// continueTarget returns the recipient a plain follow-up should go to within
// window, or ok=false when the window has expired (in which case the stale target
// is dropped so it cannot be resurrected). The caller holds u.mu (prep does).
func (u *userData) continueTarget(window time.Duration) (targetUID, targetCid string, ok bool) {
	if window <= 0 || u.lastTargetUID == "" || u.lastTargetAt.IsZero() {
		return "", "", false
	}
	if time.Since(u.lastTargetAt) > window {
		u.lastTargetUID = ""
		u.lastTargetCid = ""
		u.lastTargetAt = time.Time{}
		return "", "", false
	}
	return u.lastTargetUID, u.lastTargetCid, true
}

// nowSeconds reproduces Python's time.time() (wall-clock seconds as a float).
func nowSeconds() float64 { return float64(time.Now().UnixNano()) / 1e9 }
