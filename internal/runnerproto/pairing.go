package runnerproto

import "time"

type PairingRequest struct {
	Name string `json:"name"`
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

// DeviceSecret is delivered only to the initiating machine, never in the QR.
type PairingStart struct {
	ID           string    `json:"id"`
	DeviceSecret string    `json:"device_secret"`
	UserCode     string    `json:"user_code"`
	ExpiresAt    time.Time `json:"expires_at"`
	Interval     int       `json:"interval"`
}

type PairingReview struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	OS        string    `json:"os"`
	Arch      string    `json:"arch"`
	RunnerID  string    `json:"runner_id,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
}

type PairingDecision struct {
	Decision string `json:"decision"`
	UserCode string `json:"user_code"`
}

type RunnerCredential struct {
	RunnerID string `json:"runner_id"`
	Token    string `json:"token"`
}
