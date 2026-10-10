// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/desk"
	"github.com/guildnet-chat/guildnet/internal/egress"
	"github.com/guildnet-chat/guildnet/internal/gitremote"
	"github.com/guildnet-chat/guildnet/internal/gittest"
	"github.com/guildnet-chat/guildnet/internal/sandbox"
	"github.com/guildnet-chat/guildnet/platform"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// Git repositories on any host (GitLab, Gitea, a company's own server):
// the network's owner adds each on the console's Connectors page with two
// credentials, one to read and one to push, which stay in the console,
// sealed. A network's runner gets its own network's from the broker, one
// call at a time. acme's owner is ada.

// withGitServer lets the console reach s: it trusts s's certificate, and s
// (on this machine) is named as a private git host, as a self-hosted install
// names its own git server.
func withGitServer(s *gittest.Server) func(*Setup) {
	return func(o *Setup) {
		pool := x509.NewCertPool()
		pool.AddCert(s.Certificate())
		o.ConnectorEgress = egress.Options{RootCAs: pool}
		o.GitPrivateHosts = []string{"127.0.0.1"}
	}
}

func gitWorld(t *testing.T, tweaks ...func(*Setup)) (*World, *gittest.Server) {
	t.Helper()
	s := gittest.New(t)
	store := platform.NewMemoryStore()
	return NewWorldWith(t, func() Store { return store }, append([]func(*Setup){withGitServer(s)}, tweaks...)...), s
}

// repoForm is the form adding the repository at s as name.
func repoForm(s *gittest.Server, name string) url.Values {
	return url.Values{"name": {name}, "url": {s.URL()}, "read_user": {gittest.User}, "read_token": {gittest.ReadToken},
		"write_user": {gittest.User}, "write_token": {gittest.WriteToken}}
}

func addRepo(t *testing.T, b *Browser, form url.Values) Response {
	t.Helper()
	b.Get("/connectors")
	return b.Post("/git", form)
}

func TestTheOwnerAddsAGitRepository(t *testing.T) {
	w, s := gitWorld(t)
	ada := w.Browser("acme").SignedIn("ada")
	if page := ada.Get("/connectors"); !strings.Contains(page.Body, `action="/git"`) {
		t.Fatalf("the owner should be offered adding a git repository:\n%s", page.Text())
	}

	if r := addRepo(t, ada, repoForm(s, "web")); r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/connectors#git" {
		t.Fatalf("adding web: %d to %q\n%s", r.Status, r.Header.Get("Location"), r.Text())
	}

	repo, err := w.Store.(platform.GitRepos).GitRepo(context.Background(), "acme", "web")
	if err != nil || repo.URL != s.URL() || repo.Read.Token != gittest.ReadToken || repo.Write.Token != gittest.WriteToken || repo.Write.User != gittest.User {
		t.Fatalf("acme's web: %+v, %v", repo, err)
	}
	page := ada.Get("/connectors")
	if !strings.Contains(page.Text(), "web") || !strings.Contains(page.Text(), s.URL()) {
		t.Errorf("the page should list web and where it is:\n%s", page.Text())
	}
	if strings.Contains(page.Body, gittest.ReadToken) || strings.Contains(page.Body, gittest.WriteToken) {
		t.Error("the page shows a token")
	}
}

