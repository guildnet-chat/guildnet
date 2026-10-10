// SPDX-License-Identifier: AGPL-3.0-only

package platformtest

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"testing"

	"github.com/fxamacker/cbor/v2"
)

// Authenticator is a passkey provider, in software: one key pair, made for
// one site.
type Authenticator struct {
	t     *testing.T
	ID    []byte
	key   *ecdsa.PrivateKey
	count uint32
	// Origin is where the browser says it is; rpID the site the key is for.
	Origin, rpID string
}

// NewAuthenticator is a new passkey, on a device, for network's console.
func NewAuthenticator(t *testing.T, network string) *Authenticator {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 16)
	rand.Read(id)
	host := network + "." + BaseDomain
	return &Authenticator{t: t, ID: id, key: key, Origin: "http://" + host, rpID: host}
}

func (a *Authenticator) clientData(kind, challenge string) []byte {
	b, _ := json.Marshal(map[string]any{"type": kind, "challenge": challenge, "origin": a.Origin, "crossOrigin": false})
	return b
}

func (a *Authenticator) authData(flags byte, extra []byte) []byte {
	rp := sha256.Sum256([]byte(a.rpID))
	var count [4]byte
	binary.BigEndian.PutUint32(count[:], a.count)
	return append(append(append(rp[:], flags), count[:]...), extra...)
}

// create answers a registration's options as the browser would pass them on.
func (a *Authenticator) create(options []byte) []byte {
	a.t.Helper()
	challenge, rpID := challengeOf(a.t, options)
	if rpID != a.rpID {
		a.t.Fatalf("the passkey is asked for %q, not the network's own address %q", rpID, a.rpID)
	}
	x, y := a.key.PublicKey.X.FillBytes(make([]byte, 32)), a.key.PublicKey.Y.FillBytes(make([]byte, 32))
	cose, _ := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: x, -3: y})
	var idLen [2]byte
	binary.BigEndian.PutUint16(idLen[:], uint16(len(a.ID)))
	attested := append(append(append(make([]byte, 16), idLen[:]...), a.ID...), cose...)
	att, _ := cbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": a.authData(0x45, attested)})
	body, _ := json.Marshal(map[string]any{
		"id": b64.EncodeToString(a.ID), "rawId": b64.EncodeToString(a.ID), "type": "public-key",
		"response": map[string]any{"clientDataJSON": b64.EncodeToString(a.clientData("webauthn.create", challenge)), "attestationObject": b64.EncodeToString(att)},
	})
	return body
}

// get answers a sign-in's options.
func (a *Authenticator) get(options []byte) []byte {
	a.t.Helper()
	challenge, _ := challengeOf(a.t, options)
	a.count++
	data := a.authData(0x05, nil)
	client := a.clientData("webauthn.get", challenge)
	sum := sha256.Sum256(client)
	digest := sha256.Sum256(append(append([]byte{}, data...), sum[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	if err != nil {
		a.t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{
		"id": b64.EncodeToString(a.ID), "rawId": b64.EncodeToString(a.ID), "type": "public-key",
		"response": map[string]any{"clientDataJSON": b64.EncodeToString(client), "authenticatorData": b64.EncodeToString(data), "signature": b64.EncodeToString(sig)},
	})
	return body
}

// Call is the page's script calling the console with JSON, as fetch does:
// with the CSRF token of the last page, if there was one.
func (b *Browser) Call(path string, body []byte) Response {
	b.T.Helper()
	req, err := http.NewRequest("POST", b.W.Server.URL+path, bytes.NewReader(body))
	if err != nil {
		b.T.Fatal(err)
	}
	req.Host = b.Host
	req.Header.Set("Origin", "http://"+b.Host)
	req.Header.Set("Content-Type", "application/json")
	if token := b.CSRF(); token != "" {
		req.Header.Set("X-CSRF-Token", token)
	}
	for _, c := range b.Cookies {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		b.T.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	for _, c := range resp.Cookies() {
		if c.MaxAge < 0 {
			delete(b.Cookies, c.Name)
		} else {
			b.Cookies[c.Name] = c
		}
	}
	return Response{Status: resp.StatusCode, Body: string(raw), Header: resp.Header}
}

// To is where a JSON answer says To go next.
func (r Response) To(t *testing.T) string {
	t.Helper()
	var out struct {
		To, Error string
	}
	json.Unmarshal([]byte(r.Body), &out)
	if r.Status != http.StatusOK || out.To == "" {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	return out.To
}

// AddPasskey adds a passkey to whoever's signed in, from the page at from.
func (b *Browser) AddPasskey(a *Authenticator, from string) Response {
	b.T.Helper()
	b.Get(from)
	begin := b.Call("/account/passkeys/begin", nil)
	if begin.Status != http.StatusOK {
		b.T.Fatalf("starting to add a passkey: %d %s", begin.Status, begin.Body)
	}
	return b.Call("/account/passkeys/finish?name="+url.QueryEscape("Ada's phone"), a.create([]byte(begin.Body)))
}

// UsePasskey answers the sign-in's second step with a passkey.
func (b *Browser) UsePasskey(a *Authenticator) Response {
	b.T.Helper()
	b.Get("/signin/two-step")
	begin := b.Call("/signin/two-step/passkey/begin", nil)
	if begin.Status != http.StatusOK {
		b.T.Fatalf("starting the passkey: %d %s", begin.Status, begin.Body)
	}
	return b.Call("/signin/two-step/passkey/finish", a.get([]byte(begin.Body)))
}

var b64 = base64.RawURLEncoding

// challengeOf is the challenge in a ceremony's options, and the site they
// name, if they name one.
func challengeOf(t *testing.T, options []byte) (challenge, rpID string) {
	t.Helper()
	var o struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
			RPID      string `json:"rpId"`
			RP        struct {
				ID string `json:"id"`
			} `json:"rp"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(options, &o); err != nil || o.PublicKey.Challenge == "" {
		t.Fatalf("not a ceremony's options: %v\n%s", err, options)
	}
	rpID = o.PublicKey.RPID
	if rpID == "" {
		rpID = o.PublicKey.RP.ID
	}
	return o.PublicKey.Challenge, rpID
}
