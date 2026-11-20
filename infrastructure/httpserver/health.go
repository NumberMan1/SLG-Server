// Package httpserver 提供 HTTP 进程的生命周期装配和通用健康响应。
package httpserver

import (
	"net/http"

	orbithttp "github.com/NumberMan1/orbit/socket/http"
)

// HealthResponse 包含响应服务的名称和健康状态。
type HealthResponse struct {
	Service string `json:"service"`
	Status  string `json:"status"`
}

// Health 返回当前服务的存活检查处理器，调用方可用服务名核对响应来源。
func Health(service string) http.HandlerFunc {
	response := HealthResponse{Service: service, Status: "ok"}
	return func(w http.ResponseWriter, _ *http.Request) {
		_ = orbithttp.WriteJSON(w, http.StatusOK, response)
	}
}