// A repository is checked before it's added: its read credential reads,
// and its write credential may push. A read credential that may push too is
// taken, with a warning: it's reachable from desks.
func TestAGitRepositoryIsCheckedBeforeItsAdded(t *testing.T) {
	w, s := gitWorld(t)
	ada := w.Browser("acme").SignedIn("ada")
	for _, c := range []struct{ field, value, want string }{
		{"read_token", "wrong", "read"},
		{"write_token", gittest.ReadToken, "push"},
		{"write_token", "wrong", "push"},
	} {
		form := repoForm(s, "web")
		form.Set(c.field, c.value)
		r := addRepo(t, ada, form)
		if r.Status == http.StatusSeeOther || !strings.Contains(r.Text(), c.want) {
			t.Errorf("%s %q: %d, want it refused, saying %q\n%s", c.field, c.value, r.Status, c.want, r.Text())
		}
		if _, err := w.Store.(platform.GitRepos).GitRepo(context.Background(), "acme", "web"); err == nil {
			t.Errorf("%s %q: web was added", c.field, c.value)
		}
	}

	form := repoForm(s, "web")
	form.Set("read_token", gittest.WriteToken)
	if r := addRepo(t, ada, form); r.Status != http.StatusOK || !strings.Contains(r.Text(), "can push") {
		t.Errorf("a read credential that may push: %d, want it taken with a warning\n%s", r.Status, r.Text())
	}
	if _, err := w.Store.(platform.GitRepos).GitRepo(context.Background(), "acme", "web"); err != nil {
		t.Errorf("web wasn't added: %v", err)
	}
}

// A public repository needs no read credential, and one only read needs no
// write credential.
func TestAGitRepositoryMayHaveNoCredentialToReadOrNoneToPush(t *testing.T) {
	w, s := gitWorld(t)
	s.AnyoneReads()
	ada := w.Browser("acme").SignedIn("ada")
	form := repoForm(s, "web")
	form.Del("read_token")
	form.Del("read_user")
	form.Del("write_token")
	if r := addRepo(t, ada, form); r.Status != http.StatusSeeOther {
		t.Fatalf("adding a public repository to read only: %d\n%s", r.Status, r.Text())
	}
	repo, err := w.Store.(platform.GitRepos).GitRepo(context.Background(), "acme", "web")
	if err != nil || repo.Read.Token != "" || repo.Write.Token != "" {
		t.Errorf("acme's web: %+v, %v", repo, err)
	}
}

// Only https addresses, with no credentials in them, on the internet (or
// named as private by whoever runs the install), and SSH isn't taken yet.
func TestOnlyHTTPSGitAddressesAreTaken(t *testing.T) {
	w, s := gitWorld(t)
	ada := w.Browser("acme").SignedIn("ada")
	for addr, want := range map[string]string{
		strings.Replace(s.URL(), "https://", "http://", 1): "https://",
		"ssh://git@git.example.com/team/web.git":           "SSH",
		"git@git.example.com:team/web.git":                 "SSH",
		"https://git:token@git.example.com/team/web.git":   "user name", // trufflehog:ignore (made up)
		"https://10.0.0.5/team/web.git":                    "private",
		"https://localhost/team/web.git":                   "private",
	} {
		form := repoForm(s, "web")
		form.Set("url", addr)
		if r := addRepo(t, ada, form); r.Status == http.StatusSeeOther || !strings.Contains(r.Text(), want) {
			t.Errorf("%s: %d, want it refused, saying %q\n%s", addr, r.Status, want, r.Text())
		}
	}
	for _, name := range []string{"", "Web", "web site", "../web", strings.Repeat("w", 41)} {
		if r := addRepo(t, ada, repoForm(s, name)); r.Status == http.StatusSeeOther {
			t.Errorf("the name %q was taken", name)
		}
	}
	if repos, _ := w.Store.(platform.GitRepos).GitRepos(context.Background(), "acme"); len(repos) != 0 {
		t.Errorf("added %+v", repos)
	}
}

func TestOnlyTheOwnerManagesGitRepositories(t *testing.T) {
	w, s := gitWorld(t)
	addRepo(t, w.Browser("acme").SignedIn("ada"), repoForm(s, "web"))
	bo := w.Browser("acme").SignedIn("bo")

	page := bo.Get("/connectors")

	if !strings.Contains(page.Text(), "web") {
		t.Errorf("bo should see the network's repositories:\n%s", page.Text())
	}
	if strings.Contains(page.Body, `action="/git`) {
		t.Errorf("bo isn't the owner, but is offered managing repositories:\n%s", page.Text())
	}
	for _, path := range []string{"/git", "/git/web/test", "/git/web/remove"} {
		bo.Get("/connectors")
		if r := bo.Post(path, repoForm(s, "web")); r.Status != http.StatusNotFound {
			t.Errorf("bo posting %s: %d, want 404", path, r.Status)
		}
	}
}

