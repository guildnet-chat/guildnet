// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/guildnet-chat/guildnet/internal/org"
)

// Two-step sign-in is the console's: a password, then a code or a passkey.
// Plain IRC signs in with the password alone, and every session of an
// account looks the same to Ergo (and so to GuildServ), whichever way it
// signed in. So for those who sign in with two steps, or must, GuildServ
// takes changes to the org (and the ledger !pause and !resume) only through
// the console, which has seen both steps (POST /v1/guildserv, from its web
// chat); over IRC they can still look.

// TwoStep is how someone signs in to the console: whether the network wants
// two steps of those who may change the whole org, and whether they have
// them on.
type TwoStep struct {
	Required bool `json:"required"`
	On       bool `json:"on"`
}

// TwoSteps says how people sign in to the console (the platform's broker).
type TwoSteps interface {
	TwoStepOf(ctx context.Context, account string) (TwoStep, error)
}

// BrokerTwoSteps asks the platform's broker how people sign in, as the
// network, with its secret.
type BrokerTwoSteps struct {
	URL, Network, Secret string
	HTTP                 *http.Client // nil means a plain client
}

func (b BrokerTwoSteps) TwoStepOf(ctx context.Context, account string) (TwoStep, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(b.URL, "/")+"/two-step?account="+url.QueryEscape(account), nil)
	if err != nil {
		return TwoStep{}, err
	}
	req.Header.Set("GuildNet-Network", b.Network)
	req.Header.Set("Authorization", "Bearer "+b.Secret)
	client := b.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return TwoStep{}, fmt.Errorf("asking the broker: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return TwoStep{}, fmt.Errorf("the broker didn't say: %s", resp.Status)
	}
	var t TwoStep
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<12)).Decode(&t); err != nil {
		return TwoStep{}, fmt.Errorf("the broker's answer: %w", err)
	}
	return t, nil
}

// UseTwoSteps has GuildServ hold those who sign in with two steps to the
// console for changes. Without it (no console), IRC changes things as ever.
func (r *Runner) UseTwoSteps(t TwoSteps) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.twoSteps = t
}

// mustUseTwoStep says whether the network, wanting two steps of them, does
// of actor: the owner, and anyone who may change things across the whole
// org, or in every team (leading the one all the others are inside, say).
// The console asks it here (/v1/me's must_two_step).
func (r *Runner) mustUseTwoStep(actor string) bool {
	if r.isOwner(actor) {
		return true
	}
	teams, err := r.store.Teams(context.Background())
	if err != nil {
		return true // can't tell: held to it, not let off
	}
	for _, ab := range []ability{canApprove, canHire, canBudget, canInvite, canArrange} {
		if r.can(actor, ab, wholeOrg) {
			return true
		}
		if len(teams) > 0 && !slices.ContainsFunc(teams, func(t org.Team) bool { return !r.can(actor, ab, inTeam(t.Name)) }) {
			return true
		}
	}
	return false
}

// notOverIRC says why actor can't change the org over plain IRC, if they
// can't: they sign in with two steps, or must, so changes come from the
// console. "" lets them.
func (r *Runner) notOverIRC(actor string) string {
	r.mu.Lock()
	ts := r.twoSteps
	r.mu.Unlock()
	if ts == nil || actor == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	t, err := ts.TwoStepOf(ctx, actor)
	if err != nil {
		r.logger.Printf("checking how %s signs in: %v", actor, err)
		return "couldn't check how you sign in just now, so nothing was changed: try again, or do it in the console's web chat."
	}
	if t.On || (t.Required && r.mustUseTwoStep(actor)) {
		return "you sign in to the console with two steps, so changes to the org come from there (its web chat, or its pages), where you've shown both: an IRC app signs in with the password alone. Nothing was changed; looking still works here."
	}
	return ""
}

// changesTheOrg says whether a GuildServ command changes something, rather
// than only looking.
func changesTheOrg(cmd string, args []string) bool {
	switch cmd {
	case "APPROVE", "DENY", "HIRE", "FIRE", "SET", "TEAM", "MEMBER", "PROMOTE", "DEMOTE", "GOAL":
		return true
	case "ACCESS", "COMPANY", "APPROVALS":
		// ACCESS, COMPANY and APPROVALS alone look; GIVE, DESCRIBE, AFTER
		// and the like change.
		return len(args) > 0
	}
	return false
}

// apiGuildServ runs a GuildServ command from the console (its web chat), as
// the person signed in there, with their second step shown: answered as
// GuildServ always answers, over IRC.
func (r *Runner) apiGuildServ(w http.ResponseWriter, req *http.Request) {
	var in struct {
		Text string `json:"text"`
	}
	if !decode(w, req, &in) {
		return
	}
	text := strings.TrimSpace(in.Text)
	if text == "" || strings.ContainsAny(text, "\r\n\x00") {
		apiError(w, http.StatusBadRequest, "send one GuildServ command, e.g. HELP")
		return
	}
	actor := actorOf(req)
	go r.guildCommand(actor, actor, text, true)
	w.WriteHeader(http.StatusAccepted)
}
