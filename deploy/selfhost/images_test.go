// SPDX-License-Identifier: AGPL-3.0-only

package selfhost_test

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// calVer is a release's name: YY.0M.N, e.g. 26.10.0.
var calVer = regexp.MustCompile(`^\d\d\.(0[1-9]|1[0-2])\.(0|[1-9]\d*)$`)

// built is where a service's image is built from, as a folder of the
// module's (deploy/caddy, or "." for the Go programs) and its Dockerfile.
type built struct{ context, dockerfile string }

// buildOf is a service's build, relative to the module's root ("" context
// if it has none).
func buildOf(t *testing.T, s service) built {
	t.Helper()
	var context, dockerfile string
	switch b := s.Build.(type) {
	case nil:
		return built{}
	case string:
		context = b
	case map[string]any:
		context, _ = b["context"].(string)
		dockerfile, _ = b["dockerfile"].(string)
	default:
		t.Fatalf("a build of %T", b)
	}
	// compose.yml is in deploy/selfhost.
	context = path.Clean(path.Join("deploy/selfhost", context))
	if dockerfile == "" {
		dockerfile = path.Join(context, "Dockerfile")
	} else {
		dockerfile = path.Clean(path.Join(context, dockerfile))
	}
	return built{context, dockerfile}
}

// The stack runs the release's images, from GuildNet's registry (or one of
// yours): each named for the folder of deploy/ it's built from, and tagged
// with the release init.sh wrote into .env. The same lines build them from
// source, under the same names. What isn't built uses one of those images.
func TestTheStackRunsTheReleasesImages(t *testing.T) {
	for _, s := range []stack{readStack(t), readDesksHost(t)} {
		runsTheReleasesImages(t, s)
	}
}

func runsTheReleasesImages(t *testing.T, s stack) {
	t.Helper()
	images := map[string]bool{}
	for name, svc := range s.Services {
		b := buildOf(t, svc)
		if b.context == "" {
			continue
		}
		folder := path.Base(path.Dir(b.dockerfile))
		want := "${GUILDNET_REGISTRY:-ghcr.io/guildnet-chat}/" + folder + ":${GUILDNET_VERSION:?run init.sh first}"
		if svc.Image != want {
			t.Errorf("%s's image is %q, want %q", name, svc.Image, want)
		}
		images[svc.Image] = true
	}
	for name, svc := range s.Services {
		if svc.Build == nil && !images[svc.Image] {
			t.Errorf("%s runs %q, which isn't one of the stack's images", name, svc.Image)
		}
	}
}

