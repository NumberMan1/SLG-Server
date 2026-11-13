package testkit

import (
	"context"
	"net/http"
	"testing"
	"time"

	orbithttp "github.com/NumberMan1/orbit/socket/http"

	"slg/server/internal/gameserver"
	"slg/server/internal/gateway"
)

type HTTPStack struct {
	GatewayURL string
	StopGame   func()
}

func StartHTTPStack(t testing.TB) HTTPStack {
	t.Helper()
	gameURL, stopGame := startHTTP(t, gameserver.NewHandler())

	client, err := gateway.NewGameClient()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)
	handler, err := gateway.NewHandler(gameURL, client)
	if err != nil {
		t.Fatal(err)
	}
	gatewayURL, _ := startHTTP(t, handler)

	return HTTPStack{GatewayURL: gatewayURL, StopGame: stopGame}
}

func startHTTP(t testing.TB, handler http.Handler) (string, func()) {
	t.Helper()
	server, err := orbithttp.NewServer("127.0.0.1:0", handler, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	stop := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Stop(ctx); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(stop)
	return "http://" + server.Addr().String(), stop
}
