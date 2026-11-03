package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// FileMessagePayload 是文件消息保存在 content 字段中的 JSON 结构。
type FileMessagePayload struct {
	Type         string `json:"type"`
	FileID       int64  `json:"file_id"`
	OriginalName string `json:"original_name"`
	ContentType  string `json:"content_type"`
	Size         int64  `json:"size"`
	URL          string `json:"url"`
}

// ensureFileMessageTable 初始化文件消息关联表和请求幂等表。
func (s *Store) ensureFileMessageTable(ctx context.Context) error {
	if err := s.ensureFileTable(ctx); err != nil {
		return err
	}

	const query = `
CREATE TABLE IF NOT EXISTS file_message_links (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	file_id INTEGER NOT NULL,
	scope TEXT NOT NULL CHECK(scope IN ('public', 'private')),
	message_id INTEGER NOT NULL,
	sender_id INTEGER NOT NULL,
	receiver_id INTEGER,
	created_at INTEGER NOT NULL,
	UNIQUE(scope, message_id),
	FOREIGN KEY(file_id) REFERENCES file_attachments(id) ON DELETE CASCADE,
	FOREIGN KEY(sender_id) REFERENCES users(id) ON DELETE CASCADE,
	FOREIGN KEY(receiver_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_file_message_links_file
ON file_message_links(file_id);

CREATE INDEX IF NOT EXISTS idx_file_message_links_private
ON file_message_links(scope, sender_id, receiver_id);

CREATE TABLE IF NOT EXISTS file_message_requests (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	request_id TEXT NOT NULL,
	sender_id INTEGER NOT NULL,
	scope TEXT NOT NULL CHECK(scope IN ('public', 'private')),
	file_id INTEGER NOT NULL,
	receiver_id INTEGER,
	message_id INTEGER NOT NULL,
	created_at INTEGER NOT NULL,
	UNIQUE(sender_id, request_id),
	FOREIGN KEY(sender_id) REFERENCES users(id) ON DELETE CASCADE,
	FOREIGN KEY(receiver_id) REFERENCES users(id) ON DELETE CASCADE,
	FOREIGN KEY(file_id) REFERENCES file_attachments(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_file_message_requests_message
ON file_message_requests(scope, message_id);
`
	if _, err := s.db.ExecContext(ctx, query); err != nil {
		return fmt.Errorf("初始化文件消息关联表失败: %w", err)
	}

	return nil
}

// validateFileMessageRequest 验证请求 ID。
func validateFileMessageRequest(senderID, fileID int64, requestID string) error {
	if senderID <= 0 || fileID <= 0 {
		return errors.New("文件消息参数无效")
	}

	requestID = strings.TrimSpace(requestID)
	if requestID == "" || len(requestID) > 128 {
		return errors.New("文件消息 request_id 无效")
	}

	return nil
}

// findExistingFileMessageRequest 查询已经处理过的文件消息请求。
func (s *Store) findExistingFileMessageRequest(ctx context.Context, tx *sql.Tx, senderID int64, requestID string, scope string, receiverID, fileID int64) (int64, bool, error) {
	var (
		existingScope      string
		existingFileID     int64
		existingMessageID  int64
		existingReceiverID sql.NullInt64
	)

	err := tx.QueryRowContext(ctx, `
SELECT scope, file_id, receiver_id, message_id
FROM file_message_requests
WHERE sender_id = ? AND request_id = ?`,
		senderID, requestID,
	).Scan(
		&existingScope,
		&existingFileID,
		&existingReceiverID,
		&existingMessageID,
	)

	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("查询文件消息请求失败: %w", err)
	}

	if existingScope != scope || existingFileID != fileID {
		return 0, false, errors.New("request_id 已被其他文件消息请求使用")
	}

	if scope == "public" && existingReceiverID.Valid {
		return 0, false, errors.New("request_id 对应的消息范围不一致")
	}

	if scope == "private" && (!existingReceiverID.Valid || existingReceiverID.Int64 != receiverID) {
		return 0, false, errors.New("request_id 对应的私聊接收者不一致")
	}

	return existingMessageID, true, nil
}

