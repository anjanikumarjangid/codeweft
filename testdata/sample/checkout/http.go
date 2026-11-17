package checkout

import (
	"encoding/json"
	"net/http"

	"example.com/sample/payments"
)

// Handler serves the checkout HTTP API.
type Handler struct {
	gw payments.Gateway
}

// Routes registers the checkout endpoints on mux.
func Routes(mux *http.ServeMux, gw payments.Gateway) {
	h := &Handler{gw: gw}
	mux.HandleFunc("POST /api/v1/checkout", handleCheckout)

	mux.HandleFunc("GET /api/v1/orders/{id}", h.getOrder)
}

func handleCheckout(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) getOrder(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(newOrder(r.PathValue("id")))
}
