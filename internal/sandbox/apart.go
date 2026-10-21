// SPDX-License-Identifier: AGPL-3.0-only

package sandbox

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"time"
)

// The probe desks' programs, in the desk image's Python: one listens on
// probePort, the other says whether it reached it.
const (
	probePort   = "8099"
	probeListen = `import socket
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("0.0.0.0", ` + probePort + `))
s.listen(4)
print("listening", flush=True)
s.settimeout(90)
try:
    while True:
        s.accept()[0].close()
except OSError:
    pass
`
	probeReach = `import socket, sys
s = socket.socket()
s.settimeout(5)
try:
    s.connect((sys.argv[1], ` + probePort + `))
    print("reached")
except socket.timeout:
    print("apart")
except ConnectionRefusedError:
    print("refused")
except OSError as e:
    print("apart:", e)
`
)

// Apart checks that desks on the desks' network can't reach each other, as
// the desks' firewall keeps them: a probe desk listens, and another tries
// it. It's false if the second got through. The probes are desks as any
// are made (the image, runtime and network, no capabilities), and go once
// it's known.
func (dk *Docker) Apart(ctx context.Context) (bool, error) {
	name := "guildnet-probe-" + strings.ToLower(rand.Text()[:12])
	probe := func(args ...string) []string {
		run := []string{"run", "--rm", "--label", "guildnet.probe=1", "--network", dk.Network,
			"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--user", "1000:1000",
			"--memory", "128m", "--entrypoint", "python3"}
		if dk.Runtime != "" {
			run = append(run, "--runtime", dk.Runtime)
		}
		return append(run, args...)
	}
	if _, err := dk.docker(ctx, probe("--detach", "--name", name, dk.Image, "-c", probeListen)...); err != nil {
		return false, fmt.Errorf("starting a probe desk: %w", err)
	}
	defer dk.docker(context.Background(), "rm", "--force", name)
	for {
		if out, _ := dk.docker(ctx, "logs", name); strings.Contains(out, "listening") {
			break
		}
		select {
		case <-ctx.Done():
			return false, fmt.Errorf("the probe desk didn't start listening: %w", ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
	ip, err := dk.docker(ctx, "container", "inspect", "--format", `{{(index .NetworkSettings.Networks "`+dk.Network+`").IPAddress}}`, name)
	if ip = strings.TrimSpace(ip); err != nil || ip == "" {
		return false, fmt.Errorf("the probe desk's address: %v %s", err, ip)
	}
	out, err := dk.docker(ctx, probe(dk.Image, "-c", probeReach, ip)...)
	if err != nil {
		return false, fmt.Errorf("the second probe desk: %w", err)
	}
	switch said := strings.TrimSpace(out); {
	case said == "reached":
		return false, nil
	case strings.HasPrefix(said, "apart"):
		return true, nil
	default:
		return false, fmt.Errorf("the probe desks couldn't tell: %s", said)
	}
}
