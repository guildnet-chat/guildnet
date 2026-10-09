// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
)

// A console or chat left open since before a deploy is an older GuildNet.
// Each deploy has a version, from what browsers get: the console's pages
// and files, and the chat app's build. Console pages carry it (the console
// reloads itself on the next click after a deploy: forms.js), and so does
// the chat app's page, which asks for the current one now and then and
// offers to reload.

// buildVersion is this deploy's version.
func buildVersion(chatDir string) string {
	h := sha256.New()
	for _, files := range []fs.FS{templateFiles, staticFiles} {
		fs.WalkDir(files, ".", func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, err := fs.ReadFile(files, path)
			if err != nil {
				return err
			}
			h.Write([]byte(path + "\x00"))
			h.Write(b)
			return nil
		})
	}
	if chatDir != "" {
		// Its build names every file after its contents, so its page changes
		// with any of them.
		if b, err := os.ReadFile(filepath.Join(chatDir, "index.html")); err == nil {
			h.Write([]byte("chat\x00"))
			h.Write(b)
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// chatVersion tells the chat app which version is current.
func (p *Platform) chatVersion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSONResponse(w, http.StatusOK, map[string]string{"version": p.version})
}
