// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	. "github.com/guildnet-chat/guildnet/platformtest"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Agents that run outside GuildNet: hired from the console with a token
// shown once, and their program reaches its network through the platform
// at https://<network>.guildnet.chat/agents/v1, with that token.

func TestHiringAnAgentThatRunsOutsideShowsItsTokenOnce(t *testing.T) {
	w := NewWorld(t)
	ada, page, _ := HireOutside(t, w)

	for _, want := range []string{"acme.guildnet.test/agents/v1", "rex", "only time"} {
		if !strings.Contains(page.Text(), want) {
			t.Errorf("the page should say %q", want)
		}
	}
	if cc := page.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control %q: a page with a token mustn't be kept", cc)
	}
	if again := ada.Get("/staff/rex"); AgentToken.MatchString(again.Body) {
		t.Error("rex's page shows the token again")
	}
	if !strings.Contains(ada.Get("/?add=1").Body, `value="outside"`) {
		t.Error("the hire form should offer running outside GuildNet")
	}
}

// Where an agent works comes before its model: for an agent outside
// GuildNet, its model is what it may use (that tier and cheaper) through
// the model gateway, and its budget what it may spend there. Effort is its
// own program's to choose, so choosing outside turns it off (forms.js, on
// anything marked data-not-outside).
func TestTheHireFormAsksWhereAnAgentWorksBeforeItsModel(t *testing.T) {
	w := NewWorld(t)
	ada := w.Browser("acme").SignedIn("ada")
	page := ada.Get("/?add=1").Body

	works, model := strings.Index(page, `name="platform"`), strings.Index(page, `name="model"`)
	if works < 0 || model < 0 || works > model {
		t.Errorf("Works at (at %d) should come before Model (at %d)", works, model)
	}
	for _, box := range []string{"Model", "Daily budget"} {
		if regexp.MustCompile(`<label[^>]*data-not-outside[^>]*>` + box).MatchString(page) {
			t.Errorf("the %s box counts for outside agents too (the model gateway), so it shouldn't be turned off", box)
		}
	}
	if !regexp.MustCompile(`<label[^>]*data-not-outside[^>]*>Effort`).MatchString(ada.Get("/staff/kai").Body) {
		t.Error("an agent's Effort box should be marked to be turned off for outside agents")
	}
	if !regexp.MustCompile(`<script src="/static/forms.js\?v=[0-9a-f]+" defer></script>`).MatchString(page) {
		t.Error("the console should load forms.js")
	}
	if js := w.Browser("acme").Get("/static/forms.js"); js.Status != http.StatusOK || !strings.Contains(js.Body, "data-not-outside") {
		t.Errorf("forms.js: %d", js.Status)
	}
}

func TestAnOutsideAgentsProgramReachesItsNetworkThroughThePlatform(t *testing.T) {
	w := NewWorld(t)
	ada, _, token := HireOutside(t, w)

	me := w.Program("acme", token, "GET", "/agents/v1/me", nil)
	if me.Status != http.StatusOK || !strings.Contains(me.Body, `"nick":"rex"`) {
		t.Fatalf("/me: %d %s", me.Status, me.Body)
	}
	if r := w.Program("acme", token, "GET", "/agents/v1/inbox?wait=0", nil); r.Status != http.StatusOK || !strings.Contains(r.Body, `"wakes":[]`) {
		t.Errorf("an empty inbox: %d %s", r.Status, r.Body)
	}
	for name, tok := range map[string]string{"no token": "", "a made-up token": "gna_made-up-0123456789abcdef", "a person's": "not-a-token"} {
		if r := w.Program("acme", tok, "GET", "/agents/v1/me", nil); r.Status != http.StatusUnauthorized {
			t.Errorf("%s: %d", name, r.Status)
		}
	}
	if r := w.Program("initech", token, "GET", "/agents/v1/me", nil); r.Status == http.StatusOK {
		t.Error("rex's token works on another network")
	}
	if page := ada.Get("/staff/rex"); !strings.Contains(page.Text(), "It's connected") {
		t.Error("rex's page should say its program is connected")
	}
}

