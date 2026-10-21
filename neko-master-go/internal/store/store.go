package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动，无需 CGO
)

// DB 包装一个 SQLite 连接池。
type DB struct {
	sql  *sql.DB
	path string
}

// Open 打开（必要时创建）数据库并初始化 schema。
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}

	// 与上游一致的 pragma：WAL + 外键 + 忙等
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// 单写者模型：SQLite 并发写会互相阻塞，限制连接数避免 database is locked
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)

	if err := sqlDB.Ping(); err != nil {
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	db := &DB{sql: sqlDB, path: path}
	if err := db.init(); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return db, nil
}

// init 建表、建索引、写默认配置。
func (d *DB) init() error {
	stmts := make([]string, 0, len(schemaStatements)+len(indexStatements)+len(defaultConfig))
	stmts = append(stmts, schemaStatements...)
	stmts = append(stmts, indexStatements...)
	stmts = append(stmts, defaultConfig...)

	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("exec schema: %w\nstmt: %s", err, s)
		}
	}
	return tx.Commit()
}

// Close 关闭数据库。
func (d *DB) Close() error { return d.sql.Close() }

// Path 返回数据库文件路径。
func (d *DB) Path() string { return d.path }

// SQL 暴露底层句柄，供各 repository 使用。
func (d *DB) SQL() *sql.DB { return d.sql }
