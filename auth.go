package main

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Login: every request needs the username and password from the credentials
// file in the data folder, sent with HTTP Basic auth (the Komga extension's
// username/password settings). The file is created on first start with the
// user "admin" and a random password; edit it to change them. Changes are
// picked up within credentialsCheck, without a restart. Deleting the file
// makes a new one with a new random password.
//
// The extension only sends the password after a request is refused, so a
// successful login also sets a session cookie, sparing it a refused request
// every time. Changing the password or restarting shelf ends all sessions;
// the extension then simply logs in again.
//
// A wrong username or password is answered after failedLoginDelay, slowing
// down guessing, and logged with the client's address (at most once per
// address per failedLoginLogEvery, with a count of the attempts in between),
// so fail2ban and the like can act on it. Requests without credentials are
// part of the normal login and are not logged.
//
// shelf speaks plain HTTP: the password and the session cookie can be read
// on the way. Outside a trusted network, put a reverse proxy with TLS in
// front. Behind a proxy, the logged address is the proxy's.

const (
	credentialsName     = "credentials"
	credentialsCheck    = 2 * time.Second
	sessionCookie       = "shelf_session"
	failedLoginDelay    = time.Second
	failedLoginLogEvery = time.Minute
	failedLoginMaxAddrs = 1000 // addresses remembered for rate-limiting the log
)

type authenticator struct {
	path       string
	sessionKey []byte // random per run: restarting ends all sessions

	mu        sync.Mutex
	user      string
	pass      string
	modTime   time.Time
	size      int64
	lastCheck time.Time
	lastError string // last logged reload error, to avoid repeating it

	failDelay time.Duration // failedLoginDelay; shorter in tests
	failMu    sync.Mutex
	failures  map[string]*failedLogins // by client address
}

type failedLogins struct {
	logged     time.Time // when the last line for this address was logged
	suppressed int       // failures since then, not logged yet
}

// newAuthenticator loads the credentials file at path, creating it first if
// it doesn't exist. An unreadable or invalid file is an error: shelf doesn't
// start rather than run without the login the user configured.
func newAuthenticator(path string) (*authenticator, error) {
	a := &authenticator{path: path, sessionKey: make([]byte, 32), failDelay: failedLoginDelay}
	rand.Read(a.sessionKey)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := createCredentials(path); err != nil {
			return nil, err
		}
	}
	if err := a.load(); err != nil {
		return nil, err
	}
	a.lastCheck = time.Now()
	return a, nil
}

// createCredentials writes a credentials file with the user "admin" and a
// random password, readable only by its owner.
func createCredentials(path string) error {
	pw := make([]byte, 18)
	rand.Read(pw)
	content := "# shelf login, for the Komga extension's username and password settings.\n" +
		"# Edit and save to change them: shelf picks up the change within seconds.\n" +
		"# Delete this file to get a new random password.\n" +
		"# Spaces around the username and password are ignored.\n" +
		"username = admin\n" +
		"password = " + base64.RawURLEncoding.EncodeToString(pw) + "\n"
	if err := writeAtomic(filepath.Dir(path), filepath.Base(path), strings.NewReader(content)); err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	log.Printf("auth: created %s with the user \"admin\" and a random password; open the file to see or change it", path)
	return nil
}

// parseCredentials reads "username = ..." and "password = ..." lines. Blank
// lines and lines starting with # are ignored; spaces around values are trimmed.
func parseCredentials(data []byte) (user, pass string, err error) {
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key, value = strings.ToLower(strings.TrimSpace(key)), strings.TrimSpace(value)
		switch {
		case !ok:
			return "", "", fmt.Errorf("line %d: expected \"name = value\"", n)
		case key == "username":
			user = value
		case key == "password":
			pass = value
		default:
			return "", "", fmt.Errorf("line %d: unknown setting %q", n, key)
		}
	}
	switch {
	case user == "" || pass == "":
		return "", "", errors.New("needs both a username and a password")
	case strings.Contains(user, ":"):
		return "", "", errors.New("the username can't contain ':'") // Basic auth splits on it
	}
	return user, pass, nil
}

