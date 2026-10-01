// Package backup hace la copia de seguridad diaria de la base de datos (el
// plan Hobby de Railway no incluye respaldos de Postgres) y la restaura.
//
// Formato: un JSON por línea, comprimido con gzip y cifrado con AES-256-GCM.
// La primera línea es una cabecera con la fecha y cuántas filas tiene cada
// tabla; el resto, una línea por fila ({"t": tabla, "r": fila}). Se guardan
// solo los datos, no el esquema: al restaurar, el esquema lo crea la propia
// app (db.CreateTables), así una copia vieja se puede cargar en un esquema
// más nuevo sin problemas.
package backup

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"
	"golang.org/x/crypto/scrypt"
)

const (
	formatName    = "kidstore-backup"
	formatVersion = 1
	// MinKeyLength — la clave de cifrado es una frase que elige el dueño;
	// corta sería adivinable por fuerza bruta si alguien obtiene el archivo.
	MinKeyLength = 32
)

// magic identifica un archivo de respaldo cifrado (versión 1 del sobre).
var magic = []byte("KSBK\x01")

var (
	ErrWrongKeyOrCorrupt = errors.New("no se pudo descifrar el respaldo: la clave no es la correcta o el archivo está dañado")
	ErrNotABackup        = errors.New("el archivo no es un respaldo de KidStore")
	ErrTargetNotEmpty    = errors.New("la base de datos de destino ya tiene datos: solo se restaura sobre una base vacía")
)

// Header es la primera línea del respaldo.
type Header struct {
	Format    string         `json:"format"`
	Version   int            `json:"version"`
	CreatedAt time.Time      `json:"created_at"`
	Tables    map[string]int `json:"tables"` // filas por tabla
}

// TotalRows suma las filas de todas las tablas.
func (h Header) TotalRows() int {
	n := 0
	for _, c := range h.Tables {
		n += c
	}
	return n
}

type rowLine struct {
	Table string          `json:"t"`
	Row   json.RawMessage `json:"r"`
}

// Dump lee todas las tablas del esquema public en una sola transacción de
// solo lectura (REPEATABLE READ), así la copia es una foto coherente:
// ningún pedido queda a medias entre dos tablas.
func Dump(ctx context.Context, database *sql.DB, now time.Time) ([]byte, Header, error) {
	tx, err := database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, Header{}, err
	}
	defer tx.Rollback()

	tables, err := listTables(ctx, tx)
	if err != nil {
		return nil, Header{}, err
	}

	header := Header{Format: formatName, Version: formatVersion, CreatedAt: now.UTC(), Tables: map[string]int{}}
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	for _, table := range tables {
		if cacheTables[table] {
			continue
		}
		rows, err := tx.QueryContext(ctx, `SELECT row_to_json(t)::text FROM `+pq.QuoteIdentifier(table)+` t`)
		if err != nil {
			return nil, Header{}, fmt.Errorf("leyendo %s: %w", table, err)
		}
		count := 0
		for rows.Next() {
			var row string
			if err := rows.Scan(&row); err != nil {
				rows.Close()
				return nil, Header{}, fmt.Errorf("leyendo %s: %w", table, err)
			}
			if err := enc.Encode(rowLine{Table: table, Row: json.RawMessage(row)}); err != nil {
				rows.Close()
				return nil, Header{}, err
			}
			count++
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, Header{}, fmt.Errorf("leyendo %s: %w", table, err)
		}
		rows.Close()
		header.Tables[table] = count
	}

	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	if err := json.NewEncoder(gz).Encode(header); err != nil {
		return nil, Header{}, err
	}
	if _, err := gz.Write(body.Bytes()); err != nil {
		return nil, Header{}, err
	}
	if err := gz.Close(); err != nil {
		return nil, Header{}, err
	}
	return out.Bytes(), header, nil
}

// Encrypt cifra el respaldo con una clave derivada de la frase (scrypt, sal
// aleatoria) usando AES-256-GCM, que además detecta cualquier alteración.
func Encrypt(plain []byte, passphrase string) ([]byte, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	gcm, err := newGCM(passphrase, salt)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(magic)+len(salt)+len(nonce)+len(plain)+gcm.Overhead())
	out = append(out, magic...)
	out = append(out, salt...)
	out = append(out, nonce...)
	// La cabecera (magic + sal) va como dato autenticado: cambiarla también
	// invalida el archivo.
	return gcm.Seal(out, nonce, plain, out[:len(magic)+len(salt)]), nil
}

// Decrypt revierte Encrypt.
func Decrypt(data []byte, passphrase string) ([]byte, error) {
	if len(data) < len(magic)+16 || !bytes.Equal(data[:len(magic)], magic) {
		return nil, ErrNotABackup
	}
	salt := data[len(magic) : len(magic)+16]
	gcm, err := newGCM(passphrase, salt)
	if err != nil {
		return nil, err
	}
	rest := data[len(magic)+16:]
	if len(rest) < gcm.NonceSize() {
		return nil, ErrWrongKeyOrCorrupt
	}
	plain, err := gcm.Open(nil, rest[:gcm.NonceSize()], rest[gcm.NonceSize():], data[:len(magic)+16])
	if err != nil {
		return nil, ErrWrongKeyOrCorrupt
	}
	return plain, nil
}

