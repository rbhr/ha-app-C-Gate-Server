package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"golang.org/x/net/websocket"
	"net"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func reviewSession(t *testing.T, respond func(string) string) (*commandSession, *[]string, *sync.Mutex) {
	t.Helper()
	client, peer := net.Pipe()
	s := &commandSession{conn: client, reader: bufio.NewReader(client)}
	previous, wasUp := cmdSession, commandUp.Load()
	cmdSession = s
	commandUp.Store(true)
	var commands []string
	mu := &sync.Mutex{}
	done := make(chan struct{})
	replies := make(chan string, 32)
	go func() {
		for response := range replies {
			if _, err := fmt.Fprint(peer, response); err != nil {
				return
			}
		}
	}()
	go func() {
		defer close(done)
		defer close(replies)
		scan := bufio.NewScanner(peer)
		for scan.Scan() {
			cmd := scan.Text()
			mu.Lock()
			commands = append(commands, cmd)
			mu.Unlock()
			replies <- respond(cmd)
		}
	}()
	t.Cleanup(func() {
		client.Close()
		peer.Close()
		<-done
		s.mu.Lock()
		s.mu.Unlock()
		cmdSession = previous
		commandUp.Store(wasUp)
	})
	return s, &commands, mu
}

func TestReviewOpenProjectContinuation(t *testing.T) {
	reviewSession(t, func(string) string {
		return "123-project=HOME state=started\r\n123 project=OTHER state=stopped\r\n"
	})
	states, err := projectStates(context.Background())
	if err != nil || states["HOME"] != "started" {
		t.Fatal("HOME is open in a continuation line, but openInCGate returned false; stop/close will be skipped")
	}
}

func TestReviewUploadMustHonorCloseFailure(t *testing.T) {
	useTempProjectsDir(t)
	writeDB(t, dbPath("HOME"), "old good copy")
	old, _ := os.ReadFile(dbPath("HOME"))
	_, commands, mu := reviewSession(t, func(cmd string) string {
		if cmd == "project list" {
			return "123 project=HOME state=started\r\n"
		}
		return "400 Operation failed\r\n"
	})
	rec := httptest.NewRecorder()
	handleTagUpload(rec, uploadRequest(t, "HOME", "HOME.db", db("replacement")))
	got, _ := os.ReadFile(dbPath("HOME"))
	mu.Lock()
	defer mu.Unlock()
	if rec.Code == 200 || !bytes.Equal(got, old) {
		t.Fatalf("failed stop/close still returned HTTP %d and replaced database=%v; commands=%v", rec.Code, !bytes.Equal(got, old), *commands)
	}
}

func TestReviewIncompleteReplyMustDropSession(t *testing.T) {
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	s := &commandSession{conn: client, reader: bufio.NewReader(client)}
	commandUp.Store(true)
	defer commandUp.Store(false)
	go func() {
		bufio.NewReader(peer).ReadString('\n')
		fmt.Fprint(peer, "123-first part\r\n")
	}()
	lines, err := s.sendWithin("project list", 20*time.Millisecond)
	if err == nil || s.conn != nil {
		t.Fatalf("unfinished multiline response returned lines=%q err=%v and retained session=%v", lines, err, s.conn != nil)
	}
}

func TestReviewRejectEmbeddedCommandNewlines(t *testing.T) {
	s, _, _ := reviewSession(t, func(cmd string) string { return "200 " + cmd + "\r\n" })
	first, err := s.send("noop\r\nversion")
	if err != nil {
		return
	}
	second, err := s.send("project save HOME")
	if err == nil && len(second) > 0 && second[0] != "200 project save HOME" {
		t.Fatalf("embedded newline executes an extra command and assigns its response to the next request: first=%q second=%q", first, second)
	}
}

func TestReviewRejectArchiveWithInvalidDatabase(t *testing.T) {
	useTempProjectsDir(t)
	writeDB(t, dbPath("HOME"), "original")
	before, _ := os.ReadFile(dbPath("HOME"))
	rec := httptest.NewRecorder()
	handleTagUpload(rec, uploadRequest(t, "HOME", "backup.zip", zipBytes(t, map[string][]byte{"HOME.db": []byte("not SQLite at all")})))
	after, _ := os.ReadFile(dbPath("HOME"))
	if rec.Code == 200 || !bytes.Equal(before, after) {
		t.Fatalf("archive with text HOME.db returned HTTP %d and replaced old database=%v", rec.Code, !bytes.Equal(before, after))
	}
}

