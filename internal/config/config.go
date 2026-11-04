package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const maxUploadSizeMB int64 = ((1<<63 - 1) - (1 << 20)) / (1 << 20)

type Config struct {
	ListenAddr      string `json:"listen_addr"`
	DatabasePath    string `json:"database_path"`
	SessionHours    int    `json:"session_hours"`
	AdminUsername   string `json:"admin_username"`
	AdminPassword   string `json:"admin_password"`
	MaxUploadSizeMB int64  `json:"max_upload_size_mb"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}

	cfg.ListenAddr = strings.TrimSpace(cfg.ListenAddr)
	cfg.DatabasePath = strings.TrimSpace(cfg.DatabasePath)
	cfg.AdminUsername = strings.TrimSpace(cfg.AdminUsername)

	if cfg.ListenAddr == "" {
		return nil, fmt.Errorf("listen_addr 不能为空")
	}
	if cfg.DatabasePath == "" {
		return nil, fmt.Errorf("database_path 不能为空")
	}
	if cfg.SessionHours <= 0 {
		return nil, fmt.Errorf("session_hours 必须大于 0")
	}
	if cfg.AdminUsername == "" {
		return nil, fmt.Errorf("admin_username 不能为空")
	}
	if len(cfg.AdminPassword) < 12 {
		return nil, fmt.Errorf("admin_password 至少需要 12 个字符")
	}
	if cfg.MaxUploadSizeMB <= 0 {
		return nil, fmt.Errorf("max_upload_size_mb 必须大于 0")
	}
	if cfg.MaxUploadSizeMB > maxUploadSizeMB {
		return nil, fmt.Errorf("max_upload_size_mb 设置过大")
	}

	return &cfg, nil
}

func (c *Config) MaxUploadSizeBytes() int64 {
	return c.MaxUploadSizeMB * (1 << 20)
}
