package agentupgrade

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

type artifact struct {
	Name   string
	SHA256 string
	Size   int64
}
type manifest map[string]artifact

func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func fingerprint(target string, m manifest) string {
	b, _ := json.Marshal(m)
	return digest(append([]byte(target+"\n"), b...))
}
func (s *Service) seal(name string, data []byte) (artifact, error) {
	a := artifact{Name: name, SHA256: digest(data), Size: int64(len(data))}
	if len(data) == 0 {
		return a, errors.New("agent artifact is empty")
	}
	path := filepath.Join(s.cfg.ArtifactDir, a.SHA256)
	if old, err := os.ReadFile(path); err == nil {
		if digest(old) != a.SHA256 {
			return a, errors.New("sealed agent artifact is corrupt")
		}
		return a, nil
	}
	f, err := os.CreateTemp(s.cfg.ArtifactDir, ".artifact-")
	if err != nil {
		return a, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return a, err
	}
	if err = os.Chmod(tmp, 0o600); err != nil {
		return a, err
	}
	if err = os.Rename(tmp, path); err != nil {
		return a, err
	}
	return a, nil
}
func (s *Service) readArtifact(a artifact) ([]byte, error) {
	if len(a.SHA256) != 64 || strings.ContainsAny(a.SHA256, "/\\.") {
		return nil, errors.New("invalid sealed artifact digest")
	}
	b, err := os.ReadFile(filepath.Join(s.cfg.ArtifactDir, a.SHA256))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) != a.Size || digest(b) != a.SHA256 {
		return nil, errors.New("sealed agent artifact checksum mismatch")
	}
	return b, nil
}
func (s *Service) loadManifest(runID uint) (manifest, error) {
	items, err := s.st.AgentUpgradeItems(runID)
	if err != nil {
		return nil, err
	}
	m := manifest{}
	for _, item := range items {
		if item.ArtifactSHA256 == "" {
			continue
		}
		a := artifact{Name: item.ArtifactName, SHA256: item.ArtifactSHA256, Size: item.ArtifactSize}
		if old, ok := m[a.Name]; ok && old != a {
			return nil, errors.New("inconsistent fixed artifacts for the same platform")
		}
		m[a.Name] = a
	}
	return m, nil
}

// DownloadBinary pins the old fixed /dl path while a legacy operation may
// still fetch it. A completed run does not release an unconfirmed reservation.
func (s *Service) DownloadBinary(name string) ([]byte, bool, error) {
	if name == "" || strings.Contains(name, "..") || strings.ContainsAny(name, "/\\") {
		return nil, false, nil
	}
	runs, err := s.st.AgentUpgradeRuns()
	if err != nil {
		return nil, true, err
	}
	var selected *artifact
	for _, run := range runs {
		items, err := s.st.AgentUpgradeItems(run.ID)
		if err != nil {
			return nil, true, err
		}
		pin := false
		for _, item := range items {
			if item.Transport != "pull" {
				continue
			}
			if activePhase(item.Phase) || (item.Phase == "unconfirmed" && item.VerifiedAt == nil) || (item.Phase == "queued" && run.StopRequestedAt == nil) {
				pin = true
				break
			}
		}
		if !pin {
			continue
		}
		m, err := s.loadManifest(run.ID)
		if err != nil {
			return nil, true, err
		}
		a, ok := m[name]
		if !ok {
			continue
		}
		if run.TargetVersion != s.cfg.TargetVersion || run.InterruptedReason == "artifact_changed" {
			return nil, true, &Error{Code: "AGENT_UPGRADE_TARGET_CHANGED", Detail: "an unresolved legacy upgrade pins this download to another server version or artifact manifest"}
		}
		if selected != nil && selected.SHA256 != a.SHA256 {
			return nil, true, errors.New("conflicting legacy agent upgrade artifacts")
		}
		selected = &a
	}
	if selected == nil {
		return nil, false, nil
	}
	b, err := s.readArtifact(*selected)
	return b, true, err
}

// recoverArtifacts checks the live build only at startup. Hot replacement during
// a run is harmless because all I/O uses sealed bytes; restarting that changed
// build must explicitly interrupt the old run instead of silently resuming it.
func (s *Service) recoverArtifacts() error {
	items, err := s.st.AgentUpgradePendingItems()
	if err != nil {
		return err
	}
	runs := map[uint]bool{}
	for _, item := range items {
		runs[item.RunID] = true
	}
	for id := range runs {
		run, err := s.st.AgentUpgradeRun(id)
		if err != nil {
			return err
		}
		if run.TargetVersion != s.cfg.TargetVersion || run.InterruptedReason != "" {
			continue
		}
		m, err := s.loadManifest(id)
		if err != nil {
			return err
		}
		for name, fixed := range m {
			var live []byte
			var readErr error
			if s.cfg.ReadBinary != nil {
				live, readErr = s.cfg.ReadBinary(name)
			}
			if s.cfg.ReadBinary == nil || readErr != nil || int64(len(live)) != fixed.Size || digest(live) != fixed.SHA256 {
				if err := s.st.InterruptAgentUpgrade(id, "artifact_changed"); err != nil {
					return err
				}
				break
			}
		}
	}
	return nil
}
