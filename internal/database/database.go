package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

var (
	ErrUserNotFound   = errors.New("用户不存在")
	ErrUsernameExists = errors.New("用户名已存在")
	ErrLastAdmin      = errors.New("不能禁用最后一个管理员")
	ErrInvalidUser    = errors.New("无效的用户")
)

type Store struct {
	db *sql.DB
}

type User struct {
	ID           int64  `json:"id"`
	Username     string `json:"username"`
	PasswordHash string `json:"-"`
	IsAdmin      bool   `json:"is_admin"`
	IsActive     bool   `json:"is_active"`
	CreatedAt    int64  `json:"created_at"`
}

type Message struct {
	ID        int64  `json:"id"`
	UserID    int64  `json:"user_id"`
	Username  string `json:"username"`
	Content   string `json:"content"`
	CreatedAt int64  `json:"created_at"`
}

// PrivateMessage 私聊消息
type PrivateMessage struct {
	ID           int64  `json:"id"`
	SenderID     int64  `json:"sender_id"`
	SenderName   string `json:"sender_name"`
	ReceiverID   int64  `json:"receiver_id"`
	ReceiverName string `json:"receiver_name"`
	Content      string `json:"content"`
	CreatedAt    int64  `json:"created_at"`
	IsRead       bool   `json:"is_read"`
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

	pragmas := []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA foreign_keys=ON`,
		`PRAGMA busy_timeout=5000`,
	}

	for _, pragma := range pragmas {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("设置数据库参数失败: %w", err)
		}
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

// initTables 初始化数据库表，并兼容旧版本数据库。
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

CREATE TABLE IF NOT EXISTS private_messages (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	sender_id INTEGER NOT NULL,
	receiver_id INTEGER NOT NULL,
	content TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	is_read INTEGER NOT NULL DEFAULT 0,
	CHECK(sender_id <> receiver_id),
	FOREIGN KEY(sender_id) REFERENCES users(id),
	FOREIGN KEY(receiver_id) REFERENCES users(id)
);

CREATE INDEX IF NOT EXISTS idx_private_messages_sender_receiver
ON private_messages(sender_id, receiver_id, id);

CREATE INDEX IF NOT EXISTS idx_private_messages_receiver_sender
ON private_messages(receiver_id, sender_id, id);

CREATE INDEX IF NOT EXISTS idx_private_messages_unread
ON private_messages(receiver_id, sender_id, is_read);
`

	if _, err := s.db.Exec(sqlText); err != nil {
		return fmt.Errorf("初始化数据库表失败: %w", err)
	}

	if err := s.migrateUserColumns(); err != nil {
		return fmt.Errorf("迁移用户表失败: %w", err)
	}

	return nil
}

// migrateUserColumns 为旧数据库增加管理员和账号状态字段。
func (s *Store) migrateUserColumns() error {
	columns := []struct {
		name string
		sql  string
	}{
		{
			name: "is_admin",
			sql:  `ALTER TABLE users ADD COLUMN is_admin INTEGER NOT NULL DEFAULT 0`,
		},
		{
			name: "is_active",
			sql:  `ALTER TABLE users ADD COLUMN is_active INTEGER NOT NULL DEFAULT 1`,
		},
	}

	for _, column := range columns {
		exists, err := s.hasColumn("users", column.name)
		if err != nil {
			return err
		}

		if !exists {
			if _, err := s.db.Exec(column.sql); err != nil {
				return fmt.Errorf("增加字段 %s 失败: %w", column.name, err)
			}
		}
	}

	return nil
}

