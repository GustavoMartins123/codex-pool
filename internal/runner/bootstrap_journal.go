package runner

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"codex-pool-proxy/internal/runnerproto"
)

const journalVersion = 1

// pairingJournal is the in-flight pairing a runner must be able to finish
// after a crash: with the device secret it can recover the credential the
// pool issued instead of leaving a runner with no local copy. It never holds
// the LAN certificate or QR secret, so a restart cannot reopen the old QR.
type pairingJournal struct {
	Version      int       `json:"version"`
	PoolURL      string    `json:"pool_url"`
	Name         string    `json:"name"`
	PairingID    string    `json:"pairing_id"`
	DeviceSecret string    `json:"device_secret"`
	CommandKey   string    `json:"command_key"`
	Interval     int       `json:"interval"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// JournalPath keeps the journal beside the runner state it will become.
func JournalPath(statePath string) string {
	return filepath.Join(filepath.Dir(statePath), "pairing-journal.json")
}

func (j pairingJournal) start() runnerproto.PairingStart {
	return runnerproto.PairingStart{ID: j.PairingID, DeviceSecret: j.DeviceSecret, ExpiresAt: j.ExpiresAt, Interval: j.Interval, Delivery: runnerproto.PairingDeliveryRecoverable}
}

// recoverableUntil is when the pool forgets the attempt: an issued
// credential stays recoverable for the receipt window after issue.
func (j pairingJournal) recoverableUntil() time.Time {
	return j.ExpiresAt.Add(5 * time.Minute)
}

func writeJournal(path string, j pairingJournal) error {
	j.Version = journalVersion
	return writePrivateJSON(path, j)
}

// readJournal returns os.ErrNotExist when no pairing is in flight.
func readJournal(path string) (pairingJournal, error) {
	var j pairingJournal
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return j, os.ErrNotExist
	}
	if err != nil {
		return j, errors.New("pairing journal unavailable")
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 64<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&j) != nil || j.Version != journalVersion || ValidateOrigin(j.PoolURL) != nil || !validPairingID(j.PairingID) ||
		len(j.DeviceSecret) != 43 || j.Interval < 3 || j.Interval > 30 || j.ExpiresAt.IsZero() || strings.ContainsAny(j.Name, "\x00\r\n") {
		return j, errors.New("invalid pairing journal")
	}
	if _, err := decodeCommandKey(j.CommandKey); err != nil {
		return j, errors.New("invalid pairing journal")
	}
	return j, nil
}
