// SPDX-License-Identifier: AGPL-3.0-only

package org_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/org"
	"github.com/guildnet-chat/guildnet/pgtest"
)

// The same behaviour is expected of every Store. Postgres runs in a
// throwaway container; its cases skip if Docker isn't running.

var pg *pgtest.Server

func TestMain(m *testing.M) {
	var err error
	pg, err = pgtest.Start()
	if err != nil && !errors.Is(err, pgtest.ErrNoDocker) {
		panic(err)
	}
	code := m.Run()
	if pg != nil {
		pg.Stop()
	}
	os.Exit(code)
}

// newPostgres opens a Postgres store on a fresh database.
func newPostgres(t *testing.T, seed org.Seed) (*org.Postgres, string) {
	t.Helper()
	if pg == nil {
		t.Skip("Docker isn't running; skipping Postgres")
	}
	url, err := pg.NewDatabase()
	if err != nil {
		t.Fatal(err)
	}
	s, err := org.OpenPostgres(context.Background(), url, seed)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, url
}

var stores = map[string]func(t *testing.T, seed org.Seed) org.Store{
	"memory": func(t *testing.T, seed org.Seed) org.Store { return org.NewMemory(seed) },
	"postgres": func(t *testing.T, seed org.Seed) org.Store {
		s, _ := newPostgres(t, seed)
		return s
	},
	// Kept in memory between changes, over Postgres, as runners use it: it
	// must behave just as the store under it.
	"cached": func(t *testing.T, seed org.Seed) org.Store {
		s, _ := newPostgres(t, seed)
		return org.NewCached(s)
	},
}

// eachStore runs the test against every Store.
func eachStore(t *testing.T, seed org.Seed, test func(t *testing.T, s org.Store)) {
	for name, open := range stores {
		t.Run(name, func(t *testing.T) { test(t, open(t, seed)) })
	}
}

var ctx = context.Background()

func kai() config.Persona {
	return config.Persona{Nick: "kai", Name: "Kai Okafor", Role: "Backend engineer", Team: "Engineering",
		ReportsTo: "vera", Model: "claude-sonnet-5", Effort: "medium", DailyBudgetUSD: 1.5,
		Channels: []string{"#general", "#engineering"}, Prompt: "You're the backend engineer.", Platform: "outside", DailyWakes: 40}
}

func TestAHiredAgentIsOnStaffExactlyAsHired(t *testing.T) {
	eachStore(t, org.Seed{}, func(t *testing.T, s org.Store) {
		if err := s.Hire(ctx, kai()); err != nil {
			t.Fatal(err)
		}
		got, err := s.Agent(ctx, "kai")
		if err != nil {
			t.Fatal(err)
		}
		if !samePersona(got, kai()) {
			t.Errorf("got %+v, want %+v", got, kai())
		}
		all, _ := s.Agents(ctx)
		if len(all) != 1 || all[0].Nick != "kai" {
			t.Errorf("Agents = %v, want just kai", all)
		}
	})
}

