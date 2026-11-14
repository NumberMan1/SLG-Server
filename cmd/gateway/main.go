package main

import (
	"os"

	serverlog "github.com/NumberMan1/log"
	"github.com/NumberMan1/log/field"
	"github.com/NumberMan1/numbox/utils/env"
	orbithttp "github.com/NumberMan1/orbit/socket/http"
	orbittcp "github.com/NumberMan1/orbit/socket/tcp"

	"github.com/NumberMan1/slg-server/infrastructure/httpserver"
	"github.com/NumberMan1/slg-server/infrastructure/logging"
	"github.com/NumberMan1/slg-server/internal/gateway"
)

func main() {
	os.Exit(logging.Run("gateway", run))
}

func run(logger serverlog.Logger) error {
	addr := env.GetEnv("SLG_GATEWAY_ADDR", "127.0.0.1:8080").String()
	tcpAddr := env.GetEnv("SLG_GATEWAY_TCP_ADDR", "127.0.0.1:19090").String()
	gameURL := env.GetEnv("SLG_GAME_URL", "http://127.0.0.1:8081").String()
	account := env.GetEnv("SLG_DEMO_ACCOUNT", "demo").String()
	password := env.GetEnv("SLG_DEMO_PASSWORD").String()
	client, err := gateway.NewGameClient()
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()

	handler, err := gateway.NewHandler(gameURL, client)
	if err != nil {
		return err
	}
	sessions := gateway.NewSessions(account, password)
	options, err := gateway.SessionOptions()
	if err != nil {
		return err
	}
	options.Logger = logger.With(field.String("transport", "tcp"))
	tcpServer, err := orbittcp.NewServer(tcpAddr, options, sessions.Connect, nil)
	if err != nil {
		return err
	}
	mux := gateway.WithLogin(handler, sessions, tcpServer)
	server, err := orbithttp.NewServer(addr, mux, nil)
	if err != nil {
		return err
	}
	return httpserver.Run(logger, server, tcpServer)
}
