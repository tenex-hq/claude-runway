package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Locates the OAuth access token Claude Code already stores on this machine. Two
// backends exist and both are tried, because which one is in use varies by platform
// and install age:
//
//	1. ~/.claude/.credentials.json   (Linux, WSL, and many macOS installs)
//	2. macOS Keychain, generic password item "Claude Code-credentials"
//
// Read-only by design: this never writes credentials back, so nothing here can lock you
// out of Claude Code itself. The token is never logged and never placed in a process
// argument list (see usage.go for how it reaches curl).

const keychainService = "Claude Code-credentials"

type credentials struct {
	token     string
	source    string // "file" | "keychain"
	expiresAt time.Time
	expired   bool
	plan      string
}

func credentialsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", ".credentials.json")
}

// The subset of the credentials file we care about. Anything else in there is none of
// our business.
type credFile struct {
	ClaudeAiOauth struct {
		AccessToken      string `json:"accessToken"`
		ExpiresAt        int64  `json:"expiresAt"`
		SubscriptionType string `json:"subscriptionType"`
	} `json:"claudeAiOauth"`
}

func parseCredentials(raw []byte, source string) (credentials, bool) {
	var f credFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return credentials{}, false
	}
	o := f.ClaudeAiOauth
	if o.AccessToken == "" {
		return credentials{}, false
	}
	c := credentials{token: o.AccessToken, source: source, plan: o.SubscriptionType}
	// A missing expiresAt is treated as unknown, not as expired: a false "expired" would
	// send someone off to re-authenticate for no reason.
	if o.ExpiresAt > 0 {
		c.expiresAt = time.UnixMilli(o.ExpiresAt)
		c.expired = !c.expiresAt.After(time.Now())
	}
	return c, true
}

func fromFile() (credentials, bool) {
	p := credentialsPath()
	if p == "" {
		return credentials{}, false
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return credentials{}, false
	}
	return parseCredentials(raw, "file")
}

func fromKeychain() (credentials, bool) {
	if runtime.GOOS != "darwin" {
		return credentials{}, false
	}
	// -w prints only the secret, which lands in this process's memory. Stderr is
	// discarded so a locked keychain cannot leak a prompt into our output.
	cmd := exec.Command("security", "find-generic-password", "-s", keychainService, "-w")
	out, err := cmd.Output()
	if err != nil {
		return credentials{}, false
	}
	return parseCredentials([]byte(strings.TrimSpace(string(out))), "keychain")
}

func readCredentials() (credentials, bool) {
	if c, ok := fromFile(); ok {
		return c, true
	}
	return fromKeychain()
}

// diagnose reports where credentials were found without revealing anything about the
// token. Safe to print.
type diagnosis struct {
	platform  string
	path      string
	file      string
	keychain  string
	using     string
	expiresAt time.Time
}

func diagnose() diagnosis {
	d := diagnosis{platform: runtime.GOOS, path: credentialsPath(), keychain: "n/a"}
	f, fok := fromFile()
	switch {
	case fok:
		d.file = "found"
	case d.path == "":
		d.file = "absent"
	default:
		if _, err := os.Stat(d.path); err == nil {
			d.file = "unreadable-or-unexpected-shape"
		} else {
			d.file = "absent"
		}
	}
	var k credentials
	kok := false
	if runtime.GOOS == "darwin" {
		k, kok = fromKeychain()
		if kok {
			d.keychain = "found"
		} else {
			d.keychain = "absent"
		}
	}
	switch {
	case fok:
		d.using, d.expiresAt = f.source, f.expiresAt
	case kok:
		d.using, d.expiresAt = k.source, k.expiresAt
	default:
		d.using = "none"
	}
	return d
}
