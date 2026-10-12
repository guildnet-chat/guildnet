// SPDX-License-Identifier: AGPL-3.0-only

package sandbox_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/internal/sandbox"
)

// Against a real Docker, when GUILDNET_DOCKER_TESTS=1: a desk is a locked
// down container whose files outlive it, with no route out of the desks'
// network. A small image stands in for the desk image.

const testImage = "guildnet-desk-test:latest"

func dockerRig(t *testing.T) (*sandbox.Docker, sandbox.Desk) {
	t.Helper()
	if os.Getenv("GUILDNET_DOCKER_TESTS") != "1" {
		t.Skip("set GUILDNET_DOCKER_TESTS=1 to test against Docker")
	}
	build := exec.Command("docker", "build", "--quiet", "--tag", testImage, "-")
	build.Stdin = strings.NewReader("FROM alpine:3.20\nRUN mkdir -p /workspace /home/desk && chown 1000:1000 /workspace /home/desk\n")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the test image: %v: %s", err, out)
	}
	network := "guildnet-desks-test"
	exec.Command("docker", "network", "create", "--internal", network).Run()
	nick := strings.ToLower(strings.TrimPrefix(t.Name(), "Test"))
	d := sandbox.Desk{Network: "test", Nick: nick[:min(len(nick), 32)]}
	dk := &sandbox.Docker{Image: testImage, Network: network, Memory: "256m", CPUs: "1", Pids: 64}
	t.Cleanup(func() { dk.Remove(context.Background(), d) })
	dk.Remove(context.Background(), d)
	if err := dk.Ensure(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	return dk, d
}

