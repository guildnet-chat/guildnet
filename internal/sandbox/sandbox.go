// SPDX-License-Identifier: AGPL-3.0-only

// Package sandbox is sandboxd: it keeps the desks of GuildNet's own, one
// workspace per agent in a container of its own (gVisor on the sandbox VM),
// and runs what the runner asks there. A runner reaches only its network's
// desks, with the token the platform signed for that network. Nothing here
// holds a key a desk could use: the model loop and every credential stay in
// the runner.
package sandbox

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Desk is one agent's desk: its network and nick.
type Desk struct {
	Network, Nick string
}

// Name is the desk's container name. Neither part may hold a dot, so no two
// desks share one.
func (d Desk) Name() string { return "gn." + d.Network + "." + d.Nick }

// A name is lowercase letters, digits, - and _: no dots, slashes or spaces.
var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// Exec is a command to run at a desk.
type Exec struct {
	Args    []string
	Dir     string
	Stdin   io.Reader // nil for none
	Output  io.Writer // stdout and stderr together
	Timeout time.Duration
}

// Result is how a command ended.
type Result struct {
	Exit     int
	TimedOut bool
}

var (
	ErrNoFile = errors.New("no such file")
	ErrTooBig = errors.New("file too big")
)

// Backend is where desks' containers are: Docker on the sandbox VM, a
// stand-in in tests, Kubernetes later.
type Backend interface {
	// Ensure makes the desk's container, or starts it if it's stopped.
	Ensure(ctx context.Context, d Desk) error
	Exec(ctx context.Context, d Desk, e Exec) (Result, error)
	// ReadFile is a file's content: ErrNoFile if it isn't a file there,
	// ErrTooBig past max.
	ReadFile(ctx context.Context, d Desk, path string, max int64) ([]byte, error)
	// WriteFile makes or replaces a file, and its directories.
	WriteFile(ctx context.Context, d Desk, path string, data []byte) error
	// Stop stops the desk's container; its files stay.
	Stop(ctx context.Context, d Desk) error
	// Remove deletes the desk's container and its files.
	Remove(ctx context.Context, d Desk) error
	// RemoveNetwork deletes every desk of network, and their files: the
	// network has been deleted.
	RemoveNetwork(ctx context.Context, network string) error
}

// Sized is a backend that can limit how much a network's desks' files may
// take, in all.
type Sized interface {
	SetStorage(ctx context.Context, network string, gb int) error
}

// NewKeys is a key pair for networks' tokens: the platform keeps the
// private key and signs with it; sandboxd has only the public one, so the
// sandbox VM holds nothing that could make a token.
func NewKeys() (ed25519.PublicKey, ed25519.PrivateKey) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err) // the system's randomness failed
	}
	return public, private
}

// Keys written as text say which half they are: both are 32 bytes, and the
// private one mustn't be taken for the public one (and so be put on the
// sandbox VM).
const (
	privatePrefix = "sandbox-private:"
	publicPrefix  = "sandbox-public:"
)

// KeyText is a key written for settings: which half it is, then base64 of
// the public key or of the private key's seed.
func KeyText[K ed25519.PublicKey | ed25519.PrivateKey](k K) string {
	switch k := any(k).(type) {
	case ed25519.PrivateKey:
		return privatePrefix + base64.StdEncoding.EncodeToString(k.Seed())
	case ed25519.PublicKey:
		return publicPrefix + base64.StdEncoding.EncodeToString(k)
	}
	return ""
}

func keyBytes(text, prefix string, size int) ([]byte, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(text), prefix)
	if !ok {
		return nil, false
	}
	b, err := base64.StdEncoding.DecodeString(rest)
	return b, err == nil && len(b) == size
}

