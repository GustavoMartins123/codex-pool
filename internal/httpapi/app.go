package httpapi

import (
	"context"
	"net/http"
	"net/url"
	"path"
	"strings"
)

const RequestIDHeader = "X-Pool-Request-Id"

// Check writes the rejection response and returns false when access is denied.
type Check func(http.ResponseWriter, *http.Request) bool

// App owns the HTTP entry point and its routes. Register routes before serving.
type App struct {
	mux             *http.ServeMux
	securityHeaders http.HandlerFunc
	allowClient     Check
	newRequestID    func() string
	gateway         http.HandlerFunc
}

func New(securityHeaders http.HandlerFunc, allowClient Check, newRequestID func() string, gateway http.HandlerFunc) *App {
	app := &App{
		mux:             http.NewServeMux(),
		securityHeaders: securityHeaders,
		allowClient:     allowClient,
		newRequestID:    newRequestID,
		gateway:         gateway,
	}
	app.HandleFunc("/", gateway)
	return app
}

func (app *App) HandleFunc(pattern string, handler http.HandlerFunc) {
	app.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if originalURL, ok := r.Context().Value(originalURLKey{}).(*url.URL); ok {
			r.URL = originalURL
		}
		handler(w, r)
	})
}

func (app *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	app.securityHeaders(w, r)
	if !app.allowClient(w, r) {
		return
	}
	requestID := strings.TrimSpace(r.Header.Get("X-Request-ID"))
	if requestID == "" {
		requestID = app.newRequestID()
	}
	w.Header().Set(RequestIDHeader, requestID)

	if !strings.HasPrefix(r.URL.Path, "/") {
		app.gateway(w, r)
		return
	}
	app.mux.ServeHTTP(w, routingRequest(r))
}

func Require(check Check, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if check(w, r) {
			next(w, r)
		}
	}
}

func OnlyMethod(method string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		next(w, r)
	}
}

type originalURLKey struct{}

// routingRequest matches the decoded URL.Path without ServeMux's automatic
// path cleaning. Handlers receive the original URL, including its query and
// escaping, so the path inspected at ingress is also the path they serve.
func routingRequest(r *http.Request) *http.Request {
	p := r.URL.Path
	if r.URL.RawPath == "" && (p == "/" || path.Clean(p) == strings.TrimSuffix(p, "/")) {
		return r
	}

	segments := strings.Split(p[1:], "/")
	var escaped, decoded strings.Builder
	for i, segment := range segments {
		escaped.WriteByte('/')
		decoded.WriteByte('/')
		switch {
		case segment == "" && i < len(segments)-1:
			// Two slashes within a mux segment represent an empty segment in
			// the decoded path, where a segment cannot itself contain '/'.
			escaped.WriteString("%2F%2F")
			decoded.WriteString("//")
		case segment == "." || segment == "..":
			escaped.WriteString(strings.Repeat("%2E", len(segment)))
			decoded.WriteString(segment)
		default:
			escaped.WriteString(url.PathEscape(segment))
			decoded.WriteString(segment)
		}
	}

	lookupURL := *r.URL
	lookupURL.Path = decoded.String()
	lookupURL.RawPath = escaped.String()
	lookupRequest := r.WithContext(context.WithValue(r.Context(), originalURLKey{}, r.URL))
	lookupRequest.URL = &lookupURL
	return lookupRequest
}

// PathSegment reads a single named path segment, including an empty segment.
func PathSegment(r *http.Request, name string) string {
	value := r.PathValue(name)
	if value == "//" {
		return ""
	}
	return value
}
