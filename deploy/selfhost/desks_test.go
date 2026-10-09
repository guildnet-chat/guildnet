// SPDX-License-Identifier: AGPL-3.0-only

package selfhost_test

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// Desks (agents' workspaces, each a container sandboxd makes) are optional:
// the desks profile, turned on by desks.sh. These test what the stack lets
// them reach, and the scripts that turn them on and keep them apart.

var desksServices = []string{"sandboxd", "egress", "desk"}

func TestDesksAreOffUntilTurnedOn(t *testing.T) {
	s := readStack(t)
	for _, name := range desksServices {
		svc, ok := s.Services[name]
		if !ok {
			t.Errorf("no %s service", name)
			continue
		}
		if !slices.Equal(svc.Profiles, []string{"desks"}) {
			t.Errorf("%s's profiles are %v, want [desks]", name, svc.Profiles)
		}
	}
	for name, svc := range s.Services {
		if slices.Contains(desksServices, name) {
			continue
		}
		if len(svc.Profiles) > 0 && !slices.Equal(svc.Profiles, []string{"tools"}) {
			t.Errorf("%s is in profiles %v", name, svc.Profiles)
		}
	}
	// The console knows of desks only when desks.sh has given it sandboxd's
	// address and key.
	p := s.Services["platform"]
	for _, k := range []string{"GUILDNET_SANDBOX_URL", "GUILDNET_SANDBOX_KEY"} {
		if v := fmt.Sprint(p.Environment[k]); v != "${"+k+":-}" {
			t.Errorf("platform's %s = %q, want it from .env, empty by default", k, v)
		}
	}
}

// sandboxd drives Docker, which is as good as root on the machine: it's the
// only service with Docker's socket, and nothing from outside reaches it.
func TestOnlySandboxdHasDocker(t *testing.T) {
	s := readStack(t)
	for name, svc := range s.Services {
		has := false
		for _, v := range svc.Volumes {
			if strings.HasPrefix(v, "/var/run/docker.sock:") {
				has = true
			}
		}
		if has != (name == "sandboxd") {
			t.Errorf("%s has Docker's socket: %v", name, has)
		}
	}
	on := networksOf(s.Services["sandboxd"])
	if !slices.Equal(on, []string{"private"}) {
		t.Errorf("sandboxd is on %v, want only the private network (the console's and the runner's)", on)
	}
}

// Desks reach only their proxy: their network has no way out, and the
// proxy, at the address sandboxd gives desks, is on it and on a network of
// its own with a way out, not the console's, the runner's or the database's.
func TestDesksReachOnlyTheirProxy(t *testing.T) {
	s := readStack(t)
	sandboxd, egress, desk := s.Services["sandboxd"], s.Services["egress"], s.Services["desk"]
	deskNet := fmt.Sprint(sandboxd.Environment["GUILDNET_DESK_NETWORK"])
	var desks string
	for name := range s.Networks {
		if s.Networks[name].Name == deskNet {
			desks = name
		}
	}
	if desks == "" || !s.Networks[desks].Internal {
		t.Fatalf("sandboxd's desk network %q: want one of the stack's, with no way out", deskNet)
	}
	proxy, err := url.Parse(fmt.Sprint(sandboxd.Environment["GUILDNET_DESK_PROXY"]))
	if err != nil {
		t.Fatal(err)
	}
	m, ok := egress.Networks.(map[string]any)
	if !ok {
		t.Fatalf("egress's networks: %v", egress.Networks)
	}
	at, _ := m[desks].(map[string]any)
	if fmt.Sprint(at["ipv4_address"]) != proxy.Hostname() {
		t.Errorf("egress is at %v on the desks' network; sandboxd sends desks to %s", at["ipv4_address"], proxy.Host)
	}
	for _, n := range networksOf(egress) {
		if n == "private" || n == "data" || n == "edge" {
			t.Errorf("egress is on %s", n)
		}
		if n != desks && s.Networks[n].Internal {
			t.Errorf("egress's way out (%s) has none", n)
		}
	}
	if len(networksOf(egress)) != 2 {
		t.Errorf("egress is on %v, want the desks' network and its way out", networksOf(egress))
	}
	if img := fmt.Sprint(sandboxd.Environment["GUILDNET_DESK_IMAGE"]); desk.Image == "" || img != desk.Image {
		t.Errorf("sandboxd makes desks from %q; the desk image built is %q", img, desk.Image)
	}
	if desk.Scale == nil || *desk.Scale != 0 {
		t.Errorf("the desk image's service runs %v containers, want none: sandboxd runs desks", desk.Scale)
	}
	if rt := fmt.Sprint(sandboxd.Environment["GUILDNET_DESK_RUNTIME"]); rt != "${GUILDNET_DESK_RUNTIME:-runsc}" {
		t.Errorf("desks' runtime = %q, want gVisor (runsc) unless .env says otherwise", rt)
	}
}

// On the desks' network, a desk reaches the proxy and nothing else: not
// another desk, and not this machine. desks-firewall.sh sets that up with
// iptables; it runs whenever Docker starts, so running it again must leave
// the same rules, once.
func TestDesksFirewallKeepsDesksApart(t *testing.T) {
	if os.Getenv("GUILDNET_DOCKER_TESTS") != "1" {
		t.Skip("set GUILDNET_DOCKER_TESTS=1 to test against Docker")
	}
	fw, err := os.ReadFile("desks-firewall.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := `set -e
cat > /fw.sh
apk add -q iptables >/dev/null
iptables -N DOCKER-USER
iptables -A FORWARD -j DOCKER-USER
sh /fw.sh >/dev/null
sh /fw.sh >/dev/null
iptables -S`
	cmd := exec.Command("docker", "run", "--rm", "-i", "--cap-add", "NET_ADMIN", "alpine:3.22", "sh", "-c", script)
	cmd.Stdin = bytes.NewReader(fw)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("desks-firewall.sh: %v\n%s", err, out)
	}
	rules := string(out)
	for _, want := range []string{
		"-A DOCKER-USER -s 172.31.255.0/24 -d 172.31.255.0/24 -j GUILDNET-DESKS",
		"-A GUILDNET-DESKS -m conntrack --ctstate RELATED,ESTABLISHED -j RETURN",
		"-A GUILDNET-DESKS -d 172.31.255.2/32 -p tcp -m tcp --dport 3128 -j RETURN",
		"-A GUILDNET-DESKS -j DROP",
		"-A INPUT -s 172.31.255.0/24 -j GUILDNET-DESKS-HOST",
		"-A GUILDNET-DESKS-HOST -m conntrack --ctstate RELATED,ESTABLISHED -j RETURN",
		"-A GUILDNET-DESKS-HOST -j REJECT --reject-with icmp-port-unreachable",
	} {
		if strings.Count(rules, want+"\n") != 1 {
			t.Errorf("want once: %s\ngot:\n%s", want, rules)
		}
	}
}
