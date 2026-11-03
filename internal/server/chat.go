package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"qchat/internal/database"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 4096
	sendBufferSize = 32
)

type ChatHub struct {
	store   *database.Store
	mu      sync.Mutex
	clients map[*ChatClient]struct{}
}

type ChatClient struct {
	hub  *ChatHub
	conn *websocket.Conn
	send chan []byte
	user *database.User
}

type ChatInput struct {
	Type       string `json:"type"`
	Content    string `json:"content"`
	ReceiverID int64  `json:"receiver_id"`
	FileID     int64  `json:"file_id"`
	RequestID  string `json:"request_id"`
}

type OnlineUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

// FileMessageAck 是文件消息发送确认。
type FileMessageAck struct {
	RequestID string `json:"request_id"`
	Success   bool   `json:"success"`
	MessageID int64  `json:"message_id,omitempty"`
	Error     string `json:"error,omitempty"`
}

type ChatOutput struct {
	Type           string                   `json:"type"`
	Message        *database.Message        `json:"message,omitempty"`
	PrivateMessage *database.PrivateMessage `json:"private_message,omitempty"`
	Users          []OnlineUser             `json:"users,omitempty"`
	Count          int                      `json:"count,omitempty"`
	Error          string                   `json:"error,omitempty"`
	FileMessageAck *FileMessageAck          `json:"file_message_ack,omitempty"`
}

func NewChatHub(store *database.Store) *ChatHub {
	return &ChatHub{
		store:   store,
		clients: make(map[*ChatClient]struct{}),
	}
}

func (h *ChatHub) AddClient(client *ChatClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[client] = struct{}{}
}

func (h *ChatHub) RemoveClient(client *ChatClient) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if _, exists := h.clients[client]; exists {
		delete(h.clients, client)
		close(client.send)
	}
}

// DisconnectUser 断开指定用户的全部 WebSocket 连接。
func (h *ChatHub) DisconnectUser(userID int64) {
	removed := false
	h.mu.Lock()

	for client := range h.clients {
		if client.user.ID != userID {
			continue
		}

		delete(h.clients, client)
		close(client.send)
		_ = client.conn.Close()
		removed = true
	}

	h.mu.Unlock()

	if removed {
		h.BroadcastOnlineUsers()
	}
}