// getPublicFileMessageTx 根据消息 ID 读取公共文件消息。
func (s *Store) getPublicFileMessageTx(ctx context.Context, tx *sql.Tx, messageID int64) (*Message, error) {
	var message Message

	err := tx.QueryRowContext(ctx, `
SELECT m.id, m.user_id, u.username, m.content, m.created_at
FROM messages m
JOIN users u ON u.id = m.user_id
WHERE m.id = ?`,
		messageID,
	).Scan(
		&message.ID,
		&message.UserID,
		&message.Username,
		&message.Content,
		&message.CreatedAt,
	)

	if err != nil {
		return nil, fmt.Errorf("读取已存在的公共文件消息失败: %w", err)
	}

	return &message, nil
}

// getPrivateFileMessageTx 根据消息 ID 读取私聊文件消息。
func (s *Store) getPrivateFileMessageTx(ctx context.Context, tx *sql.Tx, messageID int64) (*PrivateMessage, error) {
	var (
		message PrivateMessage
		isRead  int
	)

	err := tx.QueryRowContext(ctx, `
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
WHERE pm.id = ?`,
		messageID,
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
		return nil, fmt.Errorf("读取已存在的私聊文件消息失败: %w", err)
	}

	message.IsRead = isRead != 0
	return &message, nil
}

// CreatePublicFileMessage 创建公共聊天室文件消息。
// 同一 senderID + requestID 只会创建一条消息，重复请求返回原消息。
func (s *Store) CreatePublicFileMessage(ctx context.Context, senderID, fileID int64, requestID string) (*Message, error) {
	if err := validateFileMessageRequest(senderID, fileID, requestID); err != nil {
		return nil, err
	}

	requestID = strings.TrimSpace(requestID)

	if err := s.ensureFileMessageTable(ctx); err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	existingMessageID, exists, err := s.findExistingFileMessageRequest(
		ctx, tx, senderID, requestID, "public", 0, fileID,
	)
	if err != nil {
		return nil, err
	}

	if exists {
		message, err := s.getPublicFileMessageTx(ctx, tx, existingMessageID)
		if err != nil {
			return nil, err
		}

		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("提交重复公共文件消息请求失败: %w", err)
		}

		return message, nil
	}

	var payload FileMessagePayload
	err = tx.QueryRowContext(ctx, `
SELECT id, original_name, content_type, file_size
FROM file_attachments
WHERE id = ? AND uploader_id = ?`,
		fileID, senderID,
	).Scan(
		&payload.FileID,
		&payload.OriginalName,
		&payload.ContentType,
		&payload.Size,
	)

	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrFileNotFound
		}
		return nil, fmt.Errorf("查询上传文件失败: %w", err)
	}

	payload.Type = "file"
	payload.URL = fmt.Sprintf("/api/files/%d", fileID)

	contentBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("生成文件消息失败: %w", err)
	}

	content := string(contentBytes)
	createdAt := time.Now().Unix()

	result, err := tx.ExecContext(ctx, `
INSERT INTO messages(user_id, content, created_at)
VALUES(?, ?, ?)`,
		senderID, content, createdAt,
	)
	if err != nil {
		return nil, fmt.Errorf("保存公共文件消息失败: %w", err)
	}

	messageID, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}

	_, err = tx.ExecContext(ctx, `
INSERT INTO file_message_links(
	file_id, scope, message_id, sender_id, receiver_id, created_at
) VALUES(?, 'public', ?, ?, NULL, ?)`,
		fileID, messageID, senderID, createdAt,
	)
	if err != nil {
		return nil, fmt.Errorf("关联公共文件消息失败: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
INSERT INTO file_message_requests(
	request_id, sender_id, scope, file_id, receiver_id, message_id, created_at
) VALUES(?, ?, 'public', ?, NULL, ?, ?)`,
		requestID, senderID, fileID, messageID, createdAt,
	)
	if err != nil {
		return nil, fmt.Errorf("保存公共文件消息请求记录失败: %w", err)
	}

	message, err := s.getPublicFileMessageTx(ctx, tx, messageID)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交公共文件消息失败: %w", err)
	}

	return message, nil
}

// CreatePrivateFileMessage 创建私聊文件消息。
// 同一 senderID + requestID 只会创建一条消息，重复请求返回原消息。
func (s *Store) CreatePrivateFileMessage(ctx context.Context, senderID, receiverID, fileID int64, requestID string) (*PrivateMessage, error) {
	if senderID <= 0 || receiverID <= 0 || fileID <= 0 || senderID == receiverID {
		return nil, errors.New("私聊文件消息参数无效")
	}

	if err := validateFileMessageRequest(senderID, fileID, requestID); err != nil {
		return nil, err
	}

	requestID = strings.TrimSpace(requestID)

	if err := s.ensureFileMessageTable(ctx); err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	existingMessageID, exists, err := s.findExistingFileMessageRequest(
		ctx, tx, senderID, requestID, "private", receiverID, fileID,
	)
	if err != nil {
		return nil, err
	}

	if exists {
		message, err := s.getPrivateFileMessageTx(ctx, tx, existingMessageID)
		if err != nil {
			return nil, err
		}

		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("提交重复私聊文件消息请求失败: %w", err)
		}

		return message, nil
	}

	var receiverActive int
	err = tx.QueryRowContext(ctx, `
SELECT is_active FROM users WHERE id = ?`,
		receiverID,
	).Scan(&receiverActive)

	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, fmt.Errorf("查询私聊接收者失败: %w", err)
	}

	if receiverActive == 0 {
		return nil, errors.New("接收者账号已被禁用")
	}

	var payload FileMessagePayload
	err = tx.QueryRowContext(ctx, `
SELECT id, original_name, content_type, file_size
FROM file_attachments
WHERE id = ? AND uploader_id = ?`,
		fileID, senderID,
	).Scan(
		&payload.FileID,
		&payload.OriginalName,
		&payload.ContentType,
		&payload.Size,
	)

	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrFileNotFound
		}
		return nil, fmt.Errorf("查询上传文件失败: %w", err)
	}

	payload.Type = "file"
	payload.URL = fmt.Sprintf("/api/files/%d", fileID)

	contentBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("生成私聊文件消息失败: %w", err)
	}

	content := string(contentBytes)
	createdAt := time.Now().Unix()

	result, err := tx.ExecContext(ctx, `
INSERT INTO private_messages(
	sender_id, receiver_id, content, created_at, is_read
) VALUES(?, ?, ?, ?, 0)`,
		senderID, receiverID, content, createdAt,
	)
	if err != nil {
		return nil, fmt.Errorf("保存私聊文件消息失败: %w", err)
	}

	messageID, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}

	_, err = tx.ExecContext(ctx, `
INSERT INTO file_message_links(
	file_id, scope, message_id, sender_id, receiver_id, created_at
) VALUES(?, 'private', ?, ?, ?, ?)`,
		fileID, messageID, senderID, receiverID, createdAt,
	)
	if err != nil {
		return nil, fmt.Errorf("关联私聊文件消息失败: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
INSERT INTO file_message_requests(
	request_id, sender_id, scope, file_id, receiver_id, message_id, created_at
) VALUES(?, ?, 'private', ?, ?, ?, ?)`,
		requestID, senderID, fileID, receiverID, messageID, createdAt,
	)
	if err != nil {
		return nil, fmt.Errorf("保存私聊文件消息请求记录失败: %w", err)
	}

	message, err := s.getPrivateFileMessageTx(ctx, tx, messageID)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交私聊文件消息失败: %w", err)
	}

	return message, nil
}

