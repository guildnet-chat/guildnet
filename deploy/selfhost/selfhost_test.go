// SPDX-License-Identifier: AGPL-3.0-only

// Package selfhost is GuildNet on one machine, with Docker Compose; its
// tests check what the stack lets in, what each service may do, and that
// init.sh makes every secret it needs without showing any.
package selfhost_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

type service struct {
	Ports       []string       `yaml:"ports"`
	CapDrop     []string       `yaml:"cap_drop"`
	CapAdd      []string       `yaml:"cap_add"`
	SecurityOpt []string       `yaml:"security_opt"`
	ReadOnly    bool           `yaml:"read_only"`
	User        string         `yaml:"user"`
	Volumes     []string       `yaml:"volumes"`
	Networks    any            `yaml:"networks"` // a list, or a map
	Environment map[string]any `yaml:"environment"`
	Profiles    []string       `yaml:"profiles"`
	Image       string         `yaml:"image"`
	Build       any            `yaml:"build"` // a folder, or a map
	Scale       *int           `yaml:"scale"`
	NetworkMode string         `yaml:"network_mode"`
	Tmpfs       []string       `yaml:"tmpfs"`
	Sysctls     map[string]any `yaml:"sysctls"`
	DependsOn   map[string]any `yaml:"depends_on"`
	Entrypoint  []string       `yaml:"entrypoint"`
}

type stack struct {
	Services map[string]service `yaml:"services"`
	Networks map[string]struct {
		Name       string `yaml:"name"`
		Internal   bool   `yaml:"internal"`
		EnableIPv6 *bool  `yaml:"enable_ipv6"`
		IPAM       struct {
			Config []struct {
				Subnet string `yaml:"subnet"`
			} `yaml:"config"`
		} `yaml:"ipam"`
	} `yaml:"networks"`
}

// networksOf is the networks a service is on, listed or as a map.
func networksOf(s service) []string {
	var names []string
	switch n := s.Networks.(type) {
	case []any:
		for _, v := range n {
			names = append(names, fmt.Sprint(v))
		}
	case map[string]any:
		for k := range n {
			names = append(names, k)
		}
	}
	return names
}

