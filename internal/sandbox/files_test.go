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

// With a files folder (Docker.Files), a desk's files are in its network's
// folder there, not in Docker volumes: on XFS, each network's folder is
// limited to its storage. Against a real Docker, when
// GUILDNET_DOCKER_TESTS=1; the limit itself needs an XFS folder mounted with
// prjquota, on the Docker host, in GUILDNET_XFS_FILES.

const filesImage = "guildnet-desk-files-test:latest"

func filesRig(t *testing.T, files string) (*sandbox.Docker, sandbox.Desk) {
	t.Helper()
	if os.Getenv("GUILDNET_DOCKER_TESTS") != "1" {
		t.Skip("set GUILDNET_DOCKER_TESTS=1 to test against Docker")
	}
	build := exec.Command("docker", "build", "--quiet", "--tag", filesImage, "-")
	build.Stdin = strings.NewReader("FROM alpine:3.20\n" +
		"RUN apk add --no-cache xfsprogs-extra && mkdir -p /workspace /home/desk && echo '[user]' > /home/desk/.gitconfig && chown -R 1000:1000 /workspace /home/desk\n")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the test image: %v: %s", err, out)
	}
	network := "guildnet-desks-test"
	exec.Command("docker", "network", "create", "--internal", network).Run()
	nick := strings.ToLower(strings.TrimPrefix(t.Name(), "Test"))
	d := sandbox.Desk{Network: "files", Nick: nick[:min(len(nick), 32)]}
	dk := &sandbox.Docker{Image: filesImage, Network: network, Memory: "256m", CPUs: "1", Pids: 64, Files: files}
	t.Cleanup(func() { dk.Remove(context.Background(), d) })
	dk.Remove(context.Background(), d)
	return dk, d
}

