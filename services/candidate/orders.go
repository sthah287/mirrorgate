package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Orders are the only write in the test services. This is the candidate
// copy: it is wired to the sandbox database and the sandbox payment and email
// endpoints, and it refuses any write that doesn't carry the shadow header.
// It also has the R5 regression in it.

const shippingFee = 5.00

type OrderItem struct {
	ProductID int `json:"product_id"`
	Quantity  int `json:"quantity"`
}

type OrderRequest struct {
	Customer string      `json:"customer"`
	Items    []OrderItem `json:"items"`
}

type Order struct {
	OrderID          string      `json:"order_id"`
	CreatedAt        time.Time   `json:"created_at"`
	Customer         string      `json:"customer"`
	Items            []OrderItem `json:"items"`
	Subtotal         float64     `json:"subtotal"`
	Shipping         float64     `json:"shipping"`
	Total            float64     `json:"total"`
	PaymentReference string      `json:"payment_reference"`
	Status           string      `json:"status"`
}

type orderStore struct {
	pool          *pgxpool.Pool
	paymentsURL   string
	emailURL      string
	client        *http.Client
	sandbox       bool
	requireShadow bool
}

func newOrderStore(ctx context.Context, databaseURL, paymentsURL, emailURL string, sandbox, requireShadow bool) (*orderStore, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("orders pool: %w", err)
	}

	// The database container is usually still starting when this runs.
	var pingErr error
	for attempt := 1; attempt <= 15; attempt++ {
		if pingErr = pool.Ping(ctx); pingErr == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if pingErr != nil {
		pool.Close()
		return nil, fmt.Errorf("orders database: %w", pingErr)
	}

	_, err = pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS orders (
			order_id          TEXT PRIMARY KEY,
			customer          TEXT NOT NULL,
			subtotal          DOUBLE PRECISION NOT NULL,
			shipping          DOUBLE PRECISION NOT NULL,
			total             DOUBLE PRECISION NOT NULL,
			payment_reference TEXT NOT NULL,
			shadow            BOOLEAN NOT NULL DEFAULT FALSE,
			created_at        TIMESTAMPTZ NOT NULL
		)`)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("orders table: %w", err)
	}

	return &orderStore{
		pool:        pool,
		paymentsURL: paymentsURL,
		emailURL:    emailURL,
		// Instrumented so a charge or email shows up as a child span of the
		// request that caused it. That is what makes the sandbox visible in
		// Jaeger: the candidate's call goes to a different endpoint.
		client: &http.Client{
			Timeout:   3 * time.Second,
			Transport: otelhttp.NewTransport(http.DefaultTransport),
		},
		sandbox:       sandbox,
		requireShadow: requireShadow,
	}, nil
}

func (s *orderStore) createOrder(w http.ResponseWriter, r *http.Request) {
	shadow := r.Header.Get("X-MirrorGate-Shadow") == "true"
	// The candidate runs with this on: it is a sandbox deployment and should
	// never see a real customer write, so anything without the shadow header
	// is refused instead of quietly stored.
	if s.requireShadow && !shadow {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "this deployment only accepts shadow traffic",
		})
		return
	}

	var req OrderRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid order"})
		return
	}
	if req.Customer == "" || len(req.Items) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "customer and items are required"})
		return
	}

	subtotal := 0.0
	for _, item := range req.Items {
		if item.Quantity < 1 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "quantity must be at least 1"})
			return
		}
		product, ok := findProduct(item.ProductID)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("unknown product %d", item.ProductID)})
			return
		}
		subtotal += product.Price * float64(item.Quantity)
	}
	subtotal = round2(subtotal)

	order := Order{
		OrderID:   "ord_" + strings.ToLower(rand.Text()[:10]),
		CreatedAt: time.Now().UTC(),
		Customer:  req.Customer,
		Items:     req.Items,
		Subtotal:  subtotal,
		Shipping:  shippingFee,
		// REGRESSION (R5): shipping is charged but left out of the total.
		Total:  subtotal,
		Status: "created",
	}

	// Charge, then store, then notify. There is no transaction across the
	// three, so a failure after the charge would leave a paid order that was
	// never saved. Fine for a test service, not something to copy.
	reference, err := s.charge(r.Context(), order.OrderID, order.Total)
	if err != nil {
		log.Printf("charge failed: %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "payment service unavailable"})
		return
	}
	order.PaymentReference = reference

	if err := s.save(r.Context(), order, shadow); err != nil {
		log.Printf("save order: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not save order"})
		return
	}

	if err := s.notify(r.Context(), order); err != nil {
		// A failed confirmation email doesn't undo the order.
		log.Printf("notify failed: %v", err)
	}

	writeJSON(w, http.StatusCreated, order)
}

// listOrders is for inspecting what this deployment wrote. It sits outside
// /api on purpose so mirrored traffic never reaches it.
func (s *orderStore) listOrders(w http.ResponseWriter, r *http.Request) {
	var count int
	if err := s.pool.QueryRow(r.Context(), "SELECT count(*) FROM orders").Scan(&count); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	rows, err := s.pool.Query(r.Context(),
		`SELECT order_id, customer, total, payment_reference, shadow, created_at
		 FROM orders ORDER BY created_at DESC LIMIT 20`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()

	orders := []map[string]any{}
	for rows.Next() {
		var id, customer, reference string
		var total float64
		var shadow bool
		var createdAt time.Time
		if err := rows.Scan(&id, &customer, &total, &reference, &shadow, &createdAt); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		orders = append(orders, map[string]any{
			"order_id": id, "customer": customer, "total": total,
			"payment_reference": reference, "shadow": shadow, "created_at": createdAt,
		})
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"sandbox": s.sandbox, "count": count, "orders": orders})
}

func (s *orderStore) save(ctx context.Context, o Order, shadow bool) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO orders (order_id, customer, subtotal, shipping, total, payment_reference, shadow, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		o.OrderID, o.Customer, o.Subtotal, o.Shipping, o.Total, o.PaymentReference, shadow, o.CreatedAt)
	return err
}

func (s *orderStore) charge(ctx context.Context, orderID string, amount float64) (string, error) {
	var reply struct {
		Reference string `json:"reference"`
	}
	if err := s.post(ctx, s.paymentsURL, orderID, amount, &reply); err != nil {
		return "", err
	}
	return reply.Reference, nil
}

func (s *orderStore) notify(ctx context.Context, o Order) error {
	return s.post(ctx, s.emailURL, o.OrderID, o.Total, nil)
}

func (s *orderStore) post(ctx context.Context, url, orderID string, amount float64, out any) error {
	body, err := json.Marshal(map[string]any{"order_id": orderID, "amount": amount})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned %d", url, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func findProduct(id int) (Product, bool) {
	for _, p := range products {
		if p.ID == id {
			return p, true
		}
	}
	return Product{}, false
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}
