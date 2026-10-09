// SPDX-License-Identifier: AGPL-3.0-only

// Package platform is GuildNet's web app: sign-in, and each network's
// console. It never touches a network's database; it manages a network
// through that network's runner API, as the signed-in person, so the same
// rules apply as in IRC. An Extension adds more (extension.go).
//
// Its API is version 0: it may change.
package platform

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/guildnet-chat/guildnet/internal/connectors"
	"github.com/guildnet-chat/guildnet/models"
)

// Network is one company's network, and how to reach its runner and Ergo.
// The tokens are that network's own secrets; the platform uses them only
// for that network, and stores them sealed.
type Network struct {
	Name        string // its address: name.<base domain>
	Region      string // where it's hosted, e.g. us-west1
	RunnerURL   string // its runner's internal API
	RunnerToken string
	ErgoURL     string // its Ergo's HTTP API
	ErgoToken   string
	ChatURL     string // its Ergo's WebSocket listener, for web chat
	ChatKey     string // signs the tokens its Ergo accepts in place of a password

	// For networks created on the home page: where setting it up got to,
	// who created it (a Person's ID) and the IRC nick they own it as, what
	// it was created from, and its settings. Keys are the owner's model
	// providers' keys, for the network's agents; they're sealed at rest.
	Status    string
	Problem   string // why setting up didn't finish, for the owner
	Owner     string
	OwnerNick string
	Template  string
	Timezone  string
	Keys      ModelKeys
	Updated   time.Time
}

// ModelKeys are a network's keys for each model provider, "" for none.
type ModelKeys struct {
	Anthropic, OpenAI, Google, OpenRouter string
}

func (k *ModelKeys) field(p models.Provider) *string {
	switch p {
	case models.Anthropic:
		return &k.Anthropic
	case models.OpenAI:
		return &k.OpenAI
	case models.Google:
		return &k.Google
	case models.OpenRouter:
		return &k.OpenRouter
	}
	return nil
}

// Get is p's key, or "".
func (k ModelKeys) Get(p models.Provider) string {
	if f := k.field(p); f != nil {
		return *f
	}
	return ""
}

// Set gives p key ("" takes it away).
func (k *ModelKeys) Set(p models.Provider, key string) {
	if f := k.field(p); f != nil {
		*f = key
	}
}

// Map is the keys there are, by provider.
func (k ModelKeys) Map() map[models.Provider]string {
	out := map[models.Provider]string{}
	for _, p := range models.Providers {
		if key := k.Get(p); key != "" {
			out[p] = key
		}
	}
	return out
}

// Where a network's setup has got to.
const (
	Creating = "creating"
	Ready    = "ready"
	Failed   = "failed"
	Deleting = "deleting"
	// Suspended by GuildNet's admins: stopped, with everything kept.
	Suspended = "suspended"
)

// Person is someone signed in on the home page, with Google or by email.
type Person struct {
	ID      string
	Email   string
	Blocked bool // by GuildNet's admins: they can't sign in
}

var (
	// ErrNotFound means there's no such network, or no such session.
	ErrNotFound = errors.New("not found")
	// ErrTaken means a network's name is already in use.
	ErrTaken = errors.New("that name is taken")
)

// Networks keeps networks, by name.
type Networks interface {
	Network(ctx context.Context, name string) (Network, error)
	AddNetwork(ctx context.Context, n Network) error // ErrTaken if the name is
	UpdateNetwork(ctx context.Context, n Network) error
	RemoveNetwork(ctx context.Context, name string) error
	NetworksOwnedBy(ctx context.Context, person string) ([]Network, error)
	AllNetworks(ctx context.Context) ([]Network, error) // by name
	// A network's GitHub installation: GitHubOf is none (a zero link) if
	// it isn't connected; BindGitHub is ErrGitHubTaken if the installation
	// is bound to another network.
	GitHubOf(ctx context.Context, network string) (GitHubLink, error)
	BindGitHub(ctx context.Context, network string, link GitHubLink) error
	UnbindGitHub(ctx context.Context, network string) error
}

// People keeps who has signed in on the home page, and who may create
// networks. The same address is the same person, with Google or by email.
type People interface {
	// GooglePerson is the person with this Google account: the first time,
	// whoever signed in by email with its address and has no Google account
	// yet, or someone new. Their email is kept up to date.
	GooglePerson(ctx context.Context, subject, email string) (Person, error)
	// EmailPerson is the person with this address (the first to have it),
	// made the first time they sign in.
	EmailPerson(ctx context.Context, email string) (Person, error)
	Person(ctx context.Context, id string) (Person, error)
	// Seen notes when someone was last signed in on the home page.
	Seen(ctx context.Context, id string, at time.Time) error
}

