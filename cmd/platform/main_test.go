// SPDX-License-Identifier: AGPL-3.0-only

package main_test

import (
	"bytes"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// What anyone can run: the console on its own, with nothing set up. It
// starts (keeping its data in memory) and serves its pages, and none of
// GuildNet's hosted site, whose pages aren't there.
func TestTheOpenConsoleStartsWithNothingSetUp(t *testing.T) {
	addr := starts(t, ".")

	if status := get(t, addr, "/signin"); status != http.StatusOK {
		t.Errorf("sign-in: %d, want %d", status, http.StatusOK)
	}
	if status := get(t, addr, "/terms"); status != http.StatusNotFound {
		t.Errorf("the hosted edition's terms: %d, want %d", status, http.StatusNotFound)
	}
}

// starts builds the command in dir and runs it with none of GuildNet's
// settings but where to listen, until the test ends. It's at the address
// starts returns.
func starts(t *testing.T, dir string) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds and runs the command")
	}
	bin := filepath.Join(t.TempDir(), "platform")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = dir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()

	cmd := exec.Command(bin)
	cmd.Env = []string{"GUILDNET_PLATFORM_LISTEN=" + addr}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GUILDNET_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	var out lockedBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	t.Cleanup(func() { cmd.Process.Kill(); <-exited })

	for deadline := time.Now().Add(30 * time.Second); !strings.Contains(out.String(), "platform listening"); {
		select {
		case <-exited:
			t.Fatalf("it stopped:\n%s", out.String())
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("it didn't start listening:\n%s", out.String())
		}
	}
	return addr
}

// get asks for a page on the platform's own address (not a network's).
func get(t *testing.T, addr, path string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "guildnet.localhost"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}
