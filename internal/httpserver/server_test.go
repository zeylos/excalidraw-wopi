package httpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"

	"github.com/zeylos/excalidraw-wopi/internal/config"
)

func TestNewSetsReadHeaderTimeout(t *testing.T) {
	cfg := config.Config{ListenAddr: ":0"}
	fsys := fstest.MapFS{"index.html": {Data: []byte("<html></html>")}}

	srv := New(cfg, fsys)

	if srv.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want 10s", srv.ReadHeaderTimeout)
	}
	if srv.ReadTimeout != 30*time.Second {
		t.Errorf("ReadTimeout = %v, want 30s", srv.ReadTimeout)
	}
	if srv.IdleTimeout != 120*time.Second {
		t.Errorf("IdleTimeout = %v, want 120s", srv.IdleTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want unset (websockets need long-lived writes)", srv.WriteTimeout)
	}
}

func TestSPAHandler(t *testing.T) {
	// fstest.MapFS derives the "assets" directory entry from the
	// "assets/app.js" path, so no explicit directory entry is needed.
	fsys := fstest.MapFS{
		"index.html":    {Data: []byte("<html>index</html>")},
		"assets/app.js": {Data: []byte("console.log('app')")},
	}

	handler := newSPAHandler(fsys)

	tests := []struct {
		name       string
		path       string
		wantStatus int
		wantBody   string
	}{
		{
			name:       "root falls back to index.html",
			path:       "/",
			wantStatus: http.StatusOK,
			wantBody:   "<html>index</html>",
		},
		{
			name:       "deep client-side route falls back to index.html",
			path:       "/boards/abc123",
			wantStatus: http.StatusOK,
			wantBody:   "<html>index</html>",
		},
		{
			name:       "missing asset with an extension returns 404",
			path:       "/assets/missing.js",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "real asset is served",
			path:       "/assets/app.js",
			wantStatus: http.StatusOK,
			wantBody:   "console.log('app')",
		},
		{
			name:       "a directory request returns 404",
			path:       "/assets",
			wantStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantBody != "" && rec.Body.String() != tt.wantBody {
				t.Errorf("body = %q, want %q", rec.Body.String(), tt.wantBody)
			}
		})
	}
}
