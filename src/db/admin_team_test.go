package db

import (
	"slices"
	"testing"
)

// El equipo en Discord: solo cuentas admin, activas y con Discord vinculado.
func TestEquipoAdmin_DiscordVinculado(t *testing.T) {
	conn := setupTestDB(t)
	asAdmin := func(o *testCustomerOpts) { o.isAdmin = true }

	admin, c1 := newTestCustomer(t, conn, asAdmin)
	defer c1()
	adminSinDiscord, c2 := newTestCustomer(t, conn, asAdmin)
	defer c2()
	cliente, c3 := newTestCustomer(t, conn)
	defer c3()
	adminInactivo, c4 := newTestCustomer(t, conn, asAdmin)
	defer c4()
	_ = adminSinDiscord

	dAdmin, dCliente, dInactivo := "team-"+admin.String()[:8], "team-"+cliente.String()[:8], "team-"+adminInactivo.String()[:8]
	conn.Exec(`UPDATE customers SET discord_id=$2 WHERE id=$1`, admin, dAdmin)
	conn.Exec(`UPDATE customers SET discord_id=$2 WHERE id=$1`, cliente, dCliente)
	conn.Exec(`UPDATE customers SET discord_id=$2, is_active=false WHERE id=$1`, adminInactivo, dInactivo)

	ids, err := AdminDiscordIDs(conn)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(ids, dAdmin) || slices.Contains(ids, dCliente) || slices.Contains(ids, dInactivo) || slices.Contains(ids, "") {
		t.Errorf("equipo = %v", ids)
	}
	for id, want := range map[string]bool{dAdmin: true, dCliente: false, dInactivo: false, "no-existe": false} {
		if got, err := IsAdminDiscordID(conn, id); err != nil || got != want {
			t.Errorf("IsAdminDiscordID(%s) = %v %v, se esperaba %v", id, got, err, want)
		}
	}

	// Quitarle el admin en el panel lo saca del equipo de Discord al instante.
	if err := SetCustomerAdmin(conn, admin, false); err != nil {
		t.Fatal(err)
	}
	if ok, _ := IsAdminDiscordID(conn, dAdmin); ok {
		t.Error("sin rol admin no debería seguir en el equipo")
	}
	if ids, _ := AdminDiscordIDs(conn); slices.Contains(ids, dAdmin) {
		t.Errorf("sigue en el equipo: %v", ids)
	}
}
