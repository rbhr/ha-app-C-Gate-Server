package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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

	// How long a single dial attempt gets before it is given up on.
	dialTimeout = 5 * time.Second

	// How long to wait before dialling again after a failed attempt.
	dialRetryInterval = 3 * time.Second

	// How often to send a heartbeat on the command connection to keep
	// it alive through NAT/firewalls and detect silent drops.
	commandHeartbeat = 2 * time.Minute

	// Total deadline, including queue time and writes, for an interactive command.
	commandReadDeadline = 5 * time.Second

	// Total deadline for the PROJECT commands an upload sends. Stopping
	// a started project means stopping its networks, which is not instant.
	projectReadDeadline = 20 * time.Second
)

// Each client has one writer and a bounded queue. A stalled browser never
// owns the hub lock while writing to a socket or blocks C-Gate stream readers.
const (
	wsQueueSize    = 32
	maxWSClients   = 64
	wsWriteTimeout = 2 * time.Second
)

type wsClientState struct {
	queue chan []byte
	done  chan struct{}
}
type wsHub struct {
	mu      sync.RWMutex
	clients map[*websocket.Conn]*wsClientState
}

func newHub() *wsHub { return &wsHub{clients: make(map[*websocket.Conn]*wsClientState)} }
func (h *wsHub) add(ws *websocket.Conn) {
	client := &wsClientState{queue: make(chan []byte, wsQueueSize), done: make(chan struct{})}
	h.mu.Lock()
	if len(h.clients) >= maxWSClients {
		h.mu.Unlock()
		ws.SetWriteDeadline(time.Now())
		ws.Close()
		return
	}
	h.clients[ws] = client
	h.mu.Unlock()
	go func() {
		defer h.remove(ws)
		for {
			select {
			case <-client.done:
				return
			case data := <-client.queue:
				ws.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
				if _, err := ws.Write(data); err != nil {
					return
				}
			}
		}
	}()
}
func (h *wsHub) remove(ws *websocket.Conn) {
	h.mu.Lock()
	client, ok := h.clients[ws]
	if ok {
		delete(h.clients, ws)
		close(client.done)
	}
	h.mu.Unlock()
	if ok {
		go func() { ws.SetWriteDeadline(time.Now()); ws.Close() }()
	}
}
func (h *wsHub) broadcast(msg map[string]string) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	var slow []*websocket.Conn
	h.mu.RLock()
	for ws, client := range h.clients {
		select {
		case client.queue <- data:
		default:
			slow = append(slow, ws)
		}
	}
	h.mu.RUnlock()
	for _, ws := range slow {
		h.remove(ws)
	}
}

var hub = newHub()

var commandRequests = make(chan struct{}, 32)

func admitCommand(w http.ResponseWriter) bool {
	select {
	case commandRequests <- struct{}{}:
		return true
	default:
		writeJSONError(w, http.StatusServiceUnavailable, "too many pending command requests")
		return false
	}
}

// Connection state for the health/ready endpoints. Written by the goroutines
// owning each connection, read by HTTP handlers, so these are atomic rather
// than guarded by the command session's mutex — a readiness probe must not
// block behind an in-flight command.
var (
	eventStreamUp  atomic.Bool
	statusStreamUp atomic.Bool
	commandUp      atomic.Bool
)

// dialCGate makes one attempt to connect to a C-Gate port, enabling TCP
// keepalive on success. It reports failure rather than retrying, so a caller
// holding a lock can bound how long it is prepared to wait.
func dialCGate(port string) (net.Conn, error) {
	addr := net.JoinHostPort(cgateHost, port)
	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		return nil, err
	}
	// Enable TCP keepalive so the OS detects dead connections
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(keepAliveInterval)
	}
	log.Printf("Connected to C-Gate %s", addr)
	return conn, nil
}