// Each release publishes every image the stack runs, built as the stack
// builds it.
func TestTheReleasePublishesEveryImage(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			Strategy struct {
				Matrix struct {
					Include []struct {
						Image   string `yaml:"image"`
						Context string `yaml:"context"`
					} `yaml:"include"`
				} `yaml:"matrix"`
			} `yaml:"strategy"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(b, &wf); err != nil {
		t.Fatal(err)
	}
	published := map[string]built{}
	for _, inc := range wf.Jobs["images"].Strategy.Matrix.Include {
		published[inc.Image] = built{path.Clean(inc.Context), path.Join("deploy", inc.Image, "Dockerfile")}
	}
	stack := map[string]built{}
	for name, svc := range readStack(t).Services {
		if b := buildOf(t, svc); b.context != "" {
			image := path.Base(path.Dir(b.dockerfile))
			if other, ok := stack[image]; ok && other != b {
				t.Errorf("%s builds %s differently from another service: %v, %v", name, image, b, other)
			}
			stack[image] = b
		}
	}
	for image, b := range stack {
		if got, ok := published[image]; !ok {
			t.Errorf("the release doesn't publish %s", image)
		} else if got != b {
			t.Errorf("the release builds %s from %v; the stack from %v", image, got, b)
		}
	}
	for image := range published {
		if _, ok := stack[image]; !ok {
			t.Errorf("the release publishes %s, which the stack doesn't run", image)
		}
	}
}

// The release is named in VERSION (what init.sh pins an install to, and
// what the release's tag must say), and the web chat's release it ships
// with in deploy/platform/chat-release: both by date.
func TestTheReleasesAreNamedByDate(t *testing.T) {
	for _, f := range []string{"../../VERSION", "../platform/chat-release"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if v := strings.TrimSpace(string(b)); !calVer.MatchString(v) || string(b) != v+"\n" {
			t.Errorf("%s says %q; want one line, YY.0M.N", f, b)
		}
	}
}

// A release's images are built from exactly the bases they were tested on:
// every FROM is pinned by digest. Updates are made here, not proposed as
// pull requests: nothing (Dependabot included) opens them.
func TestEveryImagesBasesArePinned(t *testing.T) {
	dockerfiles, err := filepath.Glob("../*/Dockerfile")
	if err != nil || len(dockerfiles) == 0 {
		t.Fatalf("no Dockerfiles (%v)", err)
	}
	for _, f := range []string{"../../.github/dependabot.yml", "../../.github/dependabot.yaml"} {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("%s would open pull requests", f)
		}
	}
	from := regexp.MustCompile(`(?im)^FROM\s+(\S+)`)
	for _, f := range dockerfiles {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range from.FindAllStringSubmatch(string(b), -1) {
			if !regexp.MustCompile(`@sha256:[0-9a-f]{64}$`).MatchString(m[1]) {
				t.Errorf("%s: FROM %s isn't pinned by digest", f, m[1])
			}
		}
	}
}

// No npm package's install scripts run while an image is built: one a
// dependency changed could change the web chat the console serves.
func TestImagesRunNoNpmInstallScripts(t *testing.T) {
	dockerfiles, _ := filepath.Glob("../*/Dockerfile")
	npm := regexp.MustCompile(`(?m)\bnpm (ci|install|i)\b[^\n]*`)
	for _, f := range dockerfiles {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range npm.FindAllString(string(b), -1) {
			if !strings.Contains(line, "--ignore-scripts") {
				t.Errorf("%s: %q runs packages' install scripts", f, line)
			}
		}
	}
}

// Releases are tagged by date, and the release workflow runs on those tags
// only.
func TestTheReleaseRunsOnTagsByDate(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		On struct {
			Push struct {
				Tags     []string `yaml:"tags"`
				Branches []string `yaml:"branches"`
			} `yaml:"push"`
		} `yaml:"on"`
	}
	if err := yaml.Unmarshal(b, &wf); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(wf.On.Push.Tags, []string{"[0-9][0-9].[0-9][0-9].[0-9]*"}) || len(wf.On.Push.Branches) > 0 {
		t.Errorf("the release runs on tags %v, branches %v; want tags by date only", wf.On.Push.Tags, wf.On.Push.Branches)
	}
}

// A release can be rehearsed: run by hand, the workflow builds every image
// as a release would, and publishes nothing (no image pushed, no release
// drafted), so a release's first real run isn't its first run.
func TestAReleaseRehearsalPublishesNothing(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	type step struct {
		Name string         `yaml:"name"`
		If   string         `yaml:"if"`
		Uses string         `yaml:"uses"`
		With map[string]any `yaml:"with"`
	}
	var wf struct {
		On   map[string]any `yaml:"on"`
		Jobs map[string]struct {
			If    string `yaml:"if"`
			Steps []step `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(b, &wf); err != nil {
		t.Fatal(err)
	}
	if _, ok := wf.On["workflow_dispatch"]; !ok {
		t.Fatal("the release can't be run by hand")
	}
	// Only a tag's push publishes: an expression that's true only then.
	onlyOnPush := func(expr string) bool {
		return strings.Contains(expr, "github.event_name == 'push'")
	}
	if !onlyOnPush(wf.Jobs["release"].If) {
		t.Errorf("the release is drafted whenever the workflow runs (if: %q)", wf.Jobs["release"].If)
	}
	built := false
	for _, s := range wf.Jobs["images"].Steps {
		if strings.HasPrefix(s.Uses, "docker/login-action") && !onlyOnPush(s.If) {
			t.Errorf("a rehearsal signs in to the registry (if: %q)", s.If)
		}
		if strings.HasPrefix(s.Uses, "docker/build-push-action") {
			built = true
			if push, _ := s.With["push"].(string); !onlyOnPush(push) {
				t.Errorf("a rehearsal pushes the images (push: %v)", s.With["push"])
			}
		}
	}
	if !built {
		t.Error("the images aren't built")
	}
}
