package store

import (
	"io/fs"
	"testing"
	"testing/fstest"
)

// v1 library upgrading to v2: only the new migration should run,
// and the v1 table must survive.
func TestMigrateIncremental(t *testing.T) {
	db := openStore(t)
	if _, err := db.SqlDB().Exec(`INSERT INTO settings(key,value,updated_at) VALUES('keep','1',1)`); err != nil {
		t.Fatal(err)
	}

	v2 := fstest.MapFS{
		"migrations/0001_init.sql": &fstest.MapFile{Data: []byte(`-- 已经在真实库里，不会再执行`)},
		"migrations/0002_extra.sql": &fstest.MapFile{Data: []byte(
			`CREATE TABLE extra (id INTEGER PRIMARY KEY);`)},
	}

	old := migrationSources
	migrationSources = fs.FS(v2)
	defer func() { migrationSources = old }()

	if err := db.Migrate(); err != nil {
		t.Fatalf("增量迁移失败: %v", err)
	}
	if db.Version() != 2 {
		t.Fatalf("版本应升到 2, got %d", db.Version())
	}
	if !hasTable(t, db.SqlDB(), "extra") {
		t.Fatal("新表 extra 应存在")
	}
	var v string
	if err := db.SqlDB().QueryRow(`SELECT value FROM settings WHERE key='keep'`).Scan(&v); err != nil {
		t.Fatalf("旧数据丢失: %v", err)
	}
}

// 迁移脚本出错时必须能回滚该条迁移，且版本号不被推进。
func TestMigrateRollbackOnBadSQL(t *testing.T) {
	db := openStore(t)

	bad := fstest.MapFS{
		"migrations/0001_init.sql":  &fstest.MapFile{Data: []byte(`SELECT 1`)},
		"migrations/0002_bad.sql":   &fstest.MapFile{Data: []byte(`CREATE TABLE ok_once(id); CREATE TABLE ok_once(again);`)},
		"migrations/0003_after.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE after_it(id);`)},
	}
	old := migrationSources
	migrationSources = fs.FS(bad)
	defer func() { migrationSources = old }()

	if err := db.Migrate(); err == nil {
		t.Fatal("坏迁移应报错")
	}
	if hasTable(t, db.SqlDB(), "after_it") {
		t.Fatal("失败之后的迁移不应继续执行")
	}
	if db.Version() != 1 {
		t.Fatalf("版本应停在 1, got %d", db.Version())
	}
}
