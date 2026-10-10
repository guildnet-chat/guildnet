// SPDX-License-Identifier: AGPL-3.0-only

package platformtest

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TOTP is the code an authenticator app shows for secret at t (RFC 6238:
// SHA-1, 30 seconds, 6 digits), worked out here, not by the platform.
func TOTP(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(strings.ReplaceAll(secret, " ", "")))
	if err != nil {
		t.Fatalf("the secret %q isn't base32: %v", secret, err)
	}
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(at.Unix()/30))
	mac := hmac.New(sha1.New, key)
	mac.Write(counter[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	n := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", n%1_000_000)
}

var (
	SecretField  = regexp.MustCompile(`name="secret" value="([A-Z2-7]+)"`)
	RecoveryCode = regexp.MustCompile(`class="recovery-code">([a-z2-7]{5}-[a-z2-7]{5})<`)
)

// SetUpTwoStep turns on two-step sign-in with an authenticator app for
// whoever's signed in, and gives back the app's secret and the recovery
// codes shown.
func (b *Browser) SetUpTwoStep() (string, []string) {
	b.T.Helper()
	page := b.Get("/account/two-step")
	m := SecretField.FindStringSubmatch(page.Body)
	if page.Status != http.StatusOK || m == nil {
		b.T.Fatalf("the setup page: %d, no secret:\n%s", page.Status, page.Text())
	}
	done := b.Post("/account/two-step", url.Values{"secret": {m[1]}, "code": {TOTP(b.T, m[1], b.W.Clock.Now())}})
	var codes []string
	for _, c := range RecoveryCode.FindAllStringSubmatch(done.Body, -1) {
		codes = append(codes, c[1])
	}
	if done.Status != http.StatusOK || len(codes) != 10 {
		b.T.Fatalf("setting it up: %d, %d recovery codes:\n%s", done.Status, len(codes), done.Text())
	}
	return m[1], codes
}

// SecondStep answers the sign-in's second step.
func (b *Browser) SecondStep(code string) Response {
	b.T.Helper()
	b.Get("/signin/two-step")
	return b.Post("/signin/two-step", url.Values{"code": {code}})
}

// PasswordOnly signs in with account's password, and checks that's
// stopped at the second step.
func (b *Browser) PasswordOnly(account string) {
	b.T.Helper()
	network := strings.TrimSuffix(b.Host, "."+BaseDomain)
	r := b.SignIn(account, Passwords[network][account])
	if r.Status != http.StatusSeeOther || r.Header.Get("Location") != "/signin/two-step" {
		b.T.Fatalf("after %s's password: %d to %q, want the second step", account, r.Status, r.Header.Get("Location"))
	}
}

// RequireTwoStep has the owner and org-wide admins sign in to consoles in
// two steps.
func RequireTwoStep(o *Setup) { o.RequireTwoStep = true }

// DatabaseHolds says whether the platform's two-step table holds s as it
// is, in any column (with or without a recovery code's dash).
func (w *World) DatabaseHolds(t *testing.T, s string) bool {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), w.DBURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var holds bool
	for _, form := range []string{s, strings.ReplaceAll(s, "-", "")} {
		var found bool
		err := conn.QueryRow(context.Background(), `select exists (select from two_step t
			where t::text ilike '%' || $1 || '%' or position(convert_to($1, 'UTF8') in coalesce(t.secret, ''::bytea)) > 0)`, form).Scan(&found)
		if err != nil {
			t.Fatal(err)
		}
		holds = holds || found
	}
	return holds
}

// SetUpHomeTwoStep sets up the authenticator app for whoever's signed in on
// the home page: its secret, and the recovery codes shown.
func (b *Browser) SetUpHomeTwoStep() (string, []string) {
	b.T.Helper()
	page := b.Get("/account/two-step")
	m := SecretField.FindStringSubmatch(page.Body)
	if page.Status != http.StatusOK || m == nil {
		b.T.Fatalf("the setup page: %d, no secret:\n%s", page.Status, page.Text())
	}
	done := b.Post("/account/two-step", url.Values{"secret": {m[1]}, "code": {TOTP(b.T, m[1], b.W.Clock.Now())}})
	var codes []string
	for _, c := range RecoveryCode.FindAllStringSubmatch(done.Body, -1) {
		codes = append(codes, c[1])
	}
	if done.Status != http.StatusOK || len(codes) != 10 {
		b.T.Fatalf("setting it up: %d, %d recovery codes:\n%s", done.Status, len(codes), done.Text())
	}
	return m[1], codes
}

// HomeSecondStep gives the home page's second step its code.
func (b *Browser) HomeSecondStep(code string) Response {
	b.T.Helper()
	b.Get("/signin/two-step")
	return b.Post("/signin/two-step", url.Values{"code": {code}})
}
