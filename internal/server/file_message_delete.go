package server

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"qchat/internal/database"
)

// FileMessageDeletedEvent 是文件消息删除通知。
type FileMessageDeletedEvent struct {
	Type       string `json:"type"`
	Scope      string `json:"scope"`
	MessageID  int64  `json:"message_id"`
	SenderID   int64  `json:"sender_id,omitempty"`
	ReceiverID int64  `json:"receiver_id,omitempty"`
}

// BroadcastFileMessageDeleted 广播文件消息删除事件。
func (h *ChatHub) BroadcastFileMessageDeleted(message *database.DeletedFileMessage) {
	if message == nil {
		return
	}

	data, err := json.Marshal(FileMessageDeletedEvent{
		Type:       "file_message_deleted",
		Scope:      message.Scope,
		MessageID:  message.MessageID,
		SenderID:   message.SenderID,
		ReceiverID: message.ReceiverID,
	})
	if err != nil {
		log.Println("文件消息删除通知序列化失败:", err)
		return
	}

	removed := false
	h.mu.Lock()

	for client := range h.clients {
		if message.Scope == "private" &&
			client.user.ID != message.SenderID &&
			client.user.ID != message.ReceiverID {
			continue
		}

		select {
		case client.send <- data:
		default:
			log.Println("文件消息删除通知队列已满，断开连接:", client.user.Username)
			delete(h.clients, client)
			close(client.send)
			_ = client.conn.Close()
			removed = true
		}
	}

	h.mu.Unlock()

	if removed {
		h.BroadcastOnlineUsers()
	}
}

// handleFileMessageDelete 删除公共或私聊文件消息。
func (s *Server) handleFileMessageDelete(w http.ResponseWriter, r *http.Request, uploadDir string) {
	if r.Method != http.MethodDelete {
		w.Header().Set("Allow", http.MethodDelete)
		writeJSON(w, http.StatusMethodNotAllowed, APIResponse{
			Success: false,
			Message: "仅支持 DELETE 请求",
		})
		return
	}

	user, err := s.currentUser(r)
	if err != nil {
		writeUnauthorized(w)
		return
	}

	// URL 格式：
	// /api/files/messages/public/{messageID}
	// /api/files/messages/private/{messageID}
	path := strings.TrimPrefix(r.URL.Path, "/api/files/messages/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}

	scope := parts[0]
	if scope != "public" && scope != "private" {
		http.NotFound(w, r)
		return
	}

	messageID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || messageID <= 0 {
		writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Message: "文件消息 ID 无效",
		})
		return
	}

	result, err := s.store.DeleteFileMessage(
		r.Context(),
		user.ID,
		user.IsAdmin,
		scope,
		messageID,
	)
	if err != nil {
		if errors.Is(err, database.ErrFileMessageNotFound) {
			writeJSON(w, http.StatusNotFound, APIResponse{
				Success: false,
				Message: "文件消息不存在或已经删除",
			})
			return
		}

		if errors.Is(err, database.ErrFileMessageForbidden) {
			writeJSON(w, http.StatusForbidden, APIResponse{
				Success: false,
				Message: "只能删除自己发送的文件消息",
			})
			return
		}

		log.Println("删除文件消息失败:", err)
		writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Message: "删除文件消息失败",
		})
		return
	}

	// 数据库事务已经提交，只有文件没有其他引用时才删除磁盘文件。
	if result.DeleteStorage {
		if len(result.StorageName) != 64 {
			log.Println("文件存储名称无效，无法删除实际文件:", result.FileID)
		} else if _, err := hex.DecodeString(result.StorageName); err != nil {
			log.Println("文件存储名称格式无效:", result.FileID, err)
		} else {
			filePath := filepath.Join(uploadDir, result.StorageName)
			if err := os.Remove(filePath); err != nil && !errors.Is(err, os.ErrNotExist) {
				log.Println("删除磁盘文件失败:", filePath, err)
			}
		}
	}

	// 公共消息通知所有在线用户；
	// 私聊消息只通知发送者和接收者。
	s.chatHub.BroadcastFileMessageDeleted(result)

	writeJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Message: "文件消息删除成功",
		Data: map[string]any{
			"scope":       result.Scope,
			"message_id":  result.MessageID,
			"sender_id":   result.SenderID,
			"receiver_id": result.ReceiverID,
		},
	})
}