func TestTheOwnerTestsAndRemovesAGitRepository(t *testing.T) {
	w, s := gitWorld(t)
	ada := w.Browser("acme").SignedIn("ada")
	addRepo(t, ada, repoForm(s, "web"))
	ada.Get("/connectors")

	r := ada.Post("/git/web/test", nil)

	if text := r.Text(); r.Status != http.StatusOK || !strings.Contains(text, "Reads") || !strings.Contains(text, "Pushes") {
		t.Errorf("testing web: %d\n%s", r.Status, text)
	}
	ada.Get("/connectors")
	if r := ada.Post("/git/web/remove", nil); r.Status != http.StatusSeeOther {
		t.Fatalf("removing web: %d %s", r.Status, r.Text())
	}
	if _, err := w.Store.(platform.GitRepos).GitRepo(context.Background(), "acme", "web"); err == nil {
		t.Error("web is still there")
	}
}

// Adding a repository, changing its credentials or removing it could hand
// the network's code to someone else: the owner hears of each.
func TestTheOwnerIsAlertedWhenGitRepositoriesChange(t *testing.T) {
	s := gittest.New(t)
	out := &Outbox{}
	w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, WithMail(out), withGitServer(s))
	_, console := w.Created("ada@example.com", "initech")

	addRepo(t, console, repoForm(s, "web"))
	addRepo(t, console, repoForm(s, "web"))
	console.Get("/connectors")
	console.Post("/git/web/remove", nil)

	for _, what := range []string{"web was added to initech", "web was changed on initech", "web was removed from initech"} {
		m := out.One(t, "ada@example.com", what, "ada", "If that wasn't you")
		if strings.Contains(m.Text+m.HTML, gittest.ReadToken) || strings.Contains(m.Text+m.HTML, gittest.WriteToken) {
			t.Errorf("the alert %q carries a token", m.Subject)
		}
	}
}

// The broker: a runner gets its own network's repositories, and their
// credentials one at a time; never another network's.
func TestTheBrokerGivesARunnerItsOwnGitCredentials(t *testing.T) {
	w, s := gitWorld(t)
	addRepo(t, w.Browser("acme").SignedIn("ada"), repoForm(s, "web"))

	status, out := BrokerCall(t, w, "acme", RunnerSecret(w, "acme"), "GET", "/git")
	list, _ := out["repos"].([]any)
	if status != http.StatusOK || len(list) != 1 {
		t.Fatalf("acme's repositories: %d %v", status, out)
	}
	if r := list[0].(map[string]any); r["name"] != "web" || r["url"] != s.URL() || r["read"] != true || r["write"] != true {
		t.Errorf("acme's web: %v", r)
	}
	if strings.Contains(strings.Join(strings.Fields(strings.ToLower(toString(out))), ""), "token") {
		t.Errorf("the list has a token: %v", out)
	}
	for access, token := range map[string]string{"read": gittest.ReadToken, "write": gittest.WriteToken} {
		status, out := BrokerCall(t, w, "acme", RunnerSecret(w, "acme"), "POST", "/git/web/"+access)
		if status != http.StatusOK || out["user"] != gittest.User || out["token"] != token {
			t.Errorf("acme's web, to %s: %d %v", access, status, out)
		}
	}
	if status, _ := BrokerCall(t, w, "acme", RunnerSecret(w, "globex"), "POST", "/git/web/write"); status != http.StatusUnauthorized {
		t.Errorf("asking for acme's with globex's secret: %d, want 401", status)
	}
	if status, _ := BrokerCall(t, w, "globex", RunnerSecret(w, "globex"), "POST", "/git/web/write"); status != http.StatusNotFound {
		t.Errorf("globex asking for web: %d, want 404", status)
	}
}

