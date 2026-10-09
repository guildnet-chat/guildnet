// SPDX-License-Identifier: AGPL-3.0-only

package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Docker keeps desks as Docker containers, through the docker command: on
// the sandbox VM with gVisor (runsc) as their runtime, locally with Docker's
// own. Each desk's container is locked down: no capabilities, a read-only
// root, a non-root user, limits on memory, CPU and processes, and only the
// desks' network, which has no way out but the proxy. Its files are on two
// volumes of its own, /workspace and its home, which outlive the container.
type Docker struct {
	Image   string // the desk image
	Runtime string // "runsc" for gVisor; "" for Docker's own
	Network string // the desks' network (internal: no route out)
	Proxy   string // the proxy's URL, e.g. http://egress:3128
	Memory  string // e.g. "3g"
	CPUs    string // e.g. "2"
	Pids    int    // most processes
	Bin     string // the docker command; "" for docker
}

func (dk *Docker) bin() string {
	if dk.Bin == "" {
		return "docker"
	}
	return dk.Bin
}

// docker runs the docker command, and gives what it printed if it failed.
func (dk *Docker) docker(ctx context.Context, args ...string) (string, error) {
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, dk.bin(), args...)
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("docker %s: %v: %s", args[0], err, strings.TrimSpace(out.String()))
	}
	return out.String(), nil
}

// Ensure makes the desk's container, or starts it if it's stopped. A
// stopped one made with other settings, or from another image, is made
// again (its files are on its volumes, and stay): a desk made before gVisor
// was turned on, say, mustn't go on without it. A running one is left to
// finish; it's made again once it has stopped.
func (dk *Docker) Ensure(ctx context.Context, d Desk) error {
	name := d.Name()
	args, err := dk.runArgs(ctx, d)
	if err != nil {
		return err
	}
	out, err := dk.docker(ctx, "container", "inspect", "--format", `{{.State.Running}} {{index .Config.Labels "guildnet.made"}}`, name)
	switch {
	case err == nil && strings.HasPrefix(out, "true"):
		return nil
	case err == nil && strings.TrimSpace(strings.TrimPrefix(out, "false")) == made(args):
		_, err = dk.docker(ctx, "start", name)
		return err
	case err == nil:
		if _, err := dk.docker(ctx, "rm", "--force", name); err != nil {
			return err
		}
	case !strings.Contains(strings.ToLower(out), "no such"):
		return err
	}
	n := len(args) - 3 // the image and its command come last
	run := append(append(slices.Clone(args[:n]), "--label", "guildnet.made="+made(args)), args[n:]...)
	_, err = dk.docker(ctx, run...)
	return err
}

// made is what a desk was made with, for its label: its settings, and the
// image they name (by its ID, so a rebuilt image counts as a change).
func made(args []string) string {
	sum := sha256.Sum256([]byte(strings.Join(args, "\x00")))
	return hex.EncodeToString(sum[:12])
}

// runArgs is how d's container is made, ending with the image (by ID) and
// its command.
func (dk *Docker) runArgs(ctx context.Context, d Desk) ([]string, error) {
	image, err := dk.docker(ctx, "image", "inspect", "--format", "{{.Id}}", dk.Image)
	if err != nil {
		return nil, err
	}
	name := d.Name()
	args := []string{"run", "--detach", "--name", name, "--hostname", d.Nick,
		"--label", "guildnet.desk=1", "--label", "guildnet.network=" + d.Network, "--label", "guildnet.nick=" + d.Nick,
		// An init reaps what commands leave running in the background.
		"--init",
		"--network", dk.Network,
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--read-only", "--tmpfs", "/tmp:rw,exec,nosuid,size=1g",
		"--user", "1000:1000",
		"--volume", name + ".work:/workspace", "--volume", name + ".home:/home/desk",
		"--workdir", "/workspace",
		"--env", "HOME=/home/desk",
	}
	if dk.Runtime != "" {
		args = append(args, "--runtime", dk.Runtime)
	}
	if dk.Memory != "" {
		args = append(args, "--memory", dk.Memory, "--memory-swap", dk.Memory)
	}
	if dk.CPUs != "" {
		args = append(args, "--cpus", dk.CPUs)
	}
	if dk.Pids > 0 {
		args = append(args, "--pids-limit", strconv.Itoa(dk.Pids))
	}
	if dk.Proxy != "" {
		for _, v := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
			args = append(args, "--env", v+"="+dk.Proxy)
		}
		args = append(args, "--env", "NO_PROXY=localhost,127.0.0.1", "--env", "no_proxy=localhost,127.0.0.1")
	}
	return append(args, strings.TrimSpace(image), "sleep", "infinity"), nil
}