// dialCGateForever retries until it connects. Only safe on a goroutine that
// blocks nothing but itself — the stream readers qualify, the command session
// does not.
func dialCGateForever(port string) net.Conn {
	addr := net.JoinHostPort(cgateHost, port)
	for {
		conn, err := dialCGate(port)
		if err == nil {
			return conn
		}
		log.Printf("Waiting for C-Gate on %s: %v", addr, err)
		time.Sleep(dialRetryInterval)
	}
}

// streamPort reads lines from a C-Gate port and broadcasts them.
// Reconnects automatically when the connection drops.
//
// There is deliberately no read deadline on these connections. Both ends live
// in the same container (cgateHost is localhost), so "peer died without
// closing the socket" is not a failure mode reachable here — if C-Gate exits,
// the read returns EOF/RST straight away and the reconnect below handles it.
// A deadline could therefore only ever fire on a healthy but quiet port, and
// both ports are legitimately quiet: the event interface emits nothing at the
// default global-event-level, and the status interface goes silent on an idle
// site. Tearing the connection down in that case cost a reconnect every
// deadline period and dropped any line arriving during it. TCP keepalive (see
// dialCGate) stays as the backstop for the connection genuinely going away.
func streamPort(port, streamName string, up *atomic.Bool) {
	for {
		conn := dialCGateForever(port)
		up.Store(true)
		scanner := bufio.NewScanner(conn)
		// C-Gate lines are short, but don't let one unusually long line kill
		// the stream with ErrTooLong and send us into a reconnect loop.
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			hub.broadcast(map[string]string{
				"stream": streamName,
				"data":   scanner.Text(),
				"time":   time.Now().Format("15:04:05"),
			})
		}
		up.Store(false)
		if err := scanner.Err(); err != nil {
			log.Printf("Stream %s connection lost: %v — reconnecting", streamName, err)
		} else {
			log.Printf("Stream %s closed by C-Gate (EOF) — reconnecting", streamName)
		}
		conn.Close()
		time.Sleep(2 * time.Second)
	}
}

// contextMutex is a zero-value usable mutex whose wait can be cancelled.
// No goroutine is left behind to execute work after its caller has timed out.
type contextMutex struct {
	once  sync.Once
	token chan struct{}
}

