package main

// Shared constants and small helpers. Determinism is a first-class rule:
// every timestamp the forge emits is fixed, ids are assigned in creation
// order, and no wall clock or unseeded randomness appears in any response.

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"strings"
)

const (
	viewerLogin = "adaiversen"
	viewerID    = 1000
	viewerName  = "ada iversen"
	viewerEmail = "adaiversen@example.invalid"

	// Fixed timestamp for every created_at / updated_at / commit time.
	fixedTimeISO  = "2025-01-01T00:00:00Z"
	fixedEpochSec = 1735689600

	zeroOID = "0000000000000000000000000000000000000000"

	agentName    = "forge/1.0"
	metaVersion  = "forge-1.0.0"
	serverName   = "forge"
	serverVer    = "1.0.0"
	mcpRevision  = "2026-07-28"
)

var hexDigits = "0123456789abcdef"

func isHexOID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func oidBytes(hexOID string) ([]byte, bool) {
	if len(hexOID) != 40 {
		return nil, false
	}
	b, err := hex.DecodeString(hexOID)
	if err != nil || len(b) != 20 {
		return nil, false
	}
	return b, true
}

func hexOID(b []byte) string { return hex.EncodeToString(b) }

// gitObjectID returns the SHA-1 object id for type+payload, as git computes it.
func gitObjectID(typ string, data []byte) string {
	h := sha1.New()
	h.Write([]byte(typ))
	h.Write([]byte{' '})
	h.Write([]byte(strconv.Itoa(len(data))))
	h.Write([]byte{0})
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// globalID builds an opaque, stable global id. It is never the plain
// database id string: the payload is base64 of "Kind:<payload>".
func globalID(kind, payload string) string {
	return base64.StdEncoding.EncodeToString([]byte(kind + ":" + payload))
}

// decodeGlobalID returns kind and payload, or ok=false.
func decodeGlobalID(id string) (kind, payload string, ok bool) {
	raw, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return "", "", false
	}
	s := string(raw)
	i := strings.IndexByte(s, ':')
	if i < 1 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

func atoiDef(s string, def int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// escapeJSONString appends s as a JSON string literal (with quotes) to buf.
func escapeJSONString(buf *[]byte, s string) {
	*buf = append(*buf, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			*buf = append(*buf, '\\', '"')
		case '\\':
			*buf = append(*buf, '\\', '\\')
		case '\n':
			*buf = append(*buf, '\\', 'n')
		case '\r':
			*buf = append(*buf, '\\', 'r')
		case '\t':
			*buf = append(*buf, '\\', 't')
		default:
			if c < 0x20 {
				*buf = append(*buf, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
			} else {
				*buf = append(*buf, c)
			}
		}
	}
	*buf = append(*buf, '"')
}

func jsonString(s string) string {
	var b []byte
	escapeJSONString(&b, s)
	return string(b)
}
