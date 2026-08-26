package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
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

// --- Project tag databases ---

// useTempTagDir points the tag database handlers at a temporary directory.
func useTempTagDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prevDir, prevActive := tagDir, activeProject
	tagDir, activeProject = dir, "HOME"
	t.Cleanup(func() { tagDir, activeProject = prevDir, prevActive })
	return dir
}

// writeDB creates a file that passes the SQLite header check.
func writeDB(t *testing.T, path, payload string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(append([]byte{}, sqliteMagic...), payload...), 0o644); err != nil {
		t.Fatal(err)
	}
}

func uploadRequest(t *testing.T, project, filename string, content []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if project != "" {
		if err := mw.WriteField("project", project); err != nil {
			t.Fatal(err)
		}
	}
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://addon:8980/tag/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

func TestListProjects(t *testing.T) {
	dir := useTempTagDir(t)

	// C-Gate's own layout, the flat layout older add-on versions left behind,
	// and files that are not project databases.
	writeDB(t, filepath.Join(dir, "HOME", "HOME.db"), "home")
	writeDB(t, filepath.Join(dir, "EXAMPLE.db"), "example")
	writeDB(t, filepath.Join(dir, "HOME", "HOME.db.bak"), "old home")
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "EMPTY"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := listProjects()
	if len(got) != 2 {
		t.Fatalf("listProjects() = %+v, want EXAMPLE and HOME", got)
	}
	if got[0].Name != "EXAMPLE" || got[0].Active {
		t.Errorf("got[0] = %+v, want EXAMPLE not active", got[0])
	}
	if got[1].Name != "HOME" || !got[1].Active {
		t.Errorf("got[1] = %+v, want HOME active", got[1])
	}
	if want := int64(len(sqliteMagic) + len("home")); got[1].Size != want {
		t.Errorf("HOME size = %d, want %d", got[1].Size, want)
	}
}

func TestDBPathPrefersCGateLayout(t *testing.T) {
	dir := useTempTagDir(t)

	// Nothing on disk yet: uploads land in C-Gate's own layout.
	if got, want := dbPath("NEW"), filepath.Join(dir, "NEW", "NEW.db"); got != want {
		t.Errorf("dbPath(NEW) = %q, want %q", got, want)
	}

	// A flat database from an older add-on version is used where it lies, so
	// an upload replaces the file C-Gate is actually reading.
	writeDB(t, filepath.Join(dir, "OLD.db"), "old")
	if got, want := dbPath("OLD"), filepath.Join(dir, "OLD.db"); got != want {
		t.Errorf("dbPath(OLD) = %q, want %q", got, want)
	}

	// With both, the nested one wins.
	writeDB(t, filepath.Join(dir, "OLD", "OLD.db"), "new")
	if got, want := dbPath("OLD"), filepath.Join(dir, "OLD", "OLD.db"); got != want {
		t.Errorf("dbPath(OLD) with both layouts = %q, want %q", got, want)
	}
}

func TestTagDownload(t *testing.T) {
	dir := useTempTagDir(t)
	writeDB(t, filepath.Join(dir, "HOME", "HOME.db"), "home")
	handler := route(http.NotFoundHandler())

	t.Run("serves the database as an attachment", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodGet, "http://addon:8980/tag/download?project=HOME", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("download = %d, want 200", rec.Code)
		}
		if got, want := rec.Header().Get("Content-Disposition"), `attachment; filename="HOME.db"`; got != want {
			t.Errorf("Content-Disposition = %q, want %q", got, want)
		}
		if !bytes.HasPrefix(rec.Body.Bytes(), sqliteMagic) || !strings.HasSuffix(rec.Body.String(), "home") {
			t.Errorf("body = %q, want the database file", rec.Body.String())
		}
	})

	t.Run("unknown project", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodGet, "http://addon:8980/tag/download?project=NOPE", nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("download of unknown project = %d, want 404", rec.Code)
		}
	})

	// The project name builds a path, so anything that could escape the tag
	// directory has to be refused outright.
	for _, name := range []string{"", "..", "../../etc/passwd", "HOME/../..", "HOME.db"} {
		t.Run("rejects "+name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler(rec, httptest.NewRequest(http.MethodGet,
				"http://addon:8980/tag/download?project="+url.QueryEscape(name), nil))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("download of %q = %d, want 400", name, rec.Code)
			}
		})
	}
}

