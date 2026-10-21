package store

import (
	"database/sql"

	"github.com/unliar/neko-master/neko-master-go/internal/model"
)

// TargetRow 是 backend_configs 的采集视图。
type TargetRow struct {
	ID     int64
	Name   string
	URL    string
	Token  string
	Type   string
	Active bool
	Listen bool
}

// ActiveTargets 返回所有 enabled 的 backend 采集目标。
//
// type=agent 的 backend 由 agent 主动推数据，采集调度器会跳过。
func (d *DB) ActiveTargets() ([]model.Target, error) {
	rows, err := d.sql.Query(`
		SELECT id, name, url, COALESCE(token,''), COALESCE(type,'clash')
		FROM backend_configs
		WHERE enabled = 1
		ORDER BY is_active DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []model.Target{}
	for rows.Next() {
		var t model.Target
		if err := rows.Scan(&t.ID, &t.Name, &t.URL, &t.Token, &t.Type); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListBackends 返回全部 backend 配置。
func (d *DB) ListBackends() ([]TargetRow, error) {
	rows, err := d.sql.Query(`
		SELECT id, name, url, COALESCE(token,''), COALESCE(type,'clash'), enabled, listening
		FROM backend_configs ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []TargetRow{}
	for rows.Next() {
		var t TargetRow
		var enabled, listening int
		if err := rows.Scan(&t.ID, &t.Name, &t.URL, &t.Token, &t.Type, &enabled, &listening); err != nil {
			return nil, err
		}
		t.Active = enabled != 0
		t.Listen = listening != 0
		out = append(out, t)
	}
	return out, rows.Err()
}

// EnsureBackend 若没有任何 backend，创建一条默认记录。
func (d *DB) EnsureBackend(name, url, token string) (int64, error) {
	var n int64
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM backend_configs`).Scan(&n); err != nil {
		return 0, err
	}
	if n > 0 {
		return 0, nil
	}
	res, err := d.sql.Exec(`
		INSERT INTO backend_configs (name, url, token, type, enabled, is_active, listening)
		VALUES (?, ?, ?, 'clash', 1, 1, 1)`, name, url, token)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpsertBackend 按 name 插入或更新。
func (d *DB) UpsertBackend(name, url, token, typ string) (int64, error) {
	var id int64
	err := d.sql.QueryRow(`SELECT id FROM backend_configs WHERE name = ?`, name).Scan(&id)
	if err == sql.ErrNoRows {
		res, err := d.sql.Exec(`
			INSERT INTO backend_configs (name, url, token, type, enabled, is_active, listening)
			VALUES (?, ?, ?, ?, 1, 1, 1)`, name, url, token, typ)
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	}
	if err != nil {
		return 0, err
	}
	_, err = d.sql.Exec(`
		UPDATE backend_configs SET url = ?, token = ?, type = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?`, url, token, typ, id)
	return id, err
}
