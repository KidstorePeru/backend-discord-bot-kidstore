package store

import (
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"testing"
)

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

// TestPayPalOrderMatchesTx cubre el punto 1 del pedido de correcciones: el
// status COMPLETED de una orden de PayPal, por sí solo, NUNCA debe alcanzar
// para acreditar KC — hace falta además una captura realmente completada
// (no DECLINED/PENDING/REFUNDED) y que el importe/divisa cobrados coincidan
// con lo que la transacción esperaba cobrar. Sin esta doble verificación, un
// atacante que lograra que una orden de PayPal cualquiera terminara con
// status COMPLETED por un importe distinto (o en otra divisa) podría hacer
// que se acreditara KC sin que se hubiera cobrado el monto correcto.
func TestPayPalOrderMatchesTx(t *testing.T) {
	cases := []struct {
		name        string
		order       payPalOrderDetails
		expectedUSD float64
		want        bool
	}{
		{
			name:        "orden completada, captura completada, importe exacto: aprueba",
			order:       payPalOrderDetails{Status: "COMPLETED", CaptureStatus: "COMPLETED", AmountValue: "27.00", CurrencyCode: "USD"},
			expectedUSD: 27.00,
			want:        true,
		},
		{
			name:        "tolera un centavo de diferencia por redondeo de punto flotante",
			order:       payPalOrderDetails{Status: "COMPLETED", CaptureStatus: "COMPLETED", AmountValue: "27.005", CurrencyCode: "USD"},
			expectedUSD: 27.00,
			want:        true,
		},
		{
			name:        "orden aprobada pero aún sin capturar: no aprueba",
			order:       payPalOrderDetails{Status: "APPROVED", CaptureStatus: "", AmountValue: "27.00", CurrencyCode: "USD"},
			expectedUSD: 27.00,
			want:        false,
		},
		{
			name:        "orden COMPLETED pero la captura fue rechazada: no aprueba",
			order:       payPalOrderDetails{Status: "COMPLETED", CaptureStatus: "DECLINED", AmountValue: "27.00", CurrencyCode: "USD"},
			expectedUSD: 27.00,
			want:        false,
		},
		{
			name:        "orden COMPLETED pero la captura fue reembolsada: no aprueba",
			order:       payPalOrderDetails{Status: "COMPLETED", CaptureStatus: "REFUNDED", AmountValue: "27.00", CurrencyCode: "USD"},
			expectedUSD: 27.00,
			want:        false,
		},
		{
			name:        "importe cobrado menor al esperado: no aprueba",
			order:       payPalOrderDetails{Status: "COMPLETED", CaptureStatus: "COMPLETED", AmountValue: "0.01", CurrencyCode: "USD"},
			expectedUSD: 27.00,
			want:        false,
		},
		{
			name:        "divisa distinta a USD: no aprueba aunque el número coincida",
			order:       payPalOrderDetails{Status: "COMPLETED", CaptureStatus: "COMPLETED", AmountValue: "27.00", CurrencyCode: "EUR"},
			expectedUSD: 27.00,
			want:        false,
		},
		{
			name:        "importe ilegible: no aprueba",
			order:       payPalOrderDetails{Status: "COMPLETED", CaptureStatus: "COMPLETED", AmountValue: "no-es-un-numero", CurrencyCode: "USD"},
			expectedUSD: 27.00,
			want:        false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := payPalOrderMatchesTx(c.order, c.expectedUSD); got != c.want {
				t.Errorf("payPalOrderMatchesTx(%+v, %v) = %v, want %v", c.order, c.expectedUSD, got, c.want)
			}
		})
	}
}

