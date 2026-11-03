package store

// DeploymentsServingName are the deployments of a served name that a batch may
// be sent to, newest first: the active ones and the stopped ones, which serve
// again once started. A deployment being deleted is neither.
func (s *Store) DeploymentsServingName(name string) ([]Deployment, error) {
	var deployments []Deployment
	err := s.reader().Where("served_name = ? AND desired_state IN ?", name, []DeploymentState{DeployActive, DeployStopped}).Order("id desc").Find(&deployments).Error
	return deployments, err
}
