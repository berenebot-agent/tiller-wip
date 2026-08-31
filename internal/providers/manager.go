package providers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sync"
	"time"

	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/id"
)

type Manager struct {
	db       *sql.DB
	registry *Registry
	mu       sync.Mutex
	locks    map[string]*sync.Mutex
}

func NewManager(db *sql.DB, registry *Registry) *Manager {
	return &Manager{db: db, registry: registry, locks: make(map[string]*sync.Mutex)}
}

func (m *Manager) Registry() *Registry { return m.registry }

func (m *Manager) Refresh(ctx context.Context, providerID string) error {
	lock := m.providerLock(providerID)
	lock.Lock()
	defer lock.Unlock()
	provider, err := m.loadProvider(ctx, providerID)
	if err != nil {
		return err
	}
	models, discoverErr := m.registry.Discover(ctx, provider)
	if discoverErr != nil {
		_, storeErr := m.db.ExecContext(ctx, `UPDATE providers SET last_refresh_error=?,updated_at=? WHERE id=?`, safeRefreshError(discoverErr), database.Now(), providerID)
		if storeErr != nil {
			return storeErr
		}
		return discoverErr
	}
	if err := m.applyCatalogue(ctx, providerID, models); err != nil {
		return err
	}
	return nil
}

func (m *Manager) loadProvider(ctx context.Context, providerID string) (Instance, error) {
	var p Instance
	var protocols string
	err := m.db.QueryRowContext(ctx, `SELECT id,name,type,base_url,coalesce(credential_secret,''),enabled,protocols FROM providers WHERE id=?`, providerID).
		Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &p.Credential, &p.Enabled, &protocols)
	p.Protocols = DecodeProtocols(protocols)
	return p, err
}

func (m *Manager) applyCatalogue(ctx context.Context, providerID string, models []Model) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := database.Now()
	seen := make(map[string]bool, len(models))
	for _, model := range models {
		if model.ID == "" || seen[model.ID] {
			continue
		}
		seen[model.ID] = true
		var modelID string
		err := tx.QueryRowContext(ctx, `SELECT id FROM provider_models WHERE provider_id=? AND upstream_model_id=?`, providerID, model.ID).Scan(&modelID)
		if err == sql.ErrNoRows {
			modelID, err = id.New()
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO provider_models(id,provider_id,upstream_model_id,display_name,context_length,max_output_tokens,native_protocol,supports_tools,supports_vision,supports_reasoning,supports_structured_output,input_modalities,output_modalities,available,first_seen_at,last_seen_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,1,?,?,?,?)`, modelID, providerID, model.ID, model.DisplayName, nullableInt(model.ContextLength), nullableInt(model.MaxOutputTokens), nullableProtocol(model.NativeProtocol), nullableBool(model.SupportsTools), nullableBool(model.SupportsVision), nullableBool(model.SupportsReasoning), nullableBool(model.SupportsStructuredOutput), nullableJSON(model.InputModalities), nullableJSON(model.OutputModalities), now, now, now, now); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO client_model_permissions(client_key_id,model_kind,model_id,enabled,created_at,updated_at)
				SELECT c.id,'real',?,coalesce(d.new_models_enabled,0),?,? FROM client_keys c LEFT JOIN client_group_defaults d ON d.client_key_id=c.id AND d.group_kind='real' AND d.group_id=?`, modelID, now, now, providerID); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if _, err = tx.ExecContext(ctx, `UPDATE provider_models SET display_name=?,context_length=?,max_output_tokens=?,native_protocol=?,supports_tools=?,supports_vision=?,supports_reasoning=?,supports_structured_output=?,input_modalities=?,output_modalities=?,available=1,last_seen_at=?,updated_at=? WHERE id=?`, model.DisplayName, nullableInt(model.ContextLength), nullableInt(model.MaxOutputTokens), nullableProtocol(model.NativeProtocol), nullableBool(model.SupportsTools), nullableBool(model.SupportsVision), nullableBool(model.SupportsReasoning), nullableBool(model.SupportsStructuredOutput), nullableJSON(model.InputModalities), nullableJSON(model.OutputModalities), now, now, modelID); err != nil {
			return err
		}
	}
	if len(seen) == 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE provider_models SET available=0,updated_at=? WHERE provider_id=?`, now, providerID); err != nil {
			return err
		}
	} else {
		rows, err := tx.QueryContext(ctx, `SELECT id,upstream_model_id FROM provider_models WHERE provider_id=? AND available=1`, providerID)
		if err != nil {
			return err
		}
		var retire []string
		for rows.Next() {
			var modelID, upstream string
			if err := rows.Scan(&modelID, &upstream); err != nil {
				rows.Close()
				return err
			}
			if !seen[upstream] {
				retire = append(retire, modelID)
			}
		}
		rows.Close()
		for _, modelID := range retire {
			if _, err := tx.ExecContext(ctx, `UPDATE provider_models SET available=0,updated_at=? WHERE id=?`, now, modelID); err != nil {
				return err
			}
		}
	}
	next := time.Now().UTC().Add(24*time.Hour + refreshJitter(providerID)).Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `UPDATE providers SET last_refresh_at=?,next_refresh_at=?,last_refresh_error=NULL,updated_at=? WHERE id=?`, now, next, now, providerID); err != nil {
		return err
	}
	return tx.Commit()
}

func (m *Manager) StartScheduler(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Minute)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.refreshDue(ctx)
			}
		}
	}()
}

func (m *Manager) refreshDue(ctx context.Context) {
	rows, err := m.db.QueryContext(ctx, `SELECT id FROM providers WHERE enabled=1 AND (next_refresh_at IS NULL OR next_refresh_at<=?)`, database.Now())
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var providerID string
		if rows.Scan(&providerID) == nil {
			ids = append(ids, providerID)
		}
	}
	rows.Close()
	for _, providerID := range ids {
		go func(value string) {
			refreshCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
			defer cancel()
			_ = m.Refresh(refreshCtx, value)
		}(providerID)
	}
}

func (m *Manager) providerLock(providerID string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	lock := m.locks[providerID]
	if lock == nil {
		lock = &sync.Mutex{}
		m.locks[providerID] = lock
	}
	return lock
}

func refreshJitter(providerID string) time.Duration {
	h := fnv.New32a()
	_, _ = h.Write([]byte(providerID))
	return time.Duration(int64(h.Sum32()%7200)-3600) * time.Second
}
func safeRefreshError(err error) string {
	msg := fmt.Sprintf("%v", err)
	if len(msg) > 1000 {
		msg = msg[:1000]
	}
	return msg
}

func nullableInt(v int) any {
	if v <= 0 {
		return nil
	}
	return v
}

func nullableProtocol(v Protocol) any {
	if v == "" {
		return nil
	}
	return string(v)
}

func nullableBool(v *bool) any {
	if v == nil {
		return nil
	}
	if *v {
		return 1
	}
	return 0
}

func nullableJSON(list []string) any {
	if len(list) == 0 {
		return nil
	}
	b, err := json.Marshal(list)
	if err != nil {
		return nil
	}
	return string(b)
}
