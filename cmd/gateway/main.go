package main

import (
	"log"

	"github.com/NumberMan1/numbox/utils/env"
	orbithttp "github.com/NumberMan1/orbit/socket/http"

	"slg/server/infrastructure/httpserver"
	"slg/server/internal/gateway"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	addr := env.GetEnv("SLG_GATEWAY_ADDR", "127.0.0.1:8080").String()
	gameURL := env.GetEnv("SLG_GAME_URL", "http://127.0.0.1:8081").String()
	client, err := gateway.NewGameClient()
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()

	handler, err := gateway.NewHandler(gameURL, client)
	if err != nil {
		return err
	}
	server, err := orbithttp.NewServer(addr, handler, nil)
	if err != nil {
		return err
	}
	return httpserver.Run("gateway", server)
}
