// SPDX-License-Identifier: AGPL-3.0-only

// Package config loads the runner config and persona files.
package config

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // timezone data built in: the container image has none

	"gopkg.in/yaml.v3"

	"github.com/guildnet-chat/guildnet/internal/egress"
	"github.com/guildnet-chat/guildnet/internal/roles"
	"github.com/guildnet-chat/guildnet/models"
)

// Runner is the top-level guildnet.yaml.
type Runner struct {
	Server         string   `yaml:"server"`
	Owner          string   `yaml:"owner"`
	Channels       []string `yaml:"channels"`
	OpsChannel     string   `yaml:"ops_channel"`
	LedgerNick     string   `yaml:"ledger_nick"`
	DailyBudgetUSD float64  `yaml:"daily_budget_usd"`
	LoopLimit      int      `yaml:"loop_limit"`
	HistoryLines   int      `yaml:"history_lines"`
	Relevance      struct {
		Enabled bool   `yaml:"enabled"`
		Model   string `yaml:"model"`
	} `yaml:"relevance"`
	// Catchup summarizes what people missed while they were away.
	Catchup struct {
		Enabled bool          `yaml:"enabled"`
		After   time.Duration `yaml:"after"` // automatic catch-up after this long away
		Model   string        `yaml:"model"`
	} `yaml:"catchup"`
	// The starting org chart. Agents come from personas_dir.
	Teams   []TeamConfig `yaml:"teams"`
	Members []struct {
		Account   string  `yaml:"account"`
		Name      string  `yaml:"name"`
		Role      string  `yaml:"role"`
		Team      string  `yaml:"team"`
		ReportsTo string  `yaml:"reports_to"`
		Guest     bool    `yaml:"guest"`
		Cost      float64 `yaml:"monthly_cost_usd"`
		OldLane   string  `yaml:"lane"` // what Team was called; see oldNames
	} `yaml:"members"`
	OldLanes []TeamConfig `yaml:"lanes"` // what Teams was called; see oldNames
	// Description is what the company does, and Goals what it (and its
	// teams) are aiming for: the guiding lights every agent reads.
	Description string       `yaml:"description"`
	Goals       []GoalConfig `yaml:"goals"`
	// Accounts lets the runner give every agent and bot its own NickServ
	// account through Ergo's HTTP API. Secrets, so from the environment:
	// GUILDNET_ERGO_API_URL, GUILDNET_ERGO_API_TOKEN, GUILDNET_ACCOUNT_SECRET.
	// Off unless all three are set.
	Accounts struct {
		APIURL   string `yaml:"-"`
		APIToken string `yaml:"-"`
		Secret   string `yaml:"-"`
	} `yaml:"-"`
	// Oper is GuildServ's IRC operator login, used to keep IRC in step with
	// the org chart. From the environment: GUILDNET_OPER_NAME and
	// GUILDNET_OPER_PASSWORD. Syncing is off unless both and Accounts are set.
	Oper struct {
		Name     string `yaml:"-"`
		Password string `yaml:"-"`
	} `yaml:"-"`
	// Timezone is the company's IANA timezone, like America/Vancouver, for
	// times people and agents see. Location is it, loaded. Defaults to UTC.
	Timezone string         `yaml:"timezone"`
	Location *time.Location `yaml:"-"`
	// Clock is the time source (tests use a fake one); nil means time.Now.
	// ApprovalCheckEvery is how often expired approvals are looked for.
	Clock              func() time.Time `yaml:"-"`
	ApprovalCheckEvery time.Duration    `yaml:"-"`
	// API is the runner's internal API, for the platform's console: where it
	// listens (GUILDNET_API_LISTEN, e.g. ":8090"; empty means off) and the
	// network's secret every call must carry (GUILDNET_API_TOKEN). It won't
	// listen without a secret.
	API struct {
		Listen string `yaml:"-"`
		Token  string `yaml:"-"`
	} `yaml:"-"`
	// ContinueWithin: lines from one person this close together are one
	// message their client split up (IRC lines are short). Default 1.5s;
	// negative turns it off.
	ContinueWithin time.Duration `yaml:"-"`
	// ReplyTimeout is how long a plain reply may take; DeskTimeout is how
	// long an agent at a desk may work through tools in one go.
	ReplyTimeout time.Duration `yaml:"-"`
	DeskTimeout  time.Duration `yaml:"-"`
	// GitHub is the company's GitHub App (see README). Owner and repos come
	// from the file; the app's id, installation, and key are secrets, from
	// the environment: GUILDNET_GITHUB_APP_ID, GUILDNET_GITHUB_INSTALLATION_ID,
	// and GUILDNET_GITHUB_APP_KEY (the PEM, or it base64-encoded) or
	// GUILDNET_GITHUB_APP_KEY_FILE. Off unless all are set.
	GitHub struct {
		Owner string   `yaml:"owner"`
		Repos []string `yaml:"repos"`
		// Teams gives each team some of Repos, by team name; teams not
		// listed get no GitHub. Without it, every team gets every repo.
		Teams          map[string][]string `yaml:"teams"`
		OldLanes       map[string][]string `yaml:"lanes"` // what Teams was called
		AppID          int64               `yaml:"-"`
		InstallationID int64               `yaml:"-"`
		Key            []byte              `yaml:"-"`
	} `yaml:"github"`
	// GitHubLive, when set, is the account and repos the network's GitHub
	// installation has now (a customer's, through the platform's broker),
	// in place of GitHub.Owner and GitHub.Repos.
	GitHubLive func() (account string, repos []string) `yaml:"-"`
	// DatabaseURL is where the org chart lives (GUILDNET_DATABASE_URL). When
	// empty, the org chart is kept in memory and resets on restart.
	DatabaseURL string `yaml:"-"`
	// GeneralChannel is the company-wide channel; AnnouncementsChannel is
	// where only leads and the owner can speak.
	GeneralChannel string `yaml:"general_channel"`
	// VhostDomain ends everyone's role hostname: kai@ai.engineering.guildnet.
	VhostDomain          string `yaml:"vhost_domain"`
	AnnouncementsChannel string `yaml:"announcements_channel"`
	// FeedbackChannel is where people talk to the GuildNet team: what's
	// said there is collected for GuildNet's admins. "" means none.
	FeedbackChannel string `yaml:"feedback_channel"`
	PersonasDir     string `yaml:"personas_dir"`
	DataDir         string `yaml:"data_dir"`
	Company         string `yaml:"company"`
	// WelcomeDir is where Ergo's welcome (MOTD) lives, shared with Ergo
	// (GUILDNET_WELCOME_DIR): GuildServ adds what the company is for. ""
	// means Ergo's welcome is left as it starts. WelcomeFiles, if set, is
	// used in its place (tests reach into Ergo's container).
	WelcomeDir   string `yaml:"-"`
	WelcomeFiles Files  `yaml:"-"`
	// Providers are the model providers the network has keys for, from the
	// environment (ANTHROPIC_API_KEY, OPENAI_API_KEY, GEMINI_API_KEY,
	// OPENROUTER_API_KEY); see ModelProviders.
	Providers []models.Provider `yaml:"-"`
	// ProvidersLive, when set, is the providers the network has keys for now
	// (fetched from the platform's broker while it runs), in place of
	// Providers.
	ProvidersLive func() []models.Provider `yaml:"-"`
	// WebhookEgress is how outside agents' webhooks are reached (tests set
	// it to reach a local server); WebhookRetryAfter is the wait before a
	// failed call's first retry (0: two seconds).
	WebhookEgress     egress.Options `yaml:"-"`
	WebhookRetryAfter time.Duration  `yaml:"-"`
	// RefreshModelKeys, when set, fetches the network's model keys again
	// (the platform asks after they change).
	RefreshModelKeys func() `yaml:"-"`
}

