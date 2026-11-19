package main

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	rawMaxHeaderBytes = 64 << 10
	rawMaxBodyBytes   = 64 << 20
	rawReadBufSize    = 16 << 10
	rawWriteBufSize   = 32 << 10
	rawCloseTimeout   = 2 * time.Second
)

var (
	errRawBadRequest = errors.New("bad request")
	errRawTooLarge   = errors.New("payload too large")
	errRawLineLong   = errors.New("header line too long")
)

var rawStatusText = map[int]string{
	200: "OK",
	201: "Created",
	204: "No Content",
	301: "Moved Permanently",
	302: "Found",
	304: "Not Modified",
	400: "Bad Request",
	401: "Unauthorized",
	403: "Forbidden",
	404: "Not Found",
	405: "Method Not Allowed",
	409: "Conflict",
	413: "Payload Too Large",
	422: "Unprocessable Entity",
	500: "Internal Server Error",
	501: "Not Implemented",
	503: "Service Unavailable",
}

func rawReason(status int) string {
	if text, ok := rawStatusText[status]; ok {
		return text
	}
	return "OK"
}

type rawServer struct {
	ln      net.Listener
	app     *App
	wg      sync.WaitGroup
	closing atomic.Bool
}

func newRawServer(ln net.Listener, app *App) *rawServer {
	return &rawServer{ln: ln, app: app}
}

func (s *rawServer) Serve() error {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if s.closing.Load() {
				return nil
			}
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			return err
		}
		s.wg.Add(1)
		go func(c net.Conn) {
			defer s.wg.Done()
			defer func() { _ = recover() }()
			s.serveConn(c)
		}(conn)
	}
}

func (s *rawServer) Close() error {
	if s.closing.CompareAndSwap(false, true) {
		_ = s.ln.Close()
	}
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(rawCloseTimeout):
	}
	return nil
}

type rawParsedRequest struct {
	req       *http.Request
	keepAlive bool
}

type connState struct {
	conn       net.Conn
	br         *bufio.Reader
	bw         *bufio.Writer
	remoteAddr string
	line       []byte
	body       []byte
	out        []byte
	keys       []string
	reqHeader  http.Header
	respHeader http.Header
	req        *http.Request
	url        *url.URL
	chunkedTE  []string
	rw         rawResponseWriter
}

func newConnState(conn net.Conn) *connState {
	cs := &connState{
		conn:       conn,
		br:         bufio.NewReaderSize(conn, rawReadBufSize),
		bw:         bufio.NewWriterSize(conn, rawWriteBufSize),
		remoteAddr: conn.RemoteAddr().String(),
		line:       make([]byte, 0, 512),
		body:       make([]byte, 0, 4096),
		out:        make([]byte, 0, 4096),
		keys:       make([]string, 0, 8),
		reqHeader:  make(http.Header, 12),
		respHeader: make(http.Header, 8),
		url:        &url.URL{},
		chunkedTE:  []string{"chunked"},
	}
	cs.req = &http.Request{URL: cs.url, Header: cs.reqHeader}
	cs.rw.header = cs.respHeader
	return cs
}

func (s *rawServer) serveConn(conn net.Conn) {
	defer conn.Close()
	cs := newConnState(conn)
	for {
		parsed, err := cs.readRequest()
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, errRawTooLarge) {
				status = http.StatusRequestEntityTooLarge
			}
			cs.writeError(status)
			return
		}
		keepAlive := parsed.keepAlive && !s.closing.Load()
		s.runHandler(cs, parsed.req, keepAlive)
		if !keepAlive {
			return
		}
	}
}

func (s *rawServer) runHandler(cs *connState, req *http.Request, keepAlive bool) {
	rw := &cs.rw
	rw.reset(cs.respHeader)
	func() {
		defer func() {
			if recover() != nil {
				rw.reset(cs.respHeader)
				rw.WriteHeader(http.StatusInternalServerError)
				_, _ = rw.Write([]byte("Internal Server Error"))
			}
		}()
		s.app.ServeHTTP(rw, req)
	}()
	cs.writeResponse(rw, strings.EqualFold(req.Method, http.MethodHead), keepAlive)
}

func readRawLine(br *bufio.Reader, scratch *[]byte, limit int) ([]byte, error) {
	line := (*scratch)[:0]
	total := 0
	for {
		chunk, err := br.ReadSlice('\n')
		total += len(chunk)
		if total > limit {
			*scratch = line
			return nil, errRawLineLong
		}
		line = append(line, chunk...)
		if err == nil {
			break
		}
		if err != bufio.ErrBufferFull {
			*scratch = line
			return nil, err
		}
	}
	*scratch = line
	if len(line) > 0 && line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
	}
	return line, nil
}

