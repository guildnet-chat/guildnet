// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"context"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/guildnet-chat/guildnet/platformtest"
)

// Signing in.

func TestSigningInWithYourIRCPasswordOpensTheConsole(t *testing.T) {
	w := NewWorld(t)
	b := w.Browser("acme")

	if r := b.Get("/"); r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/signin" {
		t.Fatalf("before signing in: %d to %q, want a redirect to /signin", r.Status, r.Header.Get("Location"))
	}
	if r := b.SignIn("ada", "ada-pw"); r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/" {
		t.Fatalf("signing in: %d to %q", r.Status, r.Header.Get("Location"))
	}
	r := b.Get("/")

	if r.Status != http.StatusOK {
		t.Fatalf("the console: %d", r.Status)
	}
	for _, want := range []string{"acme", "ada", "vera", "kai", "Engineering", "Sales"} {
		if !strings.Contains(r.Body, want) {
			t.Errorf("the org chart should show %q", want)
		}
	}
}

func TestAgentsCarryAnAIBadge(t *testing.T) {
	w := NewWorld(t)
	b := w.Browser("acme").SignedIn("ada")

	r := b.Get("/")

	if !strings.Contains(r.Body, `kai</a> <span class="badge ai">AI</span>`) {
		t.Errorf("kai should carry an AI badge:\n%s", r.Body)
	}
	if strings.Contains(r.Body, `ada</span> <span class="badge ai">`) || strings.Contains(r.Body, `cy</span> <span class="badge ai">`) {
		t.Error("people shouldn't carry the AI badge")
	}
}

func TestAWrongPasswordIsRefused(t *testing.T) {
	w := NewWorld(t)
	b := w.Browser("acme")

	r := b.SignIn("ada", "guess")

	if r.Status != http.StatusUnauthorized || !strings.Contains(r.Text(), "didn't match") {
		t.Errorf("status %d, body %q; want 401 saying the password didn't match", r.Status, r.Body)
	}
	if b.Get("/").Status != http.StatusSeeOther {
		t.Error("a wrong password mustn't sign anyone in")
	}
}

func TestSomeoneWithAnIRCAccountButNotInTheOrgCantSignIn(t *testing.T) {
	w := NewWorld(t)
	b := w.Browser("acme")

	r := b.SignIn("zed", "zed-pw")

	if r.Status != http.StatusForbidden || !strings.Contains(r.Text(), "isn't in acme's org") {
		t.Errorf("status %d, body %q; want 403 saying zed isn't in the org", r.Status, r.Body)
	}
	if b.Get("/").Status != http.StatusSeeOther {
		t.Error("zed mustn't be signed in")
	}
}

func TestRepeatedWrongPasswordsAreSlowedDown(t *testing.T) {
	w := NewWorld(t)
	b := w.Browser("acme")
	for range 5 {
		b.SignIn("ada", "guess")
	}

	r := b.SignIn("ada", "ada-pw")

	if r.Status != http.StatusTooManyRequests || !strings.Contains(r.Body, "Too many") {
		t.Errorf("status %d; want 429 after five wrong passwords, even with the right one", r.Status)
	}
}

// Guesses sent all at once are counted like guesses sent one by one.
func TestPasswordsSentAtOnceAreStillLimited(t *testing.T) {
	w := NewWorld(t)
	var guessers []*Browser
	for range 50 {
		b := w.Browser("acme")
		b.Get("/signin")
		guessers = append(guessers, b)
	}
	var checked atomic.Int64
	var wg sync.WaitGroup
	for _, b := range guessers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r := b.Post("/signin", url.Values{"account": {"ada"}, "password": {"guess"}}); r.Status == http.StatusUnauthorized {
				checked.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := checked.Load(); n > 5 {
		t.Errorf("%d of 50 passwords sent at once were checked; the limit is 5", n)
	}
}

func TestSigningOutEndsTheSession(t *testing.T) {
	w := NewWorld(t)
	b := w.Browser("acme").SignedIn("ada")
	b.Get("/")
	stolen := maps.Clone(b.Cookies) // someone who copied the cookie before sign-out

	b.Post("/signout", nil)

	if b.Get("/").Status != http.StatusSeeOther {
		t.Error("after signing out, the console should ask to sign in again")
	}
	thief := w.Browser("acme")
	thief.Cookies = stolen
	if thief.Get("/").Status != http.StatusSeeOther {
		t.Error("a copied cookie must stop working once its session ends")
	}
}

// Companies are kept apart.

func TestASessionForOneNetworkIsNothingOnAnother(t *testing.T) {
	w := NewWorld(t)
	acme := w.Browser("acme").SignedIn("ada")
	globex := w.Browser("globex")
	globex.Cookies = acme.Cookies // as if the cookie were sent to globex's address

	r := globex.Get("/")

	if r.Status != http.StatusSeeOther || strings.Contains(r.Body, "hal") {
		t.Errorf("acme's session on globex: %d; want sent to sign in, seeing nothing", r.Status)
	}
}

func TestSessionCookiesBelongToOneNetworksAddressOnly(t *testing.T) {
	w := NewWorld(t)
	b := w.Browser("acme")

	b.SignIn("ada", "ada-pw")

	if len(b.Cookies) == 0 {
		t.Fatal("no session cookie")
	}
	for _, c := range b.Cookies {
		if c.Domain != "" {
			t.Errorf("cookie %s has Domain=%s; it must be host-only, so no other network's pages get it", c.Name, c.Domain)
		}
		if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
			t.Errorf("cookie %s: HttpOnly %v, SameSite %v, Path %q", c.Name, c.HttpOnly, c.SameSite, c.Path)
		}
	}
}

