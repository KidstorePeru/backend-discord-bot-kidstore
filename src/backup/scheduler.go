package backup

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"KidStoreStore/src/safe"
)

const (
	keyPrefix = "kidstore-backups/"
	keySuffix = ".ksbk"
	keyLayout = "2006-01-02T150405Z"
	// Se hace una copia cuando la última tiene más de esto. Se revisa cada
	// hora (no "cada 24 h desde el arranque"): Railway reinicia el servidor
	// en cada push y, si no, el respaldo podría no llegar a hacerse nunca.
	backupEvery = 23 * time.Hour
	// Se conservan siempre las últimas minKeep copias, aunque sean más
	// viejas que la retención (por si los respaldos fallan varios días).
	minKeep = 7
)

// Hooks de aviso (Discord). Variables para poder reemplazarlas en pruebas.
var (
	OnFailure      = func(reason string) {}
	OnFirstSuccess = func(sizeBytes int, header Header) {}
	OnRecovered    = func() {}
)

// Job hace un respaldo cuando toca y borra los que ya vencieron.
type Job struct {
	DB            *sql.DB
	Store         ObjectStore
	Passphrase    string
	RetentionDays int
	failing       bool
}

func objectKey(t time.Time) string {
	return keyPrefix + "kidstore-" + t.UTC().Format(keyLayout) + keySuffix
}

// objectTime saca la fecha del nombre del archivo (y, si no se puede, usa
// la fecha de modificación que da el almacenamiento).
func objectTime(o Object) time.Time {
	name := strings.TrimSuffix(strings.TrimPrefix(o.Key, keyPrefix+"kidstore-"), keySuffix)
	if t, err := time.Parse(keyLayout, name); err == nil {
		return t
	}
	return o.LastModified
}

// RunOnce hace el respaldo si la última copia tiene más de 23 horas.
// Devuelve true si subió una copia nueva.
func (j *Job) RunOnce(ctx context.Context, now time.Time) (bool, error) {
	objects, err := j.Store.List(ctx, keyPrefix)
	if err != nil {
		return false, j.fail(fmt.Errorf("no se pudo leer el almacenamiento: %w", err))
	}
	backups := objects[:0]
	for _, o := range objects {
		if strings.HasSuffix(o.Key, keySuffix) {
			backups = append(backups, o)
		}
	}
	sort.Slice(backups, func(a, b int) bool { return objectTime(backups[a]).After(objectTime(backups[b])) })
	if len(backups) > 0 && now.Sub(objectTime(backups[0])) < backupEvery {
		return false, nil
	}

	plain, header, err := Dump(ctx, j.DB, now)
	if err != nil {
		return false, j.fail(fmt.Errorf("no se pudo leer la base de datos: %w", err))
	}
	data, err := Encrypt(plain, j.Passphrase)
	if err != nil {
		return false, j.fail(fmt.Errorf("no se pudo cifrar: %w", err))
	}
	// Antes de subirla, se comprueba que la copia se puede abrir con la
	// clave y trae todas las filas: un respaldo que no se puede restaurar
	// no sirve de nada.
	if check, err := Inspect(data, j.Passphrase); err != nil || check.TotalRows() != header.TotalRows() {
		return false, j.fail(fmt.Errorf("la copia generada no pasó la verificación: %v", err))
	}
	key := objectKey(now)
	if err := j.Store.Put(ctx, key, data); err != nil {
		return false, j.fail(fmt.Errorf("no se pudo subir la copia: %w", err))
	}
	slog.Info("Respaldo: copia de la base de datos guardada", "archivo", key, "bytes", len(data), "filas", header.TotalRows())

	if len(backups) == 0 {
		OnFirstSuccess(len(data), header)
	}
	if j.failing {
		j.failing = false
		OnRecovered()
	}

	// Retención: se borran las copias más viejas que RetentionDays, pero
	// siempre quedan al menos minKeep (contando la recién subida).
	all := append([]Object{{Key: key, LastModified: now}}, backups...)
	cutoff := now.AddDate(0, 0, -j.RetentionDays)
	for i, o := range all {
		if i < minKeep || !objectTime(o).Before(cutoff) {
			continue
		}
		if err := j.Store.Delete(ctx, o.Key); err != nil {
			slog.Warn("Respaldo: no se pudo borrar una copia vieja", "archivo", o.Key, "error", err)
		} else {
			slog.Info("Respaldo: copia vieja borrada", "archivo", o.Key)
		}
	}
	return true, nil
}

func (j *Job) fail(err error) error {
	j.failing = true
	slog.Error("Respaldo: falló la copia de seguridad", "error", err)
	OnFailure(err.Error())
	return err
}

// Start revisa cada hora si toca hacer el respaldo (el primero, unos minutos
// después de arrancar, para no competir con el arranque del servidor).
func Start(j *Job) {
	go func() {
		time.Sleep(3 * time.Minute)
		for {
			safe.Run("backup.RunOnce", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
				defer cancel()
				j.RunOnce(ctx, time.Now())
			})
			time.Sleep(time.Hour)
		}
	}()
	slog.Info("Respaldo diario de la base de datos activado", "retencion_dias", j.RetentionDays)
}
