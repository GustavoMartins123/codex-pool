package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestRestoreProcessInterruption(t *testing.T) {
	if os.Getenv("RESTORE_CRASH_STEP") != "" {
		restoreCheckpoint = func(step string) {
			if step == os.Getenv("RESTORE_CRASH_STEP") {
				os.Exit(73)
			}
		}
		if os.Getenv("RESTORE_RECOVER_ONLY") == "1" {
			if err := recoverPairedRestore(os.Getenv("RESTORE_BOLT"), os.Getenv("RESTORE_DUCK")); err != nil {
				t.Fatal(err)
			}
		} else if err := restorePairedBackup(os.Getenv("RESTORE_MANIFEST"), os.Getenv("RESTORE_BOLT"), os.Getenv("RESTORE_DUCK")); err != nil {
			t.Fatal(err)
		}
		return
	}
	for _, step := range []string{"prepared", "bolt-swapped", "duck-swapped", "committed"} {
		t.Run(step, func(t *testing.T) {
			manifest, bolt, duck := buildPairedRestoreFixture(t)
			crash := func(checkpoint string, recoverOnly bool) {
				cmd := exec.Command(os.Args[0], "-test.run=^TestRestoreProcessInterruption$")
				cmd.Env = append(os.Environ(), "RESTORE_CRASH_STEP="+checkpoint, "RESTORE_MANIFEST="+manifest, "RESTORE_BOLT="+bolt, "RESTORE_DUCK="+duck)
				if recoverOnly {
					cmd.Env = append(cmd.Env, "RESTORE_RECOVER_ONLY=1")
				}
				if err := cmd.Run(); err == nil {
					t.Fatal("process did not stop at checkpoint")
				} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 73 {
					t.Fatalf("unexpected subprocess error: %v", err)
				}
			}
			crash(step, false)
			if step == "bolt-swapped" || step == "duck-swapped" {
				crash("recovered:"+filepath.Base(bolt), true)
			}
			if err := recoverPairedRestore(bolt, duck); err != nil {
				t.Fatal(err)
			}
			if err := recoverPairedRestore(bolt, duck); err != nil {
				t.Fatal(err)
			}
			want := "after"
			if step == "committed" {
				want = "before"
			}
			if got := readBoltProof(t, bolt); got != want {
				t.Fatalf("Bolt %q, want %q", got, want)
			}
			if got := readDuckProof(t, duck); got != want {
				t.Fatalf("DuckDB %q, want %q", got, want)
			}
			assertNoRestoreLeftovers(t, bolt, duck)
		})
	}
}

func TestRestoreJournalFailsClosed(t *testing.T) {
	_, bolt, duck := buildPairedRestoreFixture(t)
	if err := os.WriteFile(bolt+".restore-journal", []byte(`{"format":1,"files":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := recoverPairedRestore(bolt, duck); err == nil {
		t.Fatal("invalid journal accepted")
	}
	if readBoltProof(t, bolt) != "after" || readDuckProof(t, duck) != "after" {
		t.Fatal("stores changed")
	}
}

func TestRestoreRejectsDamagedSnapshotBeforeChangingEitherStore(t *testing.T) {
	_, bolt, duck := buildPairedRestoreFixture(t)
	for _, path := range []string{bolt, duck} {
		if err := copyFile(path, path+".restore", 0600); err != nil {
			t.Fatal(err)
		}
		if err := copyFile(path, path+".prerestore", 0600); err != nil {
			t.Fatal(err)
		}
	}
	j, err := prepareRestoreJournal(bolt, duck)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeRestoreJournal(bolt+".restore-journal", j); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(duck+".prerestore", []byte("damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := recoverPairedRestore(bolt, duck); err == nil {
		t.Fatal("damaged snapshot accepted")
	}
	if readBoltProof(t, bolt) != "after" || readDuckProof(t, duck) != "after" {
		t.Fatal("stores changed before validation finished")
	}
}