func TestEachNetworkSignsInItsOwnPeople(t *testing.T) {
	w := NewWorld(t)

	if r := w.Browser("globex").SignIn("ada", "ada-pw"); r.Status == http.StatusSeeOther {
		t.Error("ada is acme's; she mustn't sign in to globex")
	}
	if r := w.Browser("globex").SignIn("gus", "gus-pw"); r.Status != http.StatusSeeOther {
		t.Errorf("gus signing in to his own network: %d", r.Status)
	}
}

func TestAnUnknownNetworkIsNotFound(t *testing.T) {
	w := NewWorld(t)

	if r := w.Browser("nope").Get("/signin"); r.Status != http.StatusNotFound {
		t.Errorf("status %d, want 404", r.Status)
	}
}

// Web basics.

func TestPagesCarryStrictSecurityHeaders(t *testing.T) {
	w := NewWorld(t)

	r := w.Browser("acme").Get("/signin")

	csp := r.Header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "frame-ancestors 'none'", "form-action 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q should include %q", csp, want)
		}
	}
	if strings.Contains(csp, "unsafe") {
		t.Errorf("CSP %q allows something unsafe", csp)
	}
	if r.Header.Get("X-Content-Type-Options") != "nosniff" || r.Header.Get("Referrer-Policy") == "" {
		t.Errorf("headers: %v", r.Header)
	}
}

func TestChangesNeedThePagesToken(t *testing.T) {
	w := NewWorld(t)
	b := w.Browser("acme").SignedIn("ada")
	b.Get("/")

	r := b.Do("POST", "/staff", url.Values{"nick": {"rex"}}) // no token

	if r.Status != http.StatusForbidden {
		t.Errorf("a change without the page's token: %d, want 403", r.Status)
	}
	if _, err := w.Networks["acme"].Store.Agent(context.Background(), "rex"); err == nil {
		t.Error("rex was hired anyway")
	}
}

func TestChangesFromAnotherSiteAreRefused(t *testing.T) {
	w := NewWorld(t)
	b := w.Browser("acme").SignedIn("ada")
	b.Get("/")
	b.Origin = "https://evil.example"

	r := b.Post("/staff", url.Values{"nick": {"rex"}})

	if r.Status != http.StatusForbidden {
		t.Errorf("a change posted from another site: %d, want 403", r.Status)
	}
}

func TestTheConsoleLoadsHtmx4FromItsOwnAddress(t *testing.T) {
	w := NewWorld(t)
	b := w.Browser("acme").SignedIn("ada")

	r := b.Get("/")

	if !strings.Contains(r.Body, `<script src="/static/htmx-4.0.0.min.js" integrity="sha384-BvJpBiO8Kh31EqtJe5DRIeWrHWnCGkwytKs9NKFi86Hhw96dEqdEMzZDeK9iEGTc"`) {
		t.Errorf("the page should load htmx 4.0.0 with its integrity hash")
	}
	js := b.Get("/static/htmx-4.0.0.min.js")
	if js.Status != http.StatusOK || !strings.Contains(js.Header.Get("Content-Type"), "javascript") || len(js.Body) < 30000 {
		t.Errorf("htmx: %d %s, %d bytes", js.Status, js.Header.Get("Content-Type"), len(js.Body))
	}
}

// The console, through the same rules as GuildServ.

func TestHiringFromTheConsole(t *testing.T) {
	w := NewWorld(t)
	b := w.Browser("acme").SignedIn("ada")
	b.Get("/")

	r := b.Post("/staff", url.Values{"nick": {"rex"}, "role": {"QA engineer"}, "team": {"Engineering"}, "budget": {"1.5"}})

	if r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/staff/rex" {
		t.Fatalf("hiring: %d %q %s", r.Status, r.Header.Get("Location"), r.Body)
	}
	p, err := w.Networks["acme"].Store.Agent(context.Background(), "rex")
	if err != nil || p.Role != "QA engineer" || p.DailyBudgetUSD != 1.5 {
		t.Errorf("rex = %+v, %v", p, err)
	}
	if page := b.Get("/staff/rex"); !strings.Contains(page.Body, "QA engineer") {
		t.Error("rex's page should show the role")
	}
}

