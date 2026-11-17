// Command server runs the checkout API.
package main

import (
	"log"
	"net/http"

	"example.com/sample/checkout"
)

func main() {
	mux := http.NewServeMux()
	checkout.Routes(mux, nil)
	log.Fatal(http.ListenAndServe(":8080", mux))
}
