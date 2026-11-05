package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"

	"qchat/internal/auth"
	"qchat/internal/config"
	"qchat/internal/database"
	"qchat/internal/server"
)

func main() {
	// 1. 获取 EXE 所在目录
	exePath, err := os.Executable()
	if err != nil {
		log.Println("获取程序路径失败:", err)
		return
	}

	exeDir := filepath.Dir(exePath)

	// 2. 初始化启动日志
	logPath := filepath.Join(exeDir, "qchat-startup.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer logFile.Close()

	log.SetOutput(logFile)
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)

	log.Println()
	log.Println("========================================")
	log.Println("QChat Starting...")
	log.Println("EXE 路径:", exePath)
	log.Println("EXE 目录:", exeDir)

	// 3. 记录当前工作目录
	workDir, err := os.Getwd()
	if err != nil {
		log.Println("获取当前工作目录失败:", err)
	} else {
		log.Println("当前工作目录:", workDir)
	}

	// 4. 捕获程序 Panic
	defer func() {
		if r := recover(); r != nil {
			log.Println("程序发生 Panic:", r)
			log.Println(string(debug.Stack()))
		}
		log.Println("QChat main() 结束")
		log.Println("========================================")
	}()

	// 5. 加载配置
	log.Println("[1/6] 正在加载配置文件 config.json")

	cfg, err := config.Load("config.json")
	if err != nil {
		log.Println("加载配置失败:", err)
		return
	}

	log.Println("配置加载成功")
	log.Println("监听地址:", cfg.ListenAddr)
	log.Println("数据库路径:", cfg.DatabasePath)
	log.Println("管理员账号:", cfg.AdminUsername)
	log.Println("单文件上传限制:", cfg.MaxUploadSizeMB, "MB")

	// 6. 打开数据库
	log.Println("[2/6] 正在初始化数据库")

	store, err := database.Open(cfg.DatabasePath)
	if err != nil {
		log.Println("打开数据库失败:", err)
		return
	}
	defer func() {
		log.Println("正在关闭数据库...")
		if err := store.Close(); err != nil {
			log.Println("关闭数据库失败:", err)
		} else {
			log.Println("数据库已关闭")
		}
	}()

	log.Println("数据库初始化成功")

	// 7. 初始化管理员账号
	log.Println("[3/6] 正在初始化管理员账号")

	passwordHash, err := auth.HashPassword(cfg.AdminPassword)
	if err != nil {
		log.Println("管理员密码加密失败:", err)
		return
	}

	if err := store.CreateAdminIfEmpty(context.Background(), cfg.AdminUsername, passwordHash); err != nil {
		log.Println("初始化管理员失败:", err)
		return
	}

	log.Println("管理员账号初始化成功")

	// 8. 创建 HTTP 服务
	log.Println("[4/6] 正在创建 HTTP 服务")

	srv := server.New(store, cfg.SessionHours)

	log.Println("HTTP Handler 创建成功")

	// 9. 接入文件上传和下载接口
	log.Println("[5/6] 正在初始化文件上传功能")

	maxUploadSize := cfg.MaxUploadSizeBytes()
	handler := srv.WithFileUploads(srv.Handler(), "uploads", maxUploadSize)

	log.Println("文件上传功能初始化成功")
	log.Println("文件上传目录: uploads")

	// 10. 启动服务
	log.Println("[6/6] 准备启动 HTTP 服务")
	log.Println("========================================")
	log.Println("QChat Server Starting...")
	log.Println("Local: http://127.0.0.1" + cfg.ListenAddr)
	log.Println("Port:", cfg.ListenAddr)
	log.Println("管理员账号:", cfg.AdminUsername)
	log.Printf("单文件上传限制: %d MB", cfg.MaxUploadSizeMB)
	log.Println("========================================")

	if err := runServer(cfg.ListenAddr, handler); err != nil {
		log.Println("QChat 服务退出:", err)
	} else {
		log.Println("QChat 服务正常退出")
	}
}
