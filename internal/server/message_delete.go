package server

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"qchat/internal/database"
)

type MessageDeletedEvent struct {
	Type       string `json:"type"`
	Scope      string `json:"scope"`
	MessageID  int64  `json:"message_id"`
	SenderID   int64  `json:"sender_id"`
	ReceiverID int64  `json:"receiver_id,omitempty"`
}

func (h *ChatHub) BroadcastMessageDeleted(message *database.DeletedMessage) {
	if message == nil {
		return
	}
	data, err := json.Marshal(MessageDeletedEvent{
		Type:       "message_deleted",
		Scope:      message.Scope,
		MessageID:  message.MessageID,
		SenderID:   message.SenderID,
		ReceiverID: message.ReceiverID,
	})
	if err != nil {
		log.Println("消息删除通知序列化失败:", err)
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
			log.Println("消息删除通知队列已满，断开连接:", client.user.Username)
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

func (s *Server) handleMessageDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		w.Header().Set("Allow", http.MethodDelete)
		writeJSON(w, http.StatusMethodNotAllowed, APIResponse{Success: false, Message: "仅支持 DELETE 请求"})
		return
	}
	user, err := s.currentUser(r)
	if err != nil {
		writeUnauthorized(w)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/messages/delete/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || (parts[0] != "public" && parts[0] != "private") {
		http.NotFound(w, r)
		return
	}
	messageID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || messageID <= 0 {
		writeJSON(w, http.StatusBadRequest, APIResponse{Success: false, Message: "消息 ID 无效"})
		return
	}

	result, err := s.store.DeleteMessage(r.Context(), user.ID, user.IsAdmin, parts[0], messageID)
	if err != nil {
		switch {
		case errors.Is(err, database.ErrMessageNotFound):
			writeJSON(w, http.StatusNotFound, APIResponse{Success: false, Message: "消息不存在或已经删除"})
		case errors.Is(err, database.ErrMessageForbidden):
			writeJSON(w, http.StatusForbidden, APIResponse{Success: false, Message: "只能删除自己发送的消息"})
		default:
			log.Println("删除消息失败:", err)
			writeJSON(w, http.StatusInternalServerError, APIResponse{Success: false, Message: "删除消息失败"})
		}
		return
	}

	s.chatHub.BroadcastMessageDeleted(result)
	writeJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Message: "消息删除成功",
		Data: map[string]any{
			"scope":       result.Scope,
			"message_id":  result.MessageID,
			"sender_id":   result.SenderID,
			"receiver_id": result.ReceiverID,
		},
	})
}
