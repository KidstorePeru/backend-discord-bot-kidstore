package discordbot

import (
	"strings"
	"testing"

	"KidStoreStore/src/types"
)

func TestBotFriendsFields(t *testing.T) {
	n := func(v int) *int { return &v }
	fields := botFriendsFields([]types.GameAccount{
		{DisplayName: "Abierta", IsActive: true, FriendsCount: n(347)},
		{DisplayName: "Llena", IsActive: true, FriendsCount: n(1000)},
		{DisplayName: "SinDato", IsActive: true},
		{DisplayName: "Inactiva", IsActive: false, FriendsCount: n(10)},
	})
	if len(fields) != 2 {
		t.Fatalf("campos = %d, se esperaban 2", len(fields))
	}
	if !strings.Contains(fields[0].Value, "`Abierta` (347/1000)") || strings.Contains(fields[0].Value, "Inactiva") || strings.Contains(fields[0].Value, "SinDato") {
		t.Errorf("abiertas = %q", fields[0].Value)
	}
	if fields[1].Value != "Llena" {
		t.Errorf("llenas = %q", fields[1].Value)
	}
	if got := botFriendsFields(nil); len(got) != 0 {
		t.Errorf("sin cuentas no debería agregar campos: %v", got)
	}
}
