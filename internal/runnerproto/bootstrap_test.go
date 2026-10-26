package runnerproto

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

type bootstrapFixtures struct {
	Now   time.Time `json:"now"`
	Valid []struct {
		Name    string           `json:"name"`
		Link    string           `json:"link"`
		Payload BootstrapPayload `json:"payload"`
	} `json:"valid"`
	Invalid []struct {
		Name string `json:"name"`
		Link string `json:"link"`
	} `json:"invalid"`
}

// The app parses the same fixtures in BootstrapLinkTest.kt.
func loadBootstrapFixtures(t *testing.T) bootstrapFixtures {
	t.Helper()
	raw, err := os.ReadFile("../../contracts/fixtures/bootstrap_links.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures bootstrapFixtures
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

func TestBootstrapLinkFixtures(t *testing.T) {
	fixtures := loadBootstrapFixtures(t)
	for _, c := range fixtures.Valid {
		got, err := ParseBootstrapLink(c.Link, fixtures.Now)
		if err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		if !reflect.DeepEqual(got, c.Payload) {
			t.Errorf("%s: payload = %+v", c.Name, got)
		}
		link, err := BootstrapLink(got)
		if err != nil {
			t.Errorf("%s: render: %v", c.Name, err)
		} else if again, err := ParseBootstrapLink(link, fixtures.Now); err != nil || !reflect.DeepEqual(again, got) {
			t.Errorf("%s: rendered link does not round-trip: %v", c.Name, err)
		}
	}
	for _, c := range fixtures.Invalid {
		if _, err := ParseBootstrapLink(c.Link, fixtures.Now); err == nil {
			t.Errorf("%s accepted", c.Name)
		}
	}
}

func TestBootstrapLinkRefusesOversizedPayloads(t *testing.T) {
	p := loadBootstrapFixtures(t).Valid[0].Payload
	p.Endpoints = []string{"https://192.168.100.200:65535", "https://192.168.100.201:65535", "https://192.168.100.202:65535", "https://192.168.100.203:65535"}
	link, err := BootstrapLink(p)
	if err != nil || len(link) > BootstrapMaxLinkBytes {
		t.Fatalf("largest valid payload: %d bytes, %v", len(link), err)
	}
	p.Endpoints = append(p.Endpoints, "https://192.168.100.204:65535")
	if _, err := BootstrapLink(p); err == nil {
		t.Fatal("five endpoints rendered")
	}
}

func FuzzParseBootstrapLink(f *testing.F) {
	raw, err := os.ReadFile("../../contracts/fixtures/bootstrap_links.json")
	if err != nil {
		f.Fatal(err)
	}
	var fixtures bootstrapFixtures
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		f.Fatal(err)
	}
	for _, c := range fixtures.Valid {
		f.Add(c.Link)
	}
	for _, c := range fixtures.Invalid {
		f.Add(c.Link)
	}
	f.Fuzz(func(t *testing.T, link string) {
		p, err := ParseBootstrapLink(link, time.Time{})
		if err != nil {
			return
		}
		if len(p.Endpoints) == 0 || len(p.Endpoints) > BootstrapMaxEndpoints || strings.ContainsAny(link, "\r\n%") {
			t.Fatalf("accepted %q", link)
		}
		if _, err := BootstrapLink(p); err != nil {
			t.Fatalf("accepted payload cannot be rendered: %v", err)
		}
	})
}
