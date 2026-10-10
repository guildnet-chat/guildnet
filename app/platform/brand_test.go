// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// A self-hosted GuildNet's brand comes from its settings: its name, the
// addresses people write to, its emails' tagline, and a directory of its
// own logo, colours and icons. With none set, it's GuildNet.
func TestTheBrandComesFromTheSettings(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "logo.svg"), []byte("<svg/>"), 0o644); err != nil {
		t.Fatal(err)
	}

	b, err := brandFrom(getenvFrom(map[string]string{
		"GUILDNET_BRAND_NAME": "Acme Chat", "GUILDNET_BRAND_TAGLINE": "chat for Acme's crews",
		"GUILDNET_BRAND_SUPPORT": "help@acme.test", "GUILDNET_BRAND_HELLO": "hi@acme.test",
		"GUILDNET_BRAND_PRIVACY": "privacy@acme.test", "GUILDNET_BRAND_SECURITY": "security@acme.test",
		"GUILDNET_BRAND_DIR": dir,
	}))

	if err != nil {
		t.Fatal(err)
	}
	if b.Name != "Acme Chat" || b.Tagline != "chat for Acme's crews" || b.Support != "help@acme.test" || b.Hello != "hi@acme.test" ||
		b.Privacy != "privacy@acme.test" || b.Security != "security@acme.test" {
		t.Errorf("brand: %+v", b)
	}
	if b.Files == nil {
		t.Fatal("no files")
	}
	if _, err := fs.Stat(b.Files, "logo.svg"); err != nil {
		t.Errorf("the logo: %v", err)
	}

	if b, err := brandFrom(getenvFrom(nil)); err != nil || b.Name != "" || b.Files != nil {
		t.Errorf("nothing set: %+v, %v; want GuildNet's", b, err)
	}
}

// A brand whose settings can't work is refused at start, saying why.
func TestABrandThatCantWorkIsRefused(t *testing.T) {
	for name, vars := range map[string]map[string]string{
		"no such directory":       {"GUILDNET_BRAND_DIR": filepath.Join(t.TempDir(), "nope")},
		"an address that isn't":   {"GUILDNET_BRAND_SUPPORT": "support"},
		"a file it doesn't know":  {"GUILDNET_BRAND_DIR": dirWith(t, "logo.png")},
		"a name too long for one": {"GUILDNET_BRAND_NAME": "An Exceedingly Long Name For A Chat Product, Really"},
	} {
		if _, err := brandFrom(getenvFrom(vars)); err == nil {
			t.Errorf("%s: taken", name)
		}
	}
}

func dirWith(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// On Kubernetes, the brand's files are a ConfigMap, mounted with folders and
// links of Kubernetes's own beside them (..data, ..<time>): those aren't
// mistakes, and the files are read through them. Anything else still is.
func TestABrandFromAConfigMapIsTaken(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"..data", "..2026_10_09_11_48_02.999164871"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "..data", "logo.svg"), []byte("<svg/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "logo.svg"), []byte("<svg/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"GUILDNET_BRAND_DIR": dir}
	b, err := brandFrom(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("a ConfigMap's brand: %v", err)
	}
	if logo, err := fs.ReadFile(b.Files, "logo.svg"); err != nil || string(logo) != "<svg/>" {
		t.Errorf("the logo: %q, %v", logo, err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".logo.svg.swp"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := brandFrom(func(k string) string { return env[k] }); err == nil {
		t.Error("a stray file beside a ConfigMap's was taken")
	}
}