func (m *contextMutex) init() { m.once.Do(func() { m.token = make(chan struct{}, 1) }) }
func (m *contextMutex) acquire(ctx context.Context) error {
	m.init()
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case m.token <- struct{}{}:
		if err := ctx.Err(); err != nil {
			m.Unlock()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (m *contextMutex) Lock()   { _ = m.acquire(context.Background()) }
func (m *contextMutex) Unlock() { <-m.token }
func (m *contextMutex) TryLock() bool {
	m.init()
	select {
	case m.token <- struct{}{}:
		return true
	default:
		return false
	}
}

const (
	maxCommandBytes   = 16 << 10
	maxReplyBytes     = 4 << 20
	maxReplyLineBytes = 1 << 20
	maxReplyLines     = 4096
	welcomeTimeout    = 10 * time.Second
)

var errCommandUnavailable = errors.New("C-Gate command connection is not ready")
var errCommandBusy = errors.New("C-Gate command queue is full")

// commandSession holds the persistent command connection and its reader.
type commandSession struct {
	mu        contextMutex
	conn      net.Conn
	reader    *bufio.Reader
	queueOnce sync.Once
	queue     chan struct{}
}

var cmdSession = &commandSession{}

// connect makes one bounded attempt. Only maintain calls this in production;
// HTTP requests fail promptly while it prepares a replacement connection.
func (s *commandSession) connect() error {
	commandUp.Store(false)
	conn, err := dialCGate(cgateCommandPort)
	if err != nil {
		return fmt.Errorf("connecting to C-Gate command port: %w", err)
	}
	return s.acceptWelcome(conn, welcomeTimeout)
}
func (s *commandSession) acceptWelcome(conn net.Conn, timeout time.Duration) error {
	reader := bufio.NewReader(conn)
	conn.SetDeadline(time.Now().Add(timeout))
	lines, code, err := readReply(reader)
	if err != nil || code != 201 {
		conn.Close()
		commandUp.Store(false)
		return fmt.Errorf("C-Gate welcome failed: %v (reply %q)", err, lines)
	}
	conn.SetDeadline(time.Time{})
	s.conn, s.reader = conn, reader
	commandUp.Store(true)
	return nil
}

// responseLine strips the numeric protocol prefix before interpreting fields.
func responseLine(line string) (code int, more bool, text string, err error) {
	if len(line) < 3 {
		return 0, false, "", errors.New("short C-Gate response")
	}
	for _, c := range line[:3] {
		if c < '0' || c > '9' {
			return 0, false, "", errors.New("invalid C-Gate response code")
		}
	}
	code, _ = strconv.Atoi(line[:3])
	if code < 100 || code > 599 {
		return 0, false, "", fmt.Errorf("unexpected C-Gate response code %d", code)
	}
	if len(line) == 3 {
		return code, false, "", nil
	}
	if line[3] != ' ' && line[3] != '-' {
		return 0, false, "", errors.New("invalid C-Gate response separator")
	}
	return code, line[3] == '-', line[4:], nil
}
func readReply(reader *bufio.Reader) ([]string, int, error) {
	var lines []string
	total, expected := 0, 0
	for len(lines) < maxReplyLines {
		var line []byte
		for {
			part, err := reader.ReadSlice('\n')
			if len(line)+len(part) > maxReplyLineBytes || total+len(line)+len(part) > maxReplyBytes {
				return lines, 0, errors.New("C-Gate reply exceeds size limit")
			}
			line = append(line, part...)
			if err == bufio.ErrBufferFull {
				continue
			}
			if err != nil {
				return lines, 0, fmt.Errorf("incomplete C-Gate reply: %w", err)
			}
			break
		}
		total += len(line)
		text := strings.TrimRight(string(line), "\r\n")
		code, more, _, err := responseLine(text)
		if err != nil {
			return lines, 0, err
		}
		if expected == 343 {
			// DBGETXML and DBGETP bracket 347 continuation lines with 343/344.
			if !((code == 347 && more) || (code == 344 && !more)) {
				return lines, 0, errors.New("invalid C-Gate XML response framing")
			}
		} else {
			if expected != 0 && code != expected {
				return lines, 0, errors.New("C-Gate response code changed before final line")
			}
			expected = code
		}
		lines = append(lines, text)
		if !more {
			return lines, code, nil
		}
	}
	return lines, 0, errors.New("C-Gate reply exceeds line limit")
}

// drop closes the command connection and marks the session down, leaving
// maintain() to rebuild it.
//
// Redialling here instead — which is what reconnect() used to do — means
// dialling with s.mu held. C-Gate is routinely down for minutes at a time (a
// restart, a project reload, a reboot) and the dial retried forever, so every
// request queued behind it for the whole outage while /health went on
// reporting a fixed "ok". Failing the request and letting a background
// goroutine wait is what keeps an outage off the HTTP path.
//
// Must be called with s.mu held.
func (s *commandSession) drop() {
	commandUp.Store(false)
	if s.conn != nil {
		s.conn.Close()
		s.conn = nil
		s.reader = nil
	}
}

// maintain keeps the command session connected and proves it is still alive.
//
// It is the only place that waits for C-Gate: it reconnects when the session
// is down, and once up sends a periodic noop to detect a silent drop before a
// real command runs into it.
func (s *commandSession) maintain() {
	// Polled on the short interval rather than slept through the heartbeat
	// one, so a session that drops between heartbeats is rebuilt promptly. On
	// the long interval the loop would not notice for up to commandHeartbeat,
	// leaving recovery to whichever request happened to come along next —
	// which on an idle console is no recovery at all.
	nextHeartbeat := time.Now().Add(commandHeartbeat)
	for {
		switch {
		case !commandUp.Load():
			s.mu.Lock()
			s.drop()
			err := s.connect()
			s.mu.Unlock()
			if err != nil {
				log.Printf("Command session: %v — retrying in %s", err, dialRetryInterval)
			} else {
				log.Printf("Command session: ready")
				nextHeartbeat = time.Now().Add(commandHeartbeat)
			}

		case time.Now().After(nextHeartbeat):
			if _, err := s.send("noop"); err != nil {
				log.Printf("Command session: heartbeat failed: %v", err)
			}
			nextHeartbeat = time.Now().Add(commandHeartbeat)
		}

		time.Sleep(dialRetryInterval)
	}
}

func (s *commandSession) send(cmd string) ([]string, error) {
	return s.sendWithin(cmd, commandReadDeadline)
}
func (s *commandSession) sendWithin(cmd string, timeout time.Duration) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return s.sendContext(ctx, cmd)
}
func validCommand(cmd string) bool {
	return strings.TrimSpace(cmd) != "" && len(cmd) <= maxCommandBytes && !strings.ContainsAny(cmd, "\r\n\x00")
}
func (s *commandSession) sendContext(ctx context.Context, cmd string) ([]string, error) {
	if !validCommand(cmd) {
		return nil, errors.New("expected one command without newlines, at most 16384 bytes")
	}
	s.queueOnce.Do(func() { s.queue = make(chan struct{}, 32) })
	select {
	case s.queue <- struct{}{}:
		defer func() { <-s.queue }()
	default:
		return nil, errCommandBusy
	}
	if err := s.mu.acquire(ctx); err != nil {
		return nil, err
	}
	defer s.mu.Unlock()
	if s.conn == nil {
		return nil, errCommandUnavailable
	}
	conn := s.conn
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(commandReadDeadline)
	}
	conn.SetDeadline(deadline)
	cancelled := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Now()); close(cancelled) })
	defer func() {
		if !stop() {
			<-cancelled
		}
		conn.SetDeadline(time.Time{})
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Never resend a failed write: some bytes may already have taken effect.
	if _, err := fmt.Fprintf(conn, "%s\r\n", cmd); err != nil {
		s.drop()
		return nil, fmt.Errorf("command write failed; outcome unknown: %w", err)
	}
	lines, _, err := readReply(s.reader)
	if err != nil {
		s.drop()
		return lines, fmt.Errorf("command outcome unknown: %w", err)
	}
	return lines, nil
}

