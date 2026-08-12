package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Credential discovery, driven entirely inside a temporary HOME. os.UserHomeDir honours $HOME,
// so redirecting it makes every file path here testable without going near the real ~/.claude.
//
// The Keychain is stubbed, never used: reading the real one is non-hermetic and can make macOS
// raise an unlock dialog, which would hang the suite waiting on a human. stubKeychain shadows
// the `security` binary on PATH instead.

// stubKeychain puts a fake `security` first on PATH for the duration of the test. On anything
// other than darwin fromKeychain returns before it would exec, so the stub is inert there and
// the darwin-only branches stay uncovered off darwin.
func stubKeychain(t *testing.T, output string, exitCode int) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s' " + shellSingleQuote(output) + "\nexit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "security"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func credentialJSON(accessToken string, expiresAtMillis int64, plan string) string {
	return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"expiresAt":%d,"subscriptionType":%q,"scopes":["user:inference"]}}`,
		accessToken, expiresAtMillis, plan)
}

// The subtlety worth pinning: a missing or zero expiresAt means "we do not know", not "it has
// expired". A false expiry sends someone off to re-authenticate a token that works.
func TestUnknownExpiryIsNotTreatedAsExpired(t *testing.T) {
	future := time.Now().Add(2 * time.Hour).UnixMilli()
	past := time.Now().Add(-2 * time.Hour).UnixMilli()

	cases := []struct {
		name        string
		raw         string
		wantExpired bool
		wantKnown   bool
	}{
		{"future expiry", credentialJSON(plantedAuth, future, "max"), false, true},
		{"past expiry", credentialJSON(plantedAuth, past, "max"), true, true},
		{"zero expiry", credentialJSON(plantedAuth, 0, "max"), false, false},
		{"negative expiry", credentialJSON(plantedAuth, -1, "max"), false, false},
		{"absent expiry", `{"claudeAiOauth":{"accessToken":"` + plantedAuth + `"}}`, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseCredentials([]byte(c.raw), "file")
			if !ok {
				t.Fatalf("parseCredentials rejected %s", c.name)
			}
			if got.expired != c.wantExpired {
				t.Errorf("expired = %v, want %v", got.expired, c.wantExpired)
			}
			if known := !got.expiresAt.IsZero(); known != c.wantKnown {
				t.Errorf("expiresAt known = %v, want %v (got %v)", known, c.wantKnown, got.expiresAt)
			}
		})
	}
}

// Provenance is reported next to the numbers, so it has to survive the parse intact.
func TestParsedCredentialsCarryTheirTokenSourceAndPlan(t *testing.T) {
	got, ok := parseCredentials([]byte(credentialJSON(plantedAuth, time.Now().Add(time.Hour).UnixMilli(), "max")), "keychain")
	if !ok {
		t.Fatal("a well-formed payload was rejected")
	}
	if got.token != plantedAuth {
		t.Errorf("token = %q, want the one in the payload", got.token)
	}
	if got.source != "keychain" {
		t.Errorf("source = %q, want the one the caller passed in", got.source)
	}
	if got.plan != "max" {
		t.Errorf("plan = %q, want max: it is reported as provenance next to the numbers", got.plan)
	}
}

// A payload we cannot use must fail rather than yield an empty token that would be sent as
// "Bearer " and come back 401 with no explanation of why.
func TestUnusableCredentialPayloadsAreRefused(t *testing.T) {
	cases := []struct{ name, raw string }{
		{"malformed json", `{"claudeAiOauth":`},
		{"not json at all", `keychain item not found`},
		{"empty input", ``},
		{"no accessToken field", `{"claudeAiOauth":{"expiresAt":1893456000000}}`},
		{"empty accessToken", credentialJSON("", time.Now().Add(time.Hour).UnixMilli(), "max")},
		{"wrong envelope", `{"someOtherProvider":{"accessToken":"x"}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseCredentials([]byte(c.raw), "file")
			if ok {
				t.Errorf("parseCredentials accepted %s: %+v", c.name, got)
			}
			if got.token != "" {
				t.Errorf("a refused payload still produced a token: %q", got.token)
			}
		})
	}
}

func TestCredentialsAreReadFromTheFileWhenItIsThere(t *testing.T) {
	isolateCache(t)
	stubKeychain(t, "", 1) // nothing in the keychain, so only the file can answer
	plantCredentialFile(t, plantedAuth, time.Now().Add(time.Hour).UnixMilli())

	got, ok := fromFile()
	if !ok {
		t.Fatalf("fromFile found nothing at %s", credentialsPath())
	}
	if got.token != plantedAuth || got.source != "file" {
		t.Errorf("got %+v, want the planted token labelled as coming from the file", got)
	}
	if got.expired {
		t.Error("a token valid for another hour was reported as expired")
	}
}

