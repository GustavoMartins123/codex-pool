package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"unicode/utf8"
)

const maxStartupEnvBytes = 1 << 20

type envAssignment struct {
	key, value string
}

func registerStartupFlags(fs *flag.FlagSet, cfg *config) *string {
	envPath := fs.String("env-file", ".env", "environment file (empty uses only the process environment)")
	fs.StringVar(&cfg.listenAddr, "listen", cfg.listenAddr, "listen address")
	fs.StringVar(&cfg.backupDir, "backup-dir", "", "create an offline paired Bolt/DuckDB backup in this directory, then exit")
	fs.StringVar(&cfg.restoreManifest, "restore-manifest", "", "restore Bolt/DuckDB from a paired backup manifest, then exit")
	fs.BoolVar(&cfg.decryptCredentials, "decrypt-credentials", false, "decrypt every encrypted credential file back to plaintext (requires POOL_CREDENTIAL_KEY), then exit")
	fs.BoolVar(&cfg.checkCredentials, "check-credentials", false, "verify every credential file decodes with POOL_CREDENTIAL_KEY (offline integrity check), then exit")
	fs.BoolVar(&cfg.rotatePassportKey, "rotate-passport-key", false, "rotate sealed client tokens in the offline Bolt database using POOL_AUTH_ENCRYPTION_KEY_OLD and POOL_AUTH_ENCRYPTION_KEY, then exit")
	return envPath
}

func prepareStartupEnvironment(args []string, output io.Writer) error {
	fs := flag.NewFlagSet("codex-pool", flag.ContinueOnError)
	fs.SetOutput(output)
	path := registerStartupFlags(fs, &config{listenAddr: "127.0.0.1:8989"})
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	return loadStartupEnv(*path)
}

func loadStartupEnv(path string) error {
	if path == "" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("environment file %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("environment file %q: must be a regular file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("environment file %q: %w", path, err)
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil {
		return fmt.Errorf("environment file %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("environment file %q: must be a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxStartupEnvBytes+1))
	if err != nil {
		return fmt.Errorf("environment file %q: %w", path, err)
	}
	entries, err := parseStartupEnv(data, runtime.GOOS == "windows")
	if err != nil {
		return fmt.Errorf("environment file %q: %w", path, err)
	}
	if err := importStartupEnv(entries, os.LookupEnv, os.Setenv, os.Unsetenv); err != nil {
		return fmt.Errorf("environment file %q: %w", path, err)
	}
	return nil
}

func parseStartupEnv(data []byte, foldKeys bool) ([]envAssignment, error) {
	if len(data) > maxStartupEnvBytes {
		return nil, errors.New("exceeds 1 MiB limit")
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return nil, errors.New("must be UTF-8 without NUL bytes")
	}
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	seen := make(map[string]bool)
	var entries []envAssignment
	for index, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		line = strings.Trim(line, " \t")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fail := func(reason string) ([]envAssignment, error) {
			return nil, fmt.Errorf("line %d: %s", index+1, reason)
		}
		if strings.ContainsRune(line, '\r') {
			return fail("unexpected carriage return")
		}
		if strings.HasPrefix(line, "export ") || strings.HasPrefix(line, "export\t") {
			line = strings.TrimLeft(line[6:], " \t")
		}
		key, raw, ok := strings.Cut(line, "=")
		key = strings.Trim(key, " \t")
		if !ok || !validEnvKey(key) {
			return fail("invalid assignment or variable name")
		}
		identity := key
		if foldKeys {
			identity = strings.ToUpper(key)
		}
		if seen[identity] {
			return fail("duplicate variable")
		}
		value, err := parseStartupEnvValue(strings.Trim(raw, " \t"))
		if err != nil {
			return fail(err.Error())
		}
		seen[identity] = true
		entries = append(entries, envAssignment{key, value})
	}
	return entries, nil
}

func validEnvKey(key string) bool {
	if key == "" {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		if c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && c >= '0' && c <= '9' {
			continue
		}
		return false
	}
	return true
}

func parseStartupEnvValue(raw string) (string, error) {
	var value string
	if len(raw) > 0 && (raw[0] == '\'' || raw[0] == '"') {
		quote := raw[0]
		var out strings.Builder
		closed := false
		for i := 1; i < len(raw); i++ {
			c := raw[i]
			if c == quote {
				rest := strings.Trim(raw[i+1:], " \t")
				if rest != "" && !strings.HasPrefix(rest, "#") {
					return "", errors.New("unexpected text after quoted value")
				}
				closed = true
				break
			}
			if quote == '"' && c == '\\' {
				i++
				if i >= len(raw) {
					return "", errors.New("unfinished escape")
				}
				switch raw[i] {
				case '\\', '"':
					c = raw[i]
				case 'n':
					c = '\n'
				case 'r':
					c = '\r'
				case 't':
					c = '\t'
				default:
					return "", errors.New("unsupported escape")
				}
			}
			out.WriteByte(c)
		}
		if !closed {
			return "", errors.New("unterminated quoted value")
		}
		value = out.String()
	} else {
		for i := 0; i < len(raw); i++ {
			if raw[i] == '#' && (i == 0 || raw[i-1] == ' ' || raw[i-1] == '\t') {
				raw = raw[:i]
				break
			}
		}
		value = strings.TrimRight(raw, " \t")
		if strings.ContainsAny(value, "'\"`") {
			return "", errors.New("quotes must enclose the entire value")
		}
	}
	for i := 0; i+1 < len(value); i++ {
		if value[i] == '$' && (value[i+1] == '{' || value[i+1] == '(' || validEnvKey(value[i+1:i+2])) {
			return "", errors.New("variable references and command substitution are unsupported")
		}
	}
	if strings.ContainsRune(value, '`') {
		return "", errors.New("command substitution is unsupported")
	}
	return value, nil
}

func importStartupEnv(entries []envAssignment, lookup func(string) (string, bool), set func(string, string) error, unset func(string) error) error {
	var imported []string
	for _, entry := range entries {
		if _, exists := lookup(entry.key); exists {
			continue
		}
		if err := set(entry.key, entry.value); err != nil {
			failure := errors.New("cannot set environment variable")
			for i := len(imported) - 1; i >= 0; i-- {
				if err := unset(imported[i]); err != nil {
					failure = errors.Join(failure, errors.New("cannot roll back environment variable"))
				}
			}
			return failure
		}
		imported = append(imported, entry.key)
	}
	return nil
}
