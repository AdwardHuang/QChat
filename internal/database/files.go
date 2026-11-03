package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// FileRecord 保存上传文件的元数据。
// 文件实体保存在文件系统中，数据库只保存随机存储名和文件属性。
type FileRecord struct {
	ID           int64  `json:"id"`
	UploaderID   int64  `json:"uploader_id"`
	StorageName  string `json:"-"`
	OriginalName string `json:"original_name"`
	ContentType  string `json:"content_type"`
	Size         int64  `json:"size"`
	CreatedAt    int64  `json:"created_at"`
}

var ErrFileNotFound = errors.New("文件记录不存在")

// ensureFileTable 初始化文件记录表。
// 使用 IF NOT EXISTS，兼容已经存在的 QChat 数据库。
func (s *Store) ensureFileTable(ctx context.Context) error {
	const query = `
CREATE TABLE IF NOT EXISTS file_attachments (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	uploader_id INTEGER NOT NULL,
	storage_name TEXT NOT NULL UNIQUE,
	original_name TEXT NOT NULL,
	content_type TEXT NOT NULL,
	file_size INTEGER NOT NULL,
	created_at INTEGER NOT NULL,
	FOREIGN KEY(uploader_id) REFERENCES users(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_file_attachments_uploader
ON file_attachments(uploader_id, id);
`
	if _, err := s.db.ExecContext(ctx, query); err != nil {
		return fmt.Errorf("初始化文件记录表失败: %w", err)
	}
	return nil
}

// CreateFileRecord 创建上传文件的数据库记录。
func (s *Store) CreateFileRecord(ctx context.Context, uploaderID int64, storageName, originalName, contentType string, size int64) (*FileRecord, error) {
	storageName = strings.TrimSpace(storageName)
	originalName = strings.TrimSpace(originalName)
	contentType = strings.TrimSpace(contentType)

	if uploaderID <= 0 || storageName == "" || originalName == "" || size < 0 {
		return nil, errors.New("文件记录参数无效")
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if err := s.ensureFileTable(ctx); err != nil {
		return nil, err
	}

	createdAt := time.Now().Unix()
	result, err := s.db.ExecContext(ctx, `
INSERT INTO file_attachments (
	uploader_id, storage_name, original_name, content_type, file_size, created_at
) VALUES (?, ?, ?, ?, ?, ?)`,
		uploaderID, storageName, originalName, contentType, size, createdAt)
	if err != nil {
		return nil, fmt.Errorf("保存文件记录失败: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("读取文件记录 ID 失败: %w", err)
	}
	return &FileRecord{
		ID:           id,
		UploaderID:   uploaderID,
		StorageName:  storageName,
		OriginalName: originalName,
		ContentType:  contentType,
		Size:         size,
		CreatedAt:    createdAt,
	}, nil
}

// GetFileRecordByID 根据文件 ID 查询文件记录。
func (s *Store) GetFileRecordByID(ctx context.Context, fileID int64) (*FileRecord, error) {
	if fileID <= 0 {
		return nil, ErrFileNotFound
	}
	if err := s.ensureFileTable(ctx); err != nil {
		return nil, err
	}

	var record FileRecord
	err := s.db.QueryRowContext(ctx, `
SELECT id, uploader_id, storage_name, original_name, content_type, file_size, created_at
FROM file_attachments
WHERE id = ?`, fileID).Scan(
		&record.ID,
		&record.UploaderID,
		&record.StorageName,
		&record.OriginalName,
		&record.ContentType,
		&record.Size,
		&record.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrFileNotFound
		}
		return nil, fmt.Errorf("查询文件记录失败: %w", err)
	}
	return &record, nil
}

// DeleteFileRecord 删除文件元数据记录。
// 调用方应在确认文件实体已删除或准备回滚时调用。
func (s *Store) DeleteFileRecord(ctx context.Context, fileID int64) error {
	if fileID <= 0 {
		return ErrFileNotFound
	}
	if err := s.ensureFileTable(ctx); err != nil {
		return err
	}

	result, err := s.db.ExecContext(ctx, `DELETE FROM file_attachments WHERE id = ?`, fileID)
	if err != nil {
		return fmt.Errorf("删除文件记录失败: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("读取文件记录删除结果失败: %w", err)
	}
	if affected == 0 {
		return ErrFileNotFound
	}
	return nil
}
