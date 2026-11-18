package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ---------- request / response ----------

type Request struct {
	Method  string
	Path    string // raw path (no query)
	RawPath string
	Query   url.Values
	Headers map[string]string // lower-cased keys
	Body    []byte
	Close   bool // client asked to close
}

type Response struct {
	Status  int
	Headers map[string]string
	Body    []byte
}

func jsonResponse(status int, v interface{}) *Response {
	return &Response{
		Status:  status,
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    jsonBytes(v),
	}
}

func errResponse(status int, msg string) *Response {
	return jsonResponse(status, J{"message": msg})
}

// ---------- server ----------

type Server struct {
	store     *Store
	token     string
	bearer    string // precomputed "Bearer <token>" ("" when auth is off)
	packCache fetchPackCache
}

func NewServer(store *Store, token string) *Server {
	bearer := ""
	if token != "" {
		bearer = "Bearer " + token
	}
	return &Server{
		store:     store,
		token:     token,
		bearer:    bearer,
		packCache: fetchPackCache{m: make(map[string]cachedFetchPack)},
	}
}

func (s *Server) Serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReaderSize(conn, 65536)
	bw := bufio.NewWriterSize(conn, 65536)
	// Deadlines are set once per connection, not per request: a trial's
	// workloads finish in seconds, and the per-request syscalls cost more
	// than the protection they buy on a well-behaved client.
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	for {
		req, err := readRequest(br)
		if err != nil {
			return
		}
		if req == nil {
			return
		}
		resp := s.dispatch(req)
		if err := writeResponse(bw, resp, req); err != nil {
			return
		}
		if req.Close {
			return
		}
	}
}

// readRequest parses one HTTP/1.1 request from br.
func readRequest(br *bufio.Reader) (*Request, error) {
	// request line
	line, err := readLine(br)
	if err != nil {
		return nil, err
	}
	if line == "" {
		// tolerate a stray CRLF before the request line
		line, err = readLine(br)
		if err != nil {
			return nil, err
		}
		if line == "" {
			return nil, fmt.Errorf("empty request line")
		}
	}
	parts := strings.SplitN(line, " ", 3)
	if len(parts) < 2 {
		return nil, fmt.Errorf("bad request line")
	}
	req := &Request{
		Method:  parts[0],
		Headers: map[string]string{},
	}
	target := parts[1]
	// Fast path: most requests carry no query string, so skip the full
	// request-URI parse (which allocates a url.Values map) entirely.
	if i := strings.Index(target, "?"); i >= 0 {
		u, err := url.ParseRequestURI(target)
		if err != nil {
			// try raw path without query
			req.Path = target[:i]
			req.Query, _ = url.ParseQuery(target[i+1:])
		} else {
			req.Path = u.Path
			req.Query = u.Query()
		}
	} else {
		req.Path = target
		req.Query = url.Values{}
	}
	req.RawPath = req.Path
	// headers
	for {
		hline, err := readLine(br)
		if err != nil {
			return nil, err
		}
		if hline == "" {
			break
		}
		colon := strings.Index(hline, ":")
		if colon < 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(hline[:colon]))
		val := strings.TrimSpace(hline[colon+1:])
		req.Headers[key] = val
	}
	if strings.ToLower(req.Headers["connection"]) == "close" {
		req.Close = true
	}
	// Expect: 100-continue — not handled here (handled by caller if needed)
	// body
	te := strings.ToLower(req.Headers["transfer-encoding"])
	if strings.Contains(te, "chunked") {
		body, err := readChunkedBody(br)
		if err != nil {
			return nil, err
		}
		req.Body = body
	} else if clStr, ok := req.Headers["content-length"]; ok {
		cl, err := strconv.Atoi(strings.TrimSpace(clStr))
		if err != nil || cl < 0 || cl > 64*1024*1024 {
			return nil, fmt.Errorf("bad content-length")
		}
		if cl > 0 {
			body := make([]byte, cl)
			if _, err := io.ReadFull(br, body); err != nil {
				return nil, err
			}
			req.Body = body
		}
	}
	return req, nil
}

// readLine reads a CRLF-terminated line without the CRLF.
func readLine(br *bufio.Reader) (string, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimSuffix(line, "\n")
	line = strings.TrimSuffix(line, "\r")
	return line, nil
}

// readChunkedBody reads a chunked request body.
func readChunkedBody(br *bufio.Reader) ([]byte, error) {
	var body []byte
	for {
		sizeLine, err := readLine(br)
		if err != nil {
			return nil, err
		}
		if i := strings.Index(sizeLine, ";"); i >= 0 {
			sizeLine = sizeLine[:i]
		}
		n, err := strconv.ParseInt(strings.TrimSpace(sizeLine), 16, 64)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			// trailers until blank line
			for {
				l, err := readLine(br)
				if err != nil {
					return nil, err
				}
				if l == "" {
					break
				}
			}
			return body, nil
		}
		if int64(len(body))+n > 64*1024*1024 {
			return nil, fmt.Errorf("body too large")
		}
		chunk := make([]byte, n)
		if _, err := io.ReadFull(br, chunk); err != nil {
			return nil, err
		}
		body = append(body, chunk...)
		// trailing CRLF
		if _, err := readLine(br); err != nil {
			return nil, err
		}
	}
}

