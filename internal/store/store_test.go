package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func openStore(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func hasTable(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n)
	if err == sql.ErrNoRows {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true
}

// WAL 是设计要求（13 节）。
func TestOpenEnablesWAL(t *testing.T) {
	db := openStore(t)
	var mode string
	if err := db.SqlDB().QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
}

// M1-T3 只建 settings 与 sessions 两张表，其余表随各自里程碑的迁移加入。
func TestMigrateFromScratch(t *testing.T) {
	db := openStore(t)
	for _, tbl := range []string{"schema_version", "settings", "sessions"} {
		if !hasTable(t, db.SqlDB(), tbl) {
			t.Errorf("迁移后应存在表 %q", tbl)
		}
	}
	if got := db.Version(); got < 1 {
		t.Errorf("schema_version 应 ≥1, got %d", got)
	}
}

// 迁移必须幂等：重复执行不报错、版本不再变化。
func TestMigrateIdempotent(t *testing.T) {
	db := openStore(t)
	v1 := db.Version()
	if err := db.Migrate(); err != nil {
		t.Fatalf("第二次 Migrate: %v", err)
	}
	if v2 := db.Version(); v2 != v1 {
		t.Fatalf("重复迁移后版本不应变化: %d -> %d", v1, v2)
	}
	// 同一物理文件再次 Open 也应幂等。
	db2, err := Open(db.Path())
	if err != nil {
		t.Fatalf("重新 Open: %v", err)
	}
	defer db2.Close()
	if db2.Version() != v1 {
		t.Fatalf("重开库版本漂移: %d -> %d", v1, db2.Version())
	}
}

// settings 表可用且主键冲突有明确行为。
func TestSettingsTableUsable(t *testing.T) {
	db := openStore(t)
	_, err := db.SqlDB().Exec(`INSERT INTO settings(key,value,updated_at) VALUES('a','1',1)`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SqlDB().Exec(`INSERT INTO settings(key,value,updated_at) VALUES('a','2',2)`); err == nil {
		t.Fatal("主键重复应报错")
	}
}
