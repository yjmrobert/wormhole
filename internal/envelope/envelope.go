// Package envelope defines the transport-independent Wormhole message
// envelope (v1), its validation rules and the identifiers it carries.
//
// Every bus adapter carries an Envelope as an opaque JSON body and reads
// only the routing fields it needs. Nothing in this package knows about
// NATS, SQLite or the Copilot app.
package envelope

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/oklog/ulid/v2"
)

// Version is the envelope schema version this package produces and accepts.
const Version = 1

// Type is the kind of message an envelope carries.
type Type string

const (
	// TypeAsk opens a thread: a question for the recipient's agent or human.
	TypeAsk Type = "ask"
	// TypeReply answers an ask (or an earlier reply) on an open thread.
	TypeReply Type = "reply"
	// TypeMessage is a free-form follow-up on an open thread.
	TypeMessage Type = "message"
	// TypeAck is published by the recipient's daemon once an inbound message
	// has been materialized (or rejected). Nothing is auto-opened on an ack.
	TypeAck Type = "ack"
)

// Limits from the design: a body is at most 32 KB of UTF-8, threads die
// after MaxHops hops, and a message is dropped once it passes ExpiresAt.
const (
	MaxBodyBytes      = 32 * 1024
	DefaultMaxHops    = 6
	DefaultTTL        = 7 * 24 * time.Hour
	MaxHandleLength   = 64
	MaxHostLength     = 253
	MaxContextRefSize = 512
)

