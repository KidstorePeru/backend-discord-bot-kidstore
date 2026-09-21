package store

import (
	"testing"
	"time"
)

// TestAlreadyOwnedResolution cubre el punto 1 del pedido de correcciones:
// distinguir "el cliente ya poseía el artículo antes de comprar" de "una
// entrega anterior de ESTE pedido está realmente comprobada" cuando Epic
// responde ErrAlreadyOwned al intentar enviar un regalo.
func TestAlreadyOwnedResolution(t *testing.T) {
	// Primer y único intento de envío de este pedido: si Epic dice "ya lo
	// tiene", lo obtuvo por su cuenta, no por este pedido — no se marca
	// "sent", se reembolsa.
	if got := alreadyOwnedResolution(false); got != "refund_not_delivered" {
		t.Errorf("alreadyOwnedResolution(false) = %q, want %q", got, "refund_not_delivered")
	}
	// Había un intento de ESTE pedido interrumpido (proceso caído a mitad de
	// la llamada a Epic) — entrega incierta: ni se inventa evidencia de
	// entrega, ni se reembolsa a ciegas (podría duplicar un reembolso).
	if got := alreadyOwnedResolution(true); got != "needs_review" {
		t.Errorf("alreadyOwnedResolution(true) = %q, want %q", got, "needs_review")
	}
}

