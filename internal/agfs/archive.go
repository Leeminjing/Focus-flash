package agfs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

type Archive struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	Sequence  int    `json:"sequence"`
	Path      string `json:"path"`
	State     string `json:"state"`
	// FilePath is the absolute host path resolved by Storage.Latest.
	FilePath string `json:"-"`
}

type ArchiveDraft struct {
	SessionID string
	Data      []byte
}

type MemoryTask struct {
	ArchiveID string `json:"archive_id"`
}

func (s *Storage) HasArchive(ctx context.Context, sessionID string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (
	SELECT 1 FROM archives WHERE session_id = ? AND state != 'staged'
)`, sessionID).Scan(&exists)
	return exists, err
}

// CommitRound publishes one batch under its stable commit ID. A retry of an
// already registered batch uses its recorded paths and archive IDs.
func (s *Storage) CommitRound(ctx context.Context, commitID string, drafts []ArchiveDraft) ([]string, error) {
	if len(drafts) == 0 {
		return nil, nil
	}
	s.archiveMu.Lock()
	defer s.archiveMu.Unlock()

	var published bool
	err := s.db.QueryRowContext(ctx, "SELECT published FROM archive_commits WHERE id = ?", commitID).Scan(&published)
	if errors.Is(err, sql.ErrNoRows) {
		if err := s.stageRound(ctx, commitID, drafts); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}

	archives, err := s.commitArchives(ctx, commitID)
	if err != nil {
		return nil, err
	}
	if !published {
		if err := s.publishRound(ctx, commitID, archives); err != nil {
			return nil, err
		}
	}
	ids := make([]string, 0, len(archives))
	for _, archive := range archives {
		ids = append(ids, archive.ID)
	}
	return ids, nil
}

func (s *Storage) stageRound(ctx context.Context, commitID string, drafts []ArchiveDraft) error {
	archives := make([]Archive, 0, len(drafts))
	for i, draft := range drafts {
		id := fmt.Sprintf("%s-%d", commitID, i+1)
		path := "/sessions/" + draft.SessionID + "/" + id + ".jsonl"
		if !validComponent(commitID) || !validComponent(draft.SessionID) {
			return fmt.Errorf("invalid commit or session identifier for archive %q", id)
		}
		if err := s.Write(ctx, stagingPath(id), draft.Data); err != nil {
			return fmt.Errorf("stage archive %s: %w", id, err)
		}
		archives = append(archives, Archive{ID: id, SessionID: draft.SessionID, Path: path})
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "INSERT INTO archive_commits (id) VALUES (?)", commitID); err != nil {
		return err
	}
	for _, archive := range archives {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO archives (id, commit_id, session_id, seq, path, state)
VALUES (?, ?, ?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM archives WHERE session_id = ?), ?, 'staged')
`, archive.ID, commitID, archive.SessionID, archive.SessionID, archive.Path); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Storage) commitArchives(ctx context.Context, commitID string) ([]Archive, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, session_id, seq, path, state FROM archives WHERE commit_id = ? ORDER BY ordinal
`, commitID)
	if err != nil {
		return nil, err
	}
	return readArchives(rows)
}

func (s *Storage) publishRound(ctx context.Context, commitID string, archives []Archive) error {
	for _, archive := range archives {
		if err := s.publishFile(archive); err != nil {
			return fmt.Errorf("publish archive %s: %w", archive.ID, err)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "UPDATE archive_commits SET published = 1 WHERE id = ?", commitID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE archives SET state = 'pending' WHERE commit_id = ?", commitID); err != nil {
		return err
	}
	for _, archive := range archives {
		data, err := json.Marshal(MemoryTask{ArchiveID: archive.ID})
		if err != nil {
			return err
		}
		if err := s.write(ctx, tx, memoryQueue+"enqueue", data); err != nil {
			return fmt.Errorf("enqueue archive %s: %w", archive.ID, err)
		}
	}
	return tx.Commit()
}

func (s *Storage) publishFile(archive Archive) error {
	source, err := s.filePath(stagingPath(archive.ID))
	if err != nil {
		return err
	}
	target, err := s.filePath(archive.Path)
	if err != nil {
		return err
	}
	info, err := os.Stat(target)
	if err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("archive path %s is not a regular file", target)
		}
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return err
	}
	return os.Rename(source, target)
}

func (s *Storage) finishRegisteredRounds(ctx context.Context) error {
	ids, err := s.unpublishedCommits(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		archives, err := s.commitArchives(ctx, id)
		if err != nil {
			return err
		}
		if err := s.publishRound(ctx, id, archives); err != nil {
			return fmt.Errorf("resume commit %s: %w", id, err)
		}
	}

	// Files left before a batch was registered have no archive or queue entry.
	staging := filepath.Join(s.root, "staging")
	entries, err := os.ReadDir(staging)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Type().IsRegular() && filepath.Ext(entry.Name()) == ".jsonl" {
			if err := os.Remove(filepath.Join(staging, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Storage) unpublishedCommits(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id FROM archive_commits WHERE published = 0 ORDER BY rowid")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func stagingPath(id string) string {
	return "/staging/" + id + ".jsonl"
}

func readArchives(rows *sql.Rows) ([]Archive, error) {
	defer rows.Close()
	var archives []Archive
	for rows.Next() {
		var archive Archive
		if err := rows.Scan(&archive.ID, &archive.SessionID, &archive.Sequence, &archive.Path, &archive.State); err != nil {
			return nil, err
		}
		archives = append(archives, archive)
	}
	return archives, rows.Err()
}
