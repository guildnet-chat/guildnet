// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	. "github.com/guildnet-chat/guildnet/platformtest"

	"github.com/jackc/pgx/v5"

	"github.com/guildnet-chat/guildnet/internal/connectors"
	"github.com/guildnet-chat/guildnet/platform"
)

func TestMain(m *testing.M) { Main(m) }

// The platform keeps networks, sessions and people. The same
// behaviour is expected of the in-memory store and Postgres; Postgres runs
// in a throwaway container, and its cases skip if Docker isn't running.

// store is what the platform keeps things in.
type store interface {
	platform.Networks
	platform.Sessions
	platform.People
	DeleteExpired(ctx context.Context, now time.Time) error
}

// eachStore runs a test against both stores.
func eachStore(t *testing.T, test func(t *testing.T, s store)) {
	t.Run("memory", func(t *testing.T) { test(t, platform.NewMemoryStore()) })
	t.Run("postgres", func(t *testing.T) {
		url := NewDatabase(t)
		s, err := platform.OpenPostgres(context.Background(), url, MustSealer(t, TestKey))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.Close)
		test(t, s)
	})
}

// unused is n without when it was last used, which the store keeps itself.
func unused(n platform.Network) platform.Network {
	n.Used = time.Time{}
	return n
}

// When a network was last used is kept apart from the rest of it: noting
// never takes it back, and changing the network leaves it alone.
func TestWhenANetworkWasLastUsedIsKeptApart(t *testing.T) {
	eachStore(t, func(t *testing.T, s store) {
		ctx := context.Background()
		if err := s.AddNetwork(ctx, acmeNet); err != nil {
			t.Fatal(err)
		}
		n, _ := s.Network(ctx, "acme")
		if time.Since(n.Used) > time.Minute {
			t.Errorf("a new network's use: %s", n.Used)
		}
		later := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
		s.NoteUsed(ctx, "acme", later)
		s.NoteUsed(ctx, "acme", later.Add(-30*time.Minute))
		n, _ = s.Network(ctx, "acme")
		if !n.Used.Equal(later) {
			t.Errorf("used at %s, want %s: never taken back", n.Used, later)
		}
		n.Used, n.Timezone = time.Time{}, "Europe/Paris"
		if err := s.UpdateNetwork(ctx, n); err != nil {
			t.Fatal(err)
		}
		if n, _ := s.Network(ctx, "acme"); !n.Used.Equal(later) || n.Timezone != "Europe/Paris" {
			t.Errorf("after a change: used %s, timezone %q", n.Used, n.Timezone)
		}
		if err := s.NoteUsed(ctx, "nowhere", later); !errors.Is(err, platform.ErrNotFound) {
			t.Errorf("an unknown network: %v", err)
		}
	})
}

// How full its desks were when its owner was last told is kept apart too.
func TestHowFullANetworksDesksWereWhenToldIsKeptApart(t *testing.T) {
	eachStore(t, func(t *testing.T, s store) {
		ctx := context.Background()
		if err := s.AddNetwork(ctx, acmeNet); err != nil {
			t.Fatal(err)
		}
		if n, _ := s.Network(ctx, "acme"); n.StorageTold != 0 {
			t.Errorf("a new network was told at %d%%", n.StorageTold)
		}
		if err := s.NoteStorageTold(ctx, "acme", 80); err != nil {
			t.Fatal(err)
		}
		n, _ := s.Network(ctx, "acme")
		n.StorageTold, n.Timezone = 0, "Europe/Paris"
		if err := s.UpdateNetwork(ctx, n); err != nil {
			t.Fatal(err)
		}
		if n, _ := s.Network(ctx, "acme"); n.StorageTold != 80 || n.Timezone != "Europe/Paris" {
			t.Errorf("after a change: told at %d%%, timezone %q", n.StorageTold, n.Timezone)
		}
		if err := s.NoteStorageTold(ctx, "nowhere", 80); !errors.Is(err, platform.ErrNotFound) {
			t.Errorf("an unknown network: %v", err)
		}
	})
}

