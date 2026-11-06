package db

// messagebox.go is the DB half of the "message box" feature: while a recipient
// has the box switched ON (users.message_box, see users.go), the bot stores each
// incoming delivery here instead of forwarding it immediately, and later drains
// the queue as one batch at a time the recipient chooses.
//
// PRIVACY MODEL: a row stores only the REFERENCES needed to reproduce the send
// later — the source chat/message id to copy from, plus small metadata — and never
// the message body or its media. At flush time the bot re-copies the original
// Telegram message by id; the text never enters this table (see MakeTables).
//
// Claim/status protocol (mirrors the outbox worker): a row is inserted as
// 'pending'; a delivery worker claims a prefix for one recipient by flipping it to
// 'delivering' with a unique claim token; on success the rows are deleted, on
// failure ReleaseBoxClaim puts them back, and ResetStaleBoxClaims recovers rows
// whose worker died mid-delivery.

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// BoxMessage is one queued delivery and also the shape of the row returned by a
// claim. The fields mirror the message_box columns 1:1; ID/status/claim/created_at
// are DB-managed (status and claim are not part of this view — a returned row is
// by definition in the delivering state under the caller's claim).
type BoxMessage struct {
	ID               int64
	RecipientUID     string
	SenderUID        string
	SourceChatID     int64
	SourceMessageID  int64
	MediaGroupID     string
	Kind             string
	TargetCID        string
	ReplyToChatID    int64
	ReplyToMessageID int64
	QuoteText        string
	QuotePosition    int64
	ExternalReply    bool
	ChannelReply     bool
}

// boxColumns is the column list (in BoxMessage field order) used by every read
// path. Nullable string columns are COALESCEd to the empty string so scanning
// never sees NULL; the AS aliases keep the source column names, which the
// UPDATE…RETURNING CTE in ClaimBoxMessages relies on (the outer SELECT re-lists
// these same names).
const boxColumns = `id,
	recipient_uid,
	sender_uid,
	source_chat_id,
	source_message_id,
	COALESCE(media_group_id, '') AS media_group_id,
	kind,
	COALESCE(target_cid, '') AS target_cid,
	reply_to_chat,
	reply_to_message,
	COALESCE(quote_text, '') AS quote_text,
	quote_position,
	external_reply,
	channel_reply`

// scanBoxMessage scans one message_box row selected with boxColumns.
func scanBoxMessage(row pgx.Row) (BoxMessage, error) {
	var m BoxMessage
	err := row.Scan(&m.ID, &m.RecipientUID, &m.SenderUID,
		&m.SourceChatID, &m.SourceMessageID, &m.MediaGroupID, &m.Kind,
		&m.TargetCID, &m.ReplyToChatID, &m.ReplyToMessageID,
		&m.QuoteText, &m.QuotePosition, &m.ExternalReply, &m.ChannelReply)
	return m, err
}

// nullableText maps the model's zero-value string to SQL NULL, so optional
// columns hold NULL (not the empty string) exactly as the schema declares them;
// the read paths COALESCE them back to the empty string.
func nullableText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// AddBoxMessages queues a batch of deliveries for their recipients, all in the
// 'pending' state, in one pgx batch (one round trip). The caller orders them; the
// queue preserves insertion order via the BIGSERIAL id.
func (db *DB) AddBoxMessages(ctx context.Context, items []BoxMessage) error {
	if len(items) == 0 {
		return nil // nothing to queue
	}
	const q = `INSERT INTO message_box
		(recipient_uid, sender_uid, source_chat_id, source_message_id,
		 media_group_id, kind, target_cid, reply_to_chat, reply_to_message,
		 quote_text, quote_position, external_reply, channel_reply, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,'pending')`

	batch := &pgx.Batch{}
	for _, m := range items {
		batch.Queue(q, m.RecipientUID, m.SenderUID, m.SourceChatID,
			m.SourceMessageID, nullableText(m.MediaGroupID), m.Kind,
			nullableText(m.TargetCID), m.ReplyToChatID, m.ReplyToMessageID,
			nullableText(m.QuoteText), m.QuotePosition, m.ExternalReply,
			m.ChannelReply)
	}
	br := db.pool.SendBatch(ctx, batch)
	defer br.Close()
	for range items {
		if _, err := br.Exec(); err != nil {
			return err
		}
	}
	return nil
}