// TestDecideFinalOrderOutcome cubre el punto 3 del pedido de correcciones —
// motivos, independientes entre sí, por los que un pedido NUNCA debe
// cancelarse diciendo "no eres amigo de ningún bot":
//  1. un bot cuya amistad no se pudo VERIFICAR (token vencido, límite de
//     solicitudes, Epic caído, fallo de red) nunca debe contar igual que uno
//     que CONFIRMÓ que el receptor no es su amigo.
//  2. un bot que SÍ es amigo confirmado del cliente, pero que en este
//     momento no tiene V-Bucks suficientes, tampoco debe hacer que el
//     pedido se cancele por "falta de amistad" — antes la amistad recién
//     se comprobaba DESPUÉS del chequeo de fondos, así que un bot amigo sin
//     saldo nunca llegaba a sumar a "es amigo", y si el resto de los bots
//     con fondos resultaban no ser amigos, se cancelaba el pedido pese a que
//     el cliente sí había agregado a un bot (uno que en ese momento no tenía
//     saldo).
//  3. un bot amigo confirmado que agotó sus CUPOS de hoy (RemainingGifts<=0)
//     tampoco — mismo bug que el punto 2, pero con cupos en vez de fondos:
//     antes ese bot se descartaba con un "continue" ANTES de comprobar su
//     amistad, así que si el resto de los bots disponibles no eran amigos,
//     se cancelaba el pedido pese a que el cliente sí había agregado a uno
//     (el que se quedó sin cupos, algo que se resetea solo a diario).
func TestDecideFinalOrderOutcome(t *testing.T) {
	const withinGrace = 1 * time.Minute
	const pastGrace = friendGracePeriod + time.Minute

	cases := []struct {
		name                  string
		anyGiftLimit          bool
		friendBots            int
		notFriendBots         int
		friendCheckErrorBots  int
		fundedBots            int
		insufficientFundsBots int
		noSlotsFriendBots     int
		orderAge              time.Duration
		want                  orderFinalOutcome
	}{
		{
			name:          "límite de gifts alcanzado siempre manda, sin importar lo demás",
			anyGiftLimit:  true,
			notFriendBots: 2, fundedBots: 2,
			orderAge: pastGrace,
			want:     outcomeGiftLimitPending,
		},
		{
			name:                 "todos los bots con cupo fallaron al verificar amistad (ninguno confirmó nada): nunca cancela",
			friendCheckErrorBots: 2,
			fundedBots:           2,
			orderAge:             pastGrace,
			want:                 outcomeFriendshipUnverifiedPending,
		},
		{
			name:                 "mezcla: uno confirmado no-amigo y otro sin poder verificar — tampoco cancela",
			notFriendBots:        1,
			friendCheckErrorBots: 1,
			fundedBots:           2,
			orderAge:             pastGrace,
			want:                 outcomeFriendshipUnverifiedPending,
		},
		{
			name:          "todos los bots CONFIRMARON que no es amigo, dentro del margen de gracia: pending, no cancela todavía",
			notFriendBots: 2,
			fundedBots:    2,
			orderAge:      withinGrace,
			want:          outcomeNotFriendGracePeriod,
		},
		{
			name:          "todos los bots CONFIRMARON que no es amigo, fuera del margen de gracia: recién ahí cancela",
			notFriendBots: 2,
			fundedBots:    2,
			orderAge:      pastGrace,
			want:          outcomeNotFriendCancel,
		},
		{
			name:                  "ningún bot con fondos suficientes, y ninguno es amigo tampoco",
			insufficientFundsBots: 2,
			orderAge:              pastGrace,
			want:                  outcomeNoFundsPending,
		},
		{
			name:       "otro motivo (ej. amistad demasiado reciente en todos los bots): reintenta sin más",
			fundedBots: 2,
			orderAge:   pastGrace,
			want:       outcomeGenericPending,
		},
		{
			// El escenario puntual reportado: el bot amigo del cliente no
			// tiene fondos, y el ÚNICO bot con fondos no es su amigo.
			// friendBots=1 (el que sí es amigo, sin fondos) evita las dos
			// vías de cancelación por más que notFriendBots también sea 1.
			name:                  "bot amigo sin fondos + bot con fondos que no es amigo: nunca cancela",
			friendBots:            1,
			notFriendBots:         1,
			fundedBots:            1, // el no-amigo
			insufficientFundsBots: 1, // el amigo, sin saldo
			orderAge:              pastGrace,
			want:                  outcomeGenericPending,
		},
		{
			// El bot amigo sin fondos es el ÚNICO bot con cupo disponible —
			// el motivo real es "sin fondos", no "sin amistad", y así debe
			// reportarse (mensaje distinto, pero de cualquier forma nunca
			// cancela).
			name:                  "único bot con cupo es amigo pero sin fondos: reporta falta de fondos, no falta de amistad",
			friendBots:            1,
			fundedBots:            0,
			insufficientFundsBots: 1,
			orderAge:              pastGrace,
			want:                  outcomeNoFundsPending,
		},
		{
			// Amigo confirmado pero con amistad demasiado reciente (<48h) —
			// junto a un bot con fondos que no es su amigo. Tampoco cancela.
			name:          "bot amigo con fondos pero amistad reciente + bot con fondos que no es amigo: nunca cancela",
			friendBots:    1,
			notFriendBots: 1,
			fundedBots:    2,
			orderAge:      pastGrace,
			want:          outcomeGenericPending,
		},
		{
			// El escenario puntual reportado esta vez: el bot amigo del
			// cliente se quedó sin CUPOS hoy, y el único bot con cupos+fondos
			// disponible no es su amigo. friendBots=1 (el amigo sin cupos)
			// evita por completo las vías de cancelación por falta de
			// amistad, aunque notFriendBots también sea 1.
			name:              "bot amigo sin cupos + bot con cupos y fondos que no es amigo: nunca cancela, reporta falta de cupos",
			friendBots:        1,
			notFriendBots:     1,
			fundedBots:        1, // el no-amigo, que sí tiene cupos y fondos
			noSlotsFriendBots: 1, // el amigo, sin cupos hoy
			orderAge:          pastGrace,
			want:              outcomeNoSlotsPending,
		},
		{
			// Único bot con cupo (en el sentido de "el único que importa acá")
			// es amigo pero sin cupos — ni siquiera hay otro bot no-amigo de
			// por medio. Debe reportar falta de cupos, no falta de amistad.
			name:              "único bot es amigo pero sin cupos hoy: reporta falta de cupos, no falta de amistad",
			friendBots:        1,
			noSlotsFriendBots: 1,
			orderAge:          pastGrace,
			want:              outcomeNoSlotsPending,
		},
		{
			// Dos bots amigos: uno sin cupos, otro sin fondos (pero con
			// cupos). Como no TODOS los amigos están bloqueados por falta de
			// cupos (friendBots=2, noSlotsFriendBots=1), el motivo real acá
			// es fondos, no cupos — cae a outcomeNoFundsPending si ningún bot
			// (amigo o no) tiene fondos.
			name:                  "un amigo sin cupos + otro amigo sin fondos: no es únicamente un problema de cupos",
			friendBots:            2,
			fundedBots:            0,
			insufficientFundsBots: 1, // el amigo con cupos pero sin fondos
			noSlotsFriendBots:     1, // el otro amigo, sin cupos
			orderAge:              pastGrace,
			want:                  outcomeNoFundsPending,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decideFinalOrderOutcome(c.anyGiftLimit, c.friendBots, c.notFriendBots, c.friendCheckErrorBots, c.fundedBots, c.insufficientFundsBots, c.noSlotsFriendBots, c.orderAge)
			if got != c.want {
				t.Errorf("decideFinalOrderOutcome(...) = %v, want %v", got, c.want)
			}
		})
	}
}
