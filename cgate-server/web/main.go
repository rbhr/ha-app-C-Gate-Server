package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/websocket"
)

//go:embed console.html
var consoleHTML embed.FS

const (
	cgateHost        = "localhost"
	cgateCommandPort = "20023"
	cgateEventPort   = "20024"
	cgateStatusPort  = "20025"
	listenAddr       = ":8980"

	// Home Assistant ingress session path prefix, stripped if the proxy
	// passes it through to us.
	ingressPrefix = "/api/hassio_ingress/"

	// TCP keepalive interval for long-lived connections
	keepAliveInterval = 30 * time.Second

	// Read deadline for stream connections — if nothing arrives within
	// this window we assume the connection is dead and reconnect.
	streamReadDeadline = 5 * time.Minute

	// Per-line read deadline for an interactive command.
	commandReadDeadline = 5 * time.Second

	// Per-line read deadline for the PROJECT commands an upload sends. Stopping
	// a started project means stopping its networks, which is not instant.
	projectReadDeadline = 20 * time.Second
)

// wsHub manages WebSocket clients
type wsHub struct {
	mu      sync.RWMutex
	clients map[*websocket.Conn]bool
}

func newHub() *wsHub {
	return &wsHub{clients: make(map[*websocket.Conn]bool)}
}

func (h *wsHub) add(ws *websocket.Conn) {
	h.mu.Lock()
	h.clients[ws] = true
	h.mu.Unlock()
}

func (h *wsHub) remove(ws *websocket.Conn) {
	h.mu.Lock()
	_, ok := h.clients[ws]
	delete(h.clients, ws)
	h.mu.Unlock()
	if ok {
		ws.Close()
	}
}

func (h *wsHub) broadcast(msg map[string]string) {
	data, _ := json.Marshal(msg)
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ws := range h.clients {
		if _, err := ws.Write(data); err != nil {
			go h.remove(ws)
		}
	}
}

var hub = newHub()

// dialTCP connects to a C-Gate port with retries and enables TCP keepalive
func dialTCP(port string) net.Conn {
	addr := net.JoinHostPort(cgateHost, port)
	for {
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err == nil {
			// Enable TCP keepalive so the OS detects dead connections
			if tc, ok := conn.(*net.TCPConn); ok {
				tc.SetKeepAlive(true)
				tc.SetKeepAlivePeriod(keepAliveInterval)
			}
			log.Printf("Connected to C-Gate %s", addr)
			return conn
		}
		log.Printf("Waiting for C-Gate on %s: %v", addr, err)
		time.Sleep(3 * time.Second)
	}
}

// streamPort reads lines from a C-Gate port and broadcasts them.
// Reconnects automatically on any error or timeout.
func streamPort(port, streamName string) {
	for {
		conn := dialTCP(port)
		scanner := bufio.NewScanner(conn)
		alive := true
		for alive {
			// Set a read deadline so we detect dead connections even when
			// C-Gate is quiet (no events/status changes for a while).
			conn.SetReadDeadline(time.Now().Add(streamReadDeadline))
			if scanner.Scan() {
				line := scanner.Text()
				hub.broadcast(map[string]string{
					"stream": streamName,
					"data":   line,
					"time":   time.Now().Format("15:04:05"),
				})
			} else {
				alive = false
			}
		}
		if err := scanner.Err(); err != nil {
			log.Printf("Stream %s error: %v — reconnecting", streamName, err)
		} else {
			log.Printf("Stream %s disconnected (EOF) — reconnecting", streamName)
		}
		conn.Close()
		time.Sleep(2 * time.Second)
	}
}

// commandSession holds the persistent command connection and its reader
type commandSession struct {
	mu     sync.Mutex
	conn   net.Conn
	reader *bufio.Reader
	// connected is readable without holding mu, which connect() keeps for as
	// long as it takes C-Gate to come up.
	connected atomic.Bool
}

var cmdSession = &commandSession{}

func (s *commandSession) connect() {
	s.conn = dialTCP(cgateCommandPort)
	s.reader = bufio.NewReader(s.conn)
	// Drain the connect banner
	s.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		line, err := s.reader.ReadString('\n')
		if err != nil {
			break
		}
		_ = line
	}
	s.conn.SetReadDeadline(time.Time{})
	s.connected.Store(true)
}

