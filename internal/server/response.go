package server

import (
	"encoding/json"
	"log"
	"net/http"
)

// ErrorResponse is the standard JSON error envelope returned by all HTTP error responses.
type ErrorResponse struct {
	Error string `json:"error"`
}

// writeError writes a JSON error response with the given HTTP status code and message.
func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(ErrorResponse{Error: message}); err != nil {
		log.Printf("[APIServer] failed to write error response: %v", err)
	}
}