// CanAccessFile 判断用户是否有权下载指定文件。
// 文件所有者始终可以访问。
// 公共消息关联文件允许所有登录用户访问。
// 私聊消息关联文件只允许发送者和接收者访问。
func (s *Store) CanAccessFile(ctx context.Context, userID, fileID int64) (bool, error) {
	if userID <= 0 || fileID <= 0 {
		return false, nil
	}

	if err := s.ensureFileMessageTable(ctx); err != nil {
		return false, err
	}

	var allowed int
	err := s.db.QueryRowContext(ctx, `
SELECT EXISTS(
	SELECT 1
	FROM file_attachments f
	WHERE f.id = ?
	AND (
		f.uploader_id = ?
		OR EXISTS(
			SELECT 1
			FROM file_message_links l
			WHERE l.file_id = f.id
			AND l.scope = 'public'
		)
		OR EXISTS(
			SELECT 1
			FROM file_message_links l
			WHERE l.file_id = f.id
			AND l.scope = 'private'
			AND (
				l.sender_id = ?
				OR l.receiver_id = ?
			)
		)
	)
)`,
		fileID, userID, userID, userID,
	).Scan(&allowed)

	if err != nil {
		return false, fmt.Errorf("检查文件访问权限失败: %w", err)
	}

	return allowed != 0, nil
}