func newGCM(passphrase string, salt []byte) (cipher.AEAD, error) {
	key, err := scrypt.Key([]byte(passphrase), salt, 1<<15, 8, 1, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// readBackup descomprime el respaldo y devuelve la cabecera y las filas de
// cada tabla, comprobando que coincidan con lo que dice la cabecera (un
// archivo truncado no pasa).
func readBackup(plain []byte) (Header, map[string][]json.RawMessage, error) {
	gz, err := gzip.NewReader(bytes.NewReader(plain))
	if err != nil {
		return Header{}, nil, ErrNotABackup
	}
	defer gz.Close()
	sc := bufio.NewScanner(gz)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)

	if !sc.Scan() {
		return Header{}, nil, ErrNotABackup
	}
	var header Header
	if err := json.Unmarshal(sc.Bytes(), &header); err != nil || header.Format != formatName {
		return Header{}, nil, ErrNotABackup
	}
	if header.Version != formatVersion {
		return Header{}, nil, fmt.Errorf("versión de respaldo %d no soportada", header.Version)
	}
	rows := map[string][]json.RawMessage{}
	for sc.Scan() {
		var line rowLine
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			return Header{}, nil, fmt.Errorf("respaldo dañado: %w", err)
		}
		rows[line.Table] = append(rows[line.Table], append(json.RawMessage(nil), line.Row...))
	}
	if err := sc.Err(); err != nil {
		return Header{}, nil, fmt.Errorf("respaldo dañado: %w", err)
	}
	for table, want := range header.Tables {
		if got := len(rows[table]); got != want {
			return Header{}, nil, fmt.Errorf("respaldo incompleto: %s tiene %d filas, la cabecera dice %d", table, got, want)
		}
	}
	for table := range rows {
		if _, ok := header.Tables[table]; !ok {
			return Header{}, nil, fmt.Errorf("respaldo dañado: filas de una tabla no declarada (%s)", table)
		}
	}
	return header, rows, nil
}

// Inspect descifra y valida un respaldo sin tocar ninguna base de datos.
func Inspect(data []byte, passphrase string) (Header, error) {
	plain, err := Decrypt(data, passphrase)
	if err != nil {
		return Header{}, err
	}
	header, _, err := readBackup(plain)
	return header, err
}

// RestoreResult resume una restauración.
type RestoreResult struct {
	Header Header
	// SkippedColumns: columnas del respaldo que ya no existen en el esquema
	// actual (tabla.columna). Se avisan en vez de abortar: en una emergencia
	// es mejor recuperar todo lo demás.
	SkippedColumns []string
}

