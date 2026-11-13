package gameserver

import (
	"net/http"

	"slg/server/infrastructure/httpserver"
)

func NewHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", httpserver.Health("game-server"))
	return mux
}
