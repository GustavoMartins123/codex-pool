// Package contractgen generates the runner and mobile wire types from one schema.
package contractgen

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/format"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const SchemaPath = "contracts/runner.json"
const GoPath = "internal/runnerproto/contracts_generated.go"
const KotlinPath = "app/composeApp/src/commonMain/kotlin/com/gustavomartins/codexpool/data/RunnerContracts.generated.kt"

type schema struct {
	Version   int        `json:"schema_version"`
	Constants []constant `json:"constants"`
	Enums     []enum     `json:"enums"`
	Types     []wireType `json:"types"`
	Events    []event    `json:"events"`
}

type constant struct {
	Go     string          `json:"go"`
	Kotlin string          `json:"kotlin"`
	Value  json.RawMessage `json:"value"`
}

type enum struct {
	Name   string      `json:"name"`
	Values []enumValue `json:"values"`
}

type enumValue struct {
	Name string `json:"name"`
	Wire string `json:"wire"`
}

type wireType struct {
	Name       string        `json:"name"`
	KotlinName string        `json:"kotlin_name,omitempty"`
	Fields     []field       `json:"fields"`
	Kotlin     []kotlinField `json:"kotlin,omitempty"`
}

type field struct {
	JSON      string `json:"json"`
	Go        string `json:"go"`
	Type      string `json:"type"`
	Pointer   bool   `json:"pointer,omitempty"`
	OmitEmpty bool   `json:"omit_empty,omitempty"`
}

type kotlinField struct {
	Field    string          `json:"field"`
	Name     string          `json:"name"`
	Nullable bool            `json:"nullable,omitempty"`
	Default  json.RawMessage `json:"default,omitempty"`
}

type event struct {
	Wire string `json:"wire"`
	Name string `json:"name"`
	Type string `json:"type"`
}

var identifier = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)
var wireName = regexp.MustCompile(`^[a-z][a-z0-9_.]*$`)

func validIdentifier(name string) bool {
	if !identifier.MatchString(name) || token.Lookup(name).IsKeyword() {
		return false
	}
	switch name {
	case "as", "break", "class", "continue", "do", "else", "false", "for", "fun", "if", "in", "interface", "is", "null", "object", "package", "return", "super", "this", "throw", "true", "try", "typealias", "typeof", "val", "var", "when", "while":
		return false
	}
	return true
}