// onHost is what script prints, run with the files folder at /files. If
// Docker can't run it, the test fails saying why (not with an empty answer).
func onHost(t *testing.T, files, script string) string {
	t.Helper()
	cmd := exec.Command("docker", "run", "--rm", "--volume", files+":/files", "--entrypoint", "/bin/sh", filesImage, "-c", script)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil && len(out) == 0 && !strings.Contains(stderr.String(), "No such file") {
		t.Fatalf("running %q with the files folder: %v: %s", script, err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

// listed is what's in the files folder, for a failure's message.
func listed(t *testing.T, files string) string {
	t.Helper()
	return onHost(t, files, "ls -laR /files 2>&1 | head -40")
}

const testFiles = "/var/tmp/guildnet-desk-files-test"

func TestADesksFilesAreInItsNetworksFolder(t *testing.T) {
	dk, d := filesRig(t, testFiles)
	ctx := context.Background()
	if err := dk.Ensure(ctx, d); err != nil {
		t.Fatal(err)
	}

	if err := dk.WriteFile(ctx, d, "/workspace/notes.md", []byte("kept")); err != nil {
		t.Fatal(err)
	}

	if got := onHost(t, testFiles, "cat /files/files/"+d.Nick+"/work/notes.md"); got != "kept" {
		t.Errorf("in the network's folder: %q; the folder has:\n%s", got, listed(t, testFiles))
	}
	// Its home starts as the image's, and is the desk's own.
	if got, err := dk.ReadFile(ctx, d, "/home/desk/.gitconfig", 100); err != nil || strings.TrimSpace(string(got)) != "[user]" {
		t.Errorf("its home's .gitconfig: %q %v", got, err)
	}
	if out, res := run(t, dk, d, 10*time.Second, "sh", "-c", "touch /home/desk/x /workspace/y && stat -c %u /workspace /home/desk"); res.Exit != 0 || strings.Fields(out)[0] != "1000" {
		t.Errorf("the desk can't write its own folders: %q %+v", out, res)
	}
}

// How much each desk's files take, for its network's owner to see.
func TestADesksFilesAreMeasured(t *testing.T) {
	dk, d := filesRig(t, testFiles)
	ctx := context.Background()
	if err := dk.Ensure(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := dk.WriteFile(ctx, d, "/workspace/big.bin", make([]byte, 2<<20)); err != nil {
		t.Fatal(err)
	}

	used, err := dk.Used(ctx, d.Network)

	if err != nil {
		t.Fatal(err)
	}
	if n := used[d.Nick]; n < 2<<20 || n > 4<<20 {
		t.Errorf("%s's desk takes %d bytes, want about 2 MiB; all: %v", d.Nick, n, used)
	}
	if none, err := dk.Used(ctx, "nobody-here"); err != nil || len(none) != 0 {
		t.Errorf("a network with no desks: %v %v", none, err)
	}
}

func TestADesksFilesOutliveItsContainerBeingMadeAgain(t *testing.T) {
	dk, d := filesRig(t, testFiles)
	ctx := context.Background()
	dk.Ensure(ctx, d)
	dk.WriteFile(ctx, d, "/workspace/keep.txt", []byte("kept"))
	dk.WriteFile(ctx, d, "/home/desk/.gitconfig", []byte("mine"))
	dk.Stop(ctx, d)

	dk.Pids = 48
	if err := dk.Ensure(ctx, d); err != nil {
		t.Fatal(err)
	}

	if got, err := dk.ReadFile(ctx, d, "/workspace/keep.txt", 100); err != nil || string(got) != "kept" {
		t.Errorf("its work: %q %v", got, err)
	}
	if got, _ := dk.ReadFile(ctx, d, "/home/desk/.gitconfig", 100); string(got) != "mine" {
		t.Errorf("its home was put back as the image's: %q", got)
	}
}

// A desk from before (its files in Docker volumes) has them moved to its
// network's folder when it's next made, and the volumes go.
func TestAnOldDesksFilesMoveToItsNetworksFolder(t *testing.T) {
	dk, d := filesRig(t, testFiles)
	ctx := context.Background()
	old := *dk
	old.Files = ""
	if err := old.Ensure(ctx, d); err != nil {
		t.Fatal(err)
	}
	old.WriteFile(ctx, d, "/workspace/old.txt", []byte("moved"))
	old.WriteFile(ctx, d, "/home/desk/.history", []byte("mine"))
	old.Stop(ctx, d)

	if err := dk.Ensure(ctx, d); err != nil {
		t.Fatal(err)
	}

	if got, err := dk.ReadFile(ctx, d, "/workspace/old.txt", 100); err != nil || string(got) != "moved" {
		t.Errorf("its work: %q %v", got, err)
	}
	if got, err := dk.ReadFile(ctx, d, "/home/desk/.history", 100); err != nil || string(got) != "mine" {
		t.Errorf("its home: %q %v", got, err)
	}
	if out, _ := exec.Command("docker", "volume", "ls", "--quiet").CombinedOutput(); strings.Contains(string(out), d.Name()+".") {
		t.Errorf("its old volumes are still there:\n%s", out)
	}
}

func TestRemovingADeskOrANetworkDeletesTheirFolders(t *testing.T) {
	dk, d := filesRig(t, testFiles)
	ctx := context.Background()
	other := sandbox.Desk{Network: d.Network, Nick: d.Nick + "-2"}
	t.Cleanup(func() { dk.Remove(context.Background(), other) })
	dk.Ensure(ctx, d)
	dk.Ensure(ctx, other)
	dk.WriteFile(ctx, d, "/workspace/a.txt", []byte("x"))
	dk.WriteFile(ctx, other, "/workspace/b.txt", []byte("y"))

	if err := dk.Remove(ctx, d); err != nil {
		t.Fatal(err)
	}
	if got := onHost(t, testFiles, "test -e /files/files/"+d.Nick+" && echo there || echo gone; cat /files/files/"+other.Nick+"/work/b.txt"); got != "gone\ny" {
		t.Errorf("after removing the desk, its folder is, and the other desk's file is: %q", got)
	}
	if err := dk.RemoveNetwork(ctx, d.Network); err != nil {
		t.Fatal(err)
	}
	if got := onHost(t, testFiles, "test -e /files/files && echo there || echo gone"); got != "gone" {
		t.Errorf("after removing the network, its folder is %s", got)
	}
}

// On XFS, a network's desks can't take more than its storage between them,
// and another network's are untouched.
func TestANetworksDesksStayWithinItsStorage(t *testing.T) {
	xfs := os.Getenv("GUILDNET_XFS_FILES")
	if xfs == "" {
		t.Skip("set GUILDNET_XFS_FILES to an XFS folder mounted with prjquota, on the Docker host")
	}
	dk, d := filesRig(t, xfs)
	ctx := context.Background()
	other := sandbox.Desk{Network: "files-other", Nick: d.Nick}
	t.Cleanup(func() { dk.RemoveNetwork(context.Background(), other.Network) })
	if err := dk.SetStorage(ctx, d.Network, 1); err != nil {
		t.Fatal(err)
	}
	dk.Ensure(ctx, d)
	dk.Ensure(ctx, other)

	_, full := run(t, dk, d, time.Minute, "dd", "if=/dev/zero", "of=/workspace/big", "bs=1M", "count=1500")
	_, fine := run(t, dk, other, time.Minute, "dd", "if=/dev/zero", "of=/workspace/big", "bs=1M", "count=1100")

	if full.Exit == 0 {
		t.Error("a network limited to 1 GB wrote 1.5 GB")
	}
	if fine.Exit != 0 {
		t.Errorf("another network couldn't write: %+v", fine)
	}
}
