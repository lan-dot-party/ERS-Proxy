package secret

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRoundTripAndBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.key")
	k, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode: %v %v", fi.Mode(), err)
	}
	ct, err := k.Encrypt([]byte("RW:secret"), []byte("device|1|192.0.2.10|443"))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := k.Decrypt(ct, []byte("device|1|192.0.2.10|443"))
	if err != nil || string(plain) != "RW:secret" {
		t.Fatalf("round trip: %q %v", plain, err)
	}
	if _, err := k.Decrypt(ct, []byte("device|1|198.51.100.7|443")); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("ciphertext opened for another host: %v", err)
	}
	if _, err := k.Decrypt(ct, []byte("device|2|192.0.2.10|443")); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("ciphertext opened for another device: %v", err)
	}
	k2, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k2.Decrypt(ct, []byte("device|1|192.0.2.10|443")); err != nil {
		t.Fatalf("reloaded key cannot decrypt: %v", err)
	}
	if err := Generate(path); err == nil {
		t.Fatal("Generate overwrote an existing key")
	}
}
