package upstream

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// dropServer closes the first n connections without an answer, like an ERS
// web server whose connection slots are taken, then answers with the body it
// received (or "ok").
func dropServer(t *testing.T, n int32) (string, *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var conns atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			if conns.Add(1) <= n {
				c.Close()
				continue
			}
			go func(c net.Conn) {
				defer c.Close()
				req, err := http.ReadRequest(bufio.NewReader(c))
				if err != nil {
					return
				}
				body, _ := io.ReadAll(req.Body)
				if len(body) == 0 {
					body = []byte("ok")
				}
				resp := &http.Response{StatusCode: 200, ProtoMajor: 1, ProtoMinor: 1, ContentLength: int64(len(body)),
					Body: io.NopCloser(strings.NewReader(string(body))), Close: true}
				_ = resp.Write(c)
			}(c)
		}
	}()
	return "http://" + ln.Addr().String(), &conns
}

func fastClient(t *testing.T, base string) *http.Client {
	t.Helper()
	c, err := New(Options{BaseURL: base, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	c.Transport.(*retryTransport).delays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	return &http.Client{Transport: c.Transport}
}

func TestNoRetryAfterSlowDrop(t *testing.T) {
	base, conns := dropServer(t, 100)
	c := fastClient(t, base)
	c.Transport.(*retryTransport).fast = 0 // every drop counts as slow
	if _, err := c.Get(base + "/help/x.html"); err == nil || conns.Load() != 1 {
		t.Fatalf("slow drop was repeated: err=%v connections=%d", err, conns.Load())
	}
}

func TestRetryDroppedGet(t *testing.T) {
	base, conns := dropServer(t, 2)
	resp, err := fastClient(t, base).Get(base + "/images/help.png")
	if err != nil {
		t.Fatalf("GET after two dropped connections: %v", err)
	}
	resp.Body.Close()
	if conns.Load() != 3 {
		t.Fatalf("connections: %d", conns.Load())
	}
}

func TestRetryGivesUp(t *testing.T) {
	base, conns := dropServer(t, 100)
	if _, err := fastClient(t, base).Get(base + "/"); err == nil {
		t.Fatal("expected an error")
	}
	if conns.Load() != 4 {
		t.Fatalf("connections: %d (want 1 + 3 retries with the test delays)", conns.Load())
	}
}

func TestNoRetryForUnmarkedPost(t *testing.T) {
	base, conns := dropServer(t, 1)
	_, err := fastClient(t, base).Post(base+"/jsp/getFormMibs.jsp", "application/x-www-form-urlencoded",
		strings.NewReader("command=setVariables"))
	if err == nil || conns.Load() != 1 {
		t.Fatalf("POST without mark was repeated: err=%v connections=%d", err, conns.Load())
	}
}

func TestRetryMarkedPost(t *testing.T) {
	base, conns := dropServer(t, 1)
	req, _ := http.NewRequestWithContext(WithRetry(context.Background()), http.MethodPost, base+"/jsp/getFormMibs.jsp",
		strings.NewReader("command=getVariables&varNames=sysName"))
	resp, err := fastClient(t, base).Do(req)
	if err != nil {
		t.Fatalf("marked POST: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "command=getVariables&varNames=sysName" || conns.Load() != 2 {
		t.Fatalf("body %q after %d connections", body, conns.Load())
	}
}

func TestEDMReadCommand(t *testing.T) {
	for cmd, want := range map[string]bool{"getVariables": true, "getNextVariables": true, "getTableVariables": true,
		"setVariables": false, "logout": false, "": false} {
		if EDMReadCommand(cmd) != want {
			t.Errorf("%q: want %v", cmd, want)
		}
	}
}

func TestDropped(t *testing.T) {
	for err, want := range map[error]bool{
		io.EOF: true,
		fmt.Errorf("read: %w", syscall.ECONNRESET):        true,
		errors.New("http: server closed idle connection"): true,
		errors.New("dial tcp: i/o timeout"):               false,
	} {
		if dropped(err) != want {
			t.Errorf("%v: want %v", err, want)
		}
	}
}
