package store

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migration 是一条按版本号升序执行的迁移。
type migration struct {
	version int
	name    string
	sql     string
}

// migrationSources 是被嵌进去的迁移脚本，抽成变量（类型为 fs.FS）以便测试注入额外版本。
var migrationSources fs.FS = migrationFS

type DB struct {
	sql     *sql.DB
	path    string
	version int
}

// Open 打开（必要时创建）数据库，启用 WAL 并执行未应用的迁移。
func Open(path string) (*DB, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库 %s: %w", path, err)
	}
	// modernc 驱动下连接池里的每个连接都要各自设置 journal_mode，
	// 因此限制为单连接，避免读到非 WAL 的连接。
	sqlDB.SetMaxOpenConns(1)

	var mode string
	if err := sqlDB.QueryRow(`PRAGMA journal_mode=WAL`).Scan(&mode); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("启用 WAL: %w", err)
	}
	if mode != "wal" {
		sqlDB.Close()
		return nil, fmt.Errorf("数据库未能进入 WAL 模式（当前 %q）", mode)
	}

	d := &DB{sql: sqlDB, path: path}
	if err := d.Migrate(); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return d, nil
}

func (d *DB) SqlDB() *sql.DB { return d.sql }
func (d *DB) Path() string   { return d.path }
func (d *DB) Version() int   { return d.version }
func (d *DB) Close() error   { return d.sql.Close() }

// Migrate 按版本号升序执行尚未应用的迁移；已应用过的会被跳过，因此可重复调用。
func (d *DB) Migrate() error {
	if _, err := d.sql.Exec(`CREATE TABLE IF NOT EXISTS schema_version (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("创建 schema_version: %w", err)
	}

	migrations, err := loadMigrations()
	if err != nil {
		return err
	}

	// 先从库里读出现有版本作为唯一事实来源（重开已迁移的库时为上次版本）。
	var cur int
	if err := d.sql.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&cur); err != nil {
		return fmt.Errorf("读取 schema_version: %w", err)
	}

	for _, m := range migrations {
		if cur >= m.version {
			continue
		}
		if err := d.apply(m); err != nil {
			return fmt.Errorf("应用迁移 %s: %w", m.name, err)
		}
		cur = m.version
	}
	d.version = cur
	return nil
}

func (d *DB) apply(m migration) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(m.sql); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO schema_version(version,name,applied_at) VALUES(?,?,?)`,
		m.version, m.name, time.Now().Unix(),
	); err != nil {
		return err
	}
	return tx.Commit()
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationSources, "migrations")
	if err != nil {
		return nil, fmt.Errorf("读取迁移目录: %w", err)
	}
	var out []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		var v int
		if _, err := fmt.Sscanf(e.Name(), "%04d_", &v); err != nil || v <= 0 {
			return nil, fmt.Errorf("迁移文件名不合法（应为 NNNN_名称.sql）：%s", e.Name())
		}
		b, err := fs.ReadFile(migrationSources, "migrations/"+e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: v, name: e.Name(), sql: string(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i := 1; i < len(out); i++ {
		if out[i].version == out[i-1].version {
			return nil, fmt.Errorf("迁移版本号重复：%s 与 %s", out[i-1].name, out[i].name)
		}
	}
	return out, nil
}
