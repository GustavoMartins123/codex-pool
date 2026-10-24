package runner

import (
	"errors"
	"os"

	"codex-pool-proxy/internal/runnerproto"
)

func purgeLocalConversation(options Options, session runnerproto.Session) error {
	if session.RunnerID != options.Config.RunnerID || session.State != "closed" {
		return errors.New("conversation purge requires its original runner and closed execution")
	}
	if session.Workspace.Mode != "ephemeral" && session.Workspace.Mode != "path" && session.Workspace.Mode != "repo" {
		return errors.New("invalid workspace mode for deletion")
	}
	control, err := runtimePath(options.WorkspaceRoot, ".control-", session.ID)
	if err != nil {
		return err
	}
	path, err := runtimePath(options.WorkspaceRoot, "", session.ID)
	if err != nil {
		return err
	}
	// User-selected paths and cloned repositories are retained. Only the
	// explicitly owned ephemeral workspace and agent control data are removed.
	if session.Workspace.Mode == "ephemeral" {
		if err := cleanupWorkspace(options.WorkspaceRoot, path, "ephemeral"); err != nil {
			return err
		}
	}
	if err := cleanupWorkspace(options.WorkspaceRoot, control, "ephemeral"); err != nil {
		return err
	}
	receipt, err := runtimePath(options.WorkspaceRoot, ".checkpoint-", session.ID)
	if err != nil {
		return err
	}
	receipt += ".json"
	info, err := os.Lstat(receipt)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("refusing to delete a replaced checkpoint receipt")
	}
	return os.Remove(receipt)
}

func (d *daemon) purge(m runnerproto.Message) {
	result := runnerproto.Frame("session.purged")
	result.SessionID = m.SessionID
	result.Status = "failed"
	if m.Session == nil || m.Session.ID != m.SessionID {
		d.command(result)
		return
	}
	d.mu.Lock()
	s := d.sessions[m.SessionID]
	if s != nil {
		select {
		case <-s.done:
		default:
			d.mu.Unlock()
			d.command(result)
			return
		}
	}
	if err := purgeLocalConversation(d.options, *m.Session); err == nil {
		delete(d.sessions, m.SessionID)
		result.Status = "deleted"
	}
	d.mu.Unlock()
	d.command(result)
}
