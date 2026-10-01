// restaurar-respaldo abre una copia de seguridad de la base de datos
// (archivo .ksbk descargado del almacenamiento) y, si se pide, la carga en
// una base de datos Postgres VACÍA.
//
// Solo comprobar que la copia se abre y ver qué contiene (no toca ninguna
// base de datos):
//
//	BACKUP_ENCRYPTION_KEY=... go run ./tools/restaurar-respaldo -archivo copia.ksbk -solo-verificar
//
// Restaurar en una base vacía (crea las tablas y carga los datos; si la
// base ya tiene datos, no hace nada):
//
//	BACKUP_ENCRYPTION_KEY=... RESTORE_DATABASE_URL="postgres://usuario:clave@host:puerto/base?sslmode=require" \
//	    go run ./tools/restaurar-respaldo -archivo copia.ksbk
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"KidStoreStore/src/backup"
	"KidStoreStore/src/db"

	_ "github.com/lib/pq"
)

func main() {
	file := flag.String("archivo", "", "ruta del archivo .ksbk")
	verifyOnly := flag.Bool("solo-verificar", false, "solo abrir la copia y mostrar su contenido, sin tocar ninguna base de datos")
	flag.Parse()

	if *file == "" {
		fail("falta -archivo (ruta del archivo .ksbk)")
	}
	key := os.Getenv("BACKUP_ENCRYPTION_KEY")
	if key == "" {
		fail("falta la variable BACKUP_ENCRYPTION_KEY (la misma clave configurada en Railway)")
	}
	data, err := os.ReadFile(*file)
	if err != nil {
		fail(err.Error())
	}

	header, err := backup.Inspect(data, key)
	if err != nil {
		fail(err.Error())
	}
	printHeader(header)
	if *verifyOnly {
		fmt.Println("\nLa copia está completa y se puede restaurar.")
		return
	}

	url := os.Getenv("RESTORE_DATABASE_URL")
	if url == "" {
		fail("falta la variable RESTORE_DATABASE_URL (la base de datos VACÍA donde restaurar)")
	}
	conn, err := sql.Open("postgres", url)
	if err != nil {
		fail(err.Error())
	}
	defer conn.Close()
	if err := conn.Ping(); err != nil {
		fail("no se pudo conectar a la base de destino: " + err.Error())
	}
	if err := db.CreateTables(conn); err != nil {
		fail("no se pudo crear el esquema: " + err.Error())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	result, err := backup.Restore(ctx, conn, data, key)
	if err != nil {
		fail(err.Error())
	}
	for _, c := range result.SkippedColumns {
		fmt.Println("Aviso: la columna", c, "ya no existe en el esquema actual; se omitió.")
	}
	fmt.Printf("\nListo: se restauraron %d filas.\n", result.Header.TotalRows())
}

func printHeader(h backup.Header) {
	fmt.Printf("Copia del %s (UTC) — %d filas en total\n", h.CreatedAt.Format("2006-01-02 15:04"), h.TotalRows())
	names := make([]string, 0, len(h.Tables))
	for t := range h.Tables {
		names = append(names, t)
	}
	sort.Strings(names)
	for _, t := range names {
		fmt.Printf("  %-32s %d\n", t, h.Tables[t])
	}
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "Error:", msg)
	os.Exit(1)
}
