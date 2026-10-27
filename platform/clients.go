// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

var httpClient = &http.Client{Timeout: 30 * time.Second}

// checkPassword asks a network's Ergo whether account's password is right.
func checkPassword(ctx context.Context, n Network, account, password string) (bool, error) {
	body, _ := json.Marshal(map[string]string{"accountName": account, "passphrase": password})
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(n.ErgoURL, "/")+"/v1/check_auth", bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+n.ErgoToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("the IRC server answered %s", resp.Status)
	}
	var out struct {
		Success bool `json:"success"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, err
	}
	return out.Success, nil
}

// ergoCall posts to a network's Ergo API.
func ergoCall(ctx context.Context, n Network, path string, in, out any) error {
	body, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(n.ErgoURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+n.ErgoToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the IRC server answered %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ergoStats is what a network's Ergo says of itself: how many are connected
// (people and agents), and how many channels there are.
type ergoStats struct {
	Success bool `json:"success"`
	Users   struct {
		Total int `json:"total"`
	} `json:"users"`
	Channels int `json:"channels"`
}

// ergoStatus is how a network's Ergo is, if it's up and answering.
func ergoStatus(ctx context.Context, n Network) (ergoStats, error) {
	var out ergoStats
	if err := ergoCall(ctx, n, "/v1/status", map[string]string{}, &out); err != nil {
		return out, err
	}
	if !out.Success {
		return out, errors.New("the IRC server isn't ready")
	}
	return out, nil
}

// errAccountExists means the account is there with another password.
var errAccountExists = errors.New("that account exists with another password")

// createAccount makes a new account on a network's Ergo; errAccountExists
// if the name is taken, whatever its password.
func createAccount(ctx context.Context, n Network, account, password string) error {
	var out struct {
		Success   bool   `json:"success"`
		ErrorCode string `json:"errorCode"`
	}
	if err := ergoCall(ctx, n, "/v1/ns/saregister", map[string]string{"accountName": account, "passphrase": password}, &out); err != nil {
		return err
	}
	switch {
	case out.Success:
		return nil
	case out.ErrorCode == "ACCOUNT_EXISTS":
		return errAccountExists
	}
	return fmt.Errorf("making account %s: %s", account, out.ErrorCode)
}

// setPassword changes an account's password on a network's Ergo.
func setPassword(ctx context.Context, n Network, account, password string) error {
	var out struct {
		Success   bool   `json:"success"`
		ErrorCode string `json:"errorCode"`
	}
	if err := ergoCall(ctx, n, "/v1/ns/passwd", map[string]string{"accountName": account, "passphrase": password}, &out); err != nil {
		return err
	}
	if !out.Success {
		return fmt.Errorf("setting %s's password: %s", account, out.ErrorCode)
	}
	return nil
}

// clearErgoPassword leaves account on a network's Ergo with no password:
// the console keeps it.
func clearErgoPassword(ctx context.Context, n Network, account string) error {
	var out struct {
		Success   bool   `json:"success"`
		ErrorCode string `json:"errorCode"`
	}
	if err := ergoCall(ctx, n, "/v1/ns/passwd", map[string]string{"accountName": account, "passphrase": ""}, &out); err != nil {
		return err
	}
	if !out.Success {
		return fmt.Errorf("clearing %s's password: %s", account, out.ErrorCode)
	}
	return nil
}

// runner calls one network's runner API as one person.
type runner struct {
	n     Network
	actor string
}

// apiError is the runner turning something down, with its reason.
type apiError struct {
	Status int
	Msg    string
}

func (e *apiError) Error() string { return e.Msg }

// statusOf is the HTTP status an error should be shown with.
func statusOf(err error) int {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Status
	}
	return http.StatusBadGateway
}

// messageOf is what to tell the person about err.
func messageOf(err error) string {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Msg
	}
	return "The network didn't answer. Try again in a moment."
}

func (r runner) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(r.n.RunnerURL, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.n.RunnerToken)
	req.Header.Set("GuildNet-Actor", r.actor)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return &apiError{Status: resp.StatusCode, Msg: e.Error}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// What the runner API returns.

type orgChart struct {
	Owner  string        `json:"owner"`
	People []chartPerson `json:"people"`
	Teams  []team        `json:"teams"`
}

type chartPerson struct {
	Name      string   `json:"name"`
	Kind      string   `json:"kind"` // human, guest, or agent
	Owner     bool     `json:"owner"`
	Role      string   `json:"role"`
	Team      string   `json:"team"`
	ReportsTo string   `json:"reports_to"`
	Access    []string `json:"access"`
}

type team struct {
	Name          string   `json:"name"`
	Channel       string   `json:"channel"`
	Leads         []string `json:"leads"`
	Parent        string   `json:"parent"`
	Focus         string   `json:"focus"`
	Repos         []string `json:"repos"`
	Connectors    []string `json:"connectors"`
	Chatter       string   `json:"chatter"`
	Private       bool     `json:"private"`
	MonthlyBudget float64  `json:"monthly_budget"`
}

// LedBy reports whether name is one of the team's leads.
func (t team) LedBy(name string) bool {
	return slices.ContainsFunc(t.Leads, func(l string) bool { return strings.EqualFold(l, name) })
}

type agentProfile struct {
	Nick      string `json:"nick"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	Team      string `json:"team"`
	ReportsTo string `json:"reports_to"`
	Model     string `json:"model"`
	// ModelTitle and ModelPricing are its model for people.
	ModelTitle   string   `json:"model_title"`
	ModelPricing string   `json:"model_pricing"`
	Effort       string   `json:"effort"`
	Budget       float64  `json:"budget"`
	Channels     []string `json:"channels"`
	Platform     string   `json:"platform"`
	Prompt       *string  `json:"prompt"`
	Notes        []string `json:"notes"`
	// An agent outside GuildNet: whether its program is checking in, when
	// it last did, and its wake-ups today and a day.
	Connected  *bool      `json:"connected"`
	LastSeen   *time.Time `json:"last_seen"`
	DailyWakes int        `json:"daily_wakes"`
	WakesToday *int       `json:"wakes_today"`
	// How an outside agent gets its work: "inbox", or "webhook" (sent to
	// WebhookURL; LastDelivery is how the last call went).
	GetsWork     string `json:"gets_work"`
	WebhookURL   string `json:"webhook_url"`
	LastDelivery *struct {
		At  time.Time `json:"at"`
		OK  bool      `json:"ok"`
		Err string    `json:"error"`
	} `json:"last_delivery"`
}

