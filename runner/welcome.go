// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"unicode"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/org"
	"github.com/guildnet-chat/guildnet/monitor"
)

// Ergo's welcome (MOTD) says what the company is for: its description and
// goals, where the template Ergo starts with has a {{COMPANY}} line. Ergo
// fills in the rest (base.motd) in the directory it shares with the runner;
// GuildServ writes the welcome Ergo shows (guildnet.motd) and has Ergo read
// it again (REHASH), on connecting and whenever the company changes.

const (
	welcomeBase     = "base.motd"
	welcomeFile     = "guildnet.motd"
	welcomeWidth    = 64
	welcomeMaxGoal  = 5
	welcomeMaxBytes = 64 << 10
)

// welcomeFiles is where the welcome lives, if anywhere.
func (r *Runner) welcomeFiles() config.Files {
	switch {
	case r.cfg.WelcomeFiles != nil:
		return r.cfg.WelcomeFiles
	case r.cfg.WelcomeDir != "":
		return sharedDir(r.cfg.WelcomeDir)
	}
	return nil
}

// sharedDir is a directory shared with another container (Ergo, which faces
// the internet), so nothing in it is trusted: links and names that lead out
// of it are refused (the runner's own files, its secrets, are out there),
// and only small, ordinary files are read, so a pipe or a huge file can't
// hold the runner up.
type sharedDir string

func (d sharedDir) ReadFile(name string) ([]byte, error) {
	root, err := os.OpenRoot(string(d))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	// Not waiting on a pipe to open: it's refused below.
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s isn't an ordinary file", name)
	}
	b, err := io.ReadAll(io.LimitReader(f, welcomeMaxBytes+1))
	if err == nil && len(b) > welcomeMaxBytes {
		err = fmt.Errorf("%s is over %d bytes", name, welcomeMaxBytes)
	}
	return b, err
}

// WriteFile writes a new file aside, then moves it into place, so Ergo never
// reads half of it; whatever was left where it's written (a link, say) is
// removed first, never written through.
func (d sharedDir) WriteFile(name string, data []byte) error {
	root, err := os.OpenRoot(string(d))
	if err != nil {
		return err
	}
	defer root.Close()
	aside := name + ".new"
	if err := root.Remove(aside); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	f, err := root.OpenFile(aside, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return root.Rename(aside, name)
}

// updateWelcome writes the welcome in the background (a slow or unreadable
// directory mustn't hold up GuildServ), and has Ergo reread it if it changed,
// or anyway when reread is set: on becoming an operator, since a change
// made before then was written but couldn't be reread.
func (r *Runner) updateWelcome(reread bool) {
	files := r.welcomeFiles()
	if files == nil || r.guildConn == nil {
		return
	}
	go func() {
		r.welcomeMu.Lock()
		defer r.welcomeMu.Unlock()
		changed, err := r.writeWelcome(files)
		if err != nil {
			monitor.Problem(r.logger, "welcome: %v", err)
			return
		}
		if changed || reread {
			r.guildConn.Send("REHASH")
		}
	}()
}

// writeWelcome fills the company into Ergo's welcome, saying whether that
// changed it.
func (r *Runner) writeWelcome(files config.Files) (bool, error) {
	base, err := files.ReadFile(welcomeBase)
	if err != nil {
		return false, err
	}
	var out []string
	for _, line := range strings.Split(string(base), "\n") {
		if strings.TrimSpace(line) == "{{COMPANY}}" {
			out = append(out, r.welcomeCompany()...)
			continue
		}
		out = append(out, line)
	}
	text := []byte(strings.Join(out, "\n"))
	if old, err := files.ReadFile(welcomeFile); err == nil && bytes.Equal(old, text) {
		return false, nil
	}
	return true, files.WriteFile(welcomeFile, text)
}

// welcomeCompany is the welcome's lines about the company: its description
// and the company's own goals (not teams'), then a blank line; none if
// there's nothing to say.
func (r *Runner) welcomeCompany() []string {
	ctx := context.Background()
	description, _ := r.store.Company(ctx)
	goals, _ := r.store.Goals(ctx)
	var company []org.Goal
	for _, g := range goals {
		if g.Team == "" && len(company) < welcomeMaxGoal {
			company = append(company, g)
		}
	}
	if description == "" && len(company) == 0 {
		return nil
	}
	name := r.cfg.Company
	if name == "" {
		name = "the company"
	}
	lines := []string{"$bAbout " + motdText(name) + "$b"}
	lines = append(lines, wrap(motdText(description), welcomeWidth)...)
	if len(company) > 0 {
		lines = append(lines, "", "$bWhat we're aiming for$b")
		for _, g := range company {
			for i, l := range wrap(motdText(describeGoal(g)), welcomeWidth-2) {
				if i == 0 {
					lines = append(lines, "- "+l)
				} else {
					lines = append(lines, "  "+l)
				}
			}
		}
	}
	return append(lines, "")
}

// motdText is text as the welcome shows it: one line, without control
// characters, and with $ (Ergo's formatting codes) meaning $.
func motdText(s string) string {
	s = strings.Map(func(c rune) rune {
		if unicode.IsControl(c) && !unicode.IsSpace(c) {
			return -1
		}
		return c
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	return strings.ReplaceAll(s, "$", "$$")
}
