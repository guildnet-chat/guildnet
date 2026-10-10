// SPDX-License-Identifier: AGPL-3.0-only

package selfhost_test

import (
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// deploy/gke is the stack in one pod on GKE, with Postgres on Cloud SQL:
// the same programs with the same settings, names and certificates, so a
// Compose install's backup restores there unchanged.

type k8sContainer struct {
	Name    string   `yaml:"name"`
	Image   string   `yaml:"image"`
	Command []string `yaml:"command"`
	Env     []struct {
		Name      string `yaml:"name"`
		Value     string `yaml:"value"`
		ValueFrom *struct {
			SecretKeyRef struct {
				Key      string `yaml:"key"`
				Optional bool   `yaml:"optional"`
			} `yaml:"secretKeyRef"`
		} `yaml:"valueFrom"`
	} `yaml:"env"`
	SecurityContext struct {
		RunAsUser                *int  `yaml:"runAsUser"`
		RunAsNonRoot             bool  `yaml:"runAsNonRoot"`
		AllowPrivilegeEscalation *bool `yaml:"allowPrivilegeEscalation"`
		ReadOnlyRootFilesystem   bool  `yaml:"readOnlyRootFilesystem"`
		Capabilities             struct {
			Drop []string `yaml:"drop"`
			Add  []string `yaml:"add"`
		} `yaml:"capabilities"`
	} `yaml:"securityContext"`
	VolumeMounts []struct {
		Name      string `yaml:"name"`
		MountPath string `yaml:"mountPath"`
		SubPath   string `yaml:"subPath"`
		ReadOnly  bool   `yaml:"readOnly"`
	} `yaml:"volumeMounts"`
}

type k8sPod struct {
	HostAliases []struct {
		IP        string   `yaml:"ip"`
		Hostnames []string `yaml:"hostnames"`
	} `yaml:"hostAliases"`
	InitContainers []k8sContainer `yaml:"initContainers"`
	Containers     []k8sContainer `yaml:"containers"`
}

type k8sObject struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Type       string         `yaml:"type"`
		Containers []k8sContainer `yaml:"containers"` // a Pod's
		Template   struct {
			Spec k8sPod `yaml:"spec"`
		} `yaml:"template"`
		Ports []struct {
			Port     int    `yaml:"port"`
			Protocol string `yaml:"protocol"`
		} `yaml:"ports"`
		Ingress []struct {
			From  []any `yaml:"from"`
			Ports []struct {
				Port int `yaml:"port"`
			} `yaml:"ports"`
		} `yaml:"ingress"`
	} `yaml:"spec"`
}

// readGKE is every object in deploy/gke's manifests.
func readGKE(t *testing.T) []k8sObject {
	t.Helper()
	var out []k8sObject
	for _, f := range []string{"statefulset.yaml", "service.yaml", "db-init.yaml", "namespace.yaml", "restore.yaml"} {
		b, err := os.ReadFile("../gke/" + f)
		if err != nil {
			t.Fatal(err)
		}
		d := yaml.NewDecoder(strings.NewReader(string(b)))
		for {
			var o k8sObject
			if err := d.Decode(&o); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			out = append(out, o)
		}
	}
	return out
}

func gkeObject(t *testing.T, kind, name string) k8sObject {
	t.Helper()
	for _, o := range readGKE(t) {
		if o.Kind == kind && o.Metadata.Name == name {
			return o
		}
	}
	t.Fatalf("deploy/gke has no %s %s", kind, name)
	return k8sObject{}
}

// The pod's containers have the settings Compose's services have, with the
// same values where Compose fixes them, except where one pod differs from
// a machine's networks: the edge's address, and sandboxd's certificate
// folder (desks are on a host of their own here).
func TestTheGKEPodHasTheStacksSettings(t *testing.T) {
	stack := readStack(t)
	pod := gkeObject(t, "StatefulSet", "guildnet").Spec.Template.Spec
	differs := map[string]bool{"GUILDNET_TRUSTED_PROXIES": true, "ERGO__SERVER__PROXY_ALLOWED_FROM": true,
		"GUILDNET_PLATFORM_DATABASE_URL": true, "GUILDNET_DATABASE_URL": true}
	notHere := map[string]bool{"GUILDNET_SANDBOX_TLS_DIR": true}
	for _, name := range []string{"caddy", "platform", "ergo", "runner"} {
		var c *k8sContainer
		for i := range pod.Containers {
			if pod.Containers[i].Name == name {
				c = &pod.Containers[i]
			}
		}
		if c == nil {
			t.Errorf("the pod has no %s", name)
			continue
		}
		env := map[string]string{}
		for _, e := range c.Env {
			env[e.Name] = e.Value
			if e.ValueFrom != nil {
				env[e.Name] = "${" + e.ValueFrom.SecretKeyRef.Key + "}"
			}
		}
		for k, v := range stack.Services[name].Environment {
			got, ok := env[k]
			want := toString(v)
			switch {
			case notHere[k]:
			case !ok:
				t.Errorf("%s: %s isn't set in the pod", name, k)
			case !strings.Contains(want, "${") && !differs[k] && got != want:
				t.Errorf("%s: %s is %q in the pod, %q in the stack", name, k, got, want)
			}
		}
	}
	var aliased []string
	for _, a := range pod.HostAliases {
		if a.IP == "127.0.0.1" {
			aliased = append(aliased, a.Hostnames...)
		}
	}
	slices.Sort(aliased)
	if !slices.Equal(aliased, []string{"ergo", "platform", "runner"}) {
		t.Errorf("localhost is called %v in the pod, want ergo, platform and runner, as the stack's addresses have them", aliased)
	}
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := yaml.Marshal(v)
	return strings.TrimSpace(string(b))
}