func parseRawRequestLine(line []byte) ([]byte, []byte, []byte, bool) {
	first := bytes.IndexByte(line, ' ')
	if first <= 0 {
		return nil, nil, nil, false
	}
	rest := line[first+1:]
	second := bytes.IndexByte(rest, ' ')
	if second <= 0 {
		return nil, nil, nil, false
	}
	method := line[:first]
	target := rest[:second]
	proto := rest[second+1:]
	if len(target) == 0 || len(proto) != 8 {
		return nil, nil, nil, false
	}
	if !bytes.Equal(proto[:7], []byte("HTTP/1.")) || (proto[7] != '0' && proto[7] != '1') {
		return nil, nil, nil, false
	}
	for _, c := range method {
		if c <= ' ' || c == 0x7f {
			return nil, nil, nil, false
		}
	}
	for _, c := range target {
		if c <= ' ' || c == 0x7f {
			return nil, nil, nil, false
		}
	}
	return method, target, proto, true
}

func validRawHeaderName(name []byte) bool {
	if len(name) == 0 {
		return false
	}
	const separators = "()<>@,;:\\\"/[]?={}"
	for _, c := range name {
		if c <= ' ' || c >= 0x7f || strings.IndexByte(separators, c) >= 0 {
			return false
		}
	}
	return true
}