// PeopleAdmin is who has signed in on the home page, as an admin sees them,
// with blocking them, and a log of what admins did. Nothing from inside a
// network.
type PeopleAdmin interface {
	Everyone(ctx context.Context) ([]PersonRecord, error) // newest first
	SetBlocked(ctx context.Context, id string, blocked bool) error
	LogAdmin(ctx context.Context, e AdminEntry) error
	AdminLog(ctx context.Context, limit int) ([]AdminEntry, error) // newest first
}

// PersonRecord is a person as the admins see them.
type PersonRecord struct {
	Person
	Joined, LastSeen time.Time // LastSeen is zero if never noted
}

// AdminEntry is something an admin did.
type AdminEntry struct {
	At     time.Time
	Admin  string // their email
	Action string // e.g. invite, block, suspend
	Target string // an email or a network's name
}

// Link is a one-time link on a network: an invitation to join it, or a
// person's password reset. Only the hash of its secret is kept.
type Link struct {
	ID        string // names it to the people who manage it; not the secret
	Network   string
	Kind      string // Invite or Reset
	Account   string // a reset's person; an invite's, once used
	Role      string // an invite's place in the org
	Team      string
	Guest     bool
	CreatedBy string
	Created   time.Time
	Expires   time.Time
	Used      time.Time // zero until used
}

// What a Link is for.
const (
	Invite = "invite"
	Reset  = "reset"
)

// ErrLinkUsed means a link has already been used (or cancelled).
var ErrLinkUsed = errors.New("that link has been used")

// Links keeps one-time links by the hash of their secret.
type Links interface {
	AddLink(ctx context.Context, hash string, l Link) error
	Link(ctx context.Context, hash string) (Link, error) // ErrNotFound
	// UseLink marks a link used by account, once: ErrLinkUsed if it was.
	UseLink(ctx context.Context, hash, account string, at time.Time) error
	// ReleaseLink makes a used link good again, when what it was used for
	// didn't work out.
	ReleaseLink(ctx context.Context, hash string) error
	// PendingLinks are a network's links of kind that are unused and
	// unexpired at now, newest first.
	PendingLinks(ctx context.Context, network, kind string, now time.Time) ([]Link, error)
	CancelLink(ctx context.Context, network, id string, at time.Time) error
}

// Onboarding keeps the steps of each network's getting-started checklist
// its owner ticked off.
type Onboarding interface {
	OnboardingDone(ctx context.Context, network string) (map[string]bool, error)
	MarkOnboarding(ctx context.Context, network, step string, at time.Time) error
}

// Session is someone signed in to one network's console.
type Session struct {
	Network string
	Account string
	CSRF    string // the token every change must carry
	Expires time.Time
}

// Sessions keeps sessions by the hash of their token, never the token
// itself, so a copy of the store can't be used to sign in.
type Sessions interface {
	Save(ctx context.Context, hash string, s Session) error
	Get(ctx context.Context, hash string) (Session, error)
	Delete(ctx context.Context, hash string) error
	// DeleteAccount signs account out of network everywhere but the session
	// keep (a hash, or "").
	DeleteAccount(ctx context.Context, network, account, keep string) error
}

// MemoryStore keeps everything in memory, for tests and local development.
type MemoryStore struct {
	mu       sync.Mutex
	networks map[string]Network
	github   map[string]GitHubLink
	sessions map[string]Session
	people   map[string]Person // by ID
	joined   map[string]time.Time
	seen     map[string]time.Time
	google   map[string]string // Google subject -> person ID
	log      []AdminEntry
	links    map[string]Link            // by hash
	steps    map[string]map[string]bool // network -> step -> done
	alerts   map[string]alertRow        // by network|account
	twoStep  map[string]TwoStepSetup    // by network|account
	// homeTwoStep is two-step sign-in to the home page, by person.
	homeTwoStep map[string]TwoStepSetup
	passkeys    map[string][]Passkey // by network|account
	// connections by network|connector; oauthClients by issuer|redirect.
	connections  map[string]Connection
	oauthClients map[string]connectors.Client
	connMu       sync.Mutex         // connection changes, one at a time
	gitRepos     map[string]GitRepo // by network|name
}

func (s *MemoryStore) OnboardingDone(_ context.Context, network string) (map[string]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]bool{}
	for k, v := range s.steps[strings.ToLower(network)] {
		out[k] = v
	}
	return out, nil
}

