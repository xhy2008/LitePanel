package store

import (
	"fmt"
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

	// 注入的版本必须避开真实库已有的版本号（迁移按版本号判定是否已应用），
	// 所以取当前版本 +1 / +2，而不是写死 0002/0003。migrations/ 里加文件
	// 不必再同步改这两个测试。
	base := db.Version()
	next := base + 1
	v2 := fstest.MapFS{
		"migrations/0001_init.sql": &fstest.MapFile{Data: []byte(`-- 已经在真实库里，不会再执行`)},
		fmt.Sprintf("migrations/%04d_extra.sql", next): &fstest.MapFile{Data: []byte(
			`CREATE TABLE extra (id INTEGER PRIMARY KEY);`)},
	}

	old := migrationSources
	migrationSources = fs.FS(v2)
	defer func() { migrationSources = old }()

	if err := db.Migrate(); err != nil {
		t.Fatalf("增量迁移失败: %v", err)
	}
	if db.Version() != next {
		t.Fatalf("版本应升到 %d, got %d", next, db.Version())
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

	base := db.Version()
	bad := fstest.MapFS{
		fmt.Sprintf("migrations/%04d_bad.sql", base+1): &fstest.MapFile{
			Data: []byte(`CREATE TABLE ok_once(id); CREATE TABLE ok_once(again);`)},
		fmt.Sprintf("migrations/%04d_after.sql", base+2): &fstest.MapFile{
			Data: []byte(`CREATE TABLE after_it(id);`)},
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
	if db.Version() != base {
		t.Fatalf("版本应停在 %d, got %d", base, db.Version())
	}
}
