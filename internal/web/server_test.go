package web

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
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
			defer func() { _ = resp.Body.Close() }()

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

// TestLayoutZeroValue proves the layout renders with no template execution
// error when handed a completely zero-value view model — the AC for #24 —
// and that a populated CurrentPath marks the matching nav link active.
func TestLayoutZeroValue(t *testing.T) {
	srv := newTestServer(t)
	tmpl := srv.pages["placeholder.html"]

	type viewModel struct {
		Title       string
		Message     string
		CurrentPath string
	}

	t.Run("zero value", func(t *testing.T) {
		var buf bytes.Buffer
		if err := tmpl.ExecuteTemplate(&buf, "layout", viewModel{}); err != nil {
			t.Fatalf("ExecuteTemplate with zero-value data: %v", err)
		}
		body := buf.String()
		if strings.Contains(body, `class="active"`) {
			t.Errorf("zero-value CurrentPath should mark no nav link active, got: %s", body)
		}
	})

	t.Run("active nav", func(t *testing.T) {
		var buf bytes.Buffer
		data := viewModel{Title: "Queue", CurrentPath: "/"}
		if err := tmpl.ExecuteTemplate(&buf, "layout", data); err != nil {
			t.Fatalf("ExecuteTemplate: %v", err)
		}
		if !strings.Contains(buf.String(), `<a href="/" class="active"`) {
			t.Errorf("CurrentPath=/ should mark the Queue link active, got: %s", buf.String())
		}
	})
}

// TestStatusBadgePartial covers the six manifest statuses from
// docs/MANIFEST.md plus the zero-value (empty string) case, which must
// render an "Unknown" badge rather than erroring.
func TestStatusBadgePartial(t *testing.T) {
	srv := newTestServer(t)
	tmpl := srv.pages["placeholder.html"]

	tests := []struct {
		status string
		want   string
	}{
		{"pending_confirmation", "badge-pending-confirmation"},
		{"ripping", "badge-ripping"},
		{"analyzing", "badge-analyzing"},
		{"pending_ocr", "badge-pending-ocr"},
		{"complete", "badge-complete"},
		{"error", "badge-error"},
		{"", "badge-unknown"},
		{"some-future-status", "badge-unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			var buf bytes.Buffer
			if err := tmpl.ExecuteTemplate(&buf, "status-badge", tt.status); err != nil {
				t.Fatalf("ExecuteTemplate(status-badge, %q): %v", tt.status, err)
			}
			if !strings.Contains(buf.String(), tt.want) {
				t.Errorf("status-badge(%q) = %q, want to contain %q", tt.status, buf.String(), tt.want)
			}
		})
	}
}

// TestWarningsPartial proves the shared warnings/errors partial renders with
// no error given a zero-value (nil-slice) manifest and given populated data.
func TestWarningsPartial(t *testing.T) {
	srv := newTestServer(t)
	tmpl := srv.pages["placeholder.html"]

	type manifest struct {
		Warnings []string
		Errors   []string
	}

	t.Run("zero value", func(t *testing.T) {
		var buf bytes.Buffer
		if err := tmpl.ExecuteTemplate(&buf, "warnings", manifest{}); err != nil {
			t.Fatalf("ExecuteTemplate with zero-value manifest: %v", err)
		}
		if strings.Contains(buf.String(), "<li") {
			t.Errorf("zero-value manifest should render no list items, got: %s", buf.String())
		}
	})

	t.Run("populated", func(t *testing.T) {
		var buf bytes.Buffer
		data := manifest{Warnings: []string{"disc label unreadable"}, Errors: []string{"tmdb lookup failed"}}
		if err := tmpl.ExecuteTemplate(&buf, "warnings", data); err != nil {
			t.Fatalf("ExecuteTemplate: %v", err)
		}
		body := buf.String()
		if !strings.Contains(body, "disc label unreadable") || !strings.Contains(body, "tmdb lookup failed") {
			t.Errorf("body missing warning/error text: %s", body)
		}
	})
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
	_ = resp.Body.Close()

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
	// Hold a port so the server is guaranteed to fail binding it, rather than
	// relying on binding a non-local address, which is not reliably rejected
	// (e.g. with net.ipv4.ip_nonlocal_bind=1, or inside some network
	// namespaces / CI runners).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	srv := newTestServer(t)
	srv.cfg.Web.Host = "127.0.0.1"
	srv.cfg.Web.Port = ln.Addr().(*net.TCPAddr).Port

	if err := srv.Start(context.Background()); err == nil {
		t.Fatal("Start on unbindable address: want error, got nil")
	}

	select {
	case <-srv.Ready():
	default:
		t.Error("Ready() not closed after failed bind")
	}
}
