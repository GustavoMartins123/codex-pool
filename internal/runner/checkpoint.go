package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"codex-pool-proxy/internal/atomicfile"
	"codex-pool-proxy/internal/runnerproto"
	"github.com/google/uuid"
)

type localCheckpoint struct {
	Version         int                   `json:"version"`
	PoolURL         string                `json:"pool_url"`
	RunnerID        string                `json:"runner_id"`
	SessionID       string                `json:"session_id"`
	Agent           string                `json:"agent"`
	NativeSessionID string                `json:"native_session_id"`
	NativeLogID     string                `json:"native_log_id"`
	Workspace       runnerproto.Workspace `json:"workspace"`
	Path            string                `json:"path"`
	Control         string                `json:"control"`
	Files           []checkpointFile      `json:"files"`
}

const maxCheckpointReceiptBytes = 8 << 20

type checkpointFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func checkpointDigest(raw []byte) string {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func runtimePath(root, prefix, id string) (string, error) {
	if _, err := uuid.Parse(id); err != nil {
		return "", errors.New("invalid checkpoint session ID")
	}
	path := filepath.Join(root, prefix+id)
	if !filepath.IsAbs(root) || filepath.Dir(path) != root {
		return "", errors.New("checkpoint path escaped workspace root")
	}
	return path, nil
}

func verifyCheckpointTree(control, agent, nativeID string, syncFiles bool) ([]checkpointFile, error) {
	if agent != "codex" && agent != "claude" {
		return nil, errors.New("unsupported checkpoint agent")
	}
	found := false
	var files []checkpointFile
	err := filepath.WalkDir(control, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("checkpoint contains a symbolic link")
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("checkpoint contains a non-regular file")
		}
		if filepath.Ext(path) == ".jsonl" && strings.Contains(entry.Name(), nativeID) {
			found = true
		}
		{
			flags := os.O_RDONLY
			if syncFiles {
				flags = os.O_RDWR
			}
			file, err := os.OpenFile(path, flags, 0)
			if err != nil {
				return err
			}
			if syncFiles {
				err = file.Sync()
			}
			if err != nil {
				file.Close()
				return err
			}
			hash := sha256.New()
			_, err = io.Copy(hash, file)
			closeErr := file.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
			rel, err := filepath.Rel(control, path)
			if err != nil {
				return err
			}
			files = append(files, checkpointFile{Path: rel, Size: info.Size(), SHA256: hex.EncodeToString(hash.Sum(nil))})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New("native session transcript is missing from checkpoint")
	}
	return files, nil
}

func saveLocalCheckpoint(options Options, session runnerproto.Session, path, control string, native runnerproto.AgentCheckpoint) (*runnerproto.AgentCheckpoint, error) {
	nativeID := native.NativeSessionID
	if nativeID == "" || len(nativeID) > 128 || strings.ContainsAny(nativeID, "/\\") {
		return nil, errors.New("invalid native session identifier")
	}
	if native.NativeLogID == "" || len(native.NativeLogID) > 128 || strings.ContainsAny(native.NativeLogID, "/\\") {
		return nil, errors.New("invalid native log identifier")
	}
	files, err := verifyCheckpointTree(control, session.Agent, native.NativeLogID, true)
	if err != nil {
		return nil, err
	}
	receipt, err := runtimePath(options.WorkspaceRoot, ".checkpoint-", session.ID)
	if err != nil {
		return nil, err
	}
	receipt += ".json"
	value := localCheckpoint{Version: 1, PoolURL: options.Config.PoolURL, RunnerID: options.Config.RunnerID, SessionID: session.ID, Agent: session.Agent, NativeSessionID: nativeID, NativeLogID: native.NativeLogID, Workspace: session.Workspace, Path: path, Control: control, Files: files}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(raw) > maxCheckpointReceiptBytes {
		return nil, errors.New("native checkpoint manifest exceeds size limit")
	}
	file, err := os.CreateTemp(options.WorkspaceRoot, ".checkpoint-write-")
	if err != nil {
		return nil, err
	}
	name := file.Name()
	defer os.Remove(name)
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return nil, err
	}
	if _, err := file.Write(raw); err != nil {
		file.Close()
		return nil, err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	if err := atomicfile.Publish(name, receipt); err != nil {
		return nil, err
	}
	native.Ready = true
	native.Digest = checkpointDigest(raw)
	return &native, nil
}

func loadLocalCheckpoint(options Options, session runnerproto.Session) (*localCheckpoint, error) {
	if session.Checkpoint == nil || session.Checkpoint.Version != 1 || !session.Checkpoint.Ready {
		return nil, errors.New("resume requires a durable native checkpoint")
	}
	receipt, err := runtimePath(options.WorkspaceRoot, ".checkpoint-", session.ID)
	if err != nil {
		return nil, err
	}
	receipt += ".json"
	info, err := os.Lstat(receipt)
	if err != nil {
		return nil, errors.New("runner checkpoint receipt is unavailable")
	}
	if !info.Mode().IsRegular() || info.Size() > maxCheckpointReceiptBytes {
		return nil, errors.New("invalid checkpoint receipt")
	}
	raw, err := os.ReadFile(receipt)
	if err != nil {
		return nil, err
	}
	if checkpointDigest(raw) != session.Checkpoint.Digest {
		return nil, errors.New("checkpoint receipt checksum mismatch")
	}
	var value localCheckpoint
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	control, err := runtimePath(options.WorkspaceRoot, ".control-", session.ID)
	if err != nil {
		return nil, err
	}
	if value.Version != 1 || value.PoolURL != options.Config.PoolURL || value.RunnerID != options.Config.RunnerID || value.SessionID != session.ID || value.Agent != session.Agent ||
		value.NativeSessionID != session.Checkpoint.NativeSessionID || value.NativeLogID != session.Checkpoint.NativeLogID || value.Workspace != session.Workspace || value.Control != control {
		return nil, errors.New("checkpoint identity does not match the claimed session")
	}
	var path string
	if session.Workspace.Mode == "path" {
		path, err = prepareWorkspace(context.Background(), options.WorkspaceRoot, options.AllowedPaths, session)
	} else {
		path, err = runtimePath(options.WorkspaceRoot, "", session.ID)
		if err == nil {
			var info os.FileInfo
			info, err = os.Lstat(path)
			if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
				err = errors.New("checkpoint workspace is not an owned directory")
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if value.Path != path {
		return nil, errors.New("checkpoint workspace path changed")
	}
	files, err := verifyCheckpointTree(control, session.Agent, value.NativeLogID, false)
	if err != nil {
		return nil, err
	}
	if !slices.Equal(files, value.Files) {
		return nil, errors.New("native checkpoint content changed or is corrupt")
	}
	return &value, nil
}