func parse(raw []byte) (schema, error) {
	var s schema
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&s); err != nil {
		return s, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return s, errors.New("schema must contain exactly one JSON value")
	}
	if s.Version != 1 {
		return s, fmt.Errorf("unsupported contract schema version %d", s.Version)
	}
	if len(s.Constants) == 0 || len(s.Types) == 0 {
		return s, errors.New("contract must declare constants and types")
	}
	names := map[string]bool{"string": true, "int": true, "long": true, "ulong": true, "boolean": true, "timestamp": true, "signatures": true}
	claim := func(name string) error {
		if !validIdentifier(name) || names[name] {
			return fmt.Errorf("invalid or duplicate name %q", name)
		}
		names[name] = true
		return nil
	}
	for _, e := range s.Enums {
		if err := claim(e.Name); err != nil {
			return s, err
		}
		if len(e.Values) == 0 {
			return s, fmt.Errorf("empty enum %s", e.Name)
		}
		values, wires := map[string]bool{}, map[string]bool{}
		for _, v := range e.Values {
			if !validIdentifier(v.Name) || !wireName.MatchString(v.Wire) || values[v.Name] || wires[v.Wire] {
				return s, fmt.Errorf("invalid or duplicate value in enum %s", e.Name)
			}
			values[v.Name], wires[v.Wire] = true, true
		}
	}
	for _, t := range s.Types {
		if err := claim(t.Name); err != nil {
			return s, err
		}
		if t.KotlinName != "" && !validIdentifier(t.KotlinName) {
			return s, fmt.Errorf("invalid Kotlin type name %q", t.KotlinName)
		}
	}
	goNames, kotlinNames := map[string]bool{}, map[string]bool{"RunnerContract": true, "SessionEvent": true}
	for _, e := range s.Enums {
		kotlinNames[e.Name] = true
		for _, v := range e.Values {
			name := e.Name + v.Name
			if goNames[name] {
				return s, fmt.Errorf("duplicate Go enum constant %s", name)
			}
			goNames[name] = true
		}
	}
	for _, c := range s.Constants {
		if !validIdentifier(c.Go) || !validIdentifier(c.Kotlin) || goNames[c.Go] || kotlinNames[c.Kotlin] {
			return s, fmt.Errorf("invalid or duplicate constant %q", c.Go)
		}
		var v any
		if err := json.Unmarshal(c.Value, &v); err != nil {
			return s, err
		}
		switch v.(type) {
		case string:
		case float64:
			integer, err := strconv.ParseInt(strings.TrimSpace(string(c.Value)), 10, 32)
			if err != nil || integer <= 0 {
				return s, fmt.Errorf("invalid integer constant %s", c.Go)
			}
		default:
			return s, fmt.Errorf("unsupported constant %s", c.Go)
		}
		goNames[c.Go], kotlinNames[c.Kotlin] = true, true
	}
	for _, t := range s.Types {
		if goNames[t.Name] {
			return s, fmt.Errorf("duplicate Go type %s", t.Name)
		}
		goNames[t.Name] = true
		if len(t.Fields) == 0 {
			return s, fmt.Errorf("empty type %s", t.Name)
		}
		fields, members := map[string]field{}, map[string]bool{}
		for _, f := range t.Fields {
			if !wireName.MatchString(f.JSON) || !validIdentifier(f.Go) || members[f.Go] {
				return s, fmt.Errorf("invalid field %s.%s", t.Name, f.Go)
			}
			if _, exists := fields[f.JSON]; exists {
				return s, fmt.Errorf("duplicate wire field %s.%s", t.Name, f.JSON)
			}
			if _, err := s.goType(f.Type); err != nil {
				return s, fmt.Errorf("%s.%s: %w", t.Name, f.Go, err)
			}
			fields[f.JSON], members[f.Go] = f, true
		}
		if len(t.Kotlin) == 0 {
			continue
		}
		name := t.kotlinName()
		if kotlinNames[name] {
			return s, fmt.Errorf("duplicate Kotlin type %s", name)
		}
		kotlinNames[name] = true
		used, projected := map[string]bool{}, map[string]bool{}
		for _, k := range t.Kotlin {
			f, ok := fields[k.Field]
			if !ok || !validIdentifier(k.Name) || used[k.Name] || projected[k.Field] {
				return s, fmt.Errorf("invalid Kotlin projection %s.%s", name, k.Name)
			}
			if _, err := s.kotlinType(f.Type); err != nil {
				return s, err
			}
			if _, err := kotlinDefault(f.Type, k); err != nil {
				return s, fmt.Errorf("%s.%s: %w", name, k.Name, err)
			}
			used[k.Name], projected[k.Field] = true, true
		}
	}
	wires, cases := map[string]bool{}, map[string]bool{}
	for _, e := range s.Events {
		if !wireName.MatchString(e.Wire) || !validIdentifier(e.Name) || wires[e.Wire] || cases[e.Name] || goNames["Event"+e.Name] {
			return s, fmt.Errorf("invalid or duplicate event %q", e.Wire)
		}
		found := false
		for _, t := range s.Types {
			if t.kotlinName() == e.Type && len(t.Kotlin) > 0 {
				found = true
			}
		}
		if !found {
			return s, fmt.Errorf("event %s references unavailable Kotlin type %s", e.Wire, e.Type)
		}
		wires[e.Wire], cases[e.Name] = true, true
		goNames["Event"+e.Name] = true
	}
	return s, nil
}

func (t wireType) kotlinName() string {
	if t.KotlinName != "" {
		return t.KotlinName
	}
	return t.Name
}

