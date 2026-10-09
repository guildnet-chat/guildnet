// SPDX-License-Identifier: AGPL-3.0-only

// Package ergotest runs a throwaway Ergo IRC server in Docker for tests that
// need real Ergo behaviour: accounts, nickname enforcement, ChanServ, opers.
// It starts from GuildNet's own deploy/ergo/ircd.yaml, then turns on the
// HTTP API and an operator for GuildServ with test credentials.
package ergotest

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

// image is GuildNet's own Ergo (deploy/ergo): the upstream image with our
// config, MOTD, and start script. Built once per test run.
const image = "guildnet-ergo:test"

var (
	buildOnce sync.Once
	buildErr  error
)

func buildImage() error {
	buildOnce.Do(func() {
		_, here, _, _ := runtime.Caller(0)
		_, buildErr = docker("build", "-q", "-t", image, filepath.Join(filepath.Dir(here), "..", "..", "deploy", "ergo"))
	})
	return buildErr
}

// Server is a running Ergo container.
type Server struct {
	Addr         string // host:port for IRC
	APIURL       string // base URL of the HTTP API
	APIToken     string
	OperName     string
	OperPassword string
	// WebSocketURL is Ergo's WebSocket listener; JWTKey signs tokens it
	// accepts (SASL IRCV3BEARER) for accounts that already exist.
	WebSocketURL string
	JWTKey       string
	// JWTAudience, if set, is the only "aud" Ergo accepts.
	JWTAudience string
	// noMultiline turns off IRCv3 multiline, as some servers don't have it.
	noMultiline bool
	// network and webURL fill in the welcome (MOTD).
	network, webURL string
	id              string
}

// Welcome is the directory Ergo's welcome lives in (/welcome), which a
// network shares with its runner: files there are read and written inside
// the container.
func (s *Server) Welcome() Welcome { return Welcome{id: s.id} }

// Welcome reads and writes files in the server's /welcome.
type Welcome struct{ id string }

func (w Welcome) ReadFile(name string) ([]byte, error) {
	out, err := docker("exec", w.id, "cat", "/welcome/"+name)
	return []byte(out), err
}

func (w Welcome) WriteFile(name string, data []byte) error {
	path := "/welcome/" + name
	cmd := exec.Command("docker", "exec", "-i", w.id, "sh", "-c", `cat > "$1.new" && mv -f "$1.new" "$1"`, "sh", path)
	cmd.Stdin = bytes.NewReader(data)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("writing %s: %v: %s", path, err, out)
	}
	return nil
}

// Option changes how the server is set up.
type Option func(*Server)

// WithoutMultiline turns off IRCv3 draft/multiline.
func WithoutMultiline() Option { return func(s *Server) { s.noMultiline = true } }

// WithWelcome names the network and its web app in the welcome (MOTD).
func WithWelcome(network, webURL string) Option {
	return func(s *Server) { s.network, s.webURL = network, webURL }
}

// WithJWTAudience makes Ergo accept only tokens for this audience.
func WithJWTAudience(aud string) Option { return func(s *Server) { s.JWTAudience = aud } }

// ErrNoDocker means Docker isn't available, so tests should skip.
var ErrNoDocker = errors.New("docker isn't available")

// Start runs a fresh Ergo. Call Stop when done.
func Start(opts ...Option) (*Server, error) {
	if exec.Command("docker", "info").Run() != nil {
		return nil, ErrNoDocker
	}
	s := &Server{APIToken: randomHex(16), OperName: "guildserv", OperPassword: randomHex(16), JWTKey: randomHex(32)}
	for _, o := range opts {
		o(s)
	}
	conf, err := s.config()
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "ergotest")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	confPath := filepath.Join(dir, "ircd.yaml")
	if err := os.WriteFile(confPath, conf, 0o644); err != nil {
		return nil, err
	}

	if err := buildImage(); err != nil {
		return nil, err
	}
	// Started the way deploy/ergo starts it, with this test's config in
	// place of the committed one.
	out, err := docker("create", "-p", "127.0.0.1::6667", "-p", "127.0.0.1::8089", "-p", "127.0.0.1::8097",
		"-e", "GUILDNET_NETWORK="+s.network, "-e", "GUILDNET_WEB_URL="+s.webURL, image)
	if err != nil {
		return nil, err
	}
	s.id = strings.TrimSpace(out)
	if _, err := docker("cp", confPath, s.id+":/ircd-bin/guildnet.yaml"); err != nil {
		s.Stop()
		return nil, err
	}
	if _, err := docker("start", s.id); err != nil {
		s.Stop()
		return nil, err
	}
	if s.Addr, err = s.port("6667"); err != nil {
		s.Stop()
		return nil, err
	}
	api, err := s.port("8089")
	if err != nil {
		s.Stop()
		return nil, err
	}
	s.APIURL = "http://" + api
	ws, err := s.port("8097")
	if err != nil {
		s.Stop()
		return nil, err
	}
	s.WebSocketURL = "ws://" + ws
	if err := s.waitReady(); err != nil {
		logs, _ := docker("logs", s.id)
		s.Stop()
		return nil, fmt.Errorf("%w\n%s", err, logs)
	}
	return s, nil
}

