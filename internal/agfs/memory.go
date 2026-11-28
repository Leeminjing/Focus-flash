package agfs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

type WorkingMemory struct {
	L0             string
	L1             string
	CoveredThrough int
}

// Latest returns the most recent published archive and latest saved memory for
// a session. A session with no archive returns the zero values.
func (s *Storage) Latest(ctx context.Context, sessionID string) (Archive, WorkingMemory, error) {
	var archive Archive
	err := s.db.QueryRowContext(ctx, `
SELECT id, session_id, seq, path, state FROM archives
WHERE session_id = ? AND state != 'staged' ORDER BY seq DESC LIMIT 1
`, sessionID).Scan(&archive.ID, &archive.SessionID, &archive.Sequence, &archive.Path, &archive.State)
	if errors.Is(err, sql.ErrNoRows) {
		return Archive{}, WorkingMemory{}, nil
	}
	if err != nil {
		return Archive{}, WorkingMemory{}, err
	}
	archive.FilePath, err = s.filePath(archive.Path)
	if err != nil {
		return Archive{}, WorkingMemory{}, err
	}
	memory, err := s.memoryThrough(ctx, sessionID, archive.Sequence)
	return archive, memory, err
}

// MemoryInput finds the last version before the target archive and the
// ordered range of original files it has yet to cover.
func (s *Storage) MemoryInput(ctx context.Context, archiveID string) (WorkingMemory, []Archive, error) {
	var sessionID string
	var sequence int
	if err := s.db.QueryRowContext(ctx, `
SELECT session_id, seq FROM archives WHERE id = ? AND state != 'staged'
`, archiveID).Scan(&sessionID, &sequence); err != nil {
		return WorkingMemory{}, nil, err
	}
	memory, err := s.memoryThrough(ctx, sessionID, sequence-1)
	if err != nil {
		return WorkingMemory{}, nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, session_id, seq, path, state FROM archives
WHERE session_id = ? AND seq > ? AND seq <= ? AND state != 'staged' ORDER BY seq
`, sessionID, memory.CoveredThrough, sequence)
	if err != nil {
		return WorkingMemory{}, nil, err
	}
	archives, err := readArchives(rows)
	return memory, archives, err
}

func (s *Storage) memoryThrough(ctx context.Context, sessionID string, sequence int) (WorkingMemory, error) {
	var memory WorkingMemory
	err := s.db.QueryRowContext(ctx, `
SELECT m.l0, m.l1, m.covered_through
FROM archives a JOIN working_memory m ON m.archive_id = a.id
WHERE a.session_id = ? AND a.seq <= ? ORDER BY a.seq DESC LIMIT 1
`, sessionID, sequence).Scan(&memory.L0, &memory.L1, &memory.CoveredThrough)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkingMemory{}, nil
	}
	return memory, err
}

// Settle publishes a memory version and acknowledges the delivery atomically.
// Repeating a successful settlement for the same archive is a no-op.
func (s *Storage) Settle(ctx context.Context, archiveID string, messageID int64, memory WorkingMemory) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state string
	var sequence int
	if err := tx.QueryRowContext(ctx, "SELECT state, seq FROM archives WHERE id = ?", archiveID).Scan(&state, &sequence); err != nil {
		return err
	}
	if state == "complete" {
		return nil
	}
	if state != "pending" || memory.CoveredThrough != sequence {
		return fmt.Errorf("archive %s cannot settle at coverage %d (state %s, sequence %d)",
			archiveID, memory.CoveredThrough, state, sequence)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO working_memory (archive_id, l0, l1, covered_through) VALUES (?, ?, ?, ?)
`, archiveID, memory.L0, memory.L1, memory.CoveredThrough); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE archives SET state = 'complete' WHERE id = ?", archiveID); err != nil {
		return err
	}
	if err := s.write(ctx, tx, memoryQueue+"ack", strconv.AppendInt(nil, messageID, 10)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Storage) Settled(ctx context.Context, archiveIDs []string) (bool, error) {
	for _, id := range archiveIDs {
		var state string
		if err := s.db.QueryRowContext(ctx, "SELECT state FROM archives WHERE id = ?", id).Scan(&state); err != nil {
			return false, err
		}
		if state != "complete" {
			return false, nil
		}
	}
	return true, nil
}
