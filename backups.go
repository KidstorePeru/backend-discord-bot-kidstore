package main

import (
	"database/sql"
	"log/slog"
	"strings"

	"KidStoreStore/src/backup"
	"KidStoreStore/src/discordbot"
	"KidStoreStore/src/store"
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

// objectStoreFromConfig crea el cliente del almacenamiento (Cloudflare R2) si
// sus variables están configuradas; si no, devuelve nil.
func objectStoreFromConfig(cfg types.EnvConfig) *backup.S3Store {
	if strings.TrimSpace(cfg.BackupS3Endpoint) == "" || strings.TrimSpace(cfg.BackupS3Bucket) == "" ||
		strings.TrimSpace(cfg.BackupS3AccessKeyID) == "" || strings.TrimSpace(cfg.BackupS3SecretAccessKey) == "" {
		return nil
	}
	s3, err := backup.NewS3Store(backup.S3Config{
		Endpoint:        cfg.BackupS3Endpoint,
		Bucket:          cfg.BackupS3Bucket,
		AccessKeyID:     cfg.BackupS3AccessKeyID,
		SecretAccessKey: cfg.BackupS3SecretAccessKey,
		Region:          cfg.BackupS3Region,
	})
	if err != nil {
		slog.Error("Almacenamiento: configuración inválida", "error", err)
		return nil
	}
	return s3
}

// startManualPayments activa la subida de comprobantes de pago manual: se
// guardan cifrados (ENCRYPTION_KEY) en la carpeta comprobantes/ del mismo
// almacenamiento de los respaldos y se borran a los 30 días.
func startManualPayments(cfg types.EnvConfig, database *sql.DB) {
	approve, reject := store.ManualPaymentDiscordActions(database)
	discordbot.SetManualPaymentActions(approve, reject)
	s3 := objectStoreFromConfig(cfg)
	if s3 == nil || cfg.EncryptionKey == "" {
		store.SetManualPaymentsConfig(nil, "", "", cfg.PublicAPIURL)
		slog.Warn("Comprobantes de pago manual: subida desactivada (falta el almacenamiento BACKUP_S3_* o ENCRYPTION_KEY)")
		return
	}
	store.SetManualPaymentsConfig(s3, cfg.EncryptionKey, cfg.SecretKey, cfg.PublicAPIURL)
	store.StartProofRetention(database)
	slog.Info("Comprobantes de pago manual: subida activada (cifrados, se borran a los 30 días)")
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
	s3 := objectStoreFromConfig(cfg)
	if s3 == nil {
		slog.Error("Respaldo diario NO activado: configuración del almacenamiento inválida")
		discordbot.AlertBackupFailed("configuración del almacenamiento inválida (BACKUP_S3_*)")
		return
	}
	backup.OnFailure = discordbot.AlertBackupFailed
	backup.OnRecovered = discordbot.ClearBackupFailedAlert
	backup.OnFirstSuccess = func(size int, header backup.Header) {
		discordbot.AlertBackupsWorking(size, header.TotalRows(), cfg.BackupRetentionDays)
	}
	backup.Start(&backup.Job{
		DB:            database,
		Store:         s3,
		Passphrase:    cfg.BackupEncryptionKey,
		RetentionDays: cfg.BackupRetentionDays,
	})
}
