package main

import (
	"context"
	"log"

	"qchat/internal/auth"
	"qchat/internal/config"
	"qchat/internal/database"
	"qchat/internal/server"
)

func main() {
	// 1. 加载配置
	cfg, err := config.Load("config.json")
	if err != nil {
		log.Fatal("加载配置失败: ", err)
	}

	// 2. 打开数据库
	store, err := database.Open(cfg.DatabasePath)
	if err != nil {
		log.Fatal("打开数据库失败: ", err)
	}
	defer store.Close()

	// 3. 初始化管理员账号
	passwordHash, err := auth.HashPassword(cfg.AdminPassword)
	if err != nil {
		log.Fatal("管理员密码加密失败: ", err)
	}

	if err := store.CreateAdminIfEmpty(context.Background(), cfg.AdminUsername, passwordHash); err != nil {
		log.Fatal("初始化管理员失败: ", err)
	}

	// 4. 创建 HTTP 服务
	srv := server.New(store, cfg.SessionHours)

	// 5. 接入文件上传和下载接口
	maxUploadSize := cfg.MaxUploadSizeBytes()
	handler := srv.WithFileUploads(srv.Handler(), "uploads", maxUploadSize)

	log.Println("================================")
	log.Println(" QChat Server Starting...")
	log.Println(" Local: http://127.0.0.1" + cfg.ListenAddr)
	log.Println(" Port :", cfg.ListenAddr)
	log.Println("================================")
	log.Println("管理员账号:", cfg.AdminUsername)
	log.Println("文件上传目录: uploads")
	log.Printf("单文件上传限制: %d MB", cfg.MaxUploadSizeMB)

	// 6. 启动服务
	if err := runServer(cfg.ListenAddr, handler); err != nil {
		log.Println("QChat 服务已停止:", err)
	}
}
