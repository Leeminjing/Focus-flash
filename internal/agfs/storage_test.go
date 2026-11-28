package agfs_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"focusflash/internal/agfs"
	"focusflash/internal/agfs/queuefs"
)

func TestRecoverArchivePublicationAndDelivery(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "workspace #1", ".focus-flash")
	dbPath := filepath.Join(root, "patrol.sqlite3")
	open := func() *agfs.Storage {
		t.Helper()
		store, err := agfs.Open(ctx, dbPath)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		if err := store.RecoverQueue(ctx); err != nil {
			t.Fatal(err)
		}
		return store
	}
	store := open()
	drafts := []agfs.ArchiveDraft{
		{SessionID: "a", Data: []byte("{\"text\":\"first\"}\n")},
		{SessionID: "b", Data: []byte("{\"text\":\"second\"}\n")},
	}
	// Allow the first rename and block the second after the index is registered.
	obstruction := filepath.Join(root, "sessions", "b", "round-2.jsonl")
	if err := os.MkdirAll(obstruction, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitRound(ctx, "round", drafts); err == nil {
		t.Fatal("commit succeeded with a blocked publication")
	}
	if data, err := store.Read(ctx, "/sessions/a/round-1.jsonl"); err != nil || string(data) != string(drafts[0].Data) {
		t.Fatalf("first file had not been published: %q, %v", data, err)
	}
	if archive, _, err := store.Latest(ctx, "a"); err != nil || archive.ID != "" {
		t.Fatalf("incomplete batch exposed an index: %+v, %v", archive, err)
	}
	if data, err := store.Read(ctx, agfs.WorkingMemoryDequeue); err != nil || data != nil {
		t.Fatalf("incomplete batch exposed a delivery: %s, %v", data, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(obstruction); err != nil {
		t.Fatal(err)
	}

	store = open()
	ids, err := store.CommitRound(ctx, "round", drafts)
	if err != nil || !slices.Equal(ids, []string{"round-1", "round-2"}) {
		t.Fatalf("recovered archives = %v, %v", ids, err)
	}
	dequeue := func(archiveID string) int64 {
		t.Helper()
		data, err := store.Read(ctx, agfs.WorkingMemoryDequeue)
		if err != nil {
			t.Fatal(err)
		}
		var message queuefs.Message
		var task agfs.MemoryTask
		if err := json.Unmarshal(data, &message); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(message.Data, &task); err != nil || task.ArchiveID != archiveID {
			t.Fatalf("delivery = %+v, %v, want archive %s", task, err, archiveID)
		}
		return message.ID
	}
	firstID := dequeue(ids[0])
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = open()
	if got := dequeue(ids[0]); got != firstID {
		t.Fatalf("recovered delivery ID = %d, want %d", got, firstID)
	}
	memory := agfs.WorkingMemory{L0: "Summary", L1: "First version", CoveredThrough: 1}
	if err := store.Settle(ctx, ids[0], firstID+1, memory); err == nil {
		t.Fatal("settled with an unclaimed delivery ID")
	}
	if archive, version, err := store.Latest(ctx, "a"); err != nil || archive.State != "pending" || version != (agfs.WorkingMemory{}) {
		t.Fatalf("failed acknowledgment left partial state: %+v, %+v, %v", archive, version, err)
	}
	if err := store.Settle(ctx, ids[0], firstID, memory); err != nil {
		t.Fatal(err)
	}
	if err := store.Settle(ctx, ids[0], firstID, agfs.WorkingMemory{}); err != nil {
		t.Fatalf("repeat settlement: %v", err)
	}
	secondID := dequeue(ids[1])
	if secondID <= firstID {
		t.Fatalf("delivery IDs out of order: %d then %d", firstID, secondID)
	}
	if err := store.Settle(ctx, ids[1], secondID, memory); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = open()
	for i, draft := range drafts {
		archive, version, err := store.Latest(ctx, draft.SessionID)
		if err != nil || archive.ID != ids[i] || archive.State != "complete" || version != memory {
			t.Fatalf("reopened snapshot = %+v, %+v, %v", archive, version, err)
		}
		data, err := store.Read(ctx, archive.Path)
		if err != nil || string(data) != string(draft.Data) {
			t.Fatalf("reopened original = %s, %v, want %s", data, err, draft.Data)
		}
	}
	if data, err := store.Read(ctx, agfs.WorkingMemoryDequeue); err != nil || data != nil {
		t.Fatalf("settled work reappeared: %s, %v", data, err)
	}
}
