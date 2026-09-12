package store

import "testing"

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
