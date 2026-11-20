package main

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseCredentials(t *testing.T) {
	for _, tc := range []struct {
		in, user, pass string
		wantErr        bool
	}{
		{"username = admin\npassword = hunter2\n", "admin", "hunter2", false},
		{"# comment\n\n  Username=me  \nPASSWORD =  p:a=ss word \n", "me", "p:a=ss word", false}, // ':' and '=' fine in passwords
		{"username = admin\n", "", "", true},                                                     // no password
		{"password = x\n", "", "", true},                                                         // no username
		{"username = a:b\npassword = x\n", "", "", true},                                         // ':' splits Basic auth
		{"username = a\npassword = x\nemail = y\n", "", "", true},                                // unknown setting
		{"username admin\npassword = x\n", "", "", true},                                         // no '='
		{"username = a\r\npassword = x\r\n", "a", "x", false},                                    // Windows line endings
	} {
		user, pass, err := parseCredentials([]byte(tc.in))
		if user != tc.user || pass != tc.pass || (err != nil) != tc.wantErr {
			t.Errorf("parseCredentials(%q) = %q, %q, %v", tc.in, user, pass, err)
		}
	}
}

func TestCredentialsCreatedOnFirstStart(t *testing.T) {
	quietLog(t)
	path := filepath.Join(t.TempDir(), "data", credentialsName)
	a, err := newAuthenticator(path)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("credentials mode %v, want 0600", fi.Mode().Perm())
	}
	if a.user != "admin" || len(a.pass) < 20 {
		t.Errorf("created login %q / %q: want admin and a long random password", a.user, a.pass)
	}
	b, _ := newAuthenticator(filepath.Join(t.TempDir(), credentialsName))
	if b.pass == a.pass {
		t.Error("two new files got the same password")
	}

	// An existing file is used as it is, and a broken one stops startup.
	os.WriteFile(path, []byte("username = me\npassword = mine\n"), 0o600)
	if a, err := newAuthenticator(path); err != nil || a.user != "me" || a.pass != "mine" {
		t.Errorf("existing file: %v, %v", a, err)
	}
	os.WriteFile(path, []byte("username = me\n"), 0o600)
	if _, err := newAuthenticator(path); err == nil {
		t.Error("broken file at startup: expected an error")
	}
}

// quietLog discards log output for the rest of the test.
func quietLog(t *testing.T) {
	t.Helper()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
}

// loginServer serves the test library with a login from a fresh credentials file.
func loginServer(t *testing.T, content string) (*authenticator, *httptest.Server) {
	t.Helper()
	cat, _ := testLibrary(t)
	path := filepath.Join(t.TempDir(), credentialsName)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := newAuthenticator(path)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newServer(cat, a))
	t.Cleanup(srv.Close)
	return a, srv
}

// editCredentials rewrites the file and lets the next request see it at once.
func editCredentials(t *testing.T, a *authenticator, content string) {
	t.Helper()
	if err := os.WriteFile(a.path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Second) // make the change visible even on coarse mtimes
	os.Chtimes(a.path, later, later)
	a.mu.Lock()
	a.lastCheck = time.Time{}
	a.mu.Unlock()
}

type login struct {
	user, pass string
	cookie     *http.Cookie
}

func get(t *testing.T, url string, l login) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if l.user != "" || l.pass != "" {
		req.SetBasicAuth(l.user, l.pass)
	}
	if l.cookie != nil {
		req.AddCookie(l.cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func sessionOf(resp *http.Response) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	return nil
}

func TestLogin(t *testing.T) {
	a, srv := loginServer(t, "username = reader\npassword = old pass\n")
	url := srv.URL + "/api/v1/libraries"

	resp := get(t, url, login{})
	if resp.StatusCode != 401 || !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Basic ") {
		t.Fatalf("no login: %d %q; want 401 with a Basic challenge", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	for name, l := range map[string]login{
		"wrong password": {user: "reader", pass: "nope"},
		"wrong user":     {user: "admin", pass: "old pass"},
		"swapped":        {user: "old pass", pass: "reader"},
		"forged cookie":  {cookie: &http.Cookie{Name: sessionCookie, Value: "forged"}},
	} {
		if resp := get(t, url, l); resp.StatusCode != 401 {
			t.Errorf("%s: %d, want 401", name, resp.StatusCode)
		}
	}
	if code, _ := fetch(t, "GET", srv.URL+"/healthz", ""); code != 200 {
		t.Errorf("healthz: %d, want 200 without login", code)
	}
	if resp := get(t, srv.URL+"/api/v1/books/X/pages/1", login{}); resp.StatusCode != 401 {
		t.Errorf("page without login: %d, want 401", resp.StatusCode)
	}

	// Right password: allowed, and a session cookie that works on its own.
	resp = get(t, url, login{user: "reader", pass: "old pass"})
	cookie := sessionOf(resp)
	if resp.StatusCode != 200 || cookie == nil || !cookie.HttpOnly {
		t.Fatalf("login: %d, cookie %v; want 200 and an HttpOnly session cookie", resp.StatusCode, cookie)
	}
	if resp := get(t, url, login{cookie: cookie}); resp.StatusCode != 200 {
		t.Errorf("session cookie alone: %d, want 200", resp.StatusCode)
	}

	// Changing the password: old password and old session stop working.
	editCredentials(t, a, "username = reader\npassword = new pass\n")
	for name, l := range map[string]login{
		"old password": {user: "reader", pass: "old pass"},
		"old session":  {cookie: cookie},
	} {
		if resp := get(t, url, l); resp.StatusCode != 401 {
			t.Errorf("after the change, %s: %d, want 401", name, resp.StatusCode)
		}
	}
	if resp := get(t, url, login{user: "reader", pass: "new pass"}); resp.StatusCode != 200 {
		t.Errorf("new password: %d, want 200", resp.StatusCode)
	}

	// A broken edit keeps the previous login instead of locking everyone out.
	editCredentials(t, a, "username = reader\n")
	if resp := get(t, url, login{user: "reader", pass: "new pass"}); resp.StatusCode != 200 {
		t.Errorf("after a broken edit: %d, want the previous login (200)", resp.StatusCode)
	}

	// Deleting the file makes a new one with a new random password.
	os.Remove(a.path)
	a.mu.Lock()
	a.lastCheck = time.Time{}
	a.mu.Unlock()
	if resp := get(t, url, login{user: "reader", pass: "new pass"}); resp.StatusCode != 401 {
		t.Errorf("after deleting the file, old login: %d, want 401", resp.StatusCode)
	}
	data, err := os.ReadFile(a.path)
	if err != nil {
		t.Fatalf("file not recreated: %v", err)
	}
	user, pass, err := parseCredentials(data)
	if err != nil || user != "admin" {
		t.Fatalf("recreated file: %q, %v", data, err)
	}
	if resp := get(t, url, login{user: "admin", pass: pass}); resp.StatusCode != 200 {
		t.Errorf("recreated login: %d, want 200", resp.StatusCode)
	}
}

func TestLoginOff(t *testing.T) {
	_, srv := testLibrary(t) // newServer(cat, nil): SHELF_AUTH=off
	if resp := get(t, srv.URL+"/api/v1/libraries", login{}); resp.StatusCode != 200 {
		t.Errorf("SHELF_AUTH=off: %d, want 200 without login", resp.StatusCode)
	}
}
