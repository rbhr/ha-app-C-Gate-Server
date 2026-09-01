package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/websocket"
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
		// "ingress_entry: /" has been dropped from config.yaml, so Supervisor
		// no longer asks for "//". These stay so that reintroducing the key
		// cannot break the panel again.
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
		if rec.Code != http.StatusOK {
			t.Errorf("health = %d %q", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"connections"`) {
			t.Errorf("health body carries no connection detail: %q", rec.Body.String())
		}
	})

	t.Run("ready", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodGet, "http://addon:8980/ready", nil))
		// Nothing is connected in a unit test, so this must not claim ready.
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("ready with no connections = %d, want 503", rec.Code)
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

// useTempProjectsDir points the project handlers at a temporary directory.
func useTempProjectsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prevDir, prevActive := projectsDir, activeProject
	projectsDir, activeProject = dir, "HOME"
	t.Cleanup(func() { projectsDir, activeProject = prevDir, prevActive })
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
	dir := useTempProjectsDir(t)

	// Only a directory holding a database of the same name is a project C-Gate
	// can see; everything else here is not one.
	writeDB(t, filepath.Join(dir, "HOME", "HOME.db"), "home")
	writeDB(t, filepath.Join(dir, "EXAMPLE", "EXAMPLE.db"), "example")
	writeDB(t, filepath.Join(dir, "HOME", "HOME.db.bak"), "old home")
	writeDB(t, filepath.Join(dir, "STRAY.db"), "loose file, not in a directory")
	writeDB(t, filepath.Join(dir, "MISMATCH", "OTHER.db"), "wrong name for its directory")
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
	// The size is the whole project directory, and the .db.bak an upload
	// leaves behind is ours rather than part of the project.
	if want := int64(len(sqliteMagic) + len("home")); got[1].Size != want || got[1].Files != 1 {
		t.Errorf("HOME = %d bytes in %d files, want %d in 1", got[1].Size, got[1].Files, want)
	}
}

// The layout is not negotiable: C-Gate reads <projects>/<name>/<name>.db and
// silently cannot find a database written anywhere else.
func TestDBPathIsTheLayoutCGateReads(t *testing.T) {
	dir := useTempProjectsDir(t)

	if got, want := dbPath("YELMAH"), filepath.Join(dir, "YELMAH", "YELMAH.db"); got != want {
		t.Errorf("dbPath(YELMAH) = %q, want %q", got, want)
	}
	if got, want := projectDir("YELMAH"), filepath.Join(dir, "YELMAH"); got != want {
		t.Errorf("projectDir(YELMAH) = %q, want %q", got, want)
	}
}

func TestTagDownload(t *testing.T) {
	dir := useTempProjectsDir(t)
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
	dir := useTempProjectsDir(t)
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
	dir := useTempProjectsDir(t)
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
	dir := useTempProjectsDir(t)
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
	dir := useTempProjectsDir(t)

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
	useTempProjectsDir(t)
	rec := httptest.NewRecorder()
	route(http.NotFoundHandler())(rec, httptest.NewRequest(http.MethodGet, "http://addon:8980/tag/upload", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /tag/upload = %d, want 405", rec.Code)
	}
}

func TestTagList(t *testing.T) {
	dir := useTempProjectsDir(t)
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

// --- Archive uploads ---

// zipBytes builds a zip the way Toolkit writes a .cbz: a flat archive of the
// project directory.
func zipBytes(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// tarBytes builds a tar the way tar(1) does from inside a project directory:
// entry names carry a "./" prefix and directories get their own entries.
func tarBytes(t *testing.T, entries map[string][]byte, compress bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	var out io.Writer = &buf
	var gz *gzip.Writer
	if compress {
		gz = gzip.NewWriter(&buf)
		out = gz
	}

	tw := tar.NewWriter(out)
	if err := tw.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	for name, content := range entries {
		if err := tw.WriteHeader(&tar.Header{
			Name: "./" + name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if gz != nil {
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

func db(payload string) []byte {
	return append(append([]byte{}, sqliteMagic...), payload...)
}

// toolkitProject is the shape of a real C-Bus Toolkit backup: the database,
// the dynamic labelling index, and a bitmap per label.
func toolkitProject(name string) map[string][]byte {
	return map[string][]byte{
		name + ".db":               db("project " + name),
		name + "-DLTD-index.txt":   []byte("2000,Font=abc,Pic2000.bmp\n"),
		name + "-DLTD-Pic2000.bmp": []byte("BM bitmap bytes"),
	}
}

func TestSniff(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want uploadKind
	}{
		{"database", db("x"), kindDatabase},
		{"zip", zipBytes(t, map[string][]byte{"HOME.db": db("x")}), kindZip},
		{"tar", tarBytes(t, map[string][]byte{"HOME.db": db("x")}, false), kindTar},
		{"tar.gz", tarBytes(t, map[string][]byte{"HOME.db": db("x")}, true), kindTarGz},
		{"text", []byte("this is not a project at all"), kindUnknown},
		{"empty", nil, kindUnknown},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sniff(bytes.NewReader(c.in)); got != c.want {
				t.Errorf("sniff(%s) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}

func TestEntryPath(t *testing.T) {
	dir := t.TempDir()

	t.Run("keeps a Toolkit entry name", func(t *testing.T) {
		got, err := entryPath(dir, "YELMAH-DLTD-Pic2000.bmp")
		if err != nil || got != filepath.Join(dir, "YELMAH-DLTD-Pic2000.bmp") {
			t.Errorf("entryPath = %q, %v", got, err)
		}
	})

	t.Run("keeps a subdirectory", func(t *testing.T) {
		got, err := entryPath(dir, "./XML Backup files/YELMAH.xml")
		if err != nil || got != filepath.Join(dir, "XML Backup files", "YELMAH.xml") {
			t.Errorf("entryPath = %q, %v", got, err)
		}
	})

	// An entry name is attacker-controlled and must never be rewritten into
	// something harmless-looking — it is refused.
	for _, name := range []string{"../evil.db", "../../etc/passwd", "/etc/passwd", "a/../../evil.db", ".."} {
		t.Run("refuses "+name, func(t *testing.T) {
			if _, err := entryPath(dir, name); err == nil {
				t.Errorf("entryPath(%q) was accepted, want refusal", name)
			}
		})
	}

	for _, name := range []string{"./", ".", "/"} {
		t.Run("skips "+name, func(t *testing.T) {
			if _, err := entryPath(dir, name); !errors.Is(err, errSkipEntry) {
				t.Errorf("entryPath(%q) err = %v, want errSkipEntry", name, err)
			}
		})
	}
}

func TestUploadToolkitBackup(t *testing.T) {
	dir := useTempProjectsDir(t)
	handler := route(http.NotFoundHandler())

	// Toolkit names the backup for the day it was taken, so the project can
	// only come from the database inside it.
	rec := httptest.NewRecorder()
	handler(rec, uploadRequest(t, "", "YELMAH_09_May_2025_2214_1.18.1.cbz",
		zipBytes(t, toolkitProject("YELMAH"))))

	if rec.Code != http.StatusOK {
		t.Fatalf("upload = %d %q, want 200", rec.Code, rec.Body.String())
	}

	var resp struct {
		Project string `json:"project"`
		Files   int    `json:"files"`
		Backup  bool   `json:"backup"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Project != "YELMAH" || resp.Files != 3 || resp.Backup {
		t.Errorf("response = %+v, want YELMAH, 3 files, no backup", resp)
	}

	for _, name := range []string{"YELMAH.db", "YELMAH-DLTD-index.txt", "YELMAH-DLTD-Pic2000.bmp"} {
		if _, err := os.Stat(filepath.Join(dir, "YELMAH", name)); err != nil {
			t.Errorf("%s was not installed: %v", name, err)
		}
	}
}

func TestUploadArchiveFormats(t *testing.T) {
	for _, c := range []struct {
		name     string
		filename string
		body     func(*testing.T) []byte
	}{
		{"zip", "YELMAH.zip", func(t *testing.T) []byte { return zipBytes(t, toolkitProject("YELMAH")) }},
		{"tar", "yel.tar", func(t *testing.T) []byte { return tarBytes(t, toolkitProject("YELMAH"), false) }},
		{"tar.gz", "yel.tar.gz", func(t *testing.T) []byte { return tarBytes(t, toolkitProject("YELMAH"), true) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := useTempProjectsDir(t)
			rec := httptest.NewRecorder()
			route(http.NotFoundHandler())(rec, uploadRequest(t, "", c.filename, c.body(t)))

			if rec.Code != http.StatusOK {
				t.Fatalf("upload = %d %q, want 200", rec.Code, rec.Body.String())
			}
			got, err := os.ReadFile(filepath.Join(dir, "YELMAH", "YELMAH.db"))
			if err != nil || !bytes.Equal(got, db("project YELMAH")) {
				t.Errorf("database = %q (%v), want the uploaded one", got, err)
			}
			if _, err := os.Stat(filepath.Join(dir, "YELMAH", "YELMAH-DLTD-Pic2000.bmp")); err != nil {
				t.Errorf("bitmap was not installed: %v", err)
			}
		})
	}
}

// The whole directory is replaced, so a bitmap that the new project does not
// have does not linger from the old one — but it is still in the backup.
func TestUploadArchiveReplacesWholeDirectory(t *testing.T) {
	dir := useTempProjectsDir(t)
	writeDB(t, filepath.Join(dir, "YELMAH", "YELMAH.db"), "old")
	if err := os.WriteFile(filepath.Join(dir, "YELMAH", "YELMAH-DLTD-Pic9999.bmp"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	route(http.NotFoundHandler())(rec, uploadRequest(t, "", "backup.cbz", zipBytes(t, toolkitProject("YELMAH"))))
	if rec.Code != http.StatusOK {
		t.Fatalf("upload = %d %q, want 200", rec.Code, rec.Body.String())
	}

	if _, err := os.Stat(filepath.Join(dir, "YELMAH", "YELMAH-DLTD-Pic9999.bmp")); !os.IsNotExist(err) {
		t.Errorf("stale bitmap survived the upload (err = %v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "YELMAH"+backupSuffix, "YELMAH-DLTD-Pic9999.bmp")); err != nil {
		t.Errorf("stale bitmap is not in the backup: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "YELMAH"+backupSuffix, "YELMAH.db")); err != nil ||
		!strings.HasSuffix(string(got), "old") {
		t.Errorf("backup database = %q (%v), want the previous one", got, err)
	}

	// The staging directory is gone either way.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".incoming-") {
			t.Errorf("staging directory left behind: %s", e.Name())
		}
	}
}

func TestUploadArchiveRefusesEscapingEntries(t *testing.T) {
	dir := useTempProjectsDir(t)
	writeDB(t, filepath.Join(dir, "HOME", "HOME.db"), "untouched")

	rec := httptest.NewRecorder()
	route(http.NotFoundHandler())(rec, uploadRequest(t, "", "evil.zip", zipBytes(t, map[string][]byte{
		"YELMAH.db":     db("x"),
		"../escaped.db": db("escaped"),
	})))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("upload = %d %q, want 400", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escaped.db")); !os.IsNotExist(err) {
		t.Errorf("an entry escaped the tag directory (err = %v)", err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "HOME", "HOME.db")); err != nil ||
		!strings.HasSuffix(string(got), "untouched") {
		t.Errorf("existing project = %q (%v), want it left alone", got, err)
	}
}

func TestUploadArchiveRejects(t *testing.T) {
	cases := []struct {
		name    string
		project string
		entries map[string][]byte
	}{
		{"no database", "", map[string][]byte{"YELMAH-DLTD-Pic2000.bmp": []byte("BM")}},
		{"two databases", "", map[string][]byte{"HOME.db": db("a"), "YELMAH.db": db("b")}},
		{"name does not match the archive", "HOME", toolkitProject("YELMAH")},
		{"C-Gate generic archive without a name", "", map[string][]byte{"tagdb.db": db("generic")}},
		{"empty archive", "", map[string][]byte{}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			useTempProjectsDir(t)
			rec := httptest.NewRecorder()
			route(http.NotFoundHandler())(rec, uploadRequest(t, c.project, "upload.zip", zipBytes(t, c.entries)))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("upload = %d %q, want 400", rec.Code, rec.Body.String())
			}
		})
	}
}

// C-Gate's own PROJECT ARCHIVE zip holds the database as tagdb.db, so the
// project name has to come from the request.
func TestUploadCGateArchiveWithName(t *testing.T) {
	dir := useTempProjectsDir(t)

	rec := httptest.NewRecorder()
	route(http.NotFoundHandler())(rec, uploadRequest(t, "HOME", "archive.zip",
		zipBytes(t, map[string][]byte{"tagdb.db": db("generic")})))

	if rec.Code != http.StatusOK {
		t.Fatalf("upload = %d %q, want 200", rec.Code, rec.Body.String())
	}
	if got, err := os.ReadFile(filepath.Join(dir, "HOME", "HOME.db")); err != nil ||
		!bytes.Equal(got, db("generic")) {
		t.Errorf("installed database = %q (%v), want the generic one under the project name", got, err)
	}
}

func TestTagArchiveDownload(t *testing.T) {
	dir := useTempProjectsDir(t)
	for name, content := range toolkitProject("YELMAH") {
		if err := os.MkdirAll(filepath.Join(dir, "YELMAH"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "YELMAH", name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	handler := route(http.NotFoundHandler())

	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "http://addon:8980/tag/archive?project=YELMAH", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("archive = %d, want 200", rec.Code)
	}
	if got, want := rec.Header().Get("Content-Disposition"), `attachment; filename="YELMAH.zip"`; got != want {
		t.Errorf("Content-Disposition = %q, want %q", got, want)
	}

	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	sort.Strings(names)
	want := []string{"YELMAH-DLTD-Pic2000.bmp", "YELMAH-DLTD-index.txt", "YELMAH.db"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("archive holds %v, want %v", names, want)
	}

	// What comes out goes back in.
	dir2 := useTempProjectsDir(t)
	rec2 := httptest.NewRecorder()
	handler(rec2, uploadRequest(t, "", "YELMAH.zip", rec.Body.Bytes()))
	if rec2.Code != http.StatusOK {
		t.Fatalf("re-upload = %d %q, want 200", rec2.Code, rec2.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir2, "YELMAH", "YELMAH-DLTD-Pic2000.bmp")); err != nil {
		t.Errorf("round trip lost the bitmap: %v", err)
	}
}

// The header's one-click backup asks for ?format=cbz. The bytes are the same
// flat zip either way; only the extension differs, and it is the extension
// that decides whether Toolkit offers to restore the file.
func TestTagArchiveDownloadAsCBZ(t *testing.T) {
	dir := useTempProjectsDir(t)
	if err := os.MkdirAll(filepath.Join(dir, "YELMAH"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range toolkitProject("YELMAH") {
		if err := os.WriteFile(filepath.Join(dir, "YELMAH", name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	handler := route(http.NotFoundHandler())

	for _, c := range []struct{ query, want string }{
		{"", `attachment; filename="YELMAH.zip"`},
		{"&format=zip", `attachment; filename="YELMAH.zip"`},
		{"&format=cbz", `attachment; filename="YELMAH.cbz"`},
		{"&format=CBZ", `attachment; filename="YELMAH.cbz"`},
		{"&format=nonsense", `attachment; filename="YELMAH.zip"`},
	} {
		t.Run("format"+c.query, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler(rec, httptest.NewRequest(http.MethodGet,
				"http://addon:8980/tag/archive?project=YELMAH"+c.query, nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("archive = %d, want 200", rec.Code)
			}
			if got := rec.Header().Get("Content-Disposition"); got != c.want {
				t.Errorf("Content-Disposition = %q, want %q", got, c.want)
			}
			if _, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len())); err != nil {
				t.Errorf("body is not a readable zip: %v", err)
			}
		})
	}
}

// An upload keeps the database it replaced as <project>.db.bak, inside the
// project directory. That file is the console's, not the project's: a .cbz
// carrying it hands Toolkit an archive with two databases in it.
func TestTagArchiveLeavesOutOurOwnBackups(t *testing.T) {
	dir := useTempProjectsDir(t)
	if err := os.MkdirAll(filepath.Join(dir, "YELMAH"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range toolkitProject("YELMAH") {
		if err := os.WriteFile(filepath.Join(dir, "YELMAH", name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "YELMAH", "YELMAH.db"+backupSuffix),
		append(append([]byte{}, sqliteMagic...), []byte("the previous one")...), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	route(http.NotFoundHandler())(rec, httptest.NewRequest(http.MethodGet,
		"http://addon:8980/tag/archive?project=YELMAH&format=cbz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("archive = %d, want 200", rec.Code)
	}

	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	sort.Strings(names)
	want := []string{"YELMAH-DLTD-Pic2000.bmp", "YELMAH-DLTD-index.txt", "YELMAH.db"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("archive holds %v, want %v", names, want)
	}
}

func TestTagArchiveDownloadOfUnknownProject(t *testing.T) {
	useTempProjectsDir(t)

	rec := httptest.NewRecorder()
	route(http.NotFoundHandler())(rec, httptest.NewRequest(http.MethodGet, "http://addon:8980/tag/archive?project=NOPE", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("archive of an unknown project = %d, want 404", rec.Code)
	}
}

// fakeCGatePort stands in for a C-Gate stream interface. It accepts one
// connection, hands it to serve, and records how many connections it saw so a
// test can tell a torn-down-and-reconnected stream from a stable one.
type fakeCGatePort struct {
	ln     net.Listener
	port   string
	accept chan net.Conn
}

func newFakeCGatePort(t *testing.T) *fakeCGatePort {
	t.Helper()
	// streamPort dials cgateHost, so bind the same name rather than 127.0.0.1
	// — on a dual-stack box they are not necessarily the same address.
	ln, err := net.Listen("tcp", net.JoinHostPort(cgateHost, "0"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	f := &fakeCGatePort{ln: ln, port: port, accept: make(chan net.Conn, 8)}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			f.accept <- conn
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return f
}

// wsClient subscribes to the hub the way the console does, so a test can read
// what streamPort actually broadcast.
func wsClient(t *testing.T) chan map[string]string {
	t.Helper()
	srv := httptest.NewServer(websocket.Handler(handleWS))
	t.Cleanup(srv.Close)

	origin := srv.URL
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	ws, err := websocket.Dial(wsURL, "", origin)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	t.Cleanup(func() { ws.Close() })

	msgs := make(chan map[string]string, 16)
	go func() {
		defer close(msgs)
		for {
			// Message.Receive, not ws.Read — Read returns whatever of the
			// frame has arrived, which truncates a large broadcast.
			var raw string
			if err := websocket.Message.Receive(ws, &raw); err != nil {
				return
			}
			var m map[string]string
			if err := json.Unmarshal([]byte(raw), &m); err != nil {
				return
			}
			msgs <- m
		}
	}()

	// hub.add happens on the server side; give it a moment to register before
	// the caller starts broadcasting, or the first line goes nowhere.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		hub.mu.RLock()
		n := len(hub.clients)
		hub.mu.RUnlock()
		if n > 0 {
			return msgs
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("ws client never registered with the hub")
	return nil
}

// TestQuietStreamIsNotTornDown covers the regression that a read deadline on
// these connections caused: both C-Gate stream interfaces are legitimately
// quiet (no events at the default global-event-level, no status changes on an
// idle site), and arming a deadline before each read reconnected them on a
// fixed cycle forever, dropping whatever arrived during the gap.
//
// Be clear about what this does and does not catch: it will not fail against
// the five-minute deadline it was written for, because waiting that out is not
// a test anyone will run. It pins the invariant — an idle stream is the same
// connection afterwards, and a line sent after the gap still arrives — and it
// fails on any deadline short enough to fire inside the idle window.
func TestQuietStreamIsNotTornDown(t *testing.T) {
	fake := newFakeCGatePort(t)
	msgs := wsClient(t)

	go streamPort(fake.port, "event", new(atomic.Bool))

	conn := <-fake.accept
	defer conn.Close()

	// Idle. A deadline short enough to be testable would fire here; the point
	// is that nothing does.
	time.Sleep(300 * time.Millisecond)

	select {
	case c := <-fake.accept:
		c.Close()
		t.Fatal("stream reconnected while idle — the read deadline is back")
	default:
	}

	if _, err := conn.Write([]byte("evt 702 //PROJECT/254/56/1\n")); err != nil {
		t.Fatalf("write after idle: %v", err)
	}

	select {
	case m, ok := <-msgs:
		if !ok {
			t.Fatal("ws client stopped before the line arrived")
		}
		if m["stream"] != "event" {
			t.Errorf("stream = %q, want %q", m["stream"], "event")
		}
		if m["data"] != "evt 702 //PROJECT/254/56/1" {
			t.Errorf("data = %q", m["data"])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("line sent after an idle gap never arrived")
	}
}

// TestStreamHandlesLongLine covers the scanner buffer raise. bufio.Scanner
// caps a token at 64KB by default and then fails with ErrTooLong, which with
// the reconnect loop underneath it would spin fast rather than slow.
func TestStreamHandlesLongLine(t *testing.T) {
	fake := newFakeCGatePort(t)
	msgs := wsClient(t)

	go streamPort(fake.port, "status", new(atomic.Bool))

	conn := <-fake.accept
	defer conn.Close()

	long := strings.Repeat("x", 200*1024)
	if _, err := conn.Write([]byte(long + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case m, ok := <-msgs:
		if !ok {
			t.Fatal("ws client stopped before the line arrived")
		}
		if len(m["data"]) != len(long) {
			t.Errorf("data length = %d, want %d", len(m["data"]), len(long))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a 200KB line was dropped — scanner buffer too small")
	}

	select {
	case c := <-fake.accept:
		c.Close()
		t.Fatal("stream reconnected after a long line — ErrTooLong")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestHealthAndReadinessSplit covers what /health and /ready each promise.
//
// /health stays 200 while the bridge is serving: Supervisor uses it to decide
// whether to restart, and C-Gate needs up to a minute to sync its networks on
// a cold start, so failing it during that window would turn a normal boot into
// a restart loop. /ready is the probe that reports whether C-Gate is actually
// reachable, which /health used to answer with a fixed "ok".
func TestHealthAndReadinessSplit(t *testing.T) {
	restore := func(e, s, c bool) {
		eventStreamUp.Store(e)
		statusStreamUp.Store(s)
		commandUp.Store(c)
	}
	t.Cleanup(func() { restore(false, false, false) })

	type body struct {
		Status      string          `json:"status"`
		Connections map[string]bool `json:"connections"`
	}

	get := func(t *testing.T, h http.HandlerFunc) (int, body) {
		t.Helper()
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, "http://addon:8980/x", nil))
		var b body
		if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
			t.Fatalf("decoding %q: %v", rec.Body.String(), err)
		}
		return rec.Code, b
	}

	t.Run("nothing connected", func(t *testing.T) {
		restore(false, false, false)

		code, b := get(t, handleHealth)
		if code != http.StatusOK {
			t.Errorf("health = %d, want 200 even with C-Gate down", code)
		}
		if b.Status != "degraded" {
			t.Errorf("health status = %q, want degraded", b.Status)
		}

		code, _ = get(t, handleReady)
		if code != http.StatusServiceUnavailable {
			t.Errorf("ready = %d, want 503", code)
		}
	})

	t.Run("partially connected", func(t *testing.T) {
		// The command port is the one an upload and the console both need.
		restore(true, true, false)

		if code, _ := get(t, handleHealth); code != http.StatusOK {
			t.Errorf("health = %d, want 200", code)
		}
		code, b := get(t, handleReady)
		if code != http.StatusServiceUnavailable {
			t.Errorf("ready with the command port down = %d, want 503", code)
		}
		if b.Connections["command"] {
			t.Error("body reports the command port up when it is not")
		}
	})

	t.Run("fully connected", func(t *testing.T) {
		restore(true, true, true)

		code, b := get(t, handleReady)
		if code != http.StatusOK {
			t.Errorf("ready = %d, want 200", code)
		}
		if b.Status != "ok" {
			t.Errorf("status = %q, want ok", b.Status)
		}
	})
}

// TestCommandDuringOutageFailsRatherThanHanging is the point of splitting the
// dial in two. connect() used to retry forever with s.mu held, so every request
// queued behind it for as long as C-Gate was down.
func TestCommandDuringOutageFailsRatherThanHanging(t *testing.T) {
	// Nothing is listening on the command port in a unit test, so the dial
	// fails the way it would during an outage.
	s := &commandSession{}
	commandUp.Store(false)
	t.Cleanup(func() { commandUp.Store(false) })

	done := make(chan error, 1)
	go func() {
		_, err := s.send("noop")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("send succeeded with no C-Gate listening")
		}
	case <-time.After(dialTimeout + 5*time.Second):
		t.Fatal("send blocked past the dial timeout — it is retrying under the lock again")
	}

	if commandUp.Load() {
		t.Error("a failed dial left the session marked up")
	}

	// The mutex has to be free for the next caller.
	if !s.mu.TryLock() {
		t.Error("send left s.mu held")
	} else {
		s.mu.Unlock()
	}
}
