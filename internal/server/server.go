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
	"unicode/utf8"

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

type CreateUserRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type ResetPasswordRequest struct {
	Password string `json:"password"`
}

type SetUserStatusRequest struct {
	IsActive *bool `json:"is_active"`
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

	// 聊天记录搜索
	s.mux.HandleFunc("/api/messages/search", s.handleMessageSearch)

	// 私聊历史和未读消息
	s.mux.HandleFunc("/api/private/messages", s.handlePrivateMessages)
	s.mux.HandleFunc("/api/private/unread", s.handlePrivateUnread)

	// 管理员用户管理
	s.mux.HandleFunc("/api/admin/users", s.handleAdminUsers)
	s.mux.HandleFunc("/api/admin/users/", s.handleAdminUserAction)

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

	if !user.IsActive {
		writeJSON(w, http.StatusForbidden, APIResponse{
			Success: false,
			Message: "该账号已被管理员禁用",
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
			"is_admin": user.IsAdmin,
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
			"is_admin": user.IsAdmin,
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

func (s *Server) currentAdmin(r *http.Request) (*database.User, error) {
	user, err := s.currentUser(r)
	if err != nil {
		return nil, err
	}

	if !user.IsAdmin {
		return nil, errors.New("需要管理员权限")
	}

	return user, nil
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

func writeUnauthorized(w http.ResponseWriter) {
	writeJSON(w, http.StatusUnauthorized, APIResponse{
		Success: false,
		Message: "尚未登录或登录已过期",
	})
}

func writeForbidden(w http.ResponseWriter) {
	writeJSON(w, http.StatusForbidden, APIResponse{
		Success: false,
		Message: "没有管理员权限",
	})
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
		writeUnauthorized(w)
		return
	}

	messages, err := s.store.ListMessages(r.Context(), parseBeforeID(r), 50)
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

// GET /api/private/messages?peer_id=2&before_id=100
func (s *Server) handlePrivateMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	user, err := s.currentUser(r)
	if err != nil {
		writeUnauthorized(w)
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

	if err := s.store.MarkPrivateMessagesRead(r.Context(), user.ID, peerID); err != nil {
		writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Message: "更新私聊已读状态失败",
		})
		return
	}

	messages, err := s.store.ListPrivateMessages(r.Context(), user.ID, peerID, parseBeforeID(r), 50)
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

// GET /api/private/unread?peer_id=2
func (s *Server) handlePrivateUnread(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	user, err := s.currentUser(r)
	if err != nil {
		writeUnauthorized(w)
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

	count, err := s.store.CountUnreadPrivateMessages(r.Context(), user.ID, peerID)
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

// GET /api/admin/users
// POST /api/admin/users
func (s *Server) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	admin, err := s.currentAdmin(r)
	if err != nil {
		if _, authErr := s.currentUser(r); authErr != nil {
			writeUnauthorized(w)
		} else {
			writeForbidden(w)
		}
		return
	}

	switch r.Method {
	case http.MethodGet:
		users, err := s.store.ListUsers(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, APIResponse{
				Success: false,
				Message: "查询用户列表失败",
			})
			return
		}

		writeJSON(w, http.StatusOK, APIResponse{
			Success: true,
			Message: "查询成功",
			Data: map[string]any{
				"users": users,
			},
		})

	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 8192)

		var req CreateUserRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, APIResponse{
				Success: false,
				Message: "请求数据格式错误",
			})
			return
		}

		req.Username = strings.TrimSpace(req.Username)

		if utf8.RuneCountInString(req.Username) < 2 ||
			utf8.RuneCountInString(req.Username) > 32 {
			writeJSON(w, http.StatusBadRequest, APIResponse{
				Success: false,
				Message: "用户名长度必须为 2 到 32 个字符",
			})
			return
		}

		if len(req.Password) < 8 || len(req.Password) > 72 {
			writeJSON(w, http.StatusBadRequest, APIResponse{
				Success: false,
				Message: "密码长度必须为 8 到 72 个字节",
			})
			return
		}

		passwordHash, err := auth.HashPassword(req.Password)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, APIResponse{
				Success: false,
				Message: "密码加密失败",
			})
			return
		}

		user, err := s.store.CreateUser(r.Context(), req.Username, passwordHash)
		if err != nil {
			if errors.Is(err, database.ErrUsernameExists) {
				writeJSON(w, http.StatusConflict, APIResponse{
					Success: false,
					Message: "用户名已经存在",
				})
				return
			}

			writeJSON(w, http.StatusInternalServerError, APIResponse{
				Success: false,
				Message: "创建用户失败",
			})
			return
		}

		_ = admin

		writeJSON(w, http.StatusCreated, APIResponse{
			Success: true,
			Message: "用户创建成功",
			Data:    user,
		})

	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// POST /api/admin/users/{id}/password
// POST /api/admin/users/{id}/status
func (s *Server) handleAdminUserAction(w http.ResponseWriter, r *http.Request) {
	if _, err := s.currentAdmin(r); err != nil {
		if _, authErr := s.currentUser(r); authErr != nil {
			writeUnauthorized(w)
		} else {
			writeForbidden(w)
		}
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/admin/users/")
	parts := strings.Split(strings.Trim(path, "/"), "/")

	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}

	userID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || userID <= 0 {
		writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Message: "用户 ID 无效",
		})
		return
	}

	switch parts[1] {
	case "password":
		s.handleAdminResetPassword(w, r, userID)

	case "status":
		s.handleAdminSetUserStatus(w, r, userID)

	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleAdminResetPassword(w http.ResponseWriter, r *http.Request, userID int64) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 4096)

	var req ResetPasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Message: "请求数据格式错误",
		})
		return
	}

	if len(req.Password) < 8 || len(req.Password) > 72 {
		writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Message: "密码长度必须为 8 到 72 个字节",
		})
		return
	}

	passwordHash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Message: "密码加密失败",
		})
		return
	}

	if err := s.store.UpdateUserPassword(r.Context(), userID, passwordHash); err != nil {
		if errors.Is(err, database.ErrUserNotFound) {
			writeJSON(w, http.StatusNotFound, APIResponse{
				Success: false,
				Message: "用户不存在",
			})
			return
		}

		writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Message: "重置密码失败",
		})
		return
	}

	// 密码重置后，断开该用户已经建立的 WebSocket 连接。
	s.chatHub.DisconnectUser(userID)

	writeJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Message: "密码重置成功，该用户需要重新登录",
	})
}