func (s *commandSession) reconnect() {
	s.connected.Store(false)
	if s.conn != nil {
		s.conn.Close()
	}
	s.connect()
}

func (s *commandSession) send(cmd string) ([]string, error) {
	return s.sendWithin(cmd, commandReadDeadline)
}

// sendWithin runs a command, allowing deadline for each line of the reply. A
// slow command is not a dead connection: PROJECT STOP has a project's networks
// to shut down, which takes far longer than an interactive query.
func (s *commandSession) sendWithin(cmd string, deadline time.Duration) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.conn == nil {
		s.connect()
	}

	_, err := fmt.Fprintf(s.conn, "%s\r\n", cmd)
	if err != nil {
		log.Printf("Command write failed: %v — reconnecting", err)
		s.reconnect()
		_, err = fmt.Fprintf(s.conn, "%s\r\n", cmd)
		if err != nil {
			return nil, err
		}
	}

	// Read response lines
	var lines []string
	for {
		s.conn.SetReadDeadline(time.Now().Add(deadline))
		line, err := s.reader.ReadString('\n')
		if err != nil {
			if len(lines) > 0 {
				break // got at least some response
			}
			// Connection probably dead — reconnect for next call
			log.Printf("Command read failed: %v — will reconnect on next call", err)
			s.reconnect()
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		lines = append(lines, line)

		// Single-line response or last line of multi-line (no dash after code)
		if len(line) >= 3 && (len(line) == 3 || line[3] != '-') {
			break
		}
	}
	s.conn.SetReadDeadline(time.Time{})
	return lines, nil
}

func handleCGate(w http.ResponseWriter, r *http.Request) {
	cmd := r.URL.Query().Get("cmd")
	if cmd == "" {
		http.Error(w, `{"error":"missing cmd parameter"}`, http.StatusBadRequest)
		return
	}

	lines, err := cmdSession.send(cmd)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusBadGateway)
		return
	}

	// Broadcast command and response to WebSocket clients
	hub.broadcast(map[string]string{
		"stream": "command",
		"data":   "> " + cmd,
		"time":   time.Now().Format("15:04:05"),
	})
	for _, line := range lines {
		hub.broadcast(map[string]string{
			"stream": "response",
			"data":   line,
			"time":   time.Now().Format("15:04:05"),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"cmd":      cmd,
		"response": lines,
	})
}

// ---------------------------------------------------------------------------
// Project databases
//
// C-Gate keeps each project in its own directory as <project>/<project>.db, a
// plain SQLite file, under the Projects directory — a path built into
// cgate.jar and reached through a symlink to /data/projects. That layout is
// the only one C-Gate reads: a database anywhere else is invisible to it, so
// this code never writes one anywhere else.
//
// A project built in C-Bus Toolkit keeps more than the database in that
// directory — the dynamic labelling bitmaps and their index — and the database
// is no use without them. So the console moves whole project directories:
// uploads take Toolkit's own .cbz backup (a flat zip of the project
// directory), a zip or a tar, as well as a bare .db, and downloads offer
// either the database or the lot.
// ---------------------------------------------------------------------------

const (
	// Project databases run to a few hundred KB and a Toolkit backup to a few
	// MB. The cap is generous but bounded so a stray request cannot fill
	// /data.
	maxUploadBytes = 64 << 20
	// An archive is expanded onto disk, so what it unpacks to is bounded
	// separately from the size of the upload itself.
	maxUnpackedBytes  = 256 << 20
	maxArchiveEntries = 4096

	dbSuffix     = ".db"
	backupSuffix = ".bak"
)

var (
	// sqliteMagic is the header every C-Gate project database starts with.
	sqliteMagic = []byte("SQLite format 3\x00")
	zipMagic    = []byte("PK\x03\x04")
	gzipMagic   = []byte("\x1f\x8b")
	// tarMagic sits at offset 257 of the first header block.
	tarMagic = []byte("ustar")
)

