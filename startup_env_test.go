package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestStartupEnvSyntax(t *testing.T) {
	data := "\ufeff# comment\r\nexport FIRST = text=more # comment\r\n" +
		"EMPTY=\r\nSINGLE='literal # value' # tail\r\nDOUBLE=\"line\\nnext\\t\\\"\\\\\"\r\n" +
		"PATH_VALUE=C:\\pool\\data\r\nHASH=value#suffix\r\nUNICODE=olá\r\n"
	got, err := parseStartupEnv([]byte(data), false)
	want := []envAssignment{{"FIRST", "text=more"}, {"EMPTY", ""}, {"SINGLE", "literal # value"},
		{"DOUBLE", "line\nnext\t\"\\"}, {"PATH_VALUE", `C:\pool\data`}, {"HASH", "value#suffix"}, {"UNICODE", "olá"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("parse = %#v, %v; want %#v", got, err, want)
	}
	template, err := os.ReadFile(".env.example")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseStartupEnv(template, true); err != nil {
		t.Fatalf("setup template rejected: %v", err)
	}
}

func TestStartupEnvRejectsInvalidFilesWithoutValuesInErrors(t *testing.T) {
	for name, input := range map[string]string{
		"assignment": "PRIVATE_VALUE\n", "key": "1KEY=private-sentinel\n",
		"quote": "KEY='private-sentinel\n", "tail": "KEY='private-sentinel' bad\n",
		"escape": `KEY="private-sentinel\q"`, "duplicate": "KEY=private-sentinel\nKEY=other\n",
		"reference": "KEY=${private-sentinel}\n", "short reference": "KEY=$PRIVATE_VALUE\n",
		"command": "KEY=$(private-sentinel)\n", "backticks": "KEY=`private-sentinel`\n",
		"quoted reference": "KEY='${private-sentinel}'\n", "NUL": "KEY=private-sentinel\x00\n",
		"encoding": "KEY=private-sentinel\xff\n", "CR": "KEY=private-sentinel\rOTHER=1\n",
		"multiline": "KEY=\"private-sentinel\nnext\"\n", "unquoted quote": "KEY=private-sentinel'\n",
		"too large": "KEY=" + strings.Repeat("x", maxStartupEnvBytes),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseStartupEnv([]byte(input), false)
			if err == nil {
				t.Fatal("invalid file accepted")
			}
			if strings.Contains(err.Error(), "private-sentinel") {
				t.Fatal("error exposes value")
			}
		})
	}
	if _, err := parseStartupEnv([]byte("Key=1\nKEY=2\n"), true); err == nil {
		t.Fatal("case-insensitive duplicate accepted")
	}
	if _, err := parseStartupEnv([]byte("Key=1\nKEY=2\n"), false); err != nil {
		t.Fatal(err)
	}
}

func TestStartupEnvImportPrecedenceAndRollback(t *testing.T) {
	for _, fail := range []bool{false, true} {
		state := map[string]string{"EXISTING": "process", "EMPTY": ""}
		lookup := func(key string) (string, bool) { value, ok := state[key]; return value, ok }
		set := func(key, value string) error {
			if fail && key == "SECOND" {
				return errors.New("private-sentinel")
			}
			state[key] = value
			return nil
		}
		unset := func(key string) error { delete(state, key); return nil }
		err := importStartupEnv([]envAssignment{{"EXISTING", "file"}, {"EMPTY", "file"}, {"FIRST", "one"}, {"SECOND", "two"}}, lookup, set, unset)
		want := map[string]string{"EXISTING": "process", "EMPTY": ""}
		if !fail {
			want["FIRST"], want["SECOND"] = "one", "two"
		}
		if !reflect.DeepEqual(state, want) || (err != nil) != fail {
			t.Fatalf("import failed=%v: state=%v error=%v", fail, state, err)
		}
		if err != nil && strings.Contains(err.Error(), "private-sentinel") {
			t.Fatal("import error exposes value")
		}
	}
}

