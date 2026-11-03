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

	-- 私聊消息表
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

func (s *Store) CreateMessage(ctx context.Context, userID int64, content string) (*Message, error) {
	result, err := s.db.ExecContext(ctx,
		`INSERT INTO messages(user_id, content, created_at)
		 VALUES(?, ?, unixepoch())`,
		userID, content)

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

	// 数据库查询是 DESC，因此返回给前端时反转成正序。
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}

	return messages, nil
}

// CreatePrivateMessage 保存一条私聊消息。
func (s *Store) CreatePrivateMessage(ctx context.Context, senderID, receiverID int64, content string) (*PrivateMessage, error) {
	if senderID == receiverID {
		return nil, fmt.Errorf("不能向自己发送私聊消息")
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

	message, err := s.getPrivateMessageByID(ctx, id)
	if err != nil {
		return nil, err
	}

	return message, nil
}

// getPrivateMessageByID 根据消息ID读取私聊消息。
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
// userID 是当前登录用户，peerID 是对方用户。
// 只能查询两人之间的消息，不能查询其他用户的私聊记录。
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
