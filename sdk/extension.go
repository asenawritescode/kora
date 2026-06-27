package sdk

import "time"

type ExtensionRecord struct {
	Name                string    `json:"name"`
	DisplayName         string    `json:"display_name"`
	Description         string    `json:"description,omitempty"`
	EndpointURL         string    `json:"endpoint_url"`
	IsActive            bool      `json:"is_active"`
	Subscriptions       []string  `json:"subscriptions"`
	APIPermissions      []string  `json:"api_permissions"`
	AccessToken         string    `json:"access_token,omitempty"`
	Site                string    `json:"site"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
	TotalDeliveries     int       `json:"total_deliveries"`
	SuccessDeliveries   int       `json:"success_deliveries"`
	FailedDeliveries    int       `json:"failed_deliveries"`
	LastDeliveryAt      time.Time `json:"last_delivery_at,omitempty"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
}

type DeliveryRecord struct {
	ID            string    `json:"id"`
	ExtensionName string    `json:"extension_name"`
	EventType     string    `json:"event_type"`
	Status        string    `json:"status"`
	StatusCode    int       `json:"status_code"`
	RequestBody   string    `json:"request_body,omitempty"`
	ResponseBody  string    `json:"response_body,omitempty"`
	ErrorMessage  string    `json:"error_message,omitempty"`
	DurationMs    int64     `json:"duration_ms"`
	CreatedAt     time.Time `json:"created_at"`
}
