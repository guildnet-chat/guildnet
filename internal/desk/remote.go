// SPDX-License-Identifier: AGPL-3.0-only

package desk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/guildnet-chat/guildnet/internal/sandbox"
)

// Remote is the Sandbox in sandboxd, reached over HTTP with the network's
// sandbox token (signed by the platform, good for a few minutes).
type Remote struct {
	url    string
	tokens TokenSource
	http   *http.Client
}

// TokenSource gives a token for sandboxd: the one it has while it's good,
// or (fresh) a new one.
type TokenSource func(ctx context.Context, fresh bool) (string, error)

// NewRemote is sandboxd at url, with one token.
func NewRemote(url, token string) *Remote {
	return NewRemoteFrom(url, func(context.Context, bool) (string, error) { return token, nil })
}

// NewRemoteFrom is sandboxd at url, with tokens from tokens (the broker's).
func NewRemoteFrom(url string, tokens TokenSource) *Remote {
	// A command may run for 10 minutes; a desk may take a while to wake.
	return &Remote{url: strings.TrimRight(url, "/"), tokens: tokens, http: &http.Client{Timeout: maxTimeout + 2*time.Minute}}
}

// Through has r reach sandboxd through rt (one trusting GuildNet's own CA,
// for sandboxd over TLS), and is r.
func (r *Remote) Through(rt http.RoundTripper) *Remote {
	r.http.Transport = rt
	return r
}

// call sends body to the desk's op and decodes the answer into out (if
// any). sandboxd's 404 and 413 are ErrNoFile and ErrTooBig. A token it
// refuses is replaced with a fresh one, and the call made again, once.
func (r *Remote) call(ctx context.Context, method, nick, op string, body, out any) error {
	var raw []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		raw = b
	}
	u := r.url + "/v1/desks"
	if nick != "" {
		u += "/" + url.PathEscape(strings.ToLower(nick))
	}
	if op != "" {
		u += "/" + op
	}
	return r.send(ctx, method, u, raw, out)
}

// get is a GET of sandboxd's path, decoded into out.
func (r *Remote) get(ctx context.Context, path string, out any) error {
	return r.send(ctx, "GET", r.url+path, nil, out)
}

func (r *Remote) send(ctx context.Context, method, u string, raw []byte, out any) error {
	var resp *http.Response
	for fresh := false; ; fresh = true {
		token, err := r.tokens(ctx, fresh)
		if err != nil {
			return fmt.Errorf("a token for the sandbox: %w", err)
		}
		var in io.Reader
		if raw != nil {
			in = bytes.NewReader(raw)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, in)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err = r.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return err
			}
			return fmt.Errorf("%w: %v", ErrUnreachable, err)
		}
		if resp.StatusCode != http.StatusUnauthorized || fresh {
			break
		}
		resp.Body.Close()
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&e)
		switch resp.StatusCode {
		case http.StatusNotFound:
			return fmt.Errorf("%w (%s)", ErrNoFile, e.Error)
		case http.StatusRequestEntityTooLarge:
			return fmt.Errorf("%w (%s)", ErrTooBig, e.Error)
		case http.StatusTooManyRequests:
			return ErrFull
		}
		if e.Error == "" {
			e.Error = resp.Status
		}
		return fmt.Errorf("the sandbox: %s", e.Error)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (r *Remote) Open(ctx context.Context, nick string) error {
	return r.call(ctx, "POST", nick, "open", nil, nil)
}

func (r *Remote) Run(ctx context.Context, nick string, c Command) (Output, error) {
	var out struct {
		Output   string `json:"output"`
		Exit     int    `json:"exit"`
		TimedOut bool   `json:"timed_out"`
	}
	err := r.call(ctx, "POST", nick, "run", map[string]any{
		"args": c.Args, "dir": c.Dir, "stdin": c.Stdin, "timeout_ms": c.Timeout.Milliseconds(), "pass": c.Pass,
	}, &out)
	return Output{Text: out.Output, Exit: out.Exit, TimedOut: out.TimedOut}, err
}

type file struct {
	Path    string `json:"path"`
	Max     int64  `json:"max,omitempty"`
	Content []byte `json:"content,omitempty"`
}

func (r *Remote) Read(ctx context.Context, nick, path string, max int64) ([]byte, error) {
	var out file
	if err := r.call(ctx, "POST", nick, "read", file{Path: path, Max: max}, &out); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return out.Content, nil
}

func (r *Remote) Write(ctx context.Context, nick, path string, data []byte) error {
	if err := r.call(ctx, "POST", nick, "write", file{Path: path, Content: data}, nil); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func (r *Remote) Remove(ctx context.Context, nick string) error {
	if nick == "" {
		return fmt.Errorf("which desk? no nick")
	}
	return r.call(ctx, "DELETE", nick, "", nil, nil)
}

// RemoveAll deletes every desk of the token's network, and their files:
// the network has been deleted.
func (r *Remote) RemoveAll(ctx context.Context) error {
	return r.call(ctx, "DELETE", "", "", nil, nil)
}

// Storage is how much the token's network's desks' files take, and its
// limit (the platform's token only).
func (r *Remote) Storage(ctx context.Context) (sandbox.Storage, error) {
	var out sandbox.Storage
	err := r.get(ctx, "/v1/storage", &out)
	return out, err
}