// PendingBoxCount returns how many pending deliveries recipientUID is holding —
// used for the "N messages waiting" badge before a batch is flushed.
func (db *DB) PendingBoxCount(ctx context.Context, recipientUID string) (int, error) {
	var n int
	err := db.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM message_box WHERE recipient_uid=$1 AND status='pending'`,
		recipientUID).Scan(&n)
	return n, err
}

// ClaimBoxMessages atomically takes up to limit pending messages for
// recipientUID, marks them 'delivering' under claim, and returns them ordered by
// id (oldest first). The subselect uses FOR UPDATE SKIP LOCKED so two workers —
// or two bot instances during a deploy overlap — can never claim the same row.
//
// A previous claim should have been released (or reset as stale) before calling;
// only status='pending' rows are eligible, so a second claim of the same batch
// returns nothing.
func (db *DB) ClaimBoxMessages(ctx context.Context, recipientUID string, limit int, claim string) ([]BoxMessage, error) {
	if limit <= 0 {
		return nil, nil // no work requested
	}
	// The UPDATE's RETURNING does not guarantee an order, so claim in a CTE and
	// order the final SELECT by id (oldest first).
	const q = `WITH claimed AS (
		UPDATE message_box
		   SET status='delivering', claim=$2
		 WHERE id IN (
		       SELECT id FROM message_box
		        WHERE recipient_uid=$1 AND status='pending'
		        ORDER BY id
		        LIMIT $3
		        FOR UPDATE SKIP LOCKED)
		 RETURNING ` + boxColumns + `
	)
	SELECT ` + boxColumns + ` FROM claimed ORDER BY id`

	rows, err := db.pool.Query(ctx, q, recipientUID, claim, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []BoxMessage
	for rows.Next() {
		m, err := scanBoxMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeleteBoxMessages removes delivered rows by id (called after a successful
// batch send). Unknown ids are silently ignored.
func (db *DB) DeleteBoxMessages(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := db.pool.Exec(ctx, `DELETE FROM message_box WHERE id = ANY($1)`, ids)
	return err
}

// ReleaseBoxClaim returns a worker's in-flight rows to 'pending' so a later flush
// can retry them (send failed, or shutdown mid-delivery).
func (db *DB) ReleaseBoxClaim(ctx context.Context, claim string) error {
	if claim == "" {
		return nil // a claim token is required; nothing could match it
	}
	_, err := db.pool.Exec(ctx,
		`UPDATE message_box SET status='pending', claim=NULL
		  WHERE claim=$1 AND status='delivering'`, claim)
	return err
}

// ResetStaleBoxClaims recovers rows whose delivering worker died: rows still
// 'delivering' and older than olderThan go back to 'pending'. Returns how many
// rows were recovered.
//
// The age is measured from created_at (the schema has no separate claimed_at), so
// callers should pass a threshold comfortably larger than how long a normal batch
// send takes, and run this before claiming rather than concurrently with a live
// claim.
func (db *DB) ResetStaleBoxClaims(ctx context.Context, olderThan time.Duration) (int64, error) {
	if olderThan <= 0 {
		return 0, nil
	}
	secs := int64(olderThan / time.Second)
	tag, err := db.pool.Exec(ctx,
		`UPDATE message_box SET status='pending', claim=NULL
		  WHERE status='delivering'
		    AND created_at < now() - ($1::bigint * interval '1 second')`, secs)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// CapBoxMessages enforces the anti-flood cap: keeps at most max pending messages
// for recipientUID (the NEWEST ones, since the oldest are the ones already
// waiting) and deletes the rest. Returns how many rows were dropped.
//
// A max of 0 clears all pending rows; a negative max is a no-op.
func (db *DB) CapBoxMessages(ctx context.Context, recipientUID string, max int) (int64, error) {
	if max < 0 {
		return 0, nil
	}
	if max == 0 {
		tag, err := db.pool.Exec(ctx,
			`DELETE FROM message_box WHERE recipient_uid=$1 AND status='pending'`,
			recipientUID)
		if err != nil {
			return 0, err
		}
		return tag.RowsAffected(), nil
	}
	tag, err := db.pool.Exec(ctx,
		`DELETE FROM message_box
		  WHERE recipient_uid=$1 AND status='pending'
		    AND id NOT IN (
		        SELECT id FROM message_box
		         WHERE recipient_uid=$1 AND status='pending'
		         ORDER BY id DESC
		         LIMIT $2)`, recipientUID, max)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
