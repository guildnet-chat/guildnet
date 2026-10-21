// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/guildnet-chat/guildnet/monitor"
)

// IRC app passwords: an IRC app signs in with a password of its own, made
// on the account page and shown once, and revoked there, which ends the
// account's IRC sessions. An account with two-step sign-in signs in over
// IRC only with these: its own password alone can't read its chat.
// Anyone may make them, to keep their password out of IRC apps.

// AppPassword is one IRC app's password: its hash, never the password.
type AppPassword struct {
	ID      string // the password's first part, to find it by
	Name    string // which app, as its person called it
	Hash    []byte
	Created time.Time
	Used    time.Time // zero until it signs in
}

// maxAppPasswords is how many one person may have.
const maxAppPasswords = 10

// newAppPassword is a new app password: an id of 6 letters, then 20 more
// in groups of five (about 100 bits), from the setup code's letters (none to
// misread), in lower case.
func newAppPassword() (id, password string) {
	letters := strings.ToLower(codeLetters)
	pick := func(n int) string {
		b := make([]byte, n)
		rand.Read(b)
		for i := range b {
			b[i] = letters[int(b[i])%len(letters)]
		}
		return string(b)
	}
	id = pick(6)
	groups := []string{id}
	for range 4 {
		groups = append(groups, pick(5))
	}
	return id, strings.Join(groups, "-")
}

// appPasswordIsRight checks password as one of account's app passwords,
// noting it was used.
func (p *Platform) appPasswordIsRight(ctx context.Context, n Network, account, password string) (bool, error) {
	id, _, ok := strings.Cut(password, "-")
	if !ok || len(id) != 6 {
		return false, nil
	}
	all, err := p.passwords().AppPasswords(ctx, n.Name, account)
	if err != nil {
		return false, err
	}
	i := slices.IndexFunc(all, func(a AppPassword) bool { return a.ID == id })
	if i < 0 || !ircPasswordIs(all[i].Hash, password) {
		return false, nil
	}
	if err := p.passwords().UsedAppPassword(ctx, n.Name, account, id, p.opts.Now()); err != nil {
		monitor.Problem(p.opts.Logger, "noting %s's app password used on %s: %v", account, n.Name, err)
	}
	return true, nil
}

// ircPasswordIsRight checks a password given over IRC: one of account's
// app passwords, or, unless they use two-step sign-in, their own.
func (p *Platform) ircPasswordIsRight(ctx context.Context, n Network, account, password string) (bool, error) {
	if right, err := p.appPasswordIsRight(ctx, n, account, password); right || err != nil {
		return right, err
	}
	two, err := p.signsInWithTwoSteps(ctx, n, account)
	if err != nil || two {
		return false, err
	}
	return p.passwordIsRight(ctx, n, account, password)
}

// signsInWithTwoSteps says whether account uses two-step sign-in, or must.
func (p *Platform) signsInWithTwoSteps(ctx context.Context, n Network, account string) (bool, error) {
	if p.opts.TwoSteps == nil {
		return false, nil
	}
	t, err := p.opts.TwoSteps.TwoStepOf(ctx, n.Name, account)
	switch {
	case err == nil && t.On():
		return true, nil
	case err != nil && !errors.Is(err, ErrNotFound):
		return false, err
	case !p.opts.RequireTwoStep:
		return false, nil
	}
	var m me
	err = (runner{n: n, actor: account}).call(ctx, "GET", "/v1/me", nil, &m)
	switch {
	case statusOf(err) == http.StatusForbidden:
		return false, nil // not a person in the org: an agent's, say
	case err != nil:
		return false, err
	}
	return mustUseTwoStep(m), nil
}

// makeAppPassword makes the one signed in an app password, and shows it,
// once.
func (p *Platform) makeAppPassword(w http.ResponseWriter, r *http.Request) {
	s, _ := sessionOf(r)
	n := networkOf(r)
	name := strings.Join(strings.Fields(r.PostFormValue("name")), " ")
	if name == "" || len(name) > 60 {
		p.renderAccount(w, r, http.StatusBadRequest, "Name the app password (at most 60 characters), for the app it's for.", false)
		return
	}
	all, err := p.passwords().AppPasswords(r.Context(), n.Name, s.Account)
	if err != nil {
		p.renderAccount(w, r, http.StatusInternalServerError, "Couldn't make it just now. Try again in a moment.", false)
		return
	}
	if len(all) >= maxAppPasswords {
		p.renderAccount(w, r, http.StatusBadRequest, "You have as many app passwords as you may: revoke one you don't use first.", false)
		return
	}
	id, password := newAppPassword()
	hash, err := ircPasswordHash(password)
	if err == nil {
		err = p.passwords().AddAppPassword(r.Context(), n.Name, s.Account, AppPassword{ID: id, Name: name, Hash: hash, Created: p.opts.Now()})
	}
	if err != nil {
		monitor.Problem(p.opts.Logger, "making %s an app password on %s: %v", s.Account, n.Name, err)
		p.renderAccount(w, r, http.StatusInternalServerError, "Couldn't make it just now. Try again in a moment.", false)
		return
	}
	p.opts.Logger.Printf("%s made an IRC app password on %s", s.Account, n.Name)
	p.renderAccountWith(w, r, http.StatusOK, "", func(v *accountView) { v.NewAppPassword, v.NewAppPasswordName = password, name })
}