// projectNamePattern is deliberately strict: the name is used to build a path
// under projectsDir and is passed to C-Gate as a command argument.
var projectNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// genericDBName is what C-Gate's own PROJECT ARCHIVE calls the database inside
// a zip, whichever project it came from.
const genericDBName = "tagdb"

var (
	// projectsDir is /data/projects, which run.sh links to /cgate/Projects.
	projectsDir = envOr("CGATE_PROJECTS_DIR", "/data/projects")
	// activeProject is the project run.sh configured C-Gate to use.
	activeProject = envOr("CGATE_PROJECT", "")
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

type projectDB struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Files    int    `json:"files"`
	Modified string `json:"modified"`
	Active   bool   `json:"active"`
}

// projectDir and dbPath are the only layout C-Gate reads. Writing anywhere
// else produces a database it silently cannot find.
func projectDir(project string) string {
	return filepath.Join(projectsDir, project)
}

func dbPath(project string) string {
	return filepath.Join(projectDir(project), project+dbSuffix)
}

// projectContents totals the files stored with a project. A Toolkit project
// keeps its dynamic labelling bitmaps and index alongside the database. The
// backups an upload leaves behind are ours rather than the project's, so they
// are left out of the count.
func projectContents(dir string) (size int64, files int) {
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(d.Name(), backupSuffix) {
			return nil
		}
		if info, err := d.Info(); err == nil && info.Mode().IsRegular() {
			size += info.Size()
			files++
		}
		return nil
	})
	return size, files
}

// listProjects reports every project C-Gate can actually see: a directory
// holding a database of the same name.
func listProjects() []projectDB {
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		log.Printf("Projects directory %s unreadable: %v", projectsDir, err)
		return nil
	}

	projects := make([]projectDB, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || !projectNamePattern.MatchString(name) {
			continue
		}
		info, err := os.Stat(dbPath(name))
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		size, files := projectContents(projectDir(name))
		projects = append(projects, projectDB{
			Name:     name,
			Size:     size,
			Files:    files,
			Modified: info.ModTime().Format("2006-01-02 15:04:05"),
			Active:   name == activeProject,
		})
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].Name < projects[j].Name })
	return projects
}

func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// cgateTry runs a C-Gate command for its side effect and reports what came
// back. It never waits on C-Gate: the command session dials with an unlimited
// retry loop and holds its lock while doing so, so a command sent from an
// upload is skipped when there is no connection and abandoned if the reply
// does not arrive. An upload must not hang because C-Gate is down.
func cgateTry(cmd string) []string { return cgateRun(cmd, commandReadDeadline) }

// cgateProject runs one of the PROJECT commands an upload sends, which are
// allowed longer to answer than an interactive query.
func cgateProject(cmd string) []string { return cgateRun(cmd, projectReadDeadline) }

func cgateRun(cmd string, deadline time.Duration) []string {
	if !cmdSession.connected.Load() {
		return []string{"> " + cmd + "  (skipped — no C-Gate connection)"}
	}

	type reply struct {
		lines []string
		err   error
	}
	done := make(chan reply, 1)
	go func() {
		lines, err := cmdSession.sendWithin(cmd, deadline)
		done <- reply{lines, err}
	}()

	// The session's own deadline should fire first; this only catches a
	// goroutine that never gets the session lock at all.
	abandon := deadline + 5*time.Second
	select {
	case r := <-done:
		if r.err != nil {
			return []string{"> " + cmd, "error: " + r.err.Error()}
		}
		return append([]string{"> " + cmd}, r.lines...)
	case <-time.After(abandon):
		return []string{"> " + cmd, "error: no reply from C-Gate within " + abandon.String()}
	}
}

// announce echoes tag database activity into the console log.
func announce(lines []string) {
	for _, line := range lines {
		hub.broadcast(map[string]string{
			"stream": "response",
			"data":   line,
			"time":   time.Now().Format("15:04:05"),
		})
	}
}

func handleTagList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"active":   activeProject,
		"projects": listProjects(),
	})
}

// requestedProject reads and validates the project named in the query string.
func requestedProject(r *http.Request) (string, error) {
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	if !projectNamePattern.MatchString(project) {
		return "", errors.New("invalid project name")
	}
	return project, nil
}

