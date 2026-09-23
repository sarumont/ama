// Package web serves AMA's HTTP interface: the queue, confirm, and history
// views plus the status endpoint HTMX polls for progress updates.
//
// This package owns the HTTP plumbing only — listener, route table, template
// rendering, static assets, request logging, and graceful shutdown. Templates
// and static files are embedded so the binary is self-contained in the
// container image.
//
// There is no authentication layer by design: AMA is expected to run on a
// trusted LAN or behind a reverse proxy that authenticates for it.
package web

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/sarumont/ama/config"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Server timeouts. Generous enough for a slow LAN client, short enough that a
// stuck connection cannot pin a goroutine for the length of a rip.
const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 15 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 60 * time.Second
	shutdownTimeout   = 10 * time.Second
)

// Options carries everything the server and its handlers need. Handlers
// currently need nothing but the config; data accessors (manifest store,
// pipeline state) are added here as the handlers that read them land.
type Options struct {
	// Config is the full AMA config. Required.
	Config *config.Config
	// Logger receives request logs. Defaults to slog.Default().
	Logger *slog.Logger
}

// Server is the AMA web server.
type Server struct {
	cfg   *config.Config
	log   *slog.Logger
	pages map[string]*template.Template
	mux   *http.ServeMux
	http  *http.Server
	ready chan struct{}
	addr  string
}

// New builds a server from opts. Templates are parsed here rather than on first
// request, so a broken template fails startup loudly.
func New(opts Options) (*Server, error) {
	if opts.Config == nil {
		return nil, errors.New("web: Options.Config is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	pages, err := loadTemplates(templateFS)
	if err != nil {
		return nil, fmt.Errorf("web: parsing templates: %w", err)
	}

	s := &Server{
		cfg:   opts.Config,
		log:   logger,
		pages: pages,
		ready: make(chan struct{}),
	}
	s.mux = s.routes()
	s.http = &http.Server{
		Handler:           s.logRequests(s.mux),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}
	return s, nil
}

// loadTemplates parses templates/layout.html as a base, then parses each
// other templates/*.html file into its own clone of that base, keyed by
// filename. Each page therefore gets its own independent "title"/"content"
// definitions rather than sharing one *template.Template — see the comment
// atop layout.html for why that isolation matters.
func loadTemplates(fsys embed.FS) (map[string]*template.Template, error) {
	base, err := template.New("layout.html").ParseFS(fsys, "templates/layout.html")
	if err != nil {
		return nil, fmt.Errorf("parsing layout.html: %w", err)
	}

	matches, err := fs.Glob(fsys, "templates/*.html")
	if err != nil {
		return nil, err
	}

	pages := make(map[string]*template.Template)
	for _, m := range matches {
		name := path.Base(m)
		if name == "layout.html" {
			continue
		}
		page, err := template.Must(base.Clone()).ParseFS(fsys, m)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", name, err)
		}
		pages[name] = page
	}
	return pages, nil
}

// routes is the single owner of the route table.
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()

	// Views. {$} keeps the root pattern from swallowing unknown paths.
	mux.HandleFunc("GET /{$}", s.handleQueue)
	mux.HandleFunc("GET /confirm/{id}", s.handleConfirm)
	mux.HandleFunc("GET /history", s.handleHistory)

	// HTMX polls this for progress updates.
	mux.HandleFunc("GET /api/status/{id}", s.handleStatus)

	// User actions (#23) — the only writes docs/CLAUDE.md's "Web UI is
	// read-mostly" allows: confirm disc ID, override a track's role, set a
	// forced subtitle track.
	mux.HandleFunc("POST /confirm/{id}", s.handleConfirmSubmit)
	mux.HandleFunc("POST /api/tracks/{id}/{index}/role", s.handleTrackRole)
	mux.HandleFunc("POST /api/subtitles/{id}/{stream_index}/forced", s.handleSubtitleForced)

	// Vendored assets, served locally so the box can be offline.
	mux.Handle("GET /static/", staticHandler())

	return mux
}

// staticHandler serves embedded static assets. It rejects paths ending in
// "/" so a missing filename 404s instead of returning a directory listing,
// and it sets a long-lived Cache-Control header since assets are immutable
// per build (embed.FS reports a zero ModTime, so there is no Last-Modified
// or ETag for the browser to validate against otherwise).
func staticHandler() http.Handler {
	fileServer := http.FileServerFS(staticFS)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		fileServer.ServeHTTP(w, r)
	})
}

// ServeHTTP lets the server be mounted directly, which tests rely on.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.http.Handler.ServeHTTP(w, r)
}

// Start binds the configured address and serves until ctx is cancelled, then
// shuts down gracefully. A bind failure is returned rather than logged, so the
// daemon cannot silently run headless.
func (s *Server) Start(ctx context.Context) error {
	addr := net.JoinHostPort(s.cfg.Web.Host, fmt.Sprint(s.cfg.Web.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		close(s.ready)
		return fmt.Errorf("web: listening on %s: %w", addr, err)
	}
	s.addr = ln.Addr().String()
	close(s.ready)
	s.log.Info("web server listening", "addr", s.addr)

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.http.Serve(ln)
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("web: serving: %w", err)
	case <-ctx.Done():
		return s.Shutdown(context.WithoutCancel(ctx))
	}
}

// Shutdown stops the server, waiting up to shutdownTimeout for in-flight
// requests to finish.
func (s *Server) Shutdown(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, shutdownTimeout)
	defer cancel()
	if err := s.http.Shutdown(ctx); err != nil {
		return fmt.Errorf("web: shutting down: %w", err)
	}
	return nil
}

// Addr is the bound listen address, available once Ready is closed. It differs
// from the configured address when the config asks for port 0.
func (s *Server) Addr() string { return s.addr }

// Ready is closed once the server is listening.
func (s *Server) Ready() <-chan struct{} { return s.ready }

// render executes page's "layout" template — which pulls in that page's own
// "title"/"content" definitions, see layout.html — and writes the result
// with status, or a 500 if execution fails.
func (s *Server) render(w http.ResponseWriter, status int, page string, data any) {
	tmpl, ok := s.pages[page]
	if !ok {
		s.log.Error("rendering template", "template", page, "err", "no such page")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "layout", data); err != nil {
		s.log.Error("rendering template", "template", page, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

// renderError renders a small full-page message through placeholder.html —
// "Not Found" for an unknown manifest id, or a generic failure notice for an
// internal error — so a handler failure or a bad :id in the URL produces a
// normal styled page instead of a bare http.Error string or a panic.
// handleQueue, handleConfirm and handleHistory (handlers.go) are its callers;
// handleStatus does not use it, since /api/status/:id returns a bare HTML
// fragment for hx-swap rather than a full page.
func (s *Server) renderError(w http.ResponseWriter, r *http.Request, status int, title, message string) {
	data := struct {
		Title       string
		Message     string
		CurrentPath string
	}{Title: title, Message: message, CurrentPath: r.URL.Path}

	s.render(w, status, "placeholder.html", data)
}

// statusRecorder captures the response status for request logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// Unwrap lets http.ResponseController reach the underlying writer, so
// handlers keep Flush/Hijack/ReadFrom through this middleware.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration", time.Since(start),
		)
	})
}
