// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"io/fs"
	"strings"
)

// Brand is what a GuildNet goes by: its name, the addresses people write
// to, what its emails say at the foot, and how it looks. A self-hosted one
// may go by its own. Its zero value is GuildNet, at GuildNet's addresses on
// its own domain (support@, hello@, privacy@, security@), looking like
// GuildNet.
type Brand struct {
	Name                              string
	Support, Hello, Privacy, Security string
	// Tagline follows the name at the foot of every email.
	Tagline string
	// Files are laid over the console's static files: logo.svg (in place of
	// GuildNet's crest), brand.css (after the console's styles, to change
	// its colours), favicon.ico and apple-touch-icon.png (also the emails'
	// logo). Any may be left out.
	Files fs.FS
}

// ourName stands for the brand's name in what GuildNet writes itself (its
// emails), and becomes it as they're sent. What people write can't contain
// it, so their words are never changed.
const ourName = "\x00name\x00"

// brand is what this GuildNet goes by.
func (p *Platform) brand() Brand {
	b := p.opts.Brand
	if b.Name == "" {
		b.Name = "GuildNet"
	}
	if b.Tagline == "" {
		b.Tagline = "an AI team you run like a real one"
	}
	for local, address := range map[string]*string{"support": &b.Support, "hello": &b.Hello, "privacy": &b.Privacy, "security": &b.Security} {
		if *address == "" {
			*address = local + "@" + p.opts.BaseDomain
		}
	}
	return b
}

// inOurName has what GuildNet wrote say the brand's name.
func (p *Platform) inOurName(s string) string {
	return strings.ReplaceAll(s, ourName, p.brand().Name)
}

// brandFile is the address of the brand's own static file name, or "" if
// it has none.
func (p *Platform) brandFile(name string) string {
	if p.opts.Brand.Files == nil {
		return ""
	}
	if _, err := fs.Stat(p.opts.Brand.Files, name); err != nil {
		return ""
	}
	u, _ := p.static(name)
	return u
}