// Exec runs a command at the desk, stopped (TERM, then KILL 5 seconds on)
// by the desk's own timeout command once its time is up.
func (dk *Docker) Exec(ctx context.Context, d Desk, e Exec) (Result, error) {
	secs := max(int(e.Timeout/time.Second), 1)
	args := []string{"exec", "--interactive", "--workdir", e.Dir, d.Name(), "timeout", "-k", "5", strconv.Itoa(secs)}
	args = append(args, e.Args...)
	// The docker command itself gets a little longer, in case.
	ctx, cancel := context.WithTimeout(ctx, e.Timeout+30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, dk.bin(), args...)
	cmd.Stdin = e.Stdin
	if cmd.Stdin == nil {
		cmd.Stdin = strings.NewReader("")
	}
	cmd.Stdout, cmd.Stderr = e.Output, e.Output
	start := time.Now()
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return Result{}, nil
	case errors.As(err, &exit):
		code := exit.ExitCode()
		// timeout exits 124 when it stopped the command (busybox's, 143),
		// or 137 if it had to kill it.
		timedOut := (code == 124 || code == 143 || code == 137) && time.Since(start) >= e.Timeout-time.Second
		return Result{Exit: code, TimedOut: timedOut}, nil
	}
	return Result{}, fmt.Errorf("running at the desk: %w", err)
}

// readFile prints a file if it's a file no larger than $2: it exits 3 if
// it isn't one, 4 if it's larger.
const readFile = `test -f "$1" || exit 3; size=$(stat -c %s -- "$1") || exit 3; test "$size" -le "$2" || exit 4; cat -- "$1"`

func (dk *Docker) ReadFile(ctx context.Context, d Desk, path string, max int64) ([]byte, error) {
	var out bytes.Buffer
	res, err := dk.Exec(ctx, d, Exec{Args: []string{"sh", "-c", readFile, "read", path, strconv.FormatInt(max, 10)},
		Dir: "/", Output: &limited{w: &out, n: max + 1}, Timeout: time.Minute})
	switch {
	case err != nil:
		return nil, err
	case res.Exit == 3:
		return nil, fmt.Errorf("%s: %w", path, ErrNoFile)
	case res.Exit == 4:
		return nil, fmt.Errorf("%s: %w", path, ErrTooBig)
	case res.Exit != 0:
		return nil, fmt.Errorf("reading %s: exit %d", path, res.Exit)
	}
	return out.Bytes(), nil
}

// writeFile writes its input to $1, making its directories.
const writeFile = `mkdir -p -- "$(dirname -- "$1")" && cat > "$1"`

func (dk *Docker) WriteFile(ctx context.Context, d Desk, path string, data []byte) error {
	var out bytes.Buffer
	res, err := dk.Exec(ctx, d, Exec{Args: []string{"sh", "-c", writeFile, "write", path},
		Dir: "/", Stdin: bytes.NewReader(data), Output: &limited{w: &out, n: 4096}, Timeout: time.Minute})
	if err != nil {
		return err
	}
	if res.Exit != 0 {
		return fmt.Errorf("writing %s: %s", path, strings.TrimSpace(out.String()))
	}
	return nil
}

func (dk *Docker) Stop(ctx context.Context, d Desk) error {
	_, err := dk.docker(ctx, "stop", "--time", "5", d.Name())
	return err
}

func (dk *Docker) Remove(ctx context.Context, d Desk) error {
	name := d.Name()
	if out, err := dk.docker(ctx, "rm", "--force", "--volumes", name); err != nil && !strings.Contains(strings.ToLower(out), "no such") {
		return err
	}
	for _, v := range []string{name + ".work", name + ".home"} {
		if out, err := dk.docker(ctx, "volume", "rm", "--force", v); err != nil && !strings.Contains(strings.ToLower(out), "no such") {
			return err
		}
	}
	return nil
}

// RemoveNetwork deletes every container labelled with network, and every
// volume of a desk of its (gn.<network>.<nick>.work and .home), whether or
// not its container is still there.
func (dk *Docker) RemoveNetwork(ctx context.Context, network string) error {
	if !validName.MatchString(network) {
		return fmt.Errorf("%q isn't a network's name", network)
	}
	out, err := dk.docker(ctx, "ps", "--all", "--quiet", "--filter", "label=guildnet.network="+network)
	if err != nil {
		return err
	}
	if ids := strings.Fields(out); len(ids) > 0 {
		if _, err := dk.docker(ctx, append([]string{"rm", "--force", "--volumes"}, ids...)...); err != nil {
			return err
		}
	}
	out, err = dk.docker(ctx, "volume", "ls", "--quiet")
	if err != nil {
		return err
	}
	prefix := "gn." + network + "."
	for _, v := range strings.Fields(out) {
		if !strings.HasPrefix(v, prefix) {
			continue
		}
		if out, err := dk.docker(ctx, "volume", "rm", "--force", v); err != nil && !strings.Contains(strings.ToLower(out), "no such") {
			return err
		}
	}
	return nil
}

// limited writes at most n bytes to w, and quietly drops the rest.
type limited struct {
	w io.Writer
	n int64
}

func (l *limited) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(int64(len(p)), l.n)
		l.w.Write(p[:k])
		l.n -= k
	}
	return len(p), nil
}