// handleRE is the shape of a handle: lowercase letters, digits, dot, dash
// and underscore, starting with a letter or digit. Handles double as NATS
// subject tokens, so they must never contain '.', '*', '>' or whitespace.
var handleRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Peer identifies one end of a thread.
type Peer struct {
	User      string `json:"user"`
	Host      string `json:"host,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

// Context is the optional pointer to what the message is about.
type Context struct {
	Repo    string  `json:"repo,omitempty"`
	Ref     string  `json:"ref,omitempty"`
	CwdHint *string `json:"cwd_hint"`
}

// Envelope is the v1 wire format.
type Envelope struct {
	V             int       `json:"v"`
	ID            string    `json:"id"`
	Type          Type      `json:"type"`
	ThreadID      string    `json:"thread_id"`
	From          Peer      `json:"from"`
	To            Peer      `json:"to"`
	InReplyTo     *string   `json:"in_reply_to"`
	RootSessionID string    `json:"root_session_id,omitempty"`
	Hop           int       `json:"hop"`
	HumanReviewed bool      `json:"human_reviewed"`
	SkillV        string    `json:"skill_v,omitempty"`
	Body          string    `json:"body"`
	Context       Context   `json:"context"`
	CreatedAt     time.Time `json:"created_at"`
	ExpiresAt     time.Time `json:"expires_at"`
}

// NewID returns a new ULID string. ULIDs sort by creation time, which keeps
// thread listings in order without a second column.
func NewID() string {
	return ulid.MustNew(ulid.Timestamp(time.Now()), rand.Reader).String()
}

// ValidID reports whether s parses as a ULID.
func ValidID(s string) bool {
	if len(s) != ulid.EncodedSize {
		return false
	}
	_, err := ulid.ParseStrict(s)
	return err == nil
}

// ValidHandle reports whether s is an acceptable user handle.
func ValidHandle(s string) bool {
	return handleRE.MatchString(s)
}

// NewAsk builds a fresh ask envelope. The thread ID equals the ask's own ID.
func NewAsk(from Peer, to string, body string, now time.Time) Envelope {
	id := NewID()
	return Envelope{
		V:             Version,
		ID:            id,
		Type:          TypeAsk,
		ThreadID:      id,
		From:          from,
		To:            Peer{User: to},
		RootSessionID: from.SessionID,
		Hop:           1,
		Body:          body,
		CreatedAt:     now.UTC().Truncate(time.Second),
		ExpiresAt:     now.UTC().Add(DefaultTTL).Truncate(time.Second),
	}
}

// NewFollowUp builds a reply or message on an existing thread. prev is the
// last message of the thread the sender has seen; hop is prev.Hop+1 so that
// agent ping-pong terminates at MaxHops.
func NewFollowUp(t Type, prev Envelope, from Peer, to string, body string, now time.Time) Envelope {
	prevID := prev.ID
	return Envelope{
		V:             Version,
		ID:            NewID(),
		Type:          t,
		ThreadID:      prev.ThreadID,
		From:          from,
		To:            Peer{User: to},
		InReplyTo:     &prevID,
		RootSessionID: prev.RootSessionID,
		Hop:           prev.Hop + 1,
		Body:          body,
		Context:       prev.Context,
		CreatedAt:     now.UTC().Truncate(time.Second),
		ExpiresAt:     now.UTC().Add(DefaultTTL).Truncate(time.Second),
	}
}

// NewAck builds the delivery receipt for an inbound message. status is
// "delivered" or "rejected: <reason>".
func NewAck(inbound Envelope, from Peer, status string, now time.Time) Envelope {
	id := inbound.ID
	return Envelope{
		V:             Version,
		ID:            NewID(),
		Type:          TypeAck,
		ThreadID:      inbound.ThreadID,
		From:          from,
		To:            Peer{User: inbound.From.User},
		InReplyTo:     &id,
		RootSessionID: inbound.RootSessionID,
		Hop:           inbound.Hop,
		Body:          status,
		CreatedAt:     now.UTC().Truncate(time.Second),
		ExpiresAt:     now.UTC().Add(DefaultTTL).Truncate(time.Second),
	}
}

// Rejected reports whether an ack carries a rejection and, if so, why.
func (e Envelope) Rejected() (reason string, ok bool) {
	if e.Type != TypeAck {
		return "", false
	}
	if rest, found := strings.CutPrefix(e.Body, "rejected:"); found {
		return strings.TrimSpace(rest), true
	}
	return "", false
}

// Validation errors. Callers compare with errors.Is.
var (
	ErrVersion   = errors.New("unsupported envelope version")
	ErrID        = errors.New("invalid message id")
	ErrType      = errors.New("invalid message type")
	ErrThread    = errors.New("invalid thread id")
	ErrHandle    = errors.New("invalid handle")
	ErrHop       = errors.New("hop out of range")
	ErrBody      = errors.New("invalid body")
	ErrTime      = errors.New("invalid timestamps")
	ErrExpired   = errors.New("message expired")
	ErrInReplyTo = errors.New("invalid in_reply_to")
)

// Validate checks the structural rules that every envelope must satisfy,
// independent of who receives it. Policy (allowlists, rate caps, max hops)
// lives in the daemon; expiry is checked here against now.
func (e Envelope) Validate(now time.Time) error {
	if e.V != Version {
		return fmt.Errorf("%w: %d", ErrVersion, e.V)
	}
	if !ValidID(e.ID) {
		return ErrID
	}
	switch e.Type {
	case TypeAsk, TypeReply, TypeMessage, TypeAck:
	default:
		return fmt.Errorf("%w: %q", ErrType, e.Type)
	}
	if !ValidID(e.ThreadID) {
		return ErrThread
	}
	if e.Type == TypeAsk && e.ThreadID != e.ID {
		return fmt.Errorf("%w: an ask's thread_id must equal its id", ErrThread)
	}
	if e.Type != TypeAsk {
		if e.InReplyTo == nil || !ValidID(*e.InReplyTo) {
			return ErrInReplyTo
		}
	}
	if !ValidHandle(e.From.User) || !ValidHandle(e.To.User) {
		return ErrHandle
	}
	if len(e.From.Host) > MaxHostLength {
		return fmt.Errorf("%w: host too long", ErrHandle)
	}
	if e.Hop < 1 || e.Hop > 1000 {
		return fmt.Errorf("%w: %d", ErrHop, e.Hop)
	}
	if len(e.Body) > MaxBodyBytes {
		return fmt.Errorf("%w: %d bytes exceeds %d", ErrBody, len(e.Body), MaxBodyBytes)
	}
	if !utf8.ValidString(e.Body) {
		return fmt.Errorf("%w: not valid UTF-8", ErrBody)
	}
	if e.Type != TypeAck && strings.TrimSpace(e.Body) == "" {
		return fmt.Errorf("%w: empty", ErrBody)
	}
	if len(e.Context.Repo) > MaxContextRefSize || len(e.Context.Ref) > MaxContextRefSize {
		return fmt.Errorf("%w: context field too long", ErrBody)
	}
	if e.CreatedAt.IsZero() || e.ExpiresAt.IsZero() || !e.ExpiresAt.After(e.CreatedAt) {
		return ErrTime
	}
	if !now.Before(e.ExpiresAt) {
		return ErrExpired
	}
	return nil
}

// Marshal encodes the envelope as compact JSON.
func Marshal(e Envelope) ([]byte, error) {
	return json.Marshal(e)
}

// Unmarshal decodes JSON into an envelope. It rejects unknown fields so a
// newer schema cannot be silently misread by an older daemon.
func Unmarshal(b []byte) (Envelope, error) {
	var e Envelope
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		return Envelope{}, fmt.Errorf("decode envelope: %w", err)
	}
	return e, nil
}