func (s schema) goType(name string) (string, error) {
	if strings.HasPrefix(name, "[]") {
		inner, err := s.goType(strings.TrimPrefix(name, "[]"))
		return "[]" + inner, err
	}
	switch name {
	case "string":
		return "string", nil
	case "int":
		return "int", nil
	case "long":
		return "int64", nil
	case "ulong":
		return "uint64", nil
	case "boolean":
		return "bool", nil
	case "timestamp":
		return "time.Time", nil
	case "signatures":
		return "map[string]string", nil
	}
	for _, e := range s.Enums {
		if e.Name == name {
			return "string", nil
		}
	}
	for _, t := range s.Types {
		if t.Name == name {
			return name, nil
		}
	}
	return "", fmt.Errorf("unknown wire type %q", name)
}

func (s schema) kotlinType(name string) (string, error) {
	if strings.HasPrefix(name, "[]") {
		inner, err := s.kotlinType(strings.TrimPrefix(name, "[]"))
		return "List<" + inner + ">", err
	}
	switch name {
	case "string", "timestamp":
		return "String", nil
	case "int":
		return "Int", nil
	case "long":
		return "Long", nil
	case "ulong":
		return "ULong", nil
	case "boolean":
		return "Boolean", nil
	case "signatures":
		return "Map<String, String>", nil
	}
	for _, e := range s.Enums {
		if e.Name == name {
			return name, nil
		}
	}
	for _, t := range s.Types {
		if t.Name == name && len(t.Kotlin) > 0 {
			return t.kotlinName(), nil
		}
	}
	return "", fmt.Errorf("wire type %q has no Kotlin projection", name)
}

func kotlinDefault(kind string, k kotlinField) (string, error) {
	if len(k.Default) == 0 {
		return "", nil
	}
	var v any
	decoder := json.NewDecoder(bytes.NewReader(k.Default))
	decoder.UseNumber()
	if err := decoder.Decode(&v); err != nil {
		return "", err
	}
	if v == nil {
		if !k.Nullable {
			return "", errors.New("null default requires a nullable field")
		}
		return " = null", nil
	}
	value := ""
	switch d := v.(type) {
	case string:
		if kind == "string" || kind == "timestamp" {
			value = kotlinString(d)
		}
	case bool:
		if kind == "boolean" {
			value = strconv.FormatBool(d)
		}
	case json.Number:
		switch kind {
		case "int":
			if n, err := strconv.ParseInt(d.String(), 10, 32); err == nil {
				value = strconv.FormatInt(n, 10)
			}
		case "long":
			if n, err := strconv.ParseInt(d.String(), 10, 64); err == nil {
				value = strconv.FormatInt(n, 10) + "L"
			}
		case "ulong":
			if n, err := strconv.ParseUint(d.String(), 10, 64); err == nil {
				value = strconv.FormatUint(n, 10) + "uL"
			}
		}
	case []any:
		if len(d) == 0 && strings.HasPrefix(kind, "[]") {
			value = "emptyList()"
		}
	case map[string]any:
		if len(d) == 0 && kind == "signatures" {
			value = "emptyMap()"
		}
	}
	if value == "" {
		return "", fmt.Errorf("default does not match %s", kind)
	}
	return " = " + value, nil
}

func kotlinString(s string) string {
	encoded, _ := json.Marshal(s)
	return strings.ReplaceAll(string(encoded), "$", `\$`)
}