func TestStartupEnvLoadIsAtomic(t *testing.T) {
	key := "CODEX_POOL_STARTUP_ENV_ATOMIC_TEST"
	os.Unsetenv(key)
	t.Cleanup(func() { os.Unsetenv(key) })
	path := filepath.Join(t.TempDir(), "invalid.env")
	if err := os.WriteFile(path, []byte(key+"=private-sentinel\nBROKEN\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadStartupEnv(path); err == nil {
		t.Fatal("malformed file accepted")
	}
	if _, ok := os.LookupEnv(key); ok {
		t.Fatal("malformed file partially imported")
	}
	if err := loadStartupEnv(filepath.Dir(path)); err == nil {
		t.Fatal("directory accepted as env file")
	}
	if err := loadStartupEnv(path + ".missing"); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestStartupEnvFlags(t *testing.T) {
	t.Chdir(t.TempDir())
	var help bytes.Buffer
	if err := prepareStartupEnvironment([]string{"-help"}, &help); !errors.Is(err, flag.ErrHelp) || !strings.Contains(help.String(), "-env-file") {
		t.Fatalf("help = %v, %s", err, help.String())
	}
	for _, args := range [][]string{{"-env-file=", "unexpected"}, {"-listen", "-env-file="}, {"-unknown"}, {"-env-file"}, {"-env-file=", "-check-credentials=bad"}} {
		if err := prepareStartupEnvironment(args, io.Discard); err == nil {
			t.Fatalf("invalid args accepted: %v", args)
		}
	}
	if err := prepareStartupEnvironment([]string{"-env-file=", "-listen", "127.0.0.1:1234", "-check-credentials"}, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestStartupEnvNativeProcess(t *testing.T) {
	if os.Getenv("CODEX_POOL_ENV_TEST_HELPER") == "1" {
		var args []string
		if err := json.Unmarshal([]byte(os.Getenv("CODEX_POOL_ENV_TEST_ARGS")), &args); err != nil {
			t.Fatal(err)
		}
		os.Args = append([]string{os.Args[0]}, args...)
		flag.CommandLine = flag.NewFlagSet("codex-pool", flag.ExitOnError)
		main()
		os.Exit(0)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	secret := hex.EncodeToString(key)
	valid := "POOL_DIR=pool\nPROXY_DB_PATH=data/proxy.db\nDUCKDB_PATH=data/usage.duckdb\n" +
		"PROXY_LISTEN_ADDR=127.0.0.1:8989\nCODEX_FINGERPRINT_AUTO_UPDATE=0\nPOOL_CREDENTIAL_KEY=" + secret +
		"\nADMIN_TOKEN=" + secret + "\nPOOL_JWT_SECRET=" + secret + "\nPOOL_AUTH_ENCRYPTION_KEY=" + secret + "\n"
	for _, tc := range []struct {
		name, file, content string
		args, env           []string
		want                string
		fail                bool
	}{
		{"default", ".env", valid, []string{"-check-credentials"}, nil, "0 file(s) decode", false},
		{"custom", "custom file.env", valid, []string{"-env-file", "custom file.env", "-check-credentials"}, nil, "0 file(s) decode", false},
		{"process precedence", ".env", strings.ReplaceAll(valid, "POOL_CREDENTIAL_KEY="+secret, "POOL_CREDENTIAL_KEY=invalid"), []string{"-check-credentials"}, []string{"POOL_CREDENTIAL_KEY=" + secret}, "0 file(s) decode", false},
		{"empty precedence", ".env", valid, []string{"-check-credentials"}, []string{"POOL_CREDENTIAL_KEY="}, "POOL_CREDENTIAL_KEY", true},
		{"disabled", ".env", "BROKEN", []string{"-env-file=", "-check-credentials"}, strings.Split(strings.TrimSpace(valid), "\n"), "0 file(s) decode", false},
		{"missing", "", "", []string{"-check-credentials"}, nil, "environment file", true},
		{"invalid", ".env", valid + "BROKEN\n", []string{"-check-credentials"}, nil, "invalid assignment", true},
		{"help", "", "", []string{"-help"}, nil, "-env-file", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, "pool"), 0700); err != nil {
				t.Fatal(err)
			}
			if tc.file != "" {
				if err := os.WriteFile(filepath.Join(dir, tc.file), []byte(tc.content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			args, _ := json.Marshal(tc.args)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStartupEnvNativeProcess$")
			cmd.Dir = dir
			for _, entry := range os.Environ() {
				name, _, _ := strings.Cut(entry, "=")
				upper := strings.ToUpper(name)
				if strings.HasPrefix(upper, "POOL_") || strings.HasPrefix(upper, "PROXY_") || strings.HasPrefix(upper, "CODEX_") || strings.HasPrefix(upper, "UPSTREAM_") || upper == "ADMIN_TOKEN" || upper == "CONFIG_PATH" || upper == "DUCKDB_PATH" || upper == "DEBUG" {
					continue
				}
				cmd.Env = append(cmd.Env, entry)
			}
			cmd.Env = append(cmd.Env, "CODEX_POOL_ENV_TEST_HELPER=1", "CODEX_POOL_ENV_TEST_ARGS="+string(args))
			cmd.Env = append(cmd.Env, tc.env...)
			output, err := cmd.CombinedOutput()
			if (err != nil) != tc.fail || !strings.Contains(string(output), tc.want) {
				t.Fatalf("startup = %v, output:\n%s", err, output)
			}
			if strings.Contains(string(output), secret) {
				t.Fatal("startup exposed secret")
			}
			if _, err := os.Stat(filepath.Join(dir, "data")); !os.IsNotExist(err) {
				t.Fatal("offline startup unexpectedly created storage")
			}
		})
	}
}
