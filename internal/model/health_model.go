package model

// --- Health types ---

type HealthResponse struct {
	Status string `json:"status" example:"ok"`
}

type ReadinessResponse struct {
	Status string            `json:"status" example:"ok"`
	Checks map[string]string `json:"checks"`
}
