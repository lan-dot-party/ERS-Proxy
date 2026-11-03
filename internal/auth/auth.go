// Package auth provides password hashing, random tokens and rate limiting.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters (OWASP recommendation: m=19 MiB, t=2, p=1).
const (
	argonTime    = 2
	argonMemory  = 19 * 1024
	argonThreads = 1
	argonKeyLen  = 32
)

// MinPasswordLength for user passwords.
const MinPasswordLength = 10

// HashPassword returns a PHC-formatted Argon2id hash.
func HashPassword(password string) (string, error) {
	if len(password) < MinPasswordLength {
		return "", fmt.Errorf("password must have at least %d characters", MinPasswordLength)
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// CheckPassword verifies a password against a hash in constant time.
func CheckPassword(hash, password string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m uint32
	var t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash makes failed logins for unknown users take as long as real ones.
var dummyHash, _ = HashPassword("dummy-password-for-timing")

// CheckDummy burns the same time as CheckPassword.
func CheckDummy(password string) { CheckPassword(dummyHash, password) }

// RandomToken returns n random bytes, hex encoded.
func RandomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// HashToken returns the SHA-256 hex digest used to store tokens and session IDs.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ErrLocked is returned while a login key is locked out.
var ErrLocked = errors.New("too many failed logins, try again later")

// LoginLimiter locks out a key (IP or username) after repeated failures.
type LoginLimiter struct {
	MaxFailures int
	Window      time.Duration
	Lockout     time.Duration

	mu    sync.Mutex
	state map[string]*loginState
}

type loginState struct {
	failures    int
	first       time.Time
	lockedUntil time.Time
}

// NewLoginLimiter allows 5 failures per 10 minutes, then locks for 5 minutes.
func NewLoginLimiter() *LoginLimiter {
	return &LoginLimiter{MaxFailures: 5, Window: 10 * time.Minute, Lockout: 5 * time.Minute, state: map[string]*loginState{}}
}

// Check returns ErrLocked if any of the keys is locked.
func (l *LoginLimiter) Check(keys ...string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	for _, k := range keys {
		if s := l.state[k]; s != nil && now.Before(s.lockedUntil) {
			return ErrLocked
		}
	}
	return nil
}

// Fail records a failed attempt for all keys.
func (l *LoginLimiter) Fail(keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	for _, k := range keys {
		s := l.state[k]
		if s == nil || now.Sub(s.first) > l.Window {
			s = &loginState{first: now}
			l.state[k] = s
		}
		s.failures++
		if s.failures >= l.MaxFailures {
			s.lockedUntil = now.Add(l.Lockout)
			s.failures = 0
			s.first = now
		}
	}
	// Opportunistic cleanup.
	if len(l.state) > 10000 {
		for k, s := range l.state {
			if now.Sub(s.first) > l.Window && now.After(s.lockedUntil) {
				delete(l.state, k)
			}
		}
	}
}

// Success clears the failures of the keys.
func (l *LoginLimiter) Success(keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		delete(l.state, k)
	}
}

// RateLimiter is a token bucket per key.
type RateLimiter struct {
	Rate  float64
	Burst int

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewRateLimiter creates a limiter with rate per second and burst size.
func NewRateLimiter(rate float64, burst int) *RateLimiter {
	return &RateLimiter{Rate: rate, Burst: burst, buckets: map[string]*bucket{}}
}

// Allow takes one token for key.
func (r *RateLimiter) Allow(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	b := r.buckets[key]
	if b == nil {
		b = &bucket{tokens: float64(r.Burst), last: now}
		r.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * r.Rate
	if b.tokens > float64(r.Burst) {
		b.tokens = float64(r.Burst)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