// Render validates the schema before producing either language's source.
func Render(raw []byte) (map[string][]byte, error) {
	s, err := parse(raw)
	if err != nil {
		return nil, err
	}
	var goCode, kotlin bytes.Buffer
	fmt.Fprintln(&goCode, "// Code generated by go run ./cmd/contractgen from contracts/runner.json; DO NOT EDIT.\n\npackage runnerproto\n\nimport \"time\"")
	fmt.Fprintln(&kotlin, "// Code generated by go run ./cmd/contractgen from contracts/runner.json; DO NOT EDIT.\n\npackage com.gustavomartins.codexpool.data\n\nimport kotlinx.serialization.SerialName\nimport kotlinx.serialization.Serializable\n\nobject RunnerContract {")
	for _, c := range s.Constants {
		fmt.Fprintf(&goCode, "\nconst %s = %s\n", c.Go, c.Value)
		value, kind := string(c.Value), "Int"
		if bytes.HasPrefix(c.Value, []byte(`"`)) {
			var decoded string
			_ = json.Unmarshal(c.Value, &decoded)
			value, kind = kotlinString(decoded), "String"
		}
		fmt.Fprintf(&kotlin, "    const val %s: %s = %s\n", c.Kotlin, kind, value)
	}
	fmt.Fprintln(&kotlin, "}")
	for _, e := range s.Enums {
		fmt.Fprintf(&kotlin, "\n@Serializable\nenum class %s(val wire: String) {\n", e.Name)
		for _, v := range e.Values {
			fmt.Fprintf(&goCode, "\nconst %s%s = %q\n", e.Name, v.Name, v.Wire)
			fmt.Fprintf(&kotlin, "    @SerialName(%s) %s(%s),\n", kotlinString(v.Wire), v.Name, kotlinString(v.Wire))
		}
		fmt.Fprintln(&kotlin, "}")
	}
	for _, t := range s.Types {
		fmt.Fprintf(&goCode, "\ntype %s struct {\n", t.Name)
		for _, f := range t.Fields {
			typeName, _ := s.goType(f.Type)
			if f.Pointer {
				typeName = "*" + typeName
			}
			tag := f.JSON
			if f.OmitEmpty {
				tag += ",omitempty"
			}
			fmt.Fprintf(&goCode, "%s %s `json:%q`\n", f.Go, typeName, tag)
		}
		fmt.Fprintln(&goCode, "}")
		if len(t.Kotlin) == 0 {
			continue
		}
		fmt.Fprintf(&kotlin, "\n@Serializable\ndata class %s(\n", t.kotlinName())
		for _, k := range t.Kotlin {
			var f field
			for _, candidate := range t.Fields {
				if candidate.JSON == k.Field {
					f = candidate
				}
			}
			typeName, _ := s.kotlinType(f.Type)
			if k.Nullable {
				typeName += "?"
			}
			value, _ := kotlinDefault(f.Type, k)
			fmt.Fprintf(&kotlin, "    @SerialName(%s) val %s: %s%s,\n", kotlinString(f.JSON), k.Name, typeName, value)
		}
		fmt.Fprintln(&kotlin, ")")
	}
	if len(s.Events) > 0 {
		fmt.Fprintln(&kotlin, "\nsealed interface SessionEvent {")
		for _, e := range s.Events {
			fmt.Fprintf(&goCode, "\nconst Event%s = %q\n", e.Name, e.Wire)
			fmt.Fprintf(&kotlin, "    data class %s(val value: %s) : SessionEvent\n", e.Name, e.Type)
		}
		fmt.Fprintln(&kotlin, "}\n\nfun decodeEvent(type: String, data: String): SessionEvent = when (type) {")
		for _, e := range s.Events {
			fmt.Fprintf(&kotlin, "    %s -> SessionEvent.%s(PoolJson.decodeFromString(data))\n", kotlinString(e.Wire), e.Name)
		}
		fmt.Fprintln(&kotlin, "    else -> error(\"Evento de sessão desconhecido: $type\")\n}")
	}
	formatted, err := format.Source(goCode.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format generated Go: %w", err)
	}
	return map[string][]byte{GoPath: formatted, KotlinPath: kotlin.Bytes()}, nil
}

// Run either writes both generated sources or checks them without mutation.
func Run(root string, check bool) error {
	raw, err := os.ReadFile(filepath.Join(root, SchemaPath))
	if err != nil {
		return err
	}
	outputs, err := Render(raw)
	if err != nil {
		return err
	}
	for _, name := range []string{GoPath, KotlinPath} {
		path := filepath.Join(root, name)
		if check {
			actual, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("read generated contract %s: %w", name, err)
			}
			if !bytes.Equal(actual, outputs[name]) {
				return fmt.Errorf("generated contract %s is stale; run go run ./cmd/contractgen", name)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(path, outputs[name], 0644); err != nil {
			return err
		}
	}
	return nil
}
