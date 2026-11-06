package server

import (
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

// GET /api/messages/search?q=关键词&page=1
func (s *Server) handleMessageSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, APIResponse{
			Success: false,
			Message: "仅支持 GET 请求",
		})
		return
	}

	user, err := s.currentUser(r)
	if err != nil {
		writeUnauthorized(w)
		return
	}

	keyword := strings.TrimSpace(r.URL.Query().Get("q"))
	if keyword == "" {
		writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Message: "请输入搜索关键词",
		})
		return
	}
	if utf8.RuneCountInString(keyword) > 100 {
		writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Message: "搜索关键词不能超过100个字符",
		})
		return
	}

	page := 1
	if value := r.URL.Query().Get("page"); value != "" {
		page, err = strconv.Atoi(value)
		if err != nil || page < 1 || page > 1000000 {
			writeJSON(w, http.StatusBadRequest, APIResponse{
				Success: false,
				Message: "页码无效",
			})
			return
		}
	}

	const pageSize = 30
	offset := (page - 1) * pageSize

	results, total, err := s.store.SearchMessages(
		r.Context(),
		user.ID,
		keyword,
		pageSize,
		offset,
	)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Message: "搜索聊天记录失败",
		})
		return
	}

	totalPages := (total + pageSize - 1) / pageSize

	writeJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Message: "搜索成功",
		Data: map[string]any{
			"keyword":     keyword,
			"results":     results,
			"total":       total,
			"page":        page,
			"page_size":   pageSize,
			"total_pages": totalPages,
		},
	})
}
