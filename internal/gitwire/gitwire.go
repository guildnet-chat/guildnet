// SPDX-License-Identifier: AGPL-3.0-only

// Package gitwire speaks enough of git's smart HTTP protocol to list a
// repository's refs and push one branch to it: the runner's way of pushing
// what a desk made to any git host, holding a credential the desk never
// sees. A push is a compare-and-swap: the branch moves only if it's where
// the pusher says it is, so it can't clobber anyone's work. Only branches,
// never tags, and nothing is deleted.
package gitwire

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Credential is how the host knows us: a user name and a token, sent as
// HTTP basic authentication. None (its zero value) is for a public
// repository.
type Credential struct {
	User, Token string
}

// Service is what refs are listed for.
type Service string

const (
	Fetching Service = "git-upload-pack"
	Pushing  Service = "git-receive-pack"
)

var (
	// ErrDenied is the host refusing the credential, or refusing it this.
	ErrDenied = errors.New("the host refused the credential")
	// ErrNotFound is no repository there (or none the credential may see).
	ErrNotFound = errors.New("no repository there")
	// ErrNotGit is an answer that isn't git's.
	ErrNotGit = errors.New("not a git server's answer")
	// ErrMoved is a branch that isn't where the pusher said it was.
	ErrMoved = errors.New("the branch isn't where it was")
)

// RejectedError is the host refusing a push, with its reason and what it
// said along the way (from its hooks, say; it's the host's, not ours).
type RejectedError struct {
	Ref, Reason, Said string
}

func (e *RejectedError) Error() string {
	return fmt.Sprintf("the host refused %s: %s", e.Ref, e.Reason)
}

// Refs is what a repository has.
type Refs struct {
	// Refs are the refs' objects, by name.
	Refs map[string]string
	// Head is the ref HEAD names (the default branch), if the host said.
	Head string
	caps map[string]string
	zero string
}

// Limits on what's read from a host.
const (
	maxAdvert = 32 << 20
	maxResult = 1 << 20
	maxSaid   = 64 << 10
)

// checkRepo is repo's address, if it's one to use: https, with a host and
// no user name or password, query or fragment.
func checkRepo(repo string) (string, error) {
	u, err := url.Parse(repo)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Opaque != "" ||
		u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%q isn't an https:// address without a user name or password in it", repo)
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// do sends req as cred, never following a redirect (the credential would
// go along), and is the answer if it's git's (of content type want).
func do(c *http.Client, req *http.Request, cred Credential, want string) (*http.Response, error) {
	cc := *c
	cc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if cred != (Credential{}) {
		req.SetBasicAuth(cred.User, cred.Token)
	}
	req.Header.Set("User-Agent", "git/guildnet")
	res, err := cc.Do(req)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*http.Response, error) {
		res.Body.Close()
		return nil, err
	}
	switch {
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		return fail(ErrDenied)
	case res.StatusCode == http.StatusNotFound:
		return fail(ErrNotFound)
	case res.StatusCode >= 300 && res.StatusCode < 400:
		return fail(fmt.Errorf("the repository has moved to %s; use that address", res.Header.Get("Location")))
	case res.StatusCode != http.StatusOK:
		return fail(fmt.Errorf("the host answered %s", res.Status))
	case res.Header.Get("Content-Type") != want:
		return fail(ErrNotGit)
	}
	return res, nil
}

// List is what repo has, listed for fetching or pushing: listing for
// pushing is how to tell whether a credential may push.
func List(ctx context.Context, c *http.Client, repo string, cred Credential, s Service) (*Refs, error) {
	repo, err := checkRepo(repo)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, repo+"/info/refs?service="+string(s), nil)
	if err != nil {
		return nil, err
	}
	res, err := do(c, req, cred, "application/x-"+string(s)+"-advertisement")
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	return readAdvert(io.LimitReader(res.Body, maxAdvert), s)
}

// readAdvert reads the refs a host lists: "# service=…", a flush, then a
// line per ref, the first with the host's capabilities after a NUL.
func readAdvert(r io.Reader, s Service) (*Refs, error) {
	p := newPktReader(r)
	line, err := p.next()
	if err != nil {
		return nil, err
	}
	if line != nil && string(bytes.TrimSuffix(line, []byte("\n"))) == "# service="+string(s) {
		if line, err = p.next(); err != nil || line != nil {
			return nil, ErrNotGit
		}
		if line, err = p.next(); err != nil {
			return nil, err
		}
	}
	refs := &Refs{Refs: map[string]string{}, caps: map[string]string{}}
	for first := true; line != nil; first = false {
		text := string(bytes.TrimSuffix(line, []byte("\n")))
		if first {
			text0, caps, _ := strings.Cut(text, "\x00")
			text = text0
			for _, c := range strings.Fields(caps) {
				k, v, _ := strings.Cut(c, "=")
				if k == "symref" && strings.HasPrefix(v, "HEAD:") {
					refs.Head = strings.TrimPrefix(v, "HEAD:")
				}
				refs.caps[k] = v
			}
		}
		oid, name, ok := strings.Cut(text, " ")
		if !ok || !isHex(oid) {
			return nil, ErrNotGit
		}
		if refs.zero == "" {
			refs.zero = strings.Repeat("0", len(oid))
		}
		// An empty repository lists only its capabilities; tags' peeled
		// objects aren't refs.
		if name != "capabilities^{}" && !strings.HasSuffix(name, "^{}") {
			refs.Refs[name] = oid
		}
		if line, err = p.next(); err != nil {
			return nil, err
		}
	}
	if refs.zero == "" {
		return nil, ErrNotGit
	}
	return refs, nil
}

// Update is a branch to move: from Old ("" for a branch that isn't there
// yet) to New.
type Update struct {
	Ref, Old, New string
}

