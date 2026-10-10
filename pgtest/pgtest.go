// SPDX-License-Identifier: AGPL-3.0-only

// Package pgtest runs a throwaway Postgres in Docker for tests. One container
// serves a whole test binary; each test gets its own fresh database.
package pgtest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Image matches the major version we run in Cloud SQL.
const Image = "postgres:18-alpine"

// ErrNoDocker means Docker isn't available, so tests should skip.
var ErrNoDocker = errors.New("docker isn't available")

// Server is a running Postgres container.
type Server struct {
	id       string
	addr     string
	password string
	mu       sync.Mutex
	n        int
}

// Start runs Postgres. Call Stop when done.
func Start() (*Server, error) {
	if exec.Command("docker", "info").Run() != nil {
		return nil, ErrNoDocker
	}
	s := &Server{password: randomHex(12)}
	out, err := docker("run", "-d", "-p", "127.0.0.1::5432", "-e", "POSTGRES_PASSWORD="+s.password, Image)
	if err != nil {
		return nil, err
	}
	s.id = strings.TrimSpace(out)
	port, err := docker("port", s.id, "5432/tcp")
	if err != nil {
		s.Stop()
		return nil, err
	}
	s.addr = strings.TrimSpace(strings.Split(port, "\n")[0])
	if err := s.waitReady(); err != nil {
		s.Stop()
		return nil, err
	}
	return s, nil
}

// Container is the Docker container's ID, for tests that attach it to
// other Docker networks.
func (s *Server) Container() string { return s.id }

// Stop removes the container.
func (s *Server) Stop() {
	if s.id != "" {
		docker("rm", "-f", "-v", s.id) // -v: and its anonymous volume
	}
}

// NewDatabase creates an empty database and returns its connection URL.
func (s *Server) NewDatabase() (string, error) {
	s.mu.Lock()
	s.n++
	name := fmt.Sprintf("test_%d_%s", s.n, randomHex(3))
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, s.url("postgres"))
	if err != nil {
		return "", err
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		return "", err
	}
	return s.url(name), nil
}

func (s *Server) url(db string) string {
	return fmt.Sprintf("postgres://postgres:%s@%s/%s?sslmode=disable", s.password, s.addr, db)
}

func (s *Server) waitReady() error {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		conn, err := pgx.Connect(ctx, s.url("postgres"))
		if err == nil {
			err = conn.Ping(ctx)
			conn.Close(ctx)
		}
		cancel()
		if err == nil {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return errors.New("postgres didn't come up within 30s")
}

func docker(args ...string) (string, error) {
	cmd := exec.Command("docker", args...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker %s: %v: %s", args[0], err, errOut.String())
	}
	return out.String(), nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
