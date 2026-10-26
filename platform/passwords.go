// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"crypto/sha3"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/guildnet-chat/guildnet/monitor"
)

// People's IRC passwords are the console's. It checks every sign-in, its
// own and IRC's: a network's Ergo asks it about each login (/irc/login, from
// guildnet-ircauth) and takes its answer as final, so guessing is held back
// the same way everywhere, and nobody can change a password over IRC.
// Agents' accounts are Ergo's own: the console says they're not its.
//
// A person's password from before, still in Ergo, moves here the first time
// they sign in with it, and goes from Ergo.

// Passwords keeps people's IRC passwords (hashed as Ergo hashes them, so
// its hashes and the console's are alike) and where their sign-ins came
// from. A store that isn't one keeps them in the console's memory.
type Passwords interface {
	// IRCPassword is account's password hash on network, or ErrNotFound.
	IRCPassword(ctx context.Context, network, account string) ([]byte, error)
	SetIRCPassword(ctx context.Context, network, account string, hash []byte) error
	// RemoveIRCPassword removes account's password, and its app passwords.
	RemoveIRCPassword(ctx context.Context, network, account string) error
	// AppPasswords are account's IRC app passwords, oldest first.
	AppPasswords(ctx context.Context, network, account string) ([]AppPassword, error)
	AddAppPassword(ctx context.Context, network, account string, a AppPassword) error
	RemoveAppPassword(ctx context.Context, network, account, id string) error // ErrNotFound if not theirs
	UsedAppPassword(ctx context.Context, network, account, id string, at time.Time) error
	// SignInFailed and SignedIn keep where account's sign-ins came from.
	SignInFailed(ctx context.Context, network, account, source string, at time.Time) error
	SignedIn(ctx context.Context, network, account, source string, at time.Time) error
	// FailedSignIns are account's failed sign-ins since then.
	FailedSignIns(ctx context.Context, network, account string, since time.Time) ([]FailedSignIn, error)
	// FailedFrom is how many sign-ins from source failed since then, as
	// any account.
	FailedFrom(ctx context.Context, network, source string, since time.Time) (int, error)
	// SignedInFrom says whether account signed in from source since then.
	SignedInFrom(ctx context.Context, network, account, source string, since time.Time) (bool, error)
}

// FailedSignIn is a wrong password: from where, and when.
type FailedSignIn struct {
	Source string
	At     time.Time
}

// Guessing is held back: from one address (an IPv6 address's /64), after
// sourceGuesses wrong for the account in sourceWindow, or sprayGuesses wrong
// for any accounts, unless it has signed in as the account within knownFor;
// from every address that hasn't, after accountGuesses wrong for the account
// in accountWindow, and the owner is told.
const (
	sourceGuesses  = 5
	sourceWindow   = 15 * time.Minute
	sprayGuesses   = 20
	accountGuesses = 20
	accountWindow  = time.Hour
	knownFor       = 30 * 24 * time.Hour
)

// ircPasswordCost is bcrypt's cost for new hashes, as Ergo's (ircd.yaml's
// bcrypt-cost).
const ircPasswordCost = 10

// ircPasswordHash is password hashed as Ergo hashes it: bcrypt of its
// SHA3-512 (no 72-byte limit on passphrases).
func ircPasswordHash(password string) ([]byte, error) {
	sum := sha3.Sum512([]byte(password))
	return bcrypt.GenerateFromPassword(sum[:], ircPasswordCost)
}

func ircPasswordIs(hash []byte, password string) bool {
	sum := sha3.Sum512([]byte(password))
	return bcrypt.CompareHashAndPassword(hash, sum[:]) == nil
}

// signInSource is where a sign-in came from, as guessing is counted: the
// address, or an IPv6 address's /64.
func signInSource(ip string) string {
	a, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return "unknown"
	}
	a = a.Unmap()
	if a.Is6() {
		prefix, _ := a.Prefix(64)
		return prefix.String()
	}
	return a.String()
}

// passwords is where the console keeps people's passwords.
func (p *Platform) passwords() Passwords {
	if s, ok := p.opts.Networks.(Passwords); ok {
		return s
	}
	return &p.signIns
}

type signInResult int

