// SPDX-License-Identifier: AGPL-3.0-only

package selfhost_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// What the release workflow publishes can be trusted to be what this repo
// says: actions are pinned to a commit (a moved tag can't change what runs
// with the right to publish), a release is only of a commit of main whose
// CI passed, and each image is signed (keyless, by this repo's release
// workflow) with its build's provenance.

type workflow struct {
	On   map[string]any `yaml:"on"`
	Jobs map[string]struct {
		If          string            `yaml:"if"`
		Permissions map[string]string `yaml:"permissions"`
		Steps       []struct {
			ID   string         `yaml:"id"`
			Name string         `yaml:"name"`
			If   string         `yaml:"if"`
			Uses string         `yaml:"uses"`
			Run  string         `yaml:"run"`
			With map[string]any `yaml:"with"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func readWorkflow(t *testing.T, name string) workflow {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("../../.github/workflows", name))
	if err != nil {
		t.Fatal(err)
	}
	var wf workflow
	if err := yaml.Unmarshal(b, &wf); err != nil {
		t.Fatal(err)
	}
	return wf
}

func TestEveryActionIsPinnedToACommit(t *testing.T) {
	files, err := filepath.Glob("../../.github/workflows/*.yml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflows (%v)", err)
	}
	pinned := regexp.MustCompile(`@[0-9a-f]{40}$`)
	for _, f := range files {
		for job, j := range readWorkflow(t, filepath.Base(f)).Jobs {
			for _, s := range j.Steps {
				if s.Uses != "" && !strings.HasPrefix(s.Uses, "./") && !pinned.MatchString(s.Uses) {
					t.Errorf("%s, %s: %s isn't pinned to a commit", filepath.Base(f), job, s.Uses)
				}
			}
		}
	}
}

func TestAReleaseIsOfACommitOfMainWhoseCIPassed(t *testing.T) {
	wf := readWorkflow(t, "release.yml")
	var gate string
	for _, s := range wf.Jobs["check"].Steps {
		if strings.Contains(s.Run, "merge-base --is-ancestor") && strings.Contains(s.Run, "ci.yml") {
			gate = s.If
		}
	}
	if gate == "" || !strings.Contains(gate, "github.event_name == 'push'") {
		t.Errorf("no check, on a tag's push, that its commit is on main and passed CI (if: %q)", gate)
	}
	if wf.Jobs["check"].Permissions["actions"] != "read" {
		t.Error("the check can't read CI's runs")
	}
}

func TestReleasedImagesAreSignedWithTheirProvenance(t *testing.T) {
	wf := readWorkflow(t, "release.yml")
	images := wf.Jobs["images"]
	if images.Permissions["id-token"] != "write" || images.Permissions["attestations"] != "write" {
		t.Errorf("the images job can't sign or attest: %v", images.Permissions)
	}
	var signs, attests bool
	for _, s := range images.Steps {
		pushOnly := strings.Contains(s.If, "github.event_name == 'push'")
		if strings.Contains(s.Run, "cosign sign") && pushOnly {
			signs = true
		}
		if strings.HasPrefix(s.Uses, "actions/attest-build-provenance@") && pushOnly {
			attests = true
		}
	}
	if !signs || !attests {
		t.Errorf("published images: signed %v, provenance attested %v", signs, attests)
	}
}

// Each week every image is built afresh from main and scanned: a base or
// module with a known fix waiting says so, rather than shipping unnoticed.
func TestImagesAreScannedEachWeek(t *testing.T) {
	wf := readWorkflow(t, "scan.yml")
	if _, ok := wf.On["schedule"]; !ok {
		t.Fatal("the scan isn't scheduled")
	}
	b, err := os.ReadFile("../../.github/workflows/scan.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, image := range []string{"platform", "runner", "sandboxd", "desk", "ergo", "caddy", "postgres"} {
		if !strings.Contains(string(b), "image: "+image+",") {
			t.Errorf("%s isn't scanned", image)
		}
	}
}
