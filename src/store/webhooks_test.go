package store

import "testing"

// Pruebas de regresión puras (sin red ni base de datos) para la
// clasificación de estados de cada pasarela — la parte que decide si un
// pago pendiente se acredita, se rechaza, o se deja tal cual. Cubren
// exactamente los casos que motivaron el punto 2 del pedido de correcciones:
// cerrar la ventana de pago o un timeout NUNCA deben poder producir un
// resultado "aprobado" o "rechazado" por sí mismos — esas señales del
// cliente ni siquiera llegan hasta acá (checkGatewayOutcome solo consulta a
// la pasarela), así que estas pruebas verifican que el mapeo de estados
// reales de cada pasarela sea el correcto.

func TestClassifyMercadoPagoStatus(t *testing.T) {
	cases := map[string]gatewayOutcome{
		"approved":   gatewayApproved,
		"rejected":   gatewayRejected,
		"cancelled":  gatewayRejected,
		"pending":    gatewayStillPending,
		"in_process": gatewayStillPending,
		"authorized": gatewayStillPending,
		"":           gatewayStillPending, // sin ningún pago encontrado todavía
	}
	for status, want := range cases {
		if got := classifyMercadoPagoStatus(status); got != want {
			t.Errorf("classifyMercadoPagoStatus(%q) = %v, want %v", status, got, want)
		}
	}
}

func TestClassifyPayPalStatus(t *testing.T) {
	cases := map[string]gatewayOutcome{
		"COMPLETED":             gatewayApproved,
		"VOIDED":                gatewayRejected,
		"APPROVED":              gatewayStillPending, // aprobado por el cliente pero aún no capturado — no es un rechazo
		"CREATED":               gatewayStillPending,
		"SAVED":                 gatewayStillPending,
		"PAYER_ACTION_REQUIRED": gatewayStillPending,
	}
	for status, want := range cases {
		if got := classifyPayPalStatus(status); got != want {
			t.Errorf("classifyPayPalStatus(%q) = %v, want %v", status, got, want)
		}
	}
}

func TestClassifyDLocalGoStatus(t *testing.T) {
	cases := map[string]gatewayOutcome{
		"PAID":      gatewayApproved,
		"REJECTED":  gatewayRejected,
		"CANCELLED": gatewayRejected,
		"EXPIRED":   gatewayRejected,
		"PENDING":   gatewayStillPending,
	}
	for status, want := range cases {
		if got := classifyDLocalGoStatus(status); got != want {
			t.Errorf("classifyDLocalGoStatus(%q) = %v, want %v", status, got, want)
		}
	}
}

func TestClassifyNOWPaymentsStatus(t *testing.T) {
	cases := map[string]gatewayOutcome{
		"confirmed":      gatewayApproved,
		"finished":       gatewayApproved,
		"failed":         gatewayRejected,
		"expired":        gatewayRejected,
		"refunded":       gatewayRejected,
		"waiting":        gatewayStillPending,
		"confirming":     gatewayStillPending,
		"sending":        gatewayStillPending,
		"partially_paid": gatewayStillPending, // se maneja aparte (alerta a soporte), nunca como aprobado ni rechazado
	}
	for status, want := range cases {
		if got := classifyNOWPaymentsStatus(status); got != want {
			t.Errorf("classifyNOWPaymentsStatus(%q) = %v, want %v", status, got, want)
		}
	}
}
