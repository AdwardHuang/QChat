package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strconv"
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
	Type    string `json:"type"`
	Content string `json:"content"`
}

type OnlineUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

type ChatOutput struct {
	Type    string            `json:"type"`
	Message *database.Message `json:"message,omitempty"`
	Users   []OnlineUser      `json:"users,omitempty"`
	Count   int               `json:"count,omitempty"`
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

	// 先清理发送队列已经满的客户端。
	for client := range h.clients {
		if len(client.send) >= cap(client.send) {
			log.Println("在线用户通知队列已满，断开连接:", client.user.Username)
			delete(h.clients, client)
			close(client.send)
			_ = client.conn.Close()
		}
	}

	// 同一用户打开多个浏览器时，只统计一次。
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

	log.Printf("用户 %s 加入聊天室，当前 WebSocket 连接数 %d",
		user.Username,
		h.ClientCount(),
	)

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

		log.Printf("用户 %s 离开聊天室，当前 WebSocket 连接数 %d",
			c.user.Username,
			c.hub.ClientCount(),
		)

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

		if input.Type != "message" {
			continue
		}

		content := input.Content

		if len(content) == 0 {
			continue
		}

		if len(content) > 2000 {
			content = content[:2000]
		}

		message, err := c.hub.store.CreateMessage(
			context.Background(),
			c.user.ID,
			content,
		)

		if err != nil {
			log.Println("保存聊天消息失败:", err)
			continue
		}

		c.hub.Broadcast(message)
	}
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
