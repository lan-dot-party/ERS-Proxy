package upstream

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"syscall"
	"time"
)

// The web server of ERS switches (Rapid Logic/1.1) has only a few connection
// slots for all clients together. When two browsers or a browser and the EDM
// poller load at the same time, it closes surplus connections without an
// answer (EOF) within milliseconds. Reads are repeated after a short pause.
// A slow EOF is not repeated: for files it does not have (e.g. /help/…) the
// switch waits about 10 s and then closes the connection.
var retryDelays = []time.Duration{200 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}

// fastDrop is the longest attempt whose EOF still counts as a refused connection.
const fastDrop = 2 * time.Second

type retryKey struct{}

// WithRetry marks a request context as safe to repeat although the method is
// not GET or HEAD (e.g. an EDM read through POST). The request needs GetBody
// if it has a body.
func WithRetry(ctx context.Context) context.Context {
	return context.WithValue(ctx, retryKey{}, true)
}

// RetryMarked reports whether WithRetry marked ctx.
func RetryMarked(ctx context.Context) bool {
	ok, _ := ctx.Value(retryKey{}).(bool)
	return ok
}

func retryable(req *http.Request) bool {
	if req.Method == http.MethodGet || req.Method == http.MethodHead {
		return req.Body == nil || req.Body == http.NoBody || req.GetBody != nil
	}
	return RetryMarked(req.Context()) && (req.Body == nil || req.Body == http.NoBody || req.GetBody != nil)
}

// dropped reports whether the device closed or reset the connection before
// sending an answer. Depending on timing, net/http reports a connection the
// server closed right away as EOF or as its unexported errServerClosedIdle
// ("http: server closed idle connection"), which does not wrap io.EOF.
func dropped(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) ||
		strings.Contains(err.Error(), "server closed idle connection")
}

type retryTransport struct {
	next   http.RoundTripper
	delays []time.Duration
	fast   time.Duration
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.next.RoundTrip(req)
	if err == nil || !retryable(req) {
		return resp, err
	}
	for _, d := range t.delays {
		if !dropped(err) || time.Since(start) > t.fast {
			return nil, err
		}
		slog.Debug("device dropped the connection, repeating", "component", "upstream", "path", req.URL.Path,
			"after", time.Since(start).Round(time.Millisecond), "err", err)
		select {
		case <-req.Context().Done():
			return nil, err
		case <-time.After(d):
		}
		r := req.Clone(req.Context())
		if req.GetBody != nil {
			body, berr := req.GetBody()
			if berr != nil {
				return nil, err
			}
			r.Body = body
		}
		start = time.Now()
		resp, err = t.next.RoundTrip(r)
		if err == nil {
			return resp, nil
		}
	}
	return nil, err
}

// EDMReadCommand reports whether an EDM bridge command (form field "command"
// of /jsp/*.jsp) only reads. Only those may be repeated.
func EDMReadCommand(command string) bool {
	switch command {
	case "getVariables", "getNextVariables", "getTableVariables":
		return true
	}
	return false
}
