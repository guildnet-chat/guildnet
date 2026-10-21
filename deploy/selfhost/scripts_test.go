// SPDX-License-Identifier: AGPL-3.0-only

package selfhost_test

import (
	"archive/tar"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
)

// The install's scripts run on the machine's own shell: Linux's, macOS's,
// or Git Bash's on Windows. These tests run them there too (CI runs them on
// all three), with a docker that only says what it was asked
// (testdata/fakedocker), so they need neither Docker nor a Linux.

var fakeDocker struct {
	once sync.Once
	dir  string
	err  error
}

// fakeBin is a folder holding fakedocker as docker (built once, for this
// machine) and a systemctl that says the desks' firewall is set up unless
// $T/firewall-off says it isn't.
func fakeBin(t *testing.T) string {
	t.Helper()
	fakeDocker.once.Do(func() {
		dir, err := os.MkdirTemp("", "fakedocker")
		if err != nil {
			fakeDocker.err = err
			return
		}
		name := "docker"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		if out, err := exec.Command("go", "build", "-o", filepath.Join(dir, name), "./testdata/fakedocker").CombinedOutput(); err != nil {
			fakeDocker.err = err
			fakeDocker.dir = string(out)
			return
		}
		systemctl := "#!/bin/sh\ncase \"$*\" in *is-active*guildnet-desks-firewall*) test ! -e \"$T/firewall-off\" ;; *) exit 1 ;; esac\n"
		fakeDocker.err = os.WriteFile(filepath.Join(dir, "systemctl"), []byte(systemctl), 0o755)
		fakeDocker.dir = dir
	})
	if fakeDocker.err != nil {
		t.Fatalf("building fakedocker: %v\n%s", fakeDocker.err, fakeDocker.dir)
	}
	return fakeDocker.dir
}

// install is an install's folder (deploy/selfhost, in a copy of the repo
// with its VERSION) with some of its scripts, a compose.yml to keep, and a
// scratch folder beside it ($T to the scripts' shell).
type install struct {
	t        *testing.T
	sh       string
	dir      string
	scratch  string
	runtimes string // what docker info says Docker's runtimes are
}

func newInstall(t *testing.T, scripts ...string) *install {
	t.Helper()
	return newInstallOf(t, "selfhost", scripts...)
}

// newInstallOf is an install of deploy/folder's (selfhost, or desks-host)
// with some of its scripts.
func newInstallOf(t *testing.T, folder string, scripts ...string) *install {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("no sh to run the scripts with: CI runs them on every system")
		}
		t.Skip("no sh here to run the scripts with (on Windows, Git Bash's)")
	}
	repo := t.TempDir()
	s := &install{t: t, sh: sh, dir: filepath.Join(repo, "deploy", folder), scratch: t.TempDir()}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	version, err := os.ReadFile("../../VERSION")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "VERSION"), version, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, f := range scripts {
		b, err := os.ReadFile(filepath.Join("..", folder, f))
		if err != nil {
			t.Fatal(err)
		}
		s.write(f, string(b))
	}
	s.write("compose.yml", "services: {}\n")
	return s
}