// IRC app passwords are kept per person, found by id, noted when used, and
// go with the person's password when they leave the org.
func TestAppPasswordsAreKeptPerPerson(t *testing.T) {
	eachStore(t, func(t *testing.T, s store) {
		ctx := context.Background()
		if err := s.AddNetwork(ctx, acmeNet); err != nil {
			t.Fatal(err)
		}
		pw := s.(platform.Passwords)
		made := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
		for _, a := range []platform.AppPassword{{ID: "aaaaaa", Name: "Laptop", Hash: []byte("h1"), Created: made}, {ID: "bbbbbb", Name: "Phone", Hash: []byte("h2"), Created: made.Add(time.Minute)}} {
			if err := pw.AddAppPassword(ctx, "acme", "Cy", a); err != nil {
				t.Fatal(err)
			}
		}
		if err := pw.UsedAppPassword(ctx, "acme", "cy", "aaaaaa", made.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		got, err := pw.AppPasswords(ctx, "acme", "cy")
		if err != nil || len(got) != 2 || got[0].Name != "Laptop" || !got[0].Used.Equal(made.Add(time.Hour)) || !got[1].Used.IsZero() || string(got[1].Hash) != "h2" {
			t.Fatalf("cy's: %+v %v", got, err)
		}
		if err := pw.RemoveAppPassword(ctx, "acme", "ada", "aaaaaa"); !errors.Is(err, platform.ErrNotFound) {
			t.Errorf("removing cy's as ada: %v, want ErrNotFound", err)
		}
		if err := pw.RemoveAppPassword(ctx, "acme", "cy", "aaaaaa"); err != nil {
			t.Fatal(err)
		}
		if got, _ := pw.AppPasswords(ctx, "acme", "cy"); len(got) != 1 || got[0].ID != "bbbbbb" {
			t.Errorf("after removing one: %+v", got)
		}
		if err := pw.RemoveIRCPassword(ctx, "acme", "cy"); err != nil {
			t.Fatal(err)
		}
		if got, _ := pw.AppPasswords(ctx, "acme", "cy"); len(got) != 0 {
			t.Errorf("after cy's password went: %+v", got)
		}
	})
}

var acmeNet = platform.Network{Name: "acme", Region: "us-west1", RunnerURL: "http://runner-acme:8090", RunnerToken: "acme-runner-secret",
	ErgoURL: "http://ergo-acme:8089", ErgoToken: "acme-ergo-secret",
	ChatURL: "ws://ergo-acme:8097", ChatKey: "acme-chat-key",
	Status: platform.Ready, OwnerNick: "ada", Template: "team", Timezone: "Europe/Paris",
	Keys: platform.ModelKeys{Anthropic: "sk-ant-acme-key-0123456789", OpenAI: "sk-proj-acme-key-0123456789"}, Updated: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)}

func TestANetworkIsFoundByName(t *testing.T) {
	eachStore(t, func(t *testing.T, s store) {
		ctx := context.Background()
		if err := s.AddNetwork(ctx, acmeNet); err != nil {
			t.Fatal(err)
		}

		got, err := s.Network(ctx, "ACME")
		got.Used = time.Time{} // the store's own (TestWhenANetworkWasLastUsedIsKeptApart)

		if err != nil || got != acmeNet {
			t.Errorf("got %+v, %v; want %+v", got, err, acmeNet)
		}
		if _, err := s.Network(ctx, "globex"); !errors.Is(err, platform.ErrNotFound) {
			t.Errorf("an unknown network: %v, want ErrNotFound", err)
		}
	})
}

func TestANetworksNameIsTakenOnce(t *testing.T) {
	eachStore(t, func(t *testing.T, s store) {
		ctx := context.Background()
		s.AddNetwork(ctx, acmeNet)
		other := acmeNet
		other.Name = "Acme"

		if err := s.AddNetwork(ctx, other); !errors.Is(err, platform.ErrTaken) {
			t.Errorf("err = %v, want ErrTaken", err)
		}
	})
}