func TestAbsentCredentialsFileIsNotAnError(t *testing.T) {
	isolateCache(t) // an empty HOME: nothing has been written into it
	if _, ok := fromFile(); ok {
		t.Errorf("fromFile claims to have found credentials at %s in an empty home", credentialsPath())
	}
	// A directory where the file should be is just as unreadable, and must not panic or hang.
	if err := os.MkdirAll(credentialsPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, ok := fromFile(); ok {
		t.Error("fromFile read a directory as credentials")
	}
}

// The file wins. It is the cheaper source (no subprocess, no unlock prompt), and on a machine
// with both, the two are written by the same client and agree.
func TestFileCredentialsWinOverTheKeychain(t *testing.T) {
	isolateCache(t)
	stubKeychain(t, credentialJSON("keychain-value-should-not-win", time.Now().Add(time.Hour).UnixMilli(), "pro"), 0)
	plantCredentialFile(t, plantedAuth, time.Now().Add(time.Hour).UnixMilli())

	got, ok := readCredentials()
	if !ok {
		t.Fatal("readCredentials found nothing with a file present")
	}
	if got.source != "file" || got.token != plantedAuth {
		t.Errorf("got %+v, want the file's token", got)
	}

	// With the file gone the keychain is the only source left. Off darwin it is structurally
	// unavailable, so the ordering above is only genuinely proven on macOS.
	if err := os.Remove(credentialsPath()); err != nil {
		t.Fatal(err)
	}
	got, ok = readCredentials()
	if runtime.GOOS != "darwin" {
		if ok {
			t.Errorf("the keychain answered on %s, where it should not be consulted at all", runtime.GOOS)
		}
		return
	}
	if !ok || got.source != "keychain" || got.token != "keychain-value-should-not-win" {
		t.Errorf("got %+v, want the keychain's token once the file is gone", got)
	}
}

// Off darwin there is no keychain to read, and the code must not shell out to find that out.
func TestKeychainIsSkippedOffDarwin(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("darwin does read the keychain; the skip-path is what is under test here")
	}
	isolateCache(t)
	// A stub that would succeed if it were ever executed, so a pass means the exec never happened.
	stubKeychain(t, credentialJSON(plantedAuth, time.Now().Add(time.Hour).UnixMilli(), "max"), 0)
	if got, ok := fromKeychain(); ok {
		t.Errorf("fromKeychain returned %+v on %s", got, runtime.GOOS)
	}
}

func TestDoctorReportsWhereTheCredentialsCameFrom(t *testing.T) {
	isolateCache(t)
	stubKeychain(t, "", 1)
	plantCredentialFile(t, plantedAuth, time.Now().Add(90*time.Minute).UnixMilli())

	var out bytes.Buffer
	if code := runDoctor(&out); code != exitOK {
		t.Fatalf("runDoctor = %d, want 0 with usable credentials. output:\n%s", code, out.String())
	}
	for _, want := range []string{
		"using: file",
		"sources[2]{source,location,state}:",
		"file," + credentialsPath() + ",found",
		"keychain," + keychainService + ",",
		"token_expires_at: ",
		"token_expires_in: 1h30m",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("doctor output missing %q:\n%s", want, out.String())
		}
	}
}

// With no file, the keychain is the source, and doctor has to say so: the whole point of the
// command is telling someone which of the two places their credentials actually came from.
func TestDoctorReportsTheKeychainWhenThatIsTheSource(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("only darwin reads the keychain, so this branch is unreachable elsewhere")
	}
	isolateCache(t)
	expires := time.Now().Add(45 * time.Minute)
	stubKeychain(t, credentialJSON(plantedAuth, expires.UnixMilli(), "max"), 0)

	d := diagnose()
	if d.using != "keychain" {
		t.Errorf("using = %q, want keychain with no file present", d.using)
	}
	if d.keychain != "found" || d.file != "absent" {
		t.Errorf("diagnosis = %+v, want keychain found and file absent", d)
	}
	if d.expiresAt.Unix() != expires.Unix() {
		t.Errorf("expiresAt = %v, want the keychain item's own expiry %v", d.expiresAt, expires)
	}

	var out bytes.Buffer
	if code := runDoctor(&out); code != exitOK {
		t.Fatalf("runDoctor = %d, want 0:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "using: keychain") {
		t.Errorf("doctor does not name the keychain as the source:\n%s", out.String())
	}
	if strings.Contains(out.String(), plantedAuth) {
		t.Fatalf("doctor printed a token that came from the keychain:\n%s", out.String())
	}
}