func (s *MemoryStore) MarkOnboarding(_ context.Context, network, step string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.steps == nil {
		s.steps = map[string]map[string]bool{}
	}
	k := strings.ToLower(network)
	if s.steps[k] == nil {
		s.steps[k] = map[string]bool{}
	}
	s.steps[k][step] = true
	return nil
}

func (s *MemoryStore) AddLink(_ context.Context, hash string, l Link) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.links == nil {
		s.links = map[string]Link{}
	}
	s.links[hash] = l
	return nil
}

func (s *MemoryStore) Link(_ context.Context, hash string) (Link, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.links[hash]
	if !ok {
		return Link{}, ErrNotFound
	}
	return l, nil
}

func (s *MemoryStore) UseLink(_ context.Context, hash, account string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.links[hash]
	if !ok {
		return ErrNotFound
	}
	if !l.Used.IsZero() {
		return ErrLinkUsed
	}
	l.Used, l.Account = at, account
	s.links[hash] = l
	return nil
}

func (s *MemoryStore) ReleaseLink(_ context.Context, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.links[hash]; ok {
		l.Used = time.Time{}
		if l.Kind == Invite {
			l.Account = ""
		}
		s.links[hash] = l
	}
	return nil
}

func (s *MemoryStore) PendingLinks(_ context.Context, network, kind string, now time.Time) ([]Link, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Link
	for _, l := range s.links {
		if strings.EqualFold(l.Network, network) && l.Kind == kind && l.Used.IsZero() && now.Before(l.Expires) {
			out = append(out, l)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

func (s *MemoryStore) CancelLink(_ context.Context, network, id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for h, l := range s.links {
		if l.ID == id && strings.EqualFold(l.Network, network) {
			if l.Used.IsZero() {
				l.Used = at
				s.links[h] = l
			}
			return nil
		}
	}
	return ErrNotFound
}

func (s *MemoryStore) DeleteAccount(_ context.Context, network, account, keep string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for h, sess := range s.sessions {
		if h != keep && strings.EqualFold(sess.Network, network) && strings.EqualFold(sess.Account, account) {
			delete(s.sessions, h)
		}
	}
	return nil
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{networks: map[string]Network{}, sessions: map[string]Session{},
		people: map[string]Person{}, joined: map[string]time.Time{}, seen: map[string]time.Time{},
		google: map[string]string{}}
}

func (s *MemoryStore) Everyone(context.Context) ([]PersonRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []PersonRecord
	for id, p := range s.people {
		out = append(out, PersonRecord{Person: p, Joined: s.joined[id], LastSeen: s.seen[id]})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Joined.Equal(out[j].Joined) {
			return out[i].Joined.After(out[j].Joined)
		}
		return out[i].Email < out[j].Email
	})
	return out, nil
}

func (s *MemoryStore) SetBlocked(_ context.Context, id string, blocked bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.people[id]
	if !ok {
		return ErrNotFound
	}
	p.Blocked = blocked
	s.people[id] = p
	return nil
}

func (s *MemoryStore) Seen(_ context.Context, id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.people[id]; ok {
		s.seen[id] = at
	}
	return nil
}

func (s *MemoryStore) AllNetworks(context.Context) ([]Network, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Network
	for _, n := range s.networks {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *MemoryStore) LogAdmin(_ context.Context, e AdminEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log = append(s.log, e)
	return nil
}

func (s *MemoryStore) AdminLog(_ context.Context, limit int) ([]AdminEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []AdminEntry
	for i := len(s.log) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, s.log[i])
	}
	return out, nil
}

func (s *MemoryStore) UpdateNetwork(_ context.Context, n Network) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.ToLower(n.Name)
	if _, ok := s.networks[key]; !ok {
		return ErrNotFound
	}
	s.networks[key] = n
	return nil
}

func (s *MemoryStore) RemoveNetwork(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.ToLower(name)
	delete(s.networks, key)
	s.dropConnections(key)
	s.dropGitRepos(key)
	for h, sess := range s.sessions {
		if strings.EqualFold(sess.Network, key) {
			delete(s.sessions, h)
		}
	}
	for h, l := range s.links {
		if strings.EqualFold(l.Network, key) {
			delete(s.links, h)
		}
	}
	delete(s.steps, key)
	for k := range s.alerts {
		if strings.HasPrefix(k, key+"|") {
			delete(s.alerts, k)
		}
	}
	return nil
}