// TestSortJSONForNOWPaymentsSignature cubre el punto 4 del pedido de
// correcciones: el reordenamiento de claves (recursivo) y la reserialización
// deben coincidir byte a byte con lo que describen los ejemplos oficiales de
// NOWPayments (Node.js/PHP/Python) — cualquier diferencia (números
// reformateados, caracteres escapados de más) produce una firma distinta a
// la que NOWPayments realmente calculó, rechazando IPNs legítimos.
func TestSortJSONForNOWPaymentsSignature(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "ordena claves de primer nivel",
			input: `{"b":1,"a":2}`,
			want:  `{"a":2,"b":1}`,
		},
		{
			name:  "ordena claves anidadas recursivamente",
			input: `{"z":1,"a":{"y":2,"b":3}}`,
			want:  `{"a":{"b":3,"y":2},"z":1}`,
		},
		{
			name:  "preserva enteros grandes sin redondear (más allá de la precisión segura de float64)",
			input: `{"payment_id":123456789012345678}`,
			want:  `{"payment_id":123456789012345678}`,
		},
		{
			name:  "no escapa '&', '<', '>' (a diferencia del default de encoding/json)",
			input: `{"order_description":"A & B < C > D"}`,
			want:  `{"order_description":"A & B < C > D"}`,
		},
		{
			name:  "ejemplo real de la documentación oficial de NOWPayments",
			input: `{"payment_id":123456789,"parent_payment_id":987654321,"invoice_id":null,"payment_status":"finished","pay_address":"address","payin_extra_id":null,"price_amount":1,"price_currency":"usd","pay_amount":15,"actually_paid":15,"actually_paid_at_fiat":0,"pay_currency":"trx","order_id":null,"order_description":null,"purchase_id":"123456789","outcome_amount":14.8106,"outcome_currency":"trx","payment_extra_ids":null}`,
			want:  `{"actually_paid":15,"actually_paid_at_fiat":0,"invoice_id":null,"order_description":null,"order_id":null,"outcome_amount":14.8106,"outcome_currency":"trx","parent_payment_id":987654321,"pay_address":"address","pay_amount":15,"pay_currency":"trx","payin_extra_id":null,"payment_extra_ids":null,"payment_id":123456789,"payment_status":"finished","price_amount":1,"price_currency":"usd","purchase_id":"123456789"}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := sortJSONForNOWPaymentsSignature([]byte(c.input))
			if err != nil {
				t.Fatalf("error inesperado: %v", err)
			}
			if string(got) != c.want {
				t.Errorf("sortJSONForNOWPaymentsSignature(%s) = %s, want %s", c.input, got, c.want)
			}
		})
	}

	t.Run("JSON inválido produce error, nunca un resultado inventado", func(t *testing.T) {
		if _, err := sortJSONForNOWPaymentsSignature([]byte(`{not json`)); err == nil {
			t.Error("se esperaba un error para un cuerpo que no es JSON válido")
		}
	})
}

// TestVerifyNOWPaymentsSignature cubre el punto 4 del pedido de
// correcciones: la validación de la firma IPN de NOWPayments (HMAC-SHA512
// sobre el cuerpo con sus claves ordenadas), siguiendo la documentación
// oficial. Las firmas "correctas" de estos casos se calculan de forma
// independiente (con hmac/sha512 de la librería estándar), nunca llamando a
// sortJSONForNOWPaymentsSignature — para no volver la prueba una tautología.
func TestVerifyNOWPaymentsSignature(t *testing.T) {
	prevCfg := paymentCfg
	defer func() { paymentCfg = prevCfg }()

	const secret = "test-ipn-secret"
	const body = `{"payment_status":"finished","payment_id":123456789,"order_id":"abc"}`
	// Firma calculada a mano sobre el JSON YA ordenado alfabéticamente
	// ("order_id","payment_id","payment_status") — es justo lo que
	// verifyNOWPaymentsSignature debe reproducir a partir del body de arriba
	// (que llega con las claves en OTRO orden, como cualquier IPN real).
	const sortedBody = `{"order_id":"abc","payment_id":123456789,"payment_status":"finished"}`
	mac := hmac.New(sha512.New, []byte(secret))
	mac.Write([]byte(sortedBody))
	validSig := hex.EncodeToString(mac.Sum(nil))

	t.Run("firma correcta, calculada de forma independiente: acepta", func(t *testing.T) {
		paymentCfg = prevCfg
		paymentCfg.NOWPaymentsIPNSecret = secret
		if !verifyNOWPaymentsSignature([]byte(body), validSig) {
			t.Error("una firma HMAC-SHA512 válida sobre el JSON ordenado debería aceptarse")
		}
	})

	t.Run("firma correcta en mayúsculas: acepta (insensible a mayúsculas)", func(t *testing.T) {
		paymentCfg = prevCfg
		paymentCfg.NOWPaymentsIPNSecret = secret
		upper := ""
		for _, r := range validSig {
			if r >= 'a' && r <= 'f' {
				r -= 32
			}
			upper += string(r)
		}
		if !verifyNOWPaymentsSignature([]byte(body), upper) {
			t.Error("la firma en mayúsculas debería aceptarse igual (no es sensible al caso)")
		}
	})

	t.Run("firma de otro secreto: rechaza", func(t *testing.T) {
		paymentCfg = prevCfg
		paymentCfg.NOWPaymentsIPNSecret = secret
		mac := hmac.New(sha512.New, []byte("otro-secreto"))
		mac.Write([]byte(sortedBody))
		wrongSig := hex.EncodeToString(mac.Sum(nil))
		if verifyNOWPaymentsSignature([]byte(body), wrongSig) {
			t.Error("una firma calculada con un secreto distinto NUNCA debe aceptarse")
		}
	})

	t.Run("cuerpo alterado después de firmar: rechaza", func(t *testing.T) {
		paymentCfg = prevCfg
		paymentCfg.NOWPaymentsIPNSecret = secret
		tampered := `{"payment_status":"finished","payment_id":999999999,"order_id":"abc"}`
		if verifyNOWPaymentsSignature([]byte(tampered), validSig) {
			t.Error("un cuerpo modificado después de calcular la firma NUNCA debe aceptarse")
		}
	})

	t.Run("sin secreto configurado: rechaza en vez de validar contra una clave vacía", func(t *testing.T) {
		paymentCfg = prevCfg
		paymentCfg.NOWPaymentsIPNSecret = ""
		if verifyNOWPaymentsSignature([]byte(body), validSig) {
			t.Error("sin NOWPaymentsIPNSecret configurado, nunca debe aceptarse ninguna firma")
		}
	})

	t.Run("sin header de firma: rechaza", func(t *testing.T) {
		paymentCfg = prevCfg
		paymentCfg.NOWPaymentsIPNSecret = secret
		if verifyNOWPaymentsSignature([]byte(body), "") {
			t.Error("un request sin x-nowpayments-sig nunca debe aceptarse")
		}
	})
}
