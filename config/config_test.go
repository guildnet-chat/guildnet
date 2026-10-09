// SPDX-License-Identifier: AGPL-3.0-only

package config_test

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/models"
)

func writeNetwork(t *testing.T, server string) string {
	t.Helper()
	dir := t.TempDir()
	cfg := "server: " + server + "\ndaily_budget_usd: 5\n"
	persona := "nick: kai\nmodel: claude-sonnet-5\nchannels: [\"#engineering\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "guildnet.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "personas"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "personas", "kai.yaml"), []byte(persona), 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "guildnet.yaml")
}

func TestRunnerConnectsToTheServerInTheConfigFile(t *testing.T) {
	t.Setenv("GUILDNET_SERVER", "")
	cfg, _, err := config.Load(writeNetwork(t, "127.0.0.1:6667"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server != "127.0.0.1:6667" {
		t.Errorf("server = %q, want the config file's 127.0.0.1:6667", cfg.Server)
	}
}

func TestCatchupDefaultsToTwoHoursOnHaiku(t *testing.T) {
	withKeys(t, "ANTHROPIC_API_KEY")
	cfg, _, err := config.Load(writeNetwork(t, "127.0.0.1:6667"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Catchup.Enabled || cfg.Catchup.After != 2*time.Hour || cfg.Catchup.Model != "fast" {
		t.Errorf("catch-up = %+v, want enabled after 2h on the fast tier", cfg.Catchup)
	}
}

func TestCatchupSettingsComeFromTheConfigFile(t *testing.T) {
	path := writeNetwork(t, "127.0.0.1:6667")
	extra := "catchup:\n  enabled: false\n  after: 90m\n  model: claude-sonnet-5\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(extra)
	f.Close()

	cfg, _, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Catchup.Enabled || cfg.Catchup.After != 90*time.Minute || cfg.Catchup.Model != "claude-sonnet-5" {
		t.Errorf("catch-up = %+v, want the file's settings", cfg.Catchup)
	}
}

func appendConfig(t *testing.T, path, extra string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.WriteString(extra)
}

func TestTheCompanyTimezoneDefaultsToUTC(t *testing.T) {
	cfg, _, err := config.Load(writeNetwork(t, "127.0.0.1:6667"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Location.String() != "UTC" {
		t.Errorf("timezone = %s, want UTC", cfg.Location)
	}
}

func TestTheCompanyTimezoneComesFromTheConfigFile(t *testing.T) {
	path := writeNetwork(t, "127.0.0.1:6667")
	appendConfig(t, path, "timezone: America/Vancouver\n")

	cfg, _, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Location.String() != "America/Vancouver" {
		t.Errorf("timezone = %s, want America/Vancouver", cfg.Location)
	}
}

func TestAnUnknownTimezoneIsAnError(t *testing.T) {
	path := writeNetwork(t, "127.0.0.1:6667")
	appendConfig(t, path, "timezone: Mars/Olympus_Mons\n")

	if _, _, err := config.Load(path); err == nil {
		t.Error("loading a config with an unknown timezone should fail, not fall back silently")
	}
}

func TestGuildnetServerEnvironmentVariableOverridesTheConfigFile(t *testing.T) {
	// In docker compose the IRC server is reachable as ergo:6667, not localhost.
	t.Setenv("GUILDNET_SERVER", "ergo:6667")
	cfg, _, err := config.Load(writeNetwork(t, "127.0.0.1:6667"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server != "ergo:6667" {
		t.Errorf("server = %q, want ergo:6667 from GUILDNET_SERVER", cfg.Server)
	}
}

const fakePEM = "-----BEGIN TEST KEY-----\nnot-a-real-key\n-----END TEST KEY-----\n"

func withGitHub(t *testing.T) string {
	t.Helper()
	path := writeNetwork(t, "127.0.0.1:6667")
	appendConfig(t, path, "github:\n  owner: acme-inc\n  repos: [guildnet, guildnet-chat]\n")
	t.Setenv("GUILDNET_GITHUB_APP_ID", "5080068")
	t.Setenv("GUILDNET_GITHUB_INSTALLATION_ID", "164983362")
	t.Setenv("GUILDNET_GITHUB_APP_KEY", "")
	t.Setenv("GUILDNET_GITHUB_APP_KEY_FILE", "")
	return path
}

func TestGitHubIsOffUntilTheAppsSecretsAreSet(t *testing.T) {
	path := withGitHub(t)

	cfg, _, err := config.Load(path)

	if err != nil {
		t.Fatal(err)
	}
	if cfg.GitHubEnabled() {
		t.Error("GitHub should be off without the app's key")
	}
}

func TestGitHubAppKeyCanBeBase64SoItFitsOnOneLine(t *testing.T) {
	path := withGitHub(t)
	t.Setenv("GUILDNET_GITHUB_APP_KEY", base64.StdEncoding.EncodeToString([]byte(fakePEM)))

	cfg, _, err := config.Load(path)

	if err != nil {
		t.Fatal(err)
	}
	if !cfg.GitHubEnabled() || string(cfg.GitHub.Key) != fakePEM {
		t.Errorf("key = %q, want the decoded PEM", cfg.GitHub.Key)
	}
	if cfg.GitHub.AppID != 5080068 || cfg.GitHub.InstallationID != 164983362 || cfg.GitHub.Owner != "acme-inc" || len(cfg.GitHub.Repos) != 2 {
		t.Errorf("github = %+v", cfg.GitHub)
	}
}

func TestGitHubAppKeyCanBeThePEMItself(t *testing.T) {
	path := withGitHub(t)
	t.Setenv("GUILDNET_GITHUB_APP_KEY", fakePEM)

	cfg, _, _ := config.Load(path)

	if string(cfg.GitHub.Key) != fakePEM {
		t.Errorf("key = %q", cfg.GitHub.Key)
	}
}

func TestGitHubAppKeyCanBeAFile(t *testing.T) {
	path := withGitHub(t)
	keyFile := filepath.Join(t.TempDir(), "app.pem")
	os.WriteFile(keyFile, []byte(fakePEM), 0o600)
	t.Setenv("GUILDNET_GITHUB_APP_KEY_FILE", keyFile)

	cfg, _, err := config.Load(path)

	if err != nil {
		t.Fatal(err)
	}
	if string(cfg.GitHub.Key) != fakePEM {
		t.Errorf("key = %q", cfg.GitHub.Key)
	}
}

func TestAKeyThatIsntAKeyIsAnError(t *testing.T) {
	path := withGitHub(t)
	t.Setenv("GUILDNET_GITHUB_APP_KEY", "hunter2")

	if _, _, err := config.Load(path); err == nil {
		t.Error("a GitHub app key that's neither PEM nor base64 PEM should fail loudly")
	}
}

func TestEachTeamsGitHubReposComeFromTheConfigFile(t *testing.T) {
	path := writeNetwork(t, "127.0.0.1:6667")
	appendConfig(t, path, "github:\n  owner: acme-inc\n  repos: [guildnet, guildnet-site]\n  teams:\n    Engineering: [guildnet]\n    Marketing: [guildnet-site]\n")

	cfg, _, err := config.Load(path)

	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.GitHub.Teams) != 2 || cfg.GitHub.Teams["Marketing"][0] != "guildnet-site" {
		t.Errorf("teams = %v", cfg.GitHub.Teams)
	}
}

// Teams used to be called lanes; files written then still load.
func TestAConfigFromWhenTeamsWereLanesStillLoads(t *testing.T) {
	path := writeNetwork(t, "127.0.0.1:6667")
	appendConfig(t, path, "lanes:\n  - name: Sales\n    lead: dana\n"+
		"members:\n  - account: dana\n    lane: Sales\n"+
		"github:\n  owner: acme-inc\n  repos: [guildnet]\n  lanes:\n    Sales: [guildnet]\n")
	persona := filepath.Join(filepath.Dir(path), "personas", "kai.yaml")
	os.WriteFile(persona, []byte("nick: kai\nmodel: claude-sonnet-5\nlane: Sales\n"), 0o644)

	cfg, personas, err := config.Load(path)

	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Teams) != 1 || cfg.Teams[0].Name != "Sales" || cfg.Teams[0].Lead != "dana" {
		t.Errorf("teams = %+v, want Sales led by dana", cfg.Teams)
	}
	if len(cfg.Members) != 1 || cfg.Members[0].Team != "Sales" {
		t.Errorf("members = %+v, want dana in Sales", cfg.Members)
	}
	if len(cfg.GitHub.Teams["Sales"]) != 1 {
		t.Errorf("github teams = %v, want Sales's repo", cfg.GitHub.Teams)
	}
	if len(personas) != 1 || personas[0].Team != "Sales" {
		t.Errorf("personas = %+v, want kai in Sales", personas)
	}
}

func TestATeamCanOnlyHaveReposTheAppIsAllowed(t *testing.T) {
	path := writeNetwork(t, "127.0.0.1:6667")
	appendConfig(t, path, "github:\n  owner: acme-inc\n  repos: [guildnet]\n  teams:\n    Marketing: [secret-plans]\n")

	if _, _, err := config.Load(path); err == nil {
		t.Error("a team naming a repo outside github.repos should be an error")
	}
}

// withKeys sets which model providers have keys, as the host would.
func withKeys(t *testing.T, vars ...string) {
	t.Helper()
	for _, p := range models.Providers {
		t.Setenv(p.KeyVar(), "")
	}
	for _, v := range vars {
		t.Setenv(v, "a-key")
	}
}

// Where a template or a persona names a tier instead of a model, it's the
// tier's model from the providers the network has keys for.
func TestTiersBecomeModelsFromTheProvidersWithKeys(t *testing.T) {
	withKeys(t, "OPENAI_API_KEY")
	path := writeNetwork(t, "127.0.0.1:6667")
	dir := filepath.Dir(path)
	os.WriteFile(filepath.Join(dir, "personas", "kai.yaml"), []byte("nick: kai\nmodel: balanced\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "personas", "sue.yaml"), []byte("nick: sue\npreset: support-agent\n"), 0o644)

	cfg, personas, err := config.Load(path)

	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.Providers, []models.Provider{models.OpenAI}) {
		t.Errorf("providers = %v, want OpenAI (the only key)", cfg.Providers)
	}
	got := map[string]string{}
	for _, p := range personas {
		got[p.Nick] = p.Model
	}
	if got["kai"] != "openai:gpt-6-sol" || got["sue"] != "openai:gpt-6-luna" {
		t.Errorf("models = %v, want OpenAI's balanced and fast", got)
	}
	// Small calls keep the tier, picked from the providers there are when
	// each is made (keys can arrive, or change, while the runner runs).
	if cfg.Catchup.Model != "fast" {
		t.Errorf("catch-ups run on %q, want the fast tier, picked when they run", cfg.Catchup.Model)
	}
}

// A runner that fetched its keys from the platform (none in its
// environment) loads its config for those providers.
func TestLoadingForTheProvidersARunnerFetched(t *testing.T) {
	withKeys(t)
	path := writeNetwork(t, "127.0.0.1:6667")
	os.WriteFile(filepath.Join(filepath.Dir(path), "personas", "kai.yaml"), []byte("nick: kai\nmodel: balanced\n"), 0o644)

	cfg, personas, err := config.LoadFor(path, []models.Provider{models.Google})

	if err != nil || personas[0].Model != "google:gemini-3.8-flash" || !slices.Equal(cfg.ModelProviders(), []models.Provider{models.Google}) {
		t.Errorf("kai on %q, providers %v: %v", personas[0].Model, cfg.ModelProviders(), err)
	}
}

// A network with no provider keys at all (a developer's, say) is on
// Anthropic, as every network was before providers.
func TestWithNoKeysANetworkIsOnAnthropic(t *testing.T) {
	withKeys(t)
	cfg, _, err := config.Load(writeNetwork(t, "127.0.0.1:6667"))
	if err != nil || !slices.Equal(cfg.ModelProviders(), []models.Provider{models.Anthropic}) {
		t.Errorf("providers %v, %v", cfg.ModelProviders(), err)
	}
}

func TestAModelNobodyServesIsAnError(t *testing.T) {
	path := writeNetwork(t, "127.0.0.1:6667")
	os.WriteFile(filepath.Join(filepath.Dir(path), "personas", "kai.yaml"), []byte("nick: kai\nmodel: gpt-9000\n"), 0o644)
	if _, _, err := config.Load(path); err == nil || !strings.Contains(err.Error(), "gpt-9000") {
		t.Errorf("err = %v, want one naming the model", err)
	}
}

// A template's persona can start from a role in the catalogue, and the
// template can say what the company and its teams are for.
func TestATemplateCanUseTheRoleCatalogueAndSetGoals(t *testing.T) {
	withKeys(t, "ANTHROPIC_API_KEY")
	path := writeNetwork(t, "127.0.0.1:6667")
	appendConfig(t, path, "description: We make ovens.\n"+
		"teams:\n  - name: Support\n    focus: Bakers get answers fast\n  - name: Returns\n    parent: Support\n"+
		"goals:\n  - text: Grow to 100 customers\n    measure: Customers\n    target: \"100\"\n  - text: Answer within 4 hours\n    team: Support\n")
	persona := filepath.Join(filepath.Dir(path), "personas", "kai.yaml")
	os.WriteFile(persona, []byte("nick: kai\npreset: support-agent\nteam: Support\nrole: Help desk\n"), 0o644)

	cfg, personas, err := config.Load(path)

	if err != nil {
		t.Fatal(err)
	}
	kai := personas[0]
	if kai.Role != "Help desk" || kai.Model != "claude-haiku-4-5" || !strings.Contains(kai.Prompt, "support agent") || kai.DailyBudgetUSD != 1 {
		t.Errorf("kai = %+v, want the support agent preset with the title given", kai)
	}
	if cfg.Description != "We make ovens." || len(cfg.Goals) != 2 || cfg.Goals[1].Team != "Support" || cfg.Goals[0].Target != "100" {
		t.Errorf("description %q, goals %+v", cfg.Description, cfg.Goals)
	}
	if cfg.Teams[0].Focus != "Bakers get answers fast" || cfg.Teams[1].Parent != "Support" {
		t.Errorf("teams = %+v", cfg.Teams)
	}
}
