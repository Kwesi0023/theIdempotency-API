package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/joho/godotenv"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var db *gorm.DB

type IdempotencyKey struct {
	ID           string    `gorm:"primaryKey;type:varchar(255)"`
	Status       string    `gorm:"type:enum('IN_FLIGHT','SUCCESS');not null"`
	RequestHash  []byte    `gorm:"type:binary(32);not null"`
	ResponseCode int       `gorm:"type:int"`
	ResponseBody []byte    `gorm:"type:longblob"`
	CreatedAt    time.Time `gorm:"autoCreateTime"`
	UpdatedAt    time.Time `gorm:"autoUpdateTime"`
}

func processPaymentHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		http.Error(w, "missing Idempotency-Key header", http.StatusBadRequest)
		return
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	// restore Body for potential downstream uses
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

	hash := sha256.Sum256(bodyBytes)
	hashBytes := hash[:]

	// We'll use a manual transaction so we can control locking and commit timing
	tx := db.Begin()
	if tx.Error != nil {
		log.Printf("failed to begin tx: %v", tx.Error)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	defer func() {
		// if still not committed, rollback
		_ = tx.Rollback()
	}()

	var rec IdempotencyKey
	res := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&rec, "id = ?", key)
	if errors.Is(res.Error, gorm.ErrRecordNotFound) {
		// First execution: insert IN_FLIGHT and perform processing while holding the transaction (so other requests block)
		rec = IdempotencyKey{
			ID:          key,
			Status:      "IN_FLIGHT",
			RequestHash: hashBytes,
		}
		if err := tx.Create(&rec).Error; err != nil {
			log.Printf("failed to create idempotency row: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		// Simulate downstream processing while tx is still open (this holds row lock)
		time.Sleep(2 * time.Second)

		// Build response
		var payload map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &payload); err != nil {
			// rollback and return
			_ = tx.Rollback()
			http.Error(w, "invalid json body", http.StatusBadRequest)
			return
		}
		amount := payload["amount"]
		currency := payload["currency"]
		message := fmt.Sprintf("Charged %v %v", amount, currency)
		respObj := map[string]string{"message": message}
		respBytes, _ := json.Marshal(respObj)

		// update row to SUCCESS
		updates := map[string]interface{}{
			"Status":       "SUCCESS",
			"ResponseCode": 200,
			"ResponseBody": respBytes,
		}
		if err := tx.Model(&IdempotencyKey{}).Where("id = ?", key).Updates(updates).Error; err != nil {
			log.Printf("failed to update success row: %v", err)
			_ = tx.Rollback()
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		if err := tx.Commit().Error; err != nil {
			log.Printf("tx commit error: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write(respBytes)
		return
	}
	if res.Error != nil {
		log.Printf("db select error: %v", res.Error)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// If we reach here, a row exists and we've acquired the lock. Check its status.
	if rec.Status == "SUCCESS" {
		// verify hash
		if !equalBytes(rec.RequestHash, hashBytes) {
			_ = tx.Commit()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "Idempotency key already used for a different request body."})
			return
		}
		// replay cached response
		_ = tx.Commit()
		w.Header().Set("X-Cache-Hit", "true")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rec.ResponseCode)
		_, _ = w.Write(rec.ResponseBody)
		return
	}

	if rec.Status == "IN_FLIGHT" { //"IN_FLIGHT" might also mean double checker for post request being sent twice.
		// This path happens when another transaction created the row but hadn't yet committed.
		// Because we used SELECT ... FOR UPDATE, we acquire the lock only after the other tx commits,
		// so after this point the row should reflect the final state (likely SUCCESS).
		// Re-fetch to get the latest state.
		if err := tx.First(&rec, "id = ?", key).Error; err != nil {
			log.Printf("re-fetch error: %v", err)
			_ = tx.Rollback()
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		if rec.Status == "SUCCESS" {
			if !equalBytes(rec.RequestHash, hashBytes) {
				_ = tx.Commit()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "Idempotency key already used for a different request body."})
				return
			}
			_ = tx.Commit()
			w.Header().Set("X-Cache-Hit", "true")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(rec.ResponseCode)
			_, _ = w.Write(rec.ResponseBody)
			return
		}
		// If still IN_FLIGHT after acquiring lock, fall through to process (rare)
	}

	// As a safety net, if we reach here, attempt to process and update
	// Simulate processing
	time.Sleep(2 * time.Second)

	var payload map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		_ = tx.Rollback()
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	amount := payload["amount"]
	currency := payload["currency"]
	message := fmt.Sprintf("Charged %v %v", amount, currency)
	respObj := map[string]string{"message": message}
	respBytes, _ := json.Marshal(respObj)

	updates := map[string]interface{}{
		"Status":       "SUCCESS",
		"ResponseCode": 200,
		"ResponseBody": respBytes,
	}
	if err := tx.Model(&IdempotencyKey{}).Where("id = ?", key).Updates(updates).Error; err != nil {
		log.Printf("final update error: %v", err)
		_ = tx.Rollback()
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if err := tx.Commit().Error; err != nil {
		log.Printf("tx commit error: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_, _ = w.Write(respBytes)
}

// calculates the bytes of each request being sent to the server and compares them to see if they are equal.
// this makes sures that the same idempotency key is not used for different requests.
func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false //hak
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Println("Warning: No .env file found, falling back to system environment variables")
	}
	dsn := os.Getenv("THEIDEMPOTENCY_DSN")

	gormDB, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatalf("failed to connect database: %v", err)
	}
	db = gormDB

	sqlDB, err := db.DB()
	if err != nil {
		log.Fatalf("failed to get sql DB: %v", err)
	}
	sqlDB.SetMaxOpenConns(25)
	sqlDB.SetMaxIdleConns(25)
	sqlDB.SetConnMaxLifetime(5 * time.Minute)

	if err := db.AutoMigrate(&IdempotencyKey{}); err != nil {
		log.Fatalf("auto migrate failed: %v", err)
	}

	http.HandleFunc("/process-payment", processPaymentHandler)

	port := ":8080"
	log.Printf("TheIdempotency Layer is running on %s", port)
	if err := http.ListenAndServe(port, nil); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}