func TestReviewAcceptArchiveOfProjectDirectory(t *testing.T) {
	useTempProjectsDir(t)
	rec := httptest.NewRecorder()
	handleTagUpload(rec, uploadRequest(t, "HOME", "project.tar", tarBytes(t, map[string][]byte{"HOME/HOME.db": db("project")}, false)))
	if rec.Code != 200 {
		t.Fatalf("ordinary tar of HOME directory rejected: HTTP %d %s", rec.Code, rec.Body.String())
	}
}

func TestReviewAbandonedCommandMustNotExecute(t *testing.T) {
	s, commands, mu := reviewSession(t, func(string) string { return "200 OK\r\n" })
	s.mu.Lock()
	done := make(chan error, 1)
	go func() { _, err := s.sendWithin("project stop HOME", 0); done <- err }()
	result := <-done // Cancellation expires before the mutex is released.
	s.mu.Unlock()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(*commands)
		mu.Unlock()
		if n > 0 {
			t.Fatalf("command executed after caller had received timeout: %v", result)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSlowWebsocketDoesNotBlockBroadcast(t *testing.T) {
	h := newHub()
	accepted := make(chan *websocket.Conn, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) { h.add(ws); accepted <- ws; <-release }))
	defer srv.Close()
	client, err := websocket.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), "", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server := <-accepted
	defer close(release)
	defer h.remove(server)
	done := make(chan struct{})
	go func() {
		for i := 0; i < wsQueueSize+10; i++ {
			h.broadcast(map[string]string{"data": strings.Repeat("x", 256<<10)})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("broadcast blocked behind a non-reading client")
	}
	if !h.mu.TryLock() {
		t.Fatal("hub lock held by slow client")
	}
	h.mu.Unlock()
}

func TestWelcomeMustBeComplete(t *testing.T) {
	for _, reply := range []string{"", "421 Connection refused\r\n", "201-partial\r\n", "200 OK\r\n"} {
		t.Run(reply, func(t *testing.T) {
			client, peer := net.Pipe()
			defer peer.Close()
			go func() { fmt.Fprint(peer, reply) }()
			s := &commandSession{}
			if err := s.acceptWelcome(client, 20*time.Millisecond); err == nil {
				t.Fatal("invalid welcome accepted")
			}
			if commandUp.Load() || s.conn != nil {
				t.Fatal("failed welcome reported ready")
			}
		})
	}
	client, peer := net.Pipe()
	defer peer.Close()
	defer client.Close()
	s := &commandSession{}
	done := make(chan error, 1)
	go func() { done <- s.acceptWelcome(client, time.Second) }()
	time.Sleep(30 * time.Millisecond)
	if commandUp.Load() {
		t.Fatal("ready before welcome")
	}
	fmt.Fprint(peer, "201 Service ready\r\n")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	commandUp.Store(false)
}

func TestCommandCancellationAndLimits(t *testing.T) {
	t.Run("cancel blocked write", func(t *testing.T) {
		client, peer := net.Pipe()
		defer client.Close()
		defer peer.Close()
		s := &commandSession{conn: client, reader: bufio.NewReader(client)}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { _, err := s.sendContext(ctx, "noop"); done <- err }()
		cancel()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("cancelled write succeeded")
			}
		case <-time.After(time.Second):
			t.Fatal("cancelled write blocked")
		}
	})
	for _, reply := range []string{"not a response\r\n", "123-first\r\n200 OK\r\n", strings.Repeat("x", maxReplyLineBytes+1) + "\r\n"} {
		t.Run("bad framing", func(t *testing.T) {
			s, _, _ := reviewSession(t, func(string) string { return reply })
			if _, err := s.sendWithin("noop", time.Second); err == nil || s.conn != nil {
				t.Fatal("bad response retained session")
			}
		})
	}
}

func TestXMLReplyFramingAndFollowingCommand(t *testing.T) {
	s, _, _ := reviewSession(t, func(cmd string) string {
		if cmd == "dbgetxml //HOME" {
			return "343-Begin XML snippet\r\n347-<?xml version=\"1.0\"?>\r\n347-<Installation/>\r\n344 End XML snippet\r\n"
		}
		return "200 OK.\r\n"
	})
	lines, err := s.send("dbgetxml //HOME")
	if err != nil || len(lines) != 4 {
		t.Fatalf("XML reply: %q %v", lines, err)
	}
	lines, err = s.send("noop")
	if err != nil || len(lines) != 1 || lines[0] != "200 OK." {
		t.Fatalf("following reply: %q %v", lines, err)
	}
}
