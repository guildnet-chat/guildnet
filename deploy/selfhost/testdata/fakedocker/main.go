// SPDX-License-Identifier: AGPL-3.0-only

// fakedocker stands in for docker when the install's scripts are tested on
// the machine's own shell. It's a program of the machine's own kind (an
// .exe on Windows), as docker is, so the shell passes it arguments as it
// would pass docker them: Git Bash, say, rewrites what looks like a path
// for a Windows program, and a test sees that here.
//
// It logs each call, one line of arguments, to $FAKEDOCKER_LOG, and
// answers: `info` with $FAKEDOCKER_RUNTIMES, sandboxd's `keygen` with a key
// pair and its `tls-request` with a request, a database dump with the line
// "dump of <db>", the files service's copy with a tar of Ergo's files, and
// anything reading from stdin by
// taking it. As Compose does, `compose run` before the stack's images are
// there (fetched or built by `compose up`, `pull` or `build`) gets them
// first, printing its progress to stdout.
package main

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	args := strings.Join(os.Args[1:], " ")
	log := os.Getenv("FAKEDOCKER_LOG")
	built := filepath.Join(filepath.Dir(log), "built")
	if log != "" {
		f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Fprintln(f, "docker "+args)
		f.Close()
	}
	switch {
	case strings.HasPrefix(args, "compose build"), strings.HasPrefix(args, "compose pull"), strings.HasPrefix(args, "compose up"):
		os.WriteFile(built, nil, 0o600)
		return
	case strings.HasPrefix(args, "compose run"):
		if _, err := os.Stat(built); err != nil {
			fmt.Print("#1 [internal] load local bake definitions\n#1 reading from stdin 553B done\n")
			os.WriteFile(built, nil, 0o600)
		}
	}
	switch {
	case strings.HasPrefix(args, "info"):
		fmt.Println(os.Getenv("FAKEDOCKER_RUNTIMES"))
	case strings.Contains(args, "keygen"):
		fmt.Print("GUILDNET_SANDBOX_KEY=private-key-text\nGUILDNET_SANDBOX_PUBLIC_KEY=public-key-text\n")
	case strings.Contains(args, "tls-request"):
		fmt.Print("-----BEGIN CERTIFICATE REQUEST-----\nTUlJ\n-----END CERTIFICATE REQUEST-----\n")
	case strings.Contains(args, "pg_dump"):
		fmt.Println("dump of " + os.Args[len(os.Args)-1])
	case strings.Contains(args, " -cf - ") && os.Getenv("FAKEDOCKER_BROKEN") == "files":
		fmt.Print("not a tar")
	case strings.Contains(args, " -cf - "):
		files := tar.NewWriter(os.Stdout)
		body := "ergo's accounts"
		files.WriteHeader(&tar.Header{Name: "ergo/ircd.db", Mode: 0o600, Size: int64(len(body))})
		io.WriteString(files, body)
		files.Close()
	case strings.Contains(args, " -xf - "), strings.Contains(args, "pg_restore"):
		io.Copy(io.Discard, os.Stdin)
	}
}
