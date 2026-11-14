package main

import (
	"os"

	serverlog "github.com/NumberMan1/log"
	"github.com/NumberMan1/numbox/utils/env"
	orbithttp "github.com/NumberMan1/orbit/socket/http"

	"github.com/NumberMan1/slg-server/infrastructure/httpserver"
	"github.com/NumberMan1/slg-server/infrastructure/logging"
	"github.com/NumberMan1/slg-server/internal/gameserver"
)

func main() {
	os.Exit(logging.Run("game-server", run))
}

func run(logger serverlog.Logger) error {
	addr := env.GetEnv("SLG_GAME_ADDR", "127.0.0.1:8081").String()
	server, err := orbithttp.NewServer(addr, gameserver.NewHandler(), nil)
	if err != nil {
		return err
	}
	return httpserver.Run(logger, server)
}
