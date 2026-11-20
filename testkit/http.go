package testkit

import (
	"context"
	"net/http"
	"testing"
	"time"

	orbithttp "github.com/NumberMan1/orbit/socket/http"

	"github.com/NumberMan1/slg-server/internal/gameserver"
	"github.com/NumberMan1/slg-server/internal/gateway"
)

type HTTPStack struct {
	GatewayURL string
	StopGame   func()
}

func StartHTTPStack(t testing.TB) HTTPStack {
	t.Helper()
	gameURL, stopGame := StartHTTP(t, gameserver.NewHandler())

	client, err := gateway.NewUpstreamClient()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)
	handler := gateway.NewHandler(func() []string { return []string{gameURL} }, client)
	gatewayURL, _ := StartHTTP(t, handler)

	return HTTPStack{GatewayURL: gatewayURL, StopGame: stopGame}
}

func StartHTTP(t testing.TB, handler http.Handler) (string, func()) {
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