// Files reads and writes files by name, somewhere.
type Files interface {
	ReadFile(name string) ([]byte, error)
	// WriteFile replaces the file all at once.
	WriteFile(name string, data []byte) error
}

// TeamConfig is a team in the starting org chart.
type TeamConfig struct {
	Name    string  `yaml:"name"`
	Channel string  `yaml:"channel"`
	Lead    string  `yaml:"lead"`
	Parent  string  `yaml:"parent"`
	Focus   string  `yaml:"focus"`
	Private bool    `yaml:"private"`
	Budget  float64 `yaml:"monthly_budget_usd"`
}

// GoalConfig is a goal in the starting org: the company's, or a team's.
type GoalConfig struct {
	Text    string `yaml:"text"`
	Team    string `yaml:"team"`
	Measure string `yaml:"measure"`
	Target  string `yaml:"target"`
	Current string `yaml:"current"`
}

// Persona is one agent employee, loaded from personas/<nick>.yaml.
type Persona struct {
	Nick    string `yaml:"nick"`
	Name    string `yaml:"name"`
	Role    string `yaml:"role"`
	Team    string `yaml:"team"`
	OldLane string `yaml:"lane,omitempty"` // what Team was called; see oldNames
	// Preset is a role from the catalogue (internal/roles) to start from;
	// whatever else the file says wins.
	Preset         string   `yaml:"preset,omitempty"`
	ReportsTo      string   `yaml:"reports_to"`
	Model          string   `yaml:"model"`
	Effort         string   `yaml:"effort"`
	DailyBudgetUSD float64  `yaml:"daily_budget_usd"`
	Channels       []string `yaml:"channels"`
	Prompt         string   `yaml:"prompt"`
	// Platform is where the agent runs: "" or "chat" for a plain model call
	// with GuildNet's tools (any model), "managed" for a desk with a workspace too, on Managed Agents,
	// "outside" for its own program, on its company's systems.
	Platform string `yaml:"platform"`
	// DailyWakes caps how many times a day an outside agent is woken (its
	// model calls happen elsewhere, so a dollar budget can't hold it); 0
	// means the default.
	DailyWakes int `yaml:"daily_wakes"`
}

