// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/guildnet-chat/guildnet/pki"
)

// brokerCerts is the TLS of a self-hosted console's broker, with no host to
// issue it: the console keeps a CA of its own in dir (a volume only it has),
// gives the broker a certificate from it for names, and puts the CA's
// certificate (public) in share, a volume the runner reads, so the runner
// trusts it and nothing else. Run at start and daily, it issues a new
// certificate only when it's near its end; the CA stays the same.
func brokerCerts(dir, share string, names []string, now time.Time) error {
	ca, err := pki.EnsureCA(dir)
	if err != nil {
		return err
	}
	if err := ca.Issue(dir, "broker", names, now); err != nil {
		return err
	}
	tmp := filepath.Join(share, "ca.crt.new")
	if err := os.WriteFile(tmp, ca.CertPEM(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(share, "ca.crt"))
}

// sandboxCerts gives sandboxd (which has Docker's socket, so only the
// console and the runner, trusting this CA, should reach it) a certificate
// from the console's CA in dir, for names, in to: a folder only sandboxd
// shares. Run with brokerCerts: at start, and daily.
func sandboxCerts(dir, to string, names []string, now time.Time) error {
	ca, err := pki.EnsureCA(dir)
	if err != nil {
		return err
	}
	return ca.Issue(to, "sandboxd", names, now)
}

// signSandboxCert signs sandboxd's request for a certificate (from in: a
// desks host of its own, whose key stays there) with the console's CA in
// dir, for names, and writes the certificate to out.
func signSandboxCert(dir string, names []string, in io.Reader, out io.Writer, now time.Time) error {
	ca, err := pki.EnsureCA(dir)
	if err != nil {
		return err
	}
	csr, err := io.ReadAll(io.LimitReader(in, 64<<10))
	if err != nil {
		return err
	}
	cert, err := ca.Sign(csr, "sandboxd", names, now)
	if err != nil {
		return err
	}
	_, err = out.Write(cert)
	return err
}
