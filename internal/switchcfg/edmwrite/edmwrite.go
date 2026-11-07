// Package edmwrite writes objects through the EDM web interface of a switch
// ("SNMP over HTTP"): one POST of the command setVariables to
// /jsp/setFormMibs.jsp per step, over the device's shared edmsession.Session.
//
// The formats come from EDM's own JavaScript (ERS 5600 webroot and the EDM of
// VOSS 8.10) and were checked on a VSP 8404 with VOSS 8.10.1 (step 8.1):
//   - form fields command=setVariables, varNames, varValues and varCount, plus
//     userId, which the session adds;
//   - several variables are joined with a TAB in varNames and in varValues;
//     varCount is their number; names are "object.index" ("name.0" for a
//     scalar);
//   - integers and enums are decimal text, text objects (ifAlias, rcVlanName)
//     plain text, PortSet bitmaps "0x" plus two-digit lowercase hex octets
//     joined with ":" ("0x00:40:80"); EDM sends an empty PortSet as "0x00:00"
//     and an empty text as "0x00:" (an empty value makes a VSP's web server
//     hang); the switch decodes text that starts with "0x" as hex, so planners
//     refuse such text;
//   - a VSP answers a successful write with "name:value\r\n" per variable,
//     and an error with "snmpErrorStatus:<n>\r\nsnmpErrorIndex:<i>\r\n";
//     EDM's own parser also knows a third line "<errIndex>:<message>" and
//     "owdErrorStatus:<n>". Any answer without an error marker is success.
//
// ERS switches are expected to take the same formats (same EDM code), but
// this has not been tried on one yet.
package edmwrite

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/lan-dot-party/ers-proxy/internal/edmsession"
	"github.com/lan-dot-party/ers-proxy/internal/monitor"
	"github.com/lan-dot-party/ers-proxy/internal/switchcfg"
)

// Path is the EDM page that takes writes.
const Path = "/jsp/setFormMibs.jsp"

// Binary lists the objects whose octet strings are sent as hex ("0x00:40").
// Other octet strings are sent as text unless they contain non-printable
// bytes.
var Binary = map[string]bool{"rcVlanPortMembers": true, "rcMltPortMembers": true}

// Writer implements switchcfg.Writer on a session.
type Writer struct{ S *edmsession.Session }

var _ switchcfg.Writer = Writer{}

// Set sends the assignments as one setVariables request.
//
// A write is never repeated by us. edmsession.Session.Post re-sends a request
// only when the switch answered with its login page, i.e. when nothing was
// executed, and it does not give setVariables the transport-level retry that
// read commands get (upstream.EDMReadCommand).
func (w Writer) Set(ctx context.Context, as []switchcfg.Assignment) error {
	if len(as) == 0 {
		return nil
	}
	form, err := Encode(as)
	if err != nil {
		return err
	}
	text, err := w.S.Post(ctx, Path, form)
	if err != nil {
		return fmt.Errorf("setVariables: %w", err)
	}
	return ParseAnswer(text)
}

// Encode builds the form of a setVariables request (without userId).
func Encode(as []switchcfg.Assignment) (url.Values, error) {
	if len(as) == 0 {
		return nil, fmt.Errorf("edmwrite: nothing to set")
	}
	names := make([]string, len(as))
	values := make([]string, len(as))
	for i, a := range as {
		names[i] = a.Instance()
		if err := checkField(names[i]); err != nil {
			return nil, fmt.Errorf("edmwrite: object name %q: %w", names[i], err)
		}
		v, err := encodeValue(a)
		if err != nil {
			return nil, fmt.Errorf("edmwrite: %s: %w", names[i], err)
		}
		values[i] = v
	}
	return url.Values{
		"command":   {"setVariables"},
		"varNames":  {strings.Join(names, "\t")},
		"varValues": {strings.Join(values, "\t")},
		"varCount":  {strconv.Itoa(len(as))},
	}, nil
}

func checkField(s string) error {
	if strings.ContainsAny(s, "\t\r\n") {
		return fmt.Errorf("contains TAB, CR or LF")
	}
	return nil
}