// Result is how a push went.
type Result struct {
	// Said is what the host said along the way: its words, not ours.
	Said string
}

// Push moves a branch on repo as cred, sending pack (the objects New has
// that the host doesn't), only if the branch is at Old.
func Push(ctx context.Context, c *http.Client, repo string, cred Credential, u Update, pack io.Reader) (Result, error) {
	if !BranchRef(u.Ref) {
		return Result{}, fmt.Errorf("%q isn't a branch to push to", u.Ref)
	}
	if !isHex(u.New) || strings.Trim(u.New, "0") == "" || u.Old != "" && !isHex(u.Old) {
		return Result{}, fmt.Errorf("%q to %q isn't a move from one commit to another", u.Old, u.New)
	}
	refs, err := List(ctx, c, repo, cred, Pushing)
	if err != nil {
		return Result{}, err
	}
	repo, _ = checkRepo(repo)
	old := u.Old
	if old == "" {
		old = refs.zero
	}
	if len(old) != len(refs.zero) || len(u.New) != len(refs.zero) {
		return Result{}, fmt.Errorf("the repository's object names are %d characters long", len(refs.zero))
	}
	if now := refs.Refs[u.Ref]; now == "" && old != refs.zero || now != "" && now != old {
		return Result{}, fmt.Errorf("%w: %s is at %q", ErrMoved, u.Ref, now)
	}
	if _, ok := refs.caps["report-status"]; !ok {
		return Result{}, errors.New("the host doesn't report how a push went")
	}
	_, sideband := refs.caps["side-band-64k"]
	caps := []string{"report-status", "agent=guildnet"}
	for _, c := range []string{"side-band-64k", "quiet"} {
		if _, ok := refs.caps[c]; ok {
			caps = append(caps, c)
		}
	}
	if f, ok := refs.caps["object-format"]; ok {
		caps = append(caps, "object-format="+f)
	}
	var head bytes.Buffer
	writePkt(&head, old+" "+u.New+" "+u.Ref+"\x00"+strings.Join(caps, " ")+"\n")
	head.WriteString("0000")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, repo+"/git-receive-pack", io.MultiReader(&head, pack))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	req.Header.Set("Accept", "application/x-git-receive-pack-result")
	res, err := do(c, req, cred, "application/x-git-receive-pack-result")
	if err != nil {
		return Result{}, err
	}
	defer res.Body.Close()
	body := io.LimitReader(res.Body, maxResult)

	var report, said bytes.Buffer
	if sideband {
		p := newPktReader(body)
		for {
			line, err := p.next()
			if err != nil {
				return Result{}, err
			}
			if line == nil {
				break
			}
			switch line[0] {
			case 1:
				report.Write(line[1:])
			case 2, 3:
				if said.Len() < maxSaid {
					said.Write(line[1:])
				}
				if line[0] == 3 {
					return Result{}, &RejectedError{Ref: u.Ref, Reason: "the host stopped", Said: tidy(said.String())}
				}
			}
		}
	} else if _, err := io.Copy(&report, body); err != nil {
		return Result{}, err
	}
	out := Result{Said: tidy(said.String())}
	return out, readReport(&report, u.Ref, out.Said)
}

// readReport reads how the host says the push went: "unpack ok", then
// "ok <ref>" or "ng <ref> <reason>".
func readReport(r io.Reader, ref, said string) error {
	p := newPktReader(r)
	line, err := p.next()
	if err != nil {
		return err
	}
	if unpack := strings.TrimSuffix(string(line), "\n"); unpack != "unpack ok" {
		return &RejectedError{Ref: ref, Reason: strings.TrimPrefix(unpack, "unpack "), Said: said}
	}
	for {
		line, err := p.next()
		if err != nil {
			return err
		}
		if line == nil {
			return errors.New("the host didn't say how the push went")
		}
		status, rest, _ := strings.Cut(strings.TrimSuffix(string(line), "\n"), " ")
		name, reason, _ := strings.Cut(rest, " ")
		switch {
		case name != ref:
		case status == "ok":
			return nil
		case status == "ng":
			return &RejectedError{Ref: ref, Reason: reason, Said: said}
		}
	}
}

// tidy is what a host said, as lines: progress's carriage returns ending
// lines too.
func tidy(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\r", "\n"))
}

// BranchRef says whether ref is a branch's full name, by git's rules for
// names (git check-ref-format).
func BranchRef(ref string) bool {
	name, ok := strings.CutPrefix(ref, "refs/heads/")
	if !ok || name == "" || name == "@" || strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") ||
		strings.Contains(name, "..") || strings.Contains(name, "@{") || strings.Contains(name, "//") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(" ~^:?*[\\", r) {
			return false
		}
	}
	return true
}

func isHex(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

// pktReader reads git's pkt-lines: four hex digits of length (the four
// included), then that; "0000" is a flush, read as nil.
type pktReader struct {
	r *bufio.Reader
}

func newPktReader(r io.Reader) *pktReader { return &pktReader{r: bufio.NewReader(r)} }

func (p *pktReader) next() ([]byte, error) {
	var size [4]byte
	if _, err := io.ReadFull(p.r, size[:]); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotGit, err)
	}
	n, err := strconv.ParseUint(string(size[:]), 16, 16)
	switch {
	case err != nil:
		return nil, ErrNotGit
	case n < 4:
		// A flush, or (protocol v2's) a delimiter or the end.
		return nil, nil
	case n == 4 || n > 65520:
		return nil, ErrNotGit
	}
	line := make([]byte, n-4)
	if _, err := io.ReadFull(p.r, line); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotGit, err)
	}
	return line, nil
}

func writePkt(w *bytes.Buffer, s string) {
	fmt.Fprintf(w, "%04x%s", len(s)+4, s)
}
