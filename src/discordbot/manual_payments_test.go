package discordbot

import (
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/google/uuid"
)

func TestManualPayment_BotonesYAviso(t *testing.T) {
	id := uuid.New()
	for _, c := range []struct {
		customID, action string
		ok               bool
	}{
		{mpApprovePrefix + id.String(), "ok", true},
		{mpRejectPrefix + id.String(), "no", true},
		{mpModalPrefix + id.String(), "reason", true},
		{mpApprovePrefix + "no-es-uuid", "ok", false},
		{"slot:algo", "", false},
	} {
		action, got, ok := parseManualCustomID(c.customID)
		if ok != c.ok || (ok && (action != c.action || got != id)) {
			t.Errorf("%q → %q %v %v", c.customID, action, got, ok)
		}
	}

	a := ManualPaymentAlert{ID: id, Customer: "Cliente1", Package: "Gamer 2,400 KC", KC: 2400, Amount: "S/ 31.20",
		Method: "Yape", Operation: "01234567", DuplicateOps: 1, ViewURL: "https://api.example.com/x"}
	e := manualPaymentEmbed(a)
	all := e.Title + e.Description
	for _, f := range e.Fields {
		all += f.Name + f.Value
	}
	for _, want := range []string{"S/ 31.20", "2400 KC", "Yape", "01234567", "posible comprobante repetido", strings.ToUpper(id.String()[:8])} {
		if !strings.Contains(all, want) {
			t.Errorf("el aviso no contiene %q", want)
		}
	}
	if e.Color != colorAlert {
		t.Error("un número de operación repetido debería resaltarse en rojo")
	}
	row := manualPaymentButtons(a)[0].(discordgo.ActionsRow)
	if len(row.Components) != 3 || row.Components[0].(discordgo.Button).URL != a.ViewURL {
		t.Errorf("botones = %+v", row.Components)
	}
}

// Sin base de datos (o sin equipo) solo el dueño es admin.
func TestEquipo_SoloElDuenoSinBase(t *testing.T) {
	oldCfg, oldDB := cfg, database
	defer func() { cfg, database = oldCfg, oldDB }()
	database = nil

	cfg.DiscordAdminUserID = "111"
	if !isAdmin("111") || isAdmin("222") || isAdmin("") {
		t.Error("solo el dueño debería ser admin")
	}
	if ids := adminDiscordIDs(); len(ids) != 1 || ids[0] != "111" {
		t.Errorf("equipo = %v", ids)
	}
	cfg.DiscordAdminUserID = ""
	if isAdmin("") || len(adminDiscordIDs()) != 0 {
		t.Error("sin dueño configurado no hay admins")
	}
}

// Al revisar un comprobante, todas las copias muestran el mismo resultado y
// quedan solo con "Ver comprobante".
func TestManualPayment_ResultadoEnTodasLasCopias(t *testing.T) {
	a := ManualPaymentAlert{ID: uuid.New(), Customer: "Cliente1", Package: "Gamer", KC: 2400, Amount: "S/ 31.20", Method: "Yape", ViewURL: "https://api.example.com/x"}
	base := manualPaymentEmbed(a)
	n := len(base.Fields)

	for result, color := range map[string]int{
		"✅ Aprobado por Discord: juan — +2400 KC acreditados":              colorSuccess,
		"❌ Rechazado por Panel: Ana — No encontramos el pago.":             colorAlert,
		"ℹ️ Ya fue revisado por otra persona del equipo o desde el panel.": colorWarnSoft,
	} {
		e := reviewedManualEmbed(base, result)
		last := e.Fields[len(e.Fields)-1]
		if e.Color != color || last.Name != "Resultado" || last.Value != result {
			t.Errorf("%q → color %x, último campo %q", result, e.Color, last.Value)
		}
	}
	if len(base.Fields) != n {
		t.Error("no debe modificar el aviso original")
	}

	rows := viewOnlyButtons(a)
	row, ok := rows[0].(discordgo.ActionsRow)
	if len(rows) != 1 || !ok || len(row.Components) != 1 {
		t.Fatalf("botones = %+v", rows)
	}
	if b := row.Components[0].(discordgo.Button); b.Style != discordgo.LinkButton || b.URL != a.ViewURL {
		t.Errorf("botón = %+v", b)
	}

	// Sin sesión de Discord igual se limpia el registro de copias.
	manualCopiesMu.Lock()
	manualAlerts[a.ID] = a
	manualCopies[a.ID] = []manualCopy{{channelID: "c", messageID: "m"}}
	manualCopiesMu.Unlock()
	ManualReviewDone(a.ID, "✅ ok")
	manualCopiesMu.Lock()
	_, left := manualCopies[a.ID]
	manualCopiesMu.Unlock()
	if left {
		t.Error("las copias deberían borrarse del registro tras la revisión")
	}
}
