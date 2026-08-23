package api

import (
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

type ctxKey int

const (
	ctxRequestID ctxKey = iota
	ctxLogger
)

// RequestID pulls the per-request id out of the context. Every log line and
// every 5xx response can be correlated through it.
func RequestID(ctx context.Context) string {
	if v, ok := ctx.Value(ctxRequestID).(string); ok {
		return v
	}
	return ""
}

// logFor returns the request-scoped logger, falling back to the server logger.
func (s *Server) logFor(r *http.Request) *slog.Logger {
	if l, ok := r.Context().Value(ctxLogger).(*slog.Logger); ok {
		return l
	}
	return s.log
}

type middleware func(http.Handler) http.Handler

// chain applies middleware so that the first argument is the outermost layer.
func chain(h http.Handler, ms ...middleware) http.Handler {
	for i := len(ms) - 1; i >= 0; i-- {
		h = ms[i](h)
	}
	return h
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is not recoverable and not worth a code path;
		// a duplicate correlation id is harmless.
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}

// requestContext attaches an id and a scoped logger. An inbound
// X-Request-ID is honoured so a reverse proxy's id survives into our logs.
func requestContext(log *slog.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get("X-Request-ID")
			if id == "" || len(id) > 64 {
				id = newRequestID()
			}
			rl := log.With("request_id", id)
			ctx := context.WithValue(r.Context(), ctxRequestID, id)
			ctx = context.WithValue(ctx, ctxLogger, rl)
			w.Header().Set("X-Request-ID", id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// statusRecorder captures the status and byte count for the access log while
// forwarding Flush and Unwrap so SSE and http.ResponseController keep working.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written int64
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.written += int64(n)
	return n, err
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the real writer, which SSE needs
// for SetWriteDeadline.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)

		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		log, ok := r.Context().Value(ctxLogger).(*slog.Logger)
		if !ok {
			return
		}
		level := slog.LevelInfo
		switch {
		case rec.status >= 500:
			level = slog.LevelError
		case rec.status >= 400:
			level = slog.LevelWarn
		case r.URL.Path == pathPing:
			// Container health checks would otherwise dominate the log.
			level = slog.LevelDebug
		}
		log.Log(r.Context(), level, "http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.written,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// recoverPanic turns a handler panic into a 500 rather than killing the
// process. The stack goes to the log, never to the client.
func recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			p := recover()
			if p == nil {
				return
			}
			// A client disconnect mid-write surfaces as this sentinel; it is
			// not a bug and should not be logged as one.
			if p == http.ErrAbortHandler {
				panic(p)
			}
			log, ok := r.Context().Value(ctxLogger).(*slog.Logger)
			if ok {
				log.Error("handler panic",
					"panic", p,
					"path", r.URL.Path,
					"stack", string(debug.Stack()))
			}
			writeJSON(w, orDiscard(log, ok), http.StatusInternalServerError,
				errorEnvelope{Error: &Error{Code: CodeInternal, Message: "internal error"}})
		}()
		next.ServeHTTP(w, r)
	})
}

func orDiscard(l *slog.Logger, ok bool) *slog.Logger {
	if ok {
		return l
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// bearerAuth enforces the static token on API data and command routes except
// the liveness probe. Static UI assets contain no operator data and remain
// reachable so the browser can collect a token. Comparison is constant time.
func bearerAuth(token string) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The SPA shell contains no operator data and must be reachable before
			// the browser can present the token stored from its Settings view.
			// Every data and command route remains under /api and authenticated.
			if token == "" || r.URL.Path == pathPing || !strings.HasPrefix(r.URL.Path, "/api/") {
				next.ServeHTTP(w, r)
				return
			}
			// CORS preflight carries no Authorization header by design.
			if r.Method == http.MethodOptions {
				next.ServeHTTP(w, r)
				return
			}

			presented := presentedToken(r)
			if presented == "" {
				w.Header().Set("WWW-Authenticate", `Bearer realm="reelay"`)
				writeError(w, loggerFrom(r), Unauthorized("missing bearer token"))
				return
			}
			if subtle.ConstantTimeCompare([]byte(presented), []byte(token)) != 1 {
				writeError(w, loggerFrom(r), Unauthorized("invalid bearer token"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// presentedToken accepts the standard Authorization header, and an
// X-Api-Key header because that is what curl one-liners and the SSE
// EventSource API (which cannot set Authorization) can actually send.
func presentedToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if after, ok := cutPrefixFold(h, "bearer "); ok {
			return strings.TrimSpace(after)
		}
	}
	if r.URL.Path == pathEvents {
		return strings.TrimSpace(r.URL.Query().Get("token"))
	}
	return strings.TrimSpace(r.Header.Get("X-Api-Key"))
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return "", false
	}
	return s[len(prefix):], true
}

func loggerFrom(r *http.Request) *slog.Logger {
	if l, ok := r.Context().Value(ctxLogger).(*slog.Logger); ok {
		return l
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// cors allows only the configured origins. An empty list means same-origin
// only, which is the correct default when Reelay serves its own UI.
func cors(origins []string) middleware {
	allowed := make(map[string]bool, len(origins))
	for _, o := range origins {
		allowed[strings.TrimRight(o, "/")] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := strings.TrimRight(r.Header.Get("Origin"), "/")
			if origin != "" && allowed[origin] {
				h := w.Header()
				h.Set("Access-Control-Allow-Origin", origin)
				h.Set("Access-Control-Allow-Credentials", "true")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Api-Key, X-Request-ID")
				h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
				h.Set("Access-Control-Max-Age", "600")
				h.Add("Vary", "Origin")
			}
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

type gzipWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	wroteHeader bool
}

func (g *gzipWriter) WriteHeader(code int) {
	if g.wroteHeader {
		return
	}
	g.wroteHeader = true
	h := g.Header()
	// Content-Length would describe the uncompressed body.
	h.Del("Content-Length")
	h.Set("Content-Encoding", "gzip")
	h.Add("Vary", "Accept-Encoding")
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipWriter) Write(b []byte) (int, error) {
	if !g.wroteHeader {
		g.WriteHeader(http.StatusOK)
	}
	return g.gz.Write(b)
}

func (g *gzipWriter) Flush() {
	_ = g.gz.Flush()
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (g *gzipWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }

// compress gzips responses for clients that ask for it.
//
// SSE is excluded: buffering an event stream defeats its purpose, and gzip's
// window means a subscriber can wait indefinitely for bytes we already wrote.
func compress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") || r.URL.Path == pathEvents {
			next.ServeHTTP(w, r)
			return
		}
		gz := gzip.NewWriter(w)
		defer func() { _ = gz.Close() }()
		next.ServeHTTP(&gzipWriter{ResponseWriter: w, gz: gz}, r)
	})
}

// securityHeaders are cheap and stop the embedded UI from being framed by
// something else on the LAN.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