func TestTheRulesShowUpInTheConsole(t *testing.T) {
	w := NewWorld(t)
	b := w.Browser("acme").SignedIn("bo") // leads Sales, not Engineering
	b.Get("/")

	r := b.Post("/staff", url.Values{"nick": {"rex"}, "team": {"Engineering"}})

	if r.Status != http.StatusForbidden || !strings.Contains(r.Body, "Engineering") {
		t.Errorf("bo hiring into Engineering: %d, want 403 with the reason on the page:\n%s", r.Status, r.Body)
	}
	if _, err := w.Networks["acme"].Store.Agent(context.Background(), "rex"); err == nil {
		t.Error("rex was hired anyway")
	}
}

func TestChangingAnAgentFromTheConsole(t *testing.T) {
	w := NewWorld(t)
	b := w.Browser("acme").SignedIn("ada")
	b.Get("/staff/kai")

	r := b.Post("/staff/kai", url.Values{"model": {"claude-opus-5"}, "role": {"Staff engineer"}})

	if r.Status != http.StatusSeeOther {
		t.Fatalf("saving: %d %s", r.Status, r.Body)
	}
	if p, _ := w.Networks["acme"].Store.Agent(context.Background(), "kai"); p.Model != "claude-opus-5" || p.Role != "Staff engineer" {
		t.Errorf("kai = %+v", p)
	}
}

// Hiring and changing an agent pick from the models the network's providers
// serve, each with what it costs; an agent on an older model keeps it as a
// choice. People who can't change an agent still see its model and price.
func TestTheConsoleOffersTheNetworksModelsWithTheirPrices(t *testing.T) {
	w := NewWorld(t)
	ada := w.Browser("acme").SignedIn("ada")

	org := ada.Get("/?add=1").Body
	for _, want := range []string{`<optgroup label="Fast">`, `<optgroup label="Best">`,
		`<option value="claude-haiku-4-5">Claude Haiku 4.5 · $1 in, $5 out per million tokens</option>`,
		`<option value="claude-opus-5-5">Claude Opus 5.5 · $4 in, $20 out per million tokens</option>`,
		`The role's (or Claude Sonnet 5.5)`} {
		if !strings.Contains(org, want) {
			t.Errorf("the hire form should have %s", want)
		}
	}
	if strings.Contains(org, "openai:") || strings.Contains(org, `value="claude-sonnet-5"`) {
		t.Error("the hire form offers a model without a key here, or an older one")
	}

	kai := ada.Get("/staff/kai").Body
	for _, want := range []string{`<option value="claude-sonnet-5" selected>Claude Sonnet 5 · $2 in, $10 out per million tokens (current)</option>`,
		`<option value="claude-opus-5-5">Claude Opus 5.5 · $4 in, $20 out per million tokens</option>`} {
		if !strings.Contains(kai, want) {
			t.Errorf("kai's page should have %s", want)
		}
	}

	byBo := w.Browser("acme").SignedIn("bo").Get("/staff/kai").Body
	if !strings.Contains(byBo, "Claude Sonnet 5 · $2 in, $10 out per million tokens") {
		t.Error("bo should see kai's model and what it costs")
	}
}

func TestOnlyThoseWhoMayChangeAnAgentGetTheFormAndThePrompt(t *testing.T) {
	w := NewWorld(t)

	byAda := w.Browser("acme").SignedIn("ada").Get("/staff/kai")
	byBo := w.Browser("acme").SignedIn("bo").Get("/staff/kai")

	if !strings.Contains(byAda.Body, `name="prompt"`) {
		t.Error("ada, the owner, should get the edit form with kai's prompt")
	}
	if strings.Contains(byBo.Body, `name="prompt"`) || strings.Contains(byBo.Body, `action="/staff/kai"`) {
		t.Error("bo leads Sales, so kai's page is read-only for him, without the prompt")
	}
}

func TestFiringFromTheConsole(t *testing.T) {
	w := NewWorld(t)
	b := w.Browser("acme").SignedIn("ada")
	b.Get("/staff/kai")

	r := b.Post("/staff/kai/fire", url.Values{"reason": {"budget cuts"}})

	if r.Status != http.StatusSeeOther {
		t.Fatalf("firing: %d %s", r.Status, r.Body)
	}
	if _, err := w.Networks["acme"].Store.Agent(context.Background(), "kai"); err == nil {
		t.Error("kai is still on staff")
	}
}