// IsConnected says whether an outside agent's program is checking in.
func (a agentProfile) IsConnected() bool { return a.Connected != nil && *a.Connected }

// Wakes is how many times an outside agent has been woken today.
func (a agentProfile) Wakes() int {
	if a.WakesToday == nil {
		return 0
	}
	return *a.WakesToday
}

// ModelLabel is its model as the console shows it: "Claude Sonnet 5.5 · $2
// in, $10 out per million tokens".
func (a agentProfile) ModelLabel() string {
	return modelLabel(cmp.Or(a.ModelTitle, a.Model), a.ModelPricing)
}

func modelLabel(title, pricing string) string {
	if pricing == "" {
		return title
	}
	return title + " · " + pricing
}

type teamBudget struct {
	Team          string  `json:"team"`
	MonthlyBudget float64 `json:"monthly_budget"`
	Commitments   []struct {
		Name    string   `json:"name"`
		Kind    string   `json:"kind"`
		Monthly *float64 `json:"monthly"`
	} `json:"commitments"`
	Committed *float64 `json:"committed"`
	Remaining *float64 `json:"remaining"`
}

type approval struct {
	ID        int64           `json:"id"`
	Agent     string          `json:"agent"`
	Approver  string          `json:"approver"`
	Tool      string          `json:"tool"`
	Summary   string          `json:"summary"`
	Input     json.RawMessage `json:"input"` // the call's whole input, compact
	Channel   string          `json:"channel"`
	Status    string          `json:"status"`
	ExpiresAt time.Time       `json:"expires_at"`
	Next      string          `json:"next"`     // who it moves to if nobody decides
	MovesAt   time.Time       `json:"moves_at"` // and when
}

// OwnWords says the approval is an outside agent's request in its own words
// (request_approval): nothing GuildNet checked is pinned to it.
func (a approval) OwnWords() bool { return a.Tool == "request_approval" }

// Brief is its summary without the start of the call's input that IRC gets
// (the page shows all of it).
func (a approval) Brief() string {
	if len(a.Input) == 0 {
		return a.Summary
	}
	brief, _, _ := strings.Cut(a.Summary, "; In full (sha256 ")
	return brief
}

// InFull is the call's whole input, pretty-printed; "" for none.
func (a approval) InFull() string {
	var b bytes.Buffer
	if len(a.Input) == 0 || json.Indent(&b, a.Input, "", "  ") != nil {
		return ""
	}
	return b.String()
}

// InFullSum is the start of the input's SHA-256, as the summary names it.
func (a approval) InFullSum() string {
	s := sha256.Sum256(a.Input)
	return hex.EncodeToString(s[:6])
}

// me is what the signed-in person may do, as the runner says (GET /v1/me):
// for each ability, whether across the whole org, and in which teams and
// for whom. The console offers only what will work; the runner still
// checks every change.
type me struct {
	Account   string           `json:"account"`
	Owner     bool             `json:"owner"`
	Guest     bool             `json:"guest"`
	Access    []string         `json:"access"`
	Abilities map[string]reach `json:"abilities"`
	// The runner's word on whether they must use two-step sign-in
	// (mustUseTwoStep); nil from a runner from before it said.
	MustTwoStep *bool `json:"must_two_step"`
}

