package main

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReplacementRefusesUnknownState(t *testing.T) {
	useTempProjectsDir(t)
	writeDB(t, dbPath("HOME"), "original")
	reviewSession(t, func(string) string { return "408 Unable to list projects\r\n" })
	w := httptest.NewRecorder()
	handleTagUpload(w, uploadRequest(t, "HOME", "HOME.db", db("new")))
	if w.Code == 200 {
		t.Fatal("upload succeeded with unknown project state")
	}
	got, _ := os.ReadFile(dbPath("HOME"))
	if !bytes.Equal(got, db("original")) {
		t.Fatal("unknown project state replaced original")
	}
	if pendingTransactions() {
		t.Fatal("unknown initial state should not create a transaction")
	}
}
func TestReplacementRollsBackFailedLoad(t *testing.T) {
	useTempProjectsDir(t)
	writeDB(t, dbPath("HOME"), "original")
	state := "started"
	loads := 0
	reviewSession(t, func(cmd string) string {
		switch cmd {
		case "project list":
			if state == "" {
				return "200 OK.\r\n"
			}
			return "123 project=HOME state=" + state + "\r\n"
		case "project stop HOME":
			state = "stopped"
		case "project close HOME":
			state = ""
		case "project load HOME":
			loads++
			if loads == 1 {
				return "408 Invalid project schema\r\n"
			}
			state = "stopped"
		case "project start HOME":
			state = "started"
		}
		return "200 OK.\r\n"
	})
	w := httptest.NewRecorder()
	handleTagUpload(w, uploadRequest(t, "HOME", "HOME.db", db("new")))
	if w.Code == 200 {
		t.Fatal("failed load returned success")
	}
	got, _ := os.ReadFile(dbPath("HOME"))
	if !bytes.Equal(got, db("original")) {
		t.Fatal("failed load did not restore original")
	}
	states, err := projectStates(context.Background())
	if err != nil || states["HOME"] != "started" {
		t.Fatalf("prior runtime state not restored: %v %v", states, err)
	}
	if pendingTransactions() {
		t.Fatal("successful rollback left a transaction")
	}
}
func TestTransactionCrashRecovery(t *testing.T) {
	for _, point := range []string{"before-moves", "old-moved", "new-installed", "rollback-old-restored", "committed", "backup-finalized"} {
		t.Run(point, func(t *testing.T) {
			useTempProjectsDir(t)
			writeDB(t, dbPath("HOME"), "original")
			tx := projectTransaction{Project: "HOME", HadOld: true, PreviousState: "started", Swapping: true}
			if err := writeTransaction(tx); err != nil {
				t.Fatal(err)
			}
			dir := transactionDir("HOME")
			newDir := filepath.Join(dir, "new")
			writeDB(t, filepath.Join(newDir, "HOME.db"), "candidate")
			if point != "before-moves" {
				if err := durableRename(projectDir("HOME"), filepath.Join(dir, "old")); err != nil {
					t.Fatal(err)
				}
			}
			if point != "before-moves" && point != "old-moved" {
				if err := durableRename(newDir, projectDir("HOME")); err != nil {
					t.Fatal(err)
				}
			}
			if point == "rollback-old-restored" {
				os.Rename(projectDir("HOME"), filepath.Join(dir, "failed"))
				os.Rename(filepath.Join(dir, "old"), projectDir("HOME"))
			}
			if point == "committed" || point == "backup-finalized" {
				tx.Committed = true
				if err := writeTransaction(tx); err != nil {
					t.Fatal(err)
				}
			}
			if point == "backup-finalized" {
				if err := durableRename(filepath.Join(dir, "old"), projectDir("HOME")+backupSuffix); err != nil {
					t.Fatal(err)
				}
			}
			if err := recoverTransactions(context.Background(), false); err != nil {
				t.Fatal(err)
			}
			want := "original"
			if tx.Committed {
				want = "candidate"
			}
			got, err := os.ReadFile(dbPath("HOME"))
			if err != nil || !bytes.Equal(got, db(want)) {
				t.Fatalf("wrong database after %s recovery", point)
			}
			if pendingTransactions() {
				t.Fatal("recovery left transaction")
			}
			if err := recoverTransactions(context.Background(), false); err != nil {
				t.Fatal("recovery is not idempotent", err)
			}
		})
	}
}
func TestRecoveryRefusesSwapWhileClosureFails(t *testing.T) {
	useTempProjectsDir(t)
	writeDB(t, dbPath("HOME"), "candidate")
	tx := projectTransaction{Project: "HOME", HadOld: true, PreviousState: "started", Swapping: true}
	if err := writeTransaction(tx); err != nil {
		t.Fatal(err)
	}
	writeDB(t, filepath.Join(transactionDir("HOME"), "old", "HOME.db"), "original")
	reviewSession(t, func(cmd string) string {
		if cmd == "project list" {
			return "123 project=HOME state=started\r\n"
		}
		return "408 Cannot stop\r\n"
	})
	if err := recoverTransactions(context.Background(), true); err == nil {
		t.Fatal("recovery ignored stop refusal")
	}
	got, _ := os.ReadFile(dbPath("HOME"))
	if !bytes.Equal(got, db("candidate")) {
		t.Fatal("recovery swapped underneath a running project")
	}
	if !pendingTransactions() {
		t.Fatal("unresolved recovery record was deleted")
	}
	w := httptest.NewRecorder()
	handleReady(w, httptest.NewRequest("GET", "/ready", nil))
	if w.Code != 503 {
		t.Fatal("ready during unresolved recovery")
	}
}
func TestBackupReportsProtocolSaveFailure(t *testing.T) {
	useTempProjectsDir(t)
	writeDB(t, dbPath("HOME"), "last saved")
	reviewSession(t, func(string) string { return "408 Save failed\r\n" })
	w := httptest.NewRecorder()
	handleTagBackup(w, httptest.NewRequest("POST", "/tag/backup?project=HOME&format=cbz", nil))
	if w.Code != 200 {
		t.Fatalf("offline fallback = %d %s", w.Code, w.Body.String())
	}
	if w.Header().Get("X-CGate-Saved") != "false" || w.Header().Get("X-CGate-Warning") == "" {
		t.Fatal("failed save lacked stale-backup warning")
	}
	if _, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len())); err != nil {
		t.Fatal(err)
	}
}
func TestBackupFailsBeforeWritingAnyArchive(t *testing.T) {
	useTempProjectsDir(t)
	writeDB(t, dbPath("HOME"), "original")
	if err := os.Symlink("/missing-companion", filepath.Join(projectDir("HOME"), "label.bmp")); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	handleTagArchive(w, httptest.NewRequest("GET", "/tag/archive?project=HOME", nil))
	if w.Code == 200 || w.Header().Get("Content-Type") == "application/zip" {
		t.Fatal("incomplete snapshot was served as an archive")
	}
	if _, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len())); err == nil {
		t.Fatal("partial valid ZIP returned")
	}
}
func TestConcurrentUploadWaitIsCancelled(t *testing.T) {
	useTempProjectsDir(t)
	writeDB(t, dbPath("HOME"), "original")
	projectOperations.Lock()
	req := uploadRequest(t, "HOME", "HOME.db", db("candidate"))
	ctx, cancel := context.WithTimeout(req.Context(), 30*time.Millisecond)
	defer cancel()
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { handleTagUpload(w, req); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		projectOperations.Unlock()
		t.Fatal("cancelled upload remained queued")
	}
	projectOperations.Unlock()
	if w.Code == 200 {
		t.Fatal("cancelled upload succeeded")
	}
	got, _ := os.ReadFile(dbPath("HOME"))
	if !bytes.Equal(got, db("original")) {
		t.Fatal("cancelled upload changed files")
	}
}
func TestDatabaseValidationRejectsTruncationAndOtherSchemas(t *testing.T) {
	useTempProjectsDir(t)
	for _, data := range [][]byte{[]byte("not sqlite"), append([]byte{}, sqliteMagic...), db("fixture")[:1024]} {
		f := filepath.Join(t.TempDir(), "bad.db")
		os.WriteFile(f, data, 0600)
		if err := validateDatabase(context.Background(), f); err == nil {
			t.Fatal("invalid database accepted")
		}
	}
	other := filepath.Join(t.TempDir(), "unrelated.db")
	if out, err := exec.Command("sqlite3", "-init", "/dev/null", other, "CREATE TABLE unrelated (id INTEGER);").CombinedOutput(); err != nil {
		t.Fatalf("fixture failed: %v %s", err, out)
	}
	if err := validateDatabase(context.Background(), other); err == nil {
		t.Fatal("ordinary SQLite file accepted as a C-Gate project")
	}
}
func TestArchiveRejectsDuplicateFiles(t *testing.T) {
	var body bytes.Buffer
	zw := zip.NewWriter(&body)
	for i := 0; i < 2; i++ {
		w, _ := zw.Create("HOME.db")
		fmt.Fprint(w, "contents")
	}
	zw.Close()
	if err := unpackZip(bytes.NewReader(body.Bytes()), int64(body.Len()), t.TempDir()); err == nil {
		t.Fatal("duplicate archive entry accepted")
	}
}
func TestTagListReportsActualStartedProject(t *testing.T) {
	useTempProjectsDir(t)
	writeDB(t, dbPath("HOME"), "home")
	writeDB(t, dbPath("OTHER"), "other")
	reviewSession(t, func(string) string { return "123 project=OTHER state=started\r\n" })
	w := httptest.NewRecorder()
	handleTagList(w, httptest.NewRequest(http.MethodGet, "/tag", nil))
	if !strings.Contains(w.Body.String(), `"active":"OTHER"`) {
		t.Fatal("list used configured HOME instead of running OTHER", w.Body.String())
	}
}