// revokeAppPassword revokes one of the signed-in person's app passwords,
// and ends their IRC sessions: one signed in with it is gone now.
func (p *Platform) revokeAppPassword(w http.ResponseWriter, r *http.Request) {
	s, _ := sessionOf(r)
	n := networkOf(r)
	err := p.passwords().RemoveAppPassword(r.Context(), n.Name, s.Account, r.PathValue("id"))
	switch {
	case errors.Is(err, ErrNotFound):
		http.Redirect(w, r, "/account", http.StatusSeeOther)
		return
	case err != nil:
		p.renderAccount(w, r, http.StatusInternalServerError, "Couldn't revoke it just now. Try again in a moment.", false)
		return
	}
	p.opts.Logger.Printf("%s revoked an IRC app password on %s", s.Account, n.Name)
	if err := p.runner(r).call(r.Context(), "POST", "/v1/people/"+s.Account+"/disconnect", nil, nil); err != nil {
		monitor.Problem(p.opts.Logger, "ending %s's IRC sessions on %s: %v", s.Account, n.Name, err)
	}
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

// The console's memory's, the memory store's and Postgres's.

func (s *signIns) AppPasswords(_ context.Context, network, account string) ([]AppPassword, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.apps[signInKey(network, account)]), nil
}

func (s *signIns) AddAppPassword(_ context.Context, network, account string, a AppPassword) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.apps == nil {
		s.apps = map[string][]AppPassword{}
	}
	k := signInKey(network, account)
	s.apps[k] = append(s.apps[k], a)
	return nil
}

func (s *signIns) RemoveAppPassword(_ context.Context, network, account, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := signInKey(network, account)
	i := slices.IndexFunc(s.apps[k], func(a AppPassword) bool { return a.ID == id })
	if i < 0 {
		return ErrNotFound
	}
	s.apps[k] = slices.Delete(slices.Clone(s.apps[k]), i, i+1)
	return nil
}

func (s *signIns) UsedAppPassword(_ context.Context, network, account, id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, a := range s.apps[signInKey(network, account)] {
		if a.ID == id {
			s.apps[signInKey(network, account)][i].Used = at
		}
	}
	return nil
}

func (s *MemoryStore) AppPasswords(ctx context.Context, network, account string) ([]AppPassword, error) {
	return s.ircPasswords.AppPasswords(ctx, network, account)
}

func (s *MemoryStore) AddAppPassword(ctx context.Context, network, account string, a AppPassword) error {
	return s.ircPasswords.AddAppPassword(ctx, network, account, a)
}

func (s *MemoryStore) RemoveAppPassword(ctx context.Context, network, account, id string) error {
	return s.ircPasswords.RemoveAppPassword(ctx, network, account, id)
}

func (s *MemoryStore) UsedAppPassword(ctx context.Context, network, account, id string, at time.Time) error {
	return s.ircPasswords.UsedAppPassword(ctx, network, account, id, at)
}

func (s *Postgres) AppPasswords(ctx context.Context, network, account string) ([]AppPassword, error) {
	rows, err := s.pool.Query(ctx, `select id, name, hash, created, used from irc_app_passwords
		where network = $1 and account = $2 order by created`, strings.ToLower(network), strings.ToLower(account))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AppPassword
	for rows.Next() {
		var a AppPassword
		var used *time.Time
		if err := rows.Scan(&a.ID, &a.Name, &a.Hash, &a.Created, &used); err != nil {
			return nil, err
		}
		if used != nil {
			a.Used = *used
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Postgres) AddAppPassword(ctx context.Context, network, account string, a AppPassword) error {
	_, err := s.pool.Exec(ctx, `insert into irc_app_passwords (network, account, id, name, hash, created) values ($1, $2, $3, $4, $5, $6)`,
		strings.ToLower(network), strings.ToLower(account), a.ID, a.Name, a.Hash, a.Created)
	return err
}

func (s *Postgres) RemoveAppPassword(ctx context.Context, network, account, id string) error {
	tag, err := s.pool.Exec(ctx, `delete from irc_app_passwords where network = $1 and account = $2 and id = $3`,
		strings.ToLower(network), strings.ToLower(account), id)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Postgres) UsedAppPassword(ctx context.Context, network, account, id string, at time.Time) error {
	_, err := s.pool.Exec(ctx, `update irc_app_passwords set used = $4 where network = $1 and account = $2 and id = $3`,
		strings.ToLower(network), strings.ToLower(account), id, at)
	return err
}
