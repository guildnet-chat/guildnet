// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/guildnet-chat/guildnet/monitor"
)

// Two-step sign-in on the home page. Google or an emailed link proves only
// a mailbox or a Google account; from here owners reset their consoles'
// passwords and two-step, and admins run GuildNet. So a code from an
// authenticator app (or a recovery code) is asked for after either. Where
// the platform requires it, everyone sets the app up before anything else.

// HomeTwoSteps keeps people's two-step sign-in to the home page, by person.
// Passkeys aren't kept here (yet).
type HomeTwoSteps interface {
	HomeTwoStep(ctx context.Context, person string) (TwoStepSetup, error) // zero if off
	SetHomeTwoStep(ctx context.Context, person string, t TwoStepSetup) error
	// UseHomeStep and UseHomeRecovery are TwoSteps' UseStep and UseRecovery.
	UseHomeStep(ctx context.Context, person string, step int64) (bool, error)
	UseHomeRecovery(ctx context.Context, person, hash string) (left int, ok bool, err error)
}

// homeTwoStepOn says whether person has the app set up.
func (p *Platform) homeTwoStepOn(ctx context.Context, person Person) (bool, error) {
	if p.opts.HomeTwoSteps == nil {
		return false, nil
	}
	t, err := p.opts.HomeTwoSteps.HomeTwoStep(ctx, person.ID)
	return t.Secret != "", err
}

// homeSetUpFirst says whether person must set the app up before anything
// else.
func (p *Platform) homeSetUpFirst(ctx context.Context, person Person) bool {
	if !p.opts.RequireHomeTwoStep || p.opts.HomeTwoSteps == nil {
		return false
	}
	on, err := p.homeTwoStepOn(ctx, person)
	return err == nil && !on
}

// homeAccount is a pretend network for the sign-ins waiting for their
// second step on the home page (their account is the person's ID).
var homeAccount = Network{}

