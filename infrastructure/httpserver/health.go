package httpserver

import (
	"net/http"

	orbithttp "github.com/NumberMan1/orbit/socket/http"
)

type HealthResponse struct {
	Service string `json:"service"`
	Status  string `json:"status"`
}

func Health(service string) http.HandlerFunc {
	response := HealthResponse{Service: service, Status: "ok"}
	return func(w http.ResponseWriter, _ *http.Request) {
		_ = orbithttp.WriteJSON(w, http.StatusOK, response)
	}
}
