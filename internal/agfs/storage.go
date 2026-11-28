// Package agfs manages workspace files and the SQLite metadata that publishes them.
package agfs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"focusflash/internal/agfs/queuefs"

	_ "modernc.org/sqlite"
)

const memoryQueue = "/queue/working-memory/"
const WorkingMemoryDequeue = memoryQueue + "dequeue"

type Storage struct {
	db        *sql.DB
	root      string
	archiveMu sync.Mutex
}

// Open migrates the database and finishes any previously registered round.
func Open(ctx context.Context, dbPath string) (*Storage, error) {
	absPath, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, err
	}
	root := filepath.Dir(absPath)
	for _, dir := range []string{root, filepath.Join(root, "sessions")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
	}
	// A file URI preserves characters such as # and ? in the directory name.
	uriPath := filepath.ToSlash(absPath)
	if filepath.VolumeName(absPath) != "" && !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	dsn := (&url.URL{Scheme: "file", Path: uriPath, RawQuery: "_pragma=foreign_keys(1)"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Storage{db: db, root: root}
	if err := s.migrate(ctx); err != nil {
		return nil, errors.Join(fmt.Errorf("migrate %s: %w", absPath, err), db.Close())
	}
	if err := s.finishRegisteredRounds(ctx); err != nil {
		return nil, errors.Join(fmt.Errorf("recover archive submissions: %w", err), db.Close())
	}
	return s, nil
}

func (s *Storage) Close() error {
	return s.db.Close()
}

func (s *Storage) RecoverQueue(ctx context.Context) error {
	return queuefs.Recover(ctx, s.db)
}

// Read routes workspace paths to files or to QueueFS. Queue claims are committed
// before their payload is returned.
func (s *Storage) Read(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	queuePath, queue := strings.CutPrefix(path, "/queue/")
	if !queue {
		file, err := s.filePath(path)
		if err != nil {
			return nil, err
		}
		return os.ReadFile(file)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	data, err := queuefs.Read(ctx, tx, queuePath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return data, tx.Commit()
}

// Write routes workspace paths to files or to QueueFS.
func (s *Storage) Write(ctx context.Context, path string, data []byte) error {
	return s.write(ctx, s.db, path, data)
}

type executor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func (s *Storage) write(ctx context.Context, db executor, path string, data []byte) error {
	if queuePath, queue := strings.CutPrefix(path, "/queue/"); queue {
		return queuefs.Write(ctx, db, queuePath, data)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := s.filePath(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	return errors.Join(writeErr, f.Sync(), f.Close())
}

func (s *Storage) filePath(path string) (string, error) {
	relative, absolute := strings.CutPrefix(path, "/")
	namespace, _, found := strings.Cut(relative, "/")
	if !absolute || !found || !fs.ValidPath(relative) || strings.ContainsAny(relative, `\:`) ||
		(namespace != "sessions" && namespace != "staging") {
		return "", fmt.Errorf("invalid workspace file path %q", path)
	}
	for _, part := range strings.Split(relative, "/") {
		if !validComponent(part) {
			return "", fmt.Errorf("invalid workspace file path %q", path)
		}
	}
	return filepath.Join(s.root, filepath.FromSlash(relative)), nil
}

func validComponent(part string) bool {
	return part != "" && part != "." && part != ".." && filepath.IsLocal(part) &&
		!strings.ContainsAny(part, `/\:`) && strings.TrimRight(part, ". ") == part
}

func (s *Storage) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	switch version {
	case 0:
		if _, err := tx.ExecContext(ctx, schemaV1+queuefs.SchemaV1+"PRAGMA user_version = 1;"); err != nil {
			return err
		}
	case 1:
	default:
		return fmt.Errorf("unsupported workspace schema version %d", version)
	}
	return tx.Commit()
}

const schemaV1 = `
CREATE TABLE archive_commits (
	id TEXT PRIMARY KEY,
	published INTEGER NOT NULL DEFAULT 0 CHECK (published IN (0, 1))
);
CREATE TABLE archives (
	ordinal INTEGER PRIMARY KEY,
	id TEXT NOT NULL UNIQUE,
	commit_id TEXT NOT NULL REFERENCES archive_commits(id),
	session_id TEXT NOT NULL,
	seq INTEGER NOT NULL,
	path TEXT NOT NULL,
	state TEXT NOT NULL CHECK (state IN ('staged', 'pending', 'complete')),
	UNIQUE (session_id, seq)
);
CREATE INDEX archives_by_commit ON archives (commit_id, ordinal);
CREATE TABLE working_memory (
	archive_id TEXT PRIMARY KEY REFERENCES archives(id),
	l0 TEXT NOT NULL,
	l1 TEXT NOT NULL,
	covered_through INTEGER NOT NULL
);
`