func handleCGate(w http.ResponseWriter, r *http.Request) {
	if !admitCommand(w) {
		return
	}
	defer func() { <-commandRequests }()
	cmd := r.URL.Query().Get("cmd")
	if !validCommand(cmd) {
		writeJSONError(w, http.StatusBadRequest, "expected one command without newlines, at most 16384 bytes")
		return
	}

	timeout := commandReadDeadline
	if fields := strings.Fields(cmd); len(fields) > 0 && strings.EqualFold(fields[0], "project") {
		timeout = projectReadDeadline
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	if err := projectOperations.acquire(ctx); err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	locked := true
	defer func() {
		if locked {
			projectOperations.Unlock()
		}
	}()
	if pendingTransactions() {
		projectOperations.Unlock()
		locked = false
		writeJSONError(w, http.StatusServiceUnavailable, "project recovery is pending")
		return
	}
	lines, err := cmdSession.sendContext(ctx, cmd)
	projectOperations.Unlock()
	locked = false
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
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
	writeJSON(w, http.StatusOK, map[string]interface{}{
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
	State    string `json:"state"`
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
			State:    "unknown",
		})
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].Name < projects[j].Name })
	return projects
}

func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Second))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
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
	if !admitCommand(w) {
		return
	}
	defer func() { <-commandRequests }()
	ctx, cancel := context.WithTimeout(r.Context(), commandReadDeadline)
	defer cancel()
	if err := projectOperations.acquire(ctx); err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	locked := true
	defer func() {
		if locked {
			projectOperations.Unlock()
		}
	}()
	projects := listProjects()
	states, err := projectStates(ctx)
	projectOperations.Unlock()
	locked = false
	active := ""
	for name, state := range states {
		if state == "started" && (active == "" || name < active) {
			active = name
		}
	}
	if states[activeProject] == "started" {
		active = activeProject
	}
	for i := range projects {
		if err == nil {
			projects[i].State = states[projects[i].Name]
			if projects[i].State == "" {
				projects[i].State = "closed"
			}
		}
		projects[i].Active = states[projects[i].Name] == "started"
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"active": active, "configured": activeProject, "state_known": err == nil, "projects": projects})
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
	serveProjectSnapshot(w, r, false, false)
}