// A runner keeps its network's repositories as the broker lists them, and
// asks it for a credential when one's needed.
func TestARunnerHearsOfItsGitRepositoriesFromTheBroker(t *testing.T) {
	w, s := gitWorld(t)
	addRepo(t, w.Browser("acme").SignedIn("ada"), repoForm(s, "web"))
	broker := httptest.NewServer(w.Broker)
	defer broker.Close()
	acme := &gitremote.Live{Broker: gitremote.Broker{URL: broker.URL, Network: "acme", Secret: RunnerSecret(w, "acme")}}

	if err := acme.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	if repos := acme.Repos(); len(repos) != 1 || repos[0].Name != "web" || repos[0].URL != s.URL() || !repos[0].Write || repos[0].Host() != "127.0.0.1" {
		t.Errorf("acme's repositories: %+v", repos)
	}
	addr, cred, err := acme.Broker.Credential(context.Background(), "web", true)
	if err != nil || addr != s.URL() || cred.Token != gittest.WriteToken {
		t.Errorf("web's push credential: %q %v %v", addr, cred.User, err)
	}
	globex := gitremote.Broker{URL: broker.URL, Network: "globex", Secret: RunnerSecret(w, "globex")}
	if _, _, err := globex.Credential(context.Background(), "web", false); err == nil {
		t.Error("globex was given acme's credential")
	}
}

// A team's leads give it the network's git repositories on the team's page,
// beside GitHub's.
func TestATeamsLeadGivesItGitRepositoriesInTheConsole(t *testing.T) {
	w, s := gitWorld(t)
	addRepo(t, w.Browser("acme").SignedIn("ada"), repoForm(s, "web"))
	bo := w.Browser("acme").SignedIn("bo")
	if page := bo.Get("/teams/Sales"); !strings.Contains(page.Body, `name="repos" value="git:web"`) {
		t.Fatalf("Sales' page should offer the git repository:\n%s", page.Text())
	}

	if r := bo.Post("/teams/Sales", url.Values{"repos": {"", "docs", "git:web"}}); r.Status != http.StatusSeeOther {
		t.Fatalf("choosing git:web: %d %s", r.Status, r.Text())
	}

	if team, _ := w.Networks["acme"].Store.Team(context.Background(), "Sales"); !slices.Equal(team.Repos, []string{"docs", "git:web"}) {
		t.Errorf("Sales' repos = %v, want docs and git:web", team.Repos)
	}
}

// A network's desks reach its git repositories' hosts with its egress
// pass, which the broker gives its runner with its sandbox token: it opens
// that network's hosts, and only those.
func TestARunnersEgressPassOpensItsGitHostsOnly(t *testing.T) {
	w, s := gitWorld(t, func(o *Setup) { o.Sandbox = platform.Sandbox{URL: "http://10.20.0.5:8443", Key: SandboxKey} })
	addRepo(t, w.Browser("acme").SignedIn("ada"), repoForm(s, "web"))
	u, _ := url.Parse(s.URL())
	port, _ := strconv.Atoi(u.Port())
	keys := []ed25519.PublicKey{SandboxKey.Public().(ed25519.PublicKey)}

	_, acme := BrokerCall(t, w, "acme", RunnerSecret(w, "acme"), "GET", "/sandbox")
	_, globex := BrokerCall(t, w, "globex", RunnerSecret(w, "globex"), "GET", "/sandbox")

	pass, _ := acme["egress"].(string)
	if !sandbox.PassOpens(keys, pass, time.Now(), "127.0.0.1", port) {
		t.Errorf("acme's pass %q doesn't open its git host", pass)
	}
	if other, _ := globex["egress"].(string); sandbox.PassOpens(keys, other, time.Now(), "127.0.0.1", port) {
		t.Error("globex's pass opens acme's git host")
	}
	broker := httptest.NewServer(w.Broker)
	defer broker.Close()
	runners := desk.Broker{URL: broker.URL, Network: "acme", Secret: RunnerSecret(w, "acme")}
	if pass, err := runners.Pass(context.Background()); err != nil || !sandbox.PassOpens(keys, pass, time.Now(), "127.0.0.1", port) {
		t.Errorf("the runner's pass: %q, %v", pass, err)
	}
}

