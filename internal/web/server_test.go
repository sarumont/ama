package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sarumont/ama/config"
)

// newTestServer builds a server that logs nowhere and listens on an ephemeral
// port.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.Web.Host = "127.0.0.1"
	cfg.Web.Port = 0

	srv, err := New(Options{
		Config: cfg,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

func TestNewRequiresConfig(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("New with no config: want error, got nil")
	}
}

func TestRoutes(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t))
	defer ts.Close()

	tests := []struct {
		name       string
		path       string
		wantStatus int
		wantBody   string
	}{
		{"queue", "/", http.StatusNotImplemented, "Queue"},
		{"confirm", "/confirm/abc123", http.StatusNotImplemented, "Confirm abc123"},
		{"history", "/history", http.StatusNotImplemented, "History"},
		{"status", "/api/status/abc123", http.StatusNotImplemented, "not yet implemented"},
		{"static htmx", "/static/htmx.min.js", http.StatusOK, "htmx"},
		{"unknown", "/nope", http.StatusNotFound, ""},
		{"unknown under root", "/confirm", http.StatusNotFound, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := ts.Client().Get(ts.URL + tt.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tt.path, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantStatus {
				t.Errorf("GET %s: status = %d, want %d", tt.path, resp.StatusCode, tt.wantStatus)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("reading body: %v", err)
			}
			if tt.wantBody != "" && !strings.Contains(string(body), tt.wantBody) {
				t.Errorf("GET %s: body does not contain %q", tt.path, tt.wantBody)
			}
		})
	}
}

// TestViewsRenderTemplate proves the template pipeline runs end to end: the
// rendered page carries the layout and the local HTMX script tag.
func TestViewsRenderTemplate(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	newTestServer(t).ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", got)
	}
	body := rec.Body.String()
	for _, want := range []string{"<!doctype html>", `src="/static/htmx.min.js"`, "<h1>Queue</h1>"} {
		if !strings.Contains(body, want) {
			t.Errorf("body does not contain %q\ngot: %s", want, body)
		}
	}
}

func TestStartShutsDownOnContextCancel(t *testing.T) {
	srv := newTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx) }()

	select {
	case <-srv.Ready():
	case err := <-errCh:
		t.Fatalf("Start returned before listening: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("server never became ready")
	}

	resp, err := http.Get("http://" + srv.Addr() + "/history")
	if err != nil {
		t.Fatalf("GET /history: %v", err)
	}
	resp.Body.Close()

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Start after cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down after context cancellation")
	}

	if _, err := http.Get("http://" + srv.Addr() + "/history"); err == nil {
		t.Error("server still serving after shutdown")
	}
}

func TestStartFailsOnBadBind(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.Web.Host = "203.0.113.1" // TEST-NET-3, not a local address
	srv.cfg.Web.Port = 8080

	if err := srv.Start(context.Background()); err == nil {
		t.Fatal("Start on unbindable address: want error, got nil")
	}
}
