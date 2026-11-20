// Package gameserver 提供游戏服的内部 HTTP 接口。
package gameserver

import (
	"net/http"

	"github.com/NumberMan1/slg-server/infrastructure/httpserver"
)

// NewHandler 创建游戏服的路由，供网关探测游戏服是否可达。
func NewHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", httpserver.Health("game-server"))
	return mux
}
