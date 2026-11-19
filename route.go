package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
)

const maxBodyBytes = 64 << 20

var errBodyTooLarge = errors.New("body too large")

type App struct {
	Store     *Store
	ShopToken string
	reqSeq    atomic.Uint64
}

func NewApp(store *Store) *App {
	return &App{Store: store, ShopToken: os.Getenv("MAKEFUN_SHOP_TOKEN")}
}

func (a *App) nextRequestID() string {
	return "req-" + strconv.FormatUint(a.reqSeq.Add(1), 10)
}

func (a *App) sendJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-GitHub-Request-Id", a.nextRequestID())
	w.WriteHeader(status)
	if body != nil && status != http.StatusNoContent {
		_, _ = w.Write(body)
	}
}

func (a *App) sendError(w http.ResponseWriter, status int, message string) {
	a.sendJSON(w, status, encodeJSON(struct {
		Message string `json:"message"`
	}{Message: message}))
}

func (a *App) isAuthed(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return false
	}
	token := h[len("Bearer "):]
	if token == "" {
		return false
	}
	if a.ShopToken == "" {
		return true
	}
	return token == a.ShopToken
}

func rawPath(rawURL string) string {
	if i := strings.IndexByte(rawURL, '?'); i >= 0 {
		return rawURL[:i]
	}
	return rawURL
}

func decodeQueryValue(s string) string {
	if !strings.ContainsAny(s, "%+") {
		return s
	}
	s = strings.ReplaceAll(s, "+", " ")
	if v, err := url.PathUnescape(s); err == nil {
		return v
	}
	return s
}

func queryParam(query, name string) string {
	pos := 0
	for pos <= len(query) {
		amp := strings.IndexByte(query[pos:], '&')
		var part string
		if amp < 0 {
			part = query[pos:]
		} else {
			part = query[pos : pos+amp]
		}
		eq := strings.IndexByte(part, '=')
		key := part
		if eq >= 0 {
			key = part[:eq]
		}
		if decodeQueryValue(key) == name {
			if eq < 0 {
				return ""
			}
			return decodeQueryValue(part[eq+1:])
		}
		if amp < 0 {
			break
		}
		pos += amp + 1
	}
	return ""
}

func pathSegments(pathname string) []string {
	out := make([]string, 0, 8)
	start := 0
	for start < len(pathname) {
		end := strings.IndexByte(pathname[start:], '/')
		if end < 0 {
			end = len(pathname)
		} else {
			end += start
		}
		if end > start {
			seg := pathname[start:end]
			if strings.IndexByte(seg, '%') >= 0 {
				seg = decodeURIComponentSafe(seg)
			}
			out = append(out, seg)
		}
		start = end + 1
	}
	return out
}

func decodeURIComponentSafe(s string) string {
	if v, err := url.PathUnescape(s); err == nil {
		return v
	}
	return s
}

func requestHasBody(r *http.Request, method string) bool {
	if method == http.MethodGet || method == http.MethodHead {
		return false
	}
	return r.ContentLength != 0
}

func readBody(r *http.Request, max int64) ([]byte, error) {
	defer r.Body.Close()
	data, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, errBodyTooLarge
	}
	return data, nil
}

func (a *App) readAPIBody(w http.ResponseWriter, r *http.Request, method string) (map[string]any, bool) {
	var data []byte
	if requestHasBody(r, method) {
		var err error
		data, err = readBody(r, maxBodyBytes)
		if err != nil {
			a.sendError(w, http.StatusBadRequest, "Problems parsing JSON")
			return nil, false
		}
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, true
	}
	var parsed any
	if err := json.Unmarshal(data, &parsed); err != nil {
		a.sendError(w, http.StatusBadRequest, "Problems parsing JSON")
		return nil, false
	}
	obj, _ := parsed.(map[string]any)
	return obj, true
}

func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if recover() != nil {
			a.sendError(w, http.StatusInternalServerError, "Internal Server Error")
		}
	}()

	method := strings.ToUpper(r.Method)
	if method == "" {
		method = http.MethodGet
	}
	rawURL := r.RequestURI
	if rawURL == "" {
		rawURL = r.URL.RequestURI()
	}
	pathname := rawPath(rawURL)
	query := ""
	if i := strings.IndexByte(rawURL, '?'); i >= 0 {
		query = rawURL[i+1:]
	}
	segs := pathSegments(pathname)

	if method == http.MethodGet && pathname == "/healthz" {
		a.sendJSON(w, http.StatusOK, []byte(`{"ok":true}`))
		return
	}

	if pathname == "/api/v3" || strings.HasPrefix(pathname, "/api/v3/") {
		if !a.isAuthed(r) {
			a.sendError(w, http.StatusUnauthorized, "Requires authentication")
			return
		}
		body, ok := a.readAPIBody(w, r, method)
		if !ok {
			return
		}
		if !a.RouteAPIV3(w, r, method, pathname, segs, query, body) {
			a.sendError(w, http.StatusNotFound, "Not Found")
		}
		return
	}

	if pathname == "/api/graphql" {
		if !a.isAuthed(r) {
			a.sendError(w, http.StatusUnauthorized, "Requires authentication")
			return
		}
		if method != http.MethodPost {
			a.sendError(w, http.StatusNotFound, "Not Found")
			return
		}
		data, err := readBody(r, maxBodyBytes)
		if err != nil {
			a.sendError(w, http.StatusBadRequest, "Problems parsing JSON")
			return
		}
		var parsed any
		if err := json.Unmarshal(data, &parsed); err != nil {
			a.sendError(w, http.StatusBadRequest, "Problems parsing JSON")
			return
		}
		a.handleGraphQL(w, r, parsed)
		return
	}

	if pathname == "/mcp" || strings.HasPrefix(pathname, "/mcp/") {
		if !a.isAuthed(r) {
			a.sendError(w, http.StatusUnauthorized, "Requires authentication")
			return
		}
		var body []byte
		if method == http.MethodPost {
			if data, err := readBody(r, maxBodyBytes); err == nil {
				body = data
			}
		}
		a.handleMCP(w, r, method, body)
		return
	}

	if len(segs) >= 2 && strings.HasSuffix(segs[1], ".git") {
		var body []byte
		if method == http.MethodPost {
			data, err := readBody(r, maxBodyBytes)
			if err != nil {
				a.sendError(w, http.StatusBadRequest, "Bad request")
				return
			}
			body = data
		}
		a.handleGitHTTP(w, r, method, segs, body, rawURL)
		return
	}

	a.sendError(w, http.StatusNotFound, "Not Found")
}
