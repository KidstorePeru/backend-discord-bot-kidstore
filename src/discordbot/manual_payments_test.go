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
