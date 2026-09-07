package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The command protocol is shared by every project. Serialize bridge commands,
// replacement and snapshots through the same cancellable operation lock.
var projectOperations contextMutex
var transferSlots = make(chan struct{}, 2)
var diskReservations struct {
	sync.Mutex
	bytes int64
}

const diskHeadroom = 64 << 20
const transferReservation = maxUploadBytes + 2*maxUnpackedBytes
const transactionTimeout = 2 * time.Minute

func reserveTransfer() (func(), error) {
	select {
	case transferSlots <- struct{}{}:
	default:
		return nil, errors.New("two file transfers are already in progress; retry shortly")
	}
	fail := func(err error) (func(), error) { <-transferSlots; return nil, err }
	if err := os.MkdirAll(projectsDir, 0755); err != nil {
		return fail(err)
	}
	diskReservations.Lock()
	defer diskReservations.Unlock()
	var stat syscall.Statfs_t
	if err := syscall.Statfs(projectsDir, &stat); err != nil {
		return fail(err)
	}
	free := uint64(stat.Bavail) * uint64(stat.Bsize)
	if free < uint64(diskReservations.bytes+transferReservation+diskHeadroom) {
		return fail(errors.New("insufficient free project storage for a safe transfer and rollback"))
	}
	diskReservations.bytes += transferReservation
	return func() {
		diskReservations.Lock()
		diskReservations.bytes -= transferReservation
		diskReservations.Unlock()
		<-transferSlots
	}, nil
}

// SQLite checks run in a bounded, read-only subprocess. C-Gate remains the
// final compatibility check; its load/start must succeed before committing.
type limitedOutput struct {
	bytes.Buffer
	max int
}

func (b *limitedOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.max {
		return 0, errors.New("SQLite output limit exceeded")
	}
	return b.Buffer.Write(p)
}
func sqlite(ctx context.Context, database, query string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sqlite3", "-init", "/dev/null", "-batch", "-bail", "-readonly", database, "PRAGMA trusted_schema=OFF; "+query)
	out := &limitedOutput{max: 64 << 10}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("database check failed: %w: %s", err, strings.TrimSpace(out.String()))
	}
	return strings.TrimSpace(out.String()), nil
}
func validateDatabase(ctx context.Context, database string) error {
	info, err := os.Lstat(database)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > maxUnpackedBytes {
		return errors.New("database must be a regular file within the project size limit")
	}
	f, err := os.Open(database)
	if err != nil {
		return err
	}
	defer f.Close()
	head := make([]byte, len(sqliteMagic))
	if _, err = io.ReadFull(f, head); err != nil || !bytes.Equal(head, sqliteMagic) {
		return errors.New("not a SQLite database")
	}
	// These core tables/columns are present in the shipped C-Gate schema. This
	// rejects ordinary SQLite files as well as corrupt/truncated project files.
	out, err := sqlite(ctx, database, `PRAGMA integrity_check(1);
 SELECT count(*) FROM project p JOIN tagged_entity t ON t.id=p.tagged_entity_id;
 SELECT network_number, project_id FROM network LIMIT 0;
 SELECT tagged_entity_id, network_id FROM application LIMIT 0;`)
	if err != nil {
		return err
	}
	if out != "ok\n1" {
		return fmt.Errorf("not an intact single C-Gate project: %s", out)
	}
	return nil
}