func encodeValue(a switchcfg.Assignment) (string, error) {
	v := a.Value
	switch v.Kind {
	case monitor.KindInt:
		return strconv.FormatInt(v.Int, 10), nil
	case monitor.KindUint:
		return strconv.FormatUint(v.Uint, 10), nil
	case monitor.KindOID:
		if err := checkField(v.OID); err != nil {
			return "", err
		}
		return v.OID, nil
	case monitor.KindBytes:
		if Binary[a.Name] {
			return hexOctets(v.Bytes), nil
		}
		if len(v.Bytes) == 0 {
			return "0x00:", nil // EDM's empty text
		}
		// Text with a TAB, CR or LF would break the framing; it is refused
		// rather than hex-encoded, because EDM has no text objects with them.
		if err := checkField(string(v.Bytes)); err != nil {
			return "", err
		}
		if !printable(v.Bytes) {
			return hexOctets(v.Bytes), nil
		}
		return string(v.Bytes), nil
	}
	return "", fmt.Errorf("no value to write")
}

func printable(b []byte) bool {
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}

func hexOctets(b []byte) string {
	if len(b) == 0 {
		return "0x00:00" // what EDM sends for an empty PortSet
	}
	var sb strings.Builder
	sb.WriteString("0x")
	for i, c := range b {
		if i > 0 {
			sb.WriteByte(':')
		}
		fmt.Fprintf(&sb, "%02x", c)
	}
	return sb.String()
}

// SetError is an error answer of the switch to a setVariables request. The
// request was rejected as a whole by the switch's SNMP agent, except for
// answers EDM reports as errors that we cannot tell apart.
type SetError struct {
	// Status is the SNMP error status (10 wrongValue, 2 noSuchName, …), or
	// the owdErrorStatus when OWD is set.
	Status int
	// OWD marks an "owdErrorStatus" answer.
	OWD bool
	// Index is the 1-based position of the failing variable (0 = not given).
	Index   int
	Message string
}

func (e *SetError) Error() string {
	kind := "snmpErrorStatus"
	if e.OWD {
		kind = "owdErrorStatus"
	}
	s := fmt.Sprintf("switch rejected the write: %s %d", kind, e.Status)
	if name := statusNames[e.Status]; name != "" && !e.OWD {
		s += " (" + name + ")"
	}
	if e.Index > 0 {
		s += fmt.Sprintf(" at variable %d", e.Index)
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// statusNames are the SNMP error statuses (RFC 3416) a write may get.
var statusNames = map[int]string{1: "tooBig", 2: "noSuchName", 3: "badValue", 4: "readOnly", 5: "genErr",
	6: "noAccess", 7: "wrongType", 8: "wrongLength", 9: "wrongEncoding", 10: "wrongValue", 11: "noCreation",
	12: "inconsistentValue", 13: "resourceUnavailable", 14: "commitFailed", 15: "undoFailed",
	16: "authorizationError", 17: "notWritable", 18: "inconsistentName"}

var (
	snmpIndex  = regexp.MustCompile(`snmpErrorIndex:(\d+)`)
	snmpStatus = regexp.MustCompile(`snmpErrorStatus:([+|-]?\d+)`)
	owdStatus  = regexp.MustCompile(`owdErrorStatus:([+|-]?\d+)`)
)

// ParseAnswer turns the answer of setFormMibs.jsp into nil or a *SetError.
// A status other than 0, or an owdErrorStatus, is an error; 0 and an answer
// without any marker count as success.
func ParseAnswer(text string) error {
	lines := strings.Split(text, "\r\n")
	if m := owdStatus.FindStringSubmatch(text); m != nil {
		n, _ := strconv.Atoi(strings.TrimPrefix(m[1], "+"))
		return &SetError{Status: n, OWD: true, Message: strings.TrimSpace(text)}
	}
	m := snmpStatus.FindStringSubmatch(text)
	if m == nil {
		return nil
	}
	n, err := strconv.Atoi(strings.TrimPrefix(m[1], "+"))
	if err != nil {
		return &SetError{Status: -1, Message: strings.TrimSpace(text)} // "|" is allowed by EDM's pattern
	}
	if n == 0 {
		return nil
	}
	e := &SetError{Status: n}
	if m := snmpIndex.FindStringSubmatch(text); m != nil {
		e.Index, _ = strconv.Atoi(m[1])
	}
	if len(lines) > 2 {
		if idx, msg, ok := strings.Cut(strings.TrimSpace(lines[2]), ":"); ok {
			if i, err := strconv.Atoi(idx); err == nil {
				e.Index, e.Message = i, strings.TrimSpace(msg)
			}
		}
	}
	return e
}