// archiveSuffix picks the extension for a project archive. The bytes are the
// same either way: a flat zip of the project directory is exactly the shape of
// the .cbz backup Toolkit writes. The extension is what decides whether
// Toolkit offers to restore the file, so it is worth being able to ask for.
func archiveSuffix(r *http.Request) string {
	if strings.EqualFold(r.URL.Query().Get("format"), "cbz") {
		return ".cbz"
	}
	return ".zip"
}

func handleTagArchive(w http.ResponseWriter, r *http.Request) {
	serveProjectSnapshot(w, r, true, false)
}
func handleTagBackup(w http.ResponseWriter, r *http.Request) { serveProjectSnapshot(w, r, true, true) }

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
	if strings.HasSuffix(name, ".db-wal") || strings.HasSuffix(name, ".db-shm") || strings.HasSuffix(name, ".db-journal") {
		return "", errors.New("archive contains a live SQLite journal; export a standalone project backup")
	}
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
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
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
		if e.FileInfo().IsDir() {
			continue
		}
		if !e.Mode().IsRegular() {
			return fmt.Errorf("unsupported archive entry %q", e.Name)
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
		if h.Typeflag == tar.TypeDir {
			continue
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA {
			return fmt.Errorf("unsupported archive entry %q", h.Name)
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
	release, err := reserveTransfer()
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	defer release()
	controller := http.NewResponseController(w)
	controller.SetReadDeadline(time.Now().Add(time.Minute))
	defer controller.SetReadDeadline(time.Time{})
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	// Stream multipart data into the project filesystem; no unbounded memory
	// buffer or spill files on another filesystem with a different free budget.
	reader, err := r.MultipartReader()
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	staging, err := os.MkdirTemp(projectsDir, ".incoming-*")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer os.RemoveAll(staging)
	var requested, filename string
	var upload *os.File
	for parts := 0; ; parts++ {
		part, e := reader.NextPart()
		if e == io.EOF {
			break
		}
		if e != nil {
			writeJSONError(w, http.StatusBadRequest, e.Error())
			return
		}
		if parts >= 3 {
			part.Close()
			writeJSONError(w, http.StatusBadRequest, "too many upload fields")
			return
		}
		switch part.FormName() {
		case "project":
			value, e := io.ReadAll(io.LimitReader(part, 65))
			if e != nil || len(value) > 64 {
				part.Close()
				writeJSONError(w, http.StatusBadRequest, "invalid project name")
				return
			}
			requested = strings.TrimSpace(string(value))
		case "file":
			if upload != nil {
				part.Close()
				writeJSONError(w, http.StatusBadRequest, "only one uploaded file is allowed")
				return
			}
			filename = part.FileName()
			upload, err = os.Create(filepath.Join(staging, "upload"))
			if err != nil {
				part.Close()
				writeJSONError(w, http.StatusInternalServerError, err.Error())
				return
			}
			defer upload.Close()
			_, err = io.Copy(upload, part)
			if err != nil {
				part.Close()
				writeJSONError(w, http.StatusBadRequest, err.Error())
				return
			}
		default:
			part.Close()
			writeJSONError(w, http.StatusBadRequest, "unknown upload field")
			return
		}
		part.Close()
	}
	controller.SetReadDeadline(time.Time{})
	if upload == nil {
		writeJSONError(w, http.StatusBadRequest, "no file in upload")
		return
	}
	if requested != "" && !projectNamePattern.MatchString(requested) {
		writeJSONError(w, http.StatusBadRequest, "invalid project name")
		return
	}
	info, err := upload.Stat()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	kind := sniff(upload)
	candidate := filepath.Join(staging, "project")
	if err = os.Mkdir(candidate, 0755); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	project := ""
	if kind == kindDatabase {
		project = requested
		if project == "" {
			project = strings.TrimSuffix(filepath.Base(filename), dbSuffix)
		}
		if !projectNamePattern.MatchString(project) {
			writeJSONError(w, http.StatusBadRequest, "invalid project name")
			return
		}
		err = os.Rename(upload.Name(), filepath.Join(candidate, project+dbSuffix))
	} else {
		upload.Seek(0, io.SeekStart)
		switch kind {
		case kindZip:
			err = unpackZip(upload, info.Size(), candidate)
		case kindTar:
			err = unpackTar(upload, candidate)
		case kindTarGz:
			var gz *gzip.Reader
			gz, err = gzip.NewReader(upload)
			if err == nil {
				err = unpackTar(gz, candidate)
				gz.Close()
			}
		default:
			writeJSONError(w, http.StatusBadRequest, "expected a .db database or .cbz, .zip, .tar or .tar.gz project archive")
			return
		}
		if err == nil {
			candidate, project, err = archiveProject(candidate)
			if err == nil {
				if project == genericDBName {
					if requested == "" {
						err = errors.New("give a project name for C-Gate's generic tagdb.db archive")
					} else {
						err = os.Rename(filepath.Join(candidate, project+dbSuffix), filepath.Join(candidate, requested+dbSuffix))
						project = requested
					}
				} else if requested != "" && requested != project {
					err = fmt.Errorf("the archive holds %s.db, not %s.db", project, requested)
				}
			}
		}
	}
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !projectNamePattern.MatchString(project) {
		writeJSONError(w, http.StatusBadRequest, "invalid project name")
		return
	}
	if err = validateDatabase(r.Context(), filepath.Join(candidate, project+dbSuffix)); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Waiting is cancellable. Once mutation starts, finish or roll back within a
	// fixed budget even if the browser disconnects halfway through replacement.
	waiting, cancelWait := context.WithTimeout(r.Context(), transactionTimeout)
	defer cancelWait()
	if err = projectOperations.acquire(waiting); err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	locked := true
	defer func() {
		if locked {
			projectOperations.Unlock()
		}
	}()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), transactionTimeout)
	defer cancel()
	notes, backedUp, err := installProject(ctx, project, candidate, kind == kindDatabase)
	size, files := projectContents(projectDir(project))
	projects := listProjects()
	projectOperations.Unlock()
	locked = false
	announce(notes)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"project": project, "path": projectDir(project), "size": size, "files": files, "backup": backedUp, "cgate": notes, "projects": projects})
}