func TestASessionIsKeptUntilItsDeleted(t *testing.T) {
	eachStore(t, func(t *testing.T, s store) {
		ctx := context.Background()
		s.AddNetwork(ctx, acmeNet)
		sess := platform.Session{Network: "acme", Account: "ada", CSRF: "c", Expires: time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)}
		if err := s.Save(ctx, "hash1", sess); err != nil {
			t.Fatal(err)
		}

		got, err := s.Get(ctx, "hash1")
		if err != nil || got != sess {
			t.Errorf("got %+v, %v; want %+v", got, err, sess)
		}
		s.Delete(ctx, "hash1")
		if _, err := s.Get(ctx, "hash1"); !errors.Is(err, platform.ErrNotFound) {
			t.Errorf("after deleting: %v, want ErrNotFound", err)
		}
	})
}

// Signing someone out everywhere else reaches their home-page sign-ins too,
// which belong to no network.
func TestSigningOutEverywhereElseOnTheHomePage(t *testing.T) {
	eachStore(t, func(t *testing.T, s store) {
		ctx := context.Background()
		s.AddNetwork(ctx, acmeNet)
		ada, _ := s.GooglePerson(ctx, "sub-ada", "ada@example.com")
		later := time.Now().Add(time.Hour)
		s.Save(ctx, "here", platform.Session{Account: ada.ID, CSRF: "c", Expires: later})
		s.Save(ctx, "elsewhere", platform.Session{Account: ada.ID, CSRF: "c", Expires: later})
		s.Save(ctx, "console", platform.Session{Network: "acme", Account: ada.ID, CSRF: "c", Expires: later})

		if err := s.DeleteAccount(ctx, "", ada.ID, "here"); err != nil {
			t.Fatal(err)
		}

		if _, err := s.Get(ctx, "elsewhere"); !errors.Is(err, platform.ErrNotFound) {
			t.Errorf("ada's other home sign-in: %v, want signed out", err)
		}
		if _, err := s.Get(ctx, "here"); err != nil {
			t.Errorf("the sign-in kept: %v", err)
		}
		if _, err := s.Get(ctx, "console"); err != nil {
			t.Errorf("a console sign-in with the same name: %v, want kept", err)
		}
	})
}

func TestExpiredSessionsAreCleanedUp(t *testing.T) {
	eachStore(t, func(t *testing.T, s store) {
		ctx := context.Background()
		s.AddNetwork(ctx, acmeNet)
		now := time.Now()
		s.Save(ctx, "old", platform.Session{Network: "acme", Account: "ada", CSRF: "c", Expires: now.Add(-time.Minute)})
		s.Save(ctx, "live", platform.Session{Network: "acme", Account: "ada", CSRF: "c", Expires: now.Add(time.Hour)})

		if err := s.DeleteExpired(ctx, now); err != nil {
			t.Fatal(err)
		}

		if _, err := s.Get(ctx, "old"); !errors.Is(err, platform.ErrNotFound) {
			t.Error("the expired session should be gone")
		}
		if _, err := s.Get(ctx, "live"); err != nil {
			t.Error("the live session should stay")
		}
	})
}

// Only Postgres: secrets at rest.

func TestNetworkSecretsAreStoredSealed(t *testing.T) {
	url := NewDatabase(t)
	ctx := context.Background()
	s, err := platform.OpenPostgres(ctx, url, MustSealer(t, TestKey))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.AddNetwork(ctx, acmeNet)

	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var runnerToken, ergoToken, chatKey, anthropicKey, modelKeys []byte
	if err := conn.QueryRow(ctx, "select runner_token, ergo_token, chat_key, anthropic_key, model_keys from networks where name = 'acme'").Scan(&runnerToken, &ergoToken, &chatKey, &anthropicKey, &modelKeys); err != nil {
		t.Fatal(err)
	}
	stored := bytes.Join([][]byte{runnerToken, ergoToken, chatKey, anthropicKey, modelKeys}, nil)
	for _, secret := range []string{"acme-runner-secret", "acme-ergo-secret", "acme-chat-key", "sk-ant-acme-key-0123456789", "sk-proj-acme-key-0123456789", "sk-proj", "openai"} {
		if bytes.Contains(stored, []byte(secret)) {
			t.Errorf("the networks table holds %q in plain text", secret)
		}
	}
}

