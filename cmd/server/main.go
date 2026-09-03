// Command server 启动 Jina -> Qwen rerank 转换网关服务。
//
// 用法：
//
//	SERVER_ADDR=:8080 \
//	UPSTREAM_TIMEOUT=60s \
//	MAX_CONCURRENCY=200 \
//	go run ./cmd/server
package main

import (
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/njdz/api_embedding_rerank/internal/rerank"
)

func main() {
	addr := getEnv("SERVER_ADDR", ":8080")
	timeout := time.Duration(getEnvInt("UPSTREAM_TIMEOUT_MS", 60000)) * time.Millisecond
	maxConc := getEnvInt("MAX_CONCURRENCY", 200)

	cfg := rerank.Config{
		Timeout:        timeout,
		MaxConcurrency: maxConc,
	}
	svc := rerank.New(cfg)

	// 可选的访问日志中间件（按需去掉，生产建议配合 LB 日志）。
	handler := withAccessLog(svc.Handler())

	log.Printf("rerank gateway listening on %s (timeout=%s, max_concurrency=%d)", addr, timeout, maxConc)

	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      timeout + 10*time.Second,
		IdleTimeout:       120 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
}

// withAccessLog 打印简洁访问日志，记录状态码与耗时，便于定位慢请求。
func withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		log.Printf("%s %s -> %d %s", r.Method, r.URL.Path, sw.status, time.Since(start))
	})
}

// statusWriter 捕获响应状态码用于日志。
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// getEnv 读取环境变量，缺省返回默认值。
func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// getEnvInt 读取整型环境变量。
func getEnvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