func TestReplacementBacksUpPreviouslyUnsavedProject(t *testing.T) {
	useTempProjectsDir(t)
	state := "started"
	original := db("previously unsaved")
	reviewSession(t, func(cmd string) string {
		switch cmd {
		case "project list":
			if state == "" {
				return "200 OK.\r\n"
			}
			return "123 project=HOME state=" + state + "\r\n"
		case "project save HOME":
			if err := os.MkdirAll(projectDir("HOME"), 0755); err != nil {
				return "408 mkdir failed\r\n"
			}
			if err := os.WriteFile(dbPath("HOME"), original, 0644); err != nil {
				return "408 save failed\r\n"
			}
		case "project stop HOME", "project load HOME":
			state = "stopped"
		case "project close HOME":
			state = ""
		case "project start HOME":
			state = "started"
		}
		return "200 OK.\r\n"
	})
	w := httptest.NewRecorder()
	handleTagUpload(w, uploadRequest(t, "HOME", "HOME.db", db("replacement")))
	if w.Code != 200 {
		t.Fatalf("replacement failed: %d %s", w.Code, w.Body.String())
	}
	got, err := os.ReadFile(filepath.Join(projectDir("HOME")+backupSuffix, "HOME.db"))
	if err != nil || !bytes.Equal(got, original) {
		t.Fatal("the newly saved original was not backed up")
	}
}

