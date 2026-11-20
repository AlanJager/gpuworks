package store

// NodeSettingSyncRevisions returns the committed revisions of settings carried
// by a full desired-state sync. Notification rows survive setting deletion, so
// the same comparison detects both replacement and clearing of an override.
func (s *Store) NodeSettingSyncRevisions(nodeID uint) ([3]uint64, error) {
	keys := [3]string{
		settingNotifyKey(string(SettingNetwork)),
		settingNotifyKey(string(NodeNetworkSettingKey(nodeID))),
		settingNotifyKey(string(SettingOrphanCleanup)),
	}
	var rows []CommittedNotification
	if err := s.db.Select("request_key", "revision").Where("request_key IN ?", keys[:]).Find(&rows).Error; err != nil {
		return [3]uint64{}, err
	}
	var revisions [3]uint64
	for _, row := range rows {
		for i, key := range keys {
			if row.RequestKey == key {
				revisions[i] = row.Revision
				break
			}
		}
	}
	return revisions, nil
}
