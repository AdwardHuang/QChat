package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var (
	ErrMessageNotFound  = errors.New("消息不存在")
	ErrMessageForbidden = errors.New("没有权限删除该消息")
)

type DeletedMessage struct {
	Scope      string
	MessageID  int64
	SenderID   int64
	ReceiverID int64
}

func (s *Store) DeleteMessage(ctx context.Context, actorID int64, isAdmin bool, scope string, messageID int64) (*DeletedMessage, error) {
	if actorID <= 0 || messageID <= 0 {
		return nil, errors.New("删除消息参数无效")
	}
	if scope != "public" && scope != "private" {
		return nil, errors.New("消息范围无效")
	}
	if err := s.ensureFileMessageTable(ctx); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("开启删除事务失败: %w", err)
	}
	defer tx.Rollback()

	result := &DeletedMessage{Scope: scope, MessageID: messageID}
	if scope == "public" {
		err = tx.QueryRowContext(ctx, `
SELECT user_id
FROM messages
WHERE id = ?
AND NOT EXISTS (
	SELECT 1 FROM file_message_links
	WHERE scope = 'public' AND message_id = messages.id
)`, messageID).Scan(&result.SenderID)
	} else {
		err = tx.QueryRowContext(ctx, `
SELECT sender_id, receiver_id
FROM private_messages
WHERE id = ?
AND NOT EXISTS (
	SELECT 1 FROM file_message_links
	WHERE scope = 'private' AND message_id = private_messages.id
)`, messageID).Scan(&result.SenderID, &result.ReceiverID)
	}
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrMessageNotFound
		}
		return nil, fmt.Errorf("查询待删除消息失败: %w", err)
	}
	if !isAdmin && result.SenderID != actorID {
		return nil, ErrMessageForbidden
	}

	deleteSQL := `DELETE FROM messages WHERE id = ?`
	if scope == "private" {
		deleteSQL = `DELETE FROM private_messages WHERE id = ?`
	}
	deleteResult, err := tx.ExecContext(ctx, deleteSQL, messageID)
	if err != nil {
		return nil, fmt.Errorf("删除消息失败: %w", err)
	}
	affected, err := deleteResult.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected == 0 {
		return nil, ErrMessageNotFound
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交消息删除事务失败: %w", err)
	}
	return result, nil
}
