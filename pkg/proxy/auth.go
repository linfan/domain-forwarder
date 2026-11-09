package proxy

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strings"
	"sync/atomic"

	"golang.org/x/crypto/bcrypt"
)

// Auth is the single Basic-auth credential pair proxy clients must present.
// Password is either plaintext or a bcrypt hash starting with "$2".
type Auth struct {
	Username string
	Password string
}

// authenticator verifies Proxy-Authorization credentials.
type authenticator struct {
	username   string
	password   string
	bcryptHash []byte

	// verified caches the digest of the last accepted password. bcrypt is
	// deliberately expensive and clients resend credentials on every proxied
	// request and every CONNECT, so only the first request pays the cost.
	verified atomic.Pointer[[sha256.Size]byte]
}

func newAuthenticator(a *Auth) *authenticator {
	au := &authenticator{username: a.Username, password: a.Password}
	if strings.HasPrefix(a.Password, "$2") {
		au.bcryptHash = []byte(a.Password)
	}
	return au
}

// allow reports whether r carries valid proxy credentials.
func (a *authenticator) allow(r *http.Request) bool {
	username, password, ok := proxyBasicAuth(r.Header.Get("Proxy-Authorization"))
	if !ok {
		return false
	}

	digest := sha256.Sum256([]byte(password))
	if cached := a.verified.Load(); cached != nil && subtle.ConstantTimeCompare(cached[:], digest[:]) == 1 {
		return secureEqual(username, a.username)
	}

	if !secureEqual(username, a.username) {
		return false
	}
	if a.bcryptHash != nil {
		if bcrypt.CompareHashAndPassword(a.bcryptHash, []byte(password)) != nil {
			return false
		}
	} else if !secureEqual(password, a.password) {
		return false
	}
	a.verified.Store(&digest)
	return true
}

// proxyBasicAuth parses a Basic Proxy-Authorization value. The scheme is
// matched case-insensitively (RFC 7235) and credentials split at the first
// colon (RFC 7617).
func proxyBasicAuth(header string) (username, password string, ok bool) {
	scheme, encoded, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Basic") {
		return "", "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return "", "", false
	}
	username, password, ok = strings.Cut(string(decoded), ":")
	return username, password, ok
}

// secureEqual compares strings in constant time by comparing SHA-256 digests,
// which also hides the length of the expected value.
func secureEqual(a, b string) bool {
	ad := sha256.Sum256([]byte(a))
	bd := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ad[:], bd[:]) == 1
}