// openStore opens a Postgres store at url with keys (the current one first),
// and a plain connection to it, to change rows as someone with the database
// but not the key could.
func openStore(t *testing.T, url string, keys ...[]byte) (*platform.Postgres, *pgx.Conn) {
	t.Helper()
	sealer, err := platform.NewAESSealer(keys[0], keys[1:]...)
	if err != nil {
		t.Fatal(err)
	}
	s, err := platform.OpenPostgres(context.Background(), url, sealer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	conn, err := pgx.Connect(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return s, conn
}

func runSQL(t *testing.T, conn *pgx.Conn, sql string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// Someone with the database but not the key can't move a sealed secret to
// where it'd be used for something else: another network's row (whose
// runner the broker would hand it to), another column, or the same row with
// its address changed (where the console would send it).
func TestASealedSecretOpensOnlyWhereItWasSealed(t *testing.T) {
	ctx := context.Background()
	s, conn := openStore(t, NewDatabase(t), TestKey)
	s.AddNetwork(ctx, acmeNet)
	globex := acmeNet
	globex.Name, globex.RunnerToken, globex.Keys = "globex", "globex-runner-secret", platform.ModelKeys{}
	s.AddNetwork(ctx, globex)

	runSQL(t, conn, "update networks set model_keys = (select model_keys from networks where name = 'acme') where name = 'globex'")
	if got, err := s.Network(ctx, "globex"); err == nil {
		t.Errorf("globex opened with acme's model keys: %+v", got.Keys)
	}

	runSQL(t, conn, "update networks set ergo_token = runner_token where name = 'acme'")
	if _, err := s.Network(ctx, "acme"); err == nil {
		t.Error("acme's runner token opened as its Ergo token")
	}

	s.UpdateNetwork(ctx, acmeNet)
	runSQL(t, conn, "update networks set runner_url = 'https://evil.example/' where name = 'acme'")
	if got, err := s.Network(ctx, "acme"); err == nil {
		t.Errorf("acme's runner token opened with its runner moved to %s", got.RunnerURL)
	}
}

// A connection's tokens and client secret open only with the connection
// they were sealed with: not another network's, and not with the service's
// token address changed (the console would POST the refresh token there).
func TestAConnectionsSecretsOpenOnlyWithIt(t *testing.T) {
	ctx := context.Background()
	s, conn := openStore(t, NewDatabase(t), TestKey)
	for _, name := range []string{"acme", "globex"} {
		n := acmeNet
		n.Name = name
		s.AddNetwork(ctx, n)
		s.SaveConnection(ctx, name, platform.Connection{Connector: "linear",
			Server: connectors.Server{Issuer: "https://linear.app", Token: "https://api.linear.app/oauth/token"},
			Client: connectors.Client{ID: "client-" + name, Secret: "secret-" + name},
			Tokens: connectors.Tokens{Access: "at-" + name, Refresh: "rt-" + name}, By: "ada", At: time.Now().UTC().Truncate(time.Second)})
	}

	runSQL(t, conn, `update connections g set access_token = a.access_token, refresh_token = a.refresh_token
		from connections a where a.network = 'acme' and g.network = 'globex'`)
	if c, err := s.Connection(ctx, "globex", "linear"); err == nil {
		t.Errorf("globex's connection opened with acme's tokens: %q", c.Tokens.Refresh)
	}

	runSQL(t, conn, `update connections set server = jsonb_set(server, '{token}', '"https://evil.example/token"') where network = 'acme'`)
	if c, err := s.Connection(ctx, "acme", "linear"); err == nil {
		t.Errorf("acme's refresh token opened to go to %s", c.Server.Token)
	}
}

// Sign-ins are kept by a keyed hash of their token: a row put in by someone
// with the database, but not the key, signs nobody in.
func TestASessionMadeWithoutTheKeySignsNobodyIn(t *testing.T) {
	w := NewWorldWithPostgres(t)
	conn, err := pgx.Connect(context.Background(), w.DBURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	sum := sha256.Sum256([]byte("forged-token"))
	runSQL(t, conn, "insert into sessions (hash, network, account, csrf, expires) values ($1, 'acme', 'ada', 'c', now() + interval '1 hour')", hex.EncodeToString(sum[:]))

	b := w.Browser("acme")
	b.Cookies["__Host-guildnet"] = &http.Cookie{Name: "__Host-guildnet", Value: "forged-token"}
	b.Cookies["guildnet"] = &http.Cookie{Name: "guildnet", Value: "forged-token"}

	if r := b.Get("/"); r.Status != http.StatusSeeOther {
		t.Errorf("a session put in the database: %d, want sent to sign in", r.Status)
	}
}

// The key can change: with the old one listed after it, everything sealed
// and kept before opens and finds, and what's saved now uses the new one.
func TestThePlatformKeyCanChange(t *testing.T) {
	ctx := context.Background()
	url := NewDatabase(t)
	old, _ := openStore(t, url, TestKey)
	old.AddNetwork(ctx, acmeNet)
	old.Save(ctx, "a-session", platform.Session{Network: "acme", Account: "ada", CSRF: "c", Expires: time.Now().Add(time.Hour)})
	old.Close()

	newKey := bytes.Repeat([]byte{9}, 32)
	s, _ := openStore(t, url, newKey, TestKey)
	if got, err := s.Network(ctx, "acme"); err != nil || got.RunnerToken != acmeNet.RunnerToken {
		t.Fatalf("with the old key listed: %+v %v", got, err)
	}
	if _, err := s.Get(ctx, "a-session"); err != nil {
		t.Errorf("a sign-in from before: %v", err)
	}
	s.UpdateNetwork(ctx, acmeNet)
	s.Close()

	alone, _ := openStore(t, url, newKey)
	if _, err := alone.Network(ctx, "acme"); err != nil {
		t.Errorf("saved since, with the new key alone: %v", err)
	}
}

// legacySeal is how secrets were sealed before they were bound to their
// rows: AES-GCM under the key itself, nonce first, nothing bound.
func legacySeal(t *testing.T, key []byte, plain string) []byte {
	t.Helper()
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, gcm.NonceSize())
	rand.Read(nonce)
	return gcm.Seal(nonce, nonce, []byte(plain), nil)
}

// A database from before is sealed again the first time it's opened: its
// secrets bound to their rows, its hashes keyed, nobody signed out, and a
// network with only an Anthropic key from before keeps it. After that,
// secrets sealed the old way aren't taken.
func TestADatabaseFromBeforeIsSealedAgain(t *testing.T) {
	ctx := context.Background()
	url := NewDatabase(t)
	first, conn := openStore(t, url, TestKey)
	first.Close()
	// As it was: no upgrade yet, secrets sealed the old way, plain hashes.
	runSQL(t, conn, "delete from upgrades")
	runSQL(t, conn, `insert into networks (name, region, runner_url, runner_token, ergo_url, ergo_token, chat_url, chat_key, status, anthropic_key, updated_at)
		values ('acme', 'local', 'http://runner', $1, 'http://ergo', $2, 'ws://chat', $3, 'ready', $4, now())`,
		legacySeal(t, TestKey, "acme-runner-secret"), legacySeal(t, TestKey, "acme-ergo-secret"), legacySeal(t, TestKey, "acme-chat-key"), legacySeal(t, TestKey, "sk-ant-old"))
	sum := sha256.Sum256([]byte("old-cookie"))
	runSQL(t, conn, "insert into sessions (hash, network, account, csrf, expires) values ($1, 'acme', 'ada', 'c', now() + interval '1 hour')", hex.EncodeToString(sum[:]))

	s, _ := openStore(t, url, TestKey)

	got, err := s.Network(ctx, "acme")
	if err != nil || got.RunnerToken != "acme-runner-secret" || got.ChatKey != "acme-chat-key" || got.Keys.Anthropic != "sk-ant-old" {
		t.Fatalf("after: %+v %v", got, err)
	}
	if _, err := s.Get(ctx, hex.EncodeToString(sum[:])); err != nil {
		t.Errorf("a sign-in from before: %v", err)
	}
	runSQL(t, conn, "update networks set runner_token = $1 where name = 'acme'", legacySeal(t, TestKey, "put-in-later"))
	if _, err := s.Network(ctx, "acme"); err == nil {
		t.Error("a secret sealed the old way was taken after the upgrade")
	}
}

func TestSecretsSealedWithOneKeyDontOpenWithAnother(t *testing.T) {
	url := NewDatabase(t)
	ctx := context.Background()
	s, _ := platform.OpenPostgres(ctx, url, MustSealer(t, TestKey))
	s.AddNetwork(ctx, acmeNet)
	s.Close()

	other, err := platform.OpenPostgres(ctx, url, MustSealer(t, bytes.Repeat([]byte{8}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()

	if _, err := other.Network(ctx, "acme"); err == nil || errors.Is(err, platform.ErrNotFound) {
		t.Errorf("err = %v, want a failure to unseal", err)
	}
}

func TestSessionsAreStoredOnlyByTheHashOfTheirToken(t *testing.T) {
	w := NewWorldWithPostgres(t)
	b := w.Browser("acme").SignedIn("ada")

	var token string
	for _, c := range b.Cookies {
		token = c.Value
	}
	conn, err := pgx.Connect(context.Background(), w.DBURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var dump string
	conn.QueryRow(context.Background(), "select coalesce(string_agg(row_to_json(s)::text, ''), '') from sessions s").Scan(&dump)
	if dump == "" || strings.Contains(dump, token) {
		t.Errorf("sessions table %q should hold the session, but not the cookie's token", dump)
	}
}

func TestSignInsSurviveARestartOfThePlatform(t *testing.T) {
	w := NewWorldWithPostgres(t)
	b := w.Browser("acme").SignedIn("ada")

	w.RestartPlatform()

	if r := b.Get("/"); r.Status != 200 {
		t.Errorf("after a restart: %d, want still signed in", r.Status)
	}
}

func TestTheKeyMustBe32Bytes(t *testing.T) {
	if _, err := platform.NewAESSealer([]byte("short")); err == nil {
		t.Error("a short key should be refused")
	}
}

func TestAPersonIsTheSameGoogleAccountEachTime(t *testing.T) {
	eachStore(t, func(t *testing.T, s store) {
		ctx := context.Background()
		first, err := s.GooglePerson(ctx, "sub-1", "Ada@Example.com")
		if err != nil {
			t.Fatal(err)
		}
		again, _ := s.GooglePerson(ctx, "sub-1", "ada@new.example")
		other, _ := s.GooglePerson(ctx, "sub-2", "ada@example.com")
		if again.ID != first.ID || again.Email != "ada@new.example" {
			t.Errorf("signing in again: %+v, want %s with the new email", again, first.ID)
		}
		if other.ID == first.ID {
			t.Error("another Google account with the same email is the same person")
		}
		if got, err := s.Person(ctx, first.ID); err != nil || got != again {
			t.Errorf("person %s: %+v %v", first.ID, got, err)
		}
		if _, err := s.Person(ctx, "nobody"); !errors.Is(err, platform.ErrNotFound) {
			t.Errorf("an unknown person: %v", err)
		}
	})
}

func TestANetworkBelongsToItsOwnerAndGoesWithItsSignIns(t *testing.T) {
	eachStore(t, func(t *testing.T, s store) {
		ctx := context.Background()
		ada, _ := s.GooglePerson(ctx, "sub-ada", "ada@example.com")
		bob, _ := s.GooglePerson(ctx, "sub-bob", "bob@example.com")
		n := acmeNet
		n.Owner = ada.ID
		if err := s.AddNetwork(ctx, n); err != nil {
			t.Fatal(err)
		}
		if owned, _ := s.NetworksOwnedBy(ctx, ada.ID); len(owned) != 1 || unused(owned[0]) != n {
			t.Errorf("ada's networks: %+v", owned)
		}
		if owned, _ := s.NetworksOwnedBy(ctx, bob.ID); len(owned) != 0 {
			t.Errorf("bob's networks: %+v", owned)
		}

		n.Status, n.Problem, n.Timezone = platform.Failed, "It broke.", "America/Edmonton"
		if err := s.UpdateNetwork(ctx, n); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.Network(ctx, "acme"); unused(got) != n {
			t.Errorf("after updating: %+v, want %+v", got, n)
		}
		if err := s.UpdateNetwork(ctx, platform.Network{Name: "globex", RunnerToken: "x", ErgoToken: "y"}); !errors.Is(err, platform.ErrNotFound) {
			t.Errorf("updating a network that isn't there: %v", err)
		}

		s.Save(ctx, "console", platform.Session{Network: "acme", Account: "ada", CSRF: "c", Expires: time.Now().Add(time.Hour)})
		s.Save(ctx, "home", platform.Session{Account: ada.ID, CSRF: "c", Expires: time.Now().Add(time.Hour)})
		if err := s.RemoveNetwork(ctx, "acme"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Network(ctx, "acme"); !errors.Is(err, platform.ErrNotFound) {
			t.Errorf("acme after removing: %v", err)
		}
		if _, err := s.Get(ctx, "console"); !errors.Is(err, platform.ErrNotFound) {
			t.Error("a sign-in to acme outlived it")
		}
		if got, err := s.Get(ctx, "home"); err != nil || got.Network != "" || got.Account != ada.ID {
			t.Errorf("ada's home sign-in: %+v %v", got, err)
		}
	})
}

func TestANetworksGitHubInstallationIsKept(t *testing.T) {
	eachStore(t, func(t *testing.T, s store) {
		ctx := context.Background()
		s.AddNetwork(ctx, acmeNet)
		globex := acmeNet
		globex.Name = "globex"
		s.AddNetwork(ctx, globex)
		at := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)

		if l, err := s.GitHubOf(ctx, "acme"); err != nil || l.Installation != 0 {
			t.Errorf("before connecting: %+v %v", l, err)
		}
		if err := s.BindGitHub(ctx, "acme", platform.GitHubLink{Installation: 7, Account: "acme-inc", By: "p1", At: at}); err != nil {
			t.Fatal(err)
		}
		if l, _ := s.GitHubOf(ctx, "ACME"); l.Installation != 7 || l.Account != "acme-inc" || !l.At.Equal(at) {
			t.Errorf("after connecting: %+v", l)
		}
		if err := s.BindGitHub(ctx, "globex", platform.GitHubLink{Installation: 7, At: at}); !errors.Is(err, platform.ErrGitHubTaken) {
			t.Errorf("binding acme's installation to globex: %v, want ErrGitHubTaken", err)
		}
		if err := s.UnbindGitHub(ctx, "acme"); err != nil {
			t.Fatal(err)
		}
		if err := s.BindGitHub(ctx, "globex", platform.GitHubLink{Installation: 7, At: at}); err != nil {
			t.Errorf("once acme let go, globex may have it: %v", err)
		}
	})
}
