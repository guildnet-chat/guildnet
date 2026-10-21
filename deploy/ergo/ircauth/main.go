// SPDX-License-Identifier: AGPL-3.0-only

// Command guildnet-ircauth is Ergo's auth-script: Ergo hands it a login
// (one line of JSON on its input), it asks the console, and it prints the
// answer (one line of JSON). GuildNet's Ergo is patched so that the answer
// is final (accounts.auth-script.authoritative): the console keeps people's
// passwords, and its lockout applies to IRC as to the console. Accounts
// the console doesn't keep (agents') are left to Ergo's own ("fallback"),
// as is everything while the console can't be reached: people's passwords
// aren't in Ergo, so they wait for it; agents still connect.
//
// Settings, from Ergo's environment:
//
//	GUILDNET_IRC_AUTH_URL    the console's broker, /irc/login
//	GUILDNET_NETWORK         this network's name
//	GUILDNET_ERGO_TOKEN      this network's Ergo token, which the console
//	                         knows Ergo by
//	GUILDNET_BROKER_CA or GUILDNET_BROKER_CA_FILE
//	                         the console's CA certificate (base64 PEM, or a
//	                         file), to reach it over TLS trusting only it
//
// Only the standard library: it's built beside Ergo, from deploy/ergo.
package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

func main() {
	run(os.Stdin, os.Stdout, os.Getenv)
}

// input is what Ergo hands over.
type input struct {
	AccountName string `json:"accountName"`
	Passphrase  string `json:"passphrase"`
	Certfp      string `json:"certfp"`
	IP          string `json:"ip"`
}

// output is what Ergo takes back.
type output struct {
	Success     bool   `json:"success"`
	AccountName string `json:"accountName,omitempty"`
	Error       string `json:"error,omitempty"`
	Fallback    bool   `json:"fallback,omitempty"`
}

func run(in io.Reader, out io.Writer, getenv func(string) string) {
	o := answer(in, getenv)
	b, _ := json.Marshal(o)
	out.Write(append(b, '\n'))
}

func answer(in io.Reader, getenv func(string) string) output {
	line, err := bufio.NewReader(io.LimitReader(in, 64<<10)).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return output{Error: "reading the login: " + err.Error()}
	}
	var login input
	if err := json.Unmarshal(line, &login); err != nil {
		return output{Error: "the login isn't JSON"}
	}
	// A certificate's login: GuildNet has none (TLS ends before Ergo).
	if login.AccountName == "" || login.Passphrase == "" {
		return output{Error: "only passwords sign in here"}
	}
	client, err := consoleClient(getenv)
	if err != nil {
		return output{Fallback: true, Error: err.Error()}
	}
	body, _ := json.Marshal(map[string]string{"account": login.AccountName, "password": login.Passphrase, "ip": login.IP})
	req, err := http.NewRequest("POST", getenv("GUILDNET_IRC_AUTH_URL"), bytes.NewReader(body))
	if err != nil {
		return output{Fallback: true, Error: "GUILDNET_IRC_AUTH_URL: " + err.Error()}
	}
	if req.URL.Scheme != "https" {
		// People's passwords go over TLS or not at all.
		return output{Fallback: true, Error: "GUILDNET_IRC_AUTH_URL isn't https"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+getenv("GUILDNET_ERGO_TOKEN"))
	req.Header.Set("GuildNet-Network", getenv("GUILDNET_NETWORK"))
	resp, err := client.Do(req)
	if err != nil {
		return output{Fallback: true, Error: "asking the console: " + err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return output{Fallback: true, Error: "asking the console: " + resp.Status}
	}
	var said struct {
		Answer  string `json:"answer"`
		Account string `json:"account"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&said); err != nil {
		return output{Fallback: true, Error: "the console's answer isn't JSON"}
	}
	switch said.Answer {
	case "yes":
		return output{Success: true, AccountName: said.Account}
	case "not-mine":
		return output{Fallback: true}
	}
	return output{}
}

// consoleClient reaches the console over TLS, trusting only its CA.
func consoleClient(getenv func(string) string) (*http.Client, error) {
	var caPEM []byte
	if b64 := getenv("GUILDNET_BROKER_CA"); b64 != "" {
		b, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("GUILDNET_BROKER_CA: %v", err)
		}
		caPEM = b
	} else if file := getenv("GUILDNET_BROKER_CA_FILE"); file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("GUILDNET_BROKER_CA_FILE: %v", err)
		}
		caPEM = b
	} else {
		return nil, errors.New("no GUILDNET_BROKER_CA or GUILDNET_BROKER_CA_FILE: the console's CA is needed")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("the console's CA certificate isn't PEM")
	}
	return &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
	}, nil
}
