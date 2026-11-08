package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"authz-service/internal/model"
)

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(model.ErrorResponse{
		Error: model.APIError{
			Code:    code,
			Message: message,
		},
	})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// maxBodyBytes caps every JSON request body.
const maxBodyBytes = 1 << 20 // 1 MiB

// decodeJSON decodes the request body into dst, limited to maxBodyBytes.
// On failure it writes a 400 invalid_request response and returns false.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusBadRequest, "invalid_request", "request body too large")
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return false
	}
	return true
}
