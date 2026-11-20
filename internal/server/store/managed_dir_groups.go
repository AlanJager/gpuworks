package store

import (
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// ReleasedManagedDirGroups answers an agent's hello (proto.Hello.ManagedDirGroups)
// with the groups whose deployment is gone: no deployment has the group's id,
// or the one that does was created at another second, so the directory
// belonged to an earlier deployment of that id (a reinstalled server, a
// restored backup). A deployment that is being deleted still exists and keeps
// its groups until its row goes. A key that is not a group key is not the
// platform's and is never named. One read of the named ids, however many
// deployments exist.
func (s *Store) ReleasedManagedDirGroups(groups []string) ([]string, error) {
	type held struct {
		key     string
		id      uint
		created int64
	}
	var keys []held
	var ids []uint
	for _, key := range groups {
		id, created, ok := proto.ParseManagedDirGroupKey(key)
		if !ok {
			continue
		}
		keys = append(keys, held{key: key, id: id, created: created})
		ids = append(ids, id)
	}
	if len(keys) == 0 {
		return nil, nil
	}
	createdByID := make(map[uint]int64, len(ids))
	const chunk = 500
	for start := 0; start < len(ids); start += chunk {
		end := min(start+chunk, len(ids))
		var rows []struct {
			ID        uint
			CreatedAt time.Time
		}
		if err := s.db.Model(&Deployment{}).Select("id", "created_at").Where("id IN ?", ids[start:end]).Find(&rows).Error; err != nil {
			return nil, err
		}
		for _, row := range rows {
			createdByID[row.ID] = row.CreatedAt.Unix()
		}
	}
	var released []string
	for _, key := range keys {
		if created, live := createdByID[key.id]; !live || created != key.created {
			released = append(released, key.key)
		}
	}
	return released, nil
}