// Load reads the runner config at path and every persona in its personas_dir.
// Relative directories resolve against the config file's directory. The
// network's model providers are those whose keys are in the environment.
func Load(path string) (*Runner, []Persona, error) {
	return LoadFor(path, providersWithKeys())
}

// LoadFor is Load for a network with keys for providers (fetched from the
// platform, say, not in the environment).
func LoadFor(path string, providers []models.Provider) (*Runner, []Persona, error) {
	var cfg Runner
	// Defaults for settings the file leaves out.
	cfg.Catchup.Enabled = true
	cfg.Catchup.After = 2 * time.Hour
	cfg.Catchup.Model = string(models.Fast)
	if err := readYAML(path, &cfg); err != nil {
		return nil, nil, err
	}
	cfg.Providers = providers
	cfg.oldNames()
	base := filepath.Dir(path)
	cfg.PersonasDir = resolve(base, cfg.PersonasDir, "personas")
	cfg.DataDir = resolve(base, cfg.DataDir, "data")
	if s := os.Getenv("GUILDNET_DATA_DIR"); s != "" {
		cfg.DataDir = s
	}
	if s := os.Getenv("GUILDNET_SERVER"); s != "" {
		cfg.Server = s
	}
	// A network made from a template: its owner, company and timezone.
	if s := os.Getenv("GUILDNET_OWNER"); s != "" {
		if !ValidNick(s) {
			return nil, nil, fmt.Errorf("GUILDNET_OWNER %q isn't a nick: letters, digits, - and _, starting with a letter, at most 32", s)
		}
		cfg.Owner = s
	}
	company := os.Getenv("GUILDNET_COMPANY")
	if strings.ContainsAny(company, "\r\n") || len(company) > 64 {
		return nil, nil, errors.New("GUILDNET_COMPANY must be one line of at most 64 characters")
	}
	if s := os.Getenv("GUILDNET_TIMEZONE"); s != "" {
		cfg.Timezone = s
	}
	cfg.Accounts.APIURL = os.Getenv("GUILDNET_ERGO_API_URL")
	cfg.Accounts.APIToken = os.Getenv("GUILDNET_ERGO_API_TOKEN")
	cfg.Accounts.Secret = os.Getenv("GUILDNET_ACCOUNT_SECRET")
	cfg.DatabaseURL = os.Getenv("GUILDNET_DATABASE_URL")
	cfg.Oper.Name = os.Getenv("GUILDNET_OPER_NAME")
	cfg.Oper.Password = os.Getenv("GUILDNET_OPER_PASSWORD")
	cfg.API.Listen = os.Getenv("GUILDNET_API_LISTEN")
	cfg.API.Token = os.Getenv("GUILDNET_API_TOKEN")
	cfg.WelcomeDir = os.Getenv("GUILDNET_WELCOME_DIR")
	if err := loadGitHub(&cfg); err != nil {
		return nil, nil, fmt.Errorf("%s: github: %w", path, err)
	}
	tz := cfg.Timezone
	if tz == "" {
		tz = "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: timezone %q: %w", path, tz, err)
	}
	cfg.Location = loc
	if cfg.GeneralChannel == "" {
		cfg.GeneralChannel = "#general"
	}
	if cfg.ContinueWithin == 0 {
		cfg.ContinueWithin = 1500 * time.Millisecond
	}
	if cfg.AnnouncementsChannel == "" {
		cfg.AnnouncementsChannel = "#announcements"
	}
	if cfg.FeedbackChannel == "" {
		cfg.FeedbackChannel = "#feedback"
	}
	if cfg.Server == "" {
		cfg.Server = "127.0.0.1:6667"
	}
	if cfg.LedgerNick == "" {
		cfg.LedgerNick = "ledger"
	}
	if cfg.LoopLimit <= 0 {
		cfg.LoopLimit = 6
	}
	if cfg.HistoryLines <= 0 {
		cfg.HistoryLines = 40
	}
	if cfg.DailyBudgetUSD <= 0 {
		return nil, nil, fmt.Errorf("%s: daily_budget_usd must be set", path)
	}
	for _, m := range []*string{&cfg.Catchup.Model, &cfg.Relevance.Model} {
		if *m == "" {
			*m = string(models.Fast)
		}
		// Checked now, but a tier stays a tier: it's picked from the
		// providers there are when each call is made.
		if _, err := cfg.modelNamed(*m); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", path, err)
		}
	}

	files, err := filepath.Glob(filepath.Join(cfg.PersonasDir, "*.yaml"))
	if err != nil {
		return nil, nil, err
	}
	var personas []Persona
	seen := map[string]bool{strings.ToLower(cfg.LedgerNick): true}
	for _, f := range files {
		var p Persona
		if err := readYAML(f, &p); err != nil {
			return nil, nil, err
		}
		if p.Team == "" {
			p.Team = p.OldLane
		}
		p.OldLane = ""
		if p.Preset != "" {
			if err := fromPreset(&p); err != nil {
				return nil, nil, fmt.Errorf("%s: %w", f, err)
			}
		}
		if p.Nick == "" || p.Model == "" {
			return nil, nil, fmt.Errorf("%s: nick and model are required", f)
		}
		if p.Model, err = cfg.modelNamed(p.Model); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", f, err)
		}
		if seen[strings.ToLower(p.Nick)] {
			return nil, nil, fmt.Errorf("%s: nick %q is already taken", f, p.Nick)
		}
		seen[strings.ToLower(p.Nick)] = true
		personas = append(personas, p)
	}
	if len(personas) == 0 {
		return nil, nil, fmt.Errorf("no personas found in %s", cfg.PersonasDir)
	}
	if err := fillIn(&cfg, personas, company); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, personas, nil
}

