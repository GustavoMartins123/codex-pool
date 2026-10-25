package runner

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

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
// turn already accepted. Runners paired before command keys skip the check.
func (s *localSession) verifyTurn(m runnerproto.Message, now time.Time) error {
	d := s.daemon
	if d.commandKey == nil {
		return nil
	}
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
	lastAt, lastTurn := readLastTurnSignature(control)
	// A redelivery of the turn accepted last, after a runner restart, keeps
	// its turn ID; anything older is a replay.
	if m.IssuedAt < lastAt || m.IssuedAt == lastAt && m.TurnID != lastTurn {
		return errors.New("turn signature was already used")
	}
	return writeLastTurnSignature(control, m.IssuedAt, m.TurnID)
}

// verifyApproval admits an approve decision only when the paired device
// signed it for the exact command this runner asked about.
func (s *localSession) verifyApproval(m runnerproto.Message, command string, now time.Time) error {
	d := s.daemon
	if d.commandKey == nil {
		return nil
	}
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

func readLastTurnSignature(control string) (int64, string) {
	data, err := os.ReadFile(filepath.Join(control, lastTurnFile))
	if err != nil {
		return 0, ""
	}
	at, turn, _ := strings.Cut(strings.TrimSpace(string(data)), " ")
	value, err := strconv.ParseInt(at, 10, 64)
	if err != nil {
		return 0, ""
	}
	return value, turn
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
	return os.Rename(temp, filepath.Join(control, lastTurnFile))
}
