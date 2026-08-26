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
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
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
	// The size is the whole project directory, and the .db.bak an upload
	// leaves behind is ours rather than part of the project.
	if want := int64(len(sqliteMagic) + len("home")); got[1].Size != want || got[1].Files != 1 {
		t.Errorf("HOME = %d bytes in %d files, want %d in 1", got[1].Size, got[1].Files, want)
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
	dir := useTempTagDir(t)
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
			dir := useTempTagDir(t)
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
	dir := useTempTagDir(t)
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
	dir := useTempTagDir(t)
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
			useTempTagDir(t)
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
	dir := useTempTagDir(t)

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
	dir := useTempTagDir(t)
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
	dir2 := useTempTagDir(t)
	rec2 := httptest.NewRecorder()
	handler(rec2, uploadRequest(t, "", "YELMAH.zip", rec.Body.Bytes()))
	if rec2.Code != http.StatusOK {
		t.Fatalf("re-upload = %d %q, want 200", rec2.Code, rec2.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir2, "YELMAH", "YELMAH-DLTD-Pic2000.bmp")); err != nil {
		t.Errorf("round trip lost the bitmap: %v", err)
	}
}

func TestTagArchiveDownloadWithoutDirectory(t *testing.T) {
	dir := useTempTagDir(t)
	writeDB(t, filepath.Join(dir, "OLD.db"), "flat")

	rec := httptest.NewRecorder()
	route(http.NotFoundHandler())(rec, httptest.NewRequest(http.MethodGet, "http://addon:8980/tag/archive?project=OLD", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("archive of a flat project = %d, want 404", rec.Code)
	}
}