func TestApprovalsFromTheConsole(t *testing.T) {
	w := NewWorld(t)
	acme := w.Networks["acme"]
	id := acme.RaiseApproval(t, "kai", "ada", "merge PR #17 'Retry webhooks' into main")
	b := w.Browser("acme").SignedIn("ada")

	page := b.Get("/approvals")
	if !strings.Contains(page.Body, "merge PR #17") {
		t.Fatalf("the approval should be listed:\n%s", page.Body)
	}
	r := b.Post("/approvals/"+Itoa(id)+"/deny", url.Values{"reason": {"not before the launch"}})

	if r.Status != http.StatusSeeOther {
		t.Fatalf("denying: %d %s", r.Status, r.Body)
	}
	a, _ := acme.Store.Approval(context.Background(), id)
	if a.Status != "denied" || a.DecidedBy != "ada" || a.Reason != "not before the launch" {
		t.Errorf("approval = %+v", a)
	}
	WaitFor(t, "kai to hear the reason", func() bool {
		got := acme.Brain.Resumed()
		return len(got) == 1 && got[0] == "kai deny: not before the launch"
	})
}

// What an outside agent asks in its own words is shown as its own, on every
// page that lists approvals; a tool's approval is the tool's description.
func TestAnAgentsOwnWordsAreShownAsTheirs(t *testing.T) {
	w := NewWorld(t)
	acme := w.Networks["acme"]
	acme.RaiseOwnWordsApproval(t, "kai", "ada", "merge PR #41 (a typo)")
	acme.RaiseApproval(t, "kai", "ada", "merge PR #17 into main")
	b := w.Browser("acme").SignedIn("ada")

	for _, path := range []string{"/approvals", "/"} {
		text := b.Get(path).Text()
		if !strings.Contains(text, "asks, in its own words, to “merge PR #41 (a typo)”") {
			t.Errorf("%s doesn't mark kai's own words as its own:\n%s", path, text)
		}
		if !strings.Contains(text, "wants to merge PR #17 into main") {
			t.Errorf("%s doesn't show the tool's approval as before:\n%s", path, text)
		}
	}
}

func TestSomeoneWhoCantDecideDoesntSeeTheApproval(t *testing.T) {
	w := NewWorld(t)
	w.Networks["acme"].RaiseApproval(t, "kai", "ada", "merge PR #17 into main")

	r := w.Browser("acme").SignedIn("bo").Get("/approvals")

	if strings.Contains(r.Body, "merge PR #17") {
		t.Error("bo isn't in kai's reporting line, so shouldn't see its approval")
	}
}

// Teams used to be called lanes; the old address still gets you there.
func TestTheOldLanesPageLeadsToTeams(t *testing.T) {
	w := NewWorld(t)

	r := w.Browser("acme").SignedIn("ada").Get("/lanes")

	if r.Status != http.StatusMovedPermanently || r.Header.Get("Location") != "/teams" {
		t.Errorf("/lanes: %d to %q, want a permanent redirect to /teams", r.Status, r.Header.Get("Location"))
	}
}

func TestTeamsAndBudgetsKeepPeoplesCostsPrivate(t *testing.T) {
	w := NewWorld(t)

	byAda := w.Browser("acme").SignedIn("ada").Get("/teams/Engineering")
	byBo := w.Browser("acme").SignedIn("bo").Get("/teams/Engineering")

	if !strings.Contains(byAda.Body, "$6,000") {
		t.Error("the owner should see what cy costs")
	}
	if !strings.Contains(byBo.Body, "cy") || strings.Contains(byBo.Body, "$6,000") {
		t.Error("bo should see cy is in Engineering, but not what cy costs")
	}
}

func TestASignInLastsAWeek(t *testing.T) {
	w := NewWorld(t)
	b := w.Browser("acme").SignedIn("ada")

	w.Clock.Advance(6 * 24 * time.Hour)
	if b.Get("/").Status != http.StatusOK {
		t.Error("six days on, ada should still be signed in")
	}
	w.Clock.Advance(2 * 24 * time.Hour)
	if b.Get("/").Status != http.StatusSeeOther {
		t.Error("after a week, the session should have ended")
	}
}

func TestWrongPasswordsStopCountingAfterAWhile(t *testing.T) {
	w := NewWorld(t)
	b := w.Browser("acme")
	for range 5 {
		b.SignIn("ada", "guess")
	}
	w.Clock.Advance(16 * time.Minute)

	if r := b.SignIn("ada", "ada-pw"); r.Status != http.StatusSeeOther {
		t.Errorf("16 minutes later the right password should work: %d", r.Status)
	}
}
