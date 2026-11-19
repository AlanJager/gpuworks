package store

import "gorm.io/gorm"

// providerInstanceHPCColumns are the columns that say which HPC cluster a
// cloud instance is a compute node of (ProviderInstance.HPCClusterID…).
var providerInstanceHPCColumns = []string{"HPCClusterID", "HPCClusterName", "HPCScheduler", "HPCSchedulerVersion"}

// addProviderInstanceHPCCluster adds them. Rows synced before it read as
// instances of no HPC cluster until the account's next sync says otherwise.
func addProviderInstanceHPCCluster(db *gorm.DB) error {
	if !db.Migrator().HasTable(&ProviderInstance{}) {
		return nil
	}
	for _, col := range providerInstanceHPCColumns {
		if db.Migrator().HasColumn(&ProviderInstance{}, col) {
			continue
		}
		if err := db.Migrator().AddColumn(&ProviderInstance{}, col); err != nil {
			return err
		}
	}
	return nil
}
