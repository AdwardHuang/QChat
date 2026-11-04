//go:build windows

package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"time"

	"github.com/getlantern/systray"
)

//go:embed assets/QChat_128x128.ico
var qchatIcon []byte

func runServer(addr string, handler http.Handler) error {
	// 先监听端口，确保端口占用等错误能在托盘启动前发现
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("监听端口失败: %w", err)
	}

	httpServer := &http.Server{
		Handler: handler,
	}

	serveErr := make(chan error, 1)

	// HTTP 服务在后台运行
	go func() {
		err := httpServer.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			systray.Quit()
		}
	}()

	// 启动系统托盘
	systray.Run(func() {
		systray.SetIcon(qchatIcon)
		systray.SetTitle("QChat")
		systray.SetTooltip("QChat - 运行中")

		openItem := systray.AddMenuItem("打开 QChat", "打开 QChat 管理页面")
		systray.AddSeparator()
		exitItem := systray.AddMenuItem("退出 QChat", "关闭 QChat 服务")

		go func() {
			for {
				select {
				case <-openItem.ClickedCh:
					target := getLocalURL(addr)
					if err := exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", target).Start(); err != nil {
						log.Println("打开浏览器失败:", err)
					}
				case <-exitItem.ClickedCh:
					log.Println("正在退出 QChat...")
					systray.Quit()
					return
				}
			}
		}()
	}, func() {
		// 托盘退出回调
	})

	// 托盘退出后，优雅关闭 HTTP 服务
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(ctx); err != nil {
		log.Println("HTTP 服务优雅关闭失败，执行强制关闭:", err)
		if closeErr := httpServer.Close(); closeErr != nil {
			log.Println("HTTP 服务关闭失败:", closeErr)
		}
	}

	select {
	case err := <-serveErr:
		return fmt.Errorf("HTTP 服务异常退出: %w", err)
	default:
	}

	log.Println("QChat 已正常退出")
	return nil
}

// 根据监听地址生成本机浏览器访问地址
func getLocalURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://127.0.0.1" + addr
	}

	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}

	return (&url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(host, port),
	}).String()
}
