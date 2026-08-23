package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizePath(t *testing.T) {
	token := "/api/hassio_ingress/w0_GdqL-u1adx1DWInbRyUcIj8xAt_uki3nFFeIhhnU"

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"root", "/", "/"},
		{"empty", "", "/"},
		{"double slash from ingress_entry", "//", "/"},
		{"triple slash", "///", "/"},
		{"cgate", "/cgate", "/cgate"},
		{"ws", "/ws", "/ws"},
		{"health", "/health", "/health"},
		{"cgate with duplicate slashes", "//cgate", "/cgate"},

		// Supervisor normally strips the session prefix, but must not break
		// us if it stops doing so.
		{"prefix passed through, root", token + "/", "/"},
		{"prefix passed through, doubled", token + "//", "/"},
		{"prefix passed through, bare", token, "/"},
		{"prefix passed through, cgate", token + "/cgate", "/cgate"},
		{"prefix passed through, ws", token + "/ws", "/ws"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := normalizePath(c.in); got != c.want {
				t.Errorf("normalizePath(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// The regression this guards: http.ServeMux answers 301 to the cleaned path
// when the request path is not already clean. Under ingress the request
// arrives as "//", so that redirect sent the panel iframe to "/" on the Home
// Assistant origin and rendered the HA dashboard inside the add-on panel.
func TestIngressPathsServeConsoleNotRedirect(t *testing.T) {
	handler := route(http.NotFoundHandler())

	for _, path := range []string{"/", "//", "///", "/anything"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler(rec, httptest.NewRequest(http.MethodGet, "http://addon:8980"+path, nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("GET %q = %d, want 200 (a redirect here breaks ingress)", path, rec.Code)
			}
			if loc := rec.Header().Get("Location"); loc != "" {
				t.Fatalf("GET %q set Location: %q, want no redirect", path, loc)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
				t.Errorf("GET %q Content-Type = %q, want text/html", path, ct)
			}
			if !strings.Contains(rec.Body.String(), "<html") {
				t.Errorf("GET %q did not return the console page", path)
			}
		})
	}
}

func TestRouteDispatch(t *testing.T) {
	wsHit := false
	ws := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { wsHit = true })
	handler := route(ws)

	t.Run("health", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodGet, "http://addon:8980/health", nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok"`) {
			t.Errorf("health = %d %q", rec.Code, rec.Body.String())
		}
	})

	t.Run("ws routed to websocket handler", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodGet, "http://addon:8980//ws", nil))
		if !wsHit {
			t.Error("doubled /ws path did not reach the websocket handler")
		}
	})

	// cmd is empty so this returns before touching C-Gate.
	t.Run("cgate without cmd", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodGet, "http://addon:8980/cgate", nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("cgate without cmd = %d, want 400", rec.Code)
		}
	})
}
