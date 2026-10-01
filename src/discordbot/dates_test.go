package discordbot

import (
	"testing"
	"time"
)

func TestFechaES_MesEnEspanolYHoraDePeru(t *testing.T) {
	// 01:00 UTC del 6 de septiembre = 20:00 del 5 de septiembre en Perú.
	got := fechaES(time.Date(2026, 9, 6, 1, 0, 0, 0, time.UTC))
	if got != "5 de septiembre de 2026" {
		t.Errorf("fechaES = %q", got)
	}
}
