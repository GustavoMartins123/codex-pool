package runnerproto

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// BootstrapClockSkew bounds how far the scanning device's clock may run
// behind the runner's when it checks a QR's expiry. The runner enforces the
// deadline with its own clock.
const BootstrapClockSkew = 10 * time.Minute

const bootstrapTokenLength = 43

// BootstrapLink renders the QR of a runner waiting for its first pairing.
// It names local endpoints only: the pool origin arrives later, from the
// device, over the pinned channel.
func BootstrapLink(p BootstrapPayload) (string, error) {
	if err := validateBootstrapPayload(p); err != nil {
		return "", err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	link := "codexpool://bootstrap?v=" + strconv.Itoa(BootstrapVersion) + "&payload=" + base64.RawURLEncoding.EncodeToString(raw)
	if len(link) > BootstrapMaxLinkBytes {
		return "", errors.New("bootstrap QR exceeds its size limit")
	}
	return link, nil
}

// ParseBootstrapLink accepts only links BootstrapLink can produce. A zero
// now skips the expiry check.
func ParseBootstrapLink(link string, now time.Time) (BootstrapPayload, error) {
	var p BootstrapPayload
	invalid := errors.New("invalid bootstrap QR")
	// url.Parse lowercases the scheme; the app compares the prefix exactly.
	if len(link) > BootstrapMaxLinkBytes || !strings.HasPrefix(link, "codexpool://bootstrap?") || strings.ContainsFunc(link, func(r rune) bool { return r < 0x21 || r == 0x7f }) {
		return p, invalid
	}
	u, err := url.Parse(link)
	if err != nil || u.Scheme != "codexpool" || u.Host != "bootstrap" || u.Opaque != "" || u.User != nil || u.Path != "" || u.RawPath != "" || u.Fragment != "" || strings.Contains(link, "#") {
		return p, invalid
	}
	// Escapes would let two spellings of one payload both parse.
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil || strings.ContainsAny(u.RawQuery, "%+;") || len(query) != 2 || len(query["v"]) != 1 || len(query["payload"]) != 1 || query.Get("v") != strconv.Itoa(BootstrapVersion) {
		return p, invalid
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(query.Get("payload"))
	if err != nil || rejectDuplicateKeys(raw) != nil {
		return p, invalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&p) != nil || decoder.Decode(new(any)) != io.EOF {
		return p, invalid
	}
	if err := validateBootstrapPayload(p); err != nil {
		return p, err
	}
	if !now.IsZero() && (!p.ExpiresAt.After(now) || p.ExpiresAt.After(now.Add(BootstrapTTLSeconds*time.Second+BootstrapClockSkew))) {
		return p, errors.New("bootstrap QR expired")
	}
	return p, nil
}

func validateBootstrapPayload(p BootstrapPayload) error {
	invalid := errors.New("invalid bootstrap QR")
	if p.Version != BootstrapVersion || !bootstrapToken(p.BootstrapID) || !bootstrapToken(p.BootstrapSecret) || !bootstrapToken(p.TLSSPKISHA256) || p.ExpiresAt.IsZero() {
		return invalid
	}
	if len(p.Endpoints) == 0 || len(p.Endpoints) > BootstrapMaxEndpoints {
		return invalid
	}
	seen := map[string]bool{}
	for _, endpoint := range p.Endpoints {
		if _, err := ParseBootstrapEndpoint(endpoint); err != nil || seen[endpoint] {
			return invalid
		}
		seen[endpoint] = true
	}
	return nil
}

// ParseBootstrapEndpoint accepts https://<private IPv4>:<port> in canonical
// form, the only shape a runner advertises.
func ParseBootstrapEndpoint(endpoint string) (netip.AddrPort, error) {
	invalid := errors.New("invalid bootstrap endpoint")
	rest, ok := strings.CutPrefix(endpoint, "https://")
	if !ok {
		return netip.AddrPort{}, invalid
	}
	address, err := netip.ParseAddrPort(rest)
	if err != nil || !BootstrapAddressAllowed(address.Addr()) || address.Port() < 1024 || address.String() != rest {
		return netip.AddrPort{}, invalid
	}
	return address, nil
}

// BootstrapAddressAllowed limits the first release to private IPv4 LAN
// addresses: no loopback, link-local, CGNAT, multicast or public address.
func BootstrapAddressAllowed(addr netip.Addr) bool {
	return addr.Is4() && addr.IsPrivate()
}

func bootstrapToken(value string) bool {
	if len(value) != bootstrapTokenLength {
		return false
	}
	_, err := base64.RawURLEncoding.Strict().DecodeString(value)
	return err == nil
}

// rejectDuplicateKeys refuses objects that repeat a key: encoding/json keeps
// the last value, so two parsers could disagree on the same payload.
func rejectDuplicateKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		switch token {
		case json.Delim('{'):
			keys := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				name, _ := key.(string)
				if keys[name] {
					return errors.New("duplicate JSON key")
				}
				keys[name] = true
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case json.Delim('['):
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		}
		return nil
	}
	return walk()
}
