// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
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

// sandboxCA is the certificate of the CA sandboxd's certificate is from,
// which the console trusts it by: a self-hosted console's own (in
// GUILDNET_BROKER_TLS_DIR), or the one a host shares with it (in
// GUILDNET_PKI_SHARED: hosted, where the host holds GuildNet's CA).
func sandboxCA(getenv func(string) string) (string, error) {
	for _, k := range []string{"GUILDNET_BROKER_TLS_DIR", "GUILDNET_PKI_SHARED"} {
		if dir := getenv(k); dir != "" {
			return filepath.Join(dir, "ca.crt"), nil
		}
	}
	return "", errors.New("an https GUILDNET_SANDBOX_URL needs the CA sandboxd's certificate is from: GUILDNET_BROKER_TLS_DIR (the console's own) or GUILDNET_PKI_SHARED (a host's)")
}

// sandboxPin is GUILDNET_SANDBOX_PIN, the key sandboxd is held to: "" if
// it's not set, an error if it isn't a pin or sandboxd isn't over TLS.
func sandboxPin(getenv func(string) string) (string, error) {
	pin := getenv("GUILDNET_SANDBOX_PIN")
	switch {
	case pin == "":
		return "", nil
	case !pki.ValidPin(pin):
		return "", errors.New(`GUILDNET_SANDBOX_PIN isn't a pin: "sha256/" and the base64 of 32 bytes (platform sandbox-cert prints it)`)
	case !strings.HasPrefix(getenv("GUILDNET_SANDBOX_URL"), "https://"):
		return "", errors.New("GUILDNET_SANDBOX_PIN needs sandboxd over TLS: an https GUILDNET_SANDBOX_URL")
	}
	return pin, nil
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
