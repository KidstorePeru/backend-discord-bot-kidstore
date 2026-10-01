package main

import (
	"strings"
	"testing"

	"KidStoreStore/src/types"
)

func fullBackupConfig() types.EnvConfig {
	return types.EnvConfig{
		BackupS3Endpoint:        "cuenta.r2.cloudflarestorage.com",
		BackupS3Bucket:          "kidstore-respaldos",
		BackupS3AccessKeyID:     "id",
		BackupS3SecretAccessKey: "secreto",
		BackupEncryptionKey:     strings.Repeat("k", 32),
		BackupRetentionDays:     30,
	}
}

func TestBackupSettings(t *testing.T) {
	if enabled, problem := backupSettings(types.EnvConfig{BackupRetentionDays: 30}); enabled || problem != "" {
		t.Errorf("sin variables BACKUP_*: enabled=%v problem=%q; se esperaba desactivado sin aviso", enabled, problem)
	}
	if enabled, problem := backupSettings(fullBackupConfig()); !enabled || problem != "" {
		t.Errorf("configuración completa: enabled=%v problem=%q", enabled, problem)
	}

	missing := fullBackupConfig()
	missing.BackupS3Bucket = ""
	missing.BackupEncryptionKey = ""
	if enabled, problem := backupSettings(missing); enabled || !strings.Contains(problem, "BACKUP_S3_BUCKET") || !strings.Contains(problem, "BACKUP_ENCRYPTION_KEY") {
		t.Errorf("configuración incompleta: enabled=%v problem=%q", enabled, problem)
	}

	short := fullBackupConfig()
	short.BackupEncryptionKey = "corta"
	if enabled, problem := backupSettings(short); enabled || !strings.Contains(problem, "32") {
		t.Errorf("clave corta: enabled=%v problem=%q", enabled, problem)
	}

	retention := fullBackupConfig()
	retention.BackupRetentionDays = 1
	if enabled, problem := backupSettings(retention); enabled || problem == "" {
		t.Errorf("retención de 1 día: enabled=%v problem=%q", enabled, problem)
	}
}