func (s *Store) hasColumn(tableName, columnName string) (bool, error) {
	rows, err := s.db.Query(`PRAGMA table_info(` + tableName + `)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue any

		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, err
		}

		if name == columnName {
			return true, nil
		}
	}

	return false, rows.Err()
}

// CreateAdminIfEmpty 初始化管理员。
// 如果数据库为空，则创建配置文件指定的管理员。
// 如果是旧数据库且没有管理员，则将最早创建的用户提升为管理员。
func (s *Store) CreateAdminIfEmpty(ctx context.Context, username, passwordHash string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		return err
	}

	if count == 0 {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO users(username, password_hash, created_at, is_admin, is_active)
			 VALUES(?, ?, unixepoch(), 1, 1)`,
			username, passwordHash,
		)
		if err != nil {
			return fmt.Errorf("创建管理员账号失败: %w", err)
		}
	} else {
		var adminCount int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM users WHERE is_admin = 1`,
		).Scan(&adminCount); err != nil {
			return err
		}

		if adminCount == 0 {
			_, err := tx.ExecContext(ctx,
				`UPDATE users
				 SET is_admin = 1
				 WHERE id = (SELECT id FROM users ORDER BY id LIMIT 1)`,
			)
			if err != nil {
				return fmt.Errorf("迁移旧管理员账号失败: %w", err)
			}
		}
	}

	return tx.Commit()
}

// FindUserByUsername 根据用户名查询用户。
func (s *Store) FindUserByUsername(ctx context.Context, username string) (*User, error) {
	var user User
	var isAdmin, isActive int

	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, is_admin, is_active, created_at
		 FROM users WHERE username = ?`,
		username,
	).Scan(
		&user.ID,
		&user.Username,
		&user.PasswordHash,
		&isAdmin,
		&isActive,
		&user.CreatedAt,
	)
	if err != nil {
		return nil, err
	}

	user.IsAdmin = isAdmin != 0
	user.IsActive = isActive != 0

	return &user, nil
}

// CreateUser 创建普通用户。
func (s *Store) CreateUser(ctx context.Context, username, passwordHash string) (*User, error) {
	result, err := s.db.ExecContext(ctx,
		`INSERT INTO users(username, password_hash, created_at, is_admin, is_active)
		 VALUES(?, ?, unixepoch(), 0, 1)`,
		username, passwordHash,
	)
	if err != nil {
		if isUniqueConstraintError(err) {
			return nil, ErrUsernameExists
		}
		return nil, fmt.Errorf("创建用户失败: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}

	return s.GetUserByID(ctx, id)
}

// GetUserByID 根据用户 ID 查询用户。
func (s *Store) GetUserByID(ctx context.Context, userID int64) (*User, error) {
	var user User
	var isAdmin, isActive int

	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, is_admin, is_active, created_at
		 FROM users WHERE id = ?`,
		userID,
	).Scan(
		&user.ID,
		&user.Username,
		&isAdmin,
		&isActive,
		&user.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}

	user.IsAdmin = isAdmin != 0
	user.IsActive = isActive != 0

	return &user, nil
}

// ListUsers 查询全部用户，按 ID 排序。
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, username, is_admin, is_active, created_at
		 FROM users ORDER BY id ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("查询用户列表失败: %w", err)
	}
	defer rows.Close()

	users := make([]User, 0)

	for rows.Next() {
		var user User
		var isAdmin, isActive int

		if err := rows.Scan(
			&user.ID,
			&user.Username,
			&isAdmin,
			&isActive,
			&user.CreatedAt,
		); err != nil {
			return nil, err
		}

		user.IsAdmin = isAdmin != 0
		user.IsActive = isActive != 0

		users = append(users, user)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return users, nil
}

// UpdateUserPassword 修改用户密码，并使该用户所有旧会话失效。
func (s *Store) UpdateUserPassword(ctx context.Context, userID int64, passwordHash string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx,
		`UPDATE users SET password_hash = ? WHERE id = ?`,
		passwordHash, userID,
	)
	if err != nil {
		return fmt.Errorf("更新用户密码失败: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrUserNotFound
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM sessions WHERE user_id = ?`,
		userID,
	); err != nil {
		return fmt.Errorf("清除用户会话失败: %w", err)
	}

	return tx.Commit()
}

// SetUserActive 设置用户启用或禁用状态。
// 禁用用户时同时删除其所有登录会话。
func (s *Store) SetUserActive(ctx context.Context, userID int64, active bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var isAdmin int
	err = tx.QueryRowContext(ctx,
		`SELECT is_admin FROM users WHERE id = ?`,
		userID,
	).Scan(&isAdmin)

	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUserNotFound
		}
		return err
	}

	if !active && isAdmin != 0 {
		var adminCount int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM users WHERE is_admin = 1 AND is_active = 1`,
		).Scan(&adminCount); err != nil {
			return err
		}

		if adminCount <= 1 {
			return ErrLastAdmin
		}
	}

	activeValue := 0
	if active {
		activeValue = 1
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET is_active = ? WHERE id = ?`,
		activeValue, userID,
	); err != nil {
		return fmt.Errorf("更新用户状态失败: %w", err)
	}

	if !active {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM sessions WHERE user_id = ?`,
			userID,
		); err != nil {
			return fmt.Errorf("清除用户会话失败: %w", err)
		}
	}

	return tx.Commit()
}

// CreateSession 创建登录会话。
func (s *Store) CreateSession(ctx context.Context, tokenHash string, userID int64, expiresAt int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions(token_hash, user_id, expires_at, created_at)
		 VALUES(?, ?, ?, unixepoch())`,
		tokenHash, userID, expiresAt,
	)
	return err
}

