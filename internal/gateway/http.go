package gateway

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/NumberMan1/orbit/discovery"
	orbithttp "github.com/NumberMan1/orbit/socket/http"

	"github.com/NumberMan1/slg-server/infrastructure/httpserver"
	"github.com/NumberMan1/slg-server/infrastructure/servicediscovery"
)

func NewUpstreamClient() (*http.Client, error) {
	client, err := orbithttp.NewClient(3 * time.Second)
	if err != nil {
		return nil, err
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return client, nil
}

func DiscoveredServices(store *discovery.Store, name string) func() []string {
	return func() []string {
		if !store.Status().Ready {
			return nil
		}
		var addresses []string
		for _, instance := range store.All() {
			if instance.Name == name && instance.Metadata[discovery.DrainingMetadata] != "true" {
				addresses = append(addresses, instance.Address)
			}
		}
		return addresses
	}
}

func NewHandler(gameAddresses func() []string, client *http.Client) http.Handler {
	var next atomic.Uint64
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", httpserver.Health("gateway"))
	mux.HandleFunc("GET /v1/game/health", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		addresses := gameAddresses()
		if len(addresses) > 0 {
			start := int((next.Add(1) - 1) % uint64(len(addresses)))
			for offset := range addresses {
				if ctx.Err() != nil {
					break
				}
				upstream, err := servicediscovery.ParseOrigin(addresses[(start+offset)%len(addresses)])
				if err != nil {
					continue
				}
				upstream.Path, upstream.RawPath = "/healthz", ""
				var result httpserver.HealthResponse
				err = orbithttp.DoJSON(ctx, client, http.MethodGet, upstream.String(), nil, &result, 1024)
				if err == nil && result.Service == servicediscovery.GameServer && result.Status == "ok" {
					_ = orbithttp.WriteJSON(w, http.StatusOK, result)
					return
				}
			}
		}
		http.Error(w, "game server unavailable", http.StatusBadGateway)
	})
	return mux
}
