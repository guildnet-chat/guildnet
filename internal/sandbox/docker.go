// SPDX-License-Identifier: AGPL-3.0-only

package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
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
// desks' network, which has no way out but the proxy. Its files, /workspace
// and its home, outlive the container: in its network's folder in Files, or
// without Files, on two volumes of its own.
type Docker struct {
	Image   string // the desk image
	Runtime string // "runsc" for gVisor; "" for Docker's own
	Network string // the desks' network (internal: no route out)
	Proxy   string // the proxy's URL, e.g. http://egress:3128
	Memory  string // e.g. "3g"
	CPUs    string // e.g. "2"
	Pids    int    // most processes
	Bin     string // the docker command; "" for docker
	// Files, if set, is a folder on the Docker host for desks' files: one
	// folder for each network, one in that for each desk (work and home).
	// On XFS mounted with prjquota, each network's can be limited
	// (SetStorage). sandboxd reaches it only through short-lived containers
	// of the desk image.
	Files string
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
	if dk.Files != "" {
		if err := dk.ready(ctx, d); err != nil {
			return err
		}
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
		"--workdir", "/workspace",
		"--env", "HOME=/home/desk",
	}
	if dk.Files != "" {
		dir := dk.Files + "/" + d.Network + "/" + d.Nick
		args = append(args, "--mount", "type=bind,src="+dir+"/work,dst=/workspace", "--mount", "type=bind,src="+dir+"/home,dst=/home/desk")
	} else {
		args = append(args, "--volume", name+".work:/workspace", "--volume", name+".home:/home/desk")
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

// What sandboxd runs at a desk itself is the image's, by its full path, so
// nothing an earlier command left in the desk's home, ahead on the PATH,
// stands in for it.
const (
	deskTimeout = "/usr/bin/timeout"
	deskShell   = "/bin/sh"
)

// runMark is the variable each command, and so everything it starts, has
// in its environment: a command that runs too long takes all of those with
// it (killMarked), however they were started (setsid, nohup).
const runMark = "DESK_RUN"

// killMarked kills every process at a desk whose environment has
// DESK_RUN=$1, but the desk's init.
const killMarked = `for p in /proc/[0-9]*; do
  pid=${p#/proc/}; [ "$pid" = 1 ] && continue
  /usr/bin/tr '\0' '\n' < "$p/environ" 2>/dev/null | /bin/grep -qxF "DESK_RUN=$1" && kill -9 "$pid" 2>/dev/null
done; exit 0`

// Exec runs a command at the desk, stopped (TERM, then KILL 5 seconds on)
// by the desk's own timeout command once its time is up, with whatever it
// started.
func (dk *Docker) Exec(ctx context.Context, d Desk, e Exec) (Result, error) {
	secs := max(int(e.Timeout/time.Second), 1)
	mark := newRunMark()
	args := []string{"exec", "--interactive", "--workdir", e.Dir, "--env", runMark + "=" + mark, d.Name(), deskTimeout, "-k", "5", strconv.Itoa(secs)}
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
		if timedOut {
			dk.killRun(d, mark)
		}
		return Result{Exit: code, TimedOut: timedOut}, nil
	}
	return Result{}, fmt.Errorf("running at the desk: %w", err)
}

// killRun kills what's left of the command marked mark at d.
func (dk *Docker) killRun(d Desk, mark string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	exec.CommandContext(ctx, dk.bin(), "exec", d.Name(), deskShell, "-c", killMarked, "kill", mark).Run()
}

// newRunMark is a command's own mark: random, so no command can name
// another's.
func newRunMark() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// readFile prints a file if it's a file no larger than $2: it exits 3 if
// it isn't one, 4 if it's larger.
const readFile = `test -f "$1" || exit 3; size=$(stat -c %s -- "$1") || exit 3; test "$size" -le "$2" || exit 4; cat -- "$1"`

func (dk *Docker) ReadFile(ctx context.Context, d Desk, path string, max int64) ([]byte, error) {
	var out bytes.Buffer
	res, err := dk.Exec(ctx, d, Exec{Args: []string{deskShell, "-c", readFile, "read", path, strconv.FormatInt(max, 10)},
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
	res, err := dk.Exec(ctx, d, Exec{Args: []string{deskShell, "-c", writeFile, "write", path},
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
	if dk.Files != "" {
		return dk.files(ctx, false, nil, removeFolder, d.Network+"/"+d.Nick)
	}
	return nil
}

// removeFolder deletes $1, a network's or a desk's folder in Files.
const removeFolder = `rm -rf -- "/files/$1"`

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
	if dk.Files != "" {
		return dk.files(ctx, false, nil, removeFolder, network)
	}
	return nil
}

// files runs script (sh, with args as $1...) as root in a short-lived
// container of the desk image, with Files at /files and the volumes given,
// and no network: privileged to set limits.
func (dk *Docker) files(ctx context.Context, privileged bool, volumes []string, script string, args ...string) error {
	run := []string{"run", "--rm", "--network", "none", "--user", "0", "--entrypoint", "/bin/sh", "--volume", dk.Files + ":/files"}
	if privileged {
		run = append(run, "--privileged")
	}
	for _, v := range volumes {
		run = append(run, "--volume", v)
	}
	run = append(append(run, dk.Image, "-c", script, "files"), args...)
	_, err := dk.docker(ctx, run...)
	return err
}

// readyFiles makes a desk's folders the first time: its home as the
// image's, or its files moved from the volumes of before (/old).
const readyFiles = `set -e
d="/files/$1/$2"
mkdir -p "$d/work" "$d/home"
if [ ! -e "$d/.made" ]; then
  if [ -d /old/work ]; then cp -a /old/work/. "$d/work/"; fi
  if [ -d /old/home ]; then cp -a /old/home/. "$d/home/"; else cp -a /home/desk/. "$d/home/"; fi
  chown 1000:1000 "$d/work" "$d/home"
  touch "$d/.made"
fi`

// ready makes d's folders in Files if they aren't there, moving its files
// from the volumes it had before, which then go.
func (dk *Docker) ready(ctx context.Context, d Desk) error {
	name := d.Name()
	var old []string
	for _, part := range []string{"work", "home"} {
		if _, err := dk.docker(ctx, "volume", "inspect", name+"."+part); err == nil {
			old = append(old, name+"."+part+":/old/"+part)
		}
	}
	if err := dk.files(ctx, false, old, readyFiles, d.Network, d.Nick); err != nil {
		return fmt.Errorf("readying the desk's files: %w", err)
	}
	for _, v := range old {
		if _, err := dk.docker(ctx, "volume", "rm", strings.Split(v, ":")[0]); err != nil {
			return err
		}
	}
	return nil
}

// setStorage puts network's folder in Files in its XFS project ($2), limited
// to $3 GB.
const setStorage = `set -e
mkdir -p "/files/$1"
xfs_quota -x -c "project -s -p /files/$1 $2" -c "limit -p bhard=$3g $2" /files >/dev/null`

// SetStorage limits network's folder in Files to gb, its desks' files in
// all: an XFS project quota, the project's ID from the network's name.
func (dk *Docker) SetStorage(ctx context.Context, network string, gb int) error {
	if dk.Files == "" {
		return nil
	}
	if !validName.MatchString(network) {
		return fmt.Errorf("%q isn't a network's name", network)
	}
	if err := dk.files(ctx, true, nil, setStorage, network, strconv.FormatUint(uint64(projectID(network)), 10), strconv.Itoa(gb)); err != nil {
		return fmt.Errorf("%w (limits need Files on XFS, mounted with prjquota)", err)
	}
	return nil
}

// projectID is network's XFS project: from its name, never 0 (no project).
// Two names sharing one would share a limit; with 2^31 to choose from, a
// few thousand networks are unlikely to.
func projectID(network string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(network))
	return h.Sum32()&0x7fffffff | 1
}

// DiskUse is how full the disk Files is on is, in percent.
func (dk *Docker) DiskUse(ctx context.Context) (int, error) {
	out, err := dk.docker(ctx, "run", "--rm", "--network", "none", "--entrypoint", "/bin/sh", "--volume", dk.Files+":/files:ro", dk.Image, "-c", "df -P /files | tail -n 1")
	if err != nil {
		return 0, err
	}
	for _, f := range strings.Fields(out) {
		if pct, ok := strings.CutSuffix(f, "%"); ok {
			return strconv.Atoi(pct)
		}
	}
	return 0, fmt.Errorf("df said %q", strings.TrimSpace(out))
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
