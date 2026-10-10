// SPDX-License-Identifier: AGPL-3.0-only

package selfhost_test

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// deploy/desks-host is desks on a machine of their own (for a console on
// GKE or Fargate, say): sandboxd, egress and the desk image, reached by the
// console and the runner over the private network, on the host's private
// address only.

func readDesksHost(t *testing.T) stack {
	t.Helper()
	b, err := os.ReadFile("../desks-host/compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	var s stack
	if err := yaml.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// Only sandboxd can be reached from outside the desks host, on its private
// address only, over TLS with a certificate it keeps read-only; its key and
// certificate are written only by the tls service, which has no network.
func TestTheDesksHostPublishesOnlySandboxdOnItsAddress(t *testing.T) {
	s := readDesksHost(t)
	for name, svc := range s.Services {
		if name == "sandboxd" {
			if want := []string{"${GUILDNET_DESKS_HOST_ADDRESS:?run init.sh first}:8443:8443"}; !slices.Equal(svc.Ports, want) {
				t.Errorf("sandboxd publishes %v, want %v", svc.Ports, want)
			}
			continue
		}
		if len(svc.Ports) > 0 {
			t.Errorf("%s publishes %v; only sandboxd should", name, svc.Ports)
		}
	}
	sandboxd := s.Services["sandboxd"]
	if dir := fmt.Sprint(sandboxd.Environment["GUILDNET_SANDBOX_TLS_DIR"]); !slices.Contains(sandboxd.Volumes, "sandbox-tls:"+dir+":ro") {
		t.Errorf("sandboxd should serve TLS from a read-only volume; it mounts %v, TLS from %q", sandboxd.Volumes, dir)
	}
	for name, svc := range s.Services {
		for _, v := range svc.Volumes {
			if strings.HasPrefix(v, "sandbox-tls:") && !strings.HasSuffix(v, ":ro") && name != "tls" {
				t.Errorf("%s can change sandboxd's key or certificate: %s", name, v)
			}
		}
	}
	if tls := s.Services["tls"]; !slices.Contains(tls.Profiles, "tools") || tls.NetworkMode != "none" {
		t.Errorf("the tls service should be a tool with no network: profiles %v, network_mode %q", tls.Profiles, tls.NetworkMode)
	}
}

// The desks' network is the one desks-firewall.sh keeps desks apart on, as
// in the self-host stack, with egress at the address sandboxd sends desks
// to; and every service has only the capabilities it needs.
func TestTheDesksHostKeepsDesksAsTheStackDoes(t *testing.T) {
	host, stack := readDesksHost(t), readStack(t)
	if host.Networks["desks"] != stack.Networks["desks"] {
		t.Errorf("the desks' network is %+v on the desks host, %+v in the stack", host.Networks["desks"], stack.Networks["desks"])
	}
	for _, name := range []string{"egress", "desk"} {
		h, s := host.Services[name], stack.Services[name]
		if fmt.Sprint(h.Networks) != fmt.Sprint(s.Networks) || h.Image != s.Image || fmt.Sprint(h.Environment) != fmt.Sprint(s.Environment) {
			t.Errorf("%s differs from the stack's:\n%+v\n%+v", name, h, s)
		}
	}
	for _, k := range []string{"GUILDNET_DESK_IMAGE", "GUILDNET_DESK_NETWORK", "GUILDNET_DESK_PROXY", "GUILDNET_DESK_RUNTIME", "GUILDNET_DESKS_AT_ONCE"} {
		if h, s := host.Services["sandboxd"].Environment[k], stack.Services["sandboxd"].Environment[k]; h != s {
			t.Errorf("sandboxd's %s is %v on the desks host, %v in the stack", k, h, s)
		}
	}
	for name, svc := range host.Services {
		if svc.Scale != nil && *svc.Scale == 0 {
			continue
		}
		if !slices.Equal(svc.CapDrop, []string{"ALL"}) || len(svc.CapAdd) > 0 || !slices.Contains(svc.SecurityOpt, "no-new-privileges:true") {
			t.Errorf("%s: drops %v, adds %v, security_opt %v", name, svc.CapDrop, svc.CapAdd, svc.SecurityOpt)
		}
	}
}

// init.sh makes the desks host's settings: the release, its address, and
// sandboxd's half of the key pair in .env; the console's half, and where
// it reaches sandboxd, in console.env, for the console's settings; and a
// request for sandboxd's certificate, for the console to sign. Neither file
// can be read by anyone else, and the console's key isn't shown.
func TestDesksHostInitMakesBothSidesSettings(t *testing.T) {
	s := newInstallOf(t, "desks-host", "init.sh")
	s.runtimes = `{"runsc":{}}`
	said, err := s.run("sh init.sh 10.20.0.9")
	if err != nil {
		t.Fatalf("init.sh: %v\n%s", err, said)
	}
	version, _ := os.ReadFile("../../VERSION")
	env, console := envOf(t, s.read(".env")), envOf(t, s.read("console.env"))
	for k, want := range map[string]string{"GUILDNET_VERSION": strings.TrimSpace(string(version)), "GUILDNET_DESKS_HOST_ADDRESS": "10.20.0.9", "GUILDNET_SANDBOX_PUBLIC_KEY": "public-key-text"} {
		if env[k] != want {
			t.Errorf(".env's %s = %q, want %q", k, env[k], want)
		}
	}
	for k, want := range map[string]string{"GUILDNET_SANDBOX_KEY": "private-key-text", "GUILDNET_SANDBOX_URL": "https://10.20.0.9:8443", "GUILDNET_SANDBOX_NAMES": "10.20.0.9"} {
		if console[k] != want {
			t.Errorf("console.env's %s = %q, want %q", k, console[k], want)
		}
	}
	if strings.Contains(s.read(".env"), "private-key-text") {
		t.Error("the desks host's .env has the console's key")
	}
	if !strings.Contains(s.read("sandboxd.csr"), "CERTIFICATE REQUEST") {
		t.Errorf("no request for sandboxd's certificate:\n%s", s.calls())
	}
	if strings.Contains(said, "private-key-text") {
		t.Error("init.sh showed the console's key")
	}
	for _, next := range []string{"console.env", "sandbox-cert", "tls-install", "docker compose up -d"} {
		if !strings.Contains(said, next) {
			t.Errorf("init.sh should say what's next (%q):\n%s", next, said)
		}
	}
	if runtime.GOOS != "windows" {
		for _, f := range []string{".env", "console.env"} {
			if info, err := os.Stat(filepath.Join(s.dir, f)); err != nil || info.Mode().Perm() != 0o600 {
				t.Errorf("%s: %v, want mode 600", f, err)
			}
		}
	}
}

func TestDesksHostInitRefusesWhatItCantUse(t *testing.T) {
	for name, c := range map[string]struct{ setup, args, says string }{
		"no address":             {"", "", "usage"},
		"not an address":         {"", "'10.20.0.9; x'", "address"},
		"a public address":       {"", "8.8.8.8", "private"},
		"settings already there": {"echo GUILDNET_X=1 > .env", "10.20.0.9", ".env"},
		"no gVisor":              {"", "10.20.0.9", "gvisor.dev"},
		"no firewall":            {"touch \"$T/firewall-off\"", "10.20.0.9", "desks-firewall.sh install"},
	} {
		s := newInstallOf(t, "desks-host", "init.sh")
		s.runtimes = `{"runsc":{}}`
		if name == "no gVisor" {
			s.runtimes = `{"runc":{}}`
		}
		before := s.read(".env")
		out, err := s.run(c.setup + "\nsh init.sh " + c.args)
		if name == "settings already there" {
			before = "GUILDNET_X=1\n"
		}
		if err == nil || s.read(".env") != before || s.read("console.env") != "" || !strings.Contains(out, c.says) {
			t.Errorf("%s: init.sh should refuse, saying %q, and write nothing:\n%s", name, c.says, out)
		}
	}
}
