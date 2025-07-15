package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
)

// App holds the database connection pool
type App struct {
	DB *sql.DB
}

// TransferRequest represents the JSON body for a fund transfer
type TransferRequest struct {
	FromAccountID int     `json:"source_account_id"`
	ToAccountID   int     `json:"destination_account_id"`
	Amount        float64 `json:"amount"`
}

// Account represents an account record
type Account struct {
	ID          int       `json:"account_id"`
	Balance     float64   `json:"balance"`
	LastUpdated time.Time `json:"-"` // used for optimistic locking
}

// CreateAccountRequest represents the JSON body for creating a new account
type CreateAccountRequest struct {
	AccountID     int     `json:"account_id"`
	InitialBalance float64 `json:"initial_balance"`
}

// APIResponse defines the structure of all API responses
type APIResponse struct {
	Status  string      `json:"status"`
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

// writeJSONError writes a standardized JSON error response
func writeJSONError(w http.ResponseWriter, message string, code int, statusCode int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(APIResponse{
		Status:  "error",
		Code:    code,
		Message: message,
	})
}

// writeJSONSuccess writes a standardized JSON success response
func writeJSONSuccess(w http.ResponseWriter, data interface{}, message string, code int, statusCode int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(APIResponse{
		Status:  "success",
		Code:    code,
		Message: message,
		Data:    data,
	})
}

func main() {
	db, err := sql.Open("postgres", "user=postgres password=postgres dbname=bank sslmode=disable")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	app := &App{DB: db}

	http.HandleFunc("/accounts", app.handleCreateAccount)
	http.HandleFunc("/accounts/", app.handleGetAccount)
	http.HandleFunc("/transactions", app.handleTransfer)

	fmt.Println("Server starting on port 8081...")
	log.Fatal(http.ListenAndServe(":8081", nil))
}

func (a *App) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, "Only POST method is allowed", 1001, http.StatusMethodNotAllowed)
		return
	}

	var req CreateAccountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "Invalid request payload", 1002, http.StatusBadRequest)
		return
	}

	_, err := a.DB.Exec("INSERT INTO accounts (id, balance, last_updated) VALUES ($1, $2, NOW())", req.AccountID, req.InitialBalance)
	if err != nil {
		if pgErr, ok := err.(*pq.Error); ok {
			if pgErr.Code == "23505" {
				writeJSONError(w, "Account already exists", 1003, http.StatusConflict)
				return
			}
			writeJSONError(w, fmt.Sprintf("Database error: %s", pgErr.Message), 1004, http.StatusInternalServerError)
			return
		}
		writeJSONError(w, "Failed to create account", 1005, http.StatusInternalServerError)
		return
	}

	writeJSONSuccess(w, map[string]interface{}{
		"account_id":      req.AccountID,
		"initial_balance": req.InitialBalance,
	}, "Account created", 2001, http.StatusCreated)
}

func (a *App) handleGetAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, "Only GET method is allowed", 1006, http.StatusMethodNotAllowed)
		return
	}

	parts := strings.Split(r.URL.Path, "/")
	if len(parts) != 3 || parts[2] == "" {
		writeJSONError(w, "Invalid account ID", 1007, http.StatusBadRequest)
		return
	}

	accountID, err := strconv.Atoi(parts[2])
	if err != nil {
		writeJSONError(w, "Invalid account ID", 1008, http.StatusBadRequest)
		return
	}

	var acc Account
	err = a.DB.QueryRow("SELECT id, balance, last_updated FROM accounts WHERE id = $1", accountID).Scan(&acc.ID, &acc.Balance, &acc.LastUpdated)
	if err != nil {
		if pgErr, ok := err.(*pq.Error); ok {
			writeJSONError(w, fmt.Sprintf("Database error: %s", pgErr.Message), 1009, http.StatusInternalServerError)
			return
		}
		writeJSONError(w, "Account not found", 1010, http.StatusNotFound)
		return
	}

	writeJSONSuccess(w, acc, "Account retrieved", 2002, http.StatusOK)
}

func (a *App) handleTransfer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, "Only POST method is allowed", 1011, http.StatusMethodNotAllowed)
		return
	}

	var tr TransferRequest
	if err := json.NewDecoder(r.Body).Decode(&tr); err != nil {
		writeJSONError(w, "Invalid request payload", 1012, http.StatusBadRequest)
		return
	}

	maxRetries := 3
	for attempt := 1; attempt <= maxRetries; attempt++ {
		tx, err := a.DB.Begin()
		if err != nil {
			writeJSONError(w, "Failed to begin transaction", 1013, http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()

		var from Account
		err = tx.QueryRow("SELECT id, balance, last_updated FROM accounts WHERE id=$1", tr.FromAccountID).Scan(&from.ID, &from.Balance, &from.LastUpdated)
		if err != nil {
			writeJSONError(w, "Source account not found", 1014, http.StatusNotFound)
			return
		}

		if from.Balance < tr.Amount {
			writeJSONError(w, "Insufficient funds", 1015, http.StatusBadRequest)
			return
		}

		result, err := tx.Exec("UPDATE accounts SET balance = balance - $1, last_updated = NOW() WHERE id = $2 AND last_updated = $3", tr.Amount, tr.FromAccountID, from.LastUpdated)
		rowsAffected, _ := result.RowsAffected()
		if err != nil || rowsAffected == 0 {
			if attempt == maxRetries {
				writeJSONError(w, "Concurrency conflict on debit after retries", 1016, http.StatusConflict)
				return
			}
			tx.Rollback()
			time.Sleep(50 * time.Millisecond)
			continue
		}

		var to Account
		err = tx.QueryRow("SELECT id, balance, last_updated FROM accounts WHERE id=$1", tr.ToAccountID).Scan(&to.ID, &to.Balance, &to.LastUpdated)
		if err != nil {
			writeJSONError(w, "Destination account not found", 1017, http.StatusNotFound)
			return
		}

		result, err = tx.Exec("UPDATE accounts SET balance = balance + $1, last_updated = NOW() WHERE id = $2 AND last_updated = $3", tr.Amount, tr.ToAccountID, to.LastUpdated)
		rowsAffected, _ = result.RowsAffected()
		if err != nil || rowsAffected == 0 {
			if attempt == maxRetries {
				writeJSONError(w, "Concurrency conflict on credit after retries", 1018, http.StatusConflict)
				return
			}
			tx.Rollback()
			time.Sleep(50 * time.Millisecond)
			continue
		}

		_, err = tx.Exec("INSERT INTO transactions (from_account, to_account, amount) VALUES ($1, $2, $3)", tr.FromAccountID, tr.ToAccountID, tr.Amount)
		if err != nil {
			writeJSONError(w, "Failed to log transaction", 1019, http.StatusInternalServerError)
			return
		}

		err = tx.Commit()
		if err != nil {
			writeJSONError(w, "Failed to commit transaction", 1020, http.StatusInternalServerError)
			return
		}

		writeJSONSuccess(w, map[string]interface{}{
			"source_account_id":      tr.FromAccountID,
			"destination_account_id": tr.ToAccountID,
			"amount":                 tr.Amount,
		}, "Transfer successful", 2003, http.StatusOK)
		return
	}
}