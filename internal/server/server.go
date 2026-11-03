package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"qchat/web"
	"strconv"
	"strings"
	"time"

	"qchat/internal/auth"
	"qchat/internal/database"
)

const sessionCookieName = "qchat_session"

type Server struct {
	store        *database.Store
	sessionHours int
	mux          *http.ServeMux
	chatHub      *ChatHub
}

type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type APIResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func New(store *database.Store, sessionHours int) *Server {
	s := &Server{
		store:        store,
		sessionHours: sessionHours,
		mux:          http.NewServeMux(),
	}

	s.chatHub = NewChatHub(store)

	s.routes()

	return s
}

func (s *Server) Handler() http.Handler {
	return s.mux
}

func (s *Server) routes() {
	s.mux.HandleFunc("/api/health", s.handleHealth)
	s.mux.HandleFunc("/api/login", s.handleLogin)
	s.mux.HandleFunc("/api/me", s.handleMe)
	s.mux.HandleFunc("/api/logout", s.handleLogout)

	// 公共聊天历史
	s.mux.HandleFunc("/api/messages", s.handleMessages)

	// 私聊历史和未读消息
	s.mux.HandleFunc("/api/private/messages", s.handlePrivateMessages)
	s.mux.HandleFunc("/api/private/unread", s.handlePrivateUnread)

	s.mux.HandleFunc("/ws/chat", s.chatHub.ServeHTTP)

	s.mux.HandleFunc("/", s.handleIndex)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Message: "QChat is running",
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 4096)

	var req LoginRequest

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Message: "请求数据格式错误",
		})
		return
	}

	req.Username = strings.TrimSpace(req.Username)

	if req.Username == "" || req.Password == "" {
		writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Message: "请输入用户名和密码",
		})
		return
	}

	ctx := r.Context()

	user, err := s.store.FindUserByUsername(ctx, req.Username)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusUnauthorized, APIResponse{
				Success: false,
				Message: "用户名或密码错误",
			})
			return
		}

		writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Message: "查询用户失败",
		})
		return
	}

	if !auth.CheckPassword(req.Password, user.PasswordHash) {
		writeJSON(w, http.StatusUnauthorized, APIResponse{
			Success: false,
			Message: "用户名或密码错误",
		})
		return
	}

	tokenBytes := make([]byte, 32)

	if _, err := rand.Read(tokenBytes); err != nil {
		writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Message: "生成登录凭证失败",
		})
		return
	}

	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	tokenHash := hashToken(token)
	expiresAt := time.Now().Add(time.Duration(s.sessionHours) * time.Hour)

	if err := s.store.CreateSession(ctx, tokenHash, user.ID, expiresAt.Unix()); err != nil {
		writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Message: "创建登录会话失败",
		})
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  expiresAt,
		MaxAge:   int(time.Until(expiresAt).Seconds()),
	})

	writeJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Message: "登录成功",
		Data: map[string]any{
			"id":       user.ID,
			"username": user.Username,
		},
	})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	user, err := s.currentUser(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, APIResponse{
			Success: false,
			Message: "尚未登录或登录已过期",
		})
		return
	}

	writeJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Message: "已登录",
		Data: map[string]any{
			"id":       user.ID,
			"username": user.Username,
		},
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	cookie, err := r.Cookie(sessionCookieName)

	if err == nil {
		_ = s.store.DeleteSession(context.Background(), hashToken(cookie.Value))
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})

	writeJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Message: "已退出登录",
	})
}

func (s *Server) currentUser(r *http.Request) (*database.User, error) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return nil, err
	}

	return s.store.FindSessionUser(
		r.Context(),
		hashToken(cookie.Value),
		time.Now().Unix(),
	)
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func writeJSON(w http.ResponseWriter, status int, data APIResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	data, err := web.FS.ReadFile("index.html")
	if err != nil {
		http.Error(w, "页面读取失败", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	if _, err := s.currentUser(r); err != nil {
		writeJSON(w, http.StatusUnauthorized, APIResponse{
			Success: false,
			Message: "尚未登录或登录已过期",
		})
		return
	}

	beforeID := parseBeforeID(r)

	messages, err := s.store.ListMessages(
		r.Context(),
		beforeID,
		50,
	)

	if err != nil {
		writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Message: "读取历史消息失败",
		})
		return
	}

	writeJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Message: "查询成功",
		Data: map[string]any{
			"messages": messages,
		},
	})
}

// 查询当前登录用户与指定用户之间的私聊历史。
// GET /api/private/messages?peer_id=2&before_id=100
func (s *Server) handlePrivateMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	user, err := s.currentUser(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, APIResponse{
			Success: false,
			Message: "尚未登录或登录已过期",
		})
		return
	}

	peerID, err := strconv.ParseInt(r.URL.Query().Get("peer_id"), 10, 64)

	if err != nil || peerID <= 0 || peerID == user.ID {
		writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Message: "私聊对象无效",
		})
		return
	}

	// 先将对方发给自己的未读消息标记为已读。
	if err := s.store.MarkPrivateMessagesRead(
		r.Context(),
		user.ID,
		peerID,
	); err != nil {
		writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Message: "更新私聊已读状态失败",
		})
		return
	}

	messages, err := s.store.ListPrivateMessages(
		r.Context(),
		user.ID,
		peerID,
		parseBeforeID(r),
		50,
	)

	if err != nil {
		writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Message: "读取私聊历史失败",
		})
		return
	}

	writeJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Message: "查询成功",
		Data: map[string]any{
			"peer_id":  peerID,
			"messages": messages,
		},
	})
}

// 查询指定用户发给当前用户的未读私聊数量。
// GET /api/private/unread?peer_id=2
func (s *Server) handlePrivateUnread(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	user, err := s.currentUser(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, APIResponse{
			Success: false,
			Message: "尚未登录或登录已过期",
		})
		return
	}

	peerID, err := strconv.ParseInt(r.URL.Query().Get("peer_id"), 10, 64)

	if err != nil || peerID <= 0 || peerID == user.ID {
		writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Message: "私聊对象无效",
		})
		return
	}

	count, err := s.store.CountUnreadPrivateMessages(
		r.Context(),
		user.ID,
		peerID,
	)

	if err != nil {
		writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Message: "查询未读消息失败",
		})
		return
	}

	writeJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Message: "查询成功",
		Data: map[string]any{
			"peer_id": peerID,
			"count":   count,
		},
	})
}