const (
	signInWrong      signInResult = iota
	signInRight                   // and counted as a sign-in from there
	signInHeld                    // too many guesses: not even checked
	signInNotAPerson              // an agent's account, or nobody's: Ergo's
)

// errNotAPerson is an account that isn't a person in the network's org.
var errNotAPerson = errors.New("not a person in the org")

// checkSignIn checks a person's password, given from ip, for the console
// or IRC, holding back guessing.
func (p *Platform) checkSignIn(ctx context.Context, n Network, account, password, ip string) (signInResult, error) {
	return p.checkSignInWith(ctx, n, account, password, ip, p.passwordIsRight)
}

// checkIRCSignIn is checkSignIn for a password given over IRC: an app
// password, or the person's own unless they use two-step sign-in.
func (p *Platform) checkIRCSignIn(ctx context.Context, n Network, account, password, ip string) (signInResult, error) {
	return p.checkSignInWith(ctx, n, account, password, ip, p.ircPasswordIsRight)
}

func (p *Platform) checkSignInWith(ctx context.Context, n Network, account, password, ip string, isRight func(context.Context, Network, string, string) (bool, error)) (signInResult, error) {
	account = strings.ToLower(account)
	// One at a time for an account: passwords sent at once are counted
	// before the next is checked.
	lock, _ := p.signInLocks.LoadOrStore(signInKey(n.Name, account), &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()
	store := p.passwords()
	source := signInSource(ip)
	now := p.opts.Now()
	failed, err := store.FailedSignIns(ctx, n.Name, account, now.Add(-accountWindow))
	if err != nil {
		return signInWrong, err
	}
	here := 0
	for _, f := range failed {
		if f.Source == source && now.Sub(f.At) < sourceWindow {
			here++
		}
	}
	if here >= sourceGuesses {
		return signInHeld, nil
	}
	sprayed, err := store.FailedFrom(ctx, n.Name, source, now.Add(-sourceWindow))
	if err != nil {
		return signInWrong, err
	}
	if len(failed) >= accountGuesses || sprayed >= sprayGuesses {
		known, err := store.SignedInFrom(ctx, n.Name, account, source, now.Add(-knownFor))
		if err != nil {
			return signInWrong, err
		}
		if !known {
			return signInHeld, nil
		}
	}
	right, err := isRight(ctx, n, account, password)
	switch {
	case errors.Is(err, errNotAPerson):
		return signInNotAPerson, nil
	case err != nil:
		return signInWrong, err
	case right:
		if err := store.SignedIn(ctx, n.Name, account, source, now); err != nil {
			monitor.Problem(p.opts.Logger, "keeping %s's sign-in on %s: %v", account, n.Name, err)
		}
		return signInRight, nil
	}
	if err := store.SignInFailed(ctx, n.Name, account, source, now); err != nil {
		return signInWrong, err
	}
	if len(failed)+1 == accountGuesses {
		p.opts.Logger.Printf("%s on %s: %d wrong passwords in an hour, from several addresses: held back", account, n.Name, accountGuesses)
		p.tellOwner(ctx, n, guessingEmail(n, account, now))
	}
	return signInWrong, nil
}

// passwordIsRight checks account's password: the console's, or for a person
// whose password is still Ergo's, Ergo's, which then moves here.
// errNotAPerson for an account the console doesn't keep.
func (p *Platform) passwordIsRight(ctx context.Context, n Network, account, password string) (bool, error) {
	hash, err := p.passwords().IRCPassword(ctx, n.Name, account)
	if err == nil {
		return ircPasswordIs(hash, password), nil
	}
	if !errors.Is(err, ErrNotFound) {
		return false, err
	}
	person, err := p.isPerson(ctx, n, account)
	if err != nil {
		return false, err
	}
	if !person {
		return false, errNotAPerson
	}
	right, err := checkPassword(ctx, n, account, password)
	if err != nil || !right {
		return right, err
	}
	if err := p.keepIRCPassword(ctx, n, account, password); err != nil {
		monitor.Problem(p.opts.Logger, "moving %s's password on %s to the console: %v", account, n.Name, err)
	} else {
		p.opts.Logger.Printf("%s's password on %s moved to the console", account, n.Name)
	}
	return true, nil
}

// isPerson says whether account is a person in n's org, as its runner says.
func (p *Platform) isPerson(ctx context.Context, n Network, account string) (bool, error) {
	err := (runner{n: n, actor: account}).call(ctx, "GET", "/v1/me", nil, nil)
	switch {
	case err == nil:
		return true, nil
	case statusOf(err) == http.StatusForbidden:
		return false, nil
	}
	return false, err
}

// keepIRCPassword makes password account's, here; Ergo keeps none for it.
func (p *Platform) keepIRCPassword(ctx context.Context, n Network, account, password string) error {
	hash, err := ircPasswordHash(password)
	if err != nil {
		return err
	}
	if err := p.passwords().SetIRCPassword(ctx, n.Name, strings.ToLower(account), hash); err != nil {
		return err
	}
	if !p.opts.ErgoAsks {
		// Ergo can't ask (no broker): it checks a copy of its own.
		return setPassword(ctx, n, account, password)
	}
	if err := clearErgoPassword(ctx, n, account); err != nil {
		// Ergo asks the console first, and its no is final: one left there
		// does nothing.
		p.opts.Logger.Printf("clearing %s's password from %s's IRC server: %v", account, n.Name, err)
	}
	return nil
}

// createPersonsAccount makes a person's new account on n's Ergo, with no
// password there: the console keeps it. errAccountExists if the name's
// taken.
func (p *Platform) createPersonsAccount(ctx context.Context, n Network, account, password string) error {
	if err := createAccount(ctx, n, account, p.ergoCopy(password)); err != nil {
		return err
	}
	return p.keepIRCPassword(ctx, n, account, password)
}

// ergoCopy is the password a person's new account has on Ergo: none, if
// Ergo asks the console.
func (p *Platform) ergoCopy(password string) string {
	if p.opts.ErgoAsks {
		return ""
	}
	return password
}

// registerPersonsAccount is createPersonsAccount, where an account that's
// there already with this password (a retry, or a password from before
// still in Ergo) is fine.
func (p *Platform) registerPersonsAccount(ctx context.Context, n Network, account, password string) error {
	err := createAccount(ctx, n, account, p.ergoCopy(password))
	if !errors.Is(err, errAccountExists) {
		if err != nil {
			return err
		}
		return p.keepIRCPassword(ctx, n, account, password)
	}
	if hash, err := p.passwords().IRCPassword(ctx, n.Name, account); err == nil {
		if ircPasswordIs(hash, password) {
			return nil
		}
		return errAccountExists
	}
	right, err := checkPassword(ctx, n, account, password)
	if err != nil {
		return err
	}
	if !right {
		return errAccountExists
	}
	return p.keepIRCPassword(ctx, n, account, password)
}

// ircLogin is a network's Ergo asking about a login (guildnet-ircauth):
// yes, no, or not-mine (an agent's account, Ergo's own to check). An
// error is answered 503, and Ergo checks its own: agents still connect,
// and people's passwords aren't there.
func (p *Platform) ircLogin(w http.ResponseWriter, r *http.Request) {
	n, ok := p.ergoCalling(w, r)
	if !ok {
		return
	}
	var in struct {
		Account, Password, IP string
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&in); err != nil {
		brokerError(w, http.StatusBadRequest, "say whose login it is")
		return
	}
	answer := "no"
	if in.Account != "" && in.Password != "" {
		res, err := p.checkIRCSignIn(r.Context(), n, in.Account, in.Password, in.IP)
		if err != nil {
			p.opts.Logger.Printf("an IRC login on %s: %v", n.Name, err)
			brokerError(w, http.StatusServiceUnavailable, "couldn't check: try again")
			return
		}
		switch res {
		case signInRight:
			answer = "yes"
			p.noteUsed(r.Context(), n) // a person, in an IRC app
		case signInNotAPerson:
			answer = "not-mine"
		}
	}
	writeJSONResponse(w, http.StatusOK, map[string]string{"answer": answer, "account": in.Account})
}

// ergoCalling is the network whose Ergo is calling: the one it names, if
// it carries that network's Ergo token.
func (p *Platform) ergoCalling(w http.ResponseWriter, r *http.Request) (Network, bool) {
	n, err := p.opts.Networks.Network(r.Context(), r.Header.Get("GuildNet-Network"))
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	switch {
	case errors.Is(err, ErrNotFound):
	case err != nil:
		p.opts.Logger.Printf("checking an IRC server's token: %v", err)
		brokerError(w, http.StatusServiceUnavailable, "couldn't check the IRC server's token: try again")
		return Network{}, false
	case n.ErgoToken != "" && token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(n.ErgoToken)) == 1 && n.Status != Suspended && n.Status != Deleting:
		return n, true
	}
	brokerError(w, http.StatusUnauthorized, "the network's IRC server token is required")
	return Network{}, false
}

