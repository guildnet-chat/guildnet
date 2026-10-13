// SPDX-License-Identifier: AGPL-3.0-only

package selfhost_test

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// deploy/aws-fargate is the stack in one ECS task, with Postgres on RDS and
// the files on EFS. It's untested on AWS, so these hold it to the stack:
// the same programs and settings, every address localhost's (one task
// shares it), and nothing more reachable or privileged than in Compose.

type ecsContainer struct {
	Name        string `yaml:"Name"`
	Image       string `yaml:"Image"`
	User        string `yaml:"User"`
	Environment []struct {
		Name  string `yaml:"Name"`
		Value string `yaml:"Value"`
	} `yaml:"Environment"`
	Secrets []struct {
		Name      string `yaml:"Name"`
		ValueFrom string `yaml:"ValueFrom"`
	} `yaml:"Secrets"`
	ReadonlyRootFilesystem bool `yaml:"ReadonlyRootFilesystem"`
	LinuxParameters        struct {
		Capabilities struct {
			Drop []string `yaml:"Drop"`
			Add  []string `yaml:"Add"`
		} `yaml:"Capabilities"`
	} `yaml:"LinuxParameters"`
	MountPoints []struct {
		SourceVolume  string `yaml:"SourceVolume"`
		ContainerPath string `yaml:"ContainerPath"`
		ReadOnly      bool   `yaml:"ReadOnly"`
	} `yaml:"MountPoints"`
}

type cfnTemplate struct {
	Parameters map[string]struct {
		Default       string   `yaml:"Default"`
		AllowedValues []string `yaml:"AllowedValues"`
	} `yaml:"Parameters"`
	Resources map[string]struct {
		Type       string    `yaml:"Type"`
		Properties yaml.Node `yaml:"Properties"`
	} `yaml:"Resources"`
}

// readFargate is the template, with CloudFormation's short tags (!Sub,
// !Ref) dropped: what they're given stays.
func readFargate(t *testing.T) cfnTemplate {
	t.Helper()
	b, err := os.ReadFile("../aws-fargate/guildnet.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(b, &root); err != nil {
		t.Fatal(err)
	}
	var untag func(*yaml.Node)
	untag = func(n *yaml.Node) {
		if strings.HasPrefix(n.Tag, "!") && !strings.HasPrefix(n.Tag, "!!") {
			n.Tag = ""
		}
		for _, c := range n.Content {
			untag(c)
		}
	}
	untag(&root)
	var tpl cfnTemplate
	if err := root.Decode(&tpl); err != nil {
		t.Fatal(err)
	}
	return tpl
}

