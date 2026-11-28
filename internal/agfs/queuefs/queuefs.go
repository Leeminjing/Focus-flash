// Package queuefs handles the queue paths routed by AGFS.
package queuefs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const SchemaV1 = `
CREATE TABLE queue_messages (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	queue TEXT NOT NULL,
	data TEXT NOT NULL,
	state TEXT NOT NULL CHECK (state IN ('pending', 'processing'))
);
CREATE INDEX queue_delivery ON queue_messages (queue, state, id);
`

type Message struct {
	ID   int64           `json:"id"`
	Data json.RawMessage `json:"data"`
}

// Read claims the oldest pending message within the caller's transaction.
// An empty queue returns nil data and no error.
func Read(ctx context.Context, tx *sql.Tx, path string) ([]byte, error) {
	queue, action, err := parsePath(path)
	if err != nil {
		return nil, err
	}
	if action != "dequeue" {
		return nil, fmt.Errorf("cannot read queue action %q", action)
	}
	var message Message
	var payload string
	err = tx.QueryRowContext(ctx, `
UPDATE queue_messages SET state = 'processing' WHERE id = (
	SELECT id FROM queue_messages WHERE queue = ? AND state = 'pending' ORDER BY id LIMIT 1
) RETURNING id, data`, queue).Scan(&message.ID, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	message.Data = json.RawMessage(payload)
	return json.Marshal(message)
}

type executor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// Write accepts a JSON payload for enqueue and a decimal message ID for ack.
// A caller can pass its transaction to couple delivery with business writes.
func Write(ctx context.Context, db executor, path string, data []byte) error {
	queue, action, err := parsePath(path)
	if err != nil {
		return err
	}
	switch action {
	case "enqueue":
		if !json.Valid(data) {
			return errors.New("queue payload must be valid JSON")
		}
		_, err := db.ExecContext(ctx, `INSERT INTO queue_messages (queue, data, state)
VALUES (?, ?, 'pending')`, queue, string(data))
		return err
	case "ack":
		id, err := strconv.ParseInt(string(data), 10, 64)
		if err != nil || id <= 0 {
			return errors.New("ack requires a positive message ID")
		}
		result, err := db.ExecContext(ctx, `DELETE FROM queue_messages
WHERE queue = ? AND id = ? AND state = 'processing'`, queue, id)
		if err != nil {
			return err
		}
		deleted, err := result.RowsAffected()
		if err == nil && deleted == 0 {
			return fmt.Errorf("message %d is not in flight in queue %q", id, queue)
		}
		return err
	default:
		return fmt.Errorf("cannot write queue action %q", action)
	}
}

// Recover returns in-flight messages to pending without changing their IDs.
func Recover(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, "UPDATE queue_messages SET state = 'pending' WHERE state = 'processing'")
	return err
}

func parsePath(path string) (queue, action string, err error) {
	queue, action, found := strings.Cut(path, "/")
	if !found || queue == "" || queue == "." || queue == ".." || strings.ContainsAny(queue, `\:`) {
		return "", "", fmt.Errorf("invalid queue path %q", path)
	}
	return queue, action, nil
}
