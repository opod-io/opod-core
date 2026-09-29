package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// A database written before the per-key policy left still carries the four
// api_keys columns. Opening it drops them, keeps every key, and a second open
// is a no-op; a fresh database never has them.
func TestKeyPolicyColumnsAreDropped(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// The table as the previous release created it, with one key in it.
	if _, err := raw.ExecContext(ctx, `CREATE TABLE api_keys (
		id TEXT PRIMARY KEY, hash TEXT NOT NULL UNIQUE, name TEXT NOT NULL, scope TEXT NOT NULL,
		user_id TEXT NOT NULL DEFAULT '', quota_daily_tokens INTEGER NOT NULL DEFAULT 0,
		rpm_limit INTEGER NOT NULL DEFAULT 0, tpm_limit INTEGER NOT NULL DEFAULT 0, allowed_models TEXT,
		expires_at INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL, revoked INTEGER NOT NULL DEFAULT 0);
		INSERT INTO api_keys(id, hash, name, scope, rpm_limit, created_at) VALUES('k1','h1','old','user',5,1)`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	for pass := 1; pass <= 2; pass++ {
		opened, err := OpenSQLite(path)
		if err != nil {
			t.Fatalf("open %d: %v", pass, err)
		}
		st := opened.(*sqliteStore)
		for _, d := range droppedColumns {
			exists, err := columnExists(ctx, st.db, d.table, d.column)
			if err != nil {
				t.Fatal(err)
			}
			if exists {
				t.Errorf("open %d: %s.%s still exists", pass, d.table, d.column)
			}
		}
		k, err := st.APIKeys().GetByID(ctx, "k1")
		if err != nil || k == nil || k.Name != "old" {
			t.Fatalf("open %d: the old key: %v %+v", pass, err, k)
		}
		st.Close()
	}
}