// FindSessionUser 根据会话查询用户。
// 禁用账号不能继续使用已有会话。
func (s *Store) FindSessionUser(ctx context.Context, tokenHash string, now int64) (*User, error) {
	var user User
	var isAdmin, isActive int

	err := s.db.QueryRowContext(ctx,
		`SELECT u.id, u.username, u.is_admin, u.is_active, u.created_at
		 FROM sessions s
		 JOIN users u ON u.id = s.user_id
		 WHERE s.token_hash = ?
		   AND s.expires_at > ?
		   AND u.is_active = 1`,
		tokenHash, now,
	).Scan(
		&user.ID,
		&user.Username,
		&isAdmin,
		&isActive,
		&user.CreatedAt,
	)

	if err != nil {
		return nil, err
	}

	user.IsAdmin = isAdmin != 0
	user.IsActive = isActive != 0

	return &user, nil
}

// DeleteSession 删除指定会话。
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE token_hash = ?`,
		tokenHash,
	)
	return err
}

// DeleteUserSessions 删除用户的全部会话。
func (s *Store) DeleteUserSessions(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE user_id = ?`,
		userID,
	)
	return err
}

// CreateMessage 保存公共消息。
func (s *Store) CreateMessage(ctx context.Context, userID int64, content string) (*Message, error) {
	result, err := s.db.ExecContext(ctx,
		`INSERT INTO messages(user_id, content, created_at)
		 VALUES(?, ?, unixepoch())`,
		userID, content,
	)
	if err != nil {
		return nil, fmt.Errorf("保存消息失败: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("读取消息ID失败: %w", err)
	}

	var message Message
	err = s.db.QueryRowContext(ctx,
		`SELECT m.id, m.user_id, u.username, m.content, m.created_at
		 FROM messages m
		 JOIN users u ON u.id = m.user_id
		 WHERE m.id = ?`,
		id,
	).Scan(
		&message.ID,
		&message.UserID,
		&message.Username,
		&message.Content,
		&message.CreatedAt,
	)

	if err != nil {
		return nil, fmt.Errorf("读取新消息失败: %w", err)
	}

	return &message, nil
}

// ListMessages 查询公共聊天历史。
func (s *Store) ListMessages(ctx context.Context, beforeID int64, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}

	var rows *sql.Rows
	var err error

	if beforeID > 0 {
		rows, err = s.db.QueryContext(ctx,
			`SELECT m.id, m.user_id, u.username, m.content, m.created_at
			 FROM messages m
			 JOIN users u ON u.id = m.user_id
			 WHERE m.id < ?
			 ORDER BY m.id DESC
			 LIMIT ?`,
			beforeID, limit,
		)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT m.id, m.user_id, u.username, m.content, m.created_at
			 FROM messages m
			 JOIN users u ON u.id = m.user_id
			 ORDER BY m.id DESC
			 LIMIT ?`,
			limit,
		)
	}

	if err != nil {
		return nil, fmt.Errorf("查询历史消息失败: %w", err)
	}
	defer rows.Close()

	messages := make([]Message, 0, limit)

	for rows.Next() {
		var message Message

		if err := rows.Scan(
			&message.ID,
			&message.UserID,
			&message.Username,
			&message.Content,
			&message.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("读取历史消息失败: %w", err)
		}

		messages = append(messages, message)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 数据库查询是 DESC，返回前端时反转为正序。
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}

	return messages, nil
}

// CreatePrivateMessage 保存一条私聊消息。
func (s *Store) CreatePrivateMessage(ctx context.Context, senderID, receiverID int64, content string) (*PrivateMessage, error) {
	if senderID <= 0 || receiverID <= 0 || senderID == receiverID {
		return nil, fmt.Errorf("无效的私聊用户")
	}

	result, err := s.db.ExecContext(ctx,
		`INSERT INTO private_messages(sender_id, receiver_id, content, created_at)
		 VALUES(?, ?, ?, unixepoch())`,
		senderID, receiverID, content,
	)
	if err != nil {
		return nil, fmt.Errorf("保存私聊消息失败: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("读取私聊消息ID失败: %w", err)
	}

	return s.getPrivateMessageByID(ctx, id)
}

// getPrivateMessageByID 根据消息 ID 读取私聊消息。
func (s *Store) getPrivateMessageByID(ctx context.Context, id int64) (*PrivateMessage, error) {
	var message PrivateMessage
	var isRead int

	err := s.db.QueryRowContext(ctx,
		`SELECT
			pm.id,
			pm.sender_id,
			sender.username,
			pm.receiver_id,
			receiver.username,
			pm.content,
			pm.created_at,
			pm.is_read
		 FROM private_messages pm
		 JOIN users sender ON sender.id = pm.sender_id
		 JOIN users receiver ON receiver.id = pm.receiver_id
		 WHERE pm.id = ?`,
		id,
	).Scan(
		&message.ID,
		&message.SenderID,
		&message.SenderName,
		&message.ReceiverID,
		&message.ReceiverName,
		&message.Content,
		&message.CreatedAt,
		&isRead,
	)

	if err != nil {
		return nil, fmt.Errorf("读取私聊消息失败: %w", err)
	}

	message.IsRead = isRead != 0

	return &message, nil
}

// ListPrivateMessages 查询当前用户与指定用户之间的私聊记录。
func (s *Store) ListPrivateMessages(ctx context.Context, userID, peerID, beforeID int64, limit int) ([]PrivateMessage, error) {
	if userID <= 0 || peerID <= 0 || userID == peerID {
		return nil, fmt.Errorf("无效的私聊用户")
	}

	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}

	query := `
SELECT
	pm.id,
	pm.sender_id,
	sender.username,
	pm.receiver_id,
	receiver.username,
	pm.content,
	pm.created_at,
	pm.is_read
FROM private_messages pm
JOIN users sender ON sender.id = pm.sender_id
JOIN users receiver ON receiver.id = pm.receiver_id
WHERE (
	(pm.sender_id = ? AND pm.receiver_id = ?)
	OR
	(pm.sender_id = ? AND pm.receiver_id = ?)
)
`

	args := []any{userID, peerID, peerID, userID}

	if beforeID > 0 {
		query += ` AND pm.id < ?`
		args = append(args, beforeID)
	}

	query += ` ORDER BY pm.id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询私聊历史失败: %w", err)
	}
	defer rows.Close()

	messages := make([]PrivateMessage, 0, limit)

	for rows.Next() {
		var message PrivateMessage
		var isRead int

		if err := rows.Scan(
			&message.ID,
			&message.SenderID,
			&message.SenderName,
			&message.ReceiverID,
			&message.ReceiverName,
			&message.Content,
			&message.CreatedAt,
			&isRead,
		); err != nil {
			return nil, fmt.Errorf("读取私聊历史失败: %w", err)
		}

		message.IsRead = isRead != 0
		messages = append(messages, message)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 按时间正序返回。
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}

	return messages, nil
}