// doctor is the one command whose whole job is to talk about the token, so it is the one place a
// leak would be most natural. It must never print it.
func TestDoctorNeverPrintsTheToken(t *testing.T) {
	isolateCache(t)
	stubKeychain(t, credentialJSON(plantedAuth, time.Now().Add(time.Hour).UnixMilli(), "max"), 0)
	plantCredentialFile(t, plantedAuth, time.Now().Add(time.Hour).UnixMilli())

	var out bytes.Buffer
	if code := runDoctor(&out); code != exitOK {
		t.Fatalf("runDoctor = %d, want 0. output:\n%s", code, out.String())
	}
	if strings.Contains(out.String(), plantedAuth) {
		t.Fatalf("doctor printed the token:\n%s", out.String())
	}
	// Not even a fragment of it, and nothing that looks like an authorization header.
	if strings.Contains(out.String(), plantedAuth[:12]) {
		t.Errorf("doctor printed part of the token:\n%s", out.String())
	}
	for _, forbidden := range []string{"Bearer", "accessToken", "refreshToken"} {
		if strings.Contains(out.String(), forbidden) {
			t.Errorf("doctor output mentions %q:\n%s", forbidden, out.String())
		}
	}
}

// The three states have to be distinguishable, because they call for different actions: absent
// means sign in, wrong-shape means the file exists and something else is wrong with it.
func TestDoctorDistinguishesAbsentFromUnreadable(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		isolateCache(t)
		stubKeychain(t, "", 1)
		if got := diagnose().file; got != "absent" {
			t.Errorf("file state = %q, want absent", got)
		}
	})
	t.Run("present but wrong shape", func(t *testing.T) {
		isolateCache(t)
		stubKeychain(t, "", 1)
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(credentialsPath(), []byte(`{"claudeAiOauth":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := diagnose().file; got != "unreadable-or-unexpected-shape" {
			t.Errorf("file state = %q, want unreadable-or-unexpected-shape: the file is there but unusable", got)
		}
	})
	t.Run("found", func(t *testing.T) {
		isolateCache(t)
		stubKeychain(t, "", 1)
		plantCredentialFile(t, plantedAuth, time.Now().Add(time.Hour).UnixMilli())
		if got := diagnose().file; got != "found" {
			t.Errorf("file state = %q, want found", got)
		}
	})
}

// With nothing to find, doctor has to say so and exit non-zero, because "no credentials" is a
// runtime failure a human has to act on, not a usage error and not a success.
func TestDoctorReportsNoneWhenNothingIsFound(t *testing.T) {
	isolateCache(t)
	stubKeychain(t, "", 1) // simulates "the item is not in the keychain"

	d := diagnose()
	if d.using != "none" {
		t.Errorf("using = %q, want none in an empty home", d.using)
	}
	if d.platform != runtime.GOOS {
		t.Errorf("platform = %q, want %q", d.platform, runtime.GOOS)
	}
	wantKeychain := "n/a"
	if runtime.GOOS == "darwin" {
		wantKeychain = "absent"
	}
	if d.keychain != wantKeychain {
		t.Errorf("keychain state = %q, want %q on %s", d.keychain, wantKeychain, runtime.GOOS)
	}
	if !d.expiresAt.IsZero() {
		t.Errorf("expiresAt = %v with no credentials at all", d.expiresAt)
	}

	var out bytes.Buffer
	if code := runDoctor(&out); code != exitError {
		t.Errorf("runDoctor = %d, want %d when there is nothing to find", code, exitError)
	}
	for _, want := range []string{"using: none", "error: no credentials found", "help: sign in with `claude`"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("doctor output missing %q:\n%s", want, out.String())
		}
	}
	// No expiry line when there is no token: an empty timestamp would read as an expiry of
	// the epoch.
	if strings.Contains(out.String(), "token_expires_at:") {
		t.Errorf("doctor printed an expiry for a token it never found:\n%s", out.String())
	}
}

// The doctor output is comma-separated TOON, so an unknown location has to be named rather than
// left as an empty cell that would shift the row's shape.
func TestUnknownLocationIsNamedRatherThanLeftBlank(t *testing.T) {
	if got := orUnknown(""); got != "unknown" {
		t.Errorf("orUnknown(\"\") = %q, want unknown", got)
	}
	if got := orUnknown("/home/x/.claude/.credentials.json"); got != "/home/x/.claude/.credentials.json" {
		t.Errorf("orUnknown rewrote a real value: %q", got)
	}
}