func (s *MemoryStore) NetworksOwnedBy(_ context.Context, person string) ([]Network, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Network
	for _, n := range s.networks {
		if person != "" && n.Owner == person {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *MemoryStore) GooglePerson(_ context.Context, subject, email string) (Person, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	email = strings.ToLower(email)
	id, ok := s.google[subject]
	if !ok {
		// Someone who signed in by email with this address, and has no
		// Google account yet, is this one.
		withGoogle := map[string]bool{}
		for _, pid := range s.google {
			withGoogle[pid] = true
		}
		id = s.firstWith(email, func(pid string) bool { return !withGoogle[pid] })
		if id == "" {
			id = newID()
			s.joined[id] = time.Now()
		}
		s.google[subject] = id
	}
	p := s.people[id]
	p.ID, p.Email = id, email
	s.people[id] = p
	return p, nil
}

// firstWith is the ID of the first person to join with email who is also ok,
// or "".
func (s *MemoryStore) firstWith(email string, ok func(id string) bool) string {
	var found string
	for id, p := range s.people {
		if p.Email != email || !ok(id) {
			continue
		}
		if found == "" || s.joined[id].Before(s.joined[found]) || (s.joined[id].Equal(s.joined[found]) && id < found) {
			found = id
		}
	}
	return found
}

func (s *MemoryStore) EmailPerson(_ context.Context, email string) (Person, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	email = strings.ToLower(strings.TrimSpace(email))
	if id := s.firstWith(email, func(string) bool { return true }); id != "" {
		return s.people[id], nil
	}
	p := Person{ID: newID(), Email: email}
	s.people[p.ID], s.joined[p.ID] = p, time.Now()
	return p, nil
}

func (s *MemoryStore) Person(_ context.Context, id string) (Person, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.people[id]
	if !ok {
		return Person{}, ErrNotFound
	}
	return p, nil
}

func (s *MemoryStore) AddNetwork(_ context.Context, n Network) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.ToLower(n.Name)
	if _, taken := s.networks[key]; taken {
		return ErrTaken
	}
	s.networks[key] = n
	return nil
}

// PutNetwork adds a network or replaces its details, for bootstrapping the
// local network from the environment.
func (s *MemoryStore) PutNetwork(_ context.Context, n Network) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.networks[strings.ToLower(n.Name)] = n
	return nil
}

func (s *MemoryStore) Network(_ context.Context, name string) (Network, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.networks[strings.ToLower(name)]
	if !ok {
		return Network{}, ErrNotFound
	}
	return n, nil
}

func (s *MemoryStore) Save(_ context.Context, hash string, sess Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[hash] = sess
	return nil
}

func (s *MemoryStore) Get(_ context.Context, hash string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[hash]
	if !ok {
		return Session{}, ErrNotFound
	}
	return sess, nil
}

func (s *MemoryStore) Delete(_ context.Context, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, hash)
	return nil
}

func (s *MemoryStore) DeleteExpired(_ context.Context, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for h, sess := range s.sessions {
		if !now.Before(sess.Expires) {
			delete(s.sessions, h)
		}
	}
	return nil
}

// Sealer keeps secrets at rest, and the hashes tokens are kept by, with the
// platform's key (GUILDNET_PLATFORM_KEY). Cloud KMS could take its place
// behind the same interface.
type Sealer interface {
	// Seal seals plain bound to where it's kept (aad: its table, column and
	// row, and where it's sent): moved anywhere else, it doesn't open.
	Seal(plain, aad []byte) ([]byte, error)
	// Open opens what Seal sealed, with the same aad.
	Open(sealed, aad []byte) ([]byte, error)
	// Hash is what a token's hash (hashToken's) is kept as: keyed, so
	// nobody without the key can make one that matches a token they hold.
	Hash(h string) string
	// Hashes are what h was kept as under each key there is, the current
	// one's first: to find one kept before the key changed.
	Hashes(h string) []string
}

// aad binds a sealed value to where it's kept: its parts, each written with
// its length, so no two lists of parts are the same bytes.
func aad(parts ...string) []byte {
	var b []byte
	for _, p := range parts {
		b = binary.AppendUvarint(b, uint64(len(p)))
		b = append(b, p...)
	}
	return b
}

// sealedV1 starts every value sealed this way: then the key's ID, then the
// nonce and the sealed bytes.
const sealedV1 = 0x01

// aesSealer is AES-256-GCM, with keys derived from the platform's key for
// each purpose (sealing, hashing), and a random nonce per secret. Values
// say which key sealed them, so the key can change: older keys (old) still
// open what they sealed, and find what they hashed.
type aesSealer struct {
	keys []sealKey // the current one first
}

