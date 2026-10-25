package runner

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"codex-pool-proxy/internal/atomicfile"
	"codex-pool-proxy/internal/runnerproto"
)

// commandSkew bounds how far a signed command's issue time may drift from
// the runner clock. It absorbs device clock skew while keeping a command the
// pool withheld from running much later than it was signed.
const commandSkew = 10 * time.Minute

// lastTurnFile, kept in the session control directory, records the newest
// accepted turn signature so a restart cannot reopen older ones to replay.
const lastTurnFile = "last-turn-signature"

var errUnsignedCommand = errors.New("command is not signed by the device paired with this runner; scan the runner's QR again with `runnerd rotate`")

// NewCommandKey returns a fresh base64url command key for a pairing QR.
func NewCommandKey() (string, error) {
	key := make([]byte, runnerproto.CommandKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(key), nil
}

func decodeCommandKey(value string) ([]byte, error) {
	key, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(key) != runnerproto.CommandKeyBytes {
		return nil, errors.New("invalid runner command key")
	}
	return key, nil
}

func commandFresh(issuedAt int64, now time.Time) bool {
	at := time.UnixMilli(issuedAt)
	return issuedAt > 0 && at.After(now.Add(-commandSkew)) && at.Before(now.Add(commandSkew))
}

// verifyTurn admits a turn only when the paired device signed this prompt
// for this session and its permission settings, recently, and after every
// turn already accepted.
func (s *localSession) verifyTurn(m runnerproto.Message, now time.Time) error {
	if m.TurnID == "" || strings.ContainsAny(m.TurnID, " \t\r\n") {
		return errors.New("invalid signed turn ID")
	}
	d := s.daemon
	signature := m.Signatures[d.commandKeyID]
	if signature == "" {
		return errUnsignedCommand
	}
	input := runnerproto.TurnSigningInput(s.session.ID, m.IssuedAt, s.session.Agent, s.session.PermissionMode, s.session.Workspace, m.Input)
	if !runnerproto.VerifyCommand(d.commandKey, input, signature) {
		return errors.New("turn signature is invalid for this session")
	}
	if !commandFresh(m.IssuedAt, now) {
		return errors.New("turn signature is outside the accepted time window; check the device and runner clocks")
	}
	s.mu.Lock()
	control := s.control
	s.mu.Unlock()
	lastAt, err := readLastTurnSignature(control)
	if err != nil {
		return fmt.Errorf("read turn replay protection: %w", err)
	}
	// An accepted signature is spent even when execution or its acknowledgement
	// was interrupted. Reconnection of a live process is deduplicated by lastTurn;
	// a restarted process must never execute that signed command again.
	if m.IssuedAt <= lastAt {
		return errors.New("turn signature was already used")
	}
	return writeLastTurnSignature(control, m.IssuedAt, m.TurnID)
}

// verifyApproval admits an approve decision only when the paired device
// signed it for the exact command this runner asked about.
func (s *localSession) verifyApproval(m runnerproto.Message, command string, now time.Time) error {
	d := s.daemon
	signature := m.Signatures[d.commandKeyID]
	if signature == "" {
		return errUnsignedCommand
	}
	input := runnerproto.ApprovalSigningInput(s.session.ID, m.ApprovalID, m.Decision, m.IssuedAt, command)
	if !runnerproto.VerifyCommand(d.commandKey, input, signature) {
		return errors.New("approval signature does not match the requested command")
	}
	if !commandFresh(m.IssuedAt, now) {
		return errors.New("approval signature is outside the accepted time window; check the device and runner clocks")
	}
	return nil
}

func readLastTurnSignature(control string) (int64, error) {
	if control == "" {
		return 0, errors.New("session control directory unavailable")
	}
	data, err := os.ReadFile(filepath.Join(control, lastTurnFile))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil // A new session has not accepted a signed turn yet.
	}
	if err != nil {
		return 0, err
	}
	at, turn, found := strings.Cut(string(data), " ")
	value, err := strconv.ParseInt(at, 10, 64)
	if err != nil || value <= 0 || strconv.FormatInt(value, 10) != at || !found || turn == "" || strings.ContainsAny(turn, " \t\r\n") {
		return 0, errors.New("invalid persisted turn signature")
	}
	return value, nil
}

func writeLastTurnSignature(control string, issuedAt int64, turn string) error {
	if control == "" {
		return errors.New("session control directory unavailable")
	}
	file, err := os.CreateTemp(control, ".last-turn-*.tmp")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer os.Remove(temp)
	if err := privateFile(file); err != nil {
		file.Close()
		return err
	}
	if _, err := file.WriteString(strconv.FormatInt(issuedAt, 10) + " " + turn); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return atomicfile.Publish(temp, filepath.Join(control, lastTurnFile))
}
