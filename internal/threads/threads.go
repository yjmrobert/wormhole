// Package threads is the local thread map: which wormhole threads exist on
// this machine, which local Copilot session each one maps to, who the peer
// is and what state the thread is in. It is a single SQLite file under the
// user's state directory and is never shared with another machine.
package threads

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/yjmrobert/wormhole/internal/envelope"
)

// State is the lifecycle of a thread as seen from this machine.
type State string

const (
	// StateSent: we published an ask and have no receipt yet.
	StateSent State = "sent"
	// StateDelivered: the peer's daemon acked the ask.
	StateDelivered State = "delivered"
	// StateReplied: a reply has crossed the thread in either direction.
	StateReplied State = "replied"
	// StateExpired: the thread passed its expires_at without a reply.
	StateExpired State = "expired"
	// StateRejected: the peer's daemon rejected the ask.
	StateRejected State = "rejected"
	// StateClosed: closed by a person.
	StateClosed State = "closed"
	// StatePending: an inbound ask waiting in the inbox for a person.
	StatePending State = "pending"
	// StateDeclined: a person read the message and passed on it. On the
	// recipient's machine `wormhole decline` sets it; on the asker's the
	// `declined` ack does.
	StateDeclined State = "declined"
)

// Direction says who opened the thread.
type Direction string

const (
	DirectionOut Direction = "out" // we asked
	DirectionIn  Direction = "in"  // they asked us
)

// Mode is the delivery mode an inbound thread was materialized with.
type Mode string

const (
	ModeInbox Mode = "inbox"
	ModeAuto  Mode = "auto"
)

// OpenStates are the states in which a thread still expects traffic.
var OpenStates = []State{StateSent, StateDelivered, StateReplied, StatePending}

// Thread is one row of the thread map.
type Thread struct {
	ID             string    `json:"thread_id"`
	Peer           string    `json:"peer"`
	Direction      Direction `json:"direction"`
	Mode           Mode      `json:"mode,omitempty"`
	State          State     `json:"state"`
	LocalSessionID string    `json:"local_session_id,omitempty"`
	RootSessionID  string    `json:"root_session_id,omitempty"`
	Title          string    `json:"title"`
	Repo           string    `json:"repo,omitempty"`
	Workdir        string    `json:"workdir,omitempty"`
	// Opened is true once the local session was shown in the app.
	Opened bool `json:"opened"`
	// StagedKickoff holds the kickoff prompt for an inbound inbox-mode
	// thread whose session has not been started yet.
	StagedKickoff string `json:"-"`
	// SnoozedUntil hides a staged message from the inbox until then; the
	// daemon notifies once more when it passes and clears it.
	SnoozedUntil  time.Time `json:"snoozed_until,omitzero"`
	LastMessageID string    `json:"last_message_id,omitempty"`
	LastPreview   string    `json:"last_preview,omitempty"`
	LastMessageAt time.Time `json:"last_message_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Message is a stored copy of one envelope that crossed a thread.
type Message struct {
	ID        string        `json:"id"`
	ThreadID  string        `json:"thread_id"`
	Type      envelope.Type `json:"type"`
	From      string        `json:"from"`
	To        string        `json:"to"`
	Hop       int           `json:"hop"`
	Body      string        `json:"body"`
	Direction Direction     `json:"direction"`
	CreatedAt time.Time     `json:"created_at"`
	// Raw is the envelope as it was on the wire, so follow-ups can be
	// built from exactly what the peer saw.
	Raw string `json:"-"`
}

// ErrNotFound is returned when a thread or message does not exist.
var ErrNotFound = errors.New("not found")

// SchemaVersion is stored in the database's user_version pragma. Version 2
// added threads.snoozed_until.
const SchemaVersion = 2

// Store is the SQLite-backed thread map.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS threads (
  id TEXT PRIMARY KEY,
  peer TEXT NOT NULL,
  direction TEXT NOT NULL,
  mode TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL,
  local_session_id TEXT NOT NULL DEFAULT '',
  root_session_id TEXT NOT NULL DEFAULT '',
  title TEXT NOT NULL DEFAULT '',
  repo TEXT NOT NULL DEFAULT '',
  workdir TEXT NOT NULL DEFAULT '',
  opened INTEGER NOT NULL DEFAULT 0,
  staged_kickoff TEXT NOT NULL DEFAULT '',
  snoozed_until TEXT NOT NULL DEFAULT '',
  last_message_id TEXT NOT NULL DEFAULT '',
  last_preview TEXT NOT NULL DEFAULT '',
  last_message_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS threads_peer ON threads(peer, state);
CREATE TABLE IF NOT EXISTS messages (
  id TEXT PRIMARY KEY,
  thread_id TEXT NOT NULL REFERENCES threads(id),
  type TEXT NOT NULL,
  from_user TEXT NOT NULL,
  to_user TEXT NOT NULL,
  hop INTEGER NOT NULL,
  body TEXT NOT NULL,
  direction TEXT NOT NULL,
  created_at TEXT NOT NULL,
  raw TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS messages_thread ON messages(thread_id, created_at);
CREATE INDEX IF NOT EXISTS messages_inbound ON messages(direction, from_user, created_at);
`

// Open opens (or creates) the thread database at path. The file and its
// directory are private to the user: bodies live in it.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open threads db: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate threads db: %w", err)
	}
	// user_version marks the schema so a later change can tell a fresh
	// file from one that needs migrating.
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		db.Close()
		return nil, fmt.Errorf("read schema version: %w", err)
	}
	switch {
	case version > SchemaVersion:
		db.Close()
		return nil, fmt.Errorf("threads db schema version %d is newer than this binary supports (%d)", version, SchemaVersion)
	case version < SchemaVersion:
		if err := migrate(db, version); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrate threads db: %w", err)
		}
	}
	if err := os.Chmod(path, 0o600); err != nil && !os.IsNotExist(err) {
		db.Close()
		return nil, fmt.Errorf("chmod threads db: %w", err)
	}
	return &Store{db: db}, nil
}

