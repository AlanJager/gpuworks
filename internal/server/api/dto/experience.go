package dto

// ExperienceStatus is GET /api/v1/experience: the experience environment
// beside this server, a console of its own holding simulated nodes and data.
type ExperienceStatus struct {
	// Enabled says the environment is running; Ready that its console
	// answers.
	Enabled bool `json:"enabled"`
	Ready   bool `json:"ready"`
	// Port is the environment console's port, on the host this console is
	// reached at.
	Port int    `json:"port"`
	User string `json:"user"`
	// Password is the environment console's own, present while it runs. It
	// opens only the environment, which holds no real data.
	Password string `json:"password,omitempty"`
	// Error is why the environment last stopped on its own.
	Error string `json:"error,omitempty"`
}

// ExperienceEnabled is PUT /api/v1/experience/enabled.
type ExperienceEnabled struct {
	Enabled *bool `json:"enabled"`
}
