package store

// SlurmClusterSource is the part of an entry made from a cloud account's HPC
// cluster that a listing of the account's clusters shows: which entry it is.
type SlurmClusterSource struct {
	ProviderClusterID string
	UUID              string
	Name              string
}

// SlurmClustersFromProvider returns the entries made from a cloud account's
// HPC clusters, by the cloud's cluster ID (idx_slurm_cluster_source). Only the
// columns a listing shows are read, never the enrollment token.
func (s *Store) SlurmClustersFromProvider(providerID uint) (map[string]SlurmClusterSource, error) {
	var rows []SlurmClusterSource
	if err := s.reader().Model(&SlurmCluster{}).Select("provider_cluster_id", "uuid", "name").Where("provider_id = ?", providerID).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]SlurmClusterSource, len(rows))
	for _, c := range rows {
		out[c.ProviderClusterID] = c
	}
	return out, nil
}