func runProjectCommand(ctx context.Context, cmd string, notes *[]string) ([]string, error) {
	step, cancel := context.WithTimeout(ctx, projectReadDeadline)
	defer cancel()
	lines, err := cmdSession.sendContext(step, cmd)
	if notes != nil {
		*notes = append(*notes, "> "+cmd)
		*notes = append(*notes, lines...)
	}
	if err != nil {
		return lines, err
	}
	if len(lines) == 0 {
		return lines, errors.New("empty C-Gate response")
	}
	code, _, _, err := responseLine(lines[len(lines)-1])
	if err != nil {
		return lines, err
	}
	if code < 200 || code >= 300 {
		return lines, fmt.Errorf("%s failed: %s", cmd, lines[len(lines)-1])
	}
	return lines, nil
}
func projectStates(ctx context.Context) (map[string]string, error) {
	step, cancel := context.WithTimeout(ctx, commandReadDeadline)
	defer cancel()
	lines, err := cmdSession.sendContext(step, "project list")
	if err != nil {
		return nil, err
	}
	states := map[string]string{}
	for _, line := range lines {
		code, _, text, err := responseLine(line)
		if err != nil {
			return nil, err
		}
		// 200 OK is the empty list in C-Gate 3.8.
		if code == 200 && len(lines) == 1 {
			return states, nil
		}
		if code != 123 {
			return nil, fmt.Errorf("cannot determine loaded projects: %s", line)
		}
		name, state := "", ""
		for _, field := range strings.Fields(text) {
			if v, ok := strings.CutPrefix(field, "project="); ok {
				name = v
			}
			if v, ok := strings.CutPrefix(field, "state="); ok {
				state = v
			}
		}
		if !projectNamePattern.MatchString(name) || (state != "started" && state != "stopped") {
			return nil, fmt.Errorf("unrecognized project state: %s", line)
		}
		if _, exists := states[name]; exists {
			return nil, fmt.Errorf("duplicate project state for %s", name)
		}
		states[name] = state
	}
	return states, nil
}
func closeProject(ctx context.Context, project string, notes *[]string) error {
	states, err := projectStates(ctx)
	if err != nil {
		return err
	}
	if states[project] == "" {
		return nil
	}
	if states[project] == "started" {
		if _, err = runProjectCommand(ctx, "project stop "+project, notes); err != nil {
			return err
		}
	}
	if _, err = runProjectCommand(ctx, "project close "+project, notes); err != nil {
		return err
	}
	states, err = projectStates(ctx)
	if err != nil {
		return err
	}
	if states[project] != "" {
		return errors.New("C-Gate still has the project loaded after close")
	}
	return nil
}
func restoreProject(ctx context.Context, project, state string, notes *[]string) error {
	states, err := projectStates(ctx)
	if err != nil {
		return err
	}
	current := states[project]
	if state == "" {
		if current != "" {
			return closeProject(ctx, project, notes)
		}
		return nil
	}
	if current == "" {
		if _, err = runProjectCommand(ctx, "project load "+project, notes); err != nil {
			return err
		}
		current = "stopped"
	}
	if current != state {
		action := "stop"
		if state == "started" {
			action = "start"
		}
		if _, err = runProjectCommand(ctx, "project "+action+" "+project, notes); err != nil {
			return err
		}
	}
	states, err = projectStates(ctx)
	if err != nil {
		return err
	}
	if states[project] != state {
		return fmt.Errorf("project %s did not reach %s", project, state)
	}
	return nil
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func durableRename(from, to string) error {
	if err := os.Rename(from, to); err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(from)); err != nil {
		return err
	}
	if filepath.Dir(from) != filepath.Dir(to) {
		return syncDir(filepath.Dir(to))
	}
	return nil
}
func syncTree(dir string) error {
	var dirs []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, p)
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("non-regular project file: %s", p)
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		err = f.Sync()
		cerr := f.Close()
		if err != nil {
			return err
		}
		return cerr
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := syncDir(dirs[i]); err != nil {
			return err
		}
	}
	return nil
}
func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// Keep the old directory until both installation and C-Gate load verification
// succeed. The journal is durable before any lifecycle command or file move.
type projectTransaction struct {
	Project       string `json:"project"`
	PreviousState string `json:"previous_state"`
	HadOld        bool   `json:"had_old"`
	Committed     bool   `json:"committed"`
	Swapping      bool   `json:"swapping"`
}

