package runnerproto

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strconv"
)

// Command signing lets a runner accept prompts and approvals only from the
// device that scanned its pairing QR. The runner generates the command key
// at pairing and shows it in the QR only; the pool never receives it. The
// app signs with HMAC-SHA256 and the pool relays the signatures opaquely.

// CommandKeyBytes is the length of a runner command key.
const CommandKeyBytes = 32

// MaxCommandSignatures bounds the signatures one command may carry: a signer
// labels one signature per runner key it holds.
const MaxCommandSignatures = 32

const commandKeyIDLength = 22
const commandSignatureLength = 43

func commandMAC(key, input []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(input)
	return mac.Sum(nil)
}

// CommandKeyID names a key without revealing it.
func CommandKeyID(key []byte) string {
	return base64.RawURLEncoding.EncodeToString(commandMAC(key, []byte("codexpool-command-key-id"))[:16])
}

// signingInput encodes fields with length prefixes ("<bytes>:<field>;"), so
// no field value can shift the boundary of another.
func signingInput(fields ...string) []byte {
	var b bytes.Buffer
	for _, field := range fields {
		b.WriteString(strconv.Itoa(len(field)))
		b.WriteByte(':')
		b.WriteString(field)
		b.WriteByte(';')
	}
	return b.Bytes()
}

// TurnSigningInput binds a prompt to its session and to the session settings
// that decide what the agent may do, so the pool cannot redirect it to
// another session or run it under different permissions. Model and reasoning
// effort stay unsigned because the pool may legitimately cap them.
func TurnSigningInput(sessionID string, issuedAt int64, agent, permissionMode string, workspace Workspace, input string) []byte {
	return signingInput("codexpool-turn/1", sessionID, strconv.FormatInt(issuedAt, 10), agent, permissionMode, workspace.Mode, workspace.Target, input)
}

// ApprovalSigningInput binds a decision to the exact command the user saw.
func ApprovalSigningInput(sessionID, approvalID, decision string, issuedAt int64, command string) []byte {
	sum := sha256.Sum256([]byte(command))
	return signingInput("codexpool-approval/1", sessionID, approvalID, decision, strconv.FormatInt(issuedAt, 10), hex.EncodeToString(sum[:]))
}

// SignCommand returns the base64url HMAC of input under key.
func SignCommand(key, input []byte) string {
	return base64.RawURLEncoding.EncodeToString(commandMAC(key, input))
}

// VerifyCommand reports whether signature is the HMAC of input under key.
func VerifyCommand(key, input []byte, signature string) bool {
	got, err := base64.RawURLEncoding.DecodeString(signature)
	return err == nil && hmac.Equal(got, commandMAC(key, input))
}

// ValidCommandKeyID reports whether id has the shape CommandKeyID produces.
func ValidCommandKeyID(id string) bool {
	return len(id) == commandKeyIDLength && base64URL(id)
}

// ValidCommandSignatures reports whether a relayed signature set is well
// formed. An empty set is valid for the commands runners accept unsigned.
func ValidCommandSignatures(issuedAt int64, signatures map[string]string) bool {
	if len(signatures) == 0 {
		return issuedAt == 0
	}
	if issuedAt <= 0 || len(signatures) > MaxCommandSignatures {
		return false
	}
	for id, signature := range signatures {
		if !ValidCommandKeyID(id) || len(signature) != commandSignatureLength || !base64URL(signature) {
			return false
		}
	}
	return true
}

func base64URL(value string) bool {
	for _, c := range value {
		if c != '-' && c != '_' && !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}
