package store

import "testing"

// TestChargedAmountAndCurrency cubre el punto 6: el comprobante debe mostrar
// el monto y la divisa REALES que cobró cada pasarela, no siempre soles.
func TestChargedAmountAndCurrency(t *testing.T) {
	cases := []struct {
		name                          string
		gateway                       string
		amountPEN, amountUSD, amountLocal float64
		currencyCode                  string
		wantAmount                    float64
		wantCurrency                  string
	}{
		{"mercadopago siempre en soles", "mercadopago", 100, 27, 0, "", 100, "PEN"},
		{"manual (yape/plin/transferencia) siempre en soles", "manual", 50, 13.5, 0, "", 50, "PEN"},
		{"paypal cobra en USD, no en soles", "paypal", 100, 27, 0, "", 27, "USD"},
		{"nowpayments cobra en USD, no en soles", "nowpayments", 100, 27, 0, "", 27, "USD"},
		{"dlocalgo cobra en la divisa real del cliente", "dlocalgo", 100, 27, 30.5, "MXN", 30.5, "MXN"},
		{"dlocalgo sin currency_code (registro antiguo) cae a USD, no inventa PEN", "dlocalgo", 100, 27, 0, "", 27, "USD"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotAmount, gotCurrency := ChargedAmountAndCurrency(c.gateway, c.amountPEN, c.amountUSD, c.amountLocal, c.currencyCode)
			if gotAmount != c.wantAmount || gotCurrency != c.wantCurrency {
				t.Errorf("ChargedAmountAndCurrency(%q, ...) = (%v, %v), want (%v, %v)",
					c.gateway, gotAmount, gotCurrency, c.wantAmount, c.wantCurrency)
			}
		})
	}
}

// TestFormatChargedAmount cubre la corrección del correo de "pago
// aprobado": antes SendPaymentApprovedEmail mostraba siempre "S/ {amount_pen}"
// sin importar la pasarela, mostrando el equivalente en soles de referencia
// como si fuera lo realmente cobrado. formatChargedAmount es lo que arma esa
// línea a partir del monto/divisa que devuelve ChargedAmountAndCurrency (la
// MISMA función y los MISMOS datos que ya usa el comprobante) — mismo
// criterio en los dos lugares.
func TestFormatChargedAmount(t *testing.T) {
	cases := []struct {
		name             string
		amount           float64
		currency         string
		want             string
	}{
		{"soles (mercadopago o recarga manual)", 10.40, "PEN", "S/ 10.40"},
		{"dólares (PayPal o NOWPayments)", 2.80, "USD", "US$ 2.80"},
		{"divisa local de dLocal Go", 55.30, "MXN", "MXN 55.30"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := formatChargedAmount(c.amount, c.currency); got != c.want {
				t.Errorf("formatChargedAmount(%v, %q) = %q, want %q", c.amount, c.currency, got, c.want)
			}
		})
	}
}
