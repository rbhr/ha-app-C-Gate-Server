package main

import (
	"bufio"
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
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
		s.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
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
// Project tag databases
//
// C-Gate keeps each project in its own directory under the tag directory, as
// <project>/<project>.db — a plain SQLite file. The console exposes those
// files so a project built in C-Bus Toolkit can be uploaded, and the running
// one taken away for backup.
// ---------------------------------------------------------------------------

const (
	// Project databases run to a few hundred KB. The cap is generous but
	// bounded so a stray request cannot fill /data.
	maxUploadBytes = 64 << 20
	dbSuffix       = ".db"
	backupSuffix   = ".bak"

	// How long an upload waits for C-Gate to acknowledge the commands that
	// close and reopen the project around the swap.
	cgateCommandTimeout = 15 * time.Second
)

// sqliteMagic is the header every C-Gate project database starts with.
// Checked on upload so the wrong file is rejected before it can replace a
// working database.
var sqliteMagic = []byte("SQLite format 3\x00")

// projectNamePattern is deliberately strict: the name is used to build a path
// under tagDir and is passed to C-Gate as a command argument.
var projectNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

var (
	// tagDir is /data/tag, which run.sh links to /cgate/tag.
	tagDir = envOr("CGATE_TAG_DIR", "/data/tag")
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
	Modified string `json:"modified"`
	Active   bool   `json:"active"`
}

// dbPath returns the file holding a project's tag database. C-Gate's own
// layout is <tag>/<project>/<project>.db; older add-on versions left a flat
// <tag>/<project>.db behind, so an existing flat file wins over a directory
// that does not exist yet.
func dbPath(project string) string {
	nested := filepath.Join(tagDir, project, project+dbSuffix)
	if _, err := os.Stat(nested); err == nil {
		return nested
	}
	flat := filepath.Join(tagDir, project+dbSuffix)
	if _, err := os.Stat(flat); err == nil {
		return flat
	}
	return nested
}

// listProjects finds every project database in the tag directory, in both the
// nested and flat layouts.
func listProjects() []projectDB {
	entries, err := os.ReadDir(tagDir)
	if err != nil {
		log.Printf("Tag directory %s unreadable: %v", tagDir, err)
		return nil
	}

	found := make(map[string]os.FileInfo)
	add := func(name, path string) {
		if !projectNamePattern.MatchString(name) {
			return
		}
		if _, seen := found[name]; seen {
			return
		}
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			found[name] = info
		}
	}

	// Directories first, so the nested layout wins for a project that has
	// both, matching dbPath.
	for _, e := range entries {
		if e.IsDir() {
			add(e.Name(), filepath.Join(tagDir, e.Name(), e.Name()+dbSuffix))
		}
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if name, ok := strings.CutSuffix(e.Name(), dbSuffix); ok {
			add(name, filepath.Join(tagDir, e.Name()))
		}
	}

	names := make([]string, 0, len(found))
	for name := range found {
		names = append(names, name)
	}
	sort.Strings(names)

	projects := make([]projectDB, 0, len(names))
	for _, name := range names {
		info := found[name]
		projects = append(projects, projectDB{
			Name:     name,
			Size:     info.Size(),
			Modified: info.ModTime().Format("2006-01-02 15:04:05"),
			Active:   name == activeProject,
		})
	}
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
func cgateTry(cmd string) []string {
	if !cmdSession.connected.Load() {
		return []string{"> " + cmd + "  (skipped — no C-Gate connection)"}
	}

	type reply struct {
		lines []string
		err   error
	}
	done := make(chan reply, 1)
	go func() {
		lines, err := cmdSession.send(cmd)
		done <- reply{lines, err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			return []string{"> " + cmd, "error: " + r.err.Error()}
		}
		return append([]string{"> " + cmd}, r.lines...)
	case <-time.After(cgateCommandTimeout):
		return []string{"> " + cmd, "error: no reply from C-Gate within " + cgateCommandTimeout.String()}
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

func handleTagDownload(w http.ResponseWriter, r *http.Request) {
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	if !projectNamePattern.MatchString(project) {
		writeJSONError(w, http.StatusBadRequest, "invalid project name")
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

// handleTagUpload replaces a project's tag database with an uploaded file.
//
// The file is written to a temporary file in the destination directory first,
// so a failed or rejected upload never touches the database in place. C-Gate
// holds an open project in memory and writes it back to disk, so it is told to
// stop and close the project before the swap, and to load and start it again
// afterwards. The previous database is kept alongside as <project>.db.bak.
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

	project := strings.TrimSpace(r.FormValue("project"))
	if project == "" {
		project = strings.TrimSuffix(filepath.Base(header.Filename), dbSuffix)
	}
	if !projectNamePattern.MatchString(project) {
		writeJSONError(w, http.StatusBadRequest,
			fmt.Sprintf("invalid project name %q — letters, digits, - and _ only", project))
		return
	}

	magic := make([]byte, len(sqliteMagic))
	if _, err := io.ReadFull(file, magic); err != nil || !bytes.Equal(magic, sqliteMagic) {
		writeJSONError(w, http.StatusBadRequest,
			"that is not a C-Gate project database (expected a SQLite .db file)")
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

	notes := cgateTry("project stop " + project)
	notes = append(notes, cgateTry("project close "+project)...)

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

	notes = append(notes, cgateTry("project load "+project)...)
	notes = append(notes, cgateTry("project start "+project)...)
	announce(append([]string{fmt.Sprintf("uploaded %s (%d bytes)", dest, size)}, notes...))

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"project":  project,
		"path":     dest,
		"size":     size,
		"backup":   backedUp,
		"cgate":    notes,
		"projects": listProjects(),
	})
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
