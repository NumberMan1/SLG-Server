package testcases

import (
	"encoding/json"
	"mime"
	"net/http"
	"testing"
	"time"

	"slg/server/testkit"
)

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
