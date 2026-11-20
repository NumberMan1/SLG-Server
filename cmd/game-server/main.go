package main

import (
	"fmt"
	"os"

	serverlog "github.com/NumberMan1/log"
	"github.com/NumberMan1/numbox/utils/env"
	orbithttp "github.com/NumberMan1/orbit/socket/http"

	"github.com/NumberMan1/slg-server/infrastructure/httpserver"
	"github.com/NumberMan1/slg-server/infrastructure/logging"
	"github.com/NumberMan1/slg-server/infrastructure/servicediscovery"
	"github.com/NumberMan1/slg-server/internal/gameserver"
)

func main() {
	os.Exit(logging.Run("game-server", run))
}

func run(logger serverlog.Logger) error {
	addr := env.GetEnv("SLG_GAME_ADDR", "127.0.0.1:8081").String()
	advertise := env.GetEnv("SLG_GAME_ADVERTISE_URL", env.EnvValue("http://"+addr)).String()
	client, registry, err := servicediscovery.Open()
	if err != nil {
		return err
	}
	defer client.Close()
	registrar, err := servicediscovery.NewHTTPRegistrar(registry, servicediscovery.GameServer, os.Getenv("SLG_GAME_INSTANCE_ID"), advertise)
	if err != nil {
		return fmt.Errorf("register game service: %w", err)
	}
	server, err := orbithttp.NewServer(addr, gameserver.NewHandler(), nil)
	if err != nil {
		return err
	}
	return httpserver.Run(httpserver.Logged(logger, server), registrar)
}
