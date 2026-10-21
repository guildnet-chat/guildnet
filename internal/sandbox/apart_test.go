// SPDX-License-Identifier: AGPL-3.0-only

package sandbox_test

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/internal/sandbox"
)

// sandboxd checks, as it starts, that desks can't reach each other: one
// probe desk listens, another tries it. Here, on a Docker with no desks'
// firewall (a CI runner, Docker Desktop), they can, and it says so.
func TestWithoutTheFirewallDesksAreFoundToReachEachOther(t *testing.T) {
	if os.Getenv("GUILDNET_DOCKER_TESTS") != "1" {
		t.Skip("set GUILDNET_DOCKER_TESTS=1 to test against Docker")
	}
	image := "guildnet-desk-test-python:latest"
	build := exec.Command("docker", "build", "--quiet", "--tag", image, "-")
	build.Stdin = strings.NewReader("FROM alpine:3.20\nRUN apk add --no-cache python3\n")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the test image: %v: %s", err, out)
	}
	network := "guildnet-desks-test-apart"
	exec.Command("docker", "network", "create", "--internal", network).Run()
	t.Cleanup(func() { exec.Command("docker", "network", "rm", network).Run() })
	dk := &sandbox.Docker{Image: image, Network: network, Memory: "256m", CPUs: "1", Pids: 64}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	apart, err := dk.Apart(ctx)

	if err != nil {
		t.Fatal(err)
	}
	if apart {
		t.Error("desks with no firewall between them were found to be kept apart")
	}
	if out, _ := exec.Command("docker", "ps", "-a", "--filter", "label=guildnet.probe=1", "--format", "{{.Names}}").Output(); strings.TrimSpace(string(out)) != "" {
		t.Errorf("probe desks left behind: %s", out)
	}
}