// Accept flat Toolkit/C-Gate archives or exactly one enclosing directory.
func archiveProject(dir string) (string, string, error) {
	found, err := projectInArchive(dir)
	if err == nil {
		return dir, found, nil
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		return "", "", readErr
	}
	if len(entries) == 1 && entries[0].IsDir() {
		nested := filepath.Join(dir, entries[0].Name())
		found, err = projectInArchive(nested)
		if err == nil {
			return nested, found, nil
		}
	}
	return "", "", err
}

func handleWS(ws *websocket.Conn) {
	// HTTP request deadlines end at the upgrade; idle WebSockets remain open.
	ws.SetReadDeadline(time.Time{})
	ws.SetWriteDeadline(time.Time{})
	ws.MaxPayloadBytes = 1024
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

// writeStatus renders the shared health/ready body.
func writeStatus(w http.ResponseWriter, code int) {
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Second))
	event, status, command := eventStreamUp.Load(), statusStreamUp.Load(), commandUp.Load()
	state := "degraded"
	if event && status && command && !pendingTransactions() {
		state = "ok"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": state,
		"connections": map[string]bool{
			"command": command,
			"event":   event,
			"status":  status,
		},
	})
}

// handleHealth reports liveness: the bridge is up and serving. It returns 200
// even when C-Gate is unreachable, because callers use this to decide whether
// to restart, and C-Gate needs up to a minute to sync its networks on a cold
// start — failing the probe during that window would turn a normal startup
// into a restart loop. The body carries the real detail; it used to be a fixed
// "ok" that said nothing about whether C-Gate was actually there.
// Gate on /ready instead if you need C-Gate itself to be up.
func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeStatus(w, http.StatusOK)
}

