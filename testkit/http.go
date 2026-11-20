// Package testkit 提供联通用例使用的本地服务装配。
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

// HTTPStack 提供网关地址和可单独关闭游戏服的回调。
type HTTPStack struct {
	GatewayURL string
	StopGame   func()
}

// StartHTTPStack 在不同端口上启动游戏服和网关，由测试清理流程关闭它们。
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

// StartHTTP 在回环地址上分配可用端口，返回访问地址和可提前调用的停止函数。
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
