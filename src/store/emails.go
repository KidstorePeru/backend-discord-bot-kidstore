package store

import (
	"KidStoreStore/src/types"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/smtp"
)

// sendEmail sends an HTML email using Resend API (production) or SMTP (local dev).
func sendEmail(cfg types.EnvConfig, to, subject, htmlBody string) error {
	if cfg.ResendAPIKey != "" {
		return sendViaResend(cfg.ResendAPIKey, cfg.SMTPFrom, to, subject, htmlBody)
	}
	if cfg.SMTPHost != "" {
		return sendViaSMTP(cfg, to, subject, htmlBody)
	}
	slog.Warn("Email: no email provider configured (set RESEND_API_KEY or SMTP_HOST)", "to", to)
	return fmt.Errorf("no email provider configured")
}

func sendViaResend(apiKey, from, to, subject, htmlBody string) error {
	payload := map[string]interface{}{
		"from":    fmt.Sprintf("KidStorePeru <%s>", from),
		"to":      []string{to},
		"subject": subject,
		"html":    htmlBody,
	}
	body, _ := json.Marshal(payload)

	req, _ := http.NewRequest("POST", "https://api.resend.com/emails", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("resend request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("resend error %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

func sendViaSMTP(cfg types.EnvConfig, to, subject, htmlBody string) error {
	msg := fmt.Sprintf("From: KidStorePeru <%s>\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/html; charset=utf-8\r\n\r\n%s",
		cfg.SMTPFrom, to, subject, htmlBody)
	auth := smtp.PlainAuth("", cfg.SMTPUser, cfg.SMTPPassword, cfg.SMTPHost)
	return smtp.SendMail(fmt.Sprintf("%s:%d", cfg.SMTPHost, cfg.SMTPPort), auth, cfg.SMTPFrom, []string{to}, []byte(msg))
}

// ==================== SMTP CONFIG ====================

var smtpConfig types.EnvConfig

func SetSMTPConfig(cfg types.EnvConfig) {
	smtpConfig = cfg
}

// GetSMTPConfig expone la config de correo ya cargada — la usan otros
// paquetes (p. ej. admin) que necesitan mandar un correo transaccional pero
// no tienen su propia copia de types.EnvConfig a mano.
func GetSMTPConfig() types.EnvConfig {
	return smtpConfig
}

// hasEmailProvider evita construir el HTML si no hay forma de mandarlo —
// igual chequeo que ya usan sendVerificationEmail/sendResetEmail en auth.go,
// repetido acá para que TODOS los correos transaccionales sean consistentes
// (antes algunos solo revisaban SMTP_HOST y se saltaban Resend por error).
func hasEmailProvider(cfg types.EnvConfig) bool {
	return cfg.ResendAPIKey != "" || cfg.SMTPHost != ""
}

// ==================== COMPONENTES VISUALES COMPARTIDOS ====================
// Estos helpers arman el mismo "look" oscuro de marca (fondo #0a0a0f,
// acentos morados, logo en el header) que ya usan los correos de
// verificación/reset/OTP en auth.go (ver emailBase) — así todos los correos
// que manda la web se ven como parte de la misma marca profesional, en vez
// de tener cada tipo de correo su propio diseño suelto.

// emailBadge — insignia de estado arriba del título (✅ pago, ↩️ reembolso, 🔐 seguridad).
func emailBadge(emoji, text, bg, color string) string {
	return fmt.Sprintf(`<div style="display:inline-block;background:%s;color:%s;padding:6px 16px;border-radius:20px;font-size:12px;font-weight:700;letter-spacing:0.3px;margin:0 0 16px;">%s %s</div>`,
		bg, color, emoji, text)
}

// emailInfoRow — una fila "etiqueta: valor" dentro de la tabla de detalles.
func emailInfoRow(label, value, valueColor string) string {
	if valueColor == "" {
		valueColor = "#ffffff"
	}
	return fmt.Sprintf(`
		<tr>
			<td style="padding:10px 16px;color:#8b8ba7;font-size:13px;border-bottom:1px solid #14142a;">%s</td>
			<td style="padding:10px 16px;color:%s;font-size:13px;font-weight:700;text-align:right;border-bottom:1px solid #14142a;">%s</td>
		</tr>`, label, valueColor, value)
}

// emailInfoTable — envuelve varias emailInfoRow en una tarjeta con bordes redondeados.
func emailInfoTable(rows string) string {
	return fmt.Sprintf(`<table width="100%%" cellpadding="0" cellspacing="0" style="background:#080810;border:1px solid #1e1e3a;border-radius:12px;margin:0 0 24px;overflow:hidden;">%s
	</table>`, rows)
}

// emailWarningBox — caja de advertencia roja para alertas de seguridad
// ("si no fuiste tú, cambia tu contraseña").
func emailWarningBox(text string) string {
	return fmt.Sprintf(`<div style="background:rgba(239,68,68,0.08);border:1px solid rgba(239,68,68,0.25);border-radius:12px;padding:16px 20px;margin:0 0 24px;">
  <p style="margin:0;font-size:13px;color:#fca5a5;line-height:1.6;">⚠️ %s</p>
</div>`, text)
}

// ==================== PAGO APROBADO (automático o manual) ====================
// Se usa tanto para pagos automáticos confirmados por una pasarela
// (MercadoPago, PayPal, NOWPayments, dLocal Go) como para recargas manuales
// aprobadas por un administrador (Yape/Plin/transferencia) — el parámetro
// "gateway" simplemente identifica el método usado en ambos casos.

func SendPaymentApprovedEmail(cfg types.EnvConfig, toEmail, productName string, amountPEN float64, kcAmount int, gateway, lang string) {
	if !hasEmailProvider(cfg) {
		return
	}
	es := lang != "en"

	subject := "KidStorePeru — "
	title, intro, productLabel, amountLabel, gatewayLabel, kcLabel, badgeText, footer := "", "", "", "", "", "", "", ""
	if es {
		subject += "Pago aprobado ✅"
		title = "¡Pago aprobado!"
		intro = "Tu recarga fue confirmada y el KC ya está disponible en tu cuenta."
		productLabel, amountLabel, gatewayLabel, kcLabel = "Producto", "Monto pagado", "Método", "KC acreditado"
		badgeText = "PAGO CONFIRMADO"
		footer = "Ya puedes usar tu KC para comprar en la tienda. Cualquier duda, escríbenos por Discord."
	} else {
		subject += "Payment approved ✅"
		title = "Payment approved!"
		intro = "Your recharge was confirmed and the KC is now available in your account."
		productLabel, amountLabel, gatewayLabel, kcLabel = "Product", "Amount paid", "Method", "KC credited"
		badgeText = "PAYMENT CONFIRMED"
		footer = "You can now use your KC to shop in the store. Any questions, message us on Discord."
	}

	rows := emailInfoRow(productLabel, productName, "")
	if amountPEN > 0 {
		rows += emailInfoRow(amountLabel, fmt.Sprintf("S/ %.2f", amountPEN), "")
	}
	rows += emailInfoRow(gatewayLabel, gateway, "")
	if kcAmount > 0 {
		rows += emailInfoRow(kcLabel, fmt.Sprintf("%d KC", kcAmount), "#4ade80")
	}

	content := fmt.Sprintf(`
%s
<h1 style="margin:0 0 8px;font-size:24px;font-weight:800;color:#ffffff;">%s</h1>
<p style="margin:0 0 24px;font-size:15px;color:#8b8ba7;line-height:1.6;">%s</p>
%s
<div style="border-top:1px solid #1e1e3a;padding-top:20px;">
  <p style="margin:0;font-size:12px;color:#4a4a6a;">%s</p>
</div>`, emailBadge("✅", badgeText, "rgba(34,197,94,0.12)", "#4ade80"), title, intro, emailInfoTable(rows), footer)

	htmlBody := emailBase(subject, intro, content)

	if err := sendEmail(cfg, toEmail, subject, htmlBody); err != nil {
		slog.Error("Email: payment approved send error", "to", toEmail, "error", err)
	} else {
		slog.Info("Email: payment approved sent", "to", toEmail)
	}
}

// ==================== PEDIDO ENVIADO ====================

func SendOrderSentEmail(cfg types.EnvConfig, toEmail, epicUsername, itemName string, priceKC int, lang string) {
	if !hasEmailProvider(cfg) {
		return
	}
	es := lang != "en"

	subject := "KidStorePeru — "
	title, intro, itemLabel, accountLabel, costLabel, badgeText, check, footer := "", "", "", "", "", "", "", ""
	if es {
		subject += "Pedido enviado 🎁"
		title = "¡Tu item ya está en camino!"
		intro = "Enviamos tu item de Fortnite como regalo a tu cuenta Epic."
		itemLabel, accountLabel, costLabel = "Item", "Cuenta Epic", "Costo"
		badgeText = "ENTREGADO"
		check = "Revisa tu cuenta de Fortnite para recibir el regalo."
		footer = "¿Alguna consulta? Escríbenos por Discord."
	} else {
		subject += "Order sent 🎁"
		title = "Your item is on its way!"
		intro = "We sent your Fortnite item as a gift to your Epic account."
		itemLabel, accountLabel, costLabel = "Item", "Epic Account", "Cost"
		badgeText = "DELIVERED"
		check = "Check your Fortnite account to receive the gift."
		footer = "Any questions? Message us on Discord."
	}

	rows := emailInfoRow(itemLabel, itemName, "")
	rows += emailInfoRow(accountLabel, epicUsername, "")
	rows += emailInfoRow(costLabel, fmt.Sprintf("%d KC", priceKC), "#a855f7")

	content := fmt.Sprintf(`
%s
<h1 style="margin:0 0 8px;font-size:24px;font-weight:800;color:#ffffff;">%s</h1>
<p style="margin:0 0 24px;font-size:15px;color:#8b8ba7;line-height:1.6;">%s</p>
%s
<p style="margin:0 0 20px;font-size:14px;color:#ffffff;font-weight:600;">✓ %s</p>
<div style="border-top:1px solid #1e1e3a;padding-top:20px;">
  <p style="margin:0;font-size:12px;color:#4a4a6a;">%s</p>
</div>`, emailBadge("🎁", badgeText, "rgba(168,85,247,0.12)", "#c4b5fd"), title, intro, emailInfoTable(rows), check, footer)

	htmlBody := emailBase(subject, intro, content)

	if err := sendEmail(cfg, toEmail, subject, htmlBody); err != nil {
		slog.Error("Email: order sent send error", "to", toEmail, "error", err)
	} else {
		slog.Info("Email: order sent notification sent", "to", toEmail)
	}
}

// ==================== PEDIDO NO COMPLETADO (reembolsado) ====================
// Antes esto no existía: si un pedido fallaba (usuario no encontrado, no es
// amigo de ningún bot, etc.) el KC se reembolsaba automáticamente pero el
// cliente nunca se enteraba salvo que revisara su panel manualmente.

func SendOrderFailedEmail(cfg types.EnvConfig, toEmail, epicUsername, itemName string, priceKC int, reason, lang string) {
	if !hasEmailProvider(cfg) {
		return
	}
	es := lang != "en"

	subject := "KidStorePeru — "
	title, intro, itemLabel, accountLabel, refundLabel, reasonLabel, badgeText, footer := "", "", "", "", "", "", "", ""
	if es {
		subject += "Pedido no se pudo completar"
		title = "Tu pedido no se pudo enviar"
		intro = "No pudimos entregar tu item — pero no te preocupes, ya te devolvimos el KC completo a tu balance."
		itemLabel, accountLabel, refundLabel, reasonLabel = "Item", "Cuenta Epic", "KC reembolsado", "Motivo"
		badgeText = "KC REEMBOLSADO"
		footer = "Puedes intentarlo de nuevo desde la tienda. Si necesitas ayuda, escríbenos por Discord."
	} else {
		subject += "Order could not be completed"
		title = "Your order couldn't be sent"
		intro = "We couldn't deliver your item — but don't worry, we already refunded the full KC to your balance."
		itemLabel, accountLabel, refundLabel, reasonLabel = "Item", "Epic Account", "KC refunded", "Reason"
		badgeText = "KC REFUNDED"
		footer = "You can try again from the store. If you need help, message us on Discord."
	}

	rows := emailInfoRow(itemLabel, itemName, "")
	rows += emailInfoRow(accountLabel, epicUsername, "")
	rows += emailInfoRow(refundLabel, fmt.Sprintf("%d KC", priceKC), "#fbbf24")
	if reason != "" {
		rows += emailInfoRow(reasonLabel, reason, "")
	}

	content := fmt.Sprintf(`
%s
<h1 style="margin:0 0 8px;font-size:24px;font-weight:800;color:#ffffff;">%s</h1>
<p style="margin:0 0 24px;font-size:15px;color:#8b8ba7;line-height:1.6;">%s</p>
%s
<div style="border-top:1px solid #1e1e3a;padding-top:20px;">
  <p style="margin:0;font-size:12px;color:#4a4a6a;">%s</p>
</div>`, emailBadge("↩️", badgeText, "rgba(251,191,36,0.12)", "#fbbf24"), title, intro, emailInfoTable(rows), footer)

	htmlBody := emailBase(subject, intro, content)

	if err := sendEmail(cfg, toEmail, subject, htmlBody); err != nil {
		slog.Error("Email: order failed send error", "to", toEmail, "error", err)
	} else {
		slog.Info("Email: order failed notification sent", "to", toEmail)
	}
}

// ==================== ALERTAS DE SEGURIDAD ====================
// Nuevas — antes no existía ningún aviso cuando cambiaba algo sensible de la
// cuenta. Si alguien más entra a la cuenta y cambia la contraseña o vincula
// su propio Discord/Google, el dueño real debe enterarse por correo aunque
// el atacante ya esté adentro — es la única forma de detectar un acceso no
// autorizado a tiempo.

func sendPasswordChangedEmail(cfg types.EnvConfig, toEmail, username, lang string) {
	if !hasEmailProvider(cfg) {
		return
	}
	es := lang != "en"

	subject := "KidStorePeru — "
	title, intro, warn, badgeText, closing := "", "", "", "", ""
	if es {
		subject += "Tu contraseña fue cambiada 🔐"
		title = "Contraseña actualizada"
		intro = fmt.Sprintf("Hola %s, la contraseña de tu cuenta en KidStorePeru se cambió correctamente.", username)
		warn = "Si fuiste tú, no necesitas hacer nada. Si <strong>NO reconoces</strong> este cambio, contáctanos de inmediato por Discord — tu cuenta podría estar comprometida."
		badgeText = "SEGURIDAD DE LA CUENTA"
		closing = "KidStorePeru nunca te pedirá tu contraseña por correo, Discord o cualquier otro medio."
	} else {
		subject += "Your password was changed 🔐"
		title = "Password updated"
		intro = fmt.Sprintf("Hi %s, your KidStorePeru account password was successfully changed.", username)
		warn = "If this was you, no action is needed. If you <strong>DON'T recognize</strong> this change, contact us immediately on Discord — your account may be compromised."
		badgeText = "ACCOUNT SECURITY"
		closing = "KidStorePeru will never ask for your password by email, Discord, or any other channel."
	}

	content := fmt.Sprintf(`
%s
<h1 style="margin:0 0 8px;font-size:24px;font-weight:800;color:#ffffff;">%s</h1>
<p style="margin:0 0 20px;font-size:15px;color:#8b8ba7;line-height:1.6;">%s</p>
%s
<div style="border-top:1px solid #1e1e3a;padding-top:20px;">
  <p style="margin:0;font-size:12px;color:#4a4a6a;">%s</p>
</div>`, emailBadge("🔐", badgeText, "rgba(124,58,237,0.15)", "#c4b5fd"), title, intro, emailWarningBox(warn), closing)

	htmlBody := emailBase(subject, intro, content)

	if err := sendEmail(cfg, toEmail, subject, htmlBody); err != nil {
		slog.Error("Email: password changed send error", "to", toEmail, "error", err)
	} else {
		slog.Info("Email: password changed notification sent", "to", toEmail)
	}
}

func SendAccountLinkedEmail(cfg types.EnvConfig, toEmail, username, provider, lang string) {
	if !hasEmailProvider(cfg) {
		return
	}
	es := lang != "en"

	providerLabel := provider
	if provider == "google" {
		providerLabel = "Google"
	} else if provider == "discord" {
		providerLabel = "Discord"
	}

	subject := "KidStorePeru — "
	title, intro, warn, badgeText, closing := "", "", "", "", ""
	if es {
		subject += "Nueva cuenta vinculada 🔐"
		title = "Se vinculó una cuenta nueva"
		intro = fmt.Sprintf("Hola %s, tu cuenta de <strong style=\"color:#ffffff;\">%s</strong> se vinculó correctamente a tu perfil de KidStorePeru.", username, providerLabel)
		warn = "Si fuiste tú, no necesitas hacer nada. Si <strong>NO reconoces</strong> esta acción, cambia tu contraseña de inmediato y contáctanos por Discord."
		badgeText = "SEGURIDAD DE LA CUENTA"
		closing = "KidStorePeru nunca te pedirá tu contraseña por correo, Discord o cualquier otro medio."
	} else {
		subject += "New account linked 🔐"
		title = "A new account was linked"
		intro = fmt.Sprintf("Hi %s, your <strong style=\"color:#ffffff;\">%s</strong> account was successfully linked to your KidStorePeru profile.", username, providerLabel)
		warn = "If this was you, no action is needed. If you <strong>DON'T recognize</strong> this, change your password immediately and contact us on Discord."
		badgeText = "ACCOUNT SECURITY"
		closing = "KidStorePeru will never ask for your password by email, Discord, or any other channel."
	}

	content := fmt.Sprintf(`
%s
<h1 style="margin:0 0 8px;font-size:24px;font-weight:800;color:#ffffff;">%s</h1>
<p style="margin:0 0 20px;font-size:15px;color:#8b8ba7;line-height:1.6;">%s</p>
%s
<div style="border-top:1px solid #1e1e3a;padding-top:20px;">
  <p style="margin:0;font-size:12px;color:#4a4a6a;">%s</p>
</div>`, emailBadge("🔐", badgeText, "rgba(124,58,237,0.15)", "#c4b5fd"), title, intro, emailWarningBox(warn), closing)

	htmlBody := emailBase(subject, intro, content)

	if err := sendEmail(cfg, toEmail, subject, htmlBody); err != nil {
		slog.Error("Email: account linked send error", "to", toEmail, "error", err)
	} else {
		slog.Info("Email: account linked notification sent", "to", toEmail)
	}
}