func handleTagDownload(w http.ResponseWriter, r *http.Request) {
	project, err := requestedProject(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	path := dbPath(project)
	f, err := os.Open(path)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "no database for project "+project)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	name := project + dbSuffix
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	log.Printf("Tag database download: %s (%d bytes)", path, info.Size())
	http.ServeContent(w, r, name, info.ModTime(), f)
}

// handleTagArchive streams a project's whole directory as a zip — the same
// shape as the .cbz backup Toolkit writes, so it can go straight back in.
func handleTagArchive(w http.ResponseWriter, r *http.Request) {
	project, err := requestedProject(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	dir := projectDir(project)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		writeJSONError(w, http.StatusNotFound, "no project directory for "+project)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+project+`.zip"`)

	zw := zip.NewWriter(w)
	defer zw.Close()

	// The response is already streaming, so a failure part way through can
	// only be logged — the client sees a truncated zip, which will not open.
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(rel)
		header.Method = zip.Deflate
		entry, err := zw.CreateHeader(header)
		if err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(entry, f)
		return err
	})
	if err != nil {
		log.Printf("Tag archive download of %s failed part way: %v", project, err)
		return
	}
	log.Printf("Tag archive download: %s", dir)
}

// uploadKind is what an uploaded file turned out to be.
type uploadKind int

const (
	kindUnknown uploadKind = iota
	kindDatabase
	kindZip
	kindTar
	kindTarGz
)

// sniff identifies an upload from its leading bytes rather than its name. A
// Toolkit backup arrives called YELMAH_09_May_2025_2214_1.18.1.cbz, which says
// nothing dependable about either the format or the project.
func sniff(r io.ReaderAt) uploadKind {
	head := make([]byte, 262)
	n, _ := r.ReadAt(head, 0)
	head = head[:n]

	switch {
	case bytes.HasPrefix(head, sqliteMagic):
		return kindDatabase
	case bytes.HasPrefix(head, zipMagic):
		return kindZip
	case bytes.HasPrefix(head, gzipMagic):
		return kindTarGz
	case len(head) >= 262 && bytes.Equal(head[257:262], tarMagic):
		return kindTar
	}
	return kindUnknown
}

// errSkipEntry marks an archive entry that is not part of a project rather
// than one that is dangerous.
var errSkipEntry = errors.New("skip entry")

