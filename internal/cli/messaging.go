package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yjmrobert/wormhole/internal/bus"
	"github.com/yjmrobert/wormhole/internal/config"
	"github.com/yjmrobert/wormhole/internal/envelope"
	"github.com/yjmrobert/wormhole/internal/threads"
)

// SessionEnv is the variable the Copilot agent's shell exposes.
const SessionEnv = "COPILOT_AGENT_SESSION_ID"

// SkillVersion is the default for --skill-version and must equal the
// version in skills/wormhole/SKILL.md; a test keeps them in step. Bump both
// together whenever the skill's CLI contract changes.
const SkillVersion = "0.4.0"

const (
	forceUnsafeHelp  = "send even if the body looks like it contains secrets"
	skillVersionHelp = "version of the skill producing this message"
	yesHelp          = "skip the confirmation (sets human_reviewed: false)"
)

// sendResult is the JSON shape of ask, reply and send.
type sendResult struct {
	OK            bool   `json:"ok"`
	ThreadID      string `json:"thread_id"`
	MessageID     string `json:"message_id"`
	Type          string `json:"type"`
	To            string `json:"to"`
	State         string `json:"state"`
	Hop           int    `json:"hop"`
	HumanReviewed bool   `json:"human_reviewed"`
	ExpiresAt     string `json:"expires_at"`
	PeerOnline    *bool  `json:"peer_online,omitempty"`
}

// me constructs the local peer identity for an outbound envelope.
func (a *App) me(cfg *config.Config) envelope.Peer {
	return envelope.Peer{User: cfg.Handle, Host: a.Hostname, SessionID: a.Getenv(SessionEnv)}
}

// guardBody runs the secret scan and the size check shared by every
// outbound command.
func (a *App) guardBody(body string, forceUnsafe bool) error {
	if strings.TrimSpace(body) == "" {
		return fail(ExitUsage, "empty body")
	}
	if len(body) > envelope.MaxBodyBytes {
		return fail(ExitRefused, "body is %d bytes; the cap is %d", len(body), envelope.MaxBodyBytes)
	}
	if hits := envelope.ScanSecrets(body); len(hits) > 0 {
		if !forceUnsafe {
			return fail(ExitRefused, "body looks like it contains secrets; remove them or pass --force-unsafe:\n%s", strings.TrimRight(envelope.DescribeMatches(hits), "\n"))
		}
		fmt.Fprintf(a.Stderr, "warning: sending despite %d secret-shaped match(es) (--force-unsafe)\n", len(hits))
	}
	return nil
}

// review shows the envelope and asks for confirmation unless --yes. It
// returns the human_reviewed value for the envelope.
func (a *App) review(c *common, e envelope.Envelope, yes bool) (bool, error) {
	if yes {
		return false, nil
	}
	if c.noInput {
		return false, fail(ExitRefused, noInputReviewMessage)
	}
	if !a.IsTerminal() {
		return false, fail(ExitRefused, "not a terminal: show the message to the person, then rerun with --yes")
	}
	pretty, _ := json.MarshalIndent(e, "", "  ")
	fmt.Fprintf(a.Stdout, "%s\n", pretty)
	ok, err := a.confirm(fmt.Sprintf("Send this %s to ^%s?", e.Type, e.To.User))
	if err != nil {
		return false, err
	}
	if !ok {
		return false, fail(ExitRefused, "not sent")
	}
	return true, nil
}

// publish connects, publishes, records the outbound message and prints.
func (a *App) publish(ctx context.Context, c *common, cfg *config.Config, store *threads.Store, e envelope.Envelope, t threads.Thread) error {
	if err := e.Validate(a.Now()); err != nil {
		return fail(ExitUsage, "envelope: %v", err)
	}
	b, err := a.NewBus(ctx, cfg)
	if err != nil {
		return fail(ExitUnavailable, busErrorFormat, err)
	}
	defer b.Close()
	var online *bool
	if list, err := b.Registry().List(ctx); err == nil {
		seen := false
		for _, p := range list {
			if p.Handle == e.To.User {
				seen = true
				v := a.Now().Sub(p.LastSeen) < 2*cfg.Daemon.RegistryInterval
				online = &v
			}
		}
		if !seen {
			fmt.Fprintf(a.Stderr, "warning: ^%s has never registered on this bus; the message will wait for their daemon\n", e.To.User)
		}
	}
	if err := b.Publish(ctx, bus.SubjectFor(e), e); err != nil {
		if errors.Is(err, bus.ErrPermission) {
			return fail(ExitRefused, "%v", err)
		}
		return fail(ExitUnavailable, "publish: %v", err)
	}
	if err := store.Put(ctx, t); err != nil {
		return err
	}
	if err := store.AddMessage(ctx, e, threads.DirectionOut); err != nil {
		return err
	}
	res := sendResult{OK: true, ThreadID: e.ThreadID, MessageID: e.ID, Type: string(e.Type), To: e.To.User, State: string(t.State), Hop: e.Hop, HumanReviewed: e.HumanReviewed, ExpiresAt: e.ExpiresAt.Format("2006-01-02T15:04:05Z07:00"), PeerOnline: online}
	a.emit(c, res, func(w io.Writer) {
		fmt.Fprintf(w, "%s sent to ^%s on thread %s\n", e.Type, e.To.User, e.ThreadID)
		if e.Type == envelope.TypeAsk {
			fmt.Fprintf(w, "The reply will arrive as a new session; track it with: wormhole threads\n")
		}
	})
	return nil
}

