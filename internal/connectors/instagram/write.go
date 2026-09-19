package instagram

// Sending, with the safeguards underneath the agent rather than beside it.
//
// The previous arrangement put pacing, a ledger and a stop condition in prose
// and asked an agent to implement them per run. One did not: it dropped the
// personalisation, sent the same sentence to five people in half a minute, and
// the account was logged out on the first message. Nothing detected the
// missing safeguards, because a send loop without them looks exactly like one
// with them.
//
// So none of it is optional here. There is no argument that turns off pacing,
// no way to send without claiming the recipient first, and no path that
// continues after Instagram has said stop. An agent calling these decides WHO
// and WHAT; it does not get to decide whether the rules apply.

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// Ledger is the part of the store this connector needs.
//
// Narrow on purpose: a connector that could reach the whole store would be one
// edit away from reading things that are none of its business, and this way it
// can be faked in a test without a database.
type Ledger interface {
	ClaimOutreach(campaign, channel, target string) (bool, error)
	SettleOutreach(campaign, target, state, detail string) error
	CountOutreach(campaign string) (int, error)
	CampaignStopped(campaign string) (bool, string, error)
}

// Ledger states, mirrored from the store so this package does not import it.
const (
	stateSent    = "sent"
	stateFailed  = "failed"
	stateStopped = "stopped"
)

const (
	// pacing bounds the gap before each send. Randomised because a fixed
	// interval is itself a signature, and applied BEFORE the call so it cannot
	// be skipped by a caller that stops reading results.
	pacingMin = 5 * time.Second
	pacingMax = 10 * time.Second

	// defaultCap is how many people one campaign may reach before a human
	// raises it. Small deliberately: the one run that got blocked did five in
	// thirty seconds, and the first send of a campaign is the riskiest moment
	// it has.
	defaultCap = 25
)

// SetLedger gives this connector somewhere durable to record who has been
// contacted. Without it the write tools refuse: an unrecorded send is one that
// can be sent again by the next run, and sending twice is the failure this is
// all here to prevent.
func (c *Connector) SetLedger(l Ledger) { c.ledger = l }

// guard runs the checks that must happen before any send, in the order that
// matters. Returns a reason when the send must not happen.
func (c *Connector) guard(campaign, target, channel string) (skip string, err error) {
	if c.ledger == nil {
		return "", fmt.Errorf("instagram: sending is not available — this build has no ledger to " +
			"record who has been contacted, and sending without one risks contacting people twice")
	}
	if strings.TrimSpace(campaign) == "" {
		return "", fmt.Errorf("instagram: every send needs a campaign, so the ledger and the cap " +
			"have something to count against")
	}

	// First, because it is about the account rather than this recipient, and
	// because it has to survive the process that saw it.
	stopped, why, err := c.ledger.CampaignStopped(campaign)
	if err != nil {
		return "", err
	}
	if stopped {
		return "", fmt.Errorf("instagram: this campaign was stopped by Instagram (%s). "+
			"It does not resume — the account was refused, and continuing is what turns a "+
			"warning into a ban. Start a new campaign only after checking the account is healthy", why)
	}

	n, err := c.ledger.CountOutreach(campaign)
	if err != nil {
		return "", err
	}
	if n >= c.cap() {
		return "", fmt.Errorf("instagram: this campaign has reached its cap of %d. That is a "+
			"pause, not a failure — raise KARMAX_INSTAGRAM_CAP deliberately if more is intended", c.cap())
	}

	// The claim is the lock. If somebody else got here first, this is a
	// duplicate and the right answer is to do nothing at all.
	claimed, err := c.ledger.ClaimOutreach(campaign, channel, target)
	if err != nil {
		return "", err
	}
	if !claimed {
		return "already contacted in this campaign", nil
	}
	return "", nil
}

// settle records how a send turned out, and translates a hard stop into a
// campaign-wide halt.
func (c *Connector) settle(campaign, target string, callErr error) error {
	if callErr == nil {
		_ = c.ledger.SettleOutreach(campaign, target, stateSent, "")
		return nil
	}
	var he *Error
	if errors.As(callErr, &he) && he.HardStop {
		// Written against this recipient, but read for the whole campaign:
		// Instagram is refusing the account, not this one message.
		_ = c.ledger.SettleOutreach(campaign, target, stateStopped, he.Type)
		return fmt.Errorf("instagram: %s — Instagram refused this account, so the campaign is "+
			"stopped. Do not retry: continuing past this is what gets an account restricted. "+
			"Tell the operator, and note that if this was LoginRequired they are signed out "+
			"in the browser too", he.Type)
	}
	_ = c.ledger.SettleOutreach(campaign, target, stateFailed, callErr.Error())
	return callErr
}