// Restore carga un respaldo cifrado en una base de datos VACÍA que ya tiene
// el esquema de la app (db.CreateTables). Todo ocurre en una transacción:
// si algo falla, la base queda como estaba.
func Restore(ctx context.Context, database *sql.DB, data []byte, passphrase string) (RestoreResult, error) {
	plain, err := Decrypt(data, passphrase)
	if err != nil {
		return RestoreResult{}, err
	}
	header, rows, err := readBackup(plain)
	if err != nil {
		return RestoreResult{}, err
	}
	result := RestoreResult{Header: header}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()

	existing, err := listTables(ctx, tx)
	if err != nil {
		return result, err
	}
	existingSet := map[string]bool{}
	for _, t := range existing {
		existingSet[t] = true
		if seededTables[t] || cacheTables[t] {
			continue
		}
		var hasRows bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM `+pq.QuoteIdentifier(t)+`)`).Scan(&hasRows); err != nil {
			return result, err
		}
		if hasRows {
			return result, fmt.Errorf("%w (la tabla %s tiene filas)", ErrTargetNotEmpty, t)
		}
	}
	for table := range header.Tables {
		if !existingSet[table] {
			return result, fmt.Errorf("la tabla %s no existe en la base de destino: arranca la app una vez contra esa base (crea el esquema) y vuelve a intentar", table)
		}
	}

	// La configuración por defecto que crea el esquema se reemplaza por la
	// del respaldo.
	for t := range seededTables {
		if existingSet[t] && header.Tables[t] > 0 {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+pq.QuoteIdentifier(t)); err != nil {
				return result, err
			}
		}
	}

	order, err := insertionOrder(ctx, tx, existing)
	if err != nil {
		return result, err
	}
	for _, table := range order {
		tableRows := rows[table]
		if len(tableRows) == 0 {
			continue
		}
		columns, err := tableColumns(ctx, tx, table)
		if err != nil {
			return result, err
		}
		// Columnas presentes en el respaldo (todas sus filas traen las
		// mismas, row_to_json incluye también las nulas). Las columnas
		// nuevas que el respaldo no trae se dejan con su DEFAULT.
		seen := map[string]bool{}
		for _, r := range tableRows {
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(r, &obj); err != nil {
				return result, fmt.Errorf("fila inválida en %s: %w", table, err)
			}
			for k := range obj {
				seen[k] = true
			}
		}
		var cols []string
		for k := range seen {
			if columns[k] {
				cols = append(cols, pq.QuoteIdentifier(k))
			} else {
				result.SkippedColumns = append(result.SkippedColumns, table+"."+k)
			}
		}
		if len(cols) == 0 {
			continue
		}
		sort.Strings(cols)
		colList := strings.Join(cols, ", ")
		stmt := fmt.Sprintf(`INSERT INTO %s (%s) SELECT %s FROM json_populate_recordset(NULL::%s, $1::json)`,
			pq.QuoteIdentifier(table), colList, colList, pq.QuoteIdentifier(table))
		for start := 0; start < len(tableRows); start += 500 {
			end := min(start+500, len(tableRows))
			batch, err := json.Marshal(tableRows[start:end])
			if err != nil {
				return result, err
			}
			if _, err := tx.ExecContext(ctx, stmt, string(batch)); err != nil {
				return result, fmt.Errorf("restaurando %s: %w", table, err)
			}
		}
	}
	if err := resetSequences(ctx, tx); err != nil {
		return result, err
	}
	sort.Strings(result.SkippedColumns)
	return result, tx.Commit()
}

// seededTables: tablas a las que db.CreateTables les agrega una fila por
// defecto (el horario de los bots). No cuentan como "la base ya tiene
// datos", y su contenido se reemplaza por el del respaldo.
var seededTables = map[string]bool{"bot_schedule": true}

// cacheTables: copias descartables (se vuelven a llenar solas) que no vale
// la pena respaldar; tampoco cuentan como "la base ya tiene datos".
var cacheTables = map[string]bool{"shop_cache": true}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func queryStrings(ctx context.Context, q querier, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func listTables(ctx context.Context, q querier) ([]string, error) {
	return queryStrings(ctx, q, `SELECT tablename FROM pg_tables WHERE schemaname = 'public' ORDER BY tablename`)
}

func tableColumns(ctx context.Context, q querier, table string) (map[string]bool, error) {
	names, err := queryStrings(ctx, q, `SELECT column_name FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = $1 AND is_generated = 'NEVER'`, table)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, n := range names {
		out[n] = true
	}
	return out, nil
}

// insertionOrder ordena las tablas para que cada una se cargue después de
// las tablas a las que hace referencia (claves foráneas).
func insertionOrder(ctx context.Context, q querier, tables []string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT src.relname, dst.relname
		FROM pg_constraint c
		JOIN pg_class src ON src.oid = c.conrelid
		JOIN pg_class dst ON dst.oid = c.confrelid
		JOIN pg_namespace n ON n.oid = src.relnamespace
		WHERE c.contype = 'f' AND n.nspname = 'public'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	deps := map[string]map[string]bool{}
	for rows.Next() {
		var src, dst string
		if err := rows.Scan(&src, &dst); err != nil {
			return nil, err
		}
		if src == dst {
			continue
		}
		if deps[src] == nil {
			deps[src] = map[string]bool{}
		}
		deps[src][dst] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return topoSort(tables, deps)
}

func topoSort(tables []string, deps map[string]map[string]bool) ([]string, error) {
	remaining := append([]string(nil), tables...)
	sort.Strings(remaining)
	done := map[string]bool{}
	var order []string
	for len(remaining) > 0 {
		progressed := false
		var next []string
		for _, t := range remaining {
			ready := true
			for d := range deps[t] {
				if !done[d] && contains(remaining, d) {
					ready = false
					break
				}
			}
			if ready {
				order = append(order, t)
				done[t] = true
				progressed = true
			} else {
				next = append(next, t)
			}
		}
		if !progressed {
			return nil, fmt.Errorf("dependencias circulares entre tablas: %v", next)
		}
		remaining = next
	}
	return order, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// resetSequences deja cada secuencia (columnas SERIAL / IDENTITY) en el
// máximo restaurado, para que los próximos registros no choquen.
func resetSequences(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT table_name, column_name FROM information_schema.columns
		WHERE table_schema = 'public' AND (column_default LIKE 'nextval(%' OR is_identity = 'YES')`)
	if err != nil {
		return err
	}
	type col struct{ table, column string }
	var cols []col
	for rows.Next() {
		var c col
		if err := rows.Scan(&c.table, &c.column); err != nil {
			rows.Close()
			return err
		}
		cols = append(cols, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range cols {
		t, k := pq.QuoteIdentifier(c.table), pq.QuoteIdentifier(c.column)
		q := fmt.Sprintf(`SELECT setval(pg_get_serial_sequence($1, $2), COALESCE((SELECT MAX(%s) FROM %s), 1), (SELECT MAX(%s) FROM %s) IS NOT NULL)`, k, t, k, t)
		if _, err := tx.ExecContext(ctx, q, "public."+c.table, c.column); err != nil {
			return fmt.Errorf("ajustando la secuencia de %s.%s: %w", c.table, c.column, err)
		}
	}
	return nil
}
