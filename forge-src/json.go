package main

// Minimal hand-rolled JSON parser (no encoding/json anywhere in the forge).
// Parses into map[string]any / []any / string / float64 / bool / nil.
// Fast enough for the small request bodies this server sees, and allocation
// conscious: strings are built once, objects use plain Go maps.

import (
	"errors"
	"strconv"
	"strings"
)

var errBadJSON = errors.New("invalid JSON")

type jsonParser struct {
	b []byte
	i int
}

func parseJSON(b []byte) (any, error) {
	p := &jsonParser{b: b}
	p.skipWS()
	v, err := p.value()
	if err != nil {
		return nil, err
	}
	p.skipWS()
	if p.i != len(p.b) {
		return nil, errBadJSON
	}
	return v, nil
}

func (p *jsonParser) skipWS() {
	for p.i < len(p.b) {
		c := p.b[p.i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			p.i++
		} else {
			break
		}
	}
}

func (p *jsonParser) value() (any, error) {
	if p.i >= len(p.b) {
		return nil, errBadJSON
	}
	switch c := p.b[p.i]; {
	case c == '{':
		return p.object()
	case c == '[':
		return p.array()
	case c == '"':
		s, err := p.str()
		return s, err
	case c == 't':
		if p.lit("true") {
			return true, nil
		}
		return nil, errBadJSON
	case c == 'f':
		if p.lit("false") {
			return false, nil
		}
		return nil, errBadJSON
	case c == 'n':
		if p.lit("null") {
			return nil, nil
		}
		return nil, errBadJSON
	default:
		return p.number()
	}
}

func (p *jsonParser) lit(s string) bool {
	if p.i+len(s) > len(p.b) || string(p.b[p.i:p.i+len(s)]) != s {
		return false
	}
	p.i += len(s)
	return true
}

func (p *jsonParser) number() (any, error) {
	start := p.i
	if p.i < len(p.b) && p.b[p.i] == '-' {
		p.i++
	}
	for p.i < len(p.b) {
		c := p.b[p.i]
		if c >= '0' && c <= '9' || c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-' {
			p.i++
		} else {
			break
		}
	}
	if start == p.i {
		return nil, errBadJSON
	}
	f, err := strconv.ParseFloat(string(p.b[start:p.i]), 64)
	if err != nil {
		return nil, errBadJSON
	}
	return f, nil
}

func (p *jsonParser) str() (string, error) {
	// p.b[p.i] == '"'
	p.i++
	var sb strings.Builder
	for p.i < len(p.b) {
		c := p.b[p.i]
		if c == '"' {
			p.i++
			return sb.String(), nil
		}
		if c == '\\' {
			p.i++
			if p.i >= len(p.b) {
				return "", errBadJSON
			}
			e := p.b[p.i]
			p.i++
			switch e {
			case '"':
				sb.WriteByte('"')
			case '\\':
				sb.WriteByte('\\')
			case '/':
				sb.WriteByte('/')
			case 'b':
				sb.WriteByte('\b')
			case 'f':
				sb.WriteByte('\f')
			case 'n':
				sb.WriteByte('\n')
			case 'r':
				sb.WriteByte('\r')
			case 't':
				sb.WriteByte('\t')
			case 'u':
				if p.i+4 > len(p.b) {
					return "", errBadJSON
				}
				n, err := strconv.ParseUint(string(p.b[p.i:p.i+4]), 16, 32)
				if err != nil {
					return "", errBadJSON
				}
				p.i += 4
				if n >= 0xD800 && n <= 0xDBFF && p.i+6 <= len(p.b) && p.b[p.i] == '\\' && p.b[p.i+1] == 'u' {
					n2, err2 := strconv.ParseUint(string(p.b[p.i+2:p.i+6]), 16, 32)
					if err2 == nil && n2 >= 0xDC00 && n2 <= 0xDFFF {
						p.i += 6
						r := 0x10000 + (rune(n-0xD800) << 10) + rune(n2-0xDC00)
						sb.WriteRune(r)
						continue
					}
				}
				sb.WriteRune(rune(n))
			default:
				return "", errBadJSON
			}
			continue
		}
		sb.WriteByte(c)
		p.i++
	}
	return "", errBadJSON
}