// Agent frameworks reach GuildNet's tools over MCP, through the platform,
// the way they'd reach any remote MCP server.
func TestAnOutsideAgentsMCPWorksThroughThePlatform(t *testing.T) {
	w := NewWorld(t)
	_, _, token := HireOutside(t, w)
	client := mcp.NewClient(&mcp.Implementation{Name: "a-langchain-agent", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             w.Server.URL + "/agents/v1/mcp",
		HTTPClient:           &http.Client{Transport: AsNetwork{Host: "acme." + BaseDomain, Token: token}},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("connecting over MCP: %v", err)
	}
	defer session.Close()

	list, err := session.ListTools(context.Background(), nil)
	if err != nil || len(list.Tools) == 0 {
		t.Fatalf("listing tools: %v", err)
	}
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_context", Arguments: map[string]any{"where": "#engineering"}})
	if err != nil || res.IsError {
		t.Errorf("get_context: %v %+v", err, res)
	}
}

// Each token gets 60 calls a minute (checking the inbox aside), so a
// runaway program can't flood its network.
func TestAnOutsideAgentsCallsAreRateLimited(t *testing.T) {
	w := NewWorld(t)
	_, _, token := HireOutside(t, w)

	var last Response
	for i := 0; i < 61; i++ {
		last = w.Program("acme", token, "GET", "/agents/v1/me", nil)
	}

	if last.Status != http.StatusTooManyRequests || last.Header.Get("Retry-After") == "" {
		t.Errorf("the 61st call in a minute: %d, Retry-After %q", last.Status, last.Header.Get("Retry-After"))
	}
	if r := w.Program("acme", token, "GET", "/agents/v1/inbox?wait=0", nil); r.Status != http.StatusOK {
		t.Errorf("checking the inbox isn't limited that way: %d", r.Status)
	}
}

// From an outside agent's page, its work can be sent to its own address (a
// webhook) instead of its inbox: the signing secret is shown once, and the
// page says where its work goes.
func TestAnOutsideAgentsWorkCanBeSentToItsOwnAddress(t *testing.T) {
	w := NewWorld(t)
	ada, _, _ := HireOutside(t, w)
	if page := ada.Get("/staff/rex"); !strings.Contains(page.Text(), "checks its inbox") || !strings.Contains(page.Body, `name="webhook_url"`) {
		t.Fatalf("rex's page should say it checks its inbox, and offer a webhook:\n%s", page.Text())
	}

	r := ada.Post("/staff/rex/webhook", url.Values{"webhook_url": {"https://agents.example.com/rex"}})

	if r.Status != http.StatusOK || !regexp.MustCompile(`whsec_[A-Za-z0-9+/=]{40,}`).MatchString(r.Text()) || !strings.Contains(r.Text(), "only time") {
		t.Fatalf("setting a webhook: %d\n%s", r.Status, r.Text())
	}
	page := ada.Get("/staff/rex")
	if !strings.Contains(page.Text(), "https://agents.example.com/rex") || strings.Contains(page.Body, "whsec_") {
		t.Errorf("rex's page should show where its work goes, but not the secret:\n%s", page.Text())
	}
	if bad := ada.Post("/staff/rex/webhook", url.Values{"webhook_url": {"http://10.0.0.5/hook"}}); bad.Status != http.StatusBadRequest {
		t.Errorf("an inside address: %d", bad.Status)
	}
	ada.Get("/staff/rex")
	ada.Post("/staff/rex/webhook", url.Values{"webhook_url": {""}})
	if again := ada.Get("/staff/rex"); !strings.Contains(again.Text(), "checks its inbox") {
		t.Error("clearing the address should put rex back on its inbox")
	}
}

// A new token, from the agent's page, is shown once, and the old one stops
// working at once.
func TestAnOutsideAgentGetsANewTokenFromItsPage(t *testing.T) {
	w := NewWorld(t)
	ada, _, old := HireOutside(t, w)
	ada.Get("/staff/rex")

	r := ada.Post("/staff/rex/token", nil)

	fresh := AgentToken.FindString(r.Body)
	if r.Status != http.StatusOK || fresh == "" || fresh == old {
		t.Fatalf("a new token: %d %q", r.Status, fresh)
	}
	if w.Program("acme", old, "GET", "/agents/v1/me", nil).Status != http.StatusUnauthorized {
		t.Error("the old token still works")
	}
	if w.Program("acme", fresh, "GET", "/agents/v1/me", nil).Status != http.StatusOK {
		t.Error("the new token doesn't work")
	}
}

// An outside agent's page shows its wake-ups today and lets them be capped.
func TestAnOutsideAgentsWakeUpsAreSetFromItsPage(t *testing.T) {
	w := NewWorld(t)
	ada, _, _ := HireOutside(t, w)
	page := ada.Get("/staff/rex")
	if !strings.Contains(page.Text(), "0 of 100") || !strings.Contains(page.Body, `name="daily_wakes"`) {
		t.Fatalf("rex's page should show its wake-ups today, and a box to cap them:\n%s", page.Text())
	}

	ada.Post("/staff/rex", url.Values{"daily_wakes": {"25"}})

	if again := ada.Get("/staff/rex"); !strings.Contains(again.Text(), "0 of 25") {
		t.Error("rex's cap should now be 25")
	}
}
