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

// TestDecideFinalOrderOutcome cubre el punto 3 del pedido de correcciones:
// un bot cuya amistad no se pudo VERIFICAR (token vencido, límite de
// solicitudes, Epic caído, fallo de red) nunca debe poder producir el mismo
// resultado que un bot que CONFIRMÓ que el receptor no es su amigo — antes
// ambos casos sumaban al mismo contador y, si todos los bots activos
// fallaban por un problema temporal, se cancelaba el pedido diciéndole al
// cliente que agregue al bot, sin haber podido comprobar nada.
func TestDecideFinalOrderOutcome(t *testing.T) {
	const withinGrace = 1 * time.Minute
	const pastGrace = friendGracePeriod + time.Minute

	cases := []struct {
		name                  string
		anyGiftLimit          bool
		activeBots            int
		notFriendBots         int
		friendCheckErrorBots  int
		insufficientFundsBots int
		orderAge              time.Duration
		want                  orderFinalOutcome
	}{
		{
			name:         "límite de gifts alcanzado siempre manda, sin importar lo demás",
			anyGiftLimit: true,
			activeBots:   2, notFriendBots: 2,
			orderAge: pastGrace,
			want:     outcomeGiftLimitPending,
		},
		{
			name:                 "todos los bots activos fallaron al verificar amistad (ninguno confirmó nada): nunca cancela",
			activeBots:           2,
			notFriendBots:        0,
			friendCheckErrorBots: 2,
			orderAge:             pastGrace,
			want:                 outcomeFriendshipUnverifiedPending,
		},
		{
			name:                 "mezcla: uno confirmado no-amigo y otro sin poder verificar — tampoco cancela",
			activeBots:           2,
			notFriendBots:        1,
			friendCheckErrorBots: 1,
			orderAge:             pastGrace,
			want:                 outcomeFriendshipUnverifiedPending,
		},
		{
			name:          "todos los bots CONFIRMARON que no es amigo, dentro del margen de gracia: pending, no cancela todavía",
			activeBots:    2,
			notFriendBots: 2,
			orderAge:      withinGrace,
			want:          outcomeNotFriendGracePeriod,
		},
		{
			name:          "todos los bots CONFIRMARON que no es amigo, fuera del margen de gracia: recién ahí cancela",
			activeBots:    2,
			notFriendBots: 2,
			orderAge:      pastGrace,
			want:          outcomeNotFriendCancel,
		},
		{
			name:                  "ningún bot con fondos suficientes",
			activeBots:            0,
			insufficientFundsBots: 2,
			orderAge:              pastGrace,
			want:                  outcomeNoFundsPending,
		},
		{
			name:          "otro motivo (ej. amistad demasiado reciente en todos los bots): reintenta sin más",
			activeBots:    2,
			notFriendBots: 0,
			orderAge:      pastGrace,
			want:          outcomeGenericPending,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decideFinalOrderOutcome(c.anyGiftLimit, c.activeBots, c.notFriendBots, c.friendCheckErrorBots, c.insufficientFundsBots, c.orderAge)
			if got != c.want {
				t.Errorf("decideFinalOrderOutcome(...) = %v, want %v", got, c.want)
			}
		})
	}
}