// MarkPrivateMessagesRead 将指定发送者发给当前用户的消息标记为已读。
func (s *Store) MarkPrivateMessagesRead(ctx context.Context, receiverID, senderID int64) error {
	if receiverID <= 0 || senderID <= 0 || receiverID == senderID {
		return fmt.Errorf("无效的私聊用户")
	}

	_, err := s.db.ExecContext(ctx,
		`UPDATE private_messages
		 SET is_read = 1
		 WHERE sender_id = ?
		   AND receiver_id = ?
		   AND is_read = 0`,
		senderID, receiverID,
	)

	if err != nil {
		return fmt.Errorf("标记私聊消息已读失败: %w", err)
	}

	return nil
}

// CountUnreadPrivateMessages 查询指定发送者发给当前用户的未读消息数量。
func (s *Store) CountUnreadPrivateMessages(ctx context.Context, receiverID, senderID int64) (int, error) {
	var count int

	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*)
		 FROM private_messages
		 WHERE receiver_id = ?
		   AND sender_id = ?
		   AND is_read = 0`,
		receiverID, senderID,
	).Scan(&count)

	if err != nil {
		return 0, fmt.Errorf("查询未读私聊消息失败: %w", err)
	}

	return count, nil
}

// isUniqueConstraintError 判断 SQLite 唯一约束冲突。
func isUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}

	message := err.Error()
	return containsAny(message,
		"UNIQUE constraint failed",
		"constraint failed",
	)
}

func containsAny(value string, patterns ...string) bool {
	for _, pattern := range patterns {
		if len(pattern) > 0 && len(value) >= len(pattern) {
			for i := 0; i <= len(value)-len(pattern); i++ {
				if value[i:i+len(pattern)] == pattern {
					return true
				}
			}
		}
	}

	return false
}