// handleReady reports readiness: every connection the bridge needs is
// established. Returns 503 until then, so clients can hold off their initial
// poll rather than retrying into "408 Operation failed" while C-Gate starts.
//
// This tracks the bridge's own TCP connections, not C-Gate's network state —
// a project can still be mid-sync when this first returns 200.
func handleReady(w http.ResponseWriter, r *http.Request) {
	code := http.StatusOK
	if !(eventStreamUp.Load() && statusStreamUp.Load() && commandUp.Load()) || pendingTransactions() {
		code = http.StatusServiceUnavailable
	}
	writeStatus(w, code)
}

func serveConsole(w http.ResponseWriter, r *http.Request) {
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Second))
	data, _ := consoleHTML.ReadFile("console.html")
	w.Header().Set("Content-Type", "text/html")
	w.Write(data)
}

// normalizePath makes routing independent of how Home Assistant's ingress
// proxy presents the request.
//
// Its live job is the session prefix: Supervisor normally strips
// "/api/hassio_ingress/<token>" before proxying, but that has not been
// consistent, so strip it here if present.
//
// Collapsing repeated slashes is now a guard rather than a fix. Supervisor
// joins the session path with the add-on's ingress_entry *relative* to a URL
// that already ends in a slash, so this add-on's old "ingress_entry: /"
// produced ".../<token>//" and every request arrived as "//". The key has been
// dropped, and Supervisor now asks for "/" — but the collapse stays, because
// reintroducing the key must not break the panel again.
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
		case "/ready":
			handleReady(w, r)
		case "/tag":
			handleTagList(w, r)
		case "/tag/download":
			handleTagDownload(w, r)
		case "/tag/archive":
			handleTagArchive(w, r)
		case "/tag/backup":
			handleTagBackup(w, r)
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
	if err := cleanupStaging(); err != nil {
		log.Fatal(err)
	}
	if len(os.Args) == 2 && os.Args[1] == "-recover" {
		if err := recoverTransactions(context.Background(), false); err != nil {
			log.Fatal(err)
		}
		return
	}
	log.Printf("C-Gate Web Console starting on %s", listenAddr)

	// Start streaming from event and status ports
	go streamPort(cgateEventPort, "event", &eventStreamUp)
	go streamPort(cgateStatusPort, "status", &statusStreamUp)

	// Keep the command connection up. Backgrounded rather than blocking
	// startup: the console has to be reachable while C-Gate is down, which is
	// exactly when someone wants to look at it.
	go cmdSession.maintain()
	go recoverWhileRunning()

	// Route explicitly rather than via http.ServeMux — see normalizePath.
	server := &http.Server{Addr: listenAddr, Handler: route(websocket.Handler(handleWS)), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: time.Minute, WriteTimeout: 5 * time.Minute, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	log.Fatal(server.ListenAndServe())
}
