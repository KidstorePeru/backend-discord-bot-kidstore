// Package testdb arranca, solo para pruebas de integración, una instancia
// de Postgres REAL pero embebida, descartable y aislada — nunca la base de
// datos de producción. Existe porque src/db/db_test.go y
// src/store/shop_db_test.go exigen TEST_DB_* configurado (ver el
// comentario al principio de db_test.go) y, sin una base de pruebas real
// disponible en el entorno, esas pruebas simplemente se saltan.
//
// Si TEST_DB_HOST ya está configurado externamente (por ejemplo, una base
// de pruebas real provista en CI), este paquete no hace nada — respeta esa
// configuración tal cual en vez de arrancar algo redundante.
package testdb

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
)

// StartEmbeddedIfNeeded arranca una Postgres embebida en un puerto libre,
// con un directorio de datos descartable en el directorio temporal del
// sistema, y deja las variables TEST_DB_* apuntando a ella — SOLO si
// TEST_DB_HOST no estaba ya configurado. Devuelve una función de limpieza
// que hay que llamar siempre (defer) para apagar el proceso y borrar sus
// archivos temporales; si no hizo falta arrancar nada, la limpieza no hace
// nada.
func StartEmbeddedIfNeeded() (cleanup func(), err error) {
	if os.Getenv("TEST_DB_HOST") != "" {
		return func() {}, nil
	}

	port, err := freePort()
	if err != nil {
		return func() {}, fmt.Errorf("no se pudo encontrar un puerto libre: %w", err)
	}

	runID := uuid.New().String()[:8]
	dataDir := filepath.Join(os.TempDir(), "kidstore-embedded-pg-data-"+runID)
	runtimeDir := filepath.Join(os.TempDir(), "kidstore-embedded-pg-runtime-"+runID)

	pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Port(uint32(port)).
		Username("kidstore_test").
		Password("kidstore_test").
		Database("kidstore_test").
		DataPath(dataDir).
		RuntimePath(runtimeDir).
		StartTimeout(90 * time.Second).
		Logger(io.Discard))

	if startErr := pg.Start(); startErr != nil {
		os.RemoveAll(dataDir)
		os.RemoveAll(runtimeDir)
		return func() {}, fmt.Errorf("no se pudo iniciar Postgres embebida: %w", startErr)
	}

	os.Setenv("TEST_DB_HOST", "localhost")
	os.Setenv("TEST_DB_PORT", fmt.Sprintf("%d", port))
	os.Setenv("TEST_DB_USER", "kidstore_test")
	os.Setenv("TEST_DB_PASSWORD", "kidstore_test")
	os.Setenv("TEST_DB_NAME", "kidstore_test")

	return func() {
		pg.Stop()
		os.RemoveAll(dataDir)
		os.RemoveAll(runtimeDir)
	}, nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