// signInHome signs person in on the home page once they've shown their
// mailbox or Google account, and sends them on to to: or first to their
// second step, if they have the app; or to set it up, if they must.
func (p *Platform) signInHome(w http.ResponseWriter, r *http.Request, person Person, to string) error {
	on, err := p.homeTwoStepOn(r.Context(), person)
	if err != nil {
		return err
	}
	if on {
		p.awaitStep(w, homeAccount, person.ID)
		http.Redirect(w, r, "/signin/two-step", http.StatusSeeOther)
		return nil
	}
	if err := p.startHomeSession(w, r, person); err != nil {
		return err
	}
	if p.homeSetUpFirst(r.Context(), person) {
		to = "/account/two-step"
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
	return nil
}

func (p *Platform) homeStepPage(w http.ResponseWriter, r *http.Request) {
	if _, x := p.pending(r, homeAccount); x == nil {
		p.renderHome(w, r, http.StatusBadRequest, "home-signin", withError{Err: "That sign-in has expired. Sign in again.", View: p.homeSigninView()})
		return
	}
	p.renderHome(w, r, http.StatusOK, "home-signin-two-step", nil)
}

func (p *Platform) homeStepDone(w http.ResponseWriter, r *http.Request) {
	hash, x := p.pending(r, homeAccount)
	if x == nil {
		p.renderHome(w, r, http.StatusBadRequest, "home-signin", withError{Err: "That sign-in has expired. Sign in again.", View: p.homeSigninView()})
		return
	}
	key := "home|" + x.account
	now := p.opts.Now()
	if !p.tryStep(x) {
		p.forgetPending(w, hash)
		p.renderHome(w, r, http.StatusTooManyRequests, "home-signin", withError{Err: "Too many wrong codes. Sign in again.", View: p.homeSigninView()})
		return
	}
	if !p.codeAttempts.Try(key, now) {
		p.forgetPending(w, hash)
		p.renderHome(w, r, http.StatusTooManyRequests, "home-signin", withError{Err: "Too many wrong codes. Wait 15 minutes, then sign in again.", View: p.homeSigninView()})
		return
	}
	ok, err := p.homeSecondStep(r.Context(), x.account, r.PostFormValue("code"))
	if err != nil {
		monitor.Problem(p.opts.Logger, "checking a home second step: %v", err)
		p.renderHome(w, r, http.StatusInternalServerError, "home-signin-two-step", withError{Err: "Couldn't check it just now. Try again in a moment."})
		return
	}
	if !ok {
		p.steps.mu.Lock()
		wrong := x.wrong
		p.steps.mu.Unlock()
		if wrong >= stepAttempts {
			p.forgetPending(w, hash)
			p.renderHome(w, r, http.StatusUnauthorized, "home-signin", withError{Err: "Too many wrong codes. Sign in again.", View: p.homeSigninView()})
			return
		}
		p.renderHome(w, r, http.StatusUnauthorized, "home-signin-two-step", withError{Err: "That code didn't match. Try the newest one your app shows."})
		return
	}
	p.forgetPending(w, hash)
	p.codeAttempts.Reset(key)
	person, err := p.opts.People.Person(r.Context(), x.account)
	if err == nil && person.Blocked {
		p.renderHome(w, r, http.StatusForbidden, "home-signin", withError{Err: p.blockedMessage(), View: p.homeSigninView()})
		return
	}
	if err == nil {
		err = p.startHomeSession(w, r, person)
	}
	if err != nil {
		monitor.Problem(p.opts.Logger, "signing in after the second step: %v", err)
		p.renderHome(w, r, http.StatusInternalServerError, "home-signin", withError{Err: "Couldn't sign you in. Try again in a moment.", View: p.homeSigninView()})
		return
	}
	http.Redirect(w, r, "/networks", http.StatusSeeOther)
}

// homeSecondStep checks a code for person's second step: the app's (once),
// or a recovery code (once).
func (p *Platform) homeSecondStep(ctx context.Context, person, code string) (bool, error) {
	t, err := p.opts.HomeTwoSteps.HomeTwoStep(ctx, person)
	if err != nil || t.Secret == "" {
		return false, err
	}
	code = normalCode(code)
	if len(code) == 6 {
		step, ok := totpStep(t.Secret, code, p.opts.Now())
		if !ok {
			return false, nil
		}
		return p.opts.HomeTwoSteps.UseHomeStep(ctx, person, step)
	}
	if len(code) != 10 {
		return false, nil
	}
	left, ok, err := p.opts.HomeTwoSteps.UseHomeRecovery(ctx, person, hashToken(code))
	if ok {
		p.opts.Logger.Printf("person %s used a home recovery code; %d left", person, left)
	}
	return ok, err
}

// Setting it up.

type homeTwoStepView struct {
	Secret   string
	Link, QR template.URL // made here, from a secret made here
	Must     bool
	Codes    []string // shown once, when it's just been set up
}

func (p *Platform) homeTwoStepView(r *http.Request, secret string) homeTwoStepView {
	s := homeOf(r)
	link := p.totpLink(p.opts.BaseDomain, s.person.Email, secret)
	return homeTwoStepView{Secret: secret, Link: template.URL(link), QR: template.URL(qrImage(link)), Must: p.homeSetUpFirst(r.Context(), s.person)}
}

func (p *Platform) homeTwoStepPage(w http.ResponseWriter, r *http.Request) {
	if on, err := p.homeTwoStepOn(r.Context(), homeOf(r).person); err == nil && on {
		http.Redirect(w, r, "/networks", http.StatusSeeOther)
		return
	}
	p.renderHome(w, r, http.StatusOK, "home-two-step", withError{View: p.homeTwoStepView(r, newTOTPSecret())})
}

func (p *Platform) setUpHomeTwoStep(w http.ResponseWriter, r *http.Request) {
	s := homeOf(r)
	ctx := r.Context()
	if on, err := p.homeTwoStepOn(ctx, s.person); err != nil || on {
		http.Redirect(w, r, "/networks", http.StatusSeeOther)
		return
	}
	secret := strings.ToUpper(strings.TrimSpace(r.PostFormValue("secret")))
	if key, err := b32.DecodeString(secret); err != nil || len(key) != 20 {
		p.renderHome(w, r, http.StatusBadRequest, "home-two-step", withError{Err: "That setup has gone stale. Start again.", View: p.homeTwoStepView(r, newTOTPSecret())})
		return
	}
	step, ok := totpStep(secret, normalCode(r.PostFormValue("code")), p.opts.Now())
	if !ok {
		p.renderHome(w, r, http.StatusBadRequest, "home-two-step", withError{Err: "That code didn't match. Check the app added this account, then enter the code it shows now.", View: p.homeTwoStepView(r, secret)})
		return
	}
	codes, hashes := newRecoveryCodes()
	if err := p.opts.HomeTwoSteps.SetHomeTwoStep(ctx, s.person.ID, TwoStepSetup{Secret: secret, LastStep: step, Recovery: hashes, Since: p.opts.Now()}); err != nil {
		monitor.Problem(p.opts.Logger, "setting up %s's home two-step: %v", s.person.ID, err)
		p.renderHome(w, r, http.StatusInternalServerError, "home-two-step", withError{Err: "Couldn't save it. Try again in a moment.", View: p.homeTwoStepView(r, secret)})
		return
	}
	// Any other sign-in skipped the step it now needs.
	if err := p.opts.Sessions.DeleteAccount(ctx, "", s.person.ID, s.hash); err != nil {
		monitor.Problem(p.opts.Logger, "signing %s out elsewhere: %v", s.person.ID, err)
	}
	p.opts.Logger.Printf("person %s turned on home two-step sign-in", s.person.ID)
	p.renderHome(w, r, http.StatusOK, "home-two-step", withError{View: homeTwoStepView{Codes: codes}})
}

// In memory.

func (s *MemoryStore) HomeTwoStep(_ context.Context, person string) (TwoStepSetup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.homeTwoStep[person]
	t.Recovery = slices.Clone(t.Recovery)
	return t, nil
}

func (s *MemoryStore) SetHomeTwoStep(_ context.Context, person string, t TwoStepSetup) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.homeTwoStep == nil {
		s.homeTwoStep = map[string]TwoStepSetup{}
	}
	t.Recovery, t.Passkeys = slices.Clone(t.Recovery), nil
	s.homeTwoStep[person] = t
	return nil
}