// oldNames moves what a file says under the names teams had when they were
// called lanes onto the new names, so files from then still load.
func (cfg *Runner) oldNames() {
	if len(cfg.Teams) == 0 {
		cfg.Teams = cfg.OldLanes
	}
	cfg.OldLanes = nil
	for i := range cfg.Members {
		if cfg.Members[i].Team == "" {
			cfg.Members[i].Team = cfg.Members[i].OldLane
		}
		cfg.Members[i].OldLane = ""
	}
	if cfg.GitHub.Teams == nil {
		cfg.GitHub.Teams = cfg.GitHub.OldLanes
	}
	cfg.GitHub.OldLanes = nil
}

var nick = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,31}$`)

// ValidNick says whether s is a nick GuildNet gives out: letters, digits, -
// and _, starting with a letter, at most 32 characters. (IRC allows more;
// these are safe everywhere a nick ends up.)
func ValidNick(s string) bool { return nick.MatchString(s) }

// fillIn puts the network's owner and company into a template's text:
// {{owner}} and {{company}} in the company description, the org chart, and
// the agents. It's done after parsing, so what's filled in can't change the
// template's shape.
func fillIn(cfg *Runner, personas []Persona, company string) error {
	var pairs []string
	if cfg.Owner != "" {
		pairs = append(pairs, "{{owner}}", cfg.Owner)
	}
	if company != "" {
		pairs = append(pairs, "{{company}}", company)
	}
	r := strings.NewReplacer(pairs...)
	var left []string
	fill := func(s *string) {
		*s = r.Replace(*s)
		if strings.Contains(*s, "{{") {
			left = append(left, *s)
		}
	}
	fill(&cfg.Company)
	fill(&cfg.Description)
	for i := range cfg.Teams {
		fill(&cfg.Teams[i].Lead)
		fill(&cfg.Teams[i].Focus)
	}
	for i := range cfg.Goals {
		fill(&cfg.Goals[i].Text)
	}
	for i := range cfg.Members {
		fill(&cfg.Members[i].Account)
		fill(&cfg.Members[i].ReportsTo)
	}
	for i := range personas {
		p := &personas[i]
		fill(&p.Name)
		fill(&p.Role)
		fill(&p.ReportsTo)
		fill(&p.Prompt)
	}
	if len(left) > 0 {
		return fmt.Errorf("this is a template: set GUILDNET_OWNER and GUILDNET_COMPANY to fill in %q", left[0])
	}
	return nil
}

func readYAML(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func resolve(base, dir, def string) string {
	if dir == "" {
		dir = def
	}
	if filepath.IsAbs(dir) {
		return dir
	}
	return filepath.Join(base, dir)
}

// GitHubEnabled says whether the GitHub App is fully configured.
func (c *Runner) GitHubEnabled() bool {
	g := c.GitHub
	return g.Owner != "" && len(g.Repos) > 0 && g.AppID != 0 && g.InstallationID != 0 && len(g.Key) > 0
}

// loadGitHub reads the GitHub App's secrets from the environment.
func loadGitHub(cfg *Runner) error {
	var err error
	if s := os.Getenv("GUILDNET_GITHUB_APP_ID"); s != "" {
		if cfg.GitHub.AppID, err = strconv.ParseInt(s, 10, 64); err != nil {
			return fmt.Errorf("GUILDNET_GITHUB_APP_ID: %w", err)
		}
	}
	if s := os.Getenv("GUILDNET_GITHUB_INSTALLATION_ID"); s != "" {
		if cfg.GitHub.InstallationID, err = strconv.ParseInt(s, 10, 64); err != nil {
			return fmt.Errorf("GUILDNET_GITHUB_INSTALLATION_ID: %w", err)
		}
	}
	key := []byte(os.Getenv("GUILDNET_GITHUB_APP_KEY"))
	if file := os.Getenv("GUILDNET_GITHUB_APP_KEY_FILE"); len(key) == 0 && file != "" {
		if key, err = os.ReadFile(file); err != nil {
			return fmt.Errorf("reading the app's key: %w", err)
		}
	}
	for team, repos := range cfg.GitHub.Teams {
		for _, r := range repos {
			if !slices.Contains(cfg.GitHub.Repos, r) {
				return fmt.Errorf("team %s has repo %s, which isn't in github.repos", team, r)
			}
		}
	}
	if len(key) == 0 {
		return nil
	}
	if !bytes.Contains(key, []byte("-----BEGIN")) {
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(key)))
		if err != nil || !bytes.Contains(decoded, []byte("-----BEGIN")) {
			return errors.New("the app's key should be its PEM file's contents, or those base64-encoded")
		}
		key = decoded
	}
	cfg.GitHub.Key = key
	return nil
}

// providersWithKeys is the model providers whose keys are in the
// environment.
func providersWithKeys() []models.Provider {
	var have []models.Provider
	for _, p := range models.Providers {
		if os.Getenv(p.KeyVar()) != "" {
			have = append(have, p)
		}
	}
	return have
}

// ModelProviders is the model providers the network has keys for; with none
// at all (a developer's network), it's on Anthropic, as every network was
// before there was a choice.
func (c *Runner) ModelProviders() []models.Provider {
	if c.ProvidersLive != nil {
		if live := c.ProvidersLive(); len(live) > 0 {
			return live
		}
	}
	if len(c.Providers) == 0 {
		return []models.Provider{models.Anthropic}
	}
	return c.Providers
}

// modelNamed is the model a file names: a tier ("balanced") is its model
// from the network's providers. A model whose provider has no key is kept,
// since files only start a network off (its agents' models can change
// later); one nobody serves is an error.
func (c *Runner) modelNamed(name string) (string, error) {
	m, err := models.Resolve(name, c.ModelProviders())
	if errors.Is(err, models.ErrNoKey) {
		if known, ok := models.Find(name); ok {
			return known.ID, nil
		}
	}
	if err != nil {
		return "", fmt.Errorf("model %w", err)
	}
	return m.ID, nil
}

// fromPreset fills in what a persona leaves out from its role in the
// catalogue: title, model tier, effort, daily cap, and prompt.
func fromPreset(p *Persona) error {
	role, ok := roles.Find(p.Preset)
	if !ok {
		return fmt.Errorf("preset %q isn't in the role catalogue", p.Preset)
	}
	if p.Role == "" {
		p.Role = role.Title
	}
	if p.Model == "" {
		p.Model = string(role.Tier)
	}
	if p.Effort == "" {
		p.Effort = role.Effort
	}
	if p.DailyBudgetUSD == 0 {
		p.DailyBudgetUSD = role.DailyBudget
	}
	if p.Prompt == "" {
		p.Prompt = role.Prompt
	}
	p.Preset = ""
	return nil
}
