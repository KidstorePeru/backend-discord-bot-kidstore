package store

import (
	"testing"
)

// Lo que de verdad le queda a la tienda tras la comisión de Mercado Pago
// (porcentaje y cargo fijo, ambos con IGV), calculado como lo hace Mercado Pago.
func netAfterGateway(total float64, f GatewayFee) float64 {
	return total - (f.Percent/100*total+f.Fixed)*(1+f.Tax/100)
}

// Soles que llegan a Perú tras cobrar total € por Bizum y enviarlos por remesa.
func penAfterRemittance(total, eurPerPEN float64, f RemittanceFee) float64 {
	sent := total*(1-f.Percent/100) - f.Fixed
	return sent * (1 - f.FXMargin/100) / eurPerPEN
}

func TestComision_MercadoPago_SiempreQuedaElPrecioExacto(t *testing.T) {
	// Ejemplo: Gamer S/31.20 con la tarifa por defecto (3.49% + S/1 + IGV).
	total, fee := gatewayTotal(31.20, defaultPaymentFees.MercadoPago)
	if total != 33.78 || fee != 2.58 {
		t.Errorf("Gamer: total %.2f, comisión %.2f (se esperaba 33.78 y 2.58)", total, fee)
	}

	tarifas := []GatewayFee{
		defaultPaymentFees.MercadoPago,
		{Percent: 3.29, Fixed: 1, Tax: 18},
		{Percent: 4.99, Fixed: 1, Tax: 18},
		{Percent: 3.99, Tax: 18},
		{Percent: 5.4, Fixed: 0.3},
		{Percent: 3.49, Fixed: 1, Tax: 18, Margin: 0.5},
		{},
	}
	for _, f := range tarifas {
		for cents := 130; cents <= 1_000_000; cents = cents*3/2 + 7 {
			price := float64(cents) / 100
			total, fee := gatewayTotal(price, f)
			net := netAfterGateway(total, f)
			if net+1e-9 < price {
				t.Fatalf("%+v: precio %.2f, cobro %.2f → quedan %.4f (¡pérdida!)", f, price, total, net)
			}
			if net-price > 0.02+f.Margin/100*total {
				t.Errorf("%+v: precio %.2f, cobro %.2f → sobran %.4f (se cobra de más)", f, price, total, net-price)
			}
			if d := total - price - fee; d > 0.001 || d < -0.001 {
				t.Errorf("la comisión mostrada (%.2f) no cuadra con total - precio (%.2f)", fee, total-price)
			}
		}
	}
}

func TestComision_Bizum_CubreLaRemesaYElCambio(t *testing.T) {
	// Con solo 1.5% da lo mismo que antes: S/10.40 a 0.25 €/S/ = €2.60 → €2.64.
	if total, fee := bizumTotal(10.40, 0.25, RemittanceFee{Percent: 1.5}); total != 2.64 || fee != 0.04 {
		t.Errorf("starter: €%.2f (comisión €%.2f), se esperaba €2.64 (€0.04)", total, fee)
	}
	tarifas := []RemittanceFee{
		{Percent: 1.5},
		{Percent: 2, Fixed: 1.99, FXMargin: 1.5},
		{Fixed: 3},
		{Percent: 0.5, FXMargin: 3},
	}
	for _, f := range tarifas {
		for _, rate := range []float64{0.2312, 0.25, 0.2777} {
			for cents := 130; cents <= 500_000; cents = cents*3/2 + 11 {
				price := float64(cents) / 100
				total, _ := bizumTotal(price, rate, f)
				got := penAfterRemittance(total, rate, f)
				if got+1e-9 < price {
					t.Fatalf("%+v a %.4f: precio S/%.2f, cobro €%.2f → llegan S/%.4f (¡pérdida!)", f, rate, price, total, got)
				}
				if got-price > 0.05 {
					t.Errorf("%+v: precio S/%.2f → llegan S/%.4f (se cobra de más)", f, price, got)
				}
			}
		}
	}
}

func TestComision_Validacion(t *testing.T) {
	if err := defaultPaymentFees.validate(); err != nil {
		t.Fatalf("las comisiones por defecto deben ser válidas: %v", err)
	}
	malas := []PaymentFees{
		{MercadoPago: GatewayFee{Percent: -1}},
		{MercadoPago: GatewayFee{Percent: 25}},
		{MercadoPago: GatewayFee{Tax: 50}},
		{MercadoPago: GatewayFee{Fixed: 100}},
		{Bizum: RemittanceFee{FXMargin: 40}},
		{Bizum: RemittanceFee{Percent: 99}},
	}
	for _, f := range malas {
		if f.validate() == nil {
			t.Errorf("%+v debería rechazarse", f)
		}
	}
}
