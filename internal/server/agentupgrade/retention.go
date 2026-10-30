package agentupgrade

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (s *Service) retainHistory() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if !s.lastRetention.IsZero() && now.Sub(s.lastRetention) < time.Hour {
		return nil
	}
	s.lastRetention = now
	if err := s.st.PruneAgentUpgrades(20); err != nil {
		return err
	}
	items, err := s.st.AgentUpgradeItems(0)
	if err != nil {
		return err
	}
	used := map[string]bool{}
	for _, i := range items {
		if i.ArtifactSHA256 != "" {
			used[i.ArtifactSHA256] = true
		}
	}
	entries, err := os.ReadDir(s.cfg.ArtifactDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || used[entry.Name()] {
			continue
		}
		if len(entry.Name()) != 64 && !strings.HasPrefix(entry.Name(), ".artifact-") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if now.Sub(info.ModTime()) < 24*time.Hour {
			continue
		}
		if err := os.Remove(filepath.Join(s.cfg.ArtifactDir, entry.Name())); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