func readStack(t *testing.T) stack {
	t.Helper()
	b, err := os.ReadFile("compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	var s stack
	if err := yaml.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// Only the edge (Caddy) can be reached from outside the machine: HTTPS for
// the home page and the console, HTTP to send people to it, and IRC over TLS.
func TestOnlyTheEdgeCanBeReachedFromOutside(t *testing.T) {
	for name, s := range readStack(t).Services {
		if name == "caddy" {
			if want := []string{"80:80", "443:443", "443:443/udp", "6697:6697"}; !slices.Equal(s.Ports, want) {
				t.Errorf("caddy publishes %v, want %v", s.Ports, want)
			}
			continue
		}
		if len(s.Ports) > 0 {
			t.Errorf("%s publishes %v; only caddy should", name, s.Ports)
		}
	}
}

// Every service runs with no Linux capabilities beyond what it needs, can't
// gain any, and (where it can) can't change its own files.
func TestEveryServiceHasOnlyTheCapabilitiesItNeeds(t *testing.T) {
	needs := map[string][]string{
		"caddy": nil, // binds 80 and 443 as itself (TestTheEdgeRunsAsItsOwnUserReadOnly)
		// Hands Caddy's certificates to its user: chown, through folders it
		// may already own.
		"caddy-files": {"CHOWN", "DAC_READ_SEARCH"},
		// The entrypoint hands the data to the postgres user, then drops to it.
		"postgres": {"CHOWN", "DAC_OVERRIDE", "FOWNER", "SETGID", "SETUID"},
		"platform": nil,
		"runner":   nil,
		"ergo":     nil,
		// Desks: sandboxd drives Docker through its socket, which needs no
		// capabilities of its own.
		"sandboxd": nil,
		"egress":   nil,
		// Backups: tar, as root, keeping who owns Ergo's and the runner's files.
		"files": {"CHOWN", "DAC_OVERRIDE", "FOWNER"},
	}
	readOnly := []string{"caddy", "platform", "runner", "ergo", "egress"}
	for name, s := range readStack(t).Services {
		if s.Scale != nil && *s.Scale == 0 {
			continue // an image built, never run
		}
		allowed, known := needs[name]
		if !known {
			t.Errorf("%s: a service this test doesn't know; say what capabilities it needs", name)
		}
		if !slices.Equal(s.CapDrop, []string{"ALL"}) {
			t.Errorf("%s drops %v, want ALL", name, s.CapDrop)
		}
		for _, add := range s.CapAdd {
			if !slices.Contains(allowed, add) {
				t.Errorf("%s adds %s, which it doesn't need", name, add)
			}
		}
		if !slices.Contains(s.SecurityOpt, "no-new-privileges:true") {
			t.Errorf("%s can gain privileges: security_opt %v", name, s.SecurityOpt)
		}
		if slices.Contains(readOnly, name) && !s.ReadOnly {
			t.Errorf("%s can change its own files; want read_only", name)
		}
		if slices.Contains(readOnly, name) && (s.User == "" || strings.HasPrefix(s.User, "0")) {
			t.Errorf("%s runs as %q; want a user that isn't root", name, s.User)
		}
	}
}

// The database is on a network of its own with no way out, shared only with
// the console and the runner: not the edge, and not Ergo.
func TestTheDatabaseIsReachedOnlyByTheConsoleAndTheRunner(t *testing.T) {
	s := readStack(t)
	on := networksOf(s.Services["postgres"])
	if len(on) != 1 {
		t.Fatalf("postgres is on %v, want one network", on)
	}
	data := on[0]
	if !s.Networks[data].Internal {
		t.Errorf("postgres's network %s has a way out; want internal", data)
	}
	for name, svc := range s.Services {
		on := slices.Contains(networksOf(svc), data)
		if want := name == "postgres" || name == "platform" || name == "runner"; on != want {
			t.Errorf("%s on %s (the database's network): %v, want %v", name, data, on, want)
		}
	}
}

// addressOn is the fixed address a service has on a network, if any.
func addressOn(s service, network string) string {
	n, _ := s.Networks.(map[string]any)
	on, _ := n[network].(map[string]any)
	a, _ := on["ipv4_address"].(string)
	return a
}

// Ergo is on a network of its own with no way out, shared only with what
// talks to it: the edge (IRC over TLS), the console (web chat, its API) and
// the runner. Only the edge and the console, at their fixed addresses
// there, may say who a visitor is (PROXY, X-Forwarded-For): not the runner,
// not sandboxd, not anything else on the machine's private networks. (It
// asks the broker about logins on the broker's network, also with no way
// out.)
func TestErgoIsReachedOnlyOnANetworkOfItsOwn(t *testing.T) {
	s := readStack(t)
	var on []string
	for _, n := range networksOf(s.Services["ergo"]) {
		if !s.Networks[n].Internal {
			t.Errorf("ergo is on %s, which has a way out", n)
		}
		if !slices.Contains(networksOf(s.Services["caddy"]), n) {
			continue // the broker's
		}
		on = append(on, n)
	}
	if len(on) != 1 {
		t.Fatalf("ergo shares %v with the edge, want one network", on)
	}
	irc := on[0]
	if !s.Networks[irc].Internal {
		t.Errorf("ergo's network %s has a way out; want internal", irc)
	}
	for name, svc := range s.Services {
		on := slices.Contains(networksOf(svc), irc)
		if want := name == "ergo" || name == "caddy" || name == "platform" || name == "runner"; on != want {
			t.Errorf("%s on %s (Ergo's network): %v, want %v", name, irc, on, want)
		}
	}
	var trusted []string
	for _, name := range []string{"caddy", "platform"} {
		a := addressOn(s.Services[name], irc)
		if a == "" {
			t.Fatalf("%s has no fixed address on %s", name, irc)
		}
		trusted = append(trusted, a+"/32")
	}
	var got []string
	if err := json.Unmarshal([]byte(fmt.Sprint(s.Services["ergo"].Environment["ERGO__SERVER__PROXY_ALLOWED_FROM"])), &got); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, trusted) {
		t.Errorf("ergo believes PROXY from %v, want just %v", got, trusted)
	}
	if subnets := s.Networks[irc].IPAM.Config; len(subnets) != 1 {
		t.Errorf("%s's addresses: %v, want one subnet the fixed ones are in", irc, subnets)
	}
}

// The broker (settings, keys, tokens; people's logins) listens only on a
// network of its own with no way out, at the console's fixed address there,
// shared only with what asks it: the runner and Ergo. Not the edge: Caddy
// faces the internet, and can't reach it.
func TestTheBrokerIsReachedOnlyOnANetworkOfItsOwn(t *testing.T) {
	s := readStack(t)
	platform := s.Services["platform"]
	host, port, err := net.SplitHostPort(fmt.Sprint(platform.Environment["GUILDNET_GITHUB_BROKER_LISTEN"]))
	if err != nil || net.ParseIP(host) == nil {
		t.Fatalf("the broker listens on %v; want one address of the console's", platform.Environment["GUILDNET_GITHUB_BROKER_LISTEN"])
	}
	var broker string
	for _, n := range networksOf(platform) {
		if addressOn(platform, n) == host {
			broker = n
		}
	}
	if broker == "" {
		t.Fatalf("the broker's address %s isn't the console's on any network", host)
	}
	if !s.Networks[broker].Internal {
		t.Errorf("the broker's network %s has a way out; want internal", broker)
	}
	if subnets := s.Networks[broker].IPAM.Config; len(subnets) != 1 {
		t.Errorf("%s's addresses: %v, want one subnet the console's is in", broker, subnets)
	} else if _, in, err := net.ParseCIDR(subnets[0].Subnet); err != nil || !in.Contains(net.ParseIP(host)) {
		t.Errorf("%s's subnet %s doesn't have %s", broker, subnets[0].Subnet, host)
	}
	for name, svc := range s.Services {
		on := slices.Contains(networksOf(svc), broker)
		if want := name == "platform" || name == "runner" || name == "ergo"; on != want {
			t.Errorf("%s on %s (the broker's network): %v, want %v", name, broker, on, want)
		}
	}

	// Its callers ask for it by a name only that network has, which its
	// certificate is for.
	n, _ := platform.Networks.(map[string]any)
	on, _ := n[broker].(map[string]any)
	var aliases []string
	if list, ok := on["aliases"].([]any); ok {
		for _, a := range list {
			aliases = append(aliases, fmt.Sprint(a))
		}
	}
	if len(aliases) != 1 {
		t.Fatalf("the console's names on %s: %v, want one", broker, aliases)
	}
	if names := strings.Split(fmt.Sprint(platform.Environment["GUILDNET_BROKER_NAMES"]), ","); !slices.Contains(names, aliases[0]) {
		t.Errorf("the broker's certificate is for %v, not %s", names, aliases[0])
	}
	at := "https://" + aliases[0] + ":" + port
	for _, k := range []struct{ service, env, want string }{
		{"runner", "GUILDNET_KEYS_BROKER", at},
		{"runner", "GUILDNET_GITHUB_BROKER", at},
		{"ergo", "GUILDNET_IRC_AUTH_URL", at + "/irc/login"},
	} {
		if got := fmt.Sprint(s.Services[k.service].Environment[k.env]); got != k.want {
			t.Errorf("%s's %s = %q, want %q", k.service, k.env, got, k.want)
		}
	}
}

// Caddy faces the internet: it runs as a user of its own, can't change its
// files, and holds no capabilities (binding 80 and 443 is allowed in its
// network namespace instead). Its certificates, root's before it ran as that
// user, are handed to it first.
func TestTheEdgeRunsAsItsOwnUserReadOnly(t *testing.T) {
	s := readStack(t)
	c := s.Services["caddy"]
	if c.User == "" || strings.HasPrefix(c.User, "0") {
		t.Errorf("caddy runs as %q; want a user that isn't root", c.User)
	}
	if !c.ReadOnly || !slices.Contains(c.Tmpfs, "/tmp") {
		t.Errorf("caddy: read_only %v, tmpfs %v; want read-only with /tmp", c.ReadOnly, c.Tmpfs)
	}
	if len(c.CapAdd) > 0 {
		t.Errorf("caddy adds %v; want none", c.CapAdd)
	}
	if got := fmt.Sprint(c.Sysctls["net.ipv4.ip_unprivileged_port_start"]); got != "0" {
		t.Errorf("caddy's net.ipv4.ip_unprivileged_port_start = %s, want 0 (so it binds 80 and 443 as itself)", got)
	}
	owner, ok := c.DependsOn["caddy-files"].(map[string]any)
	if !ok || owner["condition"] != "service_completed_successfully" {
		t.Fatalf("caddy depends on %v; want caddy-files, completed", c.DependsOn)
	}
	f := s.Services["caddy-files"]
	if want := []string{"chown", "-R", c.User, "/data", "/config"}; !slices.Equal(f.Entrypoint, want) || f.NetworkMode != "none" {
		t.Errorf("caddy-files runs %v (network %q), want %v with no network", f.Entrypoint, f.NetworkMode, want)
	}
}

// The runner gets its settings and keys from the console's broker, and
// nothing about the network but its name and secrets from the stack.
func TestTheRunnerWaitsForFirstRunSetup(t *testing.T) {
	r := readStack(t).Services["runner"]
	for k, want := range map[string]string{
		"GUILDNET_SETTINGS_FROM_BROKER": "on",
		"GUILDNET_KEYS_BROKER":          "https://broker:8082",
	} {
		if got := fmt.Sprint(r.Environment[k]); got != want {
			t.Errorf("runner's %s = %q, want %q", k, got, want)
		}
	}
	for _, k := range []string{"GUILDNET_CONFIG", "GUILDNET_OWNER", "ANTHROPIC_API_KEY"} {
		if _, ok := r.Environment[k]; ok {
			t.Errorf("runner's %s is set; it comes from first-run setup", k)
		}
	}
}

// The runner reaches the console's broker (which hands it the model keys)
// over TLS, trusting only the console's own CA: its certificate is shared
// with the runner, read-only, and the CA's key is in a volume only the
// console has.
func TestTheRunnerReachesTheBrokerOverTLS(t *testing.T) {
	s := readStack(t)
	platform, runner := s.Services["platform"], s.Services["runner"]
	for _, k := range []string{"GUILDNET_KEYS_BROKER", "GUILDNET_GITHUB_BROKER"} {
		if got := fmt.Sprint(runner.Environment[k]); !strings.HasPrefix(got, "https://") {
			t.Errorf("runner's %s = %q, want https://...", k, got)
		}
	}
	mountOf := func(svc service, path string) string {
		for _, v := range svc.Volumes {
			if parts := strings.Split(v, ":"); len(parts) >= 2 && parts[1] == path {
				return v
			}
		}
		return ""
	}
	own := mountOf(platform, fmt.Sprint(platform.Environment["GUILDNET_BROKER_TLS_DIR"]))
	share := mountOf(platform, fmt.Sprint(platform.Environment["GUILDNET_BROKER_CA_SHARE"]))
	if own == "" || share == "" {
		t.Fatalf("the console's CA (%q) and its share (%q) should each be a volume of the console's", own, share)
	}
	ownVolume, shareVolume := strings.Split(own, ":")[0], strings.Split(share, ":")[0]
	for name, svc := range s.Services {
		for _, v := range svc.Volumes {
			volume := strings.Split(v, ":")[0]
			if volume == ownVolume && name != "platform" {
				t.Errorf("%s mounts the console's CA (%s)", name, v)
			}
			if volume == shareVolume && name == "runner" && !strings.HasSuffix(v, ":ro") {
				t.Errorf("the runner can change the CA's certificate it trusts: %s", v)
			}
		}
	}
	caFile := fmt.Sprint(runner.Environment["GUILDNET_BROKER_CA_FILE"])
	if m := mountOf(runner, path.Dir(caFile)); !strings.HasPrefix(m, shareVolume+":") || path.Base(caFile) != "ca.crt" {
		t.Errorf("runner's GUILDNET_BROKER_CA_FILE = %q, mounted %q; want ca.crt in the console's share (%s)", caFile, m, shareVolume)
	}
}

// Ergo asks the console about people's logins (guildnet-ircauth), at the
// broker, over TLS trusting only the console's CA, with its own API token,
// which the console knows it by.
func TestErgoAsksTheConsoleAboutLogins(t *testing.T) {
	s := readStack(t)
	ergo, platform := s.Services["ergo"], s.Services["platform"]
	if got := fmt.Sprint(ergo.Environment["GUILDNET_IRC_AUTH_URL"]); !strings.HasPrefix(got, "https://") || !strings.HasSuffix(got, "/irc/login") {
		t.Errorf("ergo's GUILDNET_IRC_AUTH_URL = %q, want the broker's /irc/login (TestTheBrokerIsReachedOnlyOnANetworkOfItsOwn)", got)
	}
	if got := fmt.Sprint(ergo.Environment["GUILDNET_ERGO_TOKEN"]); got != "${GUILDNET_ERGO_API_TOKEN}" {
		t.Errorf("ergo's GUILDNET_ERGO_TOKEN = %q, want its API token", got)
	}
	share := ""
	for _, v := range platform.Volumes {
		if parts := strings.Split(v, ":"); len(parts) >= 2 && parts[1] == fmt.Sprint(platform.Environment["GUILDNET_BROKER_CA_SHARE"]) {
			share = parts[0]
		}
	}
	caFile := fmt.Sprint(ergo.Environment["GUILDNET_BROKER_CA_FILE"])
	if !slices.Contains(ergo.Volumes, share+":"+path.Dir(caFile)+":ro") || path.Base(caFile) != "ca.crt" {
		t.Errorf("ergo's GUILDNET_BROKER_CA_FILE = %q, volumes %q; want ca.crt in the console's share (%s), read-only", caFile, ergo.Volumes, share)
	}
}

// sandboxd, which has Docker's socket, serves TLS with a certificate the
// console gives it, in a volume only the two of them have: sandboxd can't
// change it, and nothing else can read its key. desks.sh points the console
// at it over https.
func TestSandboxdIsReachedOverTLS(t *testing.T) {
	s := readStack(t)
	platform, sandboxd := s.Services["platform"], s.Services["sandboxd"]
	mountOf := func(svc service, dir string) string {
		for _, v := range svc.Volumes {
			if parts := strings.Split(v, ":"); len(parts) >= 2 && parts[1] == dir {
				return v
			}
		}
		return ""
	}
	issued := mountOf(platform, fmt.Sprint(platform.Environment["GUILDNET_SANDBOX_TLS_DIR"]))
	served := mountOf(sandboxd, fmt.Sprint(sandboxd.Environment["GUILDNET_SANDBOX_TLS_DIR"]))
	if issued == "" || served == "" || strings.Split(issued, ":")[0] != strings.Split(served, ":")[0] {
		t.Fatalf("the console's %q and sandboxd's %q should be the one volume", issued, served)
	}
	if !strings.HasSuffix(served, ":ro") {
		t.Errorf("sandboxd can change its own certificate: %s", served)
	}
	volume := strings.Split(served, ":")[0]
	for name, svc := range s.Services {
		for _, v := range svc.Volumes {
			if strings.Split(v, ":")[0] == volume && name != "platform" && name != "sandboxd" {
				t.Errorf("%s mounts sandboxd's certificate and key (%s)", name, v)
			}
		}
	}
	b, err := os.ReadFile("desks.sh")
	if err != nil || !strings.Contains(string(b), "GUILDNET_SANDBOX_URL=https://") {
		t.Error("desks.sh should point the console at sandboxd over https")
	}
}

// requiredVars are the settings compose.yml uses with no default: init.sh
// must write each one.
func requiredVars(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile("compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	var vars []string
	for _, m := range regexp.MustCompile(`\$\{([A-Z0-9_]+)(:?[-?+][^}]*)?\}`).FindAllStringSubmatch(string(b), -1) {
		if strings.HasPrefix(strings.TrimPrefix(m[2], ":"), "-") {
			continue // has a default
		}
		if !slices.Contains(vars, m[1]) {
			vars = append(vars, m[1])
		}
	}
	if len(vars) == 0 {
		t.Fatal("compose.yml uses no settings at all")
	}
	return vars
}

func envOf(t *testing.T, s string) map[string]string {
	t.Helper()
	env := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Errorf(".env line %q isn't KEY=value", line)
			continue
		}
		env[k] = v
	}
	return env
}

