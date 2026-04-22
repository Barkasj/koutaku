package rbac

import "time"

// Init creates an in-memory RBACStore, seeds the three system roles
// (Admin, Developer, Viewer) with their default permissions, and returns the store.
// This is the recommended entry-point for standalone or test usage.
func Init() (RBACStore, error) {
	store := NewMemoryStore()
	now := time.Now().UTC()
	for _, role := range SystemRoles(now) {
		if err := store.CreateRole(&role); err != nil {
			return nil, err
		}
	}
	return store, nil
}

// InitWithStore seeds system roles into the provided store.
// Use this when you have a PostgresStore (or other persistent backend) already configured.
func InitWithStore(store RBACStore) error {
	now := time.Now().UTC()
	for _, role := range SystemRoles(now) {
		// Attempt create; if the role already exists, update it to keep
		// permissions in sync with the current code version.
		err := store.CreateRole(&role)
		if err != nil {
			// Likely already exists — try update instead
			if uErr := store.UpdateRole(&role); uErr != nil {
				return uErr
			}
		}
	}
	return nil
}
