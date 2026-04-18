package main

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestAccountStoreCRUDAndEncryption(t *testing.T) {
	t.Parallel()

	store, err := OpenAccountStore(t.TempDir()+"\\accounts.db", NewTokenCipher("unit-test-secret"))
	if err != nil {
		t.Fatalf("OpenAccountStore() error = %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	ctx := context.Background()
	record, err := store.Create(ctx, AccountInput{
		Label:    "主账号",
		Token:    "secret-token-123456",
		Enabled:  true,
		Priority: 120,
		Weight:   3,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	var ciphertext string
	if err := store.db.QueryRowContext(ctx, `SELECT token_ciphertext FROM accounts WHERE id = ?`, record.ID).Scan(&ciphertext); err != nil {
		t.Fatalf("scan ciphertext error = %v", err)
	}
	if ciphertext == record.Token || strings.Contains(ciphertext, record.Token) {
		t.Fatalf("ciphertext leaked plaintext token: %q", ciphertext)
	}

	records, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("List() len = %d, want 1", len(records))
	}
	if records[0].Token != "secret-token-123456" {
		t.Fatalf("List() token = %q, want decrypted token", records[0].Token)
	}
	if records[0].TotalRequests != 0 || records[0].SuccessRequests != 0 || records[0].FailedRequests != 0 {
		t.Fatalf("List() stats = %+v, want zero values", records[0])
	}

	newLabel := "备用账号"
	newWeight := 5
	updated, err := store.Update(ctx, record.ID, AccountUpdateInput{
		Label:  &newLabel,
		Weight: &newWeight,
	})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if updated.Label != newLabel || updated.Weight != newWeight {
		t.Fatalf("Update() = %+v, want label=%q weight=%d", updated, newLabel, newWeight)
	}

	if err := store.UpdateValidation(ctx, record.ID, accountStatusInvalid, "auth rejected", records[0].CreatedAt); err != nil {
		t.Fatalf("UpdateValidation() error = %v", err)
	}

	got, err := store.Get(ctx, record.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.LastStatus != accountStatusInvalid {
		t.Fatalf("Get().LastStatus = %q, want %q", got.LastStatus, accountStatusInvalid)
	}
	if err := store.RecordGlobalRequestStart(ctx); err != nil {
		t.Fatalf("RecordGlobalRequestStart() error = %v", err)
	}
	if err := store.RecordGlobalRequestSuccess(ctx); err != nil {
		t.Fatalf("RecordGlobalRequestSuccess() error = %v", err)
	}
	if err := store.RecordAccountRequestStart(ctx, record.ID); err != nil {
		t.Fatalf("RecordAccountRequestStart() error = %v", err)
	}
	if err := store.RecordAccountRequestFailure(ctx, record.ID); err != nil {
		t.Fatalf("RecordAccountRequestFailure() error = %v", err)
	}
	if err := store.RecordAccountRequestSuccess(ctx, record.ID); err != nil {
		t.Fatalf("RecordAccountRequestSuccess() error = %v", err)
	}

	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats() error = %v", err)
	}
	if stats.TotalRequests != 1 || stats.SuccessRequests != 1 || stats.FailedRequests != 0 {
		t.Fatalf("Stats() = %+v, want total=1 success=1 failure=0", stats)
	}

	got, err = store.Get(ctx, record.ID)
	if err != nil {
		t.Fatalf("Get() after stats error = %v", err)
	}
	if got.TotalRequests != 1 || got.SuccessRequests != 1 || got.FailedRequests != 1 {
		t.Fatalf("Get() stats = %+v, want total=1 success=1 failure=1", got)
	}
	if got.LastUsedAt.IsZero() || got.LastSuccessAt.IsZero() || got.LastFailureAt.IsZero() {
		t.Fatalf("Get() timestamps = %+v, want last used/success/failure to be set", got)
	}

	if err := store.Delete(ctx, record.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	count, err := store.Count(ctx)
	if err != nil {
		t.Fatalf("Count() error = %v", err)
	}
	if count != 0 {
		t.Fatalf("Count() = %d, want 0", count)
	}
}

func TestAccountStoreMigratesStatsColumns(t *testing.T) {
	t.Parallel()

	dbPath := t.TempDir() + "\\legacy.db"
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}

	_, err = db.Exec(`
		CREATE TABLE accounts (
			id TEXT PRIMARY KEY,
			label TEXT NOT NULL,
			token_ciphertext TEXT NOT NULL,
			token_preview TEXT NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1,
			priority INTEGER NOT NULL DEFAULT 100,
			weight INTEGER NOT NULL DEFAULT 1,
			last_status TEXT NOT NULL DEFAULT 'unknown',
			last_error TEXT NOT NULL DEFAULT '',
			last_checked_at TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
	`)
	if err != nil {
		t.Fatalf("create legacy schema error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close() error = %v", err)
	}

	store, err := OpenAccountStore(dbPath, NewTokenCipher("migration-secret"))
	if err != nil {
		t.Fatalf("OpenAccountStore() error = %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	ctx := context.Background()
	record, err := store.Create(ctx, AccountInput{
		Label:    "迁移测试",
		Token:    "token-abc",
		Enabled:  true,
		Priority: 100,
		Weight:   1,
	})
	if err != nil {
		t.Fatalf("Create() after migration error = %v", err)
	}
	if err := store.RecordAccountRequestStart(ctx, record.ID); err != nil {
		t.Fatalf("RecordAccountRequestStart() after migration error = %v", err)
	}

	got, err := store.Get(ctx, record.ID)
	if err != nil {
		t.Fatalf("Get() after migration error = %v", err)
	}
	if got.TotalRequests != 1 {
		t.Fatalf("Get().TotalRequests = %d, want 1", got.TotalRequests)
	}
}