func (a *authenticator) load() error {
	data, err := readSmallFile(a.path)
	if err != nil {
		return err
	}
	fi, err := os.Stat(a.path)
	if err != nil {
		return err
	}
	user, pass, err := parseCredentials(data)
	if err != nil {
		return fmt.Errorf("%s: %w", a.path, err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		log.Printf("WARN: auth: %s can be read by other users (mode %v); chmod 600 it", a.path, fi.Mode().Perm())
	}
	a.user, a.pass, a.modTime, a.size = user, pass, fi.ModTime(), fi.Size()
	return nil
}

// refresh reloads the file if it changed, at most once per credentialsCheck.
// A broken edit is logged and the previous login stays in use; a deleted
// file is recreated with a new random password.
func (a *authenticator) refresh() {
	if time.Since(a.lastCheck) < credentialsCheck {
		return
	}
	a.lastCheck = time.Now()
	fi, err := os.Stat(a.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err = createCredentials(a.path); err == nil {
			err = a.load()
		}
	case err == nil && fi.ModTime().Equal(a.modTime) && fi.Size() == a.size:
		return // unchanged
	case err == nil:
		if err = a.load(); err == nil {
			log.Printf("auth: login updated from %s", a.path)
		}
	}
	switch {
	case err == nil:
		a.lastError = ""
	case err.Error() != a.lastError:
		log.Printf("ERROR: auth: %v; keeping the previous login", err)
		a.lastError = err.Error()
	}
}

// session is the cookie value for the current login. It changes when the
// username or password does, which ends existing sessions.
func (a *authenticator) session() string {
	m := hmac.New(sha256.New, a.sessionKey)
	m.Write([]byte(a.user + "\x00" + a.pass))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// allow reports whether r is logged in: a valid session cookie, or correct
// Basic credentials (then a session cookie is set). Otherwise it sends the
// 401 challenge the extension waits for before sending credentials.
func (a *authenticator) allow(w http.ResponseWriter, r *http.Request) bool {
	if a == nil {
		return true // SHELF_AUTH=off
	}
	a.mu.Lock()
	a.refresh()
	session := a.session()
	user, pass := a.user, a.pass
	a.mu.Unlock()

	if c, err := r.Cookie(sessionCookie); err == nil && sameSecret(c.Value, session) {
		return true
	}
	if u, p, ok := r.BasicAuth(); ok {
		// Both compared, always: no hint which of the two was wrong.
		if okUser, okPass := sameSecret(u, user), sameSecret(p, pass); okUser && okPass {
			http.SetCookie(w, &http.Cookie{
				Name: sessionCookie, Value: session, Path: "/",
				HttpOnly: true, SameSite: http.SameSiteLaxMode,
			})
			return true
		}
		a.noteFailure(r)
		select { // slow down guessing; give up early if the client leaves
		case <-time.After(a.failDelay):
		case <-r.Context().Done():
			return false
		}
	}
	w.Header().Set("WWW-Authenticate", `Basic realm="shelf", charset="UTF-8"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
	return false
}

// noteFailure logs a failed login from r's address: the first, then at most
// one line per failedLoginLogEvery, counting the attempts in between. The
// username tried isn't logged: it is sometimes a mistyped password.
func (a *authenticator) noteFailure(r *http.Request) {
	addr := r.RemoteAddr
	if host, _, err := net.SplitHostPort(addr); err == nil {
		addr = host
	}
	now := time.Now()
	a.failMu.Lock()
	defer a.failMu.Unlock()
	if a.failures == nil || len(a.failures) >= failedLoginMaxAddrs {
		for k, f := range a.failures { // forget quiet addresses (flushing their counts)
			if now.Sub(f.logged) >= failedLoginLogEvery {
				if f.suppressed > 0 {
					log.Printf("WARN: auth: %d more failed logins from %s", f.suppressed, k)
				}
				delete(a.failures, k)
			}
		}
		if a.failures == nil {
			a.failures = map[string]*failedLogins{}
		}
	}
	f := a.failures[addr]
	switch {
	case f == nil:
		if len(a.failures) < failedLoginMaxAddrs {
			a.failures[addr] = &failedLogins{logged: now}
		}
		log.Printf("WARN: auth: failed login from %s", addr)
	case now.Sub(f.logged) >= failedLoginLogEvery:
		more := ""
		if f.suppressed > 0 {
			more = fmt.Sprintf(" (and %d more since %s)", f.suppressed, f.logged.Format(time.TimeOnly))
		}
		log.Printf("WARN: auth: failed login from %s%s", addr, more)
		f.logged, f.suppressed = now, 0
	default:
		f.suppressed++
	}
}

// sameSecret compares in constant time; hashing first hides the length.
func sameSecret(a, b string) bool {
	ha, hb := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}
