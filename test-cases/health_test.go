package testcases

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"mime"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	serverlog "github.com/NumberMan1/log"
	"github.com/NumberMan1/orbit/discovery"

	"github.com/NumberMan1/slg-server/infrastructure/httpserver"
	"github.com/NumberMan1/slg-server/infrastructure/servicediscovery"
	"github.com/NumberMan1/slg-server/internal/gateway"
	"github.com/NumberMan1/slg-server/internal/loginserver"
	"github.com/NumberMan1/slg-server/testkit"
)

func TestGatewayReadinessChecksLoginDependencies(t *testing.T) {
	var available atomic.Bool
	login := httpserver.WithReadiness(http.NotFoundHandler(), func(context.Context) error {
		if !available.Load() {
			return errors.New("ticket store unavailable")
		}
		return nil
	})
	loginURL, _ := testkit.StartHTTP(t, login)
	client, err := gateway.NewUpstreamClient()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)
	gatewayURL, _ := testkit.StartHTTP(t, httpserver.WithReadiness(http.NotFoundHandler(), func(ctx context.Context) error {
		return gateway.CheckLoginReady(ctx, []string{loginURL}, client)
	}))
	for _, want := range []int{http.StatusServiceUnavailable, http.StatusNoContent} {
		available.Store(want == http.StatusNoContent)
		response, err := client.Get(gatewayURL + "/readyz")
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != want {
			t.Fatalf("readiness returned %d, want %d", response.StatusCode, want)
		}
	}
}

func TestGatewayHealth(t *testing.T) {
	stack := testkit.StartHTTPStack(t)
	client := &http.Client{Timeout: 3 * time.Second}

	for _, tc := range []struct {
		path    string
		service string
	}{
		{path: "/healthz", service: "gateway"},
		{path: "/v1/game/health", service: "game-server"},
	} {
		t.Run(tc.service, func(t *testing.T) {
			res, err := client.Get(stack.GatewayURL + tc.path)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()

			mediaType, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type"))
			if res.StatusCode != http.StatusOK || mediaType != "application/json" {
				t.Fatalf("unexpected response: %s (%s)", res.Status, res.Header.Get("Content-Type"))
			}
			var health struct {
				Service string
				Status  string
			}
			if err := json.NewDecoder(res.Body).Decode(&health); err != nil {
				t.Fatal(err)
			}
			if health.Service != tc.service || health.Status != "ok" {
				t.Fatalf("unexpected health response: %+v", health)
			}
		})
	}
}

func TestGatewayHealthWhenGameIsDown(t *testing.T) {
	stack := testkit.StartHTTPStack(t)
	stack.StopGame()

	client := &http.Client{Timeout: 3 * time.Second}
	res, err := client.Get(stack.GatewayURL + "/v1/game/health")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502 Bad Gateway, got %s", res.Status)
	}
}

type testTCPAddress struct{ *net.TCPAddr }

func (a testTCPAddress) Addr() net.Addr { return a.TCPAddr }

func TestGatewayDiscoversServicesWithEtcd(t *testing.T) {
	endpoints := os.Getenv("SLG_TEST_ETCD_ENDPOINTS")
	if endpoints == "" {
		t.Skip("set SLG_TEST_ETCD_ENDPOINTS for middleware integration")
	}
	t.Setenv("SLG_ETCD_ENDPOINTS", endpoints)
	t.Setenv("SLG_ETCD_PREFIX", "/slg/test/"+rand.Text())
	etcd, registry, err := servicediscovery.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = etcd.Close() })
	store, err := registry.NewStore(discovery.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	stop := func(component interface{ Stop(context.Context) error }) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := component.Stop(ctx); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { stop(store) })
	client, err := gateway.NewUpstreamClient()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)
	gameHandler := gateway.NewHandler(gateway.DiscoveredServices(store, servicediscovery.GameServer), client)
	loginAddresses := gateway.DiscoveredServices(store, servicediscovery.LoginServer)
	listener := testTCPAddress{&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 19090}}
	gatewayURL, _ := testkit.StartHTTP(t, gateway.WithLogin(gameHandler, loginAddresses, client, listener))
	checkStatus := func(request func() (*http.Response, error), want int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			res, err := request()
			if err != nil {
				t.Fatal(err)
			}
			_ = res.Body.Close()
			if res.StatusCode == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("status %d, want %d", res.StatusCode, want)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	wantHealth := func(want int) {
		t.Helper()
		checkStatus(func() (*http.Response, error) { return client.Get(gatewayURL + "/v1/game/health") }, want)
	}
	wantLogin := func(want int) {
		t.Helper()
		checkStatus(func() (*http.Response, error) {
			return client.Post(gatewayURL+"/v1/login", "application/protobuf", http.NoBody)
		}, want)
	}
	wantHealth(http.StatusBadGateway)
	wantLogin(http.StatusServiceUnavailable)
	gameURL, _ := testkit.StartHTTP(t, testkit.GameHealth())
	loginURL, _ := testkit.StartHTTP(t, loginserver.NewHandler(&testAccount{}, &memoryTickets{}, serverlog.DefaultLogger(), loginserver.DefaultAdmissionLimits()))
	register := func(name, address string) *discovery.Registrar {
		t.Helper()
		registrar, err := servicediscovery.NewHTTPRegistrar(registry, name, "", address)
		if err != nil {
			t.Fatal(err)
		}
		if err := registrar.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { stop(registrar) })
		return registrar
	}
	game := register(servicediscovery.GameServer, gameURL)
	login := register(servicediscovery.LoginServer, loginURL)
	wantHealth(http.StatusOK)
	wantLogin(http.StatusUnauthorized)
	stop(game)
	// 保持 HTTP 监听可用，验证仅删除发现记录就会改变网关的路由结果。
	wantHealth(http.StatusBadGateway)
	wantLogin(http.StatusUnauthorized)
	stop(login)
	wantLogin(http.StatusServiceUnavailable)
}
