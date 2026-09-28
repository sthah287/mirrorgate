package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"mirrorgate/shared/tracing"
)

type Product struct {
	ID       int     `json:"id"`
	Name     string  `json:"name"`
	Price    float64 `json:"price"`
	Category string  `json:"category"`
}

type User struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
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
	{12, "Wireless Keyboard", 49.99, "input"},
}

var users = []User{
	{1, "Maya Patel", "maya@example.com", "pro"},
	{2, "Jordan Lee", "jordan@example.com", "free"},
	{3, "Sam Rivera", "sam@example.com", "pro"},
	{7, "Alex Kim", "alex@example.com", "team"},
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "9001"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": "v1"})
	})
	mux.HandleFunc("GET /api/products", listProducts)
	mux.HandleFunc("GET /api/products/{id}", getProduct)
	mux.HandleFunc("GET /api/users/{id}", getUser)

	shutdownTracing, err := tracing.Init(context.Background(), "stable-service")
	if err != nil {
		log.Fatalf("tracing: %v", err)
	}
	defer shutdownTracing(context.Background())

	// otelhttp reads the traceparent header the gateway sent, so this service
	// shows up as part of the gateway's trace rather than its own.
	log.Printf("stable service (v1) listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, otelhttp.NewHandler(mux, "stable-service")))
}

func listProducts(w http.ResponseWriter, r *http.Request) {
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
	for _, u := range users {
		if u.ID == id {
			writeJSON(w, http.StatusOK, u)
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
