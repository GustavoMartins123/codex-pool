package runner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

const testTriple = "x86_64-unknown-linux-musl"

type tarEntry struct {
	name string
	body string
	mode int64
	kind byte
}

func agentPackage(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		kind := e.kind
		if kind == 0 {
			kind = tar.TypeReg
		}
		header := &tar.Header{Name: e.name, Mode: e.mode, Size: int64(len(e.body)), Typeflag: kind}
		if kind != tar.TypeReg {
			header.Size = 0
			header.Linkname = "/etc/passwd"
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if kind == tar.TypeReg {
			_, _ = tw.Write([]byte(e.body))
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func codexPackage(t *testing.T, extra ...tarEntry) []byte {
	return agentPackage(t, append([]tarEntry{
		{name: "package/package.json", body: `{"name":"@openai/codex"}`, mode: 0644},
		{name: "package/vendor/" + testTriple + "/bin/codex", body: "#!/bin/sh\necho codex\n", mode: 0755},
		{name: "package/vendor/" + testTriple + "/codex-path/rg", body: "rg", mode: 0755},
		{name: "package/vendor/" + testTriple + "/codex-package.json", body: "{}", mode: 0644},
	}, extra...)...)
}

type registryStub struct {
	server    *httptest.Server
	downloads atomic.Int32
	body      []byte
	truncate  bool
}

func newRegistryStub(t *testing.T, body []byte) *registryStub {
	r := &registryStub{body: body}
	r.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.HasPrefix(req.URL.Path, "/@openai/codex/-/codex-") {
			http.NotFound(w, req)
			return
		}
		r.downloads.Add(1)
		if r.truncate {
			w.Header().Set("Content-Length", "999999")
			_, _ = w.Write(r.body[:len(r.body)/2])
			return
		}
		_, _ = w.Write(r.body)
	}))
	t.Cleanup(r.server.Close)
	return r
}

func testInstaller(t *testing.T, r *registryStub, version string, body []byte) *AgentInstaller {
	digest := sha512.Sum512(body)
	return &AgentInstaller{Root: filepath.Join(t.TempDir(), "agents"), Client: r.server.Client(), registry: r.server.URL, goos: "linux", goarch: "amd64",
		catalog: []AgentRelease{{Agent: "codex", Version: version, GOOS: "linux", GOARCH: "amd64", Package: version + "-linux-x64", Triple: testTriple,
			SHA512: hex.EncodeToString(digest[:]), UnpackedSize: 1 << 20}}}
}

func leftovers(t *testing.T, root string) []string {
	var names []string
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	return names
}

func TestAgentInstallUnpacksOnlyTheVerifiedVendorTree(t *testing.T) {
	body := codexPackage(t)
	registry := newRegistryStub(t, body)
	installer := testInstaller(t, registry, "1.0.0", body)
	if _, ok := installer.Installed("codex"); ok {
		t.Fatal("reported installed before installing")
	}
	path, err := installer.Install(context.Background(), "codex")
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(installer.Root, "codex", "1.0.0-"+testTriple, "bin", "codex") {
		t.Fatalf("installed at %s", path)
	}
	if info, err := os.Stat(path); err != nil || runtime.GOOS != "windows" && info.Mode().Perm()&0100 == 0 {
		t.Fatalf("executable mode: %v %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(installer.Root, "codex", "1.0.0-"+testTriple, "codex-path", "rg")); err != nil {
		t.Fatal("bundled tools missing")
	}
	if _, err := os.Stat(filepath.Join(installer.Root, "codex", "1.0.0-"+testTriple, "package.json")); err == nil {
		t.Fatal("files outside the vendor tree were unpacked")
	}
	if again, err := installer.Install(context.Background(), "codex"); err != nil || again != path || registry.downloads.Load() != 1 {
		t.Fatalf("reinstall = %s %v after %d downloads", again, err, registry.downloads.Load())
	}
	if names := leftovers(t, installer.Root); len(names) != 0 {
		t.Fatalf("temporary files left: %v", names)
	}

	// A newer pinned release replaces the old one.
	newer := testInstaller(t, registry, "1.1.0", body)
	newer.Root = installer.Root
	if _, err := newer.Install(context.Background(), "codex"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(filepath.Dir(path))); !os.IsNotExist(err) {
		t.Fatal("previous release kept")
	}
}

func TestAgentInstallRefusesUnverifiedOrUnsafePackages(t *testing.T) {
	vendor := "package/vendor/" + testTriple + "/"
	cases := map[string]func(*testing.T) (*AgentInstaller, *registryStub){
		"hash mismatch": func(t *testing.T) (*AgentInstaller, *registryStub) {
			r := newRegistryStub(t, codexPackage(t))
			return testInstaller(t, r, "1.0.0", []byte("another archive")), r
		},
		"truncated download": func(t *testing.T) (*AgentInstaller, *registryStub) {
			body := codexPackage(t)
			r := newRegistryStub(t, body)
			r.truncate = true
			return testInstaller(t, r, "1.0.0", body), r
		},
	}
	for name, entry := range map[string]tarEntry{
		"path traversal": {name: vendor + "../../../escape", body: "x", mode: 0644},
		"symlink":        {name: vendor + "bin/link", kind: tar.TypeSymlink},
		"hard link":      {name: vendor + "bin/hard", kind: tar.TypeLink},
		"device":         {name: vendor + "bin/dev", kind: tar.TypeChar},
		"oversized":      {name: vendor + "huge", body: strings.Repeat("x", 3<<20), mode: 0644},
	} {
		cases[name] = func(t *testing.T) (*AgentInstaller, *registryStub) {
			body := codexPackage(t, entry)
			r := newRegistryStub(t, body)
			return testInstaller(t, r, "1.0.0", body), r
		}
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			installer, _ := setup(t)
			if _, err := installer.Install(context.Background(), "codex"); err == nil {
				t.Fatal("installed")
			}
			if _, ok := installer.Installed("codex"); ok {
				t.Fatal("reported installed")
			}
			if names := leftovers(t, installer.Root); len(names) != 0 {
				t.Fatalf("temporary files left: %v", names)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(installer.Root), "escape")); err == nil {
				t.Fatal("wrote outside the install root")
			}
		})
	}
}

func TestAgentInstallHasNothingForUnknownPlatforms(t *testing.T) {
	installer := &AgentInstaller{Root: t.TempDir(), goos: "plan9", goarch: "386"}
	if _, err := installer.Install(context.Background(), "codex"); !errors.Is(err, ErrAgentUnsupported) {
		t.Fatalf("install on plan9 = %v", err)
	}
}

func TestAgentCatalogPinsEveryCodexPlatform(t *testing.T) {
	for _, p := range codexPlatforms {
		installer := &AgentInstaller{goos: p.GOOS, goarch: p.GOARCH}
		r, ok := installer.Release("codex")
		if !ok || r.Triple != p.Triple || r.Package != r.Version+"-"+p.NPM || len(r.SHA512) != 128 || r.UnpackedSize <= 0 {
			t.Errorf("%s/%s: %+v", p.GOOS, p.GOARCH, r)
		}
	}
}