func (s *Server) handleAdminSetUserStatus(w http.ResponseWriter, r *http.Request, userID int64) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	admin, err := s.currentAdmin(r)
	if err != nil {
		writeUnauthorized(w)
		return
	}

	if userID == admin.ID {
		var req SetUserStatusRequest
		r.Body = http.MaxBytesReader(w, r.Body, 4096)

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.IsActive == nil {
			writeJSON(w, http.StatusBadRequest, APIResponse{
				Success: false,
				Message: "请提供 is_active 状态",
			})
			return
		}

		if !*req.IsActive {
			writeJSON(w, http.StatusBadRequest, APIResponse{
				Success: false,
				Message: "不能禁用当前登录的管理员账号",
			})
			return
		}

		writeJSON(w, http.StatusOK, APIResponse{
			Success: true,
			Message: "账号状态未发生变化",
		})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 4096)

	var req SetUserStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.IsActive == nil {
		writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Message: "请提供 is_active 状态",
		})
		return
	}

	if err := s.store.SetUserActive(r.Context(), userID, *req.IsActive); err != nil {
		switch {
		case errors.Is(err, database.ErrUserNotFound):
			writeJSON(w, http.StatusNotFound, APIResponse{
				Success: false,
				Message: "用户不存在",
			})

		case errors.Is(err, database.ErrLastAdmin):
			writeJSON(w, http.StatusBadRequest, APIResponse{
				Success: false,
				Message: "不能禁用最后一个管理员",
			})

		default:
			writeJSON(w, http.StatusInternalServerError, APIResponse{
				Success: false,
				Message: "修改用户状态失败",
			})
		}
		return
	}

	if !*req.IsActive {
		s.chatHub.DisconnectUser(userID)
	}

	message := "用户已禁用"
	if *req.IsActive {
		message = "用户已启用"
	}

	writeJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Message: message,
	})
}