// entryPath resolves an archive entry name to a path inside dir. Entry names
// come from the uploaded file, so an absolute path or one climbing out with
// ".." is refused outright rather than quietly rewritten.
func entryPath(dir, name string) (string, error) {
	clean := path.Clean(strings.ReplaceAll(name, `\`, "/"))
	if clean == "." || clean == "/" {
		return "", errSkipEntry
	}
	if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("archive entry %q would be written outside the project directory", name)
	}
	full := filepath.Join(dir, filepath.FromSlash(clean))
	if !strings.HasPrefix(full, dir+string(os.PathSeparator)) {
		return "", fmt.Errorf("archive entry %q would be written outside the project directory", name)
	}
	return full, nil
}

// writeEntry writes one archive entry, refusing to write more than budget
// bytes so that a small archive cannot expand into a full /data.
func writeEntry(dest string, r io.Reader, budget int64) (int64, error) {
	if budget <= 0 {
		return 0, fmt.Errorf("the archive expands to more than %d MB", maxUnpackedBytes>>20)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, io.LimitReader(r, budget+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return n, err
	}
	if n > budget {
		return n, fmt.Errorf("the archive expands to more than %d MB", maxUnpackedBytes>>20)
	}
	return n, nil
}

func unpackZip(f io.ReaderAt, size int64, dir string) error {
	zr, err := zip.NewReader(f, size)
	if err != nil {
		return fmt.Errorf("not a readable zip archive: %w", err)
	}
	if len(zr.File) > maxArchiveEntries {
		return fmt.Errorf("the archive holds %d entries, more than the %d allowed", len(zr.File), maxArchiveEntries)
	}

	var total int64
	for _, e := range zr.File {
		if !e.Mode().IsRegular() {
			continue // directories, symlinks and the like are not project files
		}
		dest, err := entryPath(dir, e.Name)
		if errors.Is(err, errSkipEntry) {
			continue
		}
		if err != nil {
			return err
		}
		rc, err := e.Open()
		if err != nil {
			return err
		}
		n, err := writeEntry(dest, rc, maxUnpackedBytes-total)
		rc.Close()
		if err != nil {
			return err
		}
		total += n
	}
	if total == 0 {
		return errors.New("the archive holds no files")
	}
	return nil
}

func unpackTar(r io.Reader, dir string) error {
	tr := tar.NewReader(r)

	var total int64
	entries := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("not a readable tar archive: %w", err)
		}
		if entries++; entries > maxArchiveEntries {
			return fmt.Errorf("the archive holds more than the %d entries allowed", maxArchiveEntries)
		}
		if h.Typeflag != tar.TypeReg {
			continue // directories, symlinks, devices
		}
		dest, err := entryPath(dir, h.Name)
		if errors.Is(err, errSkipEntry) {
			continue
		}
		if err != nil {
			return err
		}
		n, err := writeEntry(dest, tr, maxUnpackedBytes-total)
		if err != nil {
			return err
		}
		total += n
	}
	if total == 0 {
		return errors.New("the archive holds no files")
	}
	return nil
}

// projectInArchive works out which project an unpacked archive holds from the
// database inside it. A Toolkit backup is named for the day it was taken, so
// the archive's own file name is no guide.
func projectInArchive(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}

	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if name, ok := strings.CutSuffix(e.Name(), dbSuffix); ok {
			names = append(names, name)
		}
	}

	switch len(names) {
	case 0:
		return "", errors.New("the archive holds no .db file, so it is not a C-Gate project")
	case 1:
		return names[0], nil
	default:
		sort.Strings(names)
		return "", fmt.Errorf("the archive holds %d databases (%s), so it is not one project",
			len(names), strings.Join(names, ", "))
	}
}

func handleTagUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "upload requires POST")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "no file in upload: "+err.Error())
		return
	}
	defer file.Close()

	requested := strings.TrimSpace(r.FormValue("project"))

	switch kind := sniff(file); kind {
	case kindDatabase:
		uploadDatabase(w, file, header, requested)
	case kindZip, kindTar, kindTarGz:
		uploadArchive(w, file, header, requested, kind)
	default:
		writeJSONError(w, http.StatusBadRequest,
			"that is not a C-Gate project: expected a .db database, or a .cbz, .zip, .tar or .tar.gz archive of a project directory")
	}
}

// uploadDatabase replaces just the database file, leaving anything else in the
// project directory as it was.
//
// The file is written to a temporary file in the destination directory first,
// so a failed upload never touches the database in place. C-Gate holds an open
// project in memory and writes it back to disk, so it is told to stop and
// close the project before the swap, and to load and start it again
// afterwards. The previous database is kept alongside as <project>.db.bak.
func uploadDatabase(w http.ResponseWriter, file multipart.File, header *multipart.FileHeader, requested string) {
	project := requested
	if project == "" {
		project = strings.TrimSuffix(filepath.Base(header.Filename), dbSuffix)
	}
	if !projectNamePattern.MatchString(project) {
		writeJSONError(w, http.StatusBadRequest,
			fmt.Sprintf("invalid project name %q — letters, digits, - and _ only", project))
		return
	}

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	dest := dbPath(project)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not create the project directory: "+err.Error())
		return
	}

	// Write the upload out in full before disturbing anything: if this fails,
	// or if C-Gate is unreachable below, the existing database is untouched.
	tmp, err := os.CreateTemp(filepath.Dir(dest), project+".upload-*")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not create a temporary file: "+err.Error())
		return
	}
	size, err := io.Copy(tmp, file)
	if err == nil {
		err = tmp.Sync()
	}
	tmp.Close()
	if err != nil {
		os.Remove(tmp.Name())
		writeJSONError(w, http.StatusBadRequest, "upload failed: "+err.Error())
		return
	}

	notes := closeInCGate(project)

	backedUp := false
	if _, err := os.Stat(dest); err == nil {
		if err := os.Rename(dest, dest+backupSuffix); err != nil {
			os.Remove(tmp.Name())
			writeJSONError(w, http.StatusInternalServerError, "could not back up the existing database: "+err.Error())
			return
		}
		backedUp = true
	}

	if err := os.Rename(tmp.Name(), dest); err != nil {
		if backedUp {
			os.Rename(dest+backupSuffix, dest)
		}
		os.Remove(tmp.Name())
		writeJSONError(w, http.StatusInternalServerError, "could not install the uploaded database: "+err.Error())
		return
	}
	log.Printf("Tag database upload: %s (%d bytes, backup=%v)", dest, size, backedUp)

	notes = append(notes, loadInCGate(project)...)
	announce(append([]string{fmt.Sprintf("uploaded %s (%d bytes)", dest, size)}, notes...))

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"project":  project,
		"path":     dest,
		"size":     size,
		"files":    1,
		"backup":   backedUp,
		"cgate":    notes,
		"projects": listProjects(),
	})
}

// uploadArchive replaces a project's whole directory with the contents of an
// uploaded archive — Toolkit's .cbz backup, or any zip or tar of a project
// directory. The archive is unpacked into a staging directory beside the
// project first, so nothing in place is touched until a complete, plausible
// project has landed on disk. The previous directory is kept as <project>.bak.
func uploadArchive(w http.ResponseWriter, file multipart.File, header *multipart.FileHeader, requested string, kind uploadKind) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not create the projects directory: "+err.Error())
		return
	}
	staging, err := os.MkdirTemp(projectsDir, ".incoming-*")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not create a staging directory: "+err.Error())
		return
	}
	// A no-op once the staging directory has been renamed into place.
	defer os.RemoveAll(staging)

	switch kind {
	case kindZip:
		err = unpackZip(file, header.Size, staging)
	case kindTar:
		err = unpackTar(file, staging)
	case kindTarGz:
		var gz *gzip.Reader
		if gz, err = gzip.NewReader(file); err == nil {
			err = unpackTar(gz, staging)
			gz.Close()
		}
	}
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	found, err := projectInArchive(staging)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	project := found
	switch {
	case found != genericDBName && requested != "" && requested != found:
		writeJSONError(w, http.StatusBadRequest,
			fmt.Sprintf("the archive holds %s%s, not %s%s", found, dbSuffix, requested, dbSuffix))
		return
	case found == genericDBName && requested == "":
		// C-Gate's own PROJECT ARCHIVE zip: the database inside is generic, so
		// only the person uploading knows which project it is.
		writeJSONError(w, http.StatusBadRequest,
			"this archive holds C-Gate's generic tagdb.db — give the project name to install it as")
		return
	case found == genericDBName:
		project = requested
	}
	if !projectNamePattern.MatchString(project) {
		writeJSONError(w, http.StatusBadRequest,
			fmt.Sprintf("invalid project name %q — letters, digits, - and _ only", project))
		return
	}
	if project != found {
		if err := os.Rename(filepath.Join(staging, found+dbSuffix), filepath.Join(staging, project+dbSuffix)); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "could not name the database: "+err.Error())
			return
		}
	}

	size, files := projectContents(staging)
	notes := closeInCGate(project)

	dest := projectDir(project)
	backup := dest + backupSuffix
	backedUp := false
	if _, err := os.Stat(dest); err == nil {
		// Only the most recent backup is kept.
		if err := os.RemoveAll(backup); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "could not clear the previous backup: "+err.Error())
			return
		}
		if err := os.Rename(dest, backup); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "could not back up the existing project: "+err.Error())
			return
		}
		backedUp = true
	}

	if err := os.Rename(staging, dest); err != nil {
		if backedUp {
			os.Rename(backup, dest)
		}
		writeJSONError(w, http.StatusInternalServerError, "could not install the uploaded project: "+err.Error())
		return
	}

	log.Printf("Tag project upload: %s (%d files, %d bytes, backup=%v)", dest, files, size, backedUp)

	notes = append(notes, loadInCGate(project)...)
	announce(append([]string{fmt.Sprintf("uploaded %s (%d files, %d bytes)", dest, files, size)}, notes...))

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"project":  project,
		"path":     dest,
		"size":     size,
		"files":    files,
		"backup":   backedUp,
		"cgate":    notes,
		"projects": listProjects(),
	})
}

// closeInCGate makes C-Gate let go of a project before its files are replaced;
// openInCGate puts it back afterwards. C-Gate holds a loaded project in memory
// and writes it back to disk, so a swap underneath it would be lost.
//
// Only a project C-Gate has open needs stopping and closing. Asking it about
// one it has never heard of — the usual case for a first upload — answers
// "Project not found", which reads in the console log like the upload failed.
func closeInCGate(project string) []string {
	if !openInCGate(project) {
		return []string{"project " + project + " is not open in C-Gate, so there is nothing to close"}
	}
	notes := cgateProject("project stop " + project)
	return append(notes, cgateProject("project close "+project)...)
}

// openInCGate reports whether C-Gate currently holds the project in memory.
// PROJECT LIST answers with a line per open project: "123-project=HOME
// state=stopped".
func openInCGate(project string) bool {
	for _, line := range cgateTry("project list") {
		for _, field := range strings.Fields(line) {
			if field == "project="+project {
				return true
			}
		}
	}
	return false
}

func loadInCGate(project string) []string {
	notes := cgateProject("project load " + project)
	return append(notes, cgateProject("project start "+project)...)
}

func handleWS(ws *websocket.Conn) {
	hub.add(ws)
	defer hub.remove(ws)
	// Keep connection alive by reading (blocks until close)
	buf := make([]byte, 512)
	for {
		if _, err := ws.Read(buf); err != nil {
			break
		}
	}
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

func serveConsole(w http.ResponseWriter, r *http.Request) {
	data, _ := consoleHTML.ReadFile("console.html")
	w.Header().Set("Content-Type", "text/html")
	w.Write(data)
}

// normalizePath makes routing independent of how Home Assistant's ingress
// proxy presents the request.
//
// Supervisor builds ingress_url by joining the session path with the add-on's
// ingress_entry, which yields a trailing double slash ("/api/hassio_ingress/
// <token>//") and reaches us as "//". It also normally strips the session
// prefix, but that has not been consistent, so strip it here if present.
//
// This must not be done with http.ServeMux: it cleans the request path and
// answers 301 to the cleaned path whenever the two differ. Under ingress that
// redirect points the iframe at "/" on the Home Assistant origin, which loads
// the HA dashboard inside the add-on panel instead of this console.
func normalizePath(p string) string {
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	if rest, ok := strings.CutPrefix(p, ingressPrefix); ok {
		if i := strings.Index(rest, "/"); i >= 0 {
			p = rest[i:]
		} else {
			p = "/"
		}
	}
	if p == "" {
		p = "/"
	}
	return p
}

func route(wsHandler http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := normalizePath(r.URL.Path)
		log.Printf("%s %s -> %s", r.Method, r.URL.Path, p)

		switch p {
		case "/cgate":
			handleCGate(w, r)
		case "/health":
			handleHealth(w, r)
		case "/tag":
			handleTagList(w, r)
		case "/tag/download":
			handleTagDownload(w, r)
		case "/tag/archive":
			handleTagArchive(w, r)
		case "/tag/upload":
			handleTagUpload(w, r)
		case "/ws":
			wsHandler.ServeHTTP(w, r)
		default:
			serveConsole(w, r)
		}
	}
}

func main() {
	log.Printf("C-Gate Web Console starting on %s", listenAddr)

	// Start streaming from event and status ports
	go streamPort(cgateEventPort, "event")
	go streamPort(cgateStatusPort, "status")

	// Initialize command connection
	go func() {
		cmdSession.mu.Lock()
		cmdSession.connect()
		cmdSession.mu.Unlock()
	}()

	// Route explicitly rather than via http.ServeMux — see normalizePath.
	log.Fatal(http.ListenAndServe(listenAddr, route(websocket.Handler(handleWS))))
}