// guessingEmail tells the owner that account's password is being guessed.
func guessingEmail(n Network, account string, at time.Time) Letter {
	return Letter{Subject: "Someone is guessing " + account + "'s password on " + n.Name,
		Key:     "guessing/" + n.Name + "/" + account + "/" + at.UTC().Truncate(accountWindow).Format(time.RFC3339),
		Heading: "Someone is guessing " + account + "'s password",
		Paras: []string{
			fmt.Sprintf("%d wrong passwords were tried for %s on %s within an hour, from more than one address, by %s.", accountGuesses, account, n.Name, stamp(at)),
			"For the next hour, " + account + " can sign in, to the console or over IRC, only from addresses they've signed in from in the last 30 days. Everywhere else is turned away, right password or not.",
			"If " + account + "'s password is short or used elsewhere, have them change it on their account page.",
		},
		Why: securityWhy,
	}
}

// The console's memory's (for a store that keeps none), and the memory
// store's.

type signIns struct {
	mu     sync.Mutex
	hashes map[string][]byte               // network|account
	failed map[string][]FailedSignIn       // network|account
	known  map[string]map[string]time.Time // network|account -> source -> last
	apps   map[string][]AppPassword        // network|account
}

func signInKey(network, account string) string {
	return strings.ToLower(network) + "|" + strings.ToLower(account)
}

