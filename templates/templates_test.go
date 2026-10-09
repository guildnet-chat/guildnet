package templates_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/guildnet-chat/guildnet/templates"
)

func TestEveryTemplateDirectoryIsOfferedAndEveryOfferHasOne(t *testing.T) {
	dirs, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	onDisk := map[string]bool{}
	for _, d := range dirs {
		if d.IsDir() {
			onDisk[d.Name()] = true
			if !templates.Known(d.Name()) {
				t.Errorf("templates/%s isn't in templates.All, so nobody can pick it", d.Name())
			}
		}
	}
	for _, tpl := range templates.All {
		if !onDisk[tpl.Name] {
			t.Errorf("templates.All offers %s, but there's no templates/%s", tpl.Name, tpl.Name)
		}
		if _, err := os.Stat(filepath.Join(tpl.Name, "guildnet.yaml")); err != nil {
			t.Errorf("template %s: %v", tpl.Name, err)
		}
		personas, _ := filepath.Glob(filepath.Join(tpl.Name, "personas", "*.yaml"))
		var nicks []string
		for _, f := range personas {
			nicks = append(nicks, strings.TrimSuffix(filepath.Base(f), ".yaml"))
		}
		slices.Sort(nicks)
		if listed := slices.Sorted(slices.Values(tpl.Agents)); !slices.Equal(nicks, listed) {
			t.Errorf("template %s lists agents %v, but has %v", tpl.Name, listed, nicks)
		}
		if tpl.Title == "" || tpl.About == "" {
			t.Errorf("template %s needs a title and a line about it", tpl.Name)
		}
	}
	if templates.Known("../personas") || templates.Known("") {
		t.Error("only listed templates are known")
	}
}