// properties decodes resource name's properties into v.
func (tpl cfnTemplate) properties(t *testing.T, name string, v any) {
	t.Helper()
	r, ok := tpl.Resources[name]
	if !ok {
		t.Fatalf("the template has no %s", name)
	}
	if err := r.Properties.Decode(v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

func fargateContainers(t *testing.T, tpl cfnTemplate, task string) map[string]ecsContainer {
	t.Helper()
	var td struct {
		ContainerDefinitions []ecsContainer `yaml:"ContainerDefinitions"`
	}
	tpl.properties(t, task, &td)
	out := map[string]ecsContainer{}
	for _, c := range td.ContainerDefinitions {
		out[c.Name] = c
	}
	return out
}

// The task's containers have every setting Compose gives each service
// (from the settings' secret, where Compose has them from .env), with
// Compose's values, but for addresses: those are localhost's.
func TestTheFargateTaskHasTheStacksSettings(t *testing.T) {
	stack, tpl := readStack(t), readFargate(t)
	containers := fargateContainers(t, tpl, "Task")
	addresses := map[string]bool{"GUILDNET_TRUSTED_PROXIES": true, "ERGO__SERVER__PROXY_ALLOWED_FROM": true,
		"GUILDNET_PLATFORM_DATABASE_URL": true, "GUILDNET_DATABASE_URL": true, "GUILDNET_RUNNER_URL": true, "GUILDNET_ERGO_URL": true,
		"GUILDNET_CHAT_URL": true, "GUILDNET_BROKER_NAMES": true, "GUILDNET_KEYS_BROKER": true, "GUILDNET_GITHUB_BROKER": true,
		"GUILDNET_SERVER": true, "GUILDNET_ERGO_API_URL": true, "GUILDNET_IRC_AUTH_URL": true}
	// No brand files on Fargate, and desks are on a host of their own.
	notHere := map[string]bool{"GUILDNET_SANDBOX_TLS_DIR": true, "GUILDNET_BRAND_DIR": true}
	for _, name := range []string{"caddy", "platform", "ergo", "runner"} {
		c, ok := containers[name]
		if !ok {
			t.Errorf("the task has no %s", name)
			continue
		}
		env, secret := map[string]string{}, map[string]bool{}
		for _, e := range c.Environment {
			env[e.Name] = e.Value
		}
		for _, s := range c.Secrets {
			secret[s.Name] = true
		}
		for k, v := range stack.Services[name].Environment {
			want := toString(v)
			got, inEnv := env[k]
			switch {
			case notHere[k]:
			case !inEnv && !secret[k]:
				t.Errorf("%s: %s isn't set in the task", name, k)
			case addresses[k]:
				if !strings.Contains(got, "localhost") && !strings.Contains(got, "127.0.0.1") && !strings.Contains(got, ".Endpoint.Address") {
					t.Errorf("%s: %s is %q; in one task it's localhost's", name, k, got)
				}
			case inEnv && !strings.Contains(want, "${") && got != want:
				t.Errorf("%s: %s is %q in the task, %q in the stack", name, k, got, want)
			}
		}
	}
}

// One task, never two: the service runs none (until the database is set up)
// or one, and stops it before starting its replacement.
func TestFargateNeverRunsTwoTasks(t *testing.T) {
	var svc struct {
		DesiredCount            string `yaml:"DesiredCount"`
		DeploymentConfiguration struct {
			MaximumPercent        int `yaml:"MaximumPercent"`
			MinimumHealthyPercent int `yaml:"MinimumHealthyPercent"`
		} `yaml:"DeploymentConfiguration"`
	}
	tpl := readFargate(t)
	tpl.properties(t, "Service", &svc)
	if svc.DesiredCount != "Running" || !slices.Equal(tpl.Parameters["Running"].AllowedValues, []string{"0", "1"}) ||
		svc.DeploymentConfiguration.MaximumPercent != 100 || svc.DeploymentConfiguration.MinimumHealthyPercent != 0 {
		t.Errorf("the service: %+v, Running %v; want no more than one task, replaced by stopping it first", svc, tpl.Parameters["Running"].AllowedValues)
	}
}

// Every container runs the release's image (the Release parameter, which
// defaults to VERSION), with a root filesystem it can't change and no
// capabilities but Caddy's NET_BIND_SERVICE; the console's CA's key is the
// console's alone, and the runner reads the CA's certificate read-only.
func TestTheFargateTaskRunsTheReleaseLockedDown(t *testing.T) {
	tpl := readFargate(t)
	version, _ := os.ReadFile("../../VERSION")
	if got := tpl.Parameters["Release"].Default; got != strings.TrimSpace(string(version)) {
		t.Errorf("the template's Release defaults to %q, not VERSION's %q", got, strings.TrimSpace(string(version)))
	}
	images := map[string]string{"rds-ca": "caddy", "caddy": "caddy", "platform": "platform", "ergo": "ergo", "runner": "runner", "db-init": "postgres"}
	all := fargateContainers(t, tpl, "Task")
	for k, v := range fargateContainers(t, tpl, "DatabaseInit") {
		all[k] = v
	}
	// Docker's default set, which Fargate starts from and can't add to.
	defaults := []string{"AUDIT_WRITE", "CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "KILL", "MKNOD", "NET_BIND_SERVICE", "NET_RAW", "SETFCAP", "SETGID", "SETPCAP", "SETUID", "SYS_CHROOT"}
	for name, c := range all {
		if want := "${Registry}/" + images[name] + ":${Release}"; c.Image != want {
			t.Errorf("%s runs %q, want %q", name, c.Image, want)
		}
		if !c.ReadonlyRootFilesystem {
			t.Errorf("%s can change its root filesystem", name)
		}
		caps := c.LinuxParameters.Capabilities
		kept := slices.DeleteFunc(slices.Clone(defaults), func(s string) bool { return slices.Contains(caps.Drop, s) })
		if slices.Contains(caps.Drop, "ALL") {
			kept = nil
		}
		if want := map[string][]string{"caddy": {"NET_BIND_SERVICE"}}[name]; !slices.Equal(kept, want) || len(caps.Add) > 0 {
			t.Errorf("%s keeps %v and adds %v; want %v", name, kept, caps.Add, want)
		}
		if name != "caddy" && name != "rds-ca" && (c.User == "" || strings.HasPrefix(c.User, "0")) {
			t.Errorf("%s runs as %q; want a user that isn't root", name, c.User)
		}
		for _, m := range c.MountPoints {
			if m.SourceVolume == "broker-ca" && name != "platform" {
				t.Errorf("%s mounts the console's CA", name)
			}
			if m.SourceVolume == "broker-ca-shared" && name == "runner" && !m.ReadOnly {
				t.Error("the runner can change the CA's certificate it trusts")
			}
		}
	}
}

// From the internet, only the edge's ports reach the task, with visitors'
// own addresses; the database and the files only from the task.
func TestOnlyTheEdgeIsReachableOnFargate(t *testing.T) {
	tpl := readFargate(t)
	type rule struct {
		IpProtocol            string `yaml:"IpProtocol"`
		FromPort              int    `yaml:"FromPort"`
		ToPort                int    `yaml:"ToPort"`
		CidrIp                string `yaml:"CidrIp"`
		SourceSecurityGroupId string `yaml:"SourceSecurityGroupId"`
	}
	groups := map[string][]rule{}
	listeners := []int{}
	for name, r := range tpl.Resources {
		switch r.Type {
		case "AWS::EC2::SecurityGroup":
			var g struct {
				SecurityGroupIngress []rule `yaml:"SecurityGroupIngress"`
			}
			tpl.properties(t, name, &g)
			groups[name] = g.SecurityGroupIngress
		case "AWS::ElasticLoadBalancingV2::Listener":
			var l struct {
				Port int `yaml:"Port"`
			}
			tpl.properties(t, name, &l)
			listeners = append(listeners, l.Port)
		case "AWS::ElasticLoadBalancingV2::TargetGroup":
			var g struct {
				Attributes []struct {
					Key   string `yaml:"Key"`
					Value string `yaml:"Value"`
				} `yaml:"TargetGroupAttributes"`
			}
			tpl.properties(t, name, &g)
			kept := false
			for _, a := range g.Attributes {
				kept = kept || a.Key == "preserve_client_ip.enabled" && a.Value == "true"
			}
			if !kept {
				t.Errorf("%s doesn't keep visitors' addresses", name)
			}
		case "AWS::EC2::SecurityGroupIngress":
			t.Errorf("%s opens more, outside its group", name)
		}
	}
	slices.Sort(listeners)
	if !slices.Equal(listeners, []int{80, 443, 6697}) {
		t.Errorf("the load balancer listens on %v, want 80, 443 and 6697", listeners)
	}
	var task []string
	for _, r := range groups["TaskGroup"] {
		task = append(task, fmt.Sprintf("%s/%d-%d from %s", r.IpProtocol, r.FromPort, r.ToPort, r.CidrIp))
	}
	if want := []string{"tcp/80-80 from 0.0.0.0/0", "tcp/443-443 from 0.0.0.0/0", "tcp/6697-6697 from 0.0.0.0/0"}; !slices.Equal(task, want) {
		t.Errorf("the task lets in %v, want %v", task, want)
	}
	for name, rules := range groups {
		if name == "TaskGroup" {
			continue
		}
		for _, r := range rules {
			if r.SourceSecurityGroupId != "TaskGroup" || r.CidrIp != "" {
				t.Errorf("%s lets in %+v; only the task should reach it", name, r)
			}
		}
	}
}