func (s *signIns) IRCPassword(_ context.Context, network, account string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.hashes[signInKey(network, account)]
	if !ok {
		return nil, ErrNotFound
	}
	return h, nil
}

func (s *signIns) SetIRCPassword(_ context.Context, network, account string, hash []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hashes == nil {
		s.hashes = map[string][]byte{}
	}
	s.hashes[signInKey(network, account)] = hash
	return nil
}

func (s *signIns) RemoveIRCPassword(_ context.Context, network, account string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.hashes, signInKey(network, account))
	delete(s.apps, signInKey(network, account))
	return nil
}

func (s *signIns) SignInFailed(_ context.Context, network, account, source string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed == nil {
		s.failed = map[string][]FailedSignIn{}
	}
	k := signInKey(network, account)
	kept := s.failed[k][:0]
	for _, f := range s.failed[k] {
		if at.Sub(f.At) < accountWindow {
			kept = append(kept, f)
		}
	}
	s.failed[k] = append(kept, FailedSignIn{Source: source, At: at})
	return nil
}

func (s *signIns) SignedIn(_ context.Context, network, account, source string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.known == nil {
		s.known = map[string]map[string]time.Time{}
	}
	k := signInKey(network, account)
	if s.known[k] == nil {
		s.known[k] = map[string]time.Time{}
	}
	s.known[k][source] = at
	return nil
}

func (s *signIns) FailedSignIns(_ context.Context, network, account string, since time.Time) ([]FailedSignIn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []FailedSignIn
	for _, f := range s.failed[signInKey(network, account)] {
		if f.At.After(since) {
			out = append(out, f)
		}
	}
	return out, nil
}

func (s *signIns) FailedFrom(_ context.Context, network, source string, since time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := signInKey(network, "")
	n := 0
	for k, fs := range s.failed {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		for _, f := range fs {
			if f.Source == source && f.At.After(since) {
				n++
			}
		}
	}
	return n, nil
}

func (s *signIns) SignedInFrom(_ context.Context, network, account, source string, since time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.known[signInKey(network, account)][source]
	return ok && at.After(since), nil
}

func (s *MemoryStore) IRCPassword(ctx context.Context, network, account string) ([]byte, error) {
	return s.ircPasswords.IRCPassword(ctx, network, account)
}

func (s *MemoryStore) SetIRCPassword(ctx context.Context, network, account string, hash []byte) error {
	return s.ircPasswords.SetIRCPassword(ctx, network, account, hash)
}