type sealKey struct {
	id     [4]byte
	aead   cipher.AEAD // for sealing
	hash   []byte      // for keying hashes
	legacy cipher.AEAD // the key itself, as values were sealed before v1
}

// NewAESSealer seals with a 32-byte key; old keys (32 bytes each) only open
// and find what they sealed and hashed, while the key changes.
func NewAESSealer(key []byte, old ...[]byte) (Sealer, error) {
	var s aesSealer
	for _, k := range append([][]byte{key}, old...) {
		if len(k) != 32 {
			return nil, fmt.Errorf("the sealing key must be 32 bytes, not %d", len(k))
		}
		sk, err := newSealKey(k)
		if err != nil {
			return nil, err
		}
		s.keys = append(s.keys, sk)
	}
	return s, nil
}

func newSealKey(key []byte) (sealKey, error) {
	var sk sealKey
	id := sha256.Sum256(append([]byte("guildnet key id|"), key...))
	copy(sk.id[:], id[:4])
	seal, err := hkdf.Key(sha256.New, key, nil, "guildnet seal v1", 32)
	if err != nil {
		return sk, err
	}
	if sk.hash, err = hkdf.Key(sha256.New, key, nil, "guildnet hash v1", 32); err != nil {
		return sk, err
	}
	if sk.aead, err = gcm(seal); err != nil {
		return sk, err
	}
	sk.legacy, err = gcm(key)
	return sk, err
}

func gcm(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (s aesSealer) Seal(plain, aad []byte) ([]byte, error) {
	k := s.keys[0]
	nonce := make([]byte, k.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append([]byte{sealedV1}, k.id[:]...)
	out = append(out, nonce...)
	return k.aead.Seal(out, nonce, plain, aad), nil
}

var errUnseal = errors.New("couldn't unseal a secret: wrong key, moved from where it was sealed, or tampered with")

func (s aesSealer) Open(sealed, aad []byte) ([]byte, error) {
	if len(sealed) < 5 || sealed[0] != sealedV1 {
		return nil, errUnseal // the old way, or not sealed at all: not taken
	}
	for _, k := range s.keys {
		if [4]byte(sealed[1:5]) != k.id {
			continue
		}
		rest := sealed[5:]
		if len(rest) < k.aead.NonceSize() {
			return nil, errUnseal
		}
		plain, err := k.aead.Open(nil, rest[:k.aead.NonceSize()], rest[k.aead.NonceSize():], aad)
		if err != nil {
			return nil, errUnseal
		}
		return plain, nil
	}
	return nil, errors.New("couldn't unseal a secret: it was sealed with a key that isn't given (GUILDNET_PLATFORM_OLD_KEYS)")
}

// openLegacy opens a value sealed the old way (before v1: the key itself,
// nothing bound), for the upgrade that seals them all again; nowhere else.
func (s aesSealer) openLegacy(sealed []byte) ([]byte, error) {
	for _, k := range s.keys {
		n := k.legacy.NonceSize()
		if len(sealed) < n {
			continue
		}
		if plain, err := k.legacy.Open(nil, sealed[:n], sealed[n:], nil); err == nil {
			return plain, nil
		}
	}
	return nil, errUnseal
}

func (s aesSealer) Hash(h string) string { return s.keys[0].keyed(h) }

func (s aesSealer) Hashes(h string) []string {
	out := make([]string, len(s.keys))
	for i, k := range s.keys {
		out[i] = k.keyed(h)
	}
	return out
}

func (k sealKey) keyed(h string) string {
	mac := hmac.New(sha256.New, k.hash)
	mac.Write([]byte(h))
	return hex.EncodeToString(mac.Sum(nil))
}

// newToken is 32 random bytes, URL-safe.
func newToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the system's randomness failing isn't something to carry on from
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// newID is a person's ID: random, so IDs say nothing about who or when.
func newID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// hashToken is what a session is stored under.
func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func (s *MemoryStore) GitHubOf(_ context.Context, network string) (GitHubLink, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.github[strings.ToLower(network)], nil
}

func (s *MemoryStore) BindGitHub(_ context.Context, network string, link GitHubLink) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.github == nil {
		s.github = map[string]GitHubLink{}
	}
	for other, l := range s.github {
		if l.Installation == link.Installation && other != strings.ToLower(network) {
			return ErrGitHubTaken
		}
	}
	s.github[strings.ToLower(network)] = link
	return nil
}

func (s *MemoryStore) UnbindGitHub(_ context.Context, network string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.github, strings.ToLower(network))
	return nil
}