func (p *jsonParser) object() (any, error) {
	p.i++ // {
	m := make(map[string]any)
	p.skipWS()
	if p.i < len(p.b) && p.b[p.i] == '}' {
		p.i++
		return m, nil
	}
	for {
		p.skipWS()
		if p.i >= len(p.b) || p.b[p.i] != '"' {
			return nil, errBadJSON
		}
		k, err := p.str()
		if err != nil {
			return nil, err
		}
		p.skipWS()
		if p.i >= len(p.b) || p.b[p.i] != ':' {
			return nil, errBadJSON
		}
		p.i++
		p.skipWS()
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		m[k] = v
		p.skipWS()
		if p.i >= len(p.b) {
			return nil, errBadJSON
		}
		if p.b[p.i] == '}' {
			p.i++
			return m, nil
		}
		if p.b[p.i] != ',' {
			return nil, errBadJSON
		}
		p.i++
	}
}

func (p *jsonParser) array() (any, error) {
	p.i++ // [
	a := make([]any, 0, 4)
	p.skipWS()
	if p.i < len(p.b) && p.b[p.i] == ']' {
		p.i++
		return a, nil
	}
	for {
		p.skipWS()
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		a = append(a, v)
		p.skipWS()
		if p.i >= len(p.b) {
			return nil, errBadJSON
		}
		if p.b[p.i] == ']' {
			p.i++
			return a, nil
		}
		if p.b[p.i] != ',' {
			return nil, errBadJSON
		}
		p.i++
	}
}

// --- typed accessors over parsed values (nil-safe) ---

func asObj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asStr(v any) string {
	s, _ := v.(string)
	return s
}

func asBool(v any, def bool) bool {
	b, ok := v.(bool)
	if !ok {
		return def
	}
	return b
}

func asInt(v any, def int) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case string:
		return atoiDef(n, def)
	}
	return def
}

func asArr(v any) []any {
	a, _ := v.([]any)
	return a
}

// --- JSON writer for generic values (used by GraphQL/MCP responses) ---

func writeJSONValue(buf *[]byte, v any) {
	switch t := v.(type) {
	case nil:
		*buf = append(*buf, "null"...)
	case bool:
		if t {
			*buf = append(*buf, "true"...)
		} else {
			*buf = append(*buf, "false"...)
		}
	case string:
		escapeJSONString(buf, t)
	case float64:
		*buf = strconv.AppendFloat(*buf, t, 'f', -1, 64)
	case int:
		*buf = strconv.AppendInt(*buf, int64(t), 10)
	case int64:
		*buf = strconv.AppendInt(*buf, t, 10)
	case []any:
		*buf = append(*buf, '[')
		for i, e := range t {
			if i > 0 {
				*buf = append(*buf, ',')
			}
			writeJSONValue(buf, e)
		}
		*buf = append(*buf, ']')
	case map[string]any:
		*buf = append(*buf, '{')
		i := 0
		// Deterministic key order: sort keys so repeated responses are
		// byte-identical (tools/list idempotence, state export).
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sortStrings(keys)
		for _, k := range keys {
			if i > 0 {
				*buf = append(*buf, ',')
			}
			escapeJSONString(buf, k)
			*buf = append(*buf, ':')
			writeJSONValue(buf, t[k])
			i++
		}
		*buf = append(*buf, '}')
	case jsonRaw:
		*buf = append(*buf, t...)
	default:
		*buf = append(*buf, "null"...)
	}
}

// jsonRaw is pre-rendered JSON embedded verbatim.
type jsonRaw []byte

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
