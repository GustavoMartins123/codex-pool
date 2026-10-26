package contractgen

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, name, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("test source location unavailable")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(name), "../.."))
}

func contractSource(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repositoryRoot(t), SchemaPath))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestCommittedContractsMatchSchema(t *testing.T) {
	if err := Run(repositoryRoot(t), true); err != nil {
		t.Fatal(err)
	}
}

func TestChangingSharedLimitUpdatesBothLanguages(t *testing.T) {
	s, err := parse(contractSource(t))
	if err != nil {
		t.Fatal(err)
	}
	for i := range s.Constants {
		if s.Constants[i].Go == "MaxCommandSignatures" {
			s.Constants[i].Value = json.RawMessage("17")
		}
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	outputs, err := Render(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(outputs[GoPath], []byte("const MaxCommandSignatures = 17")) ||
		!bytes.Contains(outputs[KotlinPath], []byte("const val MaxCommandSignatures: Int = 17")) {
		t.Fatal("changing the schema did not update both signature limits")
	}
	again, err := Render(raw)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range outputs {
		if !bytes.Equal(want, again[name]) {
			t.Fatalf("nondeterministic generation: %s", name)
		}
	}
}

func TestInvalidSchemaIsRejectedBeforeWritingEitherContract(t *testing.T) {
	for name, mutate := range map[string]func(*schema){
		"unknown version":          func(s *schema) { s.Version++ },
		"duplicate constant":       func(s *schema) { s.Constants = append(s.Constants, s.Constants[0]) },
		"unknown field type":       func(s *schema) { s.Types[0].Fields[0].Type = "MissingType" },
		"duplicate wire field":     func(s *schema) { s.Types[0].Fields = append(s.Types[0].Fields, s.Types[0].Fields[0]) },
		"missing projection field": func(s *schema) { s.Types[0].Kotlin[0].Field = "missing" },
		"invalid default":          func(s *schema) { s.Types[0].Kotlin[1].Default = json.RawMessage("42") },
		"invalid null":             func(s *schema) { s.Types[0].Kotlin[0].Default = json.RawMessage("null") },
		"duplicate enum wire":      func(s *schema) { s.Enums[0].Values[1].Wire = s.Enums[0].Values[0].Wire },
		"unknown event type":       func(s *schema) { s.Events[0].Type = "MissingType" },
		"duplicate event":          func(s *schema) { s.Events = append(s.Events, s.Events[0]) },
	} {
		t.Run(name, func(t *testing.T) {
			s, err := parse(contractSource(t))
			if err != nil {
				t.Fatal(err)
			}
			mutate(&s)
			raw, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "contracts"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, SchemaPath), raw, 0600); err != nil {
				t.Fatal(err)
			}
			if err := Run(root, false); err == nil {
				t.Fatal("invalid schema generated source")
			}
			for _, name := range []string{GoPath, KotlinPath} {
				if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
					t.Fatal("invalid schema partially wrote a contract")
				}
			}
		})
	}
	if _, err := Render(bytes.Replace(contractSource(t), []byte(`"schema_version": 1`), []byte(`"schema_version": 1, "unexpected": true`), 1)); err == nil {
		t.Fatal("unknown schema field was silently ignored")
	}
}

func TestCheckRejectsMissingAndEditedSourcesWithoutRepairingThem(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "contracts"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, SchemaPath), contractSource(t), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Run(root, true); err == nil {
		t.Fatal("missing contracts accepted")
	}
	if err := Run(root, false); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{GoPath, KotlinPath} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(root, name)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			edited := append(append([]byte{}, original...), []byte("\n// hand edit\n")...)
			if err := os.WriteFile(path, edited, 0600); err != nil {
				t.Fatal(err)
			}
			if err := Run(root, true); err == nil || !strings.Contains(err.Error(), "is stale") {
				t.Fatalf("edited contract check = %v", err)
			}
			actual, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(actual, edited) {
				t.Fatal("check mode changed source files")
			}
			if err := Run(root, false); err != nil {
				t.Fatal(err)
			}
			if err := Run(root, true); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNumericDefaultsKeepTheirPrecisionOrFailExplicitly(t *testing.T) {
	got, err := kotlinDefault("long", kotlinField{Default: json.RawMessage("9007199254740993")})
	if err != nil || got != " = 9007199254740993L" {
		t.Fatalf("64-bit default lost precision: %q, %v", got, err)
	}
	for _, value := range []string{"2147483648", "1.5", "1e3"} {
		if _, err := kotlinDefault("int", kotlinField{Default: json.RawMessage(value)}); err == nil {
			t.Fatalf("invalid Int default %s accepted", value)
		}
	}
}