// Stop removes the container.
func (s *Server) Stop() {
	if s.id != "" {
		docker("rm", "-f", "-v", s.id) // -v: and its anonymous volume
	}
}

// Register creates an account through the API.
func (s *Server) Register(account, password string) error {
	var resp struct {
		Success   bool   `json:"success"`
		ErrorCode string `json:"errorCode"`
	}
	if err := s.post("/v1/ns/saregister", map[string]string{"accountName": account, "passphrase": password}, &resp); err != nil {
		return err
	}
	if !resp.Success && resp.ErrorCode != "ACCOUNT_EXISTS" {
		return fmt.Errorf("registering %s: %s", account, resp.ErrorCode)
	}
	return nil
}

// SignsIn says whether account and password sign in over IRC (SASL PLAIN),
// as an IRC app would.
func (s *Server) SignsIn(account, password string) (bool, error) {
	c, err := net.DialTimeout("tcp", s.Addr, 5*time.Second)
	if err != nil {
		return false, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	creds := base64.StdEncoding.EncodeToString([]byte(account + "\x00" + account + "\x00" + password))
	fmt.Fprintf(c, "CAP REQ :sasl\r\nNICK signin%s\r\nUSER t 0 * :t\r\nAUTHENTICATE PLAIN\r\n", randomHex(3))
	lines := bufio.NewScanner(c)
	for lines.Scan() {
		f := strings.Fields(lines.Text())
		switch {
		case len(f) >= 2 && f[0] == "AUTHENTICATE" && f[1] == "+":
			fmt.Fprintf(c, "AUTHENTICATE %s\r\n", creds)
		case len(f) >= 2 && f[1] == "903":
			return true, nil
		case len(f) >= 2 && (f[1] == "904" || f[1] == "905" || f[1] == "906"):
			return false, nil
		}
	}
	return false, fmt.Errorf("no answer to signing in: %v", lines.Err())
}

func (s *Server) post(path string, body, out any) error {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", s.APIURL+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+s.APIToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

// config is GuildNet's Ergo config with the API, a test oper, and chat
// tokens turned on.
func (s *Server) config() ([]byte, error) {
	_, here, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(here), "..", "..", "deploy", "ergo", "ircd.yaml"))
	if err != nil {
		return nil, err
	}
	var conf map[string]any
	if err := yaml.Unmarshal(raw, &conf); err != nil {
		return nil, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(s.OperPassword), bcrypt.MinCost)
	if err != nil {
		return nil, err
	}
	conf["api"] = map[string]any{"enabled": true, "listener": ":8089", "bearer-tokens": []string{s.APIToken}}
	conf["opers"] = map[string]any{s.OperName: map[string]any{
		"class": "server-admin", "hidden": true, "password": string(hash),
	}}
	// As in production: tokens from the platform, for existing accounts
	// only, from pages on the platform's own addresses.
	token := map[string]any{"algorithm": "hmac", "key": s.JWTKey, "account-claims": []string{"account"}}
	if s.JWTAudience != "" {
		token["validate-aud"] = []string{s.JWTAudience}
	}
	accounts := conf["accounts"].(map[string]any)
	accounts["jwt-auth"] = map[string]any{"enabled": true, "autocreate": false, "tokens": []any{token}}
	if s.noMultiline {
		conf["limits"].(map[string]any)["multiline"] = map[string]any{"max-bytes": 0, "max-lines": 0}
	}
	server := conf["server"].(map[string]any)
	server["websockets"] = map[string]any{"allowed-origins": []string{"http://*.guildnet.test"}}
	return yaml.Marshal(conf)
}

func (s *Server) port(p string) (string, error) {
	out, err := docker("port", s.id, p+"/tcp")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(strings.Split(out, "\n")[0]), nil
}

func (s *Server) waitReady() error {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", s.Addr, time.Second); err == nil {
			c.Close()
			var status map[string]any
			if s.post("/v1/status", map[string]string{}, &status) == nil {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return errors.New("ergo didn't come up within 20s")
}

func docker(args ...string) (string, error) {
	cmd := exec.Command("docker", args...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker %s: %v: %s", args[0], err, errOut.String())
	}
	return out.String(), nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
