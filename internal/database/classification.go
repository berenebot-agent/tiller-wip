package database

// TableClass classifies a database table as platform-global or tenant-owned.
//
// This inventory is load-bearing: the guard test in classification_test.go
// fails when a table exists in the schema but is not classified here, and when
// a tenant-owned table is missing its account_id column. A new tenant-owned
// table must be added here and given an account_id.
type TableClass int

const (
	// ClassPlatform identifies tables that are not owned by an account
	// (migrations, accounts themselves, admin sessions, platform settings).
	ClassPlatform TableClass = iota
	// ClassTenant identifies tables whose rows belong to exactly one account.
	ClassTenant
)

// TableClassification is the authoritative table inventory for the tenancy
// boundary. Keys are SQLite table names.
var TableClassification = map[string]TableClass{
	// Platform-global.
	"schema_migrations": ClassPlatform,
	"accounts":          ClassPlatform,
	"admin_sessions":    ClassPlatform,
	"platform_settings": ClassPlatform,

	// Tenant-owned. Every one of these must carry account_id.
	"namespaces":               ClassTenant,
	"providers":                ClassTenant,
	"provider_models":          ClassTenant,
	"provider_oauth_tokens":    ClassTenant,
	"virtual_provider_groups":  ClassTenant,
	"virtual_models":           ClassTenant,
	"virtual_model_targets":    ClassTenant,
	"client_keys":              ClassTenant,
	"client_group_defaults":    ClassTenant,
	"client_model_permissions": ClassTenant,
	"client_single_bindings":   ClassTenant,
	"request_logs":             ClassTenant,
	"request_attempts":         ClassTenant,
	"settings":                 ClassTenant,
}

// TenantTables returns the tenant-owned table names.
func TenantTables() []string {
	out := make([]string, 0, len(TableClassification))
	for name, class := range TableClassification {
		if class == ClassTenant {
			out = append(out, name)
		}
	}
	return out
}
