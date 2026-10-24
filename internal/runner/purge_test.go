package runner

import (
	"codex-pool-proxy/internal/runnerproto"
	"os"
	"path/filepath"
	"testing"
)

func TestExplicitPurgeRemovesOnlyOwnedEphemeralAndControlDirectories(t *testing.T) {
	for _, mode := range []string{"ephemeral", "path", "repo"} {
		t.Run(mode, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			options := Options{WorkspaceRoot: root, Config: Config{RunnerID: "original"}}
			s := runnerproto.Session{ID: "11111111-2222-4333-8444-555555555555", RunnerID: "original", State: "closed", Workspace: runnerproto.Workspace{Mode: mode}}
			path, _ := runtimePath(root, "", s.ID)
			control, _ := runtimePath(root, ".control-", s.ID)
			receipt, _ := runtimePath(root, ".checkpoint-", s.ID)
			for _, dir := range []string{path, control} {
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "data"), []byte("test"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(receipt+".json", []byte("test receipt"), 0600); err != nil {
				t.Fatal(err)
			}
			foreign := options
			foreign.Config.RunnerID = "foreign"
			if err := purgeLocalConversation(foreign, s); err == nil {
				t.Fatal("foreign runner purged files")
			}
			if err := purgeLocalConversation(options, s); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(control); !os.IsNotExist(err) {
				t.Fatal("control data retained after explicit purge")
			}
			if _, err := os.Stat(receipt + ".json"); !os.IsNotExist(err) {
				t.Fatal("receipt retained after explicit purge")
			}
			_, err = os.Stat(path)
			if mode == "ephemeral" && !os.IsNotExist(err) {
				t.Fatal("ephemeral workspace retained")
			}
			if mode != "ephemeral" && err != nil {
				t.Fatal("user workspace or cloned repository deleted")
			}
			if err := purgeLocalConversation(options, s); err != nil {
				t.Fatal("purge retry failed:", err)
			}
		})
	}
}
