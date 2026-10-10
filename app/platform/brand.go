// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/guildnet-chat/guildnet/mail"
	"github.com/guildnet-chat/guildnet/platform"
)

// brandFiles are the files a brand's directory may have (platform.Brand's
// Files): anything else there is a mistake.
var brandFiles = map[string]bool{"logo.svg": true, "brand.css": true, "favicon.ico": true, "apple-touch-icon.png": true}

// brandFrom is what this GuildNet goes by, from its settings; GuildNet's
// own where they're not set.
func brandFrom(getenv func(string) string) (platform.Brand, error) {
	b := platform.Brand{
		Name: getenv("GUILDNET_BRAND_NAME"), Tagline: getenv("GUILDNET_BRAND_TAGLINE"),
		Support: getenv("GUILDNET_BRAND_SUPPORT"), Hello: getenv("GUILDNET_BRAND_HELLO"),
		Privacy: getenv("GUILDNET_BRAND_PRIVACY"), Security: getenv("GUILDNET_BRAND_SECURITY"),
	}
	if len(b.Name) > 40 {
		return b, errors.New("GUILDNET_BRAND_NAME: a name of at most 40 characters")
	}
	for k, v := range map[string]string{"GUILDNET_BRAND_SUPPORT": b.Support, "GUILDNET_BRAND_HELLO": b.Hello,
		"GUILDNET_BRAND_PRIVACY": b.Privacy, "GUILDNET_BRAND_SECURITY": b.Security} {
		if v != "" && !mail.Plausible(v) {
			return b, fmt.Errorf("%s: an email address", k)
		}
	}
	if dir := getenv("GUILDNET_BRAND_DIR"); dir != "" {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return b, fmt.Errorf("GUILDNET_BRAND_DIR: %v", err)
		}
		for _, e := range entries {
			// A Kubernetes ConfigMap's mount has its own beside the files
			// (..data, ..<time>), which they're links into.
			if strings.HasPrefix(e.Name(), "..") {
				continue
			}
			if !brandFiles[e.Name()] {
				return b, fmt.Errorf("GUILDNET_BRAND_DIR: %s isn't one of logo.svg, brand.css, favicon.ico or apple-touch-icon.png", e.Name())
			}
		}
		b.Files = os.DirFS(dir)
	}
	return b, nil
}