func TestDatabaseOnlyDownloadDoesNotDependOnCompanions(t *testing.T) {
	useTempProjectsDir(t)
	writeDB(t, dbPath("HOME"), "database")
	if err := os.Symlink("/missing-label", filepath.Join(projectDir("HOME"), "label.bmp")); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	handleTagDownload(w, httptest.NewRequest("GET", "/tag/download?project=HOME", nil))
	if w.Code != 200 || databaseDescription(w.Body.Bytes()) != "database" {
		t.Fatal("database download depended on companion files", w.Code)
	}
}

func TestBackupSavesProjectThatExistsOnlyInMemory(t *testing.T) {
	useTempProjectsDir(t)
	reviewSession(t, func(cmd string) string {
		if cmd == "project list" {
			return "123 project=UNSAVED state=started\r\n"
		}
		if cmd == "project save UNSAVED" {
			if err := os.MkdirAll(projectDir("UNSAVED"), 0755); err != nil {
				return "408 mkdir failed\r\n"
			}
			if err := os.WriteFile(dbPath("UNSAVED"), db("unsaved"), 0644); err != nil {
				return "408 save failed\r\n"
			}
		}
		return "200 OK.\r\n"
	})
	list := httptest.NewRecorder()
	handleTagList(list, httptest.NewRequest("GET", "/tag", nil))
	if !strings.Contains(list.Body.String(), `"active":"UNSAVED"`) {
		t.Fatal("in-memory running project was not offered for backup", list.Body.String())
	}
	backup := httptest.NewRecorder()
	handleTagBackup(backup, httptest.NewRequest("POST", "/tag/backup?project=UNSAVED", nil))
	if backup.Code != 200 || backup.Header().Get("X-CGate-Saved") != "true" {
		t.Fatal("unsaved project backup failed", backup.Code, backup.Body.String())
	}
	if _, err := zip.NewReader(bytes.NewReader(backup.Body.Bytes()), int64(backup.Body.Len())); err != nil {
		t.Fatal(err)
	}
}
