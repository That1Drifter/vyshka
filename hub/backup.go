package hub

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/That1Drifter/vyshka/hub/store"
)

// DefaultBackupKeep is how many snapshots the backup endpoint keeps in its
// directory when the configuration does not say. A week of dailies.
const DefaultBackupKeep = 7

// DefaultWebhookDeliveryRetention is how long a finished webhook delivery
// stays readable when the configuration does not say. Exported for the
// storage report, which has to apply the same window the prune pass does.
const DefaultWebhookDeliveryRetention = 7 * 24 * time.Hour

// backupNameLayout is the snapshot file name's timestamp: UTC to the
// millisecond, fixed width, so that name order is time order and a listing
// sorted by name is a listing sorted by age.
const backupNameLayout = "20060102T150405.000Z"

// backupName matches a finished snapshot and nothing else. Rotation deletes
// only files this matches, so whatever else an operator keeps in the
// directory is never touched; a snapshot still being written carries the
// `.partial` suffix and so is never counted, listed, or rotated as a backup.
var (
	backupName        = regexp.MustCompile(`^vyshka-\d{8}T\d{6}\.\d{3}Z\.db$`)
	backupPartialName = regexp.MustCompile(`^vyshka-\d{8}T\d{6}\.\d{3}Z\.db\.partial$`)
)

type backupView struct {
	Name       string    `json:"name"`
	Path       string    `json:"path"`
	SizeBytes  int64     `json:"sizeBytes"`
	TakenAt    time.Time `json:"takenAt"`
	DurationMs int64     `json:"durationMs"`
}

type backupResponse struct {
	Backup backupView `json:"backup"`
	// Kept is every finished snapshot in the directory after rotation, newest
	// first, this one included.
	Kept []string `json:"kept"`
	// Pruned names the snapshots rotation deleted to stay within the bound.
	Pruned []string `json:"pruned"`
}

// backupDir is where snapshots land: the configured directory, or a
// `backups` directory beside the SQLite file.
func (s *Server) backupDir() string {
	if s.cfg.BackupDir != "" {
		return s.cfg.BackupDir
	}
	return filepath.Join(filepath.Dir(s.store.Target()), "backups")
}

// handleBackup takes a consistent snapshot of a SQLite database while the hub
// keeps serving (store.Snapshot says how), writes it under a name the hub
// chooses in the directory the hub was configured with, and rotates the
// directory down to the configured number of snapshots. It uses nothing from
// the request: the admin gate reads and digests a body for the audit record
// like any mutation's, but no part of it reaches the handler's choices, so
// nothing a caller sends can choose a path, and a leaked admin token can cost
// at most BackupKeep snapshots plus the one being written.
func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request) {
	if s.store.Driver() != "sqlite" {
		writeErrorDetails(w, http.StatusConflict, codeConflict,
			"this hub runs on "+s.store.Driver()+"; back it up with pg_dump (deploy/README.md, \"Backup and restore\")",
			map[string]any{"backend": s.store.Driver()})
		return
	}
	if err := s.store.CanSnapshot(); err != nil {
		writeError(w, http.StatusConflict, codeConflict, err.Error())
		return
	}
	// One snapshot at a time: a second request answers at once instead of
	// queueing behind the first, so a burst cannot stack partial files or
	// park admin connections.
	if !s.backupRunning.TryLock() {
		writeError(w, http.StatusConflict, codeConflict, "a backup is already running; try again when it finishes")
		return
	}
	defer s.backupRunning.Unlock()

	dir := s.backupDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		s.writeInternalError(w, r, fmt.Errorf("backup directory: %w", err))
		return
	}
	// A partial file left behind can only be from a snapshot that died with
	// the process (one that failed in-process removes its own), and with the
	// lock held no other snapshot is writing one now.
	removeMatching(dir, backupPartialName)

	started := time.Now().UTC()
	name, err := freeBackupName(dir, started)
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	final := filepath.Join(dir, name)
	partial := final + ".partial"
	if err := s.store.Snapshot(r.Context(), partial); err != nil {
		os.Remove(partial)
		if errors.Is(err, store.ErrSnapshotUnsupported) || errors.Is(err, store.ErrSnapshotNotAFile) {
			writeError(w, http.StatusConflict, codeConflict, err.Error())
			return
		}
		s.writeInternalError(w, r, err)
		return
	}
	if err := os.Rename(partial, final); err != nil {
		os.Remove(partial)
		s.writeInternalError(w, r, fmt.Errorf("backup: %w", err))
		return
	}
	syncDir(dir)
	info, err := os.Stat(final)
	if err != nil {
		s.writeInternalError(w, r, fmt.Errorf("backup: %w", err))
		return
	}
	elapsed := time.Since(started)
	auditDetail(r, "backup", name)

	kept, pruned := rotateBackups(dir, s.cfg.BackupKeep)
	if len(pruned) > 0 {
		auditDetail(r, "pruned", pruned)
	}
	s.log.Info("backup taken", "name", name, "bytes", info.Size(),
		"durationMs", elapsed.Milliseconds(), "pruned", len(pruned))

	absolute, err := filepath.Abs(final)
	if err != nil {
		absolute = final
	}
	writeJSON(w, http.StatusCreated, backupResponse{
		Backup: backupView{
			Name:       name,
			Path:       absolute,
			SizeBytes:  info.Size(),
			TakenAt:    started,
			DurationMs: elapsed.Milliseconds(),
		},
		Kept:   kept,
		Pruned: pruned,
	})
}

