// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/models"
	"github.com/guildnet-chat/guildnet/platform"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// Signing in on the home page with Google.

func TestSigningInWithGoogleTakesYouToYourNetworks(t *testing.T) {
	w := NewWorld(t)
	b := w.Home()
	if page := b.Get("/signin"); page.Status != http.StatusOK || !strings.Contains(page.Text(), "Sign in with Google") {
		t.Fatalf("the sign-in page: %d %s", page.Status, page.Body)
	}
	r := b.SignInWithGoogle("ada@example.com")
	if r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/networks" {
		t.Fatalf("after Google: %d to %q, want your networks", r.Status, r.Header.Get("Location"))
	}
	page := b.Get("/networks")
	if page.Status != http.StatusOK || !strings.Contains(page.Text(), "ada@example.com") {
		t.Errorf("your networks page: %d %s", page.Status, page.Body)
	}
}

func TestTheHomePagesAreForPeopleSignedInWithGoogle(t *testing.T) {
	w := NewWorld(t)
	b := w.Home()
	for _, path := range []string{"/networks", "/networks/new", "/networks/acme"} {
		if r := b.Get(path); r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/signin" {
			t.Errorf("%s without signing in: %d to %q, want the sign-in page", path, r.Status, r.Header.Get("Location"))
		}
	}
	// A console session on a network is no good here, and a home session is
	// no good on a network.
	console := w.Browser("acme").SignedIn("ada")
	b.Cookies = console.Cookies
	if r := b.Get("/networks"); r.Status != http.StatusSeeOther {
		t.Errorf("acme's console session opened the home page: %d", r.Status)
	}
	home := w.OnHome("ada@example.com")
	acme := w.Browser("acme")
	acme.Cookies = home.Cookies
	if r := acme.Get("/"); r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/signin" {
		t.Errorf("a home session opened acme's console: %d", r.Status)
	}
}

func TestAConsoleSessionIsNoGoodAtHomeEvenForAnAccountNamedLikeAPerson(t *testing.T) {
	w := NewWorld(t)
	ada, err := w.Store.GooglePerson(context.Background(), "sub-ada", "ada@example.com")
	if err != nil {
		t.Fatal(err)
	}
	// Someone on a network whose IRC account happens to be called what
	// ada's ID is.
	w.Store.Save(context.Background(), hashOf("console-token"), platform.Session{Network: "acme", Account: ada.ID, CSRF: "c", Expires: time.Now().Add(time.Hour)})
	b := w.Home()
	b.Cookies["guildnet"] = &http.Cookie{Name: "guildnet", Value: "console-token"}
	if r := b.Get("/networks"); r.Status != http.StatusSeeOther {
		t.Errorf("a console session opened ada's home page: %d", r.Status)
	}
}

func hashOf(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func TestGoogleSignInIsRefusedWhenAnythingDoesntMatch(t *testing.T) {
	cases := map[string]struct {
		claims func(map[string]any)
		state  func(string) string
	}{
		"a made-up state":     {state: func(string) string { return "made-up" }},
		"an unverified email": {claims: func(c map[string]any) { c["email_verified"] = false }},
		"another app's token": {claims: func(c map[string]any) { c["aud"] = "someone-else.apps.googleusercontent.com" }},
		"another issuer":      {claims: func(c map[string]any) { c["iss"] = "https://evil.example" }},
		"an expired token":    {claims: func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() }},
		"a replayed sign-in":  {claims: func(c map[string]any) { c["nonce"] = "someone-elses-nonce" }},
		"no email":            {claims: func(c map[string]any) { delete(c, "email") }},
	}
	for what, c := range cases {
		t.Run(what, func(t *testing.T) {
			w := NewWorld(t)
			w.Google.IDToken = c.claims
			b := w.Home()
			start := b.Get("/signin/google")
			back, _ := url.Parse(w.Google.Approve(start.Header.Get("Location"), "ada@example.com", "sub-ada"))
			if c.state != nil {
				q := back.Query()
				q.Set("state", c.state(q.Get("state")))
				back.RawQuery = q.Encode()
			}
			if r := b.Get(back.RequestURI()); r.Status < 400 {
				t.Errorf("signed in anyway: %d to %q", r.Status, r.Header.Get("Location"))
			}
			if r := b.Get("/networks"); r.Status != http.StatusSeeOther {
				t.Error("a session was started")
			}
		})
	}
}

func TestAGoogleSignInCantBeFinishedInAnotherBrowser(t *testing.T) {
	w := NewWorld(t)
	victim, attacker := w.Home(), w.Home()
	start := attacker.Get("/signin/google")
	back, _ := url.Parse(w.Google.Approve(start.Header.Get("Location"), "mallory@example.com", "sub-mallory"))
	// The attacker gets the victim to open the link that finishes it.
	if r := victim.Get(back.RequestURI()); r.Status < 400 {
		t.Errorf("the victim was signed in as the attacker: %d", r.Status)
	}
}

