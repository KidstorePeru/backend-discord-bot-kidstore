package store

// Libro de Reclamaciones: plazo legal de 15 días hábiles (lunes a viernes, desde
// el día siguiente a la presentación, en hora de Perú), idioma del reclamo y
// listado de pendientes para los recordatorios.

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"KidStoreStore/src/db"
	"KidStoreStore/src/types"

	"github.com/gin-gonic/gin"
)

func lima(y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, time.FixedZone("PET", -5*3600))
}

func TestComplaintDeadline_QuinceDiasHabiles(t *testing.T) {
	cases := []struct {
		name    string
		created time.Time
		want    string
	}{
		// Lun 28/09 → cuenta desde el mar 29: vence el 15.º día hábil, lun 19/10.
		{"presentado un lunes", lima(2026, 9, 28, 10, 0), "2026-10-19"},
		// Vie 02/10 a las 23:30 en Perú (ya es sábado en UTC): cuenta el viernes peruano.
		{"viernes de noche en Perú", lima(2026, 10, 2, 23, 30), "2026-10-23"},
		// Sábado y domingo no cuentan: empieza el lunes 05/10.
		{"presentado un sábado", lima(2026, 10, 3, 12, 0), "2026-10-23"},
		{"presentado un domingo", lima(2026, 10, 4, 12, 0), "2026-10-23"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ComplaintDeadline(c.created.UTC()).Format("2006-01-02")
			if got != c.want {
				t.Errorf("vencimiento = %s, se esperaba %s", got, c.want)
			}
		})
	}
}

func TestComplaintDeadline_NuncaCaeEnFinDeSemana(t *testing.T) {
	start := lima(2026, 1, 1, 9, 0)
	for i := 0; i < 400; i++ {
		d := ComplaintDeadline(start.AddDate(0, 0, i))
		if wd := d.Weekday(); wd == time.Saturday || wd == time.Sunday {
			t.Fatalf("el vencimiento %s cae en fin de semana", d.Format("2006-01-02 Mon"))
		}
		// Entre 15 días hábiles hay al menos 19 y como mucho 23 días corridos.
		days := int(d.Sub(peruDate(start.AddDate(0, 0, i))).Hours() / 24)
		if days < 19 || days > 23 {
			t.Fatalf("plazo de %d días corridos para %s", days, start.AddDate(0, 0, i).Format("2006-01-02"))
		}
	}
}

func TestBusinessDaysLeft(t *testing.T) {
	deadline := lima(2026, 10, 19, 0, 0) // lunes
	cases := []struct {
		name string
		now  time.Time
		want int
	}{
		{"el mismo día vence hoy", lima(2026, 10, 19, 22, 0), 0},
		{"viernes anterior", lima(2026, 10, 16, 9, 0), 1},
		{"sábado anterior", lima(2026, 10, 17, 9, 0), 1},
		{"martes de la semana anterior", lima(2026, 10, 13, 9, 0), 4},
		{"un día hábil tarde", lima(2026, 10, 20, 9, 0), -1},
		{"una semana tarde", lima(2026, 10, 26, 9, 0), -5},
		// Lunes 19 a las 23:30 en Perú es martes en UTC: sigue siendo el día del vencimiento.
		{"lunes de noche en Perú", lima(2026, 10, 19, 23, 30).UTC(), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := BusinessDaysLeft(deadline, c.now); got != c.want {
				t.Errorf("días hábiles restantes = %d, se esperaba %d", got, c.want)
			}
		})
	}
}

func TestFormatComplaintDeadline(t *testing.T) {
	if got := FormatComplaintDeadline(lima(2026, 10, 19, 0, 0)); got != "lun 19/10/2026" {
		t.Errorf("formato = %q", got)
	}
}

func deleteComplaint(conn *sql.DB, reference string) {
	conn.Exec(`DELETE FROM consumer_complaints WHERE reference=$1`, reference)
	conn.Exec(`DELETE FROM audit_logs WHERE details LIKE $1`, "%"+reference+"%")
}

// El reclamo guarda el idioma en que se presentó (del header X-Lang que usa
// todo el sitio) para responderle al consumidor en ese idioma.
func TestHandlerCreateComplaint_GuardaElIdioma(t *testing.T) {
	conn := setupShopTestDB(t)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/store/complaints", HandlerCreateComplaint(conn, types.EnvConfig{}))

	for _, tc := range []struct{ header, want string }{{"en", "en"}, {"es", "es"}, {"", "es"}, {"fr", "es"}} {
		body := `{"kind":"reclamo","full_name":"Cliente Prueba","document_type":"DNI","document_number":"12345678",
			"email":"cliente@example.com","product_description":"Recarga de KC","detail":"El pedido no llegó a mi cuenta","consumer_request":"Revisión"}`
		req := httptest.NewRequest(http.MethodPost, "/store/complaints", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if tc.header != "" {
			req.Header.Set("X-Lang", tc.header)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("X-Lang=%q: status %d: %s", tc.header, w.Code, w.Body.String())
		}
		var resp struct {
			Complaint struct {
				Reference string `json:"reference"`
			} `json:"complaint"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.Complaint.Reference == "" {
			t.Fatalf("respuesta sin código de seguimiento: %s", w.Body.String())
		}
		reference := resp.Complaint.Reference
		t.Cleanup(func() { deleteComplaint(conn, reference) })
		saved, err := db.GetComplaintByReference(conn, reference)
		if err != nil {
			t.Fatal(err)
		}
		if saved.Lang != tc.want {
			t.Errorf("X-Lang=%q: idioma guardado %q, se esperaba %q", tc.header, saved.Lang, tc.want)
		}
	}
}

// Solo los reclamos sin responder entran en los recordatorios de plazo.
func TestGetPendingComplaints_SoloLosSinResponder(t *testing.T) {
	conn := setupShopTestDB(t)
	base := types.ConsumerComplaint{Kind: "queja", FullName: "Cliente Prueba", DocumentType: "DNI", DocumentNumber: "87654321",
		Email: "cliente@example.com", ProductDescription: "Tienda", Detail: "Detalle de la queja", ConsumerRequest: "Revisión"}
	pending, err := db.CreateComplaint(conn, base, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { deleteComplaint(conn, pending.Reference) })
	answered, err := db.CreateComplaint(conn, base, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { deleteComplaint(conn, answered.Reference) })
	if err := db.RespondToComplaint(conn, answered.ID, "Respuesta de prueba"); err != nil {
		t.Fatal(err)
	}

	list, err := db.GetPendingComplaints(conn)
	if err != nil {
		t.Fatal(err)
	}
	var sawPending, sawAnswered bool
	for _, c := range list {
		sawPending = sawPending || c.Reference == pending.Reference
		sawAnswered = sawAnswered || c.Reference == answered.Reference
	}
	if !sawPending || sawAnswered {
		t.Errorf("pendientes: incluye el pendiente=%v, incluye el respondido=%v", sawPending, sawAnswered)
	}
	if pending.Lang != "es" {
		t.Errorf("sin idioma indicado, el reclamo queda en español; obtuve %q", pending.Lang)
	}
	// Los recordatorios no fallan con datos reales (Discord no está configurado en pruebas).
	RemindComplaintDeadlines(conn)
}
