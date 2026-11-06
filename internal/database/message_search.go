package database

import (
	"context"
	"fmt"
	"strings"
)

type MessageSearchResult struct {
	Scope        string `json:"scope"`
	MessageID    int64  `json:"message_id"`
	SenderID     int64  `json:"sender_id"`
	SenderName   string `json:"sender_name"`
	ReceiverID   int64  `json:"receiver_id,omitempty"`
	ReceiverName string `json:"receiver_name,omitempty"`
	PeerID       int64  `json:"peer_id,omitempty"`
	PeerName     string `json:"peer_name,omitempty"`
	Content      string `json:"content"`
	CreatedAt    int64  `json:"created_at"`
}

func escapeSearchKeyword(keyword string) string {
	replacer := strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_")
	return "%" + replacer.Replace(keyword) + "%"
}

// SearchMessages 搜索当前用户可访问的公共和私聊文本消息。
func (s *Store) SearchMessages(ctx context.Context, userID int64, keyword string, limit, offset int) ([]MessageSearchResult, int, error) {
	keyword = strings.TrimSpace(keyword)
	if userID <= 0 || keyword == "" {
		return nil, 0, fmt.Errorf("搜索参数无效")
	}
	if err := s.ensureFileMessageTable(ctx); err != nil {
		return nil, 0, err
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	pattern := escapeSearchKeyword(keyword)

	const matchedSQL = `
WITH matched AS (
	SELECT
		'public' AS scope,
		m.id AS message_id,
		m.user_id AS sender_id,
		u.username AS sender_name,
		0 AS receiver_id,
		'' AS receiver_name,
		0 AS peer_id,
		'' AS peer_name,
		m.content AS content,
		m.created_at AS created_at
	FROM messages m
	JOIN users u ON u.id = m.user_id
	WHERE m.content LIKE ? ESCAPE '\'
	  AND NOT EXISTS (
		SELECT 1 FROM file_message_links l
		WHERE l.scope = 'public' AND l.message_id = m.id
	  )

	UNION ALL

	SELECT
		'private' AS scope,
		pm.id AS message_id,
		pm.sender_id AS sender_id,
		sender.username AS sender_name,
		pm.receiver_id AS receiver_id,
		receiver.username AS receiver_name,
		CASE WHEN pm.sender_id = ? THEN pm.receiver_id ELSE pm.sender_id END AS peer_id,
		CASE WHEN pm.sender_id = ? THEN receiver.username ELSE sender.username END AS peer_name,
		pm.content AS content,
		pm.created_at AS created_at
	FROM private_messages pm
	JOIN users sender ON sender.id = pm.sender_id
	JOIN users receiver ON receiver.id = pm.receiver_id
	WHERE pm.content LIKE ? ESCAPE '\'
	  AND (pm.sender_id = ? OR pm.receiver_id = ?)
	  AND NOT EXISTS (
		SELECT 1 FROM file_message_links l
		WHERE l.scope = 'private' AND l.message_id = pm.id
	  )
)
`

	var total int
	countSQL := matchedSQL + `SELECT COUNT(*) FROM matched`
	if err := s.db.QueryRowContext(ctx, countSQL,
		pattern, userID, userID, pattern, userID, userID,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("统计搜索结果失败: %w", err)
	}

	querySQL := matchedSQL + `
SELECT
	scope, message_id, sender_id, sender_name,
	receiver_id, receiver_name, peer_id, peer_name,
	content, created_at
FROM matched
ORDER BY created_at DESC, message_id DESC
LIMIT ? OFFSET ?`

	rows, err := s.db.QueryContext(ctx, querySQL,
		pattern, userID, userID, pattern, userID, userID,
		limit, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("搜索聊天记录失败: %w", err)
	}
	defer rows.Close()

	results := make([]MessageSearchResult, 0, limit)
	for rows.Next() {
		var item MessageSearchResult
		if err := rows.Scan(
			&item.Scope,
			&item.MessageID,
			&item.SenderID,
			&item.SenderName,
			&item.ReceiverID,
			&item.ReceiverName,
			&item.PeerID,
			&item.PeerName,
			&item.Content,
			&item.CreatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("读取搜索结果失败: %w", err)
		}
		results = append(results, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	return results, total, nil
}
