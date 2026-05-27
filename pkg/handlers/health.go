package handlers

import "net/http"

// HealthHandler returns sandbox health status.
func HealthHandler(w http.ResponseWriter, r *http.Request) {
	JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
