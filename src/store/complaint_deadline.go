package store

import (
	"database/sql"
	"log/slog"
	"time"

	"KidStoreStore/src/db"
	"KidStoreStore/src/discordbot"
)

// ==================== PLAZO DE RESPUESTA DE RECLAMOS ====================
//
// Ley N° 29571 (modificada por la Ley N° 31435): el proveedor debe responder
// cada reclamo o queja en un plazo máximo de 15 días hábiles improrrogables
// desde su presentación.
//
// Días hábiles = lunes a viernes, contados desde el día siguiente a la
// presentación, en hora de Perú. A propósito NO se descuentan los feriados: si
// se descontaran y la lista de feriados tuviera un error, el plazo mostrado
// podría quedar DESPUÉS del legal. Sin descontarlos, el vencimiento que se
// muestra nunca es posterior al real (como mucho, uno o dos días antes).

// ComplaintResponseBusinessDays: plazo legal de respuesta, en días hábiles.
const ComplaintResponseBusinessDays = 15

// ComplaintReminderThreshold: desde cuántos días hábiles restantes (o menos)
// se recuerda por Discord responder el reclamo.
const ComplaintReminderThreshold = 3

// Perú no usa horario de verano: UTC-5 fijo (no depende de que el servidor
// tenga instalada la base de zonas horarias).
var peruTZ = time.FixedZone("PET", -5*60*60)

func peruDate(t time.Time) time.Time {
	t = t.In(peruTZ)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, peruTZ)
}

func isBusinessDay(d time.Time) bool {
	wd := d.Weekday()
	return wd != time.Saturday && wd != time.Sunday
}

// ComplaintDeadline devuelve el día (hora de Perú, a las 00:00) en que vence el
// plazo de respuesta: el 15.º día hábil contado desde el día siguiente a la
// presentación.
func ComplaintDeadline(createdAt time.Time) time.Time {
	d := peruDate(createdAt)
	for n := 0; n < ComplaintResponseBusinessDays; {
		d = d.AddDate(0, 0, 1)
		if isBusinessDay(d) {
			n++
		}
	}
	return d
}

// BusinessDaysLeft: días hábiles que quedan hasta el vencimiento, contando
// desde hoy (hoy no cuenta). 0 = vence hoy; negativo = días hábiles de atraso.
func BusinessDaysLeft(deadline, now time.Time) int {
	today := peruDate(now)
	deadline = peruDate(deadline)
	if today.Equal(deadline) {
		return 0
	}
	from, to, sign := today, deadline, 1
	if today.After(deadline) {
		from, to, sign = deadline, today, -1
	}
	n := 0
	for d := from.AddDate(0, 0, 1); !d.After(to); d = d.AddDate(0, 0, 1) {
		if isBusinessDay(d) {
			n++
		}
	}
	return sign * n
}

// FormatComplaintDeadline: fecha legible del vencimiento (ej. "lun 20/10/2026").
func FormatComplaintDeadline(deadline time.Time) string {
	days := [...]string{"dom", "lun", "mar", "mié", "jue", "vie", "sáb"}
	d := peruDate(deadline)
	return days[d.Weekday()] + " " + d.Format("02/01/2006")
}

// RemindComplaintDeadlines avisa por Discord de los reclamos todavía sin
// responder a los que les quedan ComplaintReminderThreshold días hábiles o
// menos (incluidos los vencidos). Se llama periódicamente desde main.go; cada
// reclamo se recuerda como mucho una vez por día (ver discordbot).
func RemindComplaintDeadlines(database *sql.DB) {
	pending, err := db.GetPendingComplaints(database)
	if err != nil {
		slog.Error("RemindComplaintDeadlines: no se pudieron listar los reclamos pendientes", "error", err)
		return
	}
	now := time.Now()
	for _, c := range pending {
		deadline := ComplaintDeadline(c.CreatedAt)
		left := BusinessDaysLeft(deadline, now)
		if left > ComplaintReminderThreshold {
			continue
		}
		discordbot.AlertComplaintDeadline(c.Reference, c.Kind, FormatComplaintDeadline(deadline), left, peruDate(now).Format("2006-01-02"))
	}
}