// newAskCommand constructs the command that opens a thread.
func (a *App) newAskCommand(c *common) *cobra.Command {
	var repo, ref, skillV, session string
	var yes, force bool
	cmd := command("ask <user> <prompt>", "Open a thread with a colleague")
	cmd.Flags().StringVar(&repo, "repo", "", "owner/name the question is about")
	cmd.Flags().StringVar(&ref, "context", "", "short pointer such as 'PR #123'")
	cmd.Flags().BoolVar(&yes, "yes", false, yesHelp)
	cmd.Flags().BoolVar(&force, "force-unsafe", false, forceUnsafeHelp)
	cmd.Flags().StringVar(&skillV, "skill-version", SkillVersion, skillVersionHelp)
	cmd.Flags().StringVar(&session, "session", "", "id of the Copilot session asking (default: $"+SessionEnv+"); the reply is appended to it")
	cmd.Args = exactArgs(2, `wormhole ask <user> "<prompt>"`)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		to := args[0]
		if !envelope.ValidHandle(to) {
			return fail(ExitNotFound, invalidHandleFormat, to)
		}
		cfg, err := a.loadConfig(c)
		if err != nil {
			return err
		}
		if to == cfg.Handle {
			return fail(ExitUsage, "you cannot ask yourself")
		}
		body, err := a.readText(args[1])
		if err != nil {
			return err
		}
		if err := a.guardBody(body, force); err != nil {
			return err
		}
		from := a.me(cfg)
		if session != "" {
			from.SessionID = session
		}
		e := envelope.NewAsk(from, to, body, a.Now())
		e.Context = envelope.Context{Repo: repo, Ref: ref}
		e.SkillV = skillV
		reviewed, err := a.review(c, e, yes)
		if err != nil {
			return err
		}
		e.HumanReviewed = reviewed
		store, err := a.OpenStore()
		if err != nil {
			return err
		}
		defer store.Close()
		t := threads.Thread{ID: e.ThreadID, Peer: to, Direction: threads.DirectionOut, State: threads.StateSent, RootSessionID: e.RootSessionID, Title: threads.Preview(body, 60), Repo: repo, ExpiresAt: e.ExpiresAt, CreatedAt: e.CreatedAt}
		return a.publish(cmd.Context(), c, cfg, store, e, t)
	}
	return cmd
}

// followUp is shared by reply and send.
func (a *App) followUp(ctx context.Context, c *common, typ envelope.Type, threadRef, peer, text string, yes, force bool, skillV string) error {
	cfg, err := a.loadConfig(c)
	if err != nil {
		return err
	}
	body, err := a.readText(text)
	if err != nil {
		return err
	}
	if err := a.guardBody(body, force); err != nil {
		return err
	}
	store, err := a.OpenStore()
	if err != nil {
		return err
	}
	defer store.Close()
	t, err := a.resolveThread(ctx, store, threadRef)
	if err != nil {
		return err
	}
	threadID := t.ID
	if peer != "" && peer != t.Peer {
		return fail(ExitNotFound, "thread %s is with ^%s, not ^%s", threadID, t.Peer, peer)
	}
	switch t.State {
	case threads.StateClosed, threads.StateExpired, threads.StateRejected, threads.StateDeclined:
		return fail(ExitRefused, "thread %s is %s", threadID, t.State)
	}
	last, err := store.LastMessage(ctx, threadID)
	if err != nil {
		return fail(ExitError, "thread %s has no messages", threadID)
	}
	prev, err := last.Envelope()
	if err != nil {
		return err
	}
	if prev.Hop+1 > cfg.Limits.MaxHops {
		return fail(ExitRefused, "thread %s has reached max_hops (%d); start a new ask if it still matters", threadID, cfg.Limits.MaxHops)
	}
	e := envelope.NewFollowUp(typ, prev, a.me(cfg), t.Peer, body, a.Now())
	e.SkillV = skillV
	reviewed, err := a.review(c, e, yes)
	if err != nil {
		return err
	}
	e.HumanReviewed = reviewed
	if typ == envelope.TypeReply {
		t.State = threads.StateReplied
	}
	t.ExpiresAt = e.ExpiresAt
	return a.publish(ctx, c, cfg, store, e, t)
}

// newReplyCommand constructs the command that replies to an existing thread.
func (a *App) newReplyCommand(c *common) *cobra.Command {
	var yes, force bool
	var skillV string
	cmd := command("reply <thread> <text>", "Reply on a thread")
	cmd.Flags().BoolVar(&yes, "yes", false, yesHelp)
	cmd.Flags().BoolVar(&force, "force-unsafe", false, forceUnsafeHelp)
	cmd.Flags().StringVar(&skillV, "skill-version", SkillVersion, skillVersionHelp)
	cmd.Args = exactArgs(2, `wormhole reply <thread> "<text>"`)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		return a.followUp(cmd.Context(), c, envelope.TypeReply, args[0], "", args[1], yes, force, skillV)
	}
	return cmd
}

// newSendCommand constructs the command that sends a follow-up to a peer.
func (a *App) newSendCommand(c *common) *cobra.Command {
	var thread, skillV string
	var yes, force bool
	cmd := command("send <user> <text>", "Send a follow-up on an open thread")
	cmd.Flags().StringVar(&thread, "thread", "", "open thread to send on: "+threadHelp)
	cmd.Flags().BoolVar(&yes, "yes", false, yesHelp)
	cmd.Flags().BoolVar(&force, "force-unsafe", false, forceUnsafeHelp)
	cmd.Flags().StringVar(&skillV, "skill-version", SkillVersion, skillVersionHelp)
	cmd.Args = exactArgs(2, `wormhole send <user> --thread <thread> "<text>"`)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if thread == "" {
			return fail(ExitUsage, `usage: wormhole send <user> --thread <thread> "<text>"`)
		}
		return a.followUp(cmd.Context(), c, envelope.TypeMessage, thread, args[0], args[1], yes, force, skillV)
	}
	return cmd
}
