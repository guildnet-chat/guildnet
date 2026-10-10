// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"fmt"
	"io"
	"net"
	"time"
)

// ServeIRCHint answers IRC apps that connect to the TLS port without TLS
// (the edge sends them here): it says to turn TLS on, and closes. It never
// reads what they send beyond letting it arrive, so a closed connection
// doesn't swallow the message.
func (p *Platform) ServeIRCHint(l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go func() {
			defer c.Close()
			c.SetDeadline(time.Now().Add(5 * time.Second))
			fmt.Fprintf(c, ":%s NOTICE * :*** This port takes IRC over TLS only. Turn on SSL/TLS in your IRC app; in mIRC, type the port as +%d.\r\n"+
				"ERROR :Closing link: turn on TLS (in mIRC, port +%d)\r\n", p.opts.BaseDomain, p.opts.IRCPort, p.opts.IRCPort)
			if tc, ok := c.(*net.TCPConn); ok {
				tc.CloseWrite()
			}
			// Let what the app already sent arrive, so closing doesn't reset
			// the connection before it reads the message.
			c.SetReadDeadline(time.Now().Add(2 * time.Second))
			io.Copy(io.Discard, io.LimitReader(c, 4096))
		}()
	}
}
