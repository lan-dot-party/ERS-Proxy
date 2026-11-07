package swos

import (
	"fmt"
	"strconv"
	"strings"
)

// maxDepth limits nesting; SwOS files nest two levels deep.
const maxDepth = 8

// Parse reads a SwOS file: objects with unquoted keys, arrays, numbers
// (hex or decimal) and single-quoted strings. Objects become
// map[string]any, arrays []any, numbers uint64 and strings string (still
// hex-encoded, see Text). It is not a JavaScript parser: anything else is an
// error, nothing is evaluated.
func Parse(s string) (any, error) {
	p := &parser{s: s}
	v, err := p.value(0)
	if err != nil {
		return nil, err
	}
	p.space()
	if p.i != len(p.s) {
		return nil, p.errorf("trailing data")
	}
	return v, nil
}

type parser struct {
	s string
	i int
}

func (p *parser) errorf(format string, args ...any) error {
	return fmt.Errorf("parse at offset %d: %s", p.i, fmt.Sprintf(format, args...))
}

func (p *parser) space() {
	for p.i < len(p.s) {
		switch p.s[p.i] {
		case ' ', '\t', '\r', '\n':
			p.i++
		default:
			return
		}
	}
}

func (p *parser) value(depth int) (any, error) {
	if depth > maxDepth {
		return nil, p.errorf("nested too deeply")
	}
	p.space()
	if p.i >= len(p.s) {
		return nil, p.errorf("unexpected end")
	}
	switch c := p.s[p.i]; {
	case c == '{':
		return p.object(depth)
	case c == '[':
		return p.array(depth)
	case c == '\'' || c == '"':
		return p.str(c)
	case c >= '0' && c <= '9':
		return p.number()
	}
	return nil, p.errorf("unexpected %q", p.s[p.i])
}

func (p *parser) object(depth int) (any, error) {
	p.i++ // {
	out := map[string]any{}
	for {
		p.space()
		if p.i < len(p.s) && p.s[p.i] == '}' {
			p.i++
			return out, nil
		}
		start := p.i
		for p.i < len(p.s) && isKey(p.s[p.i]) {
			p.i++
		}
		if p.i == start {
			return nil, p.errorf("expected a key")
		}
		key := p.s[start:p.i]
		p.space()
		if p.i >= len(p.s) || p.s[p.i] != ':' {
			return nil, p.errorf("expected ':'")
		}
		p.i++
		v, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		out[key] = v
		p.space()
		if p.i < len(p.s) && p.s[p.i] == ',' {
			p.i++
			continue
		}
		if p.i < len(p.s) && p.s[p.i] == '}' {
			p.i++
			return out, nil
		}
		return nil, p.errorf("expected ',' or '}'")
	}
}

func (p *parser) array(depth int) (any, error) {
	p.i++ // [
	out := []any{}
	for {
		p.space()
		if p.i < len(p.s) && p.s[p.i] == ']' {
			p.i++
			return out, nil
		}
		v, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		p.space()
		if p.i < len(p.s) && p.s[p.i] == ',' {
			p.i++
			continue
		}
		if p.i < len(p.s) && p.s[p.i] == ']' {
			p.i++
			return out, nil
		}
		return nil, p.errorf("expected ',' or ']'")
	}
}

func (p *parser) str(quote byte) (any, error) {
	p.i++
	end := strings.IndexByte(p.s[p.i:], quote)
	if end < 0 {
		return nil, p.errorf("unterminated string")
	}
	v := p.s[p.i : p.i+end]
	if strings.IndexByte(v, '\\') >= 0 {
		return nil, p.errorf("escapes are not supported")
	}
	p.i += end + 1
	return v, nil
}

func (p *parser) number() (any, error) {
	start := p.i
	for p.i < len(p.s) && isKey(p.s[p.i]) {
		p.i++
	}
	lit := p.s[start:p.i]
	var n uint64
	var err error
	if strings.HasPrefix(lit, "0x") || strings.HasPrefix(lit, "0X") {
		n, err = strconv.ParseUint(lit[2:], 16, 64)
	} else {
		n, err = strconv.ParseUint(lit, 10, 64)
	}
	if err != nil {
		return nil, p.errorf("bad number %q", lit)
	}
	return n, nil
}

func isKey(c byte) bool {
	return c == '_' || c == '!' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}
