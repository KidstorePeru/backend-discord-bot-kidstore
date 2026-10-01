package main

import (
	"database/sql"
	"log/slog"
	"strings"

	"KidStoreStore/src/backup"
	"KidStoreStore/src/discordbot"
	"KidStoreStore/src/types"
)

// backupSettings decide si el respaldo diario está configurado. Sin ninguna
// variable BACKUP_* → desactivado en silencio (enabled=false, problem="").
// Con alguna pero no todas, o con una clave corta → problem explica qué
// falta, para avisarlo en vez de quedarse sin respaldos sin que nadie se
// entere.
func backupSettings(cfg types.EnvConfig) (enabled bool, problem string) {
	required := map[string]string{
		"BACKUP_S3_ENDPOINT":          cfg.BackupS3Endpoint,
		"BACKUP_S3_BUCKET":            cfg.BackupS3Bucket,
		"BACKUP_S3_ACCESS_KEY_ID":     cfg.BackupS3AccessKeyID,
		"BACKUP_S3_SECRET_ACCESS_KEY": cfg.BackupS3SecretAccessKey,
		"BACKUP_ENCRYPTION_KEY":       cfg.BackupEncryptionKey,
	}
	var missing []string
	for _, name := range []string{"BACKUP_S3_ENDPOINT", "BACKUP_S3_BUCKET", "BACKUP_S3_ACCESS_KEY_ID", "BACKUP_S3_SECRET_ACCESS_KEY", "BACKUP_ENCRYPTION_KEY"} {
		if strings.TrimSpace(required[name]) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) == len(required) {
		return false, ""
	}
	if len(missing) > 0 {
		return false, "faltan estas variables en Railway: " + strings.Join(missing, ", ")
	}
	if len(cfg.BackupEncryptionKey) < backup.MinKeyLength {
		return false, "BACKUP_ENCRYPTION_KEY debe tener al menos 32 caracteres"
	}
	if cfg.BackupRetentionDays < 7 {
		return false, "BACKUP_RETENTION_DAYS debe ser al menos 7"
	}
	return true, ""
}

func startDatabaseBackups(cfg types.EnvConfig, database *sql.DB) {
	enabled, problem := backupSettings(cfg)
	if problem != "" {
		slog.Error("Respaldo diario NO activado: configuración incompleta", "detalle", problem)
		discordbot.AlertBackupFailed("configuración incompleta: " + problem)
		return
	}
	if !enabled {
		slog.Warn("Respaldo diario desactivado: no hay variables BACKUP_* configuradas")
		return
	}
	store, err := backup.NewS3Store(backup.S3Config{
		Endpoint:        cfg.BackupS3Endpoint,
		Bucket:          cfg.BackupS3Bucket,
		AccessKeyID:     cfg.BackupS3AccessKeyID,
		SecretAccessKey: cfg.BackupS3SecretAccessKey,
		Region:          cfg.BackupS3Region,
	})
	if err != nil {
		slog.Error("Respaldo diario NO activado", "error", err)
		discordbot.AlertBackupFailed(err.Error())
		return
	}
	backup.OnFailure = discordbot.AlertBackupFailed
	backup.OnRecovered = discordbot.ClearBackupFailedAlert
	backup.OnFirstSuccess = func(size int, header backup.Header) {
		discordbot.AlertBackupsWorking(size, header.TotalRows(), cfg.BackupRetentionDays)
	}
	backup.Start(&backup.Job{
		DB:            database,
		Store:         store,
		Passphrase:    cfg.BackupEncryptionKey,
		RetentionDays: cfg.BackupRetentionDays,
	})
}
