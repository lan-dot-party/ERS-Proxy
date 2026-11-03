package edmlogin

import (
	"bytes"
	"compress/gzip"
	"testing"
)

func TestAuthFailed(t *testing.T) {
	page := []byte(`<html><body><b>Authentication Failed!</b> <a href="login.html">Return to Login page</a></body></html>`)
	var z bytes.Buffer
	zw := gzip.NewWriter(&z)
	_, _ = zw.Write(page)
	_ = zw.Close()
	if !authFailed(page) || !authFailed(z.Bytes()) {
		t.Fatal("authError page not recognised (plain or gzip)")
	}
	if authFailed([]byte("<html><title>Embedded Element Manager</title></html>")) || authFailed(nil) {
		t.Fatal("index page taken as failure")
	}
}
