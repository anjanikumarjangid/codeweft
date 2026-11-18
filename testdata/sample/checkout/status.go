package checkout

import (
	"net/http"
	"os"
)

// orderStatus asks the orders service, found from the environment, for an order.
func orderStatus(id string) (*http.Response, error) {
	return http.Get(os.Getenv("ORDERS_URL") + "/orders/" + id)
}
