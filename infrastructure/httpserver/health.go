// Package httpserver 提供 HTTP 进程的生命周期装配和通用健康响应。
package httpserver

import (
	"context"
	"net/http"
	"time"

	orbithttp "github.com/NumberMan1/orbit/socket/http"
)

// HealthResponse 包含响应服务的名称和健康状态。
type HealthResponse struct {
	Service string `json:"service"`
	Status  string `json:"status"`
}

// WithReadiness 在 /readyz 检查业务依赖，全部可用时返回 204，探活路由继续由 base 处理。
func WithReadiness(base http.Handler, checks ...func(context.Context) error) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", base)
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		for _, check := range checks {
			if err := check(ctx); err != nil {
				http.Error(w, "service unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

// Health 返回当前服务的存活检查处理器，调用方可用服务名核对响应来源。
func Health(service string) http.HandlerFunc {
	response := HealthResponse{Service: service, Status: "ok"}
	return func(w http.ResponseWriter, _ *http.Request) {
		_ = orbithttp.WriteJSON(w, http.StatusOK, response)
	}
}
