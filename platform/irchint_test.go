// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/platform"
)

// An IRC app that forgets TLS on 6697 (mIRC's "6697" without the "+") is told
// what to change, not just cut off.
func TestPlainIRCOnTheTLSPortIsToldToUseTLS(t *testing.T) {
	p := platform.New(platform.Options{BaseDomain: "guildnet.test", Networks: platform.NewMemoryStore(), IRCPort: 6697,
		Logger: log.New(io.Discard, "", 0)})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go p.ServeIRCHint(l)

	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "NICK ada\r\nUSER ada 0 * :Ada\r\n")
	c.SetDeadline(time.Now().Add(5 * time.Second))
	var got strings.Builder
	sc := bufio.NewScanner(c)
	for sc.Scan() {
		got.WriteString(sc.Text() + "\n")
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("reading: %v (so far %q)", err, got.String())
	}
	text := got.String()
	if !strings.Contains(text, "ERROR :") || !strings.Contains(text, "TLS") || !strings.Contains(text, "+6697") {
		t.Errorf("the app was told:\n%s", text)
	}
}