// A desk checks out the git repositories its agent's team works on, each
// with its read credential, and never the one to push with.
func TestADeskGetsItsTeamsGitRepositoriesWithTheirReadCredentials(t *testing.T) {
	w, s := gitWorld(t)
	addRepo(t, w.Browser("acme").SignedIn("ada"), repoForm(s, "web"))
	broker := httptest.NewServer(w.Broker)
	defer broker.Close()
	live := &gitremote.Live{Broker: gitremote.Broker{URL: broker.URL, Network: "acme", Secret: RunnerSecret(w, "acme")}}
	live.Refresh(context.Background())
	teams := map[string][]string{"Engineering": {"docs", "git:web"}, "Sales": {"git:gone"}}
	checkouts := gitremote.Checkouts{Live: live, TeamRepos: func(team string) []string { return teams[team] }}

	clones, err := checkouts.Clones(context.Background(), config.Persona{Nick: "kai", Team: "Engineering"})

	if err != nil || len(clones) != 1 || clones[0].Name != "web" || clones[0].URL != s.URL() || clones[0].Read.Token != gittest.ReadToken {
		t.Errorf("kai's desk checks out %+v, %v; want web with its read token", clones, err)
	}
	if clones, err := checkouts.Clones(context.Background(), config.Persona{Nick: "dana", Team: "Sales"}); err != nil || len(clones) != 0 {
		t.Errorf("a repository the network no longer has: %+v, %v", clones, err)
	}
}

func toString(v any) string {
	var b strings.Builder
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, x := range v {
				b.WriteString(k + " ")
				walk(x)
			}
		case []any:
			for _, x := range v {
				walk(x)
			}
		case string:
			b.WriteString(v + " ")
		}
	}
	walk(v)
	return b.String()
}

// Credentials are sealed, bound to the repository's address: one moved to
// another address in the database doesn't open, so it's never sent there.
func TestGitCredentialsAreSealedAndBoundToTheirAddress(t *testing.T) {
	s := gittest.New(t)
	w := NewWorldWithPostgres(t, withGitServer(s))
	addRepo(t, w.Browser("acme").SignedIn("ada"), repoForm(s, "web"))

	w.RestartPlatform()

	if status, out := BrokerCall(t, w, "acme", RunnerSecret(w, "acme"), "POST", "/git/web/write"); status != http.StatusOK || out["token"] != gittest.WriteToken {
		t.Fatalf("after a restart: %d %v", status, out)
	}
	conn, err := pgx.Connect(context.Background(), w.DBURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var read, write []byte
	if err := conn.QueryRow(context.Background(), `select read_token, write_token from git_repos`).Scan(&read, &write); err != nil {
		t.Fatal(err)
	}
	for _, b := range [][]byte{read, write} {
		if strings.Contains(string(b), "token-for-tests") {
			t.Errorf("a token is stored as it is: %q", b)
		}
	}
	if _, err := conn.Exec(context.Background(), `update git_repos set url = 'https://elsewhere.example.com/web.git'`); err != nil {
		t.Fatal(err)
	}
	if status, out := BrokerCall(t, w, "acme", RunnerSecret(w, "acme"), "POST", "/git/web/write"); status == http.StatusOK {
		t.Errorf("a credential moved to another address was given out: %v", out)
	}
}
