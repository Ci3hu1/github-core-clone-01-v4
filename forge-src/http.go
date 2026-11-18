package main

// Raw TCP HTTP/1.1 server. No net/http, no encoding/json.
// One goroutine per connection; one Write per response; TCP_NODELAY on
// every accepted connection; keep-alive; Expect: 100-continue and chunked
// request bodies supported.

import (
	"bufio"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type Request struct {
	Method string
	Path   string // decoded path without query
	Raw    string // raw request target
	Query  string
	Host   string
	Auth   string // Authorization header value (may be "")
	Body   []byte

	headers map[string]string
}

func (r *Request) Header(name string) string {
	return r.headers[strings.ToLower(name)]
}

func (r *Request) QueryParam(name string) string {
	for _, kv := range strings.Split(r.Query, "&") {
		if kv == "" {
			continue
		}
		k, v, _ := strings.Cut(kv, "=")
		if k == name {
			return unescapeQuery(v)
		}
	}
	return ""
}

func unescapeQuery(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '+' {
			b.WriteByte(' ')
		} else if c == '%' && i+2 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				b.WriteByte(byte(n))
				i += 2
			} else {
				b.WriteByte(c)
			}
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

type Response struct {
	Status      int
	ContentType string
	Body        []byte
	Headers     map[string]string // extra headers
}

func jsonResp(status int, body []byte) Response {
	return Response{Status: status, ContentType: "application/json", Body: body}
}

func errJSON(status int, msg string) Response {
	return jsonResp(status, []byte(`{"message":`+jsonString(msg)+`}`))
}

var reqCounter uint64

type Server struct {
	store *Store
}

func (s *Server) serveConn(conn net.Conn) {
	defer conn.Close()
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	br := bufio.NewReaderSize(conn, 65536)
	buf := make([]byte, 0, 8192)
	for {
		conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		req, keepAlive, err := readRequest(br, conn)
		if err != nil {
			return
		}
		resp := s.dispatch(req)
		id := atomic.AddUint64(&reqCounter, 1)
		buf = buf[:0]
		buf = append(buf, "HTTP/1.1 "...)
		buf = strconv.AppendInt(buf, int64(resp.Status), 10)
		buf = append(buf, ' ')
		buf = append(buf, statusText(resp.Status)...)
		buf = append(buf, "\r\nContent-Type: "...)
		ct := resp.ContentType
		if ct == "" {
			ct = "application/json"
		}
		buf = append(buf, ct...)
		buf = append(buf, "\r\nContent-Length: "...)
		buf = strconv.AppendInt(buf, int64(len(resp.Body)), 10)
		buf = append(buf, "\r\nX-GitHub-Request-Id: req-"...)
		buf = strconv.AppendUint(buf, id, 10)
		for k, v := range resp.Headers {
			buf = append(buf, "\r\n"...)
			buf = append(buf, k...)
			buf = append(buf, ": "...)
			buf = append(buf, v...)
		}
		if keepAlive {
			buf = append(buf, "\r\nConnection: keep-alive"...)
		} else {
			buf = append(buf, "\r\nConnection: close"...)
		}
		buf = append(buf, "\r\n\r\n"...)
		buf = append(buf, resp.Body...)
		conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
		if _, err := conn.Write(buf); err != nil {
			return
		}
		if !keepAlive {
			return
		}
	}
}

func statusText(code int) string {
	switch code {
	case 200:
		return "OK"
	case 201:
		return "Created"
	case 204:
		return "No Content"
	case 400:
		return "Bad Request"
	case 401:
		return "Unauthorized"
	case 404:
		return "Not Found"
	case 405:
		return "Method Not Allowed"
	case 409:
		return "Conflict"
	case 422:
		return "Unprocessable Entity"
	default:
		return "Status"
	}
}

var errBadRequest = errors.New("bad request")

func readLine(br *bufio.Reader) (string, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimSuffix(line, "\n")
	line = strings.TrimSuffix(line, "\r")
	if len(line) > 65536 {
		return "", errBadRequest
	}
	return line, nil
}

func readRequest(br *bufio.Reader, conn net.Conn) (*Request, bool, error) {
	line, err := readLine(br)
	if err != nil {
		return nil, false, err
	}
	if line == "" {
		// stray blank line before a request (tolerate once)
		line, err = readLine(br)
		if err != nil {
			return nil, false, err
		}
	}
	parts := strings.SplitN(line, " ", 3)
	if len(parts) != 3 {
		return nil, false, errBadRequest
	}
	req := &Request{Method: parts[0], Raw: parts[1], headers: make(map[string]string)}
	target := parts[1]
	if i := strings.IndexByte(target, '?'); i >= 0 {
		req.Path = target[:i]
		req.Query = target[i+1:]
	} else {
		req.Path = target
	}
	// Headers.
	for n := 0; n < 200; n++ {
		hline, err := readLine(br)
		if err != nil {
			return nil, false, err
		}
		if hline == "" {
			break
		}
		k, v, ok := strings.Cut(hline, ":")
		if !ok {
			return nil, false, errBadRequest
		}
		req.headers[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	req.Host = req.headers["host"]
	req.Auth = req.headers["authorization"]
	// Expect: 100-continue must be answered before the body is read.
	if strings.EqualFold(req.headers["expect"], "100-continue") {
		conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := conn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n")); err != nil {
			return nil, false, err
		}
	}
	// Body.
	te := req.headers["transfer-encoding"]
	if strings.Contains(strings.ToLower(te), "chunked") {
		body, err := readChunked(br)
		if err != nil {
			return nil, false, err
		}
		req.Body = body
	} else if cl := req.headers["content-length"]; cl != "" {
		n, err := strconv.Atoi(strings.TrimSpace(cl))
		if err != nil || n < 0 || n > 64*1024*1024 {
			return nil, false, errBadRequest
		}
		if n > 0 {
			body := make([]byte, n)
			if _, err := io.ReadFull(br, body); err != nil {
				return nil, false, err
			}
			req.Body = body
		}
	}
	// Expect: 100-continue was answered above, before the body read.
	keepAlive := true
	if c := req.headers["connection"]; strings.EqualFold(c, "close") {
		keepAlive = false
	}
	if strings.HasPrefix(parts[2], "HTTP/1.0") && !strings.EqualFold(req.headers["connection"], "keep-alive") {
		keepAlive = false
	}
	return req, keepAlive, nil
}

func readChunked(br *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		line, err := readLine(br)
		if err != nil {
			return nil, err
		}
		sizeStr := line
		if i := strings.IndexByte(sizeStr, ';'); i >= 0 {
			sizeStr = sizeStr[:i]
		}
		n, err := strconv.ParseInt(strings.TrimSpace(sizeStr), 16, 64)
		if err != nil || n < 0 || n > 64*1024*1024 {
			return nil, errBadRequest
		}
		if n == 0 {
			// trailer headers until blank line
			for {
				l, err := readLine(br)
				if err != nil {
					return nil, err
				}
				if l == "" {
					break
				}
			}
			return out, nil
		}
		chunk := make([]byte, n)
		if _, err := io.ReadFull(br, chunk); err != nil {
			return nil, err
		}
		out = append(out, chunk...)
		// trailing CRLF
		if _, err := br.ReadString('\n'); err != nil {
			return nil, err
		}
	}
}

// readChunked reads a chunked transfer-encoded body.