func transactionDir(project string) string {
	return filepath.Join(projectsDir, ".transactions", project)
}
func writeTransaction(tx projectTransaction) error {
	dir := transactionDir(tx.Project)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := json.Marshal(tx)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "record.tmp"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	cerr := f.Close()
	if err != nil {
		return err
	}
	if cerr != nil {
		return cerr
	}
	if err := durableRename(f.Name(), filepath.Join(dir, "record.json")); err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(dir)); err != nil {
		return err
	}
	return syncDir(projectsDir)
}
func finishTransaction(tx projectTransaction) error {
	dir := transactionDir(tx.Project)
	old := filepath.Join(dir, "old")
	if exists(old) {
		backup := projectDir(tx.Project) + backupSuffix
		if err := os.RemoveAll(backup); err != nil {
			return err
		}
		if err := durableRename(old, backup); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return syncDir(filepath.Dir(dir))
}
func rollbackTransaction(ctx context.Context, tx projectTransaction, online bool) error {
	dir := transactionDir(tx.Project)
	dest := projectDir(tx.Project)
	old := filepath.Join(dir, "old")
	// A bridge-only restart must first release any candidate Java loaded.
	if online && tx.Swapping {
		if err := closeProject(ctx, tx.Project, nil); err != nil {
			return fmt.Errorf("rollback waiting for confirmed closure: %w", err)
		}
	}
	if exists(old) {
		if exists(dest) {
			if err := os.RemoveAll(filepath.Join(dir, "failed")); err != nil {
				return err
			}
			if err := durableRename(dest, filepath.Join(dir, "failed")); err != nil {
				return err
			}
		}
		if err := durableRename(old, dest); err != nil {
			return err
		}
	} else if tx.Swapping && !tx.HadOld && exists(dest) {
		if err := os.RemoveAll(dest); err != nil {
			return err
		}
		if err := syncDir(projectsDir); err != nil {
			return err
		}
	} else if tx.HadOld && !exists(dest) {
		return errors.New("rollback could not locate the previous project; recovery files retained")
	}
	if online {
		if err := restoreProject(ctx, tx.Project, tx.PreviousState, nil); err != nil {
			return fmt.Errorf("previous files restored, but runtime recovery failed: %w", err)
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return syncDir(filepath.Dir(dir))
}

// recoverTransactions is called before Java starts, and after a bridge-only
// crash once its command connection returns. An unresolved journal blocks all
// new bridge commands that could interfere with recovery.
func recoverTransactions(ctx context.Context, online bool) error {
	root := filepath.Join(projectsDir, ".transactions")
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !projectNamePattern.MatchString(entry.Name()) {
			return errors.New("unexpected transaction recovery entry")
		}
		data, err := os.ReadFile(filepath.Join(root, entry.Name(), "record.json"))
		if os.IsNotExist(err) { // No durable record means no project was touched.
			if exists(filepath.Join(root, entry.Name(), "old")) {
				return errors.New("transaction backup exists without a record")
			}
			if err = os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		var tx projectTransaction
		if err = json.Unmarshal(data, &tx); err != nil {
			return err
		}
		if tx.Project != entry.Name() || (tx.PreviousState != "" && tx.PreviousState != "started" && tx.PreviousState != "stopped") {
			return errors.New("invalid transaction recovery record")
		}
		if tx.Committed {
			err = finishTransaction(tx)
		} else {
			err = rollbackTransaction(ctx, tx, online)
		}
		if err != nil {
			return fmt.Errorf("recovering %s: %w", tx.Project, err)
		}
		log.Printf("Recovered interrupted replacement of %s", tx.Project)
	}
	return nil
}
func pendingTransactions() bool {
	entries, err := os.ReadDir(filepath.Join(projectsDir, ".transactions"))
	return (!os.IsNotExist(err) && err != nil) || len(entries) > 0
}
func recoverWhileRunning() {
	for {
		if commandUp.Load() && pendingTransactions() {
			ctx, cancel := context.WithTimeout(context.Background(), transactionTimeout)
			if err := projectOperations.acquire(ctx); err == nil {
				if err = recoverTransactions(ctx, true); err != nil {
					log.Printf("Project recovery: %v", err)
				}
				projectOperations.Unlock()
			}
			cancel()
		}
		time.Sleep(dialRetryInterval)
	}
}

// copyProject snapshots regular companion files. SQLite supplies a consistent
// database snapshot for exports even if another C-Gate client saves meanwhile.
func copyProject(ctx context.Context, from, to, project string, snapshot bool) error {
	if err := os.MkdirAll(to, 0755); err != nil {
		return err
	}
	var total int64
	entries := 0
	return filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(from, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if strings.HasSuffix(d.Name(), backupSuffix) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".db-wal") || strings.HasSuffix(d.Name(), ".db-shm") || strings.HasSuffix(d.Name(), ".db-journal") {
			return nil
		}
		entries++
		if entries > maxArchiveEntries {
			return errors.New("project has too many files")
		}
		dest := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(dest, 0755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("non-regular project file: %s", rel)
		}
		if info.Size() > maxUnpackedBytes-total {
			return errors.New("project exceeds snapshot size limit")
		}
		if snapshot && rel == project+dbSuffix {
			// VACUUM INTO runs a read transaction and writes a standalone SQLite copy.
			_, err = sqlite(ctx, p, "VACUUM INTO '"+strings.ReplaceAll(dest, "'", "''")+"';")
			if err != nil {
				return err
			}
			out, err := os.Stat(dest)
			if err != nil {
				return err
			}
			total += out.Size()
			if total > maxUnpackedBytes {
				return errors.New("project exceeds snapshot size limit")
			}
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		n, err := writeEntry(dest, f, maxUnpackedBytes-total)
		f.Close()
		if err != nil {
			return err
		}
		total += n
		after, err := os.Stat(p)
		if err != nil {
			return err
		}
		if n != info.Size() || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
			return fmt.Errorf("project file changed during snapshot: %s", rel)
		}
		return nil
	})
}

func installProject(ctx context.Context, project, staging string, bare bool) (notes []string, backedUp bool, err error) {
	if pendingTransactions() {
		return nil, false, errors.New("an interrupted project replacement is awaiting recovery")
	}
	states, err := projectStates(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("cannot safely replace project: %w", err)
	}
	tx := projectTransaction{Project: project, PreviousState: states[project], HadOld: exists(projectDir(project))}
	if err = writeTransaction(tx); err != nil {
		return nil, false, err
	}
	defer func() {
		if err == nil {
			return
		}
		// Finish rollback even if the browser went away. Never restore old files
		// while a loaded candidate might still write over them.
		recovery, cancel := context.WithTimeout(context.Background(), transactionTimeout)
		defer cancel()
		if rollbackErr := rollbackTransaction(recovery, tx, true); rollbackErr != nil {
			err = fmt.Errorf("%w; recovery pending: %v", err, rollbackErr)
		}
	}()
	if tx.PreviousState != "" {
		if _, err = runProjectCommand(ctx, "project save "+project, &notes); err != nil {
			return notes, false, err
		}
	}
	if err = closeProject(ctx, project, &notes); err != nil {
		return notes, false, err
	}
	// Saving a previously unsaved in-memory project may have created its
	// directory. Include that original in rollback before starting file moves.
	tx.HadOld = exists(projectDir(project))
	dir := transactionDir(project)
	candidate := filepath.Join(dir, "new")
	if bare {
		if tx.HadOld {
			err = copyProject(ctx, projectDir(project), candidate, project, false)
		} else {
			err = os.MkdirAll(candidate, 0755)
		}
		if err != nil {
			return notes, false, err
		}
		if err = os.Rename(filepath.Join(staging, project+dbSuffix), filepath.Join(candidate, project+dbSuffix)); err != nil {
			return notes, false, err
		}
	} else {
		if err = durableRename(staging, candidate); err != nil {
			return notes, false, err
		}
	}
	if err = syncTree(candidate); err != nil {
		return notes, false, err
	}
	tx.Swapping = true
	if err = writeTransaction(tx); err != nil {
		return notes, false, err
	}
	if tx.HadOld {
		if err = durableRename(projectDir(project), filepath.Join(dir, "old")); err != nil {
			return notes, false, err
		}
	}
	if err = durableRename(candidate, projectDir(project)); err != nil {
		return notes, false, err
	}
	target := tx.PreviousState
	if target == "" {
		target = "started"
	}
	if err = restoreProject(ctx, project, target, &notes); err != nil {
		return notes, false, err
	}
	tx.Committed = true
	if err = writeTransaction(tx); err != nil {
		tx.Committed = false
		return notes, false, err
	}
	// Once committed, cleanup errors must not undo a successfully loaded project.
	if cleanupErr := finishTransaction(tx); cleanupErr != nil {
		notes = append(notes, "backup cleanup pending: "+cleanupErr.Error())
	}
	return notes, tx.HadOld, nil
}

func serveProjectSnapshot(w http.ResponseWriter, r *http.Request, archive, save bool) {
	project, err := requestedProject(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if save && r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "backup requires POST")
		return
	}
	release, err := reserveTransfer()
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	defer release()
	ctx, cancel := context.WithTimeout(r.Context(), transactionTimeout)
	defer cancel()
	if err = projectOperations.acquire(ctx); err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	// Release before serving a potentially slow download.
	locked := true
	defer func() {
		if locked {
			projectOperations.Unlock()
		}
	}()
	if pendingTransactions() {
		writeJSONError(w, http.StatusServiceUnavailable, "project recovery is pending")
		return
	}

	saved := false
	warning := ""
	if save {
		if _, err = runProjectCommand(ctx, "project save "+project, nil); err != nil {
			warning = "Save failed; this backup contains the last readable disk state and may be out of date: " + err.Error()
		} else {
			saved = true
		}
	}
	if !exists(dbPath(project)) {
		writeJSONError(w, http.StatusNotFound, "no database for project "+project)
		return
	}
	info, statErr := os.Lstat(dbPath(project))
	if statErr != nil || !info.Mode().IsRegular() || info.Size() > maxUnpackedBytes {
		writeJSONError(w, http.StatusInternalServerError, "database is missing, non-regular, or exceeds the snapshot size limit")
		return
	}
	staging, err := os.MkdirTemp(projectsDir, ".snapshot-*")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer os.RemoveAll(staging)
	snapshot := filepath.Join(staging, "project")
	if archive {
		err = copyProject(ctx, projectDir(project), snapshot, project, true)
	} else {
		// A database-only download does not need to copy or validate bitmap files.
		err = os.MkdirAll(snapshot, 0755)
		if err == nil {
			_, err = sqlite(ctx, dbPath(project), "VACUUM INTO '"+strings.ReplaceAll(filepath.Join(snapshot, project+dbSuffix), "'", "''")+"';")
		}
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err = validateDatabase(ctx, filepath.Join(snapshot, project+dbSuffix)); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	name := project + dbSuffix
	output := filepath.Join(snapshot, name)
	if archive {
		name = project + archiveSuffix(r)
		output = filepath.Join(staging, "download.zip")
		if err = buildArchive(ctx, snapshot, output); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	projectOperations.Unlock()
	locked = false
	f, err := os.Open(output)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	if archive {
		w.Header().Set("Content-Type", "application/zip")
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	if save {
		w.Header().Set("X-CGate-Saved", fmt.Sprint(saved))
	}
	if warning != "" {
		w.Header().Set("X-CGate-Warning", strings.NewReplacer("\r", " ", "\n", " ").Replace(warning))
	}
	// Only the completed, checked snapshot is sent. Connection failures are
	// detectable using its known Content-Length; no valid partial ZIP is emitted.
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(2 * time.Minute))
	defer http.NewResponseController(w).SetWriteDeadline(time.Time{})
	http.ServeContent(w, r, name, info.ModTime(), f)
}
func buildArchive(ctx context.Context, dir, output string) error {
	f, err := os.Create(output)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(f)
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("non-regular snapshot file: %s", p)
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
		src, err := os.Open(p)
		if err != nil {
			return err
		}
		n, err := io.Copy(entry, src)
		src.Close()
		if err == nil && n != info.Size() {
			err = errors.New("snapshot changed while archiving")
		}
		return err
	})
	closeErr := zw.Close()
	fileErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return fileErr
}

// No transfer goroutine exists yet when main calls this. Crash leftovers are
// separate from durable recovery records and can be removed safely.
func cleanupStaging() error {
	entries, err := os.ReadDir(projectsDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".incoming-") || strings.HasPrefix(e.Name(), ".snapshot-") || strings.HasPrefix(e.Name(), ".seed-") {
			if err := os.RemoveAll(filepath.Join(projectsDir, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}
