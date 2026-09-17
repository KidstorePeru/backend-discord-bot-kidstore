package db

// TestMain arranca (una sola vez, para todo el paquete) una Postgres
// embebida y descartable si no hay ya una base de pruebas real configurada
// externamente (TEST_DB_HOST) — ver testdb.StartEmbeddedIfNeeded y el
// comentario grande al principio de db_test.go para el porqué de todo
// esto: nunca conectarse por accidente a la base de producción. Si por
// cualquier motivo la Postgres embebida no puede arrancar (sin red,
// binario no disponible para esta plataforma, etc.), las pruebas
// simplemente se siguen salteando como ya hacían antes de este archivo.

import (
	"fmt"
	"os"
	"testing"

	"KidStoreStore/src/testdb"
)

func TestMain(m *testing.M) {
	cleanup, err := testdb.StartEmbeddedIfNeeded()
	if err != nil {
		fmt.Fprintln(os.Stderr, "aviso: no se pudo iniciar Postgres embebida para pruebas — las pruebas de integración se saltarán:", err)
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}
