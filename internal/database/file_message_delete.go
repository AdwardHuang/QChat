package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var (
	ErrFileMessageNotFound  = errors.New("文件消息不存在")
	ErrFileMessageForbidden = errors.New("没有权限删除该文件消息")
)

// DeletedFileMessage 保存删除文件消息后的必要信息。
type DeletedFileMessage struct {
	Scope         string
	MessageID     int64
	SenderID      int64
	ReceiverID    int64
	FileID        int64
	StorageName   string
	DeleteStorage bool
}

// DeleteFileMessage 删除文件消息。
// 普通用户只能删除自己发送的消息，管理员可以删除任何文件消息。
// 只有文件不再被其他消息引用时，才删除文件记录。
func (s *Store) DeleteFileMessage(ctx context.Context, actorID int64, isAdmin bool, scope string, messageID int64) (*DeletedFileMessage, error) {
	if actorID <= 0 || messageID <= 0 {
		return nil, errors.New("删除文件消息参数无效")
	}
	if scope != "public" && scope != "private" {
		return nil, errors.New("文件消息范围无效")
	}
	if err := s.ensureFileMessageTable(ctx); err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("开启删除事务失败: %w", err)
	}
	defer tx.Rollback()

	result := &DeletedFileMessage{
		Scope:     scope,
		MessageID: messageID,
	}

	var query string
	if scope == "public" {
		query = `
SELECT m.user_id, l.file_id, f.storage_name
FROM messages m
JOIN file_message_links l
	ON l.message_id = m.id AND l.scope = 'public'
JOIN file_attachments f
	ON f.id = l.file_id
WHERE m.id = ?`
		err = tx.QueryRowContext(ctx, query, messageID).Scan(
			&result.SenderID,
			&result.FileID,
			&result.StorageName,
		)
	} else {
		query = `
SELECT pm.sender_id, pm.receiver_id, l.file_id, f.storage_name
FROM private_messages pm
JOIN file_message_links l
	ON l.message_id = pm.id AND l.scope = 'private'
JOIN file_attachments f
	ON f.id = l.file_id
WHERE pm.id = ?`
		err = tx.QueryRowContext(ctx, query, messageID).Scan(
			&result.SenderID,
			&result.ReceiverID,
			&result.FileID,
			&result.StorageName,
		)
	}

	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrFileMessageNotFound
		}
		return nil, fmt.Errorf("查询待删除文件消息失败: %w", err)
	}

	if !isAdmin && result.SenderID != actorID {
		return nil, ErrFileMessageForbidden
	}

	// 删除原始聊天消息。
	var deleteSQL string
	if scope == "public" {
		deleteSQL = `DELETE FROM messages WHERE id = ?`
	} else {
		deleteSQL = `DELETE FROM private_messages WHERE id = ?`
	}

	deleteResult, err := tx.ExecContext(ctx, deleteSQL, messageID)
	if err != nil {
		return nil, fmt.Errorf("删除聊天消息失败: %w", err)
	}

	affected, err := deleteResult.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("读取消息删除结果失败: %w", err)
	}
	if affected == 0 {
		return nil, ErrFileMessageNotFound
	}

	// 删除文件与聊天消息之间的关联。
	_, err = tx.ExecContext(ctx, `
DELETE FROM file_message_links
WHERE scope = ? AND message_id = ?`,
		scope, messageID,
	)
	if err != nil {
		return nil, fmt.Errorf("删除文件消息关联失败: %w", err)
	}

	// 检查文件是否仍然被其他聊天消息引用。
	var referenceCount int
	err = tx.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM file_message_links
WHERE file_id = ?`,
		result.FileID,
	).Scan(&referenceCount)
	if err != nil {
		return nil, fmt.Errorf("检查文件引用失败: %w", err)
	}

	// 没有其他消息引用时，删除文件数据库记录。
	if referenceCount == 0 {
		_, err = tx.ExecContext(ctx, `
DELETE FROM file_attachments
WHERE id = ?`,
			result.FileID,
		)
		if err != nil {
			return nil, fmt.Errorf("删除文件记录失败: %w", err)
		}
		result.DeleteStorage = true
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交文件消息删除事务失败: %w", err)
	}

	return result, nil
}
