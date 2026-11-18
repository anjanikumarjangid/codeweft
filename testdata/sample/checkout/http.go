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

// inventoryBase is where the inventory service listens.
const inventoryBase = "http://inventory:8000"

// stockLevel asks the inventory service for an item's stock.
func stockLevel(base, sku string) (*http.Response, error) {
	return http.Get(base + "/inventory/stock/" + sku)
}

// reserveRequest builds the request holding an item in the inventory service.
func reserveRequest(sku string) (*http.Request, error) {
	return http.NewRequest(http.MethodPost, inventoryBase+"/inventory/stock/"+sku+"/reserve", nil)
}
