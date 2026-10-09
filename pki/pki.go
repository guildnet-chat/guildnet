// SPDX-License-Identifier: AGPL-3.0-only

// Package pki is GuildNet's own certificate authority, for TLS on the hops
// inside the VM (the platform to the host, runners to the platform's
// broker), where the internet's CAs can't issue names like "host". The host
// holds the CA, its key never leaving the host's own volume, and issues
// certificates for its API and the platform's broker; clients trust this CA
// alone. Certificates last a year and are issued again when near their end;
// servers pick new ones up without restarting.
package pki

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

const (
	caLife   = 10 * 365 * 24 * time.Hour
	certLife = 365 * 24 * time.Hour
	// renewWithin: a certificate with less than this left is issued again.
	renewWithin = 30 * 24 * time.Hour
)

// CA is the certificate authority.
type CA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte
}

// CertPEM is the CA's certificate, which clients trust: public.
func (c CA) CertPEM() []byte { return c.certPEM }

// EnsureCA is the CA kept in dir (ca.crt, and ca.key readable only by its
// owner), made there the first time.
func EnsureCA(dir string) (CA, error) {
	certFile, keyFile := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")
	if certPEM, err := os.ReadFile(certFile); err == nil {
		keyPEM, err := os.ReadFile(keyFile)
		if err != nil {
			return CA{}, err
		}
		return parseCA(certPEM, keyPEM)
	} else if !errors.Is(err, os.ErrNotExist) {
		return CA{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return CA{}, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return CA{}, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "GuildNet internal CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(caLife),
		IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return CA{}, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return CA{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := writeFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return CA{}, err
	}
	if err := writeFile(certFile, certPEM, 0o644); err != nil {
		return CA{}, err
	}
	return parseCA(certPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
}

func parseCA(certPEM, keyPEM []byte) (CA, error) {
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return CA{}, errors.New("the CA's files aren't PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return CA{}, err
	}
	k, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		return CA{}, err
	}
	key, ok := k.(*ecdsa.PrivateKey)
	if !ok || !key.PublicKey.Equal(cert.PublicKey) {
		return CA{}, errors.New("the CA's key doesn't match its certificate")
	}
	return CA{cert: cert, key: key, certPEM: certPEM}, nil
}

// Issue makes sure dir has name.crt and name.key, a server certificate from
// the CA for hosts (names, or IP addresses), good for more than a month
// after now: it issues a new one if not. The key is readable by anyone who
// can reach dir, which only the server it's for should.
func (c CA) Issue(dir, name string, hosts []string, now time.Time) error {
	certFile, keyFile := filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
	names, ips := splitHosts(hosts)
	if b, err := os.ReadFile(certFile); err == nil {
		if block, _ := pem.Decode(b); block != nil {
			if cert, err := x509.ParseCertificate(block.Bytes); err == nil && cert.CheckSignatureFrom(c.cert) == nil &&
				cert.NotAfter.Sub(now) > renewWithin && sameNames(cert.DNSNames, names) && sameNames(ipStrings(cert.IPAddresses), ipStrings(ips)) {
				return nil
			}
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	der, err := x509.CreateCertificate(rand.Reader, serverTemplate(name, hosts, now), c.cert, &key.PublicKey, c.key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := writeFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o644); err != nil {
		return err
	}
	return writeFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

// splitHosts is hosts' names, and its IP addresses.
func splitHosts(hosts []string) (names []string, ips []net.IP) {
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			ips = append(ips, ip)
		} else {
			names = append(names, h)
		}
	}
	return names, ips
}

// serverTemplate is a server certificate for hosts, from now for a year.
func serverTemplate(name string, hosts []string, now time.Time) *x509.Certificate {
	names, ips := splitHosts(hosts)
	return &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: name},
		DNSNames: names, IPAddresses: ips, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(certLife),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
}

// Request is a request for a certificate for the key in dir (name.key,
// readable only by its owner), made there the first time: the server
// elsewhere sends it to the CA's side to Sign, and its key never leaves.
// Asked again (to renew), the key stays the same, so the certificate being
// served goes on matching it until the new one is installed.
func Request(dir, name string) ([]byte, error) {
	keyFile := filepath.Join(dir, name+".key")
	var key *ecdsa.PrivateKey
	if b, err := os.ReadFile(keyFile); err == nil {
		block, _ := pem.Decode(b)
		if block == nil {
			return nil, fmt.Errorf("%s isn't PEM", keyFile)
		}
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		var ok bool
		if key, ok = k.(*ecdsa.PrivateKey); !ok {
			return nil, fmt.Errorf("%s isn't an ECDSA key", keyFile)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
			return nil, err
		}
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		if err := writeFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
			return nil, err
		}
	} else {
		return nil, err
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: name}}, key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}

