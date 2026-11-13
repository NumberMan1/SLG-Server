package main

import (
	"log"

	"github.com/NumberMan1/numbox/utils/env"
	orbithttp "github.com/NumberMan1/orbit/socket/http"

	"slg/server/infrastructure/httpserver"
	"slg/server/internal/gameserver"
)

func main() {
	addr := env.GetEnv("SLG_GAME_ADDR", "127.0.0.1:8081").String()
	server, err := orbithttp.NewServer(addr, gameserver.NewHandler(), nil)
	if err != nil {
		log.Fatal(err)
	}
	if err := httpserver.Run("game-server", server); err != nil {
		log.Fatal(err)
	}
}