// Creating a network.

func TestCreatingANetworkSetsItUpAndTakesTheOwnerToItsConsole(t *testing.T) {
	w := NewWorld(t)
	b := w.OnHome("ada@example.com")
	form := b.Get("/networks/new")
	for _, want := range []string{"Solo developer", "Small dev team", "Open-source project", ".guildnet.test"} {
		if !strings.Contains(form.Text(), want) {
			t.Errorf("the create page doesn't offer %q", want)
		}
	}
	r := b.Post("/networks", CreateForm("initech"))
	if r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/networks/initech" {
		t.Fatalf("creating: %d to %q %s", r.Status, r.Header.Get("Location"), r.Body)
	}
	ready := b.WaitReady("initech")
	if !strings.Contains(ready.Text(), "ready") {
		t.Fatalf("initech never got ready:\n%s", ready.Text())
	}

	// The host set it up from the team template, for ada, with the owner's
	// key, and nothing of hers it doesn't need.
	s, _ := w.Host.Spec("initech")
	if s.Template != "team" || s.Owner != "ada" || s.Timezone != "Europe/Paris" || s.Company != "initech" ||
		s.ModelKeys[models.Anthropic] != "sk-ant-api03-test-key-0123456789" || len(s.ModelKeys) != 1 || s.WebOrigin != "http://initech.guildnet.test" {
		t.Errorf("the host was asked for %+v", s)
	}
	// ada's IRC account has the password she chose.
	if _, ok := w.Ergo.Password("initech", "ada"); !ok || !SignsInOverIRC(t, w, "initech", "ada", "correct-horse-battery") {
		t.Error("ada's IRC account doesn't take the password she chose")
	}

	// Opening the console signs her in there, as the owner.
	b.Get("/networks/initech")
	open := b.Post("/networks/initech/open", nil)
	to, _ := url.Parse(open.Header.Get("Location"))
	if open.Status != http.StatusSeeOther || to.Host != "initech.guildnet.test" {
		t.Fatalf("opening the console: %d to %q", open.Status, open.Header.Get("Location"))
	}
	console := w.Browser("initech")
	if r := console.Get(to.RequestURI()); r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/" {
		t.Fatalf("arriving at the console: %d to %q %s", r.Status, r.Header.Get("Location"), r.Body)
	}
	org := console.Get("/")
	for _, want := range []string{"kit", "rey", "pia", "owner"} {
		if !strings.Contains(org.Text(), want) {
			t.Errorf("initech's org chart doesn't show %q:\n%s", want, org.Text())
		}
	}
	// And her password works there too.
	if r := w.Browser("initech").SignIn("ada", "correct-horse-battery"); r.Status != http.StatusSeeOther {
		t.Errorf("signing in to initech with her IRC password: %d", r.Status)
	}
	if list := b.Get("/networks"); !strings.Contains(list.Text(), "initech") {
		t.Error("your networks doesn't list initech")
	}
}

func TestANetworkNeedsAGoodFreeName(t *testing.T) {
	w := NewWorld(t)
	b := w.OnHome("ada@example.com")
	for name, why := range map[string]string{
		"acme":        "taken",
		"www":         "reserved",
		"send":        "reserved", // email's bounce address, send.<domain>, is a name in guildnet.chat's DNS
		"Initech Inc": "letters",
		"x":           "letters",
	} {
		b.Get("/networks/new")
		r := b.Post("/networks", CreateForm(name))
		if r.Status != http.StatusBadRequest || !strings.Contains(strings.ToLower(r.Text()), why) {
			t.Errorf("name %q: %d, want 400 saying %q:\n%s", name, r.Status, why, r.Text())
		}
	}
}

func TestTheCreateFormIsChecked(t *testing.T) {
	w := NewWorld(t)
	b := w.OnHome("ada@example.com")
	for what, change := range map[string]func(url.Values){
		"an unknown template":        func(f url.Values) { f.Set("template", "enterprise") },
		"an unknown timezone":        func(f url.Values) { f.Set("timezone", "Mars/Olympus") },
		"a nick with a space":        func(f url.Values) { f.Set("nick", "ada lovelace") },
		"an agent's nick":            func(f url.Values) { f.Set("nick", "kit") },
		"GuildServ's nick":           func(f url.Values) { f.Set("nick", "GuildServ") },
		"a short password":           func(f url.Values) { f.Set("password", "hunter2") },
		"a password with a space":    func(f url.Values) { f.Set("password", "correct horse battery") },
		"a key that isn't one":       func(f url.Values) { f.Set("model_key", "not a key") },
		"an unknown provider":        func(f url.Values) { f.Set("provider", "skynet") },
		"a key the provider refuses": func(f url.Values) { f.Set("model_key", "sk-ant-api03-refused-0123456789") },
	} {
		b.Get("/networks/new")
		f := CreateForm("initech")
		change(f)
		if r := b.Post("/networks", f); r.Status != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", what, r.Status)
		}
	}
	if _, asked := w.Host.Spec("initech"); asked {
		t.Error("the host was asked to create a network from a bad form")
	}
}

