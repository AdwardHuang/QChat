package database

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

type User struct {
	ID           int64  `json:"id"`
	Username     string `json:"username"`
	PasswordHash string `json:"-"`
}

func Open(path string) (*Store, error) {
	dir := filepath.Dir(path)

	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("创建数据库目录失败: %w", err)
	}

	db, err := sql.Open("sqlite", filepath.ToSlash(path))
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}

	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		db.Close()
		return nil, err
	}

	if _, err := db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		db.Close()
		return nil, err
	}

	if _, err := db.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		db.Close()
		return nil, err
	}

	store := &Store{db: db}

	if err := store.initTables(); err != nil {
		db.Close()
		return nil, err
	}

	return store, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) initTables() error {
	sqlText := `
	CREATE TABLE IF NOT EXISTS users (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT NOT NULL UNIQUE,
		password_hash TEXT NOT NULL,
		created_at INTEGER NOT NULL
	);

	CREATE TABLE IF NOT EXISTS sessions (
		token_hash TEXT PRIMARY KEY,
		user_id INTEGER NOT NULL,
		expires_at INTEGER NOT NULL,
		created_at INTEGER NOT NULL,
		FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
	);

	CREATE INDEX IF NOT EXISTS idx_sessions_user_id
	ON sessions(user_id);

	CREATE INDEX IF NOT EXISTS idx_sessions_expires_at
	ON sessions(expires_at);

	CREATE TABLE IF NOT EXISTS messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id INTEGER NOT NULL,
		content TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		FOREIGN KEY(user_id) REFERENCES users(id)
	);
	`

	_, err := s.db.Exec(sqlText)
	if err != nil {
		return fmt.Errorf("初始化数据库表失败: %w", err)
	}

	return nil
}

func (s *Store) CreateAdminIfEmpty(ctx context.Context, username, passwordHash string) error {
	var count int

	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		return err
	}

	if count > 0 {
		return nil
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO users(username, password_hash, created_at) VALUES(?, ?, unixepoch())`,
		username, passwordHash)

	if err != nil {
		return fmt.Errorf("创建管理员账号失败: %w", err)
	}

	return nil
}

func (s *Store) FindUserByUsername(ctx context.Context, username string) (*User, error) {
	var user User

	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash FROM users WHERE username = ?`,
		username,
	).Scan(&user.ID, &user.Username, &user.PasswordHash)

	if err != nil {
		return nil, err
	}

	return &user, nil
}

func (s *Store) CreateSession(ctx context.Context, tokenHash string, userID int64, expiresAt int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions(token_hash, user_id, expires_at, created_at)
		 VALUES(?, ?, ?, unixepoch())`,
		tokenHash, userID, expiresAt)

	return err
}

func (s *Store) FindSessionUser(ctx context.Context, tokenHash string, now int64) (*User, error) {
	var user User

	err := s.db.QueryRowContext(ctx,
		`SELECT u.id, u.username
		 FROM sessions s
		 JOIN users u ON u.id = s.user_id
		 WHERE s.token_hash = ? AND s.expires_at > ?`,
		tokenHash, now,
	).Scan(&user.ID, &user.Username)

	if err != nil {
		return nil, err
	}

	return &user, nil
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE token_hash = ?`,
		tokenHash)

	return err
}