// freeBackupName picks the snapshot's name from its start time, but never a
// name that sorts at or before a finished snapshot already in the directory:
// when the clock is behind the newest name there (it was stepped back, or the
// directory holds files from a host whose clock ran ahead), the name is one
// millisecond after that newest one instead. Name order is therefore creation
// order, which is what rotation trusts: the snapshot just taken is always the
// newest and is never the one rotated out. Without this, a directory of
// future-dated names made every new snapshot the oldest, and rotation deleted
// it as it answered 201 naming it.
func freeBackupName(dir string, at time.Time) (string, error) {
	if existing := matching(dir, backupName); len(existing) > 0 {
		newest := slices.Max(existing)
		stamp := strings.TrimSuffix(strings.TrimPrefix(newest, "vyshka-"), ".db")
		if last, err := time.Parse(backupNameLayout, stamp); err == nil && !at.After(last) {
			at = last.Add(time.Millisecond)
		}
	}
	for range 1000 {
		name := "vyshka-" + at.Format(backupNameLayout) + ".db"
		_, finalErr := os.Lstat(filepath.Join(dir, name))
		_, partialErr := os.Lstat(filepath.Join(dir, name+".partial"))
		if errors.Is(finalErr, os.ErrNotExist) && errors.Is(partialErr, os.ErrNotExist) {
			return name, nil
		}
		at = at.Add(time.Millisecond)
	}
	return "", fmt.Errorf("backup: no free snapshot name near %s in %s", at.Format(time.RFC3339), dir)
}

// rotateBackups deletes the oldest finished snapshots beyond keep, and returns
// the ones it kept (newest first) and the ones it deleted. A file it fails to
// delete stays listed as kept, so the answer describes the directory as it is.
func rotateBackups(dir string, keep int) (kept, pruned []string) {
	names := matching(dir, backupName)
	slices.Sort(names)
	slices.Reverse(names)
	kept, pruned = []string{}, []string{}
	for i, name := range names {
		if i < keep {
			kept = append(kept, name)
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			kept = append(kept, name)
			continue
		}
		pruned = append(pruned, name)
	}
	return kept, pruned
}

func matching(dir string, pattern *regexp.Regexp) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, entry := range entries {
		if entry.Type().IsRegular() && pattern.MatchString(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	return names
}

func removeMatching(dir string, pattern *regexp.Regexp) {
	for _, name := range matching(dir, pattern) {
		os.Remove(filepath.Join(dir, name))
	}
}

// syncDir makes a rename durable where the platform allows it. Windows cannot
// open a directory for sync, and its rename is journaled by NTFS anyway, so a
// failure here is not an error.
func syncDir(dir string) {
	handle, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = handle.Sync()
	handle.Close()
}