func TestInTheBetaEachPersonCreatesOneNetwork(t *testing.T) {
	w := NewWorld(t)
	b := w.OnHome("ada@example.com")
	b.Get("/networks/new")
	b.Post("/networks", CreateForm("initech"))
	b.WaitReady("initech")
	b.Get("/networks/new")
	if r := b.Post("/networks", CreateForm("initrode")); r.Status != http.StatusForbidden {
		t.Errorf("a second network: %d, want 403", r.Status)
	}
}

func TestAFailedSetupCanBeTriedAgain(t *testing.T) {
	w := NewWorld(t)
	w.Host.FailWith(ErrDiskFull)
	b := w.OnHome("ada@example.com")
	b.Get("/networks/new")
	b.Post("/networks", CreateForm("initech"))
	failed := b.WaitReady("initech")
	if !strings.Contains(failed.Text(), "didn't finish") || strings.Contains(failed.Text(), "no space left") {
		t.Fatalf("a failed setup should say so without the machine's details:\n%s", failed.Text())
	}
	w.Host.FailWith(nil)
	r := b.Post("/networks/initech/retry", url.Values{"password": {"correct-horse-battery"}})
	if r.Status != http.StatusSeeOther {
		t.Fatalf("trying again: %d %s", r.Status, r.Body)
	}
	if ready := b.WaitReady("initech"); !strings.Contains(ready.Text(), "ready") {
		t.Errorf("after trying again:\n%s", ready.Text())
	}
}

func TestOnlyTheOwnerSeesANetworksSetup(t *testing.T) {
	w := NewWorld(t)
	ada := w.OnHome("ada@example.com")
	ada.Get("/networks/new")
	ada.Post("/networks", CreateForm("initech"))
	ada.WaitReady("initech")
	bob := w.OnHome("bob@example.com")
	for _, path := range []string{"/networks/initech", "/networks/acme"} {
		if r := bob.Get(path); r.Status != http.StatusNotFound {
			t.Errorf("bob opened %s: %d", path, r.Status)
		}
	}
	bob.Get("/networks")
	if r := bob.Post("/networks/initech/open", nil); r.Status != http.StatusNotFound {
		t.Errorf("bob opened initech's console: %d", r.Status)
	}
}

func TestAConsoleHandoffWorksOnceOnItsOwnNetworkAndSoon(t *testing.T) {
	w := NewWorld(t)
	b := w.OnHome("ada@example.com")
	b.Get("/networks/new")
	b.Post("/networks", CreateForm("initech"))
	b.WaitReady("initech")
	handoff := func() string {
		b.Get("/networks/initech")
		u, _ := url.Parse(b.Post("/networks/initech/open", nil).Header.Get("Location"))
		return u.RequestURI()
	}

	link := handoff()
	w.Browser("initech").Get(link)
	if r := w.Browser("initech").Get(link); r.Status == http.StatusSeeOther && r.Header.Get("Location") == "/" {
		t.Error("the same link signed in twice")
	}
	if r := w.Browser("acme").Get(handoff()); r.Status == http.StatusSeeOther && r.Header.Get("Location") == "/" {
		t.Error("initech's link signed in to acme")
	}
	link = handoff()
	w.Clock.Advance(2 * time.Minute)
	if r := w.Browser("initech").Get(link); r.Status == http.StatusSeeOther && r.Header.Get("Location") == "/" {
		t.Error("a two-minute-old link still signed in")
	}
}

// Browsers hold a form's redirect to the page's form-action policy, so the
// network's page lets its "Open" form go on to that network, and only that.
func TestANetworksPageMayHandItsOwnerOnToItsConsoleOnly(t *testing.T) {
	w := NewWorld(t)
	b := w.OnHome("ada@example.com")
	b.Get("/networks/new")
	b.Post("/networks", CreateForm("initech"))
	b.WaitReady("initech")
	policy := b.Get("/networks/initech").Header.Get("Content-Security-Policy")
	var formAction string
	for _, d := range strings.Split(policy, ";") {
		if f := strings.Fields(d); len(f) > 0 && f[0] == "form-action" {
			formAction = strings.Join(f[1:], " ")
		}
	}
	if formAction != "'self' http://initech.guildnet.test" {
		t.Errorf("form-action = %q, want this site and initech's console only", formAction)
	}
	if policy := b.Get("/networks").Header.Get("Content-Security-Policy"); !strings.Contains(policy, "form-action 'self';") {
		t.Errorf("other pages' policy: %q", policy)
	}
}
