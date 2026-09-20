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

// TableClassification is the authoritative table inventory for the central
// (control-plane) database's tenancy boundary. Keys are SQLite table names.
var TableClassification = map[string]TableClass{
	// Platform-global.
	"schema_migrations":         ClassPlatform,
	"accounts":                  ClassPlatform,
	"admin_sessions":            ClassPlatform,
	"platform_settings":         ClassPlatform,
	"users":                     ClassPlatform,
	"user_sessions":             ClassPlatform,
	"email_verification_tokens": ClassPlatform,
	"password_reset_tokens":     ClassPlatform,
	"platform_admin_sessions":   ClassPlatform,

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
	"settings":                 ClassTenant,
}

// ActivityTableClassification is the table inventory for the separate Activity
// database. Activity tables live outside the central database so the central
// backup never contains logs; they are still account-scoped.
var ActivityTableClassification = map[string]TableClass{
	"activity_schema_migrations": ClassPlatform,
	"request_logs":               ClassTenant,
	"request_attempts":           ClassTenant,
}

// MainTenantTables returns the tenant-owned table names in the central
// database.
func MainTenantTables() []string {
	out := make([]string, 0, len(TableClassification))
	for name, class := range TableClassification {
		if class == ClassTenant {
			out = append(out, name)
		}
	}
	return out
}

// ActivityTenantTables returns the tenant-owned table names in the Activity
// database.
func ActivityTenantTables() []string {
	out := make([]string, 0, len(ActivityTableClassification))
	for name, class := range ActivityTableClassification {
		if class == ClassTenant {
			out = append(out, name)
		}
	}
	return out
}

// AuditTableClassification is the table inventory for the central audit
// database (one file shared by every account). Account audit events are
// tenant-owned and carry account_id; platform audit events are global.
var AuditTableClassification = map[string]TableClass{
	"audit_schema_migrations": ClassPlatform,
	"audit_meta":              ClassPlatform,
	"account_audit_events":    ClassTenant,
	"platform_audit_events":   ClassPlatform,
}

// AuditTenantTables returns the tenant-owned table names in the audit database.
func AuditTenantTables() []string {
	out := make([]string, 0, len(AuditTableClassification))
	for name, class := range AuditTableClassification {
		if class == ClassTenant {
			out = append(out, name)
		}
	}
	return out
}

// TenantTables returns every tenant-owned table name across the central,
// Activity, and audit databases. The SQL-boundary guard uses this union so
// tenant SQL cannot escape internal/store regardless of which database it
// targets.
func TenantTables() []string {
	out := append(MainTenantTables(), ActivityTenantTables()...)
	out = append(out, AuditTenantTables()...)
	return out
}
