package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"mirrorgate/shared/tracing"
)

// The candidate is a copy of the stable service with a few intentional
// regressions so MirrorGate has something real to catch. Each one is marked
// with "REGRESSION" and listed in the README.

type Product struct {
	ID       int     `json:"id"`
	Name     string  `json:"name"`
	Price    float64 `json:"price"`
	Category string  `json:"category"`
}

// Field order is different from v1 on purpose. The JSON is the same data,
// so MirrorGate should still count these responses as matching.
type User struct {
	Email string `json:"email"`
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Plan  string `json:"plan"`
}

var products = []Product{
	{1, "USB-C Charger", 19.99, "accessories"},
	{2, "27in Monitor", 229.00, "displays"},
	{3, "Laptop Stand", 34.50, "accessories"},
	{4, "Noise Cancelling Headphones", 179.99, "audio"},
	{5, "Webcam 1080p", 59.00, "video"},
	{6, "Mechanical Keyboard", 89.99, "input"},
	{7, "Ergonomic Mouse", 39.99, "input"},
	{8, "External SSD 1TB", 99.99, "storage"},
	{9, "HDMI Cable 2m", 9.99, "accessories"},
	{10, "Desk Lamp", 24.99, "office"},
	{11, "Bluetooth Speaker", 45.00, "audio"},
	// REGRESSION (body): price changed from 49.99 to 59.99.
	{12, "Wireless Keyboard", 59.99, "input"},
}

var users = []User{
	{"maya@example.com", 1, "Maya Patel", "pro"},
	{"jordan@example.com", 2, "Jordan Lee", "free"},
	{"sam@example.com", 3, "Sam Rivera", "pro"},
	{"alex@example.com", 7, "Alex Kim", "team"},
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "9002"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": "v2"})
	})
	mux.HandleFunc("GET /api/products", listProducts)
	mux.HandleFunc("GET /api/products/{id}", getProduct)
	mux.HandleFunc("GET /api/users/{id}", getUser)

	shutdownTracing, err := tracing.Init(context.Background(), "candidate-service")
	if err != nil {
		log.Fatalf("tracing: %v", err)
	}
	defer shutdownTracing(context.Background())

	// otelhttp reads the traceparent header the gateway sent, so this service
	// shows up as part of the gateway's trace rather than its own.
	log.Printf("candidate service (v2) listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, otelhttp.NewHandler(mux, "candidate-service")))
}

func listProducts(w http.ResponseWriter, r *http.Request) {
	// REGRESSION (latency): same response as v1, but about 400ms slower.
	time.Sleep(400 * time.Millisecond)

	category := r.URL.Query().Get("category")
	result := []Product{}
	for _, p := range products {
		if category == "" || p.Category == category {
			result = append(result, p)
		}
	}
	writeJSON(w, http.StatusOK, result)
}

func getProduct(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid product id"})
		return
	}

	// REGRESSION (error): product 8 fails in v2 while v1 returns it normally.
	if id == 8 {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "inventory lookup failed"})
		return
	}

	for _, p := range products {
		if p.ID == id {
			writeJSON(w, http.StatusOK, p)
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "product not found"})
}

func getUser(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid user id"})
		return
	}

	// REGRESSION (timeout): user 7 hangs for 3s, longer than the gateway's
	// default shadow timeout of 2s.
	if id == 7 {
		time.Sleep(3 * time.Second)
	}

	for _, u := range users {
		if u.ID == id {
			w.Header().Set("Content-Type", "application/json")
			body, err := json.MarshalIndent(u, "", "    ")
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if _, err := w.Write(body); err != nil {
				log.Printf("write response: %v", err)
			}
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "user not found"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write response: %v", err)
	}
}