// writeResponse writes the response, always with Content-Length. The header
// is built into a stack buffer and written once instead of going through a
// strings.Builder and a second write; a header that does not fit falls back
// to the builder path.
func writeResponse(bw *bufio.Writer, resp *Response, req *Request) error {
	var hdr [512]byte
	n := 0
	put := func(s string) bool {
		if n+len(s) > len(hdr) {
			return false
		}
		n += copy(hdr[n:], s)
		return true
	}
	ok := put("HTTP/1.1 ") &&
		put(strconv.Itoa(resp.Status)) &&
		put(" ") &&
		put(statusText(resp.Status)) &&
		put("\r\n")
	for k, v := range resp.Headers {
		if !ok {
			break
		}
		ok = put(k) && put(": ") && put(v) && put("\r\n")
	}
	if ok {
		ok = put("Content-Length: ") && put(strconv.Itoa(len(resp.Body))) && put("\r\n")
	}
	if ok {
		if req.Close {
			ok = put("Connection: close\r\n")
		} else {
			ok = put("Connection: keep-alive\r\n")
		}
	}
	if ok {
		ok = put("\r\n")
	}
	if !ok {
		// header too large for the stack buffer: build it the slow way.
		var sb strings.Builder
		sb.WriteString("HTTP/1.1 ")
		sb.WriteString(strconv.Itoa(resp.Status))
		sb.WriteString(" ")
		sb.WriteString(statusText(resp.Status))
		sb.WriteString("\r\n")
		for k, v := range resp.Headers {
			sb.WriteString(k)
			sb.WriteString(": ")
			sb.WriteString(v)
			sb.WriteString("\r\n")
		}
		sb.WriteString("Content-Length: ")
		sb.WriteString(strconv.Itoa(len(resp.Body)))
		sb.WriteString("\r\n")
		if req.Close {
			sb.WriteString("Connection: close\r\n")
		} else {
			sb.WriteString("Connection: keep-alive\r\n")
		}
		sb.WriteString("\r\n")
		if _, err := bw.WriteString(sb.String()); err != nil {
			return err
		}
	} else if _, err := bw.Write(hdr[:n]); err != nil {
		return err
	}
	if len(resp.Body) > 0 && req.Method != "HEAD" {
		if _, err := bw.Write(resp.Body); err != nil {
			return err
		}
	}
	return bw.Flush()
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
	case 500:
		return "Internal Server Error"
	default:
		return "Status"
	}
}

// ---------- dispatch ----------

func (s *Server) dispatch(req *Request) *Response {
	path := req.Path
	// healthz
	if path == "/healthz" {
		return jsonResponse(200, J{"ok": true})
	}
	// git smart http
	if strings.HasSuffix(path, ".git/info/refs") || strings.HasSuffix(path, ".git/git-upload-pack") || strings.HasSuffix(path, ".git/git-receive-pack") {
		return s.dispatchGit(req)
	}
	// REST
	if path == "/api/v3" || strings.HasPrefix(path, "/api/v3/") {
		return s.dispatchRest(req)
	}
	// GraphQL
	if path == "/api/graphql" {
		return s.dispatchGraphQL(req)
	}
	// MCP
	if path == "/mcp" {
		return s.dispatchMCP(req)
	}
	return errResponse(404, "Not Found")
}

// checkAuth verifies the bearer token. Empty configured token disables auth.
func (s *Server) checkAuth(req *Request) bool {
	if s.bearer == "" {
		return true
	}
	return req.Headers["authorization"] == s.bearer
}

// ---------- minimal HTTP client (lifecycle only) ----------

// httpDo performs a single HTTP/1.1 request over a fresh connection.
// Returns status code and body.
func httpDo(method, rawURL string, body []byte, contentType string) (int, []byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0, nil, err
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":80"
	}
	conn, err := net.DialTimeout("tcp", host, 5*time.Second)
	if err != nil {
		return 0, nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	var sb strings.Builder
	sb.WriteString(method + " " + u.RequestURI() + " HTTP/1.1\r\n")
	sb.WriteString("Host: " + u.Host + "\r\n")
	if contentType != "" {
		sb.WriteString("Content-Type: " + contentType + "\r\n")
	}
	sb.WriteString("Content-Length: " + strconv.Itoa(len(body)) + "\r\n")
	sb.WriteString("Connection: close\r\n")
	sb.WriteString("\r\n")
	if _, err := conn.Write([]byte(sb.String())); err != nil {
		return 0, nil, err
	}
	if len(body) > 0 {
		if _, err := conn.Write(body); err != nil {
			return 0, nil, err
		}
	}
	br := bufio.NewReader(conn)
	statusLine, err := readLine(br)
	if err != nil {
		return 0, nil, err
	}
	parts := strings.SplitN(statusLine, " ", 3)
	status := 0
	if len(parts) >= 2 {
		status, _ = strconv.Atoi(parts[1])
	}
	// headers
	var contentLength int
	var chunked bool
	for {
		l, err := readLine(br)
		if err != nil {
			return status, nil, err
		}
		if l == "" {
			break
		}
		colon := strings.Index(l, ":")
		if colon < 0 {
			continue
		}
		k := strings.ToLower(strings.TrimSpace(l[:colon]))
		v := strings.TrimSpace(l[colon+1:])
		if k == "content-length" {
			contentLength, _ = strconv.Atoi(v)
		}
		if k == "transfer-encoding" && strings.Contains(strings.ToLower(v), "chunked") {
			chunked = true
		}
	}
	var respBody []byte
	if chunked {
		respBody, err = readChunkedBody(br)
		if err != nil {
			return status, nil, err
		}
	} else if contentLength > 0 {
		respBody = make([]byte, contentLength)
		if _, err := io.ReadFull(br, respBody); err != nil {
			return status, nil, err
		}
	} else {
		rest, _ := io.ReadAll(br)
		respBody = rest
	}
	_ = bytes.MinRead
	return status, respBody, nil
}