func (s *install) write(name, content string) {
	s.t.Helper()
	if err := os.WriteFile(filepath.Join(s.dir, name), []byte(content), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

// read is a file in the stack ("" if it isn't there).
func (s *install) read(name string) string {
	b, _ := os.ReadFile(filepath.Join(s.dir, name))
	return string(b)
}

// run runs script in the stack's folder with the machine's sh, docker
// standing in as fakedocker, and returns what it printed.
func (s *install) run(script string) (string, error) {
	s.t.Helper()
	cmd := exec.Command(s.sh, "-c", script)
	cmd.Dir = s.dir
	cmd.Env = append(os.Environ(),
		"PATH="+fakeBin(s.t)+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKEDOCKER_LOG="+filepath.Join(s.scratch, "calls"),
		"FAKEDOCKER_RUNTIMES="+s.runtimes,
		"T="+filepath.ToSlash(s.scratch))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// calls are what docker was asked, a line each.
func (s *install) calls() string {
	b, _ := os.ReadFile(filepath.Join(s.scratch, "calls"))
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// init.sh writes .env, readable only by whoever ran it, with the domain, the
// network's name and every secret the stack needs, each its own; and shows
// none of them.
func TestInitMakesEverySecretAndShowsNone(t *testing.T) {
	s := newInstall(t, "init.sh")
	said, err := s.run("sh init.sh guildnet.example.com acme")
	if err != nil {
		t.Fatalf("init.sh: %v\n%s", err, said)
	}
	if runtime.GOOS != "windows" { // files there have no Unix modes
		info, err := os.Stat(filepath.Join(s.dir, ".env"))
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf(".env's mode is %o, want 600", mode)
		}
	}
	env := envOf(t, s.read(".env"))
	if env["GUILDNET_DOMAIN"] != "guildnet.example.com" || env["GUILDNET_NETWORK"] != "acme" {
		t.Errorf("domain %q, network %q; want guildnet.example.com, acme", env["GUILDNET_DOMAIN"], env["GUILDNET_NETWORK"])
	}
	seen := map[string]string{}
	for _, k := range requiredVars(t) {
		v, ok := env[k]
		if !ok || v == "" {
			t.Errorf("init.sh doesn't write %s, which compose.yml needs", k)
			continue
		}
		if k == "GUILDNET_DOMAIN" || k == "GUILDNET_NETWORK" || k == "GUILDNET_VERSION" {
			continue
		}
		if len(v) < 32 {
			t.Errorf("%s is %d characters; want a long random secret", k, len(v))
		}
		if other, dup := seen[v]; dup {
			t.Errorf("%s and %s are the same secret", k, other)
		}
		seen[v] = k
		if strings.Contains(said, v) {
			t.Errorf("init.sh showed %s", k)
		}
		if strings.ContainsAny(v, "$'\" \\`\r") {
			t.Errorf("%s has characters Compose or a shell would read: %q", k, v)
		}
	}
	if !strings.Contains(said, "docker compose up") {
		t.Errorf("init.sh should say what to do next; it said:\n%s", said)
	}
}

// An install runs the release it came with, until it's upgraded on purpose
// (to the release a backup was made with, say): init.sh pins it in .env,
// and says to fetch its images, or build them from source.
func TestInitPinsTheRelease(t *testing.T) {
	s := newInstall(t, "init.sh")
	said, err := s.run("sh init.sh guildnet.example.com acme")
	if err != nil {
		t.Fatalf("%v\n%s", err, said)
	}
	version, _ := os.ReadFile("../../VERSION")
	if got, want := envOf(t, s.read(".env"))["GUILDNET_VERSION"], strings.TrimSpace(string(version)); got != want {
		t.Errorf("GUILDNET_VERSION=%q, want this release, %q", got, want)
	}
	for _, want := range []string{"docker compose pull", "docker compose up -d", "--build"} {
		if !strings.Contains(said, want) {
			t.Errorf("init.sh should say %q next; it said:\n%s", want, said)
		}
	}
}

// Without the release's name beside it, init.sh can't pin one: it says so
// and writes nothing.
func TestInitNeedsTheReleasesName(t *testing.T) {
	for name, version := range map[string]string{"no VERSION": "", "not a release": "main\n", "two lines": "26.10.0\nGUILDNET_X=1\n"} {
		s := newInstall(t, "init.sh")
		script := "rm ../../VERSION && sh init.sh guildnet.example.com acme"
		if version != "" {
			script = "printf '" + strings.ReplaceAll(version, "\n", `\n`) + "' > ../../VERSION && sh init.sh guildnet.example.com acme"
		}
		out, err := s.run(script)
		if err == nil || s.read(".env") != "" || !strings.Contains(out, "VERSION") {
			t.Errorf("%s: init.sh should refuse, naming VERSION, and write nothing:\n%s", name, out)
		}
	}
}

// The console seals what it keeps with GUILDNET_PLATFORM_KEY: 32 random bytes.
func TestInitsPlatformKeyIs32Bytes(t *testing.T) {
	s := newInstall(t, "init.sh")
	if out, err := s.run("sh init.sh guildnet.example.com acme"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	key, err := base64.StdEncoding.DecodeString(envOf(t, s.read(".env"))["GUILDNET_PLATFORM_KEY"])
	if err != nil || len(key) != 32 {
		t.Errorf("GUILDNET_PLATFORM_KEY is %d bytes (%v), want 32", len(key), err)
	}
}

// Once data is made with them the secrets can't change: init.sh never
// replaces an .env that's there.
func TestInitKeepsAnEnvThatsThere(t *testing.T) {
	s := newInstall(t, "init.sh")
	if out, err := s.run("sh init.sh guildnet.example.com acme"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	before := s.read(".env")
	if out, err := s.run("sh init.sh other.example.com hq"); err == nil {
		t.Errorf("a second init.sh should fail:\n%s", out)
	}
	if s.read(".env") != before {
		t.Error("a second init.sh changed .env")
	}
}

func TestInitRefusesWhatCantBeADomainOrANetwork(t *testing.T) {
	for _, args := range []string{"", "example", "Example.COM hq", "example.com HQ", "example.com h", "example.com -hq", "example.com hq--x", "'exa mple.com' hq", "example.com hq extra",
		// A line each grep checks is fine, but .env would get a line more.
		`"$(printf 'example.com\nx.com')" hq`, `example.com "$(printf 'hq\nGUILDNET_X')"`} {
		s := newInstall(t, "init.sh")
		out, err := s.run("sh init.sh " + args)
		if err == nil || s.read(".env") != "" {
			t.Errorf("init.sh %s: accepted it (or wrote .env):\n%s", args, out)
		}
	}
}

// backup.sh copies everything the install keeps into backups/<time>: its
// .env, its brand files, both databases, and Ergo's and the runner's files,
// with those two paused only while their files are copied.
func TestBackupKeepsEverythingTheInstallHas(t *testing.T) {
	s := newInstall(t, "init.sh", "backup.sh")
	out, err := s.run("sh init.sh guildnet.example.com hq >/dev/null && docker compose up -d && mkdir -p brand && echo '<svg/>' > brand/logo.svg && sh backup.sh")
	if err != nil {
		t.Fatalf("backup.sh: %v\n%s", err, out)
	}
	backups, _ := filepath.Glob(filepath.Join(s.dir, "backups", "*"))
	if len(backups) != 1 {
		t.Fatalf("backups: %v; want one\n%s", backups, out)
	}
	got := func(name string) string {
		b, err := os.ReadFile(filepath.Join(backups[0], name))
		if err != nil {
			t.Errorf("the backup has no %s", name)
		}
		return string(b)
	}
	if got("env") != s.read(".env") {
		t.Error("the backup's env isn't the install's .env")
	}
	for db, want := range map[string]string{"platform.dump": "dump of platform", "runner.dump": "dump of runner"} {
		if !strings.Contains(got(db), want) {
			t.Errorf("%s isn't the %s database's dump", db, strings.TrimSuffix(db, ".dump"))
		}
	}
	// Whole, and only the files: were Compose to fetch or build the files
	// service's image first, it would print how it went where the files go.
	files := tar.NewReader(strings.NewReader(got("files.tar")))
	if h, err := files.Next(); err != nil || h.Name != "ergo/ircd.db" {
		t.Errorf("files.tar isn't the files service's tar of Ergo's and the runner's files (%v):\n%.80q", err, got("files.tar"))
	}
	if !strings.Contains(got("brand.tar"), "<svg/>") {
		t.Error("brand.tar doesn't have the brand files")
	}
	calls := s.calls()
	pause, copy, unpause := strings.Index(calls, "compose pause ergo runner"), strings.Index(calls, "files -cf - -C /files ."), strings.Index(calls, "compose unpause ergo runner")
	if pause < 0 || copy < pause || unpause < copy {
		t.Errorf("backup.sh should pause Ergo and the runner, copy /files, then unpause them; docker was asked:\n%s", calls)
	}
	// The copy uses the image Postgres runs: a backup never changes an image.
	if strings.Contains(calls, "compose build") || strings.Contains(calls, "compose pull") {
		t.Errorf("backup.sh built or fetched an image:\n%s", calls)
	}
}

// A backup that didn't come out whole isn't called one: backup.sh fails,
// saying so, and Ergo and the runner are running again.
func TestBackupSaysSoWhenItIsntWhole(t *testing.T) {
	s := newInstall(t, "init.sh", "backup.sh")
	out, err := s.run("sh init.sh guildnet.example.com hq >/dev/null && docker compose up -d && mkdir -p brand && FAKEDOCKER_BROKEN=files sh backup.sh")
	if err == nil || strings.Contains(out, "backed up to") {
		t.Errorf("backup.sh called a broken copy of the files a backup:\n%s", out)
	}
	if !strings.HasSuffix(strings.TrimSpace(s.calls()), "compose unpause ergo runner") {
		t.Errorf("Ergo and the runner should be running again; docker was last asked:\n%s", s.calls())
	}
}

// restore.sh puts a backup back into an install with no data, and starts
// it, asking docker exactly what it means to on every shell.
func TestRestorePutsABackupBack(t *testing.T) {
	s := newInstall(t, "restore.sh")
	if out, err := s.run(makeBackup("")); err != nil {
		t.Fatalf("making the backup: %v\n%s", err, out)
	}
	out, err := s.run(`sh restore.sh "$T/backup"`)
	if err != nil {
		t.Fatalf("restore.sh: %v\n%s", err, out)
	}
	if !strings.Contains(s.read("brand/logo.svg"), "<svg/>") {
		t.Error("restore.sh didn't put the brand files back")
	}
	if !strings.Contains(s.read(".env"), "GUILDNET_DOMAIN=example.com") {
		t.Error("restore.sh didn't make the backup's env this install's .env")
	}
	calls := strings.Split(strings.TrimSpace(s.calls()), "\n")
	for _, want := range []string{"docker compose run --rm -T files -xf - -C /files", "docker compose up -d"} {
		if !slices.Contains(calls, want) {
			t.Errorf("restore.sh should ask docker %q; it asked:\n%s", want, strings.Join(calls, "\n"))
		}
	}
}

// restore.sh trusts nothing in a backup but what a backup has: brand files
// (nothing else of the stack's, and no links), and the stack's settings in
// .env. A backup someone changed, where it was kept, can't swap compose.yml
// or point Compose at another, and so take over the machine it's restored
// on.
func TestRestoreTrustsOnlyWhatABackupShouldHave(t *testing.T) {
	tampered := map[string]string{
		"compose.yml in the brand files": "mkdir x; echo 'services: {evil: {privileged: true}}' > x/compose.yml; tar -cf brand.tar -C x compose.yml",
		"a path out of brand/":           "mkdir -p brand x; echo evil > x/c; tar -cf brand.tar brand x/c",
		"Compose pointed elsewhere":      "echo 'COMPOSE_FILE=evil.yml' >> env",
		"a line that isn't a setting":    "echo 'x' >> env",
		// The images the stack runs come from GuildNet's registry, unless
		// the one restoring says otherwise.
		"images from elsewhere":       "echo 'GUILDNET_REGISTRY=evil.example/x' >> env",
		"a desk image from elsewhere": "echo 'GUILDNET_DESK_IMAGE=evil.example/desk' >> env",
	}
	if runtime.GOOS != "windows" { // Git Bash can't make links without being an administrator
		tampered["a link in the brand files"] = "mkdir brand; ln -s ../stack/compose.yml brand/logo.svg; tar -cf brand.tar brand"
	}
	for name, tamper := range tampered {
		s := newInstall(t, "restore.sh")
		if out, err := s.run(makeBackup(tamper)); err != nil {
			t.Fatalf("%s: making the backup: %v\n%s", name, err, out)
		}
		out, err := s.run(`sh restore.sh "$T/backup"`)
		if err == nil || strings.Contains(s.read("compose.yml"), "evil") || strings.Contains(s.calls(), "compose up") {
			t.Errorf("%s: restored it, or started something:\n%s", name, out)
		}
	}
}

// The release a restored install runs is this checkout's, not what the
// backup says: a backup can't take the install back to an older release.
func TestRestoreRunsThisCheckoutsRelease(t *testing.T) {
	s := newInstall(t, "restore.sh")
	if out, err := s.run(makeBackup("echo 'GUILDNET_VERSION=25.01.0' >> env")); err != nil {
		t.Fatalf("making the backup: %v\n%s", err, out)
	}
	if out, err := s.run(`sh restore.sh "$T/backup"`); err != nil {
		t.Fatalf("restore.sh: %v\n%s", err, out)
	}
	env := s.read(".env")
	if strings.Contains(env, "GUILDNET_VERSION=25.01.0") || !strings.Contains(env, "GUILDNET_VERSION="+strings.TrimSpace(s.read("../../VERSION"))) {
		t.Errorf("the restored install's release:\n%s", env)
	}
}

// makeBackup is a shell script making, at $T/backup, a backup as backup.sh
// makes one, then changed by tamper (run in the backup).
func makeBackup(tamper string) string {
	return `set -e; mkdir "$T/backup"; cd "$T/backup"
mkdir brand; echo '<svg/>' > brand/logo.svg; tar -cf brand.tar brand; rm -r brand
printf 'GUILDNET_DOMAIN=example.com\nGUILDNET_NETWORK=hq\n# a comment\n\nCOMPOSE_PROFILES=desks\n' > env
touch platform.dump runner.dump files.tar
` + tamper
}

// desks.sh turns desks on: a key pair (the console's half to sign tokens
// with, sandboxd's to check them) and sandboxd's address go in .env, never
// shown, and the desks profile is on.
func TestDesksShTurnsDesksOnWithoutShowingTheKey(t *testing.T) {
	s := newInstall(t, "init.sh", "desks.sh")
	s.runtimes = `{"runc":{},"runsc":{"path":"/usr/bin/runsc"}}`
	said, err := s.run("sh init.sh guildnet.example.com hq >/dev/null && sh desks.sh")
	if err != nil {
		t.Fatalf("desks.sh: %v\n%s", err, said)
	}
	env := envOf(t, s.read(".env"))
	for k, want := range map[string]string{
		"GUILDNET_SANDBOX_KEY": "private-key-text", "GUILDNET_SANDBOX_PUBLIC_KEY": "public-key-text",
		"GUILDNET_SANDBOX_URL": "https://sandboxd:8443", "COMPOSE_PROFILES": "desks",
	} {
		if env[k] != want {
			t.Errorf(".env's %s = %q, want %q", k, env[k], want)
		}
	}
	if strings.Contains(said, "private-key-text") {
		t.Error("desks.sh showed the console's key")
	}
	if !strings.Contains(said, "docker compose up") {
		t.Errorf("desks.sh should say what to do next:\n%s", said)
	}
	if !strings.Contains(s.calls(), "keygen") {
		t.Errorf("desks.sh didn't ask sandboxd for keys:\n%s", s.calls())
	}
	// From the release's images, or from source: whichever this install uses.
	if strings.Contains(s.calls(), "compose build") {
		t.Errorf("desks.sh built an image:\n%s", s.calls())
	}
	if _, ok := env["GUILDNET_DESK_RUNTIME"]; ok {
		t.Error("with gVisor there, desks.sh set another runtime")
	}
}

// Once desks are on, their keys stay: tokens the console has signed keep
// working. Running desks.sh again changes nothing.
func TestDesksShKeepsTheKeysItMade(t *testing.T) {
	s := newInstall(t, "init.sh", "desks.sh")
	s.runtimes = `{"runsc":{}}`
	if out, err := s.run("sh init.sh guildnet.example.com hq >/dev/null && sh desks.sh"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	before := s.read(".env")
	if out, err := s.run("sh desks.sh"); err != nil || s.read(".env") != before {
		t.Errorf("a second desks.sh should leave .env as it was: %v\n%s", err, out)
	}
}

// Desks run under gVisor, each with a kernel of its own: without it,
// desks.sh says how to install it and does nothing, unless told to go
// without (each desk is then an ordinary container).
func TestDesksShWantsGVisor(t *testing.T) {
	s := newInstall(t, "init.sh", "desks.sh")
	s.runtimes = `{"runc":{}}`
	if out, err := s.run("sh init.sh guildnet.example.com hq"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	before := s.read(".env")
	out, err := s.run("sh desks.sh")
	if err == nil || s.read(".env") != before || !strings.Contains(out, "gvisor.dev") {
		t.Errorf("without gVisor, desks.sh should refuse, change nothing and say where to get it:\n%s", out)
	}
	out, err = s.run("sh desks.sh --without-gvisor")
	env := envOf(t, s.read(".env"))
	if err != nil || env["GUILDNET_DESK_RUNTIME"] != "runc" || env["COMPOSE_PROFILES"] != "desks" {
		t.Errorf("desks.sh --without-gvisor should turn desks on with Docker's own runtime:\n%s", out)
	}
}

// The desks' firewall is what keeps desks from each other and this machine:
// desks.sh turns desks on only once it's set up, unless told to go without
// (to try GuildNet out where that's acceptable).
func TestDesksShWantsTheFirewall(t *testing.T) {
	s := newInstall(t, "init.sh", "desks.sh")
	s.runtimes = `{"runsc":{}}`
	if out, err := s.run(`touch "$T/firewall-off" && sh init.sh guildnet.example.com hq`); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	before := s.read(".env")
	out, err := s.run("sh desks.sh")
	if err == nil || s.read(".env") != before || !strings.Contains(out, "desks-firewall.sh install") {
		t.Errorf("without the firewall, desks.sh should refuse, change nothing and say how to set it up:\n%s", out)
	}
	out, err = s.run("sh desks.sh --without-firewall")
	env := envOf(t, s.read(".env"))
	if err != nil || env["COMPOSE_PROFILES"] != "desks" {
		t.Errorf("desks.sh --without-firewall should turn desks on all the same:\n%s", out)
	}
	// sandboxd checks the firewall as it starts, and refuses to serve
	// without it: told it goes without, it doesn't check.
	if env["GUILDNET_DESK_FIREWALL"] != "off" {
		t.Errorf("desks.sh --without-firewall should tell sandboxd (GUILDNET_DESK_FIREWALL=off); .env has %q", env["GUILDNET_DESK_FIREWALL"])
	}
	if got := fmt.Sprint(readStack(t).Services["sandboxd"].Environment["GUILDNET_DESK_FIREWALL"]); got != "${GUILDNET_DESK_FIREWALL:-}" {
		t.Errorf("sandboxd's GUILDNET_DESK_FIREWALL is %q, want it from .env", got)
	}
}

func TestDesksShNeedsInitFirst(t *testing.T) {
	s := newInstall(t, "desks.sh")
	s.runtimes = `{"runsc":{}}`
	out, err := s.run("sh desks.sh")
	if err == nil || s.read(".env") != "" || !strings.Contains(out, "init.sh") {
		t.Errorf("before init.sh, desks.sh should say to run it first:\n%s", out)
	}
}