// restore.sh puts back everything backup.sh makes, and needs nothing else.
func TestRestoreTakesWhatBackupMakes(t *testing.T) {
	read := func(name string) string {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	var made []string
	for _, m := range regexp.MustCompile(`"\$dir/([a-z.-]+)"`).FindAllStringSubmatch(read("backup.sh"), -1) {
		if !slices.Contains(made, m[1]) {
			made = append(made, m[1])
		}
	}
	m := regexp.MustCompile(`(?m)^for f in ([a-z. -]+); do`).FindStringSubmatch(read("restore.sh"))
	if m == nil {
		t.Fatal("restore.sh doesn't list what a backup has")
	}
	needed := strings.Fields(m[1])
	// backup.sh dumps its databases in a loop, each to <db>.dump.
	dbs := regexp.MustCompile(`(?m)^for db in ([a-z ]+); do`).FindStringSubmatch(read("backup.sh"))
	if dbs == nil {
		t.Fatal("backup.sh dumps no databases")
	}
	for _, db := range strings.Fields(dbs[1]) {
		made = append(made, db+".dump")
	}
	slices.Sort(made)
	slices.Sort(needed)
	if !slices.Equal(made, needed) {
		t.Errorf("backup.sh makes %v; restore.sh needs %v", made, needed)
	}
}

// The images are built from the repo, where an install keeps its .env, its
// brand files and its backups: none of them go into a build (or its cache,
// which outlives the backups pruned away).
func TestBuildsDontTakeTheInstallsSecretsOrBackups(t *testing.T) {
	if os.Getenv("GUILDNET_DOCKER_TESTS") != "1" {
		t.Skip("set GUILDNET_DOCKER_TESTS=1 to test against Docker")
	}
	mark := "sentinel-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	// Cleaned up last first: the folders, if this made them (Remove leaves
	// one that isn't empty), after the backup's, after the files.
	t.Cleanup(func() { os.Remove("brand"); os.Remove("backups") })
	t.Cleanup(func() { os.Remove(filepath.Join("backups", mark)) })
	for _, f := range []string{".env." + mark, filepath.Join("backups", mark, "env"), filepath.Join("brand", mark)} {
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Remove(f) })
	}
	cmd := exec.Command("docker", "build", "--no-cache", "--progress=plain", "-f", "-", filepath.Join("..", ".."))
	cmd.Stdin = strings.NewReader("FROM alpine:3.22\nCOPY . /ctx\nRUN find /ctx -name '*" + mark + "*'\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("building: %v\n%s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "/ctx/") && strings.Contains(line, mark) {
			t.Errorf("the build took %s", line)
		}
	}
}

