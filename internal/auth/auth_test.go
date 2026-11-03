package auth

import (
	"strings"
	"testing"
)

func TestPasswordHash(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$") {
		t.Fatalf("unexpected hash format %q", h)
	}
	if !CheckPassword(h, "correct horse battery") {
		t.Fatal("correct password rejected")
	}
	if CheckPassword(h, "wrong horse battery") {
		t.Fatal("wrong password accepted")
	}
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("short password accepted")
	}
}

func TestRandomToken(t *testing.T) {
	tok := RandomToken(16)
	if len(tok) != 32 || RandomToken(16) == tok {
		t.Fatalf("token %q", tok)
	}
	if HashToken(tok) == tok || len(HashToken(tok)) != 64 {
		t.Fatal("hash looks wrong")
	}
}

func TestLoginLimiter(t *testing.T) {
	l := NewLoginLimiter()
	for i := 0; i < 4; i++ {
		l.Fail("ip:1")
	}
	if l.Check("ip:1") != nil {
		t.Fatal("locked too early")
	}
	l.Fail("ip:1")
	if l.Check("ip:1") != ErrLocked {
		t.Fatal("not locked after 5 failures")
	}
	if l.Check("ip:2") != nil {
		t.Fatal("other key locked")
	}
}

func TestRateLimiter(t *testing.T) {
	r := NewRateLimiter(1, 3)
	for i := 0; i < 3; i++ {
		if !r.Allow("a") {
			t.Fatal("burst denied")
		}
	}
	if r.Allow("a") {
		t.Fatal("over burst allowed")
	}
}
