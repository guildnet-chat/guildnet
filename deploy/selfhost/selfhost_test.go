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
}

type stack struct {
	Services map[string]service `yaml:"services"`
	Networks map[string]struct {
		Name     string `yaml:"name"`
		Internal bool   `yaml:"internal"`
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
		"caddy": {"NET_BIND_SERVICE"}, // ports 80 and 443
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
	readOnly := []string{"platform", "runner", "ergo", "egress"}
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

// The runner gets its settings and keys from the console's broker, and
// nothing about the network but its name and secrets from the stack.
func TestTheRunnerWaitsForFirstRunSetup(t *testing.T) {
	r := readStack(t).Services["runner"]
	for k, want := range map[string]string{
		"GUILDNET_SETTINGS_FROM_BROKER": "on",
		"GUILDNET_KEYS_BROKER":          "https://platform:8082",
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
		if got := fmt.Sprint(runner.Environment[k]); !strings.HasPrefix(got, "https://platform:") {
			t.Errorf("runner's %s = %q, want https://platform:...", k, got)
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

// docker runs docker, failing the test if it fails.
func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
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
		"-e", "GUILDNET_OPER_PASSWORD=Gs-0per_pw", "-e", "ERGO__OPERS__GUILDSERV__CLASS=server-admin",
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
