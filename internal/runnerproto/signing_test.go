package runnerproto

import "testing"

// These vectors are shared with the app's CommandSigningTest, so both sides
// of the protocol must keep producing the same bytes.
func testCommandKey() []byte {
	key := make([]byte, CommandKeyBytes)
	for i := range key {
		key[i] = byte(i)
	}
	return key
}

const vectorSession = "5d1d9c3e-9f1a-4f0e-8a51-2f4b7c1d0e11"
const vectorIssuedAt = int64(1761379200000)

func TestCommandSigningVectors(t *testing.T) {
	key := testCommandKey()
	if got := CommandKeyID(key); got != "CYxfwqA9-ufHeg_CwDnJkw" {
		t.Fatalf("key id = %s", got)
	}
	turn := TurnSigningInput(vectorSession, vectorIssuedAt, "codex", "gated", Workspace{Mode: "repo", Target: "https://example.com/r.git"}, "olá; 1:2\nrun tests")
	if string(turn) != "16:codexpool-turn/1;36:"+vectorSession+";13:1761379200000;5:codex;5:gated;4:repo;25:https://example.com/r.git;19:olá; 1:2\nrun tests;" {
		t.Fatalf("turn signing input = %q", turn)
	}
	if got := SignCommand(key, turn); got != "ahaf6ty62ZJHooWFfwoZqQDLZJfcuxdYTJezViYcY18" {
		t.Fatalf("turn signature = %s", got)
	}
	approval := ApprovalSigningInput(vectorSession, "call_7", "approve", vectorIssuedAt, "go test ./...")
	if got := SignCommand(key, approval); got != "9juZdutr3gbV9kd2RiYJDNxNbAYzZECcxn1OWyeXfx8" {
		t.Fatalf("approval signature = %s", got)
	}
}

func TestVerifyCommandRejectsTampering(t *testing.T) {
	key := testCommandKey()
	input := TurnSigningInput(vectorSession, vectorIssuedAt, "claude", "gated", Workspace{Mode: "ephemeral"}, "list files")
	signature := SignCommand(key, input)
	if !VerifyCommand(key, input, signature) {
		t.Fatal("valid signature rejected")
	}
	for _, tampered := range [][]byte{
		TurnSigningInput(vectorSession, vectorIssuedAt, "claude", "auto", Workspace{Mode: "ephemeral"}, "list files"),
		TurnSigningInput(vectorSession, vectorIssuedAt+1, "claude", "gated", Workspace{Mode: "ephemeral"}, "list files"),
		TurnSigningInput("other-session", vectorIssuedAt, "claude", "gated", Workspace{Mode: "ephemeral"}, "list files"),
		TurnSigningInput(vectorSession, vectorIssuedAt, "claude", "gated", Workspace{Mode: "path", Target: "/home"}, "list files"),
		TurnSigningInput(vectorSession, vectorIssuedAt, "claude", "gated", Workspace{Mode: "ephemeral"}, "list files; rm -rf ~"),
	} {
		if VerifyCommand(key, tampered, signature) {
			t.Fatalf("signature accepted for %q", tampered)
		}
	}
	other := testCommandKey()
	other[0] = 0xff
	if VerifyCommand(other, input, signature) || VerifyCommand(key, input, "not-base64!") {
		t.Fatal("signature accepted under the wrong key or encoding")
	}
}

func TestValidCommandSignatures(t *testing.T) {
	id := CommandKeyID(testCommandKey())
	signature := SignCommand(testCommandKey(), []byte("x"))
	if !ValidCommandSignatures(0, nil) || !ValidCommandSignatures(vectorIssuedAt, map[string]string{id: signature}) {
		t.Fatal("well-formed signature sets rejected")
	}
	for _, bad := range []struct {
		at  int64
		set map[string]string
	}{
		{vectorIssuedAt, nil},
		{0, map[string]string{id: signature}},
		{vectorIssuedAt, map[string]string{"short": signature}},
		{vectorIssuedAt, map[string]string{id: signature[:42]}},
		{vectorIssuedAt, map[string]string{id: signature[:42] + "!"}},
	} {
		if ValidCommandSignatures(bad.at, bad.set) {
			t.Fatalf("malformed signature set accepted: %v %v", bad.at, bad.set)
		}
	}
}

func TestCommandSignatureLimitCoversEveryStoredAppKey(t *testing.T) {
	if MaxCommandSignatures != 32 {
		t.Fatal("signature contract no longer covers the app's 32 stored keys")
	}
	for _, count := range []int{1, 16, 17, 32, 33} {
		signatures := map[string]string{}
		for i := 0; i < count; i++ {
			key := []byte{byte(i)}
			signatures[CommandKeyID(key)] = SignCommand(key, []byte("input"))
		}
		if got := ValidCommandSignatures(vectorIssuedAt, signatures); got != (count <= 32) {
			t.Fatalf("signature count %d accepted=%v", count, got)
		}
	}
}