type reach struct {
	Org    bool     `json:"org"`
	Teams  []string `json:"teams"`
	People []string `json:"people"`
}

// anywhere reports whether they may use ability anywhere at all.
func (m me) anywhere(ability string) bool {
	x := m.Abilities[ability]
	return m.Owner || x.Org || len(x.Teams) > 0 || len(x.People) > 0
}

// inTeam reports whether they may use ability in team ("" for the whole
// org).
func (m me) inTeam(ability, team string) bool {
	x := m.Abilities[ability]
	if m.Owner || x.Org {
		return true
	}
	return team != "" && slices.ContainsFunc(x.Teams, func(t string) bool { return strings.EqualFold(t, team) })
}

// lacks are the abilities them holds somewhere m doesn't, sorted.
func (m me) lacks(them me) []string {
	if m.Owner {
		return nil
	}
	var out []string
	for ability, theirs := range them.Abilities {
		mine := m.Abilities[ability]
		if mine.Org {
			continue
		}
		if theirs.Org || !within(theirs.Teams, mine.Teams) || !within(theirs.People, mine.People) {
			out = append(out, ability)
		}
	}
	slices.Sort(out)
	return out
}

// within reports whether every name in some is in all, ignoring case.
func within(some, all []string) bool {
	for _, s := range some {
		if !slices.ContainsFunc(all, func(a string) bool { return strings.EqualFold(a, s) }) {
			return false
		}
	}
	return true
}

// forPerson reports whether they may use ability for person.
func (m me) forPerson(ability, person string) bool {
	return slices.ContainsFunc(m.Abilities[ability].People, func(p string) bool { return strings.EqualFold(p, person) })
}

// teams are the teams they may use ability in, in the order given.
func (m me) teams(ability string, all []team) []string {
	var out []string
	for _, t := range all {
		if m.inTeam(ability, t.Name) {
			out = append(out, t.Name)
		}
	}
	return out
}

func (p *Platform) loadMe(r *http.Request) (me, error) {
	// A page asks several times (the sidebar, two-step's check, the page
	// itself): once is enough while showing one. Not after a change, which
	// may have changed what they may do.
	memo, _ := r.Context().Value(meKey).(*meMemo)
	if memo == nil || r.Method != http.MethodGet {
		var m me
		err := p.runner(r).call(r.Context(), "GET", "/v1/me", nil, &m)
		return m, err
	}
	memo.mu.Lock()
	defer memo.mu.Unlock()
	if !memo.done {
		memo.err = p.runner(r).call(r.Context(), "GET", "/v1/me", nil, &memo.m)
		memo.done = memo.err == nil
	}
	return memo.m, memo.err
}

// meMemo is who's signed in, as their network's runner said, for the
// request being answered.
type meMemo struct {
	mu   sync.Mutex
	done bool
	m    me
	err  error
}

type meKeyType struct{}

var meKey meKeyType

type accessLevel struct {
	Name      string   `json:"name"`
	Abilities []string `json:"abilities"`
	Scope     string   `json:"scope"`
	Teams     []string `json:"teams"`
	Preset    bool     `json:"preset"`
	Describe  string   `json:"describe"`
}

type accessLevels struct {
	Levels    []accessLevel `json:"levels"`
	Abilities []string      `json:"abilities"`
	Scopes    []string      `json:"scopes"`
}

type logEntry struct {
	ID     int64     `json:"id"`
	At     time.Time `json:"at"`
	Actor  string    `json:"actor"`
	Action string    `json:"action"`
	Target string    `json:"target"`
	Team   string    `json:"team"`
	Detail string    `json:"detail"`
}

type goal struct {
	ID      int64  `json:"id"`
	Team    string `json:"team"`
	Text    string `json:"text"`
	Measure string `json:"measure"`
	Target  string `json:"target"`
	Current string `json:"current"`
	Serves  int64  `json:"serves"`
}

type company struct {
	Description string `json:"description"`
	Goals       []goal `json:"goals"`
	GuestsAsk   bool   `json:"guests_ask"` // guests may have agents work for them
}

type member struct {
	Account     string   `json:"account"`
	Name        string   `json:"name"`
	Role        string   `json:"role"`
	Team        string   `json:"team"`
	ReportsTo   string   `json:"reports_to"`
	Guest       bool     `json:"guest"`
	MonthlyCost *float64 `json:"monthly_cost"`
}

// Works reports whether the team works on repo, for the form.
func (t team) Works(repo string) bool { return slices.Contains(t.Repos, repo) }

// Uses reports whether the team uses the connector, for the form.
func (t team) Uses(connector string) bool { return slices.Contains(t.Connectors, connector) }
