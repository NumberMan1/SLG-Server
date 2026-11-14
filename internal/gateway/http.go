package gateway

import (
	"errors"
	"net/http"
	"net/url"
	"time"

	orbithttp "github.com/NumberMan1/orbit/socket/http"

	"github.com/NumberMan1/slg-server/infrastructure/httpserver"
)

func NewGameClient() (*http.Client, error) {
	client, err := orbithttp.NewClient(3 * time.Second)
	if err != nil {
		return nil, err
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return client, nil
}

func NewHandler(gameURL string, client *http.Client) (http.Handler, error) {
	upstream, err := url.Parse(gameURL)
	if err != nil || (upstream.Scheme != "http" && upstream.Scheme != "https") ||
		upstream.Hostname() == "" || upstream.User != nil ||
		(upstream.Path != "" && upstream.Path != "/") ||
		upstream.ForceQuery || upstream.RawQuery != "" || upstream.Fragment != "" {
		return nil, errors.New("SLG_GAME_URL must be an HTTP(S) origin")
	}
	upstream.Path = "/healthz"
	upstream.RawPath = ""
	healthURL := upstream.String()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", httpserver.Health("gateway"))
	mux.HandleFunc("GET /v1/game/health", func(w http.ResponseWriter, r *http.Request) {
		var result httpserver.HealthResponse
		err := orbithttp.DoJSON(r.Context(), client, http.MethodGet, healthURL, nil, &result, 1024)
		if err != nil || result.Service != "game-server" || result.Status != "ok" {
			http.Error(w, "game server unavailable", http.StatusBadGateway)
			return
		}
		_ = orbithttp.WriteJSON(w, http.StatusOK, result)
	})
	return mux, nil
}