// Every container runs the release's image, with no capabilities but those
// it needs, none to gain, a root filesystem it can't change, and (but for
// the edge, and the init container that sets the disk's ownership) not as
// root.
func TestTheGKEPodRunsTheReleaseLockedDown(t *testing.T) {
	version, _ := os.ReadFile("../../VERSION")
	pod := gkeObject(t, "StatefulSet", "guildnet").Spec.Template.Spec
	job := gkeObject(t, "Job", "guildnet-db-init").Spec.Template.Spec
	restore := gkeObject(t, "Pod", "guildnet-restore").Spec
	// The edge binds 80 and 443; the init container and the restore pod set
	// and keep the disk's ownership.
	keeps := []string{"CHOWN", "FOWNER", "DAC_OVERRIDE"}
	needs := map[string][]string{"caddy": {"NET_BIND_SERVICE"}, "own": keeps, "restore": keeps}
	root := map[string]bool{"caddy": true, "own": true, "restore": true}
	for _, c := range slices.Concat(pod.InitContainers, pod.Containers, job.Containers, restore.Containers) {
		image, tag, _ := strings.Cut(c.Image, ":")
		if !strings.HasPrefix(image, "ghcr.io/guildnet-chat/") || tag != strings.TrimSpace(string(version)) {
			t.Errorf("%s runs %s, want one of the release's (%s)", c.Name, c.Image, strings.TrimSpace(string(version)))
		}
		sc := c.SecurityContext
		if !slices.Equal(sc.Capabilities.Drop, []string{"ALL"}) || !slices.Equal(sc.Capabilities.Add, needs[c.Name]) {
			t.Errorf("%s drops %v and adds %v; want ALL, and %v", c.Name, sc.Capabilities.Drop, sc.Capabilities.Add, needs[c.Name])
		}
		if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation || !sc.ReadOnlyRootFilesystem {
			t.Errorf("%s can gain privileges or change its root filesystem", c.Name)
		}
		if !root[c.Name] && (!sc.RunAsNonRoot || sc.RunAsUser == nil || *sc.RunAsUser == 0) {
			t.Errorf("%s may run as root", c.Name)
		}
	}
	b, err := os.ReadFile("../gke/kustomization.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var k struct {
		Images []struct {
			Name   string `yaml:"name"`
			NewTag string `yaml:"newTag"`
		} `yaml:"images"`
	}
	yaml.Unmarshal(b, &k)
	for _, i := range k.Images {
		if i.NewTag != strings.TrimSpace(string(version)) {
			t.Errorf("kustomization sets %s to %s, not the release", i.Name, i.NewTag)
		}
	}
}

// The console's CA (its key) is mounted only in the console; the runner
// has the CA's certificate only, read-only.
func TestOnlyTheConsoleHasItsCAInThePod(t *testing.T) {
	pod := gkeObject(t, "StatefulSet", "guildnet").Spec.Template.Spec
	for _, c := range pod.Containers {
		for _, m := range c.VolumeMounts {
			if m.SubPath == "broker-ca" && c.Name != "platform" {
				t.Errorf("%s mounts the console's CA", c.Name)
			}
			if m.SubPath == "broker-ca-shared" && c.Name == "runner" && !m.ReadOnly {
				t.Error("the runner can change the CA's certificate it trusts")
			}
		}
	}
}

// From outside, only the edge's ports: the load balancer has nothing else,
// and the network policy lets nothing else into the pod.
func TestOnlyTheEdgeIsReachableOnGKE(t *testing.T) {
	want := []int{80, 443, 6697}
	for _, o := range readGKE(t) {
		switch {
		case o.Kind == "Service" && o.Spec.Type == "LoadBalancer":
			var ports []int
			for _, p := range o.Spec.Ports {
				ports = append(ports, p.Port)
			}
			if !slices.Equal(ports, want) {
				t.Errorf("the load balancer has %v, want %v", ports, want)
			}
		case o.Kind == "Service" && len(o.Spec.Ports) > 0:
			t.Errorf("service %s exposes %v", o.Metadata.Name, o.Spec.Ports)
		case o.Kind == "NetworkPolicy":
			var ports []int
			for _, in := range o.Spec.Ingress {
				for _, p := range in.Ports {
					ports = append(ports, p.Port)
				}
				if len(in.Ports) == 0 {
					t.Error("the network policy lets in every port")
				}
			}
			if !slices.Equal(ports, want) {
				t.Errorf("the network policy lets in %v, want %v", ports, want)
			}
		}
	}
}