// An outside agent's token is kept only as a hash: the store says whose it
// is, a new one replaces the old at once, and firing the agent drops it, so
// hiring the same nick again doesn't bring it back.
func TestAnAgentsTokenIsKeptAsAHashAndGoesWhenItsFired(t *testing.T) {
	eachStore(t, org.Seed{Agents: []config.Persona{kai()}}, func(t *testing.T, s org.Store) {
		if err := s.SetAgentToken(ctx, "kai", "hash-1"); err != nil {
			t.Fatal(err)
		}
		if nick, err := s.AgentForToken(ctx, "hash-1"); err != nil || nick != "kai" {
			t.Fatalf("hash-1 is %q's (%v), want kai's", nick, err)
		}
		if err := s.SetAgentToken(ctx, "KAI", "hash-2"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AgentForToken(ctx, "hash-1"); !errors.Is(err, org.ErrNotFound) {
			t.Errorf("the old token still works: %v", err)
		}
		if err := s.SetAgentToken(ctx, "nobody", "hash-3"); !errors.Is(err, org.ErrNotFound) {
			t.Errorf("a token for someone not on staff: %v", err)
		}
		if err := s.Fire(ctx, "kai"); err != nil {
			t.Fatal(err)
		}
		s.Hire(ctx, kai())
		if _, err := s.AgentForToken(ctx, "hash-2"); !errors.Is(err, org.ErrNotFound) {
			t.Errorf("the fired agent's token still works: %v", err)
		}
	})
}

// An outside agent's webhook: where its wake-ups are sent, and the secret
// (sealed by the runner) they're signed with. Clearing it, or firing the
// agent, drops both.
func TestAnAgentsWebhookIsKeptUntilClearedOrFired(t *testing.T) {
	eachStore(t, org.Seed{Agents: []config.Persona{kai()}}, func(t *testing.T, s org.Store) {
		if url, _, err := s.AgentWebhook(ctx, "kai"); err != nil || url != "" {
			t.Fatalf("before one's set: %q, %v", url, err)
		}
		if err := s.SetAgentWebhook(ctx, "KAI", "https://agents.example.com/kai", []byte("sealed-1")); err != nil {
			t.Fatal(err)
		}
		if url, secret, err := s.AgentWebhook(ctx, "kai"); err != nil || url != "https://agents.example.com/kai" || string(secret) != "sealed-1" {
			t.Errorf("kai's webhook: %q %q %v", url, secret, err)
		}
		s.SetAgentWebhook(ctx, "kai", "", nil)
		if url, secret, _ := s.AgentWebhook(ctx, "kai"); url != "" || secret != nil {
			t.Errorf("after clearing: %q %q", url, secret)
		}
		s.SetAgentWebhook(ctx, "kai", "https://agents.example.com/kai", []byte("sealed-2"))
		s.Fire(ctx, "kai")
		s.Hire(ctx, kai())
		if url, _, _ := s.AgentWebhook(ctx, "kai"); url != "" {
			t.Errorf("the fired agent's webhook came back: %q", url)
		}
		if err := s.SetAgentWebhook(ctx, "nobody", "https://x.example.com/", []byte("s")); !errors.Is(err, org.ErrNotFound) {
			t.Errorf("a webhook for someone not on staff: %v", err)
		}
	})
}

func TestNamesAreCaseInsensitive(t *testing.T) {
	eachStore(t, org.Seed{}, func(t *testing.T, s org.Store) {
		p := kai()
		p.Nick = "Kai"
		if err := s.Hire(ctx, p); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Agent(ctx, "KAI"); err != nil {
			t.Errorf("looking up KAI: %v", err)
		}
		p.Nick = "kai"
		if err := s.Hire(ctx, p); !errors.Is(err, org.ErrExists) {
			t.Errorf("hiring kai again: got %v, want ErrExists", err)
		}
	})
}

func TestAgentsAndMembersShareOneSetOfNames(t *testing.T) {
	eachStore(t, org.Seed{}, func(t *testing.T, s org.Store) {
		if err := s.AddMember(ctx, org.Member{Account: "dana", Role: "Head of Sales"}); err != nil {
			t.Fatal(err)
		}
		p := kai()
		p.Nick = "dana"
		if err := s.Hire(ctx, p); !errors.Is(err, org.ErrExists) {
			t.Errorf("hiring an agent named after a member: got %v, want ErrExists", err)
		}
		if err := s.Hire(ctx, kai()); err != nil {
			t.Fatal(err)
		}
		if err := s.AddMember(ctx, org.Member{Account: "KAI"}); !errors.Is(err, org.ErrExists) {
			t.Errorf("adding a member named after an agent: got %v, want ErrExists", err)
		}
	})
}

func TestUpdatingChangesAnAgent(t *testing.T) {
	eachStore(t, org.Seed{}, func(t *testing.T, s org.Store) {
		s.Hire(ctx, kai())
		p := kai()
		p.Model, p.Channels = "claude-opus-5", []string{"#ops"}
		if err := s.Update(ctx, p); err != nil {
			t.Fatal(err)
		}
		got, _ := s.Agent(ctx, "kai")
		if got.Model != "claude-opus-5" || !slices.Equal(got.Channels, []string{"#ops"}) {
			t.Errorf("after update got %+v", got)
		}
		p.Nick = "nobody"
		if err := s.Update(ctx, p); !errors.Is(err, org.ErrNotFound) {
			t.Errorf("updating nobody: got %v, want ErrNotFound", err)
		}
	})
}

func TestFiringRemovesTheAgentAndItsLeadership(t *testing.T) {
	seed := org.Seed{Agents: []config.Persona{kai()}, Teams: []org.Team{{Name: "Engineering", Channel: "#engineering", Leads: []string{"kai"}}}}
	eachStore(t, seed, func(t *testing.T, s org.Store) {
		if err := s.Fire(ctx, "kai"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Agent(ctx, "kai"); !errors.Is(err, org.ErrNotFound) {
			t.Errorf("kai after firing: got %v, want ErrNotFound", err)
		}
		l, _ := s.Team(ctx, "Engineering")
		if len(l.Leads) != 0 {
			t.Errorf("Engineering still led by %q after kai was fired", l.Leads)
		}
		if err := s.Fire(ctx, "kai"); !errors.Is(err, org.ErrNotFound) {
			t.Errorf("firing kai twice: got %v, want ErrNotFound", err)
		}
		if err := s.Hire(ctx, kai()); err != nil {
			t.Errorf("the name kai should be free again: %v", err)
		}
	})
}

func TestMembersCanBeAddedChangedAndRemoved(t *testing.T) {
	seed := org.Seed{Teams: []org.Team{{Name: "Sales", Channel: "#sales", Leads: []string{"eli", "dana"}}}}
	eachStore(t, seed, func(t *testing.T, s org.Store) {
		dana := org.Member{Account: "dana", Name: "Dana", Role: "Head of Sales", Team: "Sales", ReportsTo: "method", MonthlyCost: 6000, Access: []string{"admin", "ops"}}
		if err := s.AddMember(ctx, dana); err != nil {
			t.Fatal(err)
		}
		dana.MonthlyCost, dana.Guest = 6500, true
		if err := s.UpdateMember(ctx, dana); err != nil {
			t.Fatal(err)
		}
		got, _ := s.Member(ctx, "DANA")
		if !reflect.DeepEqual(got, dana) {
			t.Errorf("got %+v, want %+v", got, dana)
		}
		if err := s.RemoveMember(ctx, "dana"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Member(ctx, "dana"); !errors.Is(err, org.ErrNotFound) {
			t.Errorf("dana after removal: got %v, want ErrNotFound", err)
		}
		if l, _ := s.Team(ctx, "Sales"); !slices.Equal(l.Leads, []string{"eli"}) {
			t.Errorf("Sales led by %q after dana was removed, want just eli", l.Leads)
		}
	})
}

func TestTeamsCanBeCreatedAndChanged(t *testing.T) {
	eachStore(t, org.Seed{}, func(t *testing.T, s org.Store) {
		sales := org.Team{Name: "Sales", Channel: "#sales", MonthlyBudget: 500}
		if err := s.CreateTeam(ctx, sales); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateTeam(ctx, org.Team{Name: "sales"}); !errors.Is(err, org.ErrExists) {
			t.Errorf("creating sales twice: got %v, want ErrExists", err)
		}
		s.CreateTeam(ctx, org.Team{Name: "Engineering", Channel: "#engineering"})
		sales.Leads, sales.Parent, sales.Private, sales.MonthlyBudget = []string{"dana", "eli"}, "Engineering", true, 750
		sales.Focus, sales.Repos, sales.Connectors, sales.Chatter = "Win customers who stay", []string{"crm", "site"}, []string{"linear", "notion"}, "quiet"
		if err := s.UpdateTeam(ctx, sales); err != nil {
			t.Fatal(err)
		}
		teams, _ := s.Teams(ctx)
		if len(teams) != 2 || teams[0].Name != "Engineering" || !reflect.DeepEqual(teams[1], sales) {
			t.Errorf("Teams = %+v, want Engineering then the updated Sales", teams)
		}
	})
}

// A team uses its own connectors, or those of the nearest team it's inside
// that has some, as with repos.
func TestATeamsConnectorsAreItsOwnOrItsParents(t *testing.T) {
	s := org.NewMemory(org.Seed{Teams: []org.Team{
		{Name: "Engineering", Connectors: []string{"linear", "sentry"}},
		{Name: "Platform", Parent: "Engineering"},
		{Name: "Docs", Parent: "Engineering", Connectors: []string{"notion"}},
		{Name: "Sales"},
	}})
	for team, want := range map[string][]string{"Engineering": {"linear", "sentry"}, "Platform": {"linear", "sentry"}, "Docs": {"notion"}, "Sales": nil, "": nil} {
		if got := org.ConnectorsFor(ctx, s, team); !reflect.DeepEqual(got, want) {
			t.Errorf("%q's connectors: %v, want %v", team, got, want)
		}
	}
}

// What the network says about each connector tool (its policy, and the
// definition people approved) is kept, and changed in place.
func TestConnectorToolSettingsAreKept(t *testing.T) {
	eachStore(t, org.Seed{}, func(t *testing.T, s org.Store) {
		if got, err := s.ConnectorTools(ctx, "linear"); err != nil || len(got) != 0 {
			t.Fatalf("a new network's: %v, %v", got, err)
		}
		create := org.ConnectorTool{Connector: "linear", Tool: "create_issue", Approved: "h1", Seen: "h1"}
		search := org.ConnectorTool{Connector: "linear", Tool: "search_issues", Approved: "h2", Seen: "h2"}
		page := org.ConnectorTool{Connector: "notion", Tool: "create_page", Seen: "h3"}
		if err := s.SaveConnectorTools(ctx, []org.ConnectorTool{create, search, page}); err != nil {
			t.Fatal(err)
		}
		create.Policy, create.Seen = "deny", "h4"
		if err := s.SaveConnectorTools(ctx, []org.ConnectorTool{create}); err != nil {
			t.Fatal(err)
		}
		got, err := s.ConnectorTools(ctx, "linear")
		if err != nil || !reflect.DeepEqual(got, []org.ConnectorTool{create, search}) {
			t.Errorf("Linear's: %+v, %v; want create_issue changed, then search_issues", got, err)
		}
		if all, _ := s.ConnectorTools(ctx, ""); len(all) != 3 {
			t.Errorf("all: %+v, want 3", all)
		}
	})
}

// Each wake-up's work is kept for the agent's page: newest first, the last
// few dozen per agent, and gone when the agent is fired.
func TestAnAgentsRecentWorkIsKept(t *testing.T) {
	eachStore(t, org.Seed{Agents: []config.Persona{kai(), {Nick: "sam", Name: "Sam", Role: "Support"}}}, func(t *testing.T, s org.Store) {
		at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
		for i := range org.WorkKept + 5 {
			w := org.Work{Agent: "kai", Team: "Engineering", Where: "#engineering", Asker: "method", Asked: fmt.Sprintf("job %d", i),
				Started: at.Add(time.Duration(i) * time.Minute), Finished: at.Add(time.Duration(i)*time.Minute + 30*time.Second),
				Steps:   []org.WorkStep{{At: at, Summary: "reads a.go"}, {At: at, Summary: "opened PR #3", Result: true}},
				Outcome: "answered", Cost: 0.04}
			if err := s.AddWork(ctx, w); err != nil {
				t.Fatal(err)
			}
		}
		s.AddWork(ctx, org.Work{Agent: "sam", Where: "a DM", Started: at, Finished: at})

		got, err := s.Work(ctx, "KAI", 10)
		if err != nil || len(got) != 10 {
			t.Fatalf("kai's work: %d, %v", len(got), err)
		}
		newest := got[0]
		if newest.Asked != fmt.Sprintf("job %d", org.WorkKept+4) || len(newest.Steps) != 2 || !newest.Steps[1].Result || newest.Cost != 0.04 || newest.ID == 0 {
			t.Errorf("newest: %+v", newest)
		}
		if all, _ := s.Work(ctx, "kai", 1000); len(all) != org.WorkKept {
			t.Errorf("kept %d, want the last %d", len(all), org.WorkKept)
		}
		s.Fire(ctx, "kai")
		if left, _ := s.Work(ctx, "kai", 10); len(left) != 0 {
			t.Errorf("a fired agent's work is kept: %d", len(left))
		}
		if sams, _ := s.Work(ctx, "sam", 10); len(sams) != 1 {
			t.Errorf("sam's work went too: %d", len(sams))
		}
	})
}

func TestTheSeedFillsANewStore(t *testing.T) {
	seed := org.Seed{
		Agents:  []config.Persona{kai()},
		Members: []org.Member{{Account: "ada", Role: "Founder"}},
		Teams:   []org.Team{{Name: "Engineering", Channel: "#engineering", Leads: []string{"kai"}, Focus: "Ship it"}},
		Company: "We make ovens.",
		Goals:   []org.Goal{{Text: "Grow to 100 customers", Target: "100"}, {Team: "Engineering", Text: "Pages load in a second"}},
	}
	eachStore(t, seed, func(t *testing.T, s org.Store) {
		if d, _ := s.Company(ctx); d != "We make ovens." {
			t.Errorf("company = %q", d)
		}
		if goals, _ := s.Goals(ctx); len(goals) != 2 || goals[0].Target != "100" || goals[1].Team != "Engineering" {
			t.Errorf("goals = %+v", goals)
		}
		if l, _ := s.Team(ctx, "Engineering"); l.Focus != "Ship it" {
			t.Errorf("Engineering's focus = %q", l.Focus)
		}
		if _, err := s.Agent(ctx, "kai"); err != nil {
			t.Error("kai missing from seeded store")
		}
		if _, err := s.Member(ctx, "ada"); err != nil {
			t.Error("ada missing from seeded store")
		}
		if l, err := s.Team(ctx, "Engineering"); err != nil || !slices.Equal(l.Leads, []string{"kai"}) {
			t.Errorf("Engineering = %+v, %v", l, err)
		}
	})
}

func TestTheOrgChartSurvivesARestart(t *testing.T) {
	s, url := newPostgres(t, org.Seed{})
	s.Hire(ctx, kai())
	s.CreateTeam(ctx, org.Team{Name: "Sales", Channel: "#sales", MonthlyBudget: 500})
	s.AddMember(ctx, org.Member{Account: "dana", Team: "Sales", MonthlyCost: 400})
	s.Close()

	again, err := org.OpenPostgres(ctx, url, org.Seed{})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if got, err := again.Agent(ctx, "kai"); err != nil || !samePersona(got, kai()) {
		t.Errorf("kai after restart: %+v, %v", got, err)
	}
	if l, err := again.Team(ctx, "Sales"); err != nil || l.MonthlyBudget != 500 {
		t.Errorf("Sales after restart: %+v, %v", l, err)
	}
	if m, err := again.Member(ctx, "dana"); err != nil || m.MonthlyCost != 400 {
		t.Errorf("dana after restart: %+v, %v", m, err)
	}
}

func TestTheSeedOnlyAppliesToANewDatabase(t *testing.T) {
	s, url := newPostgres(t, org.Seed{Teams: []org.Team{{Name: "Sales", Channel: "#sales"}}})
	s.UpdateTeam(ctx, org.Team{Name: "Sales", Channel: "#sales", MonthlyBudget: 900})
	s.Close()

	// A later deploy with a different guildnet.yaml must not overwrite what
	// people changed through GuildServ.
	again, err := org.OpenPostgres(ctx, url, org.Seed{Teams: []org.Team{{Name: "Ops", Channel: "#ops2"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	teams, _ := again.Teams(ctx)
	if len(teams) != 1 || teams[0].Name != "Sales" || teams[0].MonthlyBudget != 900 {
		t.Errorf("teams = %+v, want only Sales with its changed budget", teams)
	}
}

func TestAccessLevelsCanBeSavedChangedAndDropped(t *testing.T) {
	eachStore(t, org.Seed{}, func(t *testing.T, s org.Store) {
		ops := org.AccessLevel{Name: "Ops", Abilities: []string{"spend", "log"}, Scope: "org"}
		support := org.AccessLevel{Name: "Support desk", Abilities: []string{"invite"}, Scope: "teams", Teams: []string{"Support"}}
		for _, a := range []org.AccessLevel{ops, support} {
			if err := s.SaveAccessLevel(ctx, a); err != nil {
				t.Fatal(err)
			}
		}
		ops.Abilities = []string{"spend"}
		if err := s.SaveAccessLevel(ctx, ops); err != nil {
			t.Fatal(err)
		}
		levels, _ := s.AccessLevels(ctx)
		if len(levels) != 2 || !reflect.DeepEqual(levels[0], ops) || !reflect.DeepEqual(levels[1], support) {
			t.Errorf("levels = %+v, want the changed Ops then Support desk", levels)
		}
		if err := s.DropAccessLevel(ctx, "OPS"); err != nil {
			t.Fatal(err)
		}
		if err := s.DropAccessLevel(ctx, "ops"); !errors.Is(err, org.ErrNotFound) {
			t.Errorf("dropping Ops twice: got %v, want ErrNotFound", err)
		}
		if levels, _ := s.AccessLevels(ctx); len(levels) != 1 {
			t.Errorf("levels after dropping Ops = %+v", levels)
		}
	})
}

func TestTheOrgLogKeepsEveryChangeNewestFirst(t *testing.T) {
	eachStore(t, org.Seed{}, func(t *testing.T, s org.Store) {
		at := time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC)
		first := org.LogEntry{At: at, Actor: "ada", Action: "hire", Target: "kai", Team: "Engineering", Detail: "Backend engineer"}
		second := org.LogEntry{At: at.Add(time.Minute), Actor: "dana", Action: "set agent", Target: "rex", Team: "Sales", Detail: "budget $1.00 → $2.00"}
		for _, e := range []org.LogEntry{first, second} {
			if err := s.AddLog(ctx, e); err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.Log(ctx, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].Detail != second.Detail || got[1].Actor != "ada" || !got[1].At.Equal(at) || got[0].ID <= got[1].ID {
			t.Errorf("log = %+v, want dana's change, then ada's", got)
		}
		if got, _ := s.Log(ctx, 1); len(got) != 1 || got[0].Actor != "dana" {
			t.Errorf("log limited to 1 = %+v, want just the newest", got)
		}
	})
}

func TestTheCompanyDescriptionIsKept(t *testing.T) {
	eachStore(t, org.Seed{}, func(t *testing.T, s org.Store) {
		if d, err := s.Company(ctx); err != nil || d != "" {
			t.Errorf("before any: %q, %v; want nothing", d, err)
		}
		if err := s.SetCompany(ctx, "We make ovens for small bakeries."); err != nil {
			t.Fatal(err)
		}
		if d, _ := s.Company(ctx); d != "We make ovens for small bakeries." {
			t.Errorf("description = %q", d)
		}
	})
}

func TestGoalsCanBeAddedChangedAndDropped(t *testing.T) {
	eachStore(t, org.Seed{}, func(t *testing.T, s org.Store) {
		grow, err := s.AddGoal(ctx, org.Goal{Text: "Grow to 100 weekly customers", Measure: "Weekly active customers", Target: "100", Current: "40"})
		if err != nil {
			t.Fatal(err)
		}
		fast, _ := s.AddGoal(ctx, org.Goal{Team: "Support", Text: "Answer within 4 hours", Serves: grow.ID})
		keep, _ := s.AddGoal(ctx, org.Goal{Text: "Keep churn under 3%"})
		if grow.ID == 0 || fast.ID <= grow.ID {
			t.Fatalf("ids: %d then %d, want them given in order", grow.ID, fast.ID)
		}
		grow.Current = "55"
		if err := s.UpdateGoal(ctx, grow); err != nil {
			t.Fatal(err)
		}
		if err := s.DropGoal(ctx, keep.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.DropGoal(ctx, keep.ID); !errors.Is(err, org.ErrNotFound) {
			t.Errorf("dropping a goal twice: got %v, want ErrNotFound", err)
		}
		goals, _ := s.Goals(ctx)
		if len(goals) != 2 || !reflect.DeepEqual(goals[0], grow) || !reflect.DeepEqual(goals[1], fast) {
			t.Errorf("goals = %+v, want the changed company goal, then Support's", goals)
		}
	})
}

func mergeRequest() org.Approval {
	raised := time.Date(2026, 9, 25, 17, 0, 0, 0, time.UTC)
	return org.Approval{Agent: "kai", Approver: "ada", Tool: "github.merge_pull_request",
		Summary: "merge PR #17 'Retry webhooks' into main", Channel: "#engineering", Asker: "ada",
		Resume: "sess_1/sevt_9", RaisedAt: raised, ExpiresAt: raised.Add(72 * time.Hour)}
}

func TestAnApprovalWaitsUntilSomeoneDecides(t *testing.T) {
	eachStore(t, org.Seed{}, func(t *testing.T, s org.Store) {
		a, err := s.RaiseApproval(ctx, mergeRequest())
		if err != nil {
			t.Fatal(err)
		}
		if a.ID == 0 || a.Status != org.Pending {
			t.Fatalf("raised approval = %+v, want an id and pending", a)
		}
		pending, _ := s.PendingApprovals(ctx)
		if len(pending) != 1 || pending[0].ID != a.ID || pending[0].Summary != a.Summary || !pending[0].ExpiresAt.Equal(a.ExpiresAt) {
			t.Errorf("pending = %+v, want the raised approval", pending)
		}

		decided := time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC)
		if err := s.DecideApproval(ctx, a.ID, org.Denied, "ada", "not before launch", decided); err != nil {
			t.Fatal(err)
		}
		got, _ := s.Approval(ctx, a.ID)
		if got.Status != org.Denied || got.DecidedBy != "ada" || got.Reason != "not before launch" || !got.DecidedAt.Equal(decided) {
			t.Errorf("after deciding: %+v", got)
		}
		if pending, _ := s.PendingApprovals(ctx); len(pending) != 0 {
			t.Errorf("still pending after a decision: %+v", pending)
		}
	})
}

// What an agent needs to carry on (its whole conversation) is read for the
// one approval it carries on from, not with every list of them.
func TestListsOfApprovalsLeaveOutWhatTheAgentCarriesOnFrom(t *testing.T) {
	eachStore(t, org.Seed{}, func(t *testing.T, s org.Store) {
		a, _ := s.RaiseApproval(ctx, mergeRequest())
		pending, _ := s.PendingApprovals(ctx)
		since, _ := s.ApprovalsSince(ctx, a.RaisedAt.Add(-time.Hour))
		if len(pending) != 1 || pending[0].Resume != "" || len(since) != 1 || since[0].Resume != "" {
			t.Errorf("lists: %+v, %+v", pending, since)
		}
		if got, _ := s.Approval(ctx, a.ID); got.Resume != "sess_1/sevt_9" {
			t.Errorf("the approval itself: %q", got.Resume)
		}
	})
}

func TestAnApprovalCanOnlyBeDecidedOnce(t *testing.T) {
	eachStore(t, org.Seed{}, func(t *testing.T, s org.Store) {
		a, _ := s.RaiseApproval(ctx, mergeRequest())
		now := time.Now().UTC()
		if err := s.DecideApproval(ctx, a.ID, org.Approved, "ada", "", now); err != nil {
			t.Fatal(err)
		}
		if err := s.DecideApproval(ctx, a.ID, org.Denied, "sam", "too late", now); !errors.Is(err, org.ErrDecided) {
			t.Errorf("deciding twice: got %v, want ErrDecided", err)
		}
		if err := s.DecideApproval(ctx, 9999, org.Approved, "ada", "", now); !errors.Is(err, org.ErrNotFound) {
			t.Errorf("deciding an unknown approval: got %v, want ErrNotFound", err)
		}
	})
}

// A decision is carried on with once: until then it's among the unresumed
// (a restart's runner carries on with it), and only the first claim wins.
func TestADecisionIsCarriedOnWithOnce(t *testing.T) {
	eachStore(t, org.Seed{}, func(t *testing.T, s org.Store) {
		decided, _ := s.RaiseApproval(ctx, mergeRequest())
		waiting, _ := s.RaiseApproval(ctx, mergeRequest())
		now := time.Now().UTC()
		s.DecideApproval(ctx, decided.ID, org.Approved, "ada", "", now)

		if claimed, err := s.ClaimResume(ctx, waiting.ID, now); claimed || err != nil {
			t.Errorf("claimed one still waiting: %v, %v", claimed, err)
		}
		if got, _ := s.Unresumed(ctx); len(got) != 1 || got[0].ID != decided.ID || got[0].Status != org.Approved {
			t.Errorf("unresumed: %+v, want #%d", got, decided.ID)
		}
		first, err1 := s.ClaimResume(ctx, decided.ID, now)
		second, err2 := s.ClaimResume(ctx, decided.ID, now)
		if !first || second || err1 != nil || err2 != nil {
			t.Errorf("claims: %v %v, %v %v; want only the first", first, second, err1, err2)
		}
		if got, _ := s.Unresumed(ctx); len(got) != 0 {
			t.Errorf("unresumed after the claim: %+v", got)
		}
	})
}

// What an agent carries on from can be replaced (sealed) while it waits,
// and isn't kept once it has carried on.
func TestWhatAnAgentCarriesOnFromIsKeptUntilItHas(t *testing.T) {
	eachStore(t, org.Seed{}, func(t *testing.T, s org.Store) {
		a, _ := s.RaiseApproval(ctx, mergeRequest())
		if err := s.SetApprovalResume(ctx, a.ID, "sealed"); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.Approval(ctx, a.ID); got.Resume != "sealed" {
			t.Errorf("replaced: %q", got.Resume)
		}
		if err := s.SetApprovalResume(ctx, 9999, "sealed"); !errors.Is(err, org.ErrNotFound) {
			t.Errorf("replacing an unknown approval's: %v, want ErrNotFound", err)
		}

		now := time.Now().UTC()
		s.DecideApproval(ctx, a.ID, org.Approved, "ada", "", now)
		if got, _ := s.Approval(ctx, a.ID); got.Resume != "sealed" {
			t.Errorf("decided, not carried on with yet: %q", got.Resume)
		}
		s.ClaimResume(ctx, a.ID, now)
		if got, _ := s.Approval(ctx, a.ID); got.Resume != "" || got.Status != org.Approved {
			t.Errorf("carried on with: %+v", got)
		}
	})
}

// Upgrading a network's database counts what was decided before as carried
// on with: the first start after the upgrade doesn't do it all again.
func TestDecisionsFromBeforeTheUpgradeArentCarriedOnWithAgain(t *testing.T) {
	s, url := newPostgres(t, org.Seed{})
	a, _ := s.RaiseApproval(ctx, mergeRequest())
	s.DecideApproval(ctx, a.ID, org.Approved, "ada", "", time.Now().UTC())
	s.RaiseApproval(ctx, mergeRequest()) // still waiting
	// The database as it was before (migration 16).
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `ALTER TABLE approvals DROP COLUMN resumed_at; DELETE FROM goose_db_version WHERE version_id >= 16`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	upgraded, err := org.OpenPostgres(ctx, url, org.Seed{})
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()

	if got, _ := upgraded.Unresumed(ctx); len(got) != 0 {
		t.Errorf("after the upgrade, decisions to carry on with: %+v", got)
	}
}

// Upgrading drops the conversations agents already carried on from, and
// keeps the ones still needed.
func TestTheUpgradeDropsConversationsAlreadyCarriedOnFrom(t *testing.T) {
	s, url := newPostgres(t, org.Seed{})
	done, _ := s.RaiseApproval(ctx, mergeRequest())
	waiting, _ := s.RaiseApproval(ctx, mergeRequest())
	now := time.Now().UTC()
	s.DecideApproval(ctx, done.ID, org.Approved, "ada", "", now)
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	// As before migration 17: carried on with, and its conversation kept.
	if _, err := conn.Exec(ctx, `UPDATE approvals SET resumed_at = now(), resume = 'sess_1/sevt_9' WHERE id = $1`, done.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `DELETE FROM goose_db_version WHERE version_id >= 17`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	upgraded, err := org.OpenPostgres(ctx, url, org.Seed{})
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()

	if got, _ := upgraded.Approval(ctx, done.ID); got.Resume != "" {
		t.Errorf("carried on from, still kept: %q", got.Resume)
	}
	if got, _ := upgraded.Approval(ctx, waiting.ID); got.Resume != "sess_1/sevt_9" {
		t.Errorf("still waiting, kept as %q", got.Resume)
	}
}

func TestAnApprovalMovesAlongItsRoute(t *testing.T) {
	eachStore(t, org.Seed{}, func(t *testing.T, s org.Store) {
		req := mergeRequest()
		req.Approver, req.Route = "dana", []string{"dana", "sam", "ada"}
		req.MovesAt = req.RaisedAt.Add(4 * time.Hour)
		a, _ := s.RaiseApproval(ctx, req)
		later := req.MovesAt.Add(4 * time.Hour)
		if err := s.MoveApproval(ctx, a.ID, "sam", later); err != nil {
			t.Fatal(err)
		}
		got, _ := s.Approval(ctx, a.ID)
		if got.Approver != "sam" || !got.MovesAt.Equal(later) || !slices.Equal(got.Route, req.Route) {
			t.Errorf("after moving: %+v", got)
		}
		s.DecideApproval(ctx, a.ID, org.Approved, "sam", "", later)
		if err := s.MoveApproval(ctx, a.ID, "ada", later); !errors.Is(err, org.ErrDecided) {
			t.Errorf("moving a decided approval: got %v, want ErrDecided", err)
		}
	})
}

func TestSettingsAreKept(t *testing.T) {
	eachStore(t, org.Seed{}, func(t *testing.T, s org.Store) {
		if v, err := s.Setting(ctx, "approvals_move_after"); err != nil || v != "" {
			t.Errorf("unset: %q, %v", v, err)
		}
		s.SetSetting(ctx, "approvals_move_after", "1h")
		s.SetSetting(ctx, "approvals_move_after", "2h")
		if v, _ := s.Setting(ctx, "approvals_move_after"); v != "2h" {
			t.Errorf("setting = %q, want 2h", v)
		}
	})
}

func TestApprovalsSinceATime(t *testing.T) {
	eachStore(t, org.Seed{}, func(t *testing.T, s org.Store) {
		old := mergeRequest()
		s.RaiseApproval(ctx, old)
		recent := mergeRequest()
		recent.RaisedAt = old.RaisedAt.Add(48 * time.Hour)
		r, _ := s.RaiseApproval(ctx, recent)
		s.DecideApproval(ctx, r.ID, org.Approved, "ada", "", recent.RaisedAt.Add(time.Hour))

		got, err := s.ApprovalsSince(ctx, old.RaisedAt.Add(time.Hour))

		if err != nil || len(got) != 1 || got[0].ID != r.ID || got[0].Status != org.Approved {
			t.Errorf("since: %+v %v, want just the recent, decided one", got, err)
		}
	})
}

func TestApprovalNumbersGoUp(t *testing.T) {
	eachStore(t, org.Seed{}, func(t *testing.T, s org.Store) {
		a, _ := s.RaiseApproval(ctx, mergeRequest())
		b, _ := s.RaiseApproval(ctx, mergeRequest())
		if b.ID <= a.ID {
			t.Errorf("second approval #%d should come after #%d", b.ID, a.ID)
		}
	})
}

func TestAnAgentsDeskIsRemembered(t *testing.T) {
	eachStore(t, org.Seed{}, func(t *testing.T, s org.Store) {
		if _, err := s.Desk(ctx, "kai"); !errors.Is(err, org.ErrNotFound) {
			t.Errorf("desk before any: got %v, want ErrNotFound", err)
		}
		d := org.Desk{Nick: "kai", AgentID: "agent_1", AgentVersion: 3, SessionID: "sesn_1", CostCents: 12}
		if err := s.SaveDesk(ctx, d); err != nil {
			t.Fatal(err)
		}
		d.CostCents, d.SessionID = 20, "sesn_2"
		if err := s.SaveDesk(ctx, d); err != nil {
			t.Fatal(err)
		}
		if got, err := s.Desk(ctx, "KAI"); err != nil || got != d {
			t.Errorf("desk = %+v, %v; want %+v", got, err, d)
		}
	})
}

func samePersona(a, b config.Persona) bool {
	return a.Nick == b.Nick && a.Name == b.Name && a.Role == b.Role && a.Team == b.Team &&
		a.ReportsTo == b.ReportsTo && a.Model == b.Model && a.Effort == b.Effort &&
		a.DailyBudgetUSD == b.DailyBudgetUSD && slices.Equal(a.Channels, b.Channels) && a.Prompt == b.Prompt &&
		a.Platform == b.Platform && a.DailyWakes == b.DailyWakes
}