func (s *MemoryStore) UseHomeStep(_ context.Context, person string, step int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.homeTwoStep[person]
	if !ok || step <= t.LastStep {
		return false, nil
	}
	t.LastStep = step
	s.homeTwoStep[person] = t
	return true, nil
}

func (s *MemoryStore) UseHomeRecovery(_ context.Context, person, hash string) (int, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.homeTwoStep[person]
	i := slices.Index(t.Recovery, hash)
	if !ok || i < 0 {
		return len(t.Recovery), false, nil
	}
	t.Recovery = slices.Delete(slices.Clone(t.Recovery), i, i+1)
	s.homeTwoStep[person] = t
	return len(t.Recovery), true, nil
}

// In Postgres, the app's secret sealed.

func (s *Postgres) HomeTwoStep(ctx context.Context, person string) (TwoStepSetup, error) {
	var t TwoStepSetup
	var secret []byte
	err := s.pool.QueryRow(ctx, `select secret, last_step, recovery, since from home_two_step where person = $1`, person).
		Scan(&secret, &t.LastStep, &t.Recovery, &t.Since)
	if errors.Is(err, pgx.ErrNoRows) {
		return TwoStepSetup{}, nil
	}
	if err != nil {
		return TwoStepSetup{}, err
	}
	if t.Secret, err = s.openString(secret, homeTwoStepSecret(person)); err != nil {
		return TwoStepSetup{}, fmt.Errorf("%s's home two-step secret: %w", person, err)
	}
	t.Since = t.Since.UTC()
	return t, nil
}

// homeTwoStepSecret is where a person's home-page authenticator secret is
// kept: theirs, and nobody else's.
func homeTwoStepSecret(person string) []byte { return aad("home_two_step", "secret", person) }

func (s *Postgres) SetHomeTwoStep(ctx context.Context, person string, t TwoStepSetup) error {
	secret, err := s.sealString(t.Secret, homeTwoStepSecret(person))
	if err != nil {
		return err
	}
	t.Recovery = s.keyed(t.Recovery)
	_, err = s.pool.Exec(ctx, `insert into home_two_step (person, secret, last_step, recovery, since) values ($1, $2, $3, $4, $5)
		on conflict (person) do update set secret = $2, last_step = $3, recovery = $4, since = $5`,
		person, secret, t.LastStep, t.Recovery, t.Since)
	return err
}

func (s *Postgres) UseHomeStep(ctx context.Context, person string, step int64) (bool, error) {
	tag, err := s.pool.Exec(ctx, `update home_two_step set last_step = $2 where person = $1 and last_step < $2`, person, step)
	return err == nil && tag.RowsAffected() == 1, err
}

func (s *Postgres) UseHomeRecovery(ctx context.Context, person, hash string) (int, bool, error) {
	for _, keyed := range s.sealer.Hashes(hash) {
		var left int
		err := s.pool.QueryRow(ctx, `update home_two_step set recovery = array_remove(recovery, $2)
			where person = $1 and $2 = any (recovery) returning cardinality(recovery)`, person, keyed).Scan(&left)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		return left, err == nil, err
	}
	return 0, false, nil
}
