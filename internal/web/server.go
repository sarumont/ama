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
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
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
	tmpl  *template.Template
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

	tmpl, err := template.ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("web: parsing templates: %w", err)
	}

	s := &Server{
		cfg:   opts.Config,
		log:   logger,
		tmpl:  tmpl,
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

// routes is the single owner of the route table.
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()

	// Views. {$} keeps the root pattern from swallowing unknown paths.
	mux.HandleFunc("GET /{$}", s.handleQueue)
	mux.HandleFunc("GET /confirm/{id}", s.handleConfirm)
	mux.HandleFunc("GET /history", s.handleHistory)

	// HTMX polls this for progress updates.
	mux.HandleFunc("GET /api/status/{id}", s.handleStatus)

	// Vendored assets, served locally so the box can be offline.
	mux.Handle("GET /static/", http.FileServerFS(staticFS))

	return mux
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

// placeholder renders the stand-in page for a view whose handler has not landed
// yet. It exercises the full template pipeline so the plumbing is testable
// ahead of the real handlers.
func (s *Server) placeholder(w http.ResponseWriter, title string) {
	data := struct {
		Title   string
		Message string
	}{Title: title, Message: "Not yet implemented."}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotImplemented)
	if err := s.tmpl.ExecuteTemplate(w, "placeholder.html", data); err != nil {
		// The status line is already written, so this can only be logged.
		s.log.Error("rendering template", "template", "placeholder.html", "err", err)
	}
}

func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	s.placeholder(w, "Queue")
}

func (s *Server) handleConfirm(w http.ResponseWriter, r *http.Request) {
	s.placeholder(w, "Confirm "+r.PathValue("id"))
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	s.placeholder(w, "History")
}

// handleStatus returns an HTMX fragment, not a page, so it skips the template.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "not yet implemented", http.StatusNotImplemented)
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
