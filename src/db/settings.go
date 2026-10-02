package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Ajustes de la tienda guardados como JSON (los edita el admin desde el panel).

// GetSetting devuelve el valor guardado; ok=false si todavía no existe.
func GetSetting(db *sql.DB, key string, dest any) (ok bool, updatedBy string, updatedAt time.Time, err error) {
	var raw []byte
	err = db.QueryRow(`SELECT value, updated_by, updated_at FROM app_settings WHERE key = $1`, key).Scan(&raw, &updatedBy, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", time.Time{}, nil
	}
	if err != nil {
		return false, "", time.Time{}, err
	}
	return true, updatedBy, updatedAt, json.Unmarshal(raw, dest)
}

// SetSetting guarda (o reemplaza) el valor.
func SetSetting(db *sql.DB, key string, value any, updatedBy string) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO app_settings (key, value, updated_by, updated_at) VALUES ($1, $2, $3, NOW())
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_by = EXCLUDED.updated_by, updated_at = NOW()`,
		key, raw, updatedBy)
	return err
}