// migrate brings a database at version from up to SchemaVersion. A fresh
// file (version 0) already has the full schema from the CREATE statements
// and only needs the version stamp.
func migrate(db *sql.DB, from int) error {
	if from == 1 {
		if _, err := db.Exec(`ALTER TABLE threads ADD COLUMN snoozed_until TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	_, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, SchemaVersion))
	return err
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

const timeLayout = time.RFC3339

func fmtTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(s string) time.Time {
	t, _ := time.Parse(timeLayout, s)
	return t
}

// Put inserts or replaces a thread row.
func (s *Store) Put(ctx context.Context, t Thread) error {
	if !envelope.ValidID(t.ID) {
		return fmt.Errorf("put thread: %w", envelope.ErrThread)
	}
	now := time.Now()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	if t.LastMessageAt.IsZero() {
		t.LastMessageAt = t.CreatedAt
	}
	t.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `INSERT INTO threads
		(id, peer, direction, mode, state, local_session_id, root_session_id, title, repo, workdir, opened, staged_kickoff,
		 snoozed_until, last_message_id, last_preview, last_message_at, expires_at, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
		 peer=excluded.peer, direction=excluded.direction, mode=excluded.mode, state=excluded.state,
		 local_session_id=excluded.local_session_id, root_session_id=excluded.root_session_id,
		 title=excluded.title, repo=excluded.repo, workdir=excluded.workdir, opened=excluded.opened,
		 staged_kickoff=excluded.staged_kickoff, snoozed_until=excluded.snoozed_until, last_message_id=excluded.last_message_id,
		 last_preview=excluded.last_preview, last_message_at=excluded.last_message_at,
		 expires_at=excluded.expires_at, updated_at=excluded.updated_at`,
		t.ID, t.Peer, string(t.Direction), string(t.Mode), string(t.State), t.LocalSessionID, t.RootSessionID,
		t.Title, t.Repo, t.Workdir, boolInt(t.Opened), t.StagedKickoff, fmtOptTime(t.SnoozedUntil), t.LastMessageID, t.LastPreview,
		fmtTime(t.LastMessageAt), fmtTime(t.ExpiresAt), fmtTime(t.CreatedAt), fmtTime(t.UpdatedAt))
	if err != nil {
		return fmt.Errorf("put thread: %w", err)
	}
	return nil
}

// fmtOptTime stores the zero time as the empty string.
func fmtOptTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return fmtTime(t)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

const threadCols = `id, peer, direction, mode, state, local_session_id, root_session_id, title, repo, workdir, opened,
	staged_kickoff, snoozed_until, last_message_id, last_preview, last_message_at, expires_at, created_at, updated_at`

type scanner interface{ Scan(dest ...any) error }

func scanThread(r scanner) (Thread, error) {
	var t Thread
	var dir, mode, state string
	var opened int
	var snoozed, last, exp, created, updated string
	err := r.Scan(&t.ID, &t.Peer, &dir, &mode, &state, &t.LocalSessionID, &t.RootSessionID, &t.Title, &t.Repo, &t.Workdir,
		&opened, &t.StagedKickoff, &snoozed, &t.LastMessageID, &t.LastPreview, &last, &exp, &created, &updated)
	if err != nil {
		return Thread{}, err
	}
	t.Direction, t.Mode, t.State, t.Opened = Direction(dir), Mode(mode), State(state), opened == 1
	if snoozed != "" {
		t.SnoozedUntil = parseTime(snoozed)
	}
	t.LastMessageAt, t.ExpiresAt, t.CreatedAt, t.UpdatedAt = parseTime(last), parseTime(exp), parseTime(created), parseTime(updated)
	return t, nil
}

// Get returns one thread.
func (s *Store) Get(ctx context.Context, id string) (Thread, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+threadCols+` FROM threads WHERE id = ?`, id)
	t, err := scanThread(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Thread{}, fmt.Errorf("thread %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Thread{}, fmt.Errorf("get thread: %w", err)
	}
	return t, nil
}

// Filter narrows List.
type Filter struct {
	Open      bool
	Peer      string
	Direction Direction
	State     State
	Unopened  bool
	// Staged selects threads with a message waiting in the inbox.
	Staged bool
	// SnoozeDue selects staged threads whose snooze has passed; the
	// daemon notifies once more and clears it.
	SnoozeDue *time.Time
}

// List returns threads newest first.
func (s *Store) List(ctx context.Context, f Filter) ([]Thread, error) {
	var where []string
	var args []any
	if f.Open {
		ph := make([]string, len(OpenStates))
		for i, st := range OpenStates {
			ph[i] = "?"
			args = append(args, string(st))
		}
		where = append(where, "state IN ("+strings.Join(ph, ",")+")")
	}
	if f.Peer != "" {
		where = append(where, "peer = ?")
		args = append(args, f.Peer)
	}
	if f.Direction != "" {
		where = append(where, "direction = ?")
		args = append(args, string(f.Direction))
	}
	if f.State != "" {
		where = append(where, "state = ?")
		args = append(args, string(f.State))
	}
	if f.Unopened {
		where = append(where, "opened = 0 AND local_session_id <> ''")
	}
	if f.Staged {
		where = append(where, "staged_kickoff <> ''")
	}
	if f.SnoozeDue != nil {
		where = append(where, "staged_kickoff <> '' AND snoozed_until <> '' AND snoozed_until <= ?")
		args = append(args, fmtTime(*f.SnoozeDue))
	}
	q := `SELECT ` + threadCols + ` FROM threads`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY last_message_at DESC, id DESC"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list threads: %w", err)
	}
	defer rows.Close()
	var out []Thread
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, fmt.Errorf("list threads: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Update applies fn to a thread and writes it back.
func (s *Store) Update(ctx context.Context, id string, fn func(*Thread)) (Thread, error) {
	t, err := s.Get(ctx, id)
	if err != nil {
		return Thread{}, err
	}
	fn(&t)
	if err := s.Put(ctx, t); err != nil {
		return Thread{}, err
	}
	return t, nil
}

// AddMessage records an envelope on its thread and refreshes the thread's
// last-message columns. Storing the same message twice is a no-op, which
// makes at-least-once delivery idempotent at this layer.
func (s *Store) AddMessage(ctx context.Context, e envelope.Envelope, dir Direction) error {
	raw, err := envelope.Marshal(e)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO messages
		(id, thread_id, type, from_user, to_user, hop, body, direction, created_at, raw)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		e.ID, e.ThreadID, string(e.Type), e.From.User, e.To.User, e.Hop, e.Body, string(dir), fmtTime(e.CreatedAt), string(raw))
	if err != nil {
		return fmt.Errorf("add message: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	_, err = s.db.ExecContext(ctx, `UPDATE threads SET last_message_id = ?, last_preview = ?, last_message_at = ?, updated_at = ?
		WHERE id = ? AND last_message_at <= ?`,
		e.ID, Preview(e.Body, 80), fmtTime(e.CreatedAt), fmtTime(time.Now()), e.ThreadID, fmtTime(e.CreatedAt))
	if err != nil {
		return fmt.Errorf("update thread preview: %w", err)
	}
	return nil
}

// DeleteMessage removes one stored message copy. Deleting a message that
// is not there is not an error.
func (s *Store) DeleteMessage(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM messages WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete message: %w", err)
	}
	return nil
}

// DeleteThread removes a thread and every message copy on it.
func (s *Store) DeleteThread(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM messages WHERE thread_id = ?`, id); err != nil {
		return fmt.Errorf("delete thread messages: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM threads WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete thread: %w", err)
	}
	return nil
}

// HasMessage reports whether a message id was already stored.
func (s *Store) HasMessage(ctx context.Context, id string) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM messages WHERE id = ?`, id).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

func scanMessage(r scanner) (Message, error) {
	var m Message
	var typ, dir, created string
	if err := r.Scan(&m.ID, &m.ThreadID, &typ, &m.From, &m.To, &m.Hop, &m.Body, &dir, &created, &m.Raw); err != nil {
		return Message{}, err
	}
	m.Type, m.Direction, m.CreatedAt = envelope.Type(typ), Direction(dir), parseTime(created)
	return m, nil
}

const messageCols = `id, thread_id, type, from_user, to_user, hop, body, direction, created_at, raw`

// Messages returns a thread's messages oldest first.
func (s *Store) Messages(ctx context.Context, threadID string) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+messageCols+` FROM messages WHERE thread_id = ? ORDER BY created_at, id`, threadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// LastMessage returns the newest non-ack message on a thread.
func (s *Store) LastMessage(ctx context.Context, threadID string) (Message, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+messageCols+` FROM messages WHERE thread_id = ? AND type <> ? ORDER BY created_at DESC, id DESC LIMIT 1`, threadID, string(envelope.TypeAck))
	m, err := scanMessage(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, ErrNotFound
	}
	return m, err
}

// Envelope reconstructs the wire envelope of a stored message.
func (m Message) Envelope() (envelope.Envelope, error) {
	return envelope.Unmarshal([]byte(m.Raw))
}

// CountInbound counts inbound asks received since a point in time, for the
// per-sender (from != "") and total daily caps.
func (s *Store) CountInbound(ctx context.Context, from string, since time.Time) (int, error) {
	q := `SELECT COUNT(1) FROM messages WHERE direction = ? AND type = ? AND created_at >= ?`
	args := []any{string(DirectionIn), string(envelope.TypeAsk), fmtTime(since)}
	if from != "" {
		q += " AND from_user = ?"
		args = append(args, from)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// ExpireStale moves open outbound threads past their expiry to expired.
func (s *Store) ExpireStale(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE threads SET state = ?, updated_at = ? WHERE state IN (?, ?) AND expires_at < ?`,
		string(StateExpired), fmtTime(now), string(StateSent), string(StateDelivered), fmtTime(now))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Snoozed reports whether the thread is hidden from the inbox at now.
func (t Thread) Snoozed(now time.Time) bool {
	return !t.SnoozedUntil.IsZero() && now.Before(t.SnoozedUntil)
}

// Preview shortens a body to one line of at most n runes.
func Preview(body string, n int) string {
	line := strings.Join(strings.Fields(body), " ")
	r := []rune(line)
	if len(r) <= n {
		return line
	}
	return string(r[:n-1]) + "…"
}
