package main

import (
	"context"
	"log"
	"net/http"

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

	log.Println("================================")
	log.Println(" QChat Server Starting...")
	log.Println(" Local: http://127.0.0.1" + cfg.ListenAddr)
	log.Println(" Port :", cfg.ListenAddr)
	log.Println("================================")
	log.Println("管理员账号:", cfg.AdminUsername)

	// 5. 启动服务
	if err := http.ListenAndServe(cfg.ListenAddr, srv.Handler()); err != nil {
		log.Fatal("服务启动失败: ", err)
	}
}
