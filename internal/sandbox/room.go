// SPDX-License-Identifier: AGPL-3.0-only

package sandbox

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// How many desks the VM has room for: its memory, less what Docker,
// gVisor, sandboxd and the proxy need, shared out at each desk's limit.

// reserved is what's kept back from desks.
const reserved = 1536 << 20

// DesksFor is how many desks of deskMemory bytes fit in total bytes: at
// least one.
func DesksFor(total, deskMemory int64) int {
	if deskMemory <= 0 {
		return 1
	}
	return max(int((total-reserved)/deskMemory), 1)
}

// ParseMemory reads a size as Docker takes it: "3g", "512m", "2048k", or
// bytes.
func ParseMemory(s string) (int64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "g"):
		mult, s = 1<<30, strings.TrimSuffix(s, "g")
	case strings.HasSuffix(s, "m"):
		mult, s = 1<<20, strings.TrimSuffix(s, "m")
	case strings.HasSuffix(s, "k"):
		mult, s = 1<<10, strings.TrimSuffix(s, "k")
	case strings.HasSuffix(s, "b"):
		s = strings.TrimSuffix(s, "b")
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%q isn't a size like 3g or 512m", s)
	}
	return n * mult, nil
}

// MemTotal is the machine's memory, from /proc/meminfo's MemTotal.
func MemTotal(meminfo io.Reader) (int64, error) {
	sc := bufio.NewScanner(meminfo)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "MemTotal:"); ok {
			f := strings.Fields(rest)
			if len(f) == 0 {
				break
			}
			kb, err := strconv.ParseInt(f[0], 10, 64)
			if err != nil {
				return 0, err
			}
			return kb << 10, nil
		}
	}
	return 0, fmt.Errorf("no MemTotal in meminfo")
}

// StopAll stops every desk running: when sandboxd starts, it knows of none,
// so none may be running uncounted.
func (dk *Docker) StopAll(ctx context.Context) error {
	out, err := dk.docker(ctx, "ps", "--quiet", "--filter", "label=guildnet.desk=1")
	if err != nil {
		return err
	}
	if ids := strings.Fields(out); len(ids) > 0 {
		_, err = dk.docker(ctx, append([]string{"stop", "--time", "5"}, ids...)...)
	}
	return err
}