func TestTagUploadReplacesDatabase(t *testing.T) {
	dir := useTempTagDir(t)
	dest := filepath.Join(dir, "HOME", "HOME.db")
	writeDB(t, dest, "old contents")
	handler := route(http.NotFoundHandler())

	uploaded := append(append([]byte{}, sqliteMagic...), "new contents"...)
	rec := httptest.NewRecorder()
	handler(rec, uploadRequest(t, "HOME", "HOME.db", uploaded))

	if rec.Code != http.StatusOK {
		t.Fatalf("upload = %d %q, want 200", rec.Code, rec.Body.String())
	}

	var resp struct {
		Project string `json:"project"`
		Size    int64  `json:"size"`
		Backup  bool   `json:"backup"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Project != "HOME" || resp.Size != int64(len(uploaded)) || !resp.Backup {
		t.Errorf("response = %+v, want HOME, %d bytes, backed up", resp, len(uploaded))
	}

	if got, err := os.ReadFile(dest); err != nil || !bytes.Equal(got, uploaded) {
		t.Errorf("installed database = %q (%v), want the uploaded bytes", got, err)
	}
	if got, err := os.ReadFile(dest + backupSuffix); err != nil || !strings.HasSuffix(string(got), "old contents") {
		t.Errorf("backup = %q (%v), want the previous database", got, err)
	}

	// Nothing left behind from the temporary file the upload is staged in.
	entries, err := os.ReadDir(filepath.Dir(dest))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".upload-") {
			t.Errorf("temporary upload file left behind: %s", e.Name())
		}
	}
}

func TestTagUploadCreatesNewProject(t *testing.T) {
	dir := useTempTagDir(t)
	handler := route(http.NotFoundHandler())

	// No project field: the name comes from the file name.
	rec := httptest.NewRecorder()
	handler(rec, uploadRequest(t, "", "OFFICE.db", append(append([]byte{}, sqliteMagic...), "office"...)))

	if rec.Code != http.StatusOK {
		t.Fatalf("upload = %d %q, want 200", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "OFFICE", "OFFICE.db")); err != nil {
		t.Errorf("OFFICE.db was not created: %v", err)
	}
}

func TestTagUploadRejectsBadRequests(t *testing.T) {
	dir := useTempTagDir(t)
	dest := filepath.Join(dir, "HOME", "HOME.db")
	writeDB(t, dest, "untouched")
	handler := route(http.NotFoundHandler())

	cases := []struct {
		name     string
		project  string
		filename string
		content  []byte
	}{
		{"not a database", "HOME", "HOME.db", []byte("this is a text file")},
		{"path traversal in project", "../../etc/passwd", "x.db", append(append([]byte{}, sqliteMagic...), "x"...)},
		{"invalid characters in file name", "", "my project!.db", append(append([]byte{}, sqliteMagic...), "x"...)},
		{"empty file", "HOME", "HOME.db", nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler(rec, uploadRequest(t, c.project, c.filename, c.content))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("upload = %d %q, want 400", rec.Code, rec.Body.String())
			}
		})
	}

	if got, err := os.ReadFile(dest); err != nil || !strings.HasSuffix(string(got), "untouched") {
		t.Errorf("existing database = %q (%v), want it left alone", got, err)
	}
}

// A file name is attacker-controlled, so the project it implies is taken from
// its base name only and can never point outside the tag directory.
func TestTagUploadIgnoresPathsInFileName(t *testing.T) {
	dir := useTempTagDir(t)

	rec := httptest.NewRecorder()
	route(http.NotFoundHandler())(rec,
		uploadRequest(t, "", "../../HOME.db", append(append([]byte{}, sqliteMagic...), "x"...)))

	if rec.Code != http.StatusOK {
		t.Fatalf("upload = %d %q, want 200", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "HOME", "HOME.db")); err != nil {
		t.Errorf("upload did not land in the tag directory: %v", err)
	}
}

func TestTagUploadRequiresPost(t *testing.T) {
	useTempTagDir(t)
	rec := httptest.NewRecorder()
	route(http.NotFoundHandler())(rec, httptest.NewRequest(http.MethodGet, "http://addon:8980/tag/upload", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /tag/upload = %d, want 405", rec.Code)
	}
}

func TestTagList(t *testing.T) {
	dir := useTempTagDir(t)
	writeDB(t, filepath.Join(dir, "HOME", "HOME.db"), "home")

	rec := httptest.NewRecorder()
	route(http.NotFoundHandler())(rec, httptest.NewRequest(http.MethodGet, "http://addon:8980/tag", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("/tag = %d, want 200", rec.Code)
	}
	var resp struct {
		Active   string      `json:"active"`
		Projects []projectDB `json:"projects"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Active != "HOME" || len(resp.Projects) != 1 || resp.Projects[0].Name != "HOME" {
		t.Errorf("/tag = %+v, want the HOME project", resp)
	}
}