// Sign is a server certificate from the CA, from now for a year, for the
// key in csrPEM (a Request) and for hosts: the names and addresses this
// side chooses, whatever the request asked for.
func (c CA) Sign(csrPEM []byte, name string, hosts []string, now time.Time) ([]byte, error) {
	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, errors.New("that isn't a certificate request")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("the request isn't signed by its key: %w", err)
	}
	der, err := x509.CreateCertificate(rand.Reader, serverTemplate(name, hosts, now), c.cert, csr.PublicKey, c.key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// Install puts certPEM (from Sign) in dir as name.crt, if it's for the key
// there (name.key): a server serving them picks it up.
func Install(dir, name string, certPEM []byte) error {
	keyPEM, err := os.ReadFile(filepath.Join(dir, name+".key"))
	if err != nil {
		return err
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return fmt.Errorf("the certificate isn't for this server's key: %w", err)
	}
	return writeFile(filepath.Join(dir, name+".crt"), certPEM, 0o644)
}

func sameNames(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

func ipStrings(ips []net.IP) []string {
	var out []string
	for _, ip := range ips {
		out = append(out, ip.String())
	}
	return out
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}

// writeFile writes a file aside, then moves it into place.
func writeFile(path string, b []byte, mode os.FileMode) error {
	tmp := path + ".new"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ServerTLS serves the certificate in certFile and keyFile, read again
// whenever the file changes (so a renewed one is picked up), and waited for
// if it isn't there yet. TLS 1.3 only.
func ServerTLS(certFile, keyFile string) *tls.Config {
	var mu sync.Mutex
	var cached *tls.Certificate
	var modified time.Time
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			mu.Lock()
			defer mu.Unlock()
			info, err := os.Stat(certFile)
			if err == nil && (cached == nil || info.ModTime() != modified) {
				if pair, err := tls.LoadX509KeyPair(certFile, keyFile); err == nil {
					cached, modified = &pair, info.ModTime()
				}
			}
			if cached == nil {
				return nil, fmt.Errorf("no certificate in %s yet", certFile)
			}
			return cached, nil
		},
	}
}

// ClientTLS trusts only the CA whose certificate is caPEM. TLS 1.3 only.
func ClientTLS(caPEM []byte) (*tls.Config, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("that isn't a CA certificate")
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool}, nil
}

// ClientTLSFromFile is ClientTLS with the CA's certificate read from caFile
// at each connection, so it can be set up before the file exists. It checks
// the server against the name the client sent, so it refuses a server
// reached by IP address (for which no name is sent): ClientTransport
// reaches those.
func ClientTLSFromFile(caFile string) *tls.Config {
	return clientTLSFromFile(caFile, "")
}

// ClientTransport is an HTTP transport trusting only the CA whose
// certificate is in caFile (read at each connection), which checks each
// server against the host it dialled, a name or an IP address.
func ClientTransport(caFile string) *http.Transport {
	return &http.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			d := tls.Dialer{Config: clientTLSFromFile(caFile, host)}
			return d.DialContext(ctx, network, addr)
		},
	}
}

// clientTLSFromFile checks the server against host, or, if that's "", the
// name the client sent.
func clientTLSFromFile(caFile, host string) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		ServerName: host,
		// The usual check is replaced by VerifyConnection's, against the CA
		// read from the file: never skipped.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			name := host
			if name == "" {
				name = cs.ServerName
			}
			if name == "" {
				return errors.New("no name to check the server's certificate against (reached by address: use pki.ClientTransport)")
			}
			caPEM, err := os.ReadFile(caFile)
			if err != nil {
				return fmt.Errorf("no CA to check the server against: %w", err)
			}
			roots := x509.NewCertPool()
			if !roots.AppendCertsFromPEM(caPEM) {
				return errors.New("the CA file isn't a certificate")
			}
			if len(cs.PeerCertificates) == 0 {
				return errors.New("the server gave no certificate")
			}
			inter := x509.NewCertPool()
			for _, c := range cs.PeerCertificates[1:] {
				inter.AddCert(c)
			}
			_, err = cs.PeerCertificates[0].Verify(x509.VerifyOptions{DNSName: name, Roots: roots, Intermediates: inter})
			return err
		},
	}
}