func (s *MemoryStore) RemoveIRCPassword(ctx context.Context, network, account string) error {
	return s.ircPasswords.RemoveIRCPassword(ctx, network, account)
}

func (s *MemoryStore) SignInFailed(ctx context.Context, network, account, source string, at time.Time) error {
	return s.ircPasswords.SignInFailed(ctx, network, account, source, at)
}

func (s *MemoryStore) SignedIn(ctx context.Context, network, account, source string, at time.Time) error {
	return s.ircPasswords.SignedIn(ctx, network, account, source, at)
}

func (s *MemoryStore) FailedSignIns(ctx context.Context, network, account string, since time.Time) ([]FailedSignIn, error) {
	return s.ircPasswords.FailedSignIns(ctx, network, account, since)
}

func (s *MemoryStore) SignedInFrom(ctx context.Context, network, account, source string, since time.Time) (bool, error) {
	return s.ircPasswords.SignedInFrom(ctx, network, account, source, since)
}

// Postgres's.

func (s *Postgres) IRCPassword(ctx context.Context, network, account string) ([]byte, error) {
	var hash []byte
	err := s.pool.QueryRow(ctx, `select hash from irc_passwords where network = $1 and account = $2`,
		strings.ToLower(network), strings.ToLower(account)).Scan(&hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return hash, err
}

func (s *Postgres) SetIRCPassword(ctx context.Context, network, account string, hash []byte) error {
	_, err := s.pool.Exec(ctx, `insert into irc_passwords (network, account, hash) values ($1, $2, $3)
		on conflict (network, account) do update set hash = excluded.hash`,
		strings.ToLower(network), strings.ToLower(account), hash)
	return err
}

func (s *Postgres) RemoveIRCPassword(ctx context.Context, network, account string) error {
	if _, err := s.pool.Exec(ctx, `delete from irc_app_passwords where network = $1 and account = $2`,
		strings.ToLower(network), strings.ToLower(account)); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `delete from irc_passwords where network = $1 and account = $2`,
		strings.ToLower(network), strings.ToLower(account))
	return err
}

func (s *Postgres) SignInFailed(ctx context.Context, network, account, source string, at time.Time) error {
	network, account = strings.ToLower(network), strings.ToLower(account)
	// Every account's, not only this one's: guesses at made-up accounts
	// would stay otherwise.
	if _, err := s.pool.Exec(ctx, `delete from irc_sign_in_failures where network = $1 and at < $2`,
		network, at.Add(-accountWindow)); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `insert into irc_sign_in_failures (network, account, source, at) values ($1, $2, $3, $4)`,
		network, account, source, at)
	return err
}

func (s *Postgres) SignedIn(ctx context.Context, network, account, source string, at time.Time) error {
	_, err := s.pool.Exec(ctx, `insert into irc_sign_in_sources (network, account, source, at) values ($1, $2, $3, $4)
		on conflict (network, account, source) do update set at = excluded.at`,
		strings.ToLower(network), strings.ToLower(account), source, at)
	return err
}

func (s *Postgres) FailedSignIns(ctx context.Context, network, account string, since time.Time) ([]FailedSignIn, error) {
	rows, err := s.pool.Query(ctx, `select source, at from irc_sign_in_failures where network = $1 and account = $2 and at > $3`,
		strings.ToLower(network), strings.ToLower(account), since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FailedSignIn
	for rows.Next() {
		var f FailedSignIn
		if err := rows.Scan(&f.Source, &f.At); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Postgres) FailedFrom(ctx context.Context, network, source string, since time.Time) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `select count(*) from irc_sign_in_failures where network = $1 and source = $2 and at > $3`,
		strings.ToLower(network), source, since).Scan(&n)
	return n, err
}

func (s *MemoryStore) FailedFrom(ctx context.Context, network, source string, since time.Time) (int, error) {
	return s.ircPasswords.FailedFrom(ctx, network, source, since)
}

func (s *Postgres) SignedInFrom(ctx context.Context, network, account, source string, since time.Time) (bool, error) {
	var yes bool
	err := s.pool.QueryRow(ctx, `select exists (select 1 from irc_sign_in_sources where network = $1 and account = $2 and source = $3 and at > $4)`,
		strings.ToLower(network), strings.ToLower(account), source, since).Scan(&yes)
	return yes, err
}
