package runnerproto

import "time"

const Version = 3
const MaxMessageBytes = 256 << 10
const MaxInputBytes = 64 << 10

type Workspace struct {
	Mode   string `json:"mode"`
	Target string `json:"target,omitempty"`
}

type CreateSession struct {
	Agent           string    `json:"agent"`
	Model           string    `json:"model"`
	ReasoningEffort string    `json:"reasoning_effort"`
	Workspace       Workspace `json:"workspace"`
	PermissionMode  string    `json:"permission_mode"`
	Prompt          string    `json:"prompt,omitempty"`
}

type TurnSettings struct {
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort"`
}

type TurnInput struct {
	Input           string  `json:"input"`
	Model           *string `json:"model,omitempty"`
	ReasoningEffort *string `json:"reasoning_effort,omitempty"`
	// IssuedAt (Unix milliseconds) and Signatures, keyed by CommandKeyID,
	// carry the app's TurnSigningInput signatures; see signing.go.
	IssuedAt   int64             `json:"issued_at,omitempty"`
	Signatures map[string]string `json:"signatures,omitempty"`
}

// ApprovalDecision is the app's answer to an approval request. Approving on
// a runner with a command key requires ApprovalSigningInput signatures.
type ApprovalDecision struct {
	Decision   string            `json:"decision"`
	IssuedAt   int64             `json:"issued_at,omitempty"`
	Signatures map[string]string `json:"signatures,omitempty"`
}

type Approval struct {
	ID        string    `json:"approval_id"`
	Command   string    `json:"command"`
	Context   string    `json:"context"`
	TurnID    string    `json:"turn_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

type TurnSummary struct {
	TurnID     string `json:"turn_id"`
	Tokens     int64  `json:"tokens"`
	DurationMS int64  `json:"duration_ms"`
	ExitCode   int    `json:"exit_code"`
}

type AgentCheckpoint struct {
	Version         int    `json:"version"`
	NativeSessionID string `json:"native_session_id"`
	NativeLogID     string `json:"native_log_id"`
	Digest          string `json:"digest"`
	Ready           bool   `json:"ready"`
}

type Session struct {
	ID              string           `json:"id"`
	ConversationID  string           `json:"conversation_id"`
	PrincipalID     string           `json:"principal_id"`
	ClientID        string           `json:"client_id"`
	Agent           string           `json:"agent"`
	Model           string           `json:"model"`
	ReasoningEffort string           `json:"reasoning_effort"`
	Workspace       Workspace        `json:"workspace"`
	PermissionMode  string           `json:"permission_mode"`
	State           string           `json:"state"`
	RunnerID        string           `json:"runner_id,omitempty"`
	LeaseExpiresAt  time.Time        `json:"lease_expires_at"`
	TurnID          string           `json:"turn_id,omitempty"`
	CreatedAt       time.Time        `json:"created_at"`
	UpdatedAt       time.Time        `json:"updated_at"`
	ClosedAt        *time.Time       `json:"closed_at,omitempty"`
	Turns           int              `json:"turns"`
	ClosedReason    string           `json:"closed_reason,omitempty"`
	Approvals       []Approval       `json:"approvals"`
	Summaries       []TurnSummary    `json:"turn_summaries"`
	Checkpoint      *AgentCheckpoint `json:"checkpoint,omitempty"`
	ResumeRequested bool             `json:"resume_requested"`
}

type Hello struct {
	Agents             []string `json:"agents"`
	OS                 string   `json:"os"`
	Arch               string   `json:"arch"`
	Labels             []string `json:"labels"`
	AllowedPaths       []string `json:"allowed_paths"`
	PersistentSessions bool     `json:"persistent_sessions"`
	// CommandKeyID is set by runners that require signed commands. Only
	// those runners receive IssuedAt and Signatures in command frames.
	CommandKeyID string `json:"command_key_id,omitempty"`
}

type Runner struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Online       bool      `json:"online"`
	Agents       []string  `json:"agents"`
	Labels       []string  `json:"labels"`
	AllowedPaths []string  `json:"allowed_paths"`
	OS           string    `json:"os"`
	Arch         string    `json:"arch"`
	Revoked      bool      `json:"revoked"`
	CreatedAt    time.Time `json:"created_at"`
	CommandKeyID string    `json:"command_key_id,omitempty"`
}

// Every frame carries a version. A connection never negotiates another protocol.
type Message struct {
	Version        int              `json:"version"`
	Type           string           `json:"type"`
	SessionID      string           `json:"session_id,omitempty"`
	TurnID         string           `json:"turn_id,omitempty"`
	Hello          *Hello           `json:"hello,omitempty"`
	Session        *Session         `json:"session,omitempty"`
	Checkpoint     *AgentCheckpoint `json:"checkpoint,omitempty"`
	Sessions       []string         `json:"sessions,omitempty"`
	CredentialPath string           `json:"credential_path,omitempty"`
	Input          string           `json:"input,omitempty"`
	Settings       *TurnSettings    `json:"settings,omitempty"`
	Kind           string           `json:"kind,omitempty"`
	Content        string           `json:"content,omitempty"`
	ApprovalID     string           `json:"approval_id,omitempty"`
	Command        string           `json:"command,omitempty"`
	Context        string           `json:"context,omitempty"`
	Decision       string           `json:"decision,omitempty"`
	Status         string           `json:"status,omitempty"`
	Reason         string           `json:"reason,omitempty"`
	Tokens         int64            `json:"tokens,omitempty"`
	DurationMS     int64            `json:"duration_ms,omitempty"`
	ExitCode       int              `json:"exit_code,omitempty"`
	Sequence       uint64           `json:"sequence,omitempty"`
	IssuedAt       int64            `json:"issued_at,omitempty"`
	// Signatures holds only the receiving runner's entry.
	Signatures map[string]string `json:"signatures,omitempty"`
}

func Frame(kind string) Message { return Message{Version: Version, Type: kind} }

// ExitAutoPermissionsDisabled is the process.exit status of a session the
// runner refused because its launch config does not allow auto permissions.
const ExitAutoPermissionsDisabled = "auto_permissions_disabled"