// PrivateKeyFrom reads a private key written by KeyText.
func PrivateKeyFrom(text string) (ed25519.PrivateKey, error) {
	seed, ok := keyBytes(text, privatePrefix, ed25519.SeedSize)
	if !ok {
		return nil, errors.New("not a sandbox private key")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// PublicKeyFrom reads a public key written by KeyText.
func PublicKeyFrom(text string) (ed25519.PublicKey, error) {
	b, ok := keyBytes(text, publicPrefix, ed25519.PublicKeySize)
	if !ok {
		return nil, errors.New("not a sandbox public key")
	}
	return ed25519.PublicKey(b), nil
}

// What a token lets its holder do: a runner works its network's desks; only
// the platform removes all of them, once the network's deleted.
const (
	Desks = "desks"
	Admin = "admin"
	// Egress is a network's egress pass, which its desks hold: it opens its
	// git repositories' hosts at the desks' proxy, and does nothing here.
	Egress = "egress"
)

// A token is good for a while only (the platform signs a new one when
// asked), with a little slack for clocks that differ.
const (
	tokenSkew    = time.Minute
	tokenLongest = time.Hour
)

// Claims are what a token says: whose network it's for, what it may do,
// and when it was signed and stops working.
type Claims struct {
	Network string    `json:"net"`
	Scope   string    `json:"scope"`
	Issued  time.Time `json:"iat"`
	Expires time.Time `json:"exp"`
	// Hosts are what an egress pass opens: host, or host:port (443 if
	// none).
	Hosts []string `json:"hosts,omitempty"`
	// StorageGB is how much the network's desks' files may take, in all
	// (its plan's): 0 says nothing of it.
	StorageGB int `json:"gb,omitempty"`
}

// tokenPurposeV2 keeps these signatures from meaning anything elsewhere.
const tokenPurposeV2 = "guildnet sandbox v2|"

// TokenFor is a token for network's desks, to do scope, signed now and good
// for ttl: "v2.", what it says, and the platform's signature of exactly
// that.
func TokenFor(key ed25519.PrivateKey, network, scope string, now time.Time, ttl time.Duration) string {
	return sign(key, Claims{Network: network, Scope: scope, Issued: now.UTC().Truncate(time.Second), Expires: now.Add(ttl).UTC().Truncate(time.Second)})
}

// DesksTokenFor is a token for network's desks that says how much their
// files may take, in all (storageGB; 0 for nothing said).
func DesksTokenFor(key ed25519.PrivateKey, network string, storageGB int, now time.Time, ttl time.Duration) string {
	return sign(key, Claims{Network: network, Scope: Desks, StorageGB: storageGB, Issued: now.UTC().Truncate(time.Second), Expires: now.Add(ttl).UTC().Truncate(time.Second)})
}

// PassFor is network's egress pass, opening hosts (host, or host:port), signed
// now and good for ttl.
func PassFor(key ed25519.PrivateKey, network string, hosts []string, now time.Time, ttl time.Duration) string {
	return sign(key, Claims{Network: network, Scope: Egress, Hosts: hosts, Issued: now.UTC().Truncate(time.Second), Expires: now.Add(ttl).UTC().Truncate(time.Second)})
}

func sign(key ed25519.PrivateKey, c Claims) string {
	payload, _ := json.Marshal(c)
	sig := ed25519.Sign(key, append([]byte(tokenPurposeV2), payload...))
	return "v2." + base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// PassOpens says whether pass, an egress pass one of keys signed that's
// good now, opens host on port.
func PassOpens(keys []ed25519.PublicKey, pass string, now time.Time, host string, port int) bool {
	c, ok := claimsOf(keys, pass, now)
	if !ok || c.Scope != Egress {
		return false
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, h := range c.Hosts {
		name, p, err := net.SplitHostPort(h)
		if err != nil {
			name, p = h, "443"
		}
		if strings.EqualFold(strings.TrimSuffix(name, "."), host) && p == strconv.Itoa(port) {
			return true
		}
	}
	return false
}

// claimsOf is what token says, if one of keys signed it and it's good now.
// Old tokens (a network's name, signed, for ever) aren't taken.
func claimsOf(keys []ed25519.PublicKey, token string, now time.Time) (Claims, bool) {
	parts := strings.Split(token, ".")
	switch {
	case len(parts) == 3 && parts[0] == "v2":
		payload, err1 := base64.RawURLEncoding.DecodeString(parts[1])
		sig, err2 := base64.RawURLEncoding.DecodeString(parts[2])
		if err1 != nil || err2 != nil || !verified(keys, append([]byte(tokenPurposeV2), payload...), sig) {
			return Claims{}, false
		}
		var c Claims
		if json.Unmarshal(payload, &c) != nil || !validName.MatchString(c.Network) || (c.Scope != Desks && c.Scope != Admin && c.Scope != Egress) {
			return Claims{}, false
		}
		good := !c.Issued.After(now.Add(tokenSkew)) && !now.After(c.Expires.Add(tokenSkew)) && c.Expires.Sub(c.Issued) <= tokenLongest
		return c, good
	}
	return Claims{}, false
}

func verified(keys []ed25519.PublicKey, msg, sig []byte) bool {
	for _, k := range keys {
		if len(k) == ed25519.PublicKeySize && ed25519.Verify(k, msg, sig) {
			return true
		}
	}
	return false
}

// may says whether c lets its holder do scope.
func (c Claims) may(scope string) bool { return c.Scope == scope }