func run(t *testing.T, dk *sandbox.Docker, d sandbox.Desk, timeout time.Duration, args ...string) (string, sandbox.Result) {
	t.Helper()
	var out bytes.Buffer
	res, err := dk.Exec(context.Background(), d, sandbox.Exec{Args: args, Dir: "/workspace", Output: &out, Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	return out.String(), res
}

func TestADeskIsALockedDownContainer(t *testing.T) {
	dk, d := dockerRig(t)

	out, err := exec.Command("docker", "container", "inspect", "--format",
		"{{.HostConfig.CapDrop}} {{.HostConfig.ReadonlyRootfs}} {{.Config.User}} {{.HostConfig.PidsLimit}} {{.HostConfig.SecurityOpt}}", d.Name()).CombinedOutput()

	if err != nil || strings.TrimSpace(string(out)) != "[ALL] true 1000:1000 64 [no-new-privileges]" {
		t.Errorf("%s (%v)", out, err)
	}
	if who, _ := run(t, dk, d, 10*time.Second, "id", "-u"); strings.TrimSpace(who) != "1000" {
		t.Errorf("runs as %q", who)
	}
	if _, res := run(t, dk, d, 10*time.Second, "touch", "/etc/x"); res.Exit == 0 {
		t.Error("wrote to the image's files")
	}
}

func TestADesksFilesComeAndGoThroughDocker(t *testing.T) {
	dk, d := dockerRig(t)
	ctx := context.Background()

	if err := dk.WriteFile(ctx, d, "/workspace/deep/dir/notes.md", []byte("# plan\n")); err != nil {
		t.Fatal(err)
	}
	got, err := dk.ReadFile(ctx, d, "/workspace/deep/dir/notes.md", 1<<20)
	_, missing := dk.ReadFile(ctx, d, "/workspace/nope", 1<<20)
	_, big := dk.ReadFile(ctx, d, "/workspace/deep/dir/notes.md", 3)

	if err != nil || string(got) != "# plan\n" {
		t.Errorf("read %q, %v", got, err)
	}
	if !errors.Is(missing, sandbox.ErrNoFile) || !errors.Is(big, sandbox.ErrTooBig) {
		t.Errorf("missing: %v, too big: %v", missing, big)
	}
}

func TestACommandsOutputExitAndInputThroughDocker(t *testing.T) {
	dk, d := dockerRig(t)

	out, res := run(t, dk, d, 10*time.Second, "sh", "-c", "echo out; echo err >&2; exit 3")
	var echoed bytes.Buffer
	dk.Exec(context.Background(), d, sandbox.Exec{Args: []string{"cat"}, Dir: "/workspace", Stdin: strings.NewReader("secret"), Output: &echoed, Timeout: 10 * time.Second})

	if !strings.Contains(out, "out") || !strings.Contains(out, "err") || res.Exit != 3 || res.TimedOut {
		t.Errorf("%q, %+v", out, res)
	}
	if echoed.String() != "secret" {
		t.Errorf("input came out as %q", echoed.String())
	}
}

// What sandboxd runs at a desk itself (the timeout around each command, the
// shell that reads and writes files) is the image's, by its full path: a
// program an earlier command left in the home, ahead on the PATH, doesn't
// stand in for it and see what comes in on its input.
func TestADesksOwnFilesDontStandInForWhatSandboxdRuns(t *testing.T) {
	if os.Getenv("GUILDNET_DOCKER_TESTS") != "1" {
		t.Skip("set GUILDNET_DOCKER_TESTS=1 to test against Docker")
	}
	image := "guildnet-desk-test-planted:latest"
	build := exec.Command("docker", "build", "--quiet", "--tag", image, "-")
	build.Stdin = strings.NewReader("FROM alpine:3.20\n" +
		"RUN mkdir -p /workspace /home/desk/bin && for c in timeout sh; do printf '#!/bin/busybox sh\\necho planted\\n' > /home/desk/bin/$c; chmod +x /home/desk/bin/$c; done && chown -R 1000:1000 /workspace /home/desk\n" +
		"ENV PATH=/home/desk/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the test image: %v: %s", err, out)
	}
	network := "guildnet-desks-test"
	exec.Command("docker", "network", "create", "--internal", network).Run()
	d := sandbox.Desk{Network: "test", Nick: "planted"}
	dk := &sandbox.Docker{Image: image, Network: network, Memory: "256m", CPUs: "1", Pids: 64}
	t.Cleanup(func() { dk.Remove(context.Background(), d) })
	dk.Remove(context.Background(), d)
	if err := dk.Ensure(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	out, _ := run(t, dk, d, 10*time.Second, "/bin/echo", "hi")
	writeErr := dk.WriteFile(ctx, d, "/workspace/notes.md", []byte("kept"))
	got, readErr := dk.ReadFile(ctx, d, "/workspace/notes.md", 100)

	if strings.TrimSpace(out) != "hi" {
		t.Errorf("a command ran as %q: the desk's own timeout stood in", out)
	}
	if writeErr != nil || readErr != nil || string(got) != "kept" {
		t.Errorf("files through the desk's own sh: wrote %v, read %q, %v", writeErr, got, readErr)
	}
}

func TestACommandThatRunsTooLongIsStopped(t *testing.T) {
	dk, d := dockerRig(t)

	start := time.Now()
	_, res := run(t, dk, d, 2*time.Second, "sleep", "60")

	if !res.TimedOut || time.Since(start) > 20*time.Second {
		t.Errorf("%+v after %v", res, time.Since(start))
	}
}

func TestADesksFilesOutliveItsContainerStopping(t *testing.T) {
	dk, d := dockerRig(t)
	ctx := context.Background()
	dk.WriteFile(ctx, d, "/workspace/keep.txt", []byte("kept"))

	if err := dk.Stop(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := dk.Ensure(ctx, d); err != nil {
		t.Fatal(err)
	}

	if got, err := dk.ReadFile(ctx, d, "/workspace/keep.txt", 100); err != nil || string(got) != "kept" {
		t.Errorf("after a stop: %q, %v", got, err)
	}
}

func TestADeskHasNoRouteOut(t *testing.T) {
	dk, d := dockerRig(t)

	_, res := run(t, dk, d, 15*time.Second, "wget", "-q", "-T", "3", "-O", "-", "http://1.1.1.1/")

	if res.Exit == 0 {
		t.Error("the desk reached the internet directly")
	}
}

// A deleted network's desks all go, their volumes too; another network's
// stay.
func TestRemovingANetworkDeletesAllItsDesks(t *testing.T) {
	dk, d := dockerRig(t)
	ctx := context.Background()
	other := sandbox.Desk{Network: "test-other", Nick: d.Nick}
	t.Cleanup(func() { dk.Remove(context.Background(), other) })
	if err := dk.Ensure(ctx, other); err != nil {
		t.Fatal(err)
	}
	dk.WriteFile(ctx, d, "/workspace/gone.txt", []byte("x"))
	dk.WriteFile(ctx, other, "/workspace/kept.txt", []byte("y"))

	if err := dk.RemoveNetwork(ctx, d.Network); err != nil {
		t.Fatal(err)
	}

	if out, _ := exec.Command("docker", "volume", "ls", "--quiet").CombinedOutput(); strings.Contains(string(out), d.Name()+".") {
		t.Errorf("the network's volumes are still there:\n%s", out)
	}
	if got, err := dk.ReadFile(ctx, other, "/workspace/kept.txt", 100); err != nil || string(got) != "y" {
		t.Errorf("another network's desk: %q %v", got, err)
	}
}

// sandboxd starting knows of no desk running, so it stops any left running:
// none may run uncounted.
func TestStartingStopsDesksLeftRunning(t *testing.T) {
	dk, d := dockerRig(t)
	ctx := context.Background()
	dk.WriteFile(ctx, d, "/workspace/kept.txt", []byte("x"))

	if err := dk.StopAll(ctx); err != nil {
		t.Fatal(err)
	}

	if out, _ := exec.Command("docker", "container", "inspect", "--format", "{{.State.Running}}", d.Name()).CombinedOutput(); strings.TrimSpace(string(out)) != "false" {
		t.Errorf("the desk is still running: %s", out)
	}
	if got, err := dk.ReadFile(ctx, d, "/workspace/kept.txt", 100); err != nil || string(got) != "x" { // ReadFile needs it running
		dk.Ensure(ctx, d)
		if got, err = dk.ReadFile(ctx, d, "/workspace/kept.txt", 100); err != nil || string(got) != "x" {
			t.Errorf("its files after: %q %v", got, err)
		}
	}
}

// A desk made before its settings changed (gVisor turned on, tighter
// limits, a new image) is made again with them the next time it starts,
// its files kept: no desk keeps running as it was.
func TestADeskIsMadeAgainWhenItsSettingsChange(t *testing.T) {
	dk, d := dockerRig(t)
	ctx := context.Background()
	dk.WriteFile(ctx, d, "/workspace/keep.txt", []byte("kept"))
	if err := dk.Stop(ctx, d); err != nil {
		t.Fatal(err)
	}

	dk.Pids = 48
	if err := dk.Ensure(ctx, d); err != nil {
		t.Fatal(err)
	}

	if out, _ := exec.Command("docker", "container", "inspect", "--format", "{{.HostConfig.PidsLimit}}", d.Name()).CombinedOutput(); strings.TrimSpace(string(out)) != "48" {
		t.Errorf("after the change, its pids limit is %s, want 48", out)
	}
	if got, err := dk.ReadFile(ctx, d, "/workspace/keep.txt", 100); err != nil || string(got) != "kept" {
		t.Errorf("its files after: %q, %v", got, err)
	}
}

// A desk reaps what's left of commands that started things in the
// background, so they don't pile up against its limit on processes.
func TestADeskReapsWhatsLeftOfCommands(t *testing.T) {
	dk, d := dockerRig(t)

	run(t, dk, d, 10*time.Second, "sh", "-c", "for i in 1 2 3 4 5; do (sleep 0.1 &) ; done; sleep 0.5")
	time.Sleep(time.Second)

	if out, _ := run(t, dk, d, 10*time.Second, "sh", "-c", "ps -o stat | grep -c Z || true"); strings.TrimSpace(out) != "0" {
		t.Errorf("%s processes left as zombies", strings.TrimSpace(out))
	}
}

func TestRemovingADeskDeletesItsFiles(t *testing.T) {
	dk, d := dockerRig(t)
	ctx := context.Background()
	dk.WriteFile(ctx, d, "/workspace/gone.txt", []byte("x"))

	if err := dk.Remove(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := dk.Ensure(ctx, d); err != nil {
		t.Fatal(err)
	}

	if _, err := dk.ReadFile(ctx, d, "/workspace/gone.txt", 100); !errors.Is(err, sandbox.ErrNoFile) {
		t.Errorf("after removing: %v", err)
	}
}
