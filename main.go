package main

import (
	"log"
	"net/http"
)

func main() {
	// 注册首页路由
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}

		http.ServeFile(w, r, "web/index.html")
	})

	// 健康检查接口
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("QChat is running"))
	})

	// 监听所有 IPv4 网络接口的 8080 端口
	addr := ":8080"

	log.Println("================================")
	log.Println(" QChat Server Starting...")
	log.Println(" Local: http://127.0.0.1:8080")
	log.Println(" Port : 8080")
	log.Println("================================")

	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal("Server startup failed: ", err)
	}
}