func (h *ChatHub) Broadcast(message *database.Message) {
	data, err := json.Marshal(ChatOutput{
		Type:    "message",
		Message: message,
	})
	if err != nil {
		log.Println("消息序列化失败:", err)
		return
	}

	removed := false
	h.mu.Lock()

	for client := range h.clients {
		select {
		case client.send <- data:
		default:
			log.Println("客户端发送队列已满，断开连接:", client.user.Username)
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

// 向私聊双方的所有在线连接推送消息。
func (h *ChatHub) BroadcastPrivateMessage(message *database.PrivateMessage) {
	data, err := json.Marshal(ChatOutput{
		Type:           "private_message",
		PrivateMessage: message,
	})
	if err != nil {
		log.Println("私聊消息序列化失败:", err)
		return
	}

	removed := false
	h.mu.Lock()

	for client := range h.clients {
		if client.user.ID != message.SenderID && client.user.ID != message.ReceiverID {
			continue
		}

		select {
		case client.send <- data:
		default:
			log.Println("私聊发送队列已满，断开连接:", client.user.Username)
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

func (h *ChatHub) SendError(client *ChatClient, message string) {
	data, err := json.Marshal(ChatOutput{
		Type:  "error",
		Error: message,
	})
	if err != nil {
		return
	}

	select {
	case client.send <- data:
	default:
		log.Println("发送错误通知失败:", client.user.Username)
	}
}

// SendFileMessageAck 只向发起请求的 WebSocket 连接返回文件消息确认。
func (h *ChatHub) SendFileMessageAck(client *ChatClient, ack FileMessageAck) {
	data, err := json.Marshal(ChatOutput{
		Type:           "file_message_ack",
		FileMessageAck: &ack,
	})
	if err != nil {
		log.Println("文件消息 ACK 序列化失败:", err)
		return
	}

	select {
	case client.send <- data:
	default:
		log.Println("文件消息 ACK 发送队列已满:", client.user.Username)
	}
}

func (h *ChatHub) OnlineUsers() []OnlineUser {
	h.mu.Lock()
	defer h.mu.Unlock()

	userMap := make(map[int64]OnlineUser)
	for client := range h.clients {
		userMap[client.user.ID] = OnlineUser{
			ID:       client.user.ID,
			Username: client.user.Username,
		}
	}

	users := make([]OnlineUser, 0, len(userMap))
	for _, user := range userMap {
		users = append(users, user)
	}

	sort.Slice(users, func(i, j int) bool {
		return users[i].ID < users[j].ID
	})

	return users
}

func (h *ChatHub) BroadcastOnlineUsers() {
	h.mu.Lock()
	defer h.mu.Unlock()

	for client := range h.clients {
		if len(client.send) >= cap(client.send) {
			log.Println("在线用户通知队列已满，断开连接:", client.user.Username)
			delete(h.clients, client)
			close(client.send)
			_ = client.conn.Close()
		}
	}

	userMap := make(map[int64]OnlineUser)
	for client := range h.clients {
		userMap[client.user.ID] = OnlineUser{
			ID:       client.user.ID,
			Username: client.user.Username,
		}
	}

	users := make([]OnlineUser, 0, len(userMap))
	for _, user := range userMap {
		users = append(users, user)
	}

	sort.Slice(users, func(i, j int) bool {
		return users[i].ID < users[j].ID
	})

	data, err := json.Marshal(ChatOutput{
		Type:  "online_users",
		Users: users,
		Count: len(users),
	})
	if err != nil {
		log.Println("在线用户列表序列化失败:", err)
		return
	}

	for client := range h.clients {
		select {
		case client.send <- data:
		default:
			log.Println("发送在线用户列表失败:", client.user.Username)
		}
	}
}

func (h *ChatHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user, err := h.getUser(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("WebSocket升级失败:", err)
		return
	}

	client := &ChatClient{
		hub:  h,
		conn: conn,
		send: make(chan []byte, sendBufferSize),
		user: user,
	}

	h.AddClient(client)

	go client.writePump()
	go client.readPump()

	log.Printf("用户 %s 加入聊天室，当前 WebSocket 连接数 %d", user.Username, h.ClientCount())
	h.BroadcastOnlineUsers()
}

func (h *ChatHub) ClientCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

func (h *ChatHub) getUser(r *http.Request) (*database.User, error) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return nil, err
	}

	return h.store.FindSessionUser(
		r.Context(),
		hashToken(cookie.Value),
		time.Now().Unix(),
	)
}

func (c *ChatClient) readPump() {
	defer func() {
		c.hub.RemoveClient(c)
		_ = c.conn.Close()

		log.Printf("用户 %s 离开聊天室，当前 WebSocket 连接数 %d", c.user.Username, c.hub.ClientCount())
		c.hub.BroadcastOnlineUsers()
	}()

	c.conn.SetReadLimit(maxMessageSize)
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))

	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	for {
		messageType, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}

		if messageType != websocket.TextMessage {
			continue
		}

		var input ChatInput
		if err := json.Unmarshal(data, &input); err != nil {
			continue
		}

		switch input.Type {
		case "message":
			c.handlePublicMessage(input.Content)
		case "private_message":
			c.handlePrivateMessage(input.ReceiverID, input.Content)
		case "file_message":
			c.handlePublicFileMessage(input.FileID, input.RequestID)
		case "private_file_message":
			c.handlePrivateFileMessage(input.ReceiverID, input.FileID, input.RequestID)
		}
	}
}

func (c *ChatClient) handlePublicMessage(content string) {
	content = strings.TrimSpace(content)
	if content == "" {
		return
	}

	runes := []rune(content)
	if len(runes) > 2000 {
		content = string(runes[:2000])
	}

	message, err := c.hub.store.CreateMessage(context.Background(), c.user.ID, content)
	if err != nil {
		log.Println("保存公共消息失败:", err)
		c.hub.SendError(c, "公共消息发送失败")
		return
	}

	c.hub.Broadcast(message)
}

func (c *ChatClient) handlePrivateMessage(receiverID int64, content string) {
	content = strings.TrimSpace(content)

	if receiverID <= 0 {
		c.hub.SendError(c, "接收者无效")
		return
	}
	if receiverID == c.user.ID {
		c.hub.SendError(c, "不能向自己发送私聊消息")
		return
	}
	if content == "" {
		return
	}

	runes := []rune(content)
	if len(runes) > 2000 {
		content = string(runes[:2000])
	}

	message, err := c.hub.store.CreatePrivateMessage(context.Background(), c.user.ID, receiverID, content)
	if err != nil {
		log.Println("保存私聊消息失败:", err)
		c.hub.SendError(c, "私聊消息发送失败")
		return
	}

	c.hub.BroadcastPrivateMessage(message)
}

// handlePublicFileMessage 将已上传的文件作为公共聊天消息发送，并返回 ACK。
func (c *ChatClient) handlePublicFileMessage(fileID int64, requestID string) {
	if fileID <= 0 {
		c.hub.SendFileMessageAck(c, FileMessageAck{
			RequestID: requestID,
			Success:   false,
			Error:     "文件 ID 无效",
		})
		return
	}

	message, err := c.hub.store.CreatePublicFileMessage(context.Background(), c.user.ID, fileID)
	if err != nil {
		log.Println("发送公共文件消息失败:", err)
		c.hub.SendFileMessageAck(c, FileMessageAck{
			RequestID: requestID,
			Success:   false,
			Error:     "文件消息发送失败，请确认文件存在且属于当前用户",
		})
		return
	}

	c.hub.Broadcast(message)

	c.hub.SendFileMessageAck(c, FileMessageAck{
		RequestID: requestID,
		Success:   true,
		MessageID: message.ID,
	})
}

// handlePrivateFileMessage 将已上传的文件作为私聊消息发送，并返回 ACK。
func (c *ChatClient) handlePrivateFileMessage(receiverID, fileID int64, requestID string) {
	if receiverID <= 0 || receiverID == c.user.ID {
		c.hub.SendFileMessageAck(c, FileMessageAck{
			RequestID: requestID,
			Success:   false,
			Error:     "私聊接收者无效",
		})
		return
	}

	if fileID <= 0 {
		c.hub.SendFileMessageAck(c, FileMessageAck{
			RequestID: requestID,
			Success:   false,
			Error:     "文件 ID 无效",
		})
		return
	}

	message, err := c.hub.store.CreatePrivateFileMessage(context.Background(), c.user.ID, receiverID, fileID)
	if err != nil {
		log.Println("发送私聊文件消息失败:", err)
		c.hub.SendFileMessageAck(c, FileMessageAck{
			RequestID: requestID,
			Success:   false,
			Error:     "私聊文件发送失败，请确认接收者和文件有效",
		})
		return
	}

	c.hub.BroadcastPrivateMessage(message)

	c.hub.SendFileMessageAck(c, FileMessageAck{
		RequestID: requestID,
		Success:   true,
		MessageID: message.ID,
	})
}

func (c *ChatClient) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		_ = c.conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, message); err != nil {
				return
			}

		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

func parseBeforeID(r *http.Request) int64 {
	value := r.URL.Query().Get("before_id")
	if value == "" {
		return 0
	}

	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0
	}

	return id
}
