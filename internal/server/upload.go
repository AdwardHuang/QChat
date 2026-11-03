package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"qchat/internal/database"
)

const (
	maxUploadSize        int64 = 20 << 20
	maxMultipartOverhead int64 = 1 << 20
)

// WithFileUploads 在现有 HTTP Handler 外层增加文件上传和下载接口。
// 上传：POST /api/files/upload，multipart/form-data，文件字段名为 file。
// 下载：GET /api/files/{id}。
// 文件实体保存在 uploadDir，数据库只保存文件元数据。
func (s *Server) WithFileUploads(next http.Handler, uploadDir string) http.Handler {
	uploadDir = strings.TrimSpace(uploadDir)
	if uploadDir == "" {
		uploadDir = "uploads"
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/files/upload":
			s.handleFileUpload(w, r, uploadDir)
		case strings.HasPrefix(r.URL.Path, "/api/files/"):
			s.handleFileDownload(w, r, uploadDir)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

func (s *Server) handleFileUpload(w http.ResponseWriter, r *http.Request, uploadDir string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, APIResponse{
			Success: false,
			Message: "仅支持 POST 请求",
		})
		return
	}

	user, err := s.currentUser(r)
	if err != nil {
		writeUnauthorized(w)
		return
	}

	// 限制整个 multipart 请求大小，额外预留 1 MB multipart 开销。
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize+maxMultipartOverhead)

	if err := r.ParseMultipartForm(1 << 20); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, APIResponse{
				Success: false,
				Message: "上传请求过大，单个文件最大 20 MB",
			})
			return
		}
		writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Message: "上传数据格式错误，请使用 multipart/form-data，文件字段名为 file",
		})
		return
	}

	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}

	src, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Message: "请选择要上传的文件",
		})
		return
	}
	defer src.Close()

	// 处理文件名，避免用户通过文件名指定存储路径。
	originalName := strings.TrimSpace(header.Filename)
	originalName = strings.ReplaceAll(originalName, "\\", "/")
	originalName = filepath.Base(originalName)

	if originalName == "" || originalName == "." || !utf8.ValidString(originalName) {
		writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Message: "文件名无效",
		})
		return
	}

	if header.Size > maxUploadSize {
		writeJSON(w, http.StatusRequestEntityTooLarge, APIResponse{
			Success: false,
			Message: "单个文件最大 20 MB",
		})
		return
	}

	if err := os.MkdirAll(uploadDir, 0755); err != nil {
		writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Message: "创建上传目录失败",
		})
		return
	}

	// 先写入临时文件，成功后再移动为正式文件。
	tempFile, err := os.CreateTemp(uploadDir, ".qchat-upload-*")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Message: "创建临时文件失败",
		})
		return
	}

	tempPath := tempFile.Name()
	defer os.Remove(tempPath)

	written, copyErr := io.Copy(tempFile, io.LimitReader(src, maxUploadSize+1))
	closeErr := tempFile.Close()

	if copyErr != nil || closeErr != nil {
		writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Message: "保存上传文件失败",
		})
		return
	}

	if written > maxUploadSize {
		writeJSON(w, http.StatusRequestEntityTooLarge, APIResponse{
			Success: false,
			Message: "单个文件最大 20 MB",
		})
		return
	}

	if written == 0 {
		writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Message: "不能上传空文件",
		})
		return
	}

	// 根据文件实际内容识别 MIME 类型，不直接信任客户端传入的 Content-Type。
	contentType := "application/octet-stream"
	sampleFile, err := os.Open(tempPath)
	if err == nil {
		buffer := make([]byte, 512)
		n, readErr := sampleFile.Read(buffer)
		_ = sampleFile.Close()

		if readErr == nil || errors.Is(readErr, io.EOF) {
			if detected := http.DetectContentType(buffer[:n]); detected != "" {
				contentType = detected
			}
		}
	}

	// 使用随机 32 字节生成存储文件名，不使用用户上传的原始文件名。
	randomName := make([]byte, 32)
	if _, err := rand.Read(randomName); err != nil {
		writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Message: "生成文件存储名称失败",
		})
		return
	}

	storageName := hex.EncodeToString(randomName)
	finalPath := filepath.Join(uploadDir, storageName)

	if err := os.Rename(tempPath, finalPath); err != nil {
		writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Message: "移动上传文件失败",
		})
		return
	}

	// 保存文件元数据。
	record, err := s.store.CreateFileRecord(
		r.Context(),
		user.ID,
		storageName,
		originalName,
		contentType,
		written,
	)
	if err != nil {
		_ = os.Remove(finalPath)

		writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Message: "保存文件记录失败",
		})
		return
	}

	writeJSON(w, http.StatusCreated, APIResponse{
		Success: true,
		Message: "文件上传成功",
		Data: map[string]any{
			"id":            record.ID,
			"original_name": record.OriginalName,
			"content_type":  record.ContentType,
			"size":          record.Size,
			"created_at":    record.CreatedAt,
			"url":           fmt.Sprintf("/api/files/%d", record.ID),
		},
	})
}

func (s *Server) handleFileDownload(w http.ResponseWriter, r *http.Request, uploadDir string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeJSON(w, http.StatusMethodNotAllowed, APIResponse{
			Success: false,
			Message: "仅支持 GET 或 HEAD 请求",
		})
		return
	}

	// 下载接口同样要求登录。
	if _, err := s.currentUser(r); err != nil {
		writeUnauthorized(w)
		return
	}

	idText := strings.TrimPrefix(r.URL.Path, "/api/files/")
	if idText == "" || strings.Contains(idText, "/") {
		http.NotFound(w, r)
		return
	}

	fileID, err := strconv.ParseInt(idText, 10, 64)
	if err != nil || fileID <= 0 {
		http.NotFound(w, r)
		return
	}

	record, err := s.store.GetFileRecordByID(r.Context(), fileID)
	if err != nil {
		if errors.Is(err, database.ErrFileNotFound) {
			http.NotFound(w, r)
			return
		}

		writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Message: "读取文件记录失败",
		})
		return
	}

	// 存储名只允许 64 位十六进制字符，不能包含路径分隔符。
	if len(record.StorageName) != 64 {
		http.Error(w, "文件存储记录无效", http.StatusInternalServerError)
		return
	}

	if _, err := hex.DecodeString(record.StorageName); err != nil {
		http.Error(w, "文件存储记录无效", http.StatusInternalServerError)
		return
	}

	filePath := filepath.Join(uploadDir, record.StorageName)

	file, err := os.Open(filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
			return
		}

		writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Message: "打开文件失败",
		})
		return
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		http.Error(w, "文件不可用", http.StatusInternalServerError)
		return
	}

	contentType := record.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set(
		"Content-Disposition",
		mime.FormatMediaType("attachment", map[string]string{
			"filename": record.OriginalName,
		}),
	)

	http.ServeContent(
		w,
		r,
		record.OriginalName,
		time.Unix(record.CreatedAt, 0),
		file,
	)
}