// docker runs docker, failing the test if it fails, and returns what it
// printed: on stdout only, so an image it pulled first (whose progress is on
// stderr) doesn't come back as part of a container's ID.
func docker(t *testing.T, args ...string) string {
	t.Helper()
	var stderr strings.Builder
	cmd := exec.Command("docker", args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s%s", strings.Join(args, " "), err, out, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

// The edge sends the web to the console and IRC to Ergo at their addresses
// in the stack, unless its settings say others (in one pod or task, say,
// where everything is on localhost).
func TestTheEdgesUpstreamsAreSettings(t *testing.T) {
	if os.Getenv("GUILDNET_DOCKER_TESTS") != "1" {
		t.Skip("set GUILDNET_DOCKER_TESTS=1 to test against Docker")
	}
	image := "guildnet-selfhost-test-caddy"
	docker(t, "build", "--quiet", "--tag", image, "../caddy")
	adapt := func(env ...string) string {
		args := []string{"run", "--rm", "-e", "GUILDNET_DOMAIN=chat.example.com", "-e", "GUILDNET_NETWORK=hq"}
		for _, e := range env {
			args = append(args, "-e", e)
		}
		return docker(t, append(args, image, "caddy", "adapt", "--config", "/etc/caddy/Caddyfile")...)
	}
	// upstreams are every address the config dials, sorted.
	upstreams := func(config string) []string {
		var v any
		if err := json.Unmarshal([]byte(config), &v); err != nil {
			t.Fatalf("caddy adapt: %v\n%s", err, config)
		}
		var got []string
		var walk func(any)
		walk = func(v any) {
			switch v := v.(type) {
			case map[string]any:
				for k, x := range v {
					switch d := x.(type) {
					case string:
						if k == "dial" {
							got = append(got, d)
						}
					case []any:
						if k == "dial" {
							for _, s := range d {
								got = append(got, fmt.Sprint(s))
							}
							continue
						}
					}
					walk(x)
				}
			case []any:
				for _, x := range v {
					walk(x)
				}
			}
		}
		walk(v)
		slices.Sort(got)
		return slices.Compact(got)
	}
	if got, want := upstreams(adapt()), []string{"ergo:6668", "platform:6699", "platform:8080"}; !slices.Equal(got, want) {
		t.Errorf("by default the edge sends to %v, want %v", got, want)
	}
	got := upstreams(adapt("GUILDNET_CONSOLE_UPSTREAM=localhost:8080", "GUILDNET_IRC_UPSTREAM=localhost:6668", "GUILDNET_IRC_HINT_UPSTREAM=localhost:6699"))
	if want := []string{"localhost:6668", "localhost:6699", "localhost:8080"}; !slices.Equal(got, want) {
		t.Errorf("with settings the edge sends to %v, want %v", got, want)
	}
}

// Postgres on a new volume gives the console and the runner a database and
// login each: neither login reaches the other's database, or the admin's.
func TestTheConsoleAndTheRunnerEachHaveTheirOwnDatabase(t *testing.T) {
	if os.Getenv("GUILDNET_DOCKER_TESTS") != "1" {
		t.Skip("set GUILDNET_DOCKER_TESTS=1 to test against Docker")
	}
	image := "guildnet-selfhost-test-postgres"
	docker(t, "build", "--quiet", "--tag", image, "../postgres")
	id := docker(t, "run", "-d", "--rm", "-e", "POSTGRES_USER=guildnet", "-e", "POSTGRES_DB=guildnet", "-e", "POSTGRES_PASSWORD=admin-pw",
		"-e", "PLATFORM_DB_PASSWORD=platform-pw", "-e", "RUNNER_DB_PASSWORD=runner-pw", image)
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", id).Run() })
	deadline := time.Now().Add(time.Minute)
	for exec.Command("docker", "exec", id, "pg_isready", "-h", "127.0.0.1", "-U", "guildnet", "-d", "guildnet").Run() != nil {
		if time.Now().After(deadline) {
			t.Fatal("postgres didn't start")
		}
		time.Sleep(500 * time.Millisecond)
	}
	// From another address, as the stack connects: inside, the socket and
	// 127.0.0.1 are trusted without a password.
	ip := docker(t, "inspect", "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", id)
	connects := func(user, pw, db string) bool {
		return exec.Command("docker", "exec", "-e", "PGPASSWORD="+pw, id, "psql", "-h", ip, "-U", user, "-d", db, "-c", "select 1").Run() == nil
	}
	for _, c := range []struct {
		user, pw, db string
		want         bool
	}{
		{"platform", "platform-pw", "platform", true},
		{"runner", "runner-pw", "runner", true},
		{"platform", "platform-pw", "runner", false},
		{"runner", "runner-pw", "platform", false},
		{"runner", "runner-pw", "guildnet", false},
		{"platform", "platform-pw", "guildnet", false},
		{"runner", "wrong-pw", "runner", false},
	} {
		if got := connects(c.user, c.pw, c.db); got != c.want {
			t.Errorf("%s connecting to %s: %v, want %v", c.user, c.db, got, c.want)
		}
	}
}

// On managed Postgres (Cloud SQL, RDS) the admin isn't a superuser, only a
// role that may make roles and databases; and the scripts may run more than
// once (a job run again). They still give the console and the runner a
// database and login each, the admin keeps no way into either, and running
// them again changes nothing.
func TestTheDatabasesCanBeMadeOnManagedPostgres(t *testing.T) {
	if os.Getenv("GUILDNET_DOCKER_TESTS") != "1" {
		t.Skip("set GUILDNET_DOCKER_TESTS=1 to test against Docker")
	}
	// The image's own base, without its init scripts: an empty server, as a
	// managed one starts. The scripts are run against it as its admin.
	dockerfile, err := os.ReadFile("../postgres/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	base := regexp.MustCompile(`(?m)^FROM (\S+)`).FindStringSubmatch(string(dockerfile))[1]
	id := docker(t, "run", "-d", "--rm", "-e", "POSTGRES_USER=guildnet", "-e", "POSTGRES_PASSWORD=su-pw", base)
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", id).Run() })
	for _, script := range []string{"10-platform.sh", "20-runner.sh"} {
		docker(t, "cp", filepath.Join("..", "postgres", script), id+":/tmp/"+script)
	}
	deadline := time.Now().Add(time.Minute)
	for exec.Command("docker", "exec", id, "pg_isready", "-h", "127.0.0.1", "-U", "guildnet").Run() != nil {
		if time.Now().After(deadline) {
			t.Fatal("postgres didn't start")
		}
		time.Sleep(500 * time.Millisecond)
	}
	docker(t, "exec", id, "psql", "-h", "127.0.0.1", "-U", "guildnet", "-v", "ON_ERROR_STOP=1", "-c", "create role admin login password 'admin-pw' createrole createdb")
	ip := docker(t, "inspect", "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", id)
	for range 2 {
		for _, script := range []string{"10-platform.sh", "20-runner.sh"} {
			docker(t, "exec", "-e", "PGHOST="+ip, "-e", "PGPASSWORD=admin-pw", "-e", "POSTGRES_USER=admin", "-e", "POSTGRES_DB=postgres",
				"-e", "PLATFORM_DB_PASSWORD=platform-pw", "-e", "RUNNER_DB_PASSWORD=runner-pw", id, "sh", "/tmp/"+script)
		}
	}
	connects := func(user, pw, db string) bool {
		return exec.Command("docker", "exec", "-e", "PGPASSWORD="+pw, id, "psql", "-h", ip, "-U", user, "-d", db, "-c", "select 1").Run() == nil
	}
	for _, c := range []struct {
		user, pw, db string
		want         bool
	}{
		{"platform", "platform-pw", "platform", true},
		{"runner", "runner-pw", "runner", true},
		{"platform", "platform-pw", "runner", false},
		{"runner", "runner-pw", "platform", false},
	} {
		if got := connects(c.user, c.pw, c.db); got != c.want {
			t.Errorf("%s connecting to %s: %v, want %v", c.user, c.db, got, c.want)
		}
	}
	if m := docker(t, "exec", id, "psql", "-h", "127.0.0.1", "-U", "guildnet", "-tAc",
		"select count(*) from pg_auth_members m join pg_roles r on r.oid = m.member where r.rolname = 'admin' and m.roleid in (select oid from pg_roles where rolname in ('platform', 'runner')) and (m.set_option or m.inherit_option)"); m != "0" {
		t.Errorf("the admin can still act as the console's or the runner's login (%s memberships)", m)
	}
}

// Ergo is given GuildServ's operator password as the runner has it, and
// hashes it itself as it starts: no hash to make by hand.
func TestErgoTakesGuildServsPasswordAsItIs(t *testing.T) {
	if os.Getenv("GUILDNET_DOCKER_TESTS") != "1" {
		t.Skip("set GUILDNET_DOCKER_TESTS=1 to test against Docker")
	}
	image := "guildnet-selfhost-test-ergo"
	docker(t, "build", "--quiet", "--tag", image, "../ergo")
	id := docker(t, "run", "-d", "--rm", "--user", "65534:65534", "-p", "127.0.0.1::6667",
		"-e", "GUILDNET_OPER_PASSWORD=Gs-0per_pw", "-e", "ERGO__OPERS__GUILDSERV__CLASS=guildserv",
		// start.sh won't start Ergo without one.
		"-e", `ERGO__API__BEARER_TOKENS=["api-token"]`,
		// Connecting without an account, to try OPER alone.
		"-e", "ERGO__ACCOUNTS__REQUIRE_SASL__ENABLED=false", image)
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", id).Run() })
	var addr string
	for deadline := time.Now().Add(20 * time.Second); addr == "" && time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if out, err := exec.Command("docker", "port", id, "6667/tcp").Output(); err == nil {
			addr = strings.Split(strings.TrimSpace(string(out)), "\n")[0]
		}
	}
	if addr == "" {
		t.Fatalf("ergo's port wasn't published:\n%s", docker(t, "logs", id))
	}

	// oper connects as nick (each attempt its own: the last may still be
	// leaving) and answers the reply to OPER. Docker publishes the port
	// before Ergo listens, and closes what it accepts until then: a
	// connection closed before Ergo says anything is tried again.
	oper := func(nick, password string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var said []string
		var err error
		for ctx.Err() == nil {
			var conn net.Conn
			if conn, err = (&net.Dialer{}).DialContext(ctx, "tcp", addr); err != nil {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			conn.SetDeadline(time.Now().Add(15 * time.Second))
			fmt.Fprintf(conn, "NICK %s\r\nUSER %s 0 * :%s\r\n", nick, nick, nick)
			sc := bufio.NewScanner(conn)
			said = nil
			opered := false
			for sc.Scan() {
				said = append(said, sc.Text())
				f := strings.Fields(sc.Text())
				switch {
				case len(f) > 1 && f[0] == "PING":
					fmt.Fprintf(conn, "PONG %s\r\n", f[1])
				case len(f) > 1 && f[1] == "001": // welcome
					fmt.Fprintf(conn, "OPER guildserv %s\r\n", password)
					opered = true
				case len(f) > 1 && opered && (f[1] == "381" || f[1][0] == '4' || f[1][0] == '5'):
					conn.Close()
					return f[1]
				case len(f) > 1 && !opered && (f[1][0] == '4' || f[1][0] == '5'):
					conn.Close()
					t.Fatalf("connecting as %s: %s", nick, sc.Text())
				}
			}
			err = sc.Err()
			conn.Close()
			if len(said) > 0 {
				break // Ergo answered, then hung up: no point trying again
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("no answer to OPER (%v); ergo said:\n%s\nits log:\n%s", err, strings.Join(said, "\n"), docker(t, "logs", id))
		return ""
	}
	if got := oper("right", "Gs-0per_pw"); got != "381" { // RPL_YOUREOPER
		t.Errorf("OPER with the password: %s, want 381", got)
	}
	if got := oper("wrong", "not-it"); got == "381" {
		t.Errorf("OPER with the wrong password worked")
	}
	if env := docker(t, "exec", id, "sh", "-c", "tr '\\0' '\\n' < /proc/1/environ"); strings.Contains(env, "Gs-0per_pw") {
		t.Errorf("ergo keeps the password in its environment once it's hashed")
	}
}

// The runner shares /welcome with Ergo and rewrites the welcome there, but
// can't make Ergo send something else of its own: a welcome swapped for a
// link (to Ergo's account database, say) is refused when Ergo rereads it.
// One the runner writes as a file of its own is still sent.
func TestErgoSendsOnlyAWelcomeThatIsAFile(t *testing.T) {
	if os.Getenv("GUILDNET_DOCKER_TESTS") != "1" {
		t.Skip("set GUILDNET_DOCKER_TESTS=1 to test against Docker")
	}
	image := "guildnet-selfhost-test-ergo"
	docker(t, "build", "--quiet", "--tag", image, "../ergo")
	id := docker(t, "run", "-d", "--rm", "--user", "65534:65534", "-p", "127.0.0.1::6667",
		"-e", "GUILDNET_OPER_PASSWORD=Gs-0per_pw", "-e", "ERGO__OPERS__GUILDSERV__CLASS=guildserv",
		"-e", `ERGO__API__BEARER_TOKENS=["api-token"]`, "-e", "ERGO__ACCOUNTS__REQUIRE_SASL__ENABLED=false", image)
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", id).Run() })
	var addr string
	for deadline := time.Now().Add(20 * time.Second); addr == "" && time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if out, err := exec.Command("docker", "port", id, "6667/tcp").Output(); err == nil {
			addr = strings.Split(strings.TrimSpace(string(out)), "\n")[0]
		}
	}
	if addr == "" {
		t.Fatalf("ergo's port wasn't published:\n%s", docker(t, "logs", id))
	}
	// Connected and opered as GuildServ, as the runner is. Docker publishes
	// the port before Ergo listens: a connection closed before Ergo says
	// anything is tried again.
	var conn net.Conn
	var sc *bufio.Scanner
	for deadline := time.Now().Add(30 * time.Second); conn == nil; time.Sleep(200 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("couldn't connect to ergo; its log:\n%s", docker(t, "logs", id))
		}
		c, err := net.Dial("tcp", addr)
		if err != nil {
			continue
		}
		c.SetDeadline(time.Now().Add(time.Minute))
		fmt.Fprintf(c, "NICK gs\r\nUSER gs 0 * :gs\r\n")
		sc = bufio.NewScanner(c)
		if sc.Scan() {
			conn = c
		} else {
			c.Close()
		}
	}
	defer conn.Close()
	// until reads to the first of these replies and answers it.
	until := func(replies ...string) string {
		t.Helper()
		var said []string
		for sc.Scan() {
			said = append(said, sc.Text())
			f := strings.Fields(sc.Text())
			if len(f) > 1 && f[0] == "PING" {
				fmt.Fprintf(conn, "PONG %s\r\n", f[1])
			}
			if len(f) > 1 && slices.Contains(replies, f[1]) {
				return f[1]
			}
		}
		t.Fatalf("no %v from ergo (%v); it said:\n%s\nits log:\n%s", replies, sc.Err(), strings.Join(said, "\n"), docker(t, "logs", id))
		return ""
	}
	if !strings.Contains(sc.Text(), " 001 ") {
		until("001")
	}
	fmt.Fprintf(conn, "OPER guildserv Gs-0per_pw\r\n")
	if got := until("381", "464", "491"); got != "381" {
		t.Fatalf("OPER: %s", got)
	}
	// motd rereads the welcome and says what Ergo sends now.
	motd := func() string {
		t.Helper()
		fmt.Fprintf(conn, "REHASH\r\n")
		if got := until("NOTICE", "400", "481"); got != "NOTICE" { // "Rehash complete"
			t.Fatalf("REHASH: %s", got)
		}
		fmt.Fprintf(conn, "MOTD\r\n")
		var lines []string
		for sc.Scan() {
			f := strings.SplitN(sc.Text(), " ", 4)
			switch {
			case len(f) == 4 && f[1] == "372":
				lines = append(lines, f[3])
			case len(f) > 1 && (f[1] == "376" || f[1] == "422"):
				return strings.Join(lines, "\n")
			}
		}
		t.Fatalf("no end to the MOTD (%v)", sc.Err())
		return ""
	}
	runner := func(script string) {
		t.Helper()
		docker(t, "exec", "--user", "65532:65532", id, "sh", "-c", script)
	}
	runner("ln -sfn /ircd/ircd.db /welcome/guildnet.motd")
	if got := motd(); got != "" {
		t.Errorf("with the welcome a link to Ergo's database, Ergo sent:\n%s", got)
	}
	runner("printf 'Made by GuildServ\\n' > /welcome/.new && mv -f /welcome/.new /welcome/guildnet.motd")
	if got := motd(); !strings.Contains(got, "Made by GuildServ") {
		t.Errorf("with the runner's own welcome, Ergo sent %q", got)
	}
}