func rawHeaderHasToken(h http.Header, name, token string) bool {
	for _, value := range h.Values(name) {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

func reserveRawBytes(b []byte, n int) []byte {
	if cap(b) >= n {
		return b
	}
	size := n
	if doubled := cap(b) * 2; doubled > size {
		size = doubled
	}
	next := make([]byte, len(b), size)
	copy(next, b)
	return next
}

func (cs *connState) readRequest() (*rawParsedRequest, error) {
	h := cs.reqHeader
	for k := range h {
		delete(h, k)
	}

	headerBytes := 0
	line, err := readRawLine(cs.br, &cs.line, rawMaxHeaderBytes)
	if err != nil {
		if errors.Is(err, errRawLineLong) {
			return nil, errRawBadRequest
		}
		return nil, err
	}
	headerBytes += len(line) + 2
	methodBytes, targetBytes, protoBytes, ok := parseRawRequestLine(line)
	if !ok {
		return nil, errRawBadRequest
	}
	method := string(methodBytes)
	target := string(targetBytes)
	proto := string(protoBytes)

	for {
		line, err = readRawLine(cs.br, &cs.line, rawMaxHeaderBytes)
		if err != nil {
			if errors.Is(err, errRawLineLong) {
				return nil, errRawBadRequest
			}
			return nil, err
		}
		if len(line) == 0 {
			break
		}
		headerBytes += len(line) + 2
		if headerBytes > rawMaxHeaderBytes {
			return nil, errRawBadRequest
		}
		colon := bytes.IndexByte(line, ':')
		if colon <= 0 {
			return nil, errRawBadRequest
		}
		name := bytes.TrimSpace(line[:colon])
		value := bytes.TrimSpace(line[colon+1:])
		if !validRawHeaderName(name) {
			return nil, errRawBadRequest
		}
		h.Add(string(name), string(value))
	}

	path, query := target, ""
	if q := strings.IndexByte(target, '?'); q >= 0 {
		path, query = target[:q], target[q+1:]
	}

	body, contentLength, chunked, err := cs.readRequestBody(h)
	if err != nil {
		return nil, err
	}

	req := cs.req
	req.Method = method
	req.Proto = "HTTP/1.1"
	req.ProtoMajor = 1
	req.ProtoMinor = 1
	req.Header = h
	req.Host = h.Get("Host")
	h.Del("Host")
	req.RequestURI = target
	req.RemoteAddr = cs.remoteAddr
	req.ContentLength = contentLength
	if chunked {
		req.TransferEncoding = cs.chunkedTE
	} else {
		req.TransferEncoding = nil
	}
	if len(body) == 0 {
		req.Body = http.NoBody
	} else {
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	cs.url.Scheme = ""
	cs.url.Host = ""
	cs.url.Path = path
	cs.url.RawPath = ""
	cs.url.RawQuery = query
	cs.url.Fragment = ""
	cs.url.ForceQuery = false
	req.URL = cs.url

	keepAlive := proto == "HTTP/1.1"
	if rawHeaderHasToken(h, "Connection", "close") {
		keepAlive = false
	} else if rawHeaderHasToken(h, "Connection", "keep-alive") {
		keepAlive = true
	}
	return &rawParsedRequest{req: req, keepAlive: keepAlive}, nil
}

func (cs *connState) readRequestBody(h http.Header) ([]byte, int64, bool, error) {
	if rawHeaderHasToken(h, "Transfer-Encoding", "chunked") {
		body, err := cs.readChunkedBody()
		if err != nil {
			return nil, 0, true, err
		}
		return body, -1, true, nil
	}

	rawLength := h.Get("Content-Length")
	if rawLength == "" {
		cs.body = cs.body[:0]
		return cs.body, 0, false, nil
	}
	length, err := strconv.ParseInt(strings.TrimSpace(rawLength), 10, 64)
	if err != nil || length < 0 {
		return nil, 0, false, errRawBadRequest
	}
	if length > rawMaxBodyBytes {
		return nil, 0, false, errRawTooLarge
	}
	n := int(length)
	cs.body = reserveRawBytes(cs.body, n)[:n]
	if _, err := io.ReadFull(cs.br, cs.body); err != nil {
		return nil, 0, false, errRawBadRequest
	}
	return cs.body, length, false, nil
}

func (cs *connState) readChunkedBody() ([]byte, error) {
	cs.body = cs.body[:0]
	for {
		line, err := readRawLine(cs.br, &cs.line, rawMaxHeaderBytes)
		if err != nil {
			return nil, errRawBadRequest
		}
		if len(line) == 0 {
			return nil, errRawBadRequest
		}
		sizePart := line
		if semi := bytes.IndexByte(line, ';'); semi >= 0 {
			sizePart = line[:semi]
		}
		sizePart = bytes.TrimSpace(sizePart)
		if len(sizePart) == 0 {
			return nil, errRawBadRequest
		}
		size, err := strconv.ParseInt(string(sizePart), 16, 64)
		if err != nil || size < 0 {
			return nil, errRawBadRequest
		}
		if size == 0 {
			trailers := 0
			for {
				line, err = readRawLine(cs.br, &cs.line, rawMaxHeaderBytes)
				if err != nil {
					return nil, errRawBadRequest
				}
				trailers += len(line) + 2
				if trailers > rawMaxHeaderBytes {
					return nil, errRawBadRequest
				}
				if len(line) == 0 {
					break
				}
			}
			return cs.body, nil
		}
		if size > rawMaxBodyBytes-int64(len(cs.body)) {
			return nil, errRawTooLarge
		}
		oldLen := len(cs.body)
		need := oldLen + int(size)
		cs.body = reserveRawBytes(cs.body, need)
		cs.body = cs.body[:need]
		if _, err := io.ReadFull(cs.br, cs.body[oldLen:]); err != nil {
			return nil, errRawBadRequest
		}
		var crlf [2]byte
		if _, err := io.ReadFull(cs.br, crlf[:]); err != nil {
			return nil, errRawBadRequest
		}
		if crlf[0] != '\r' || crlf[1] != '\n' {
			return nil, errRawBadRequest
		}
	}
}

type rawResponseWriter struct {
	header      http.Header
	status      int
	body        bytes.Buffer
	wroteHeader bool
	flushed     bool
}

func (w *rawResponseWriter) reset(h http.Header) {
	for k := range w.header {
		delete(w.header, k)
	}
	w.header = h
	w.status = 0
	w.body.Reset()
	w.wroteHeader = false
	w.flushed = false
}

func (w *rawResponseWriter) Header() http.Header {
	return w.header
}

func (w *rawResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	if status < 100 || status > 999 {
		status = http.StatusOK
	}
	w.status = status
	w.wroteHeader = true
}

func (w *rawResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.body.Write(p)
}

func (w *rawResponseWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	w.flushed = true
}

func (cs *connState) writeResponse(rw *rawResponseWriter, head, keepAlive bool) {
	status := rw.status
	if status == 0 {
		status = http.StatusOK
	}
	h := rw.header
	cs.keys = cs.keys[:0]
	out := reserveRawBytes(cs.out[:0], 256+rw.body.Len())
	for key := range h {
		if key == "Content-Length" || key == "Connection" || key == "Transfer-Encoding" {
			continue
		}
		cs.keys = append(cs.keys, key)
	}
	sort.Strings(cs.keys)

	out = append(out, "HTTP/1.1 "...)
	out = strconv.AppendInt(out, int64(status), 10)
	out = append(out, ' ')
	out = append(out, rawReason(status)...)
	out = append(out, "\r\n"...)
	for _, key := range cs.keys {
		for _, value := range h[key] {
			out = append(out, key...)
			out = append(out, ": "...)
			for i := 0; i < len(value); i++ {
				c := value[i]
				if c == '\r' || c == '\n' {
					c = ' '
				}
				out = append(out, c)
			}
			out = append(out, "\r\n"...)
		}
	}
	out = append(out, "Content-Length: "...)
	out = strconv.AppendInt(out, int64(rw.body.Len()), 10)
	out = append(out, "\r\nConnection: "...)
	if keepAlive {
		out = append(out, "keep-alive\r\n\r\n"...)
	} else {
		out = append(out, "close\r\n\r\n"...)
	}
	if !head && rw.body.Len() > 0 {
		out = append(out, rw.body.Bytes()...)
	}
	cs.out = out
	_, _ = cs.bw.Write(out)
	_ = cs.bw.Flush()
}

func (cs *connState) writeError(status int) {
	reason := rawReason(status)
	out := cs.out[:0]
	out = append(out, "HTTP/1.1 "...)
	out = strconv.AppendInt(out, int64(status), 10)
	out = append(out, ' ')
	out = append(out, reason...)
	out = append(out, "\r\nContent-Length: "...)
	out = strconv.AppendInt(out, int64(len(reason)), 10)
	out = append(out, "\r\nConnection: close\r\n\r\n"...)
	out = append(out, reason...)
	cs.out = out
	_, _ = cs.bw.Write(out)
	_ = cs.bw.Flush()
}