// pace waits before a send.
//
// Before rather than after, so a caller that abandons the result still paid the
// gap, and so the very first send of a run is paced like every other one.
func pace(ctx context.Context) error {
	d := pacingMin + time.Duration(rand.Int63n(int64(pacingMax-pacingMin)+1))
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func (c *Connector) sendDM(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	campaign := str(in["campaign"])
	target := str(in["user_id"])
	text := strings.TrimSpace(str(in["text"]))
	if target == "" || text == "" {
		return nil, fmt.Errorf("instagram: a direct message needs a user_id and text")
	}

	skip, err := c.guard(campaign, target, "instagram.dm")
	if err != nil {
		return nil, err
	}
	if skip != "" {
		return map[string]any{"sent": false, "skipped": true, "reason": skip}, nil
	}

	if _, err := c.ensure(ctx, cr); err != nil {
		_ = c.ledger.SettleOutreach(campaign, target, stateFailed, err.Error())
		return nil, err
	}
	if err := pace(ctx); err != nil {
		_ = c.ledger.SettleOutreach(campaign, target, stateFailed, "cancelled before sending")
		return nil, err
	}

	c.mu.Lock()
	callErr := c.h.call(ctx, "send_dm", map[string]any{"user_id": target, "text": text}, nil)
	c.mu.Unlock()

	if err := c.settle(campaign, target, callErr); err != nil {
		return nil, err
	}
	return map[string]any{"sent": true, "user_id": target}, nil
}

func (c *Connector) replyComment(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	campaign := str(in["campaign"])
	mediaID := str(in["media_id"])
	commentID := str(in["comment_id"])
	text := strings.TrimSpace(str(in["text"]))
	if mediaID == "" || text == "" {
		return nil, fmt.Errorf("instagram: a reply needs a media_id and text")
	}
	// Keyed by comment when replying to one, by post otherwise: a person who
	// commented three times can be answered three times, which is the opposite
	// of the DM rule and is why the two ledgers never share a key.
	target := commentID
	if target == "" {
		target = "media:" + mediaID
	}

	skip, err := c.guard(campaign, target, "instagram.comment")
	if err != nil {
		return nil, err
	}
	if skip != "" {
		return map[string]any{"posted": false, "skipped": true, "reason": skip}, nil
	}

	if _, err := c.ensure(ctx, cr); err != nil {
		_ = c.ledger.SettleOutreach(campaign, target, stateFailed, err.Error())
		return nil, err
	}
	if err := pace(ctx); err != nil {
		_ = c.ledger.SettleOutreach(campaign, target, stateFailed, "cancelled before sending")
		return nil, err
	}

	args := map[string]any{"media_id": mediaID, "text": text}
	if commentID != "" {
		args["comment_id"] = commentID
	}
	var out any
	c.mu.Lock()
	callErr := c.h.call(ctx, "reply_comment", args, &out)
	c.mu.Unlock()

	if err := c.settle(campaign, target, callErr); err != nil {
		return nil, err
	}
	return out, nil
}

func str(v any) string {
	if v == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

// SendingEnabled reports whether this install has turned writing on.
//
// A second switch beyond KARMAX_ENABLE_INSTAGRAM, and off by default even when
// that one is on. Reading someone's inbox and messaging their followers are
// different decisions with different consequences, and the connector has
// always refused the second — this keeps that refusal as the default while
// making it possible to opt in deliberately.
func SendingEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("KARMAX_INSTAGRAM_SEND")), "true")
}

// cap is the per-campaign ceiling, raisable but not removable.
func (c *Connector) cap() int {
	if v := strings.TrimSpace(os.Getenv("KARMAX_INSTAGRAM_CAP")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			// Still bounded. An operator who wants a thousand is asking for the
			// account, and a cap that can be set to infinity is not a cap.
			if n > 200 {
				return 200
			}
			return n
		}
	}
	return defaultCap
}
