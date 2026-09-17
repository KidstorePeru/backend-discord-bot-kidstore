package store

// TestMain arranca (una sola vez, para todo el paquete) una Postgres
// embebida y descartable si no hay ya una base de pruebas real configurada
// externamente (TEST_DB_HOST) — ver testdb.StartEmbeddedIfNeeded y el
// comentario al principio de setupShopTestDB (shop_db_test.go) para el
// porqué: nunca conectarse por accidente a la base de producción.

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
