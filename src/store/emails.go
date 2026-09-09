package store

import (
	"KidStoreStore/src/types"
	"bytes"
	"encoding/json"
	"fmt"
	htmlpkg "html"
	"io"
	"log/slog"
	"net/http"
	"net/smtp"
	"strings"
	"time"
)

// esc escapa HTML en cualquier texto que venga de un cliente (nombre de
// usuario Epic, nombre de producto de una nota de admin, motivo de un
// pedido fallido, etc.) antes de meterlo en el HTML del correo. Los
// clientes de correo modernos ya filtran <script> agresivamente, pero no
// hay que depender de eso — nunca se debe confiar en texto de usuario
// dentro de HTML sin escapar.
func esc(s string) string {
	return htmlpkg.EscapeString(s)
}

// hasCRLF detecta un salto de línea crudo — sendViaSMTP arma las cabeceras
// del correo con fmt.Sprintf directo sobre "to" y "subject". Si cualquiera
// de los dos llegara a traer un \r o \n, alguien podría inyectar cabeceras
// SMTP extra (p. ej. un Bcc oculto) — esto es "email header injection", una
// vulnerabilidad clásica. Los formularios de registro/cambio de correo ya
// validan el formato del email antes de llegar acá, pero no hay que confiar
// en que TODO llamador futuro lo haga bien — se corta acá, en el único punto
// por el que pasan todos los correos que manda la web.
func hasCRLF(s string) bool {
	return strings.ContainsAny(s, "\r\n")
}

// sendEmail sends an HTML email using Resend API (production) or SMTP (local dev).
func sendEmail(cfg types.EnvConfig, to, subject, htmlBody string) error {
	if hasCRLF(to) || hasCRLF(subject) {
		slog.Error("Email: to/subject con salto de línea, bloqueado (posible inyección de cabeceras)", "to", to, "subject", subject)
		return fmt.Errorf("destinatario o asunto inválido")
	}
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

func hasEmailProvider(cfg types.EnvConfig) bool {
	return cfg.ResendAPIKey != "" || cfg.SMTPHost != ""
}

// SupportEmail — dirección de contacto que aparece en el pie de todos los correos.
const SupportEmail = "contacto@kidstoreperu.com"

// ==================== SISTEMA DE DISEÑO — "Extracto" ====================
// Un solo tema (claro), logo y KC reales, tipografía Fraunces (cifras/títulos)
// + Work Sans (cuerpo). Todos los correos transaccionales de la web se arman
// con estos mismos bloques para que se vean consistentes entre sí.

const (
	logoURL    = "https://www.kidstoreperu.net/logotipo.png"
	kcIconURL  = "https://www.kidstoreperu.net/kidcoin.png"
	fontsLink  = `<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Fraunces:opsz,wght@9..144,500&family=Work+Sans:wght@400;500;600;700&display=swap">`
)

// emailShell envuelve el contenido de cualquier correo en el mismo cascarón:
// fuentes, logo arriba, la fecha, y un pie con el link a kidstoreperu.net y
// el correo de soporte.
func emailShell(subject, preheader, dateStr, bodyHTML string) string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="es">
<head>
<meta charset="UTF-8"/>
<meta name="viewport" content="width=device-width,initial-scale=1"/>
%s
<title>%s</title>
</head>
<body style="margin:0;padding:0;background:#f7f6f3;font-family:'Work Sans',Arial,sans-serif;">
<span style="display:none;max-height:0;overflow:hidden;">%s</span>
<table width="100%%" cellpadding="0" cellspacing="0" style="background:#f7f6f3;padding:36px 16px;">
<tr><td align="center">
<table width="100%%" cellpadding="0" cellspacing="0" style="max-width:400px;background:#ffffff;border:1px solid #e4e3dd;border-radius:16px;">
<tr><td style="padding:30px 28px 26px;">

  <table width="100%%" cellpadding="0" cellspacing="0" style="margin-bottom:24px;">
    <tr>
      <td><img src="%s" alt="KidStorePeru" height="26" style="display:block;height:26px;width:auto;"/></td>
      <td align="right" style="font-size:11px;color:#6d716f;">%s</td>
    </tr>
  </table>

  %s

  <table width="100%%" cellpadding="0" cellspacing="0" style="margin-top:22px;padding-top:16px;border-top:1px solid #e6e6e2;">
    <tr><td style="font-size:11px;color:#6d716f;line-height:1.7;">
      ¿Alguna duda? Escríbenos a <a href="mailto:%s" style="color:#33396b;text-decoration:none;font-weight:600;">%s</a>
      o por <a href="https://discord.gg/kidstore" style="color:#33396b;text-decoration:none;font-weight:600;">Discord</a>.<br/>
      <span style="color:#9a9d97;">KidStorePeru · <a href="https://www.kidstoreperu.net" style="color:#9a9d97;text-decoration:none;">kidstoreperu.net</a></span>
    </td></tr>
  </table>

</td></tr>
</table>
</td></tr>
</table>
</body>
</html>`, fontsLink, subject, preheader, logoURL, dateStr, bodyHTML, SupportEmail, SupportEmail)
}

func emailEyebrow(text string) string {
	return fmt.Sprintf(`<p style="margin:0 0 4px;font-size:11.5px;color:#6d716f;">%s</p>`, text)
}

// emailHero — el dato principal del correo, en Fraunces grande. iconHTML es
// opcional (el ícono de KC antes de la cifra).
func emailHero(iconHTML, text string, small bool) string {
	size := "32px"
	if small {
		size = "23px"
	}
	return fmt.Sprintf(`<h1 style="margin:0 0 8px;font-family:'Fraunces',Georgia,serif;font-weight:500;font-size:%s;line-height:1.15;color:#14161a;">%s%s</h1>`,
		size, iconHTML, text)
}

func emailCopy(text string) string {
	return fmt.Sprintf(`<p style="margin:0 0 18px;font-size:13px;color:#6d716f;line-height:1.65;">%s</p>`, text)
}

func emailKCIcon(size int) string {
	return fmt.Sprintf(`<img src="%s" alt="KC" width="%d" height="%d" style="display:inline-block;vertical-align:-4px;margin-right:8px;"/>`, kcIconURL, size, size)
}

// emailItemRow — miniatura del producto + nombre, usado en correos de pedido.
func emailItemRow(imageURL, name, sub string) string {
	img := imageURL
	if img == "" {
		img = kcIconURL // si el item no tiene imagen, evita un <img> roto
	}
	return fmt.Sprintf(`
	<table width="100%%" cellpadding="0" cellspacing="0" style="background:#faf9f7;border:1px solid #e6e6e2;border-radius:12px;margin:0 0 16px;">
	<tr>
		<td width="52" style="padding:12px 0 12px 12px;">
			<img src="%s" alt="" width="52" height="52" style="display:block;border-radius:9px;object-fit:cover;background:#eee;"/>
		</td>
		<td style="padding:12px;">
			<div style="font-size:13px;font-weight:600;color:#14161a;line-height:1.35;">%s</div>
			<div style="font-size:11.5px;color:#6d716f;margin-top:2px;">%s</div>
		</td>
	</tr>
	</table>`, img, name, sub)
}

func emailRow(label, value string) string {
	return fmt.Sprintf(`
	<tr><td style="padding:10px 0;border-bottom:1px solid #e6e6e2;font-size:12.5px;color:#6d716f;">%s</td>
	    <td style="padding:10px 0;border-bottom:1px solid #e6e6e2;font-size:12.5px;color:#14161a;font-weight:600;text-align:right;">%s</td></tr>`, label, value)
}

func emailRows(rows string) string {
	return fmt.Sprintf(`<table width="100%%" cellpadding="0" cellspacing="0" style="margin-bottom:8px;">%s</table>`, rows)
}

func emailButton(text, url string) string {
	return fmt.Sprintf(`
	<table width="100%%" cellpadding="0" cellspacing="0" style="margin-top:20px;">
	<tr><td>
		<a href="%s" style="display:block;text-align:center;background:#14161a;color:#ffffff;font-weight:600;font-size:13.5px;padding:13px;border-radius:8px;text-decoration:none;">%s</a>
	</td></tr>
	</table>`, url, text)
}

// emailNotice — aviso al pie (expiración de enlaces, o alertas de seguridad
// con tag en color ámbar). tag es opcional.
func emailNotice(tag, text string) string {
	tagHTML := ""
	if tag != "" {
		tagHTML = fmt.Sprintf(`<span style="display:block;font-size:10.5px;font-weight:700;letter-spacing:.1em;text-transform:uppercase;color:#a15c1f;margin-bottom:6px;">%s</span>`, tag)
	}
	return fmt.Sprintf(`
	<table width="100%%" cellpadding="0" cellspacing="0" style="margin-top:20px;padding-top:14px;border-top:1px solid #a15c1f;">
	<tr><td>%s<p style="margin:0;font-size:11.5px;color:#6d716f;line-height:1.6;">%s</p></td></tr>
	</table>`, tagHTML, text)
}

var spanishMonths = [...]string{"ene.", "feb.", "mar.", "abr.", "may.", "jun.", "jul.", "ago.", "sep.", "oct.", "nov.", "dic."}

// fmtDateEs devuelve la fecha de hoy como "7 sep. 2026" — el formato que
// aparece arriba a la derecha en cada correo.
func fmtDateEs() string {
	now := time.Now()
	return fmt.Sprintf("%d %s %d", now.Day(), spanishMonths[now.Month()-1], now.Year())
}

// ==================== PAGO APROBADO (automático o manual) ====================

func SendPaymentApprovedEmail(cfg types.EnvConfig, toEmail, productName string, amountPEN float64, kcAmount int, gateway, voucherURL, lang string) {
	if !hasEmailProvider(cfg) {
		return
	}
	productName = esc(productName)
	es := lang != "en"

	subject := "KidStorePeru — "
	intro, productLabel, amountLabel, gatewayLabel, kcLabel, btnText := "", "", "", "", "", ""
	if es {
		subject += "Pago aprobado"
		intro = "Tu recarga fue confirmada y el KC ya está disponible en tu cuenta."
		productLabel, amountLabel, gatewayLabel, kcLabel = "Producto", "Monto pagado", "Método", "KC acreditado"
		btnText = "Ver comprobante"
	} else {
		subject += "Payment approved"
		intro = "Your recharge was confirmed and the KC is now available in your account."
		productLabel, amountLabel, gatewayLabel, kcLabel = "Product", "Amount paid", "Method", "KC credited"
		btnText = "View receipt"
	}

	rows := emailRow(productLabel, productName)
	if amountPEN > 0 {
		rows += emailRow(amountLabel, fmt.Sprintf("S/ %.2f", amountPEN))
	}
	rows += emailRow(gatewayLabel, gateway)

	body := emailEyebrow(kcLabel) +
		emailHero(emailKCIcon(26), fmt.Sprintf("+%d KC", kcAmount), false) +
		emailCopy(intro) +
		emailRows(rows) +
		emailButton(btnText, voucherURL)

	htmlBody := emailShell(subject, intro, fmtDateEs(), body)

	if err := sendEmail(cfg, toEmail, subject, htmlBody); err != nil {
		slog.Error("Email: payment approved send error", "to", toEmail, "error", err)
	} else {
		slog.Info("Email: payment approved sent", "to", toEmail)
	}
}

// ==================== PEDIDO ENVIADO ====================

func SendOrderSentEmail(cfg types.EnvConfig, toEmail, epicUsername, itemName, itemImage, orderID string, priceKC int, lang string) {
	if !hasEmailProvider(cfg) {
		return
	}
	epicUsername, itemName = esc(epicUsername), esc(itemName)
	es := lang != "en"

	subject := "KidStorePeru — "
	intro, itemSub, accountLabel, costLabel, btnText := "", "", "", "", ""
	if es {
		subject += "Pedido enviado"
		intro = "Ya está en tu cuenta — revisa Fortnite para recibir el regalo."
		itemSub = "Enviado a " + epicUsername
		accountLabel, costLabel = "Cuenta Epic", "Costo"
		btnText = "Ver comprobante"
	} else {
		subject += "Order sent"
		intro = "It's already on your account — check Fortnite to receive the gift."
		itemSub = "Sent to " + epicUsername
		accountLabel, costLabel = "Epic Account", "Cost"
		btnText = "View receipt"
	}

	rows := emailRow(accountLabel, epicUsername)
	rows += emailRow(costLabel, emailKCIcon(14)+fmt.Sprintf("%d KC", priceKC))

	body := emailEyebrow(map[bool]string{true: "Entregado a tu cuenta Epic", false: "Delivered to your Epic account"}[es]) +
		emailItemRow(itemImage, itemName, itemSub) +
		emailCopy(intro) +
		emailRows(rows) +
		emailButton(btnText, "https://www.kidstoreperu.net/dashboard/comprobantes/pedido/"+orderID)

	htmlBody := emailShell(subject, intro, fmtDateEs(), body)

	if err := sendEmail(cfg, toEmail, subject, htmlBody); err != nil {
		slog.Error("Email: order sent send error", "to", toEmail, "error", err)
	} else {
		slog.Info("Email: order sent notification sent", "to", toEmail)
	}
}

// ==================== PEDIDO NO COMPLETADO (reembolsado) ====================

// refunded debe reflejar si el reembolso de KC REALMENTE se confirmó (ver
// failOrderAndRefund en shop.go) — nunca se afirma en el correo que el KC
// ya volvió sin haberlo confirmado primero. Antes este correo siempre decía
// "ya te devolvimos el KC completo" sin importar si el reembolso había
// fallado.
func SendOrderFailedEmail(cfg types.EnvConfig, toEmail, epicUsername, itemName, itemImage string, priceKC int, reason string, refunded bool, lang string) {
	if !hasEmailProvider(cfg) {
		return
	}
	epicUsername, itemName, reason = esc(epicUsername), esc(itemName), esc(reason)
	es := lang != "en"

	subject := "KidStorePeru — "
	intro, itemSub, accountLabel, reasonLabel, btnText, heroText, eyebrow := "", "", "", "", "", "", ""
	if es {
		subject += "Pedido no se pudo completar"
		itemSub = "No se pudo entregar"
		accountLabel, reasonLabel = "Cuenta Epic", "Motivo"
		btnText = "Ir a la tienda"
		if refunded {
			intro = "No pudimos entregar tu item — ya te devolvimos el KC completo."
			heroText = fmt.Sprintf("%d KC de vuelta", priceKC)
			eyebrow = "KC reembolsado"
		} else {
			intro = "No pudimos entregar tu item. Tu reembolso de KC está en proceso — te avisamos apenas se confirme, no hace falta que hagas nada."
			heroText = fmt.Sprintf("%d KC en camino", priceKC)
			eyebrow = "Reembolso en proceso"
		}
	} else {
		subject += "Order could not be completed"
		itemSub = "Could not be delivered"
		accountLabel, reasonLabel = "Epic Account", "Reason"
		btnText = "Go to the store"
		if refunded {
			intro = "We couldn't deliver your item — we already refunded the full KC."
			heroText = fmt.Sprintf("%d KC back", priceKC)
			eyebrow = "KC refunded"
		} else {
			intro = "We couldn't deliver your item. Your KC refund is being processed — we'll let you know as soon as it's confirmed, no action needed on your side."
			heroText = fmt.Sprintf("%d KC pending", priceKC)
			eyebrow = "Refund in progress"
		}
	}

	rows := emailRow(accountLabel, epicUsername)
	if reason != "" {
		rows += emailRow(reasonLabel, reason)
	}

	body := emailEyebrow(eyebrow) +
		emailItemRow(itemImage, itemName, itemSub) +
		emailHero(emailKCIcon(22), heroText, true) +
		emailCopy(intro) +
		emailRows(rows) +
		emailButton(btnText, "https://www.kidstoreperu.net/store")

	htmlBody := emailShell(subject, intro, fmtDateEs(), body)

	if err := sendEmail(cfg, toEmail, subject, htmlBody); err != nil {
		slog.Error("Email: order failed send error", "to", toEmail, "error", err)
	} else {
		slog.Info("Email: order failed notification sent", "to", toEmail)
	}
}

// ==================== VERIFICACIÓN DE CUENTA ====================

func sendVerificationEmailNew(cfg types.EnvConfig, toEmail, username, verifyURL, lang string) {
	if !hasEmailProvider(cfg) {
		return
	}
	username = esc(username)
	es := lang != "en"

	subject := "KidStorePeru — "
	intro, hero, btnText, notice := "", "", "", ""
	if es {
		subject += "Verifica tu cuenta"
		intro = fmt.Sprintf("Hola %s — gracias por registrarte. Un clic y ya puedes comprar en la tienda.", username)
		hero = "Verifica tu correo para activarla"
		btnText = "Verificar mi cuenta"
		notice = "Este enlace expira en 24 horas. Si no creaste esta cuenta, ignora este correo."
	} else {
		subject += "Verify your account"
		intro = fmt.Sprintf("Hi %s — thanks for signing up. One click and you can start shopping.", username)
		hero = "Verify your email to activate it"
		btnText = "Verify my account"
		notice = "This link expires in 24 hours. If you didn't create this account, you can ignore this email."
	}

	body := emailEyebrow(map[bool]string{true: "Nueva cuenta", false: "New account"}[es]) +
		emailHero("", hero, true) +
		emailCopy(intro) +
		emailButton(btnText, verifyURL) +
		emailNotice("", notice)

	htmlBody := emailShell(subject, intro, fmtDateEs(), body)

	if err := sendEmail(cfg, toEmail, subject, htmlBody); err != nil {
		slog.Error("Email: error enviando verificacion", "to", toEmail, "error", err)
	} else {
		slog.Info("Email: verificacion enviada", "to", toEmail)
	}
}

// ==================== RECUPERAR CONTRASEÑA ====================

func sendResetEmailNew(cfg types.EnvConfig, toEmail, username, resetURL, lang string) {
	if !hasEmailProvider(cfg) {
		return
	}
	username = esc(username)
	es := lang != "en"

	subject := "KidStorePeru — "
	intro, hero, btnText, notice := "", "", "", ""
	if es {
		subject += "Recuperar contraseña"
		intro = fmt.Sprintf("Hola %s, recibimos una solicitud para cambiar la contraseña de tu cuenta.", username)
		hero = "Restablecer tu contraseña"
		btnText = "Crear nueva contraseña"
		notice = "Este enlace expira en 10 minutos. Si no fuiste tú, tu contraseña no cambiará."
	} else {
		subject += "Reset your password"
		intro = fmt.Sprintf("Hi %s, we received a request to change your account password.", username)
		hero = "Reset your password"
		btnText = "Create new password"
		notice = "This link expires in 10 minutes. If this wasn't you, your password won't change."
	}

	body := emailEyebrow(map[bool]string{true: "Solicitud de acceso", false: "Access request"}[es]) +
		emailHero("", hero, true) +
		emailCopy(intro) +
		emailButton(btnText, resetURL) +
		emailNotice("", notice)

	htmlBody := emailShell(subject, intro, fmtDateEs(), body)

	if err := sendEmail(cfg, toEmail, subject, htmlBody); err != nil {
		slog.Error("Email: error enviando reset", "to", toEmail, "error", err)
	} else {
		slog.Info("Email: reset enviado", "to", toEmail)
	}
}

// ==================== CÓDIGO OTP (cambio de correo) ====================

func sendEmailChangeOTPNew(cfg types.EnvConfig, toEmail, username, code, lang string) {
	if !hasEmailProvider(cfg) {
		return
	}
	username = esc(username)
	es := lang != "en"

	subject := "KidStorePeru — "
	intro, notice := "", ""
	if es {
		subject += "Tu código de verificación"
		intro = fmt.Sprintf("Hola %s, usa este código para confirmar el cambio de correo en tu cuenta.", username)
		notice = "Este código expira en 15 minutos. Si no lo solicitaste, ignora este correo."
	} else {
		subject += "Your verification code"
		intro = fmt.Sprintf("Hi %s, use this code to confirm the email change on your account.", username)
		notice = "This code expires in 15 minutes. If you didn't request it, ignore this email."
	}

	codeHTML := fmt.Sprintf(`<div style="margin:2px 0 18px;font-family:'Fraunces',Georgia,serif;font-weight:500;font-size:32px;letter-spacing:.08em;color:#14161a;">%s</div>`, code)

	body := emailEyebrow(map[bool]string{true: "Confirma tu nuevo correo", false: "Confirm your new email"}[es]) +
		codeHTML +
		emailCopy(intro) +
		emailNotice("", notice)

	htmlBody := emailShell(subject, intro, fmtDateEs(), body)

	if err := sendEmail(cfg, toEmail, subject, htmlBody); err != nil {
		slog.Error("Email: error enviando codigo de cambio de correo", "to", toEmail, "error", err)
	} else {
		slog.Info("Email: codigo de cambio de correo enviado", "to", toEmail)
	}
}

// ==================== ALERTAS DE SEGURIDAD ====================

func sendPasswordChangedEmail(cfg types.EnvConfig, toEmail, username, lang string) {
	if !hasEmailProvider(cfg) {
		return
	}
	username = esc(username)
	es := lang != "en"

	subject := "KidStorePeru — "
	intro, hero, warn := "", "", ""
	if es {
		subject += "Tu contraseña fue cambiada"
		intro = fmt.Sprintf("Hola %s, la contraseña de tu cuenta se cambió correctamente.", username)
		hero = "Contraseña actualizada"
		warn = fmt.Sprintf("Contáctanos de inmediato a %s — tu cuenta podría estar comprometida. Nunca te pediremos tu contraseña.", SupportEmail)
	} else {
		subject += "Your password was changed"
		intro = fmt.Sprintf("Hi %s, your account password was successfully changed.", username)
		hero = "Password updated"
		warn = fmt.Sprintf("Contact us immediately at %s — your account may be compromised. We will never ask for your password.", SupportEmail)
	}

	body := emailEyebrow(map[bool]string{true: "Seguridad de la cuenta", false: "Account security"}[es]) +
		emailHero("", hero, true) +
		emailCopy(intro) +
		emailNotice(map[bool]string{true: "Si no fuiste tú", false: "If this wasn't you"}[es], warn)

	htmlBody := emailShell(subject, intro, fmtDateEs(), body)

	if err := sendEmail(cfg, toEmail, subject, htmlBody); err != nil {
		slog.Error("Email: password changed send error", "to", toEmail, "error", err)
	} else {
		slog.Info("Email: password changed notification sent", "to", toEmail)
	}
}

func providerLabel(provider string) string {
	switch provider {
	case "google":
		return "Google"
	case "discord":
		return "Discord"
	default:
		return provider
	}
}

func SendAccountLinkedEmail(cfg types.EnvConfig, toEmail, username, provider, lang string) {
	if !hasEmailProvider(cfg) {
		return
	}
	username = esc(username)
	es := lang != "en"
	pl := providerLabel(provider)

	subject := "KidStorePeru — "
	intro, hero, warn := "", "", ""
	if es {
		subject += "Nueva cuenta vinculada"
		intro = fmt.Sprintf("Hola %s, tu cuenta de %s se vinculó correctamente a tu perfil.", username, pl)
		hero = fmt.Sprintf("Se vinculó tu cuenta de %s", pl)
		warn = fmt.Sprintf("Cambia tu contraseña de inmediato y contáctanos a %s.", SupportEmail)
	} else {
		subject += "New account linked"
		intro = fmt.Sprintf("Hi %s, your %s account was successfully linked to your profile.", username, pl)
		hero = fmt.Sprintf("Your %s account was linked", pl)
		warn = fmt.Sprintf("Change your password immediately and contact us at %s.", SupportEmail)
	}

	body := emailEyebrow(map[bool]string{true: "Seguridad de la cuenta", false: "Account security"}[es]) +
		emailHero("", hero, true) +
		emailCopy(intro) +
		emailNotice(map[bool]string{true: "Si no fuiste tú", false: "If this wasn't you"}[es], warn)

	htmlBody := emailShell(subject, intro, fmtDateEs(), body)

	if err := sendEmail(cfg, toEmail, subject, htmlBody); err != nil {
		slog.Error("Email: account linked send error", "to", toEmail, "error", err)
	} else {
		slog.Info("Email: account linked notification sent", "to", toEmail)
	}
}

// SendAccountUnlinkedEmail avisa cuando se desvincula un método de acceso
// (Google/Discord) — cierra el mismo hueco de seguridad que "cuenta
// vinculada" pero para el otro sentido: si alguien más entra a la cuenta y
// quita esa protección, el dueño real debe enterarse igual.
func SendAccountUnlinkedEmail(cfg types.EnvConfig, toEmail, username, provider, lang string) {
	if !hasEmailProvider(cfg) {
		return
	}
	username = esc(username)
	es := lang != "en"
	pl := providerLabel(provider)

	subject := "KidStorePeru — "
	intro, hero, warn := "", "", ""
	if es {
		subject += "Cuenta desvinculada"
		intro = fmt.Sprintf("Hola %s, tu cuenta de %s ya no está vinculada a tu perfil de KidStorePeru.", username, pl)
		hero = fmt.Sprintf("Se desvinculó tu cuenta de %s", pl)
		warn = fmt.Sprintf("Cambia tu contraseña de inmediato y contáctanos a %s.", SupportEmail)
	} else {
		subject += "Account unlinked"
		intro = fmt.Sprintf("Hi %s, your %s account is no longer linked to your KidStorePeru profile.", username, pl)
		hero = fmt.Sprintf("Your %s account was unlinked", pl)
		warn = fmt.Sprintf("Change your password immediately and contact us at %s.", SupportEmail)
	}

	body := emailEyebrow(map[bool]string{true: "Seguridad de la cuenta", false: "Account security"}[es]) +
		emailHero("", hero, true) +
		emailCopy(intro) +
		emailNotice(map[bool]string{true: "Si no fuiste tú", false: "If this wasn't you"}[es], warn)

	htmlBody := emailShell(subject, intro, fmtDateEs(), body)

	if err := sendEmail(cfg, toEmail, subject, htmlBody); err != nil {
		slog.Error("Email: account unlinked send error", "to", toEmail, "error", err)
	} else {
		slog.Info("Email: account unlinked notification sent", "to", toEmail)
	}
}

// SendEmailChangedNoticeEmail va al correo ANTERIOR de la cuenta (no al
// nuevo, que ya recibe su propio código OTP) — es la única forma de que el
// dueño real se entere si alguien más cambió el correo de acceso y todavía
// tiene la bandeja vieja a mano.
func SendEmailChangedNoticeEmail(cfg types.EnvConfig, oldEmail, username, newEmailMasked, lang string) {
	if !hasEmailProvider(cfg) {
		return
	}
	username, newEmailMasked = esc(username), esc(newEmailMasked)
	es := lang != "en"

	subject := "KidStorePeru — "
	intro, hero, warn := "", "", ""
	if es {
		subject += "Tu correo de acceso cambió"
		intro = fmt.Sprintf(`Hola %s, el correo de tu cuenta se cambió de este a <strong style="color:#14161a;">%s</strong>.`, username, newEmailMasked)
		hero = "Tu correo de acceso cambió"
		warn = fmt.Sprintf("Contáctanos de inmediato a %s — con este correo antiguo verificamos que la cuenta es tuya.", SupportEmail)
	} else {
		subject += "Your login email changed"
		intro = fmt.Sprintf(`Hi %s, your account email was changed from this one to <strong style="color:#14161a;">%s</strong>.`, username, newEmailMasked)
		hero = "Your login email changed"
		warn = fmt.Sprintf("Contact us immediately at %s — we can use this old email to verify the account is yours.", SupportEmail)
	}

	body := emailEyebrow(map[bool]string{true: "Seguridad de la cuenta", false: "Account security"}[es]) +
		emailHero("", hero, true) +
		emailCopy(intro) +
		emailNotice(map[bool]string{true: "Si no fuiste tú", false: "If this wasn't you"}[es], warn)

	htmlBody := emailShell(subject, intro, fmtDateEs(), body)

	if err := sendEmail(cfg, oldEmail, subject, htmlBody); err != nil {
		slog.Error("Email: email changed notice send error", "to", oldEmail, "error", err)
	} else {
		slog.Info("Email: email changed notice sent", "to", oldEmail)
	}
}

// SendComplaintReceivedEmail confirma al consumidor que su reclamo/queja del
// Libro de Reclamaciones fue registrado, con una copia de lo que declaró y su
// código de seguimiento — es práctica estándar (y esperada por INDECOPI) que
// el consumidor reciba constancia de lo que presentó.
func SendComplaintReceivedEmail(cfg types.EnvConfig, toEmail, fullName, reference, kind, productDescription, detail, lang string) {
	if !hasEmailProvider(cfg) {
		return
	}
	fullName, productDescription, detail = esc(fullName), esc(productDescription), esc(detail)
	es := lang != "en"
	kindLabel := map[string]string{"reclamo": map[bool]string{true: "Reclamo", false: "Complaint"}[es], "queja": map[bool]string{true: "Queja", false: "Grievance"}[es]}[kind]

	subject := fmt.Sprintf("KidStorePeru — %s #%s %s", kindLabel, reference, map[bool]string{true: "recibido", false: "received"}[es])
	intro, hero, plazo := "", "", ""
	if es {
		intro = fmt.Sprintf("Hola %s, registramos tu %s en nuestro Libro de Reclamaciones Virtual. Guarda tu código de seguimiento.", fullName, strings.ToLower(kindLabel))
		hero = "Código: " + reference
		plazo = "Tienes derecho a una respuesta en un plazo máximo de 15 días hábiles improrrogables. Te escribiremos a este correo apenas tengamos una respuesta."
	} else {
		intro = fmt.Sprintf("Hi %s, we've registered your %s in our Virtual Complaints Book. Save your tracking code.", fullName, strings.ToLower(kindLabel))
		hero = "Code: " + reference
		plazo = "You're entitled to a response within a maximum of 15 business days, which cannot be extended. We'll email you here as soon as we have one."
	}

	rows := emailRow(map[bool]string{true: "Bien contratado", false: "Product/service"}[es], productDescription) +
		emailRow(map[bool]string{true: "Detalle", false: "Detail"}[es], detail)

	body := emailEyebrow(map[bool]string{true: "Libro de Reclamaciones Virtual", false: "Virtual Complaints Book"}[es]) +
		emailHero("", hero, true) +
		emailCopy(intro) +
		emailRows(rows) +
		emailNotice(map[bool]string{true: "Plazo de respuesta", false: "Response timeframe"}[es], plazo)

	htmlBody := emailShell(subject, intro, fmtDateEs(), body)

	if err := sendEmail(cfg, toEmail, subject, htmlBody); err != nil {
		slog.Error("Email: complaint received send error", "to", toEmail, "error", err)
	} else {
		slog.Info("Email: complaint received confirmation sent", "to", toEmail, "reference", reference)
	}
}

// SendComplaintRespondedEmail notifica al consumidor cuando el negocio
// responde su reclamo/queja.
func SendComplaintRespondedEmail(cfg types.EnvConfig, toEmail, fullName, reference, response, lang string) {
	if !hasEmailProvider(cfg) {
		return
	}
	fullName, response = esc(fullName), esc(response)
	es := lang != "en"

	subject := fmt.Sprintf("KidStorePeru — %s #%s", map[bool]string{true: "Respuesta a tu reclamo", false: "Response to your complaint"}[es], reference)
	intro, hero := "", ""
	if es {
		intro = fmt.Sprintf("Hola %s, respondimos tu reclamo/queja #%s.", fullName, reference)
		hero = "Respondimos tu reclamo"
	} else {
		intro = fmt.Sprintf("Hi %s, we responded to your complaint #%s.", fullName, reference)
		hero = "We responded to your complaint"
	}

	body := emailEyebrow(map[bool]string{true: "Libro de Reclamaciones Virtual", false: "Virtual Complaints Book"}[es]) +
		emailHero("", hero, true) +
		emailCopy(intro) +
		emailRows(emailRow(map[bool]string{true: "Respuesta", false: "Response"}[es], response))

	htmlBody := emailShell(subject, intro, fmtDateEs(), body)

	if err := sendEmail(cfg, toEmail, subject, htmlBody); err != nil {
		slog.Error("Email: complaint responded send error", "to", toEmail, "error", err)
	} else {
		slog.Info("Email: complaint responded notification sent", "to", toEmail, "reference", reference)
	}
}

// sendTwoFactorDisabledEmail avisa cuando se desactiva el 2FA de una cuenta
// admin — es un cambio que baja la seguridad de la cuenta, así que el dueño
// real debe enterarse de inmediato si no fue él quien lo hizo (mismo motivo
// que sendPasswordChangedEmail).
func sendTwoFactorDisabledEmail(cfg types.EnvConfig, toEmail, username, lang string) {
	if !hasEmailProvider(cfg) {
		return
	}
	username = esc(username)
	es := lang != "en"

	subject := "KidStorePeru — "
	intro, hero, warn := "", "", ""
	if es {
		subject += "Se desactivó la verificación en dos pasos"
		intro = fmt.Sprintf("Hola %s, la verificación en dos pasos de tu cuenta se desactivó.", username)
		hero = "2FA desactivado"
		warn = fmt.Sprintf("Contáctanos de inmediato a %s — tu cuenta podría estar comprometida.", SupportEmail)
	} else {
		subject += "Two-factor verification was disabled"
		intro = fmt.Sprintf("Hi %s, two-factor verification on your account was turned off.", username)
		hero = "2FA disabled"
		warn = fmt.Sprintf("Contact us immediately at %s — your account may be compromised.", SupportEmail)
	}

	body := emailEyebrow(map[bool]string{true: "Seguridad de la cuenta", false: "Account security"}[es]) +
		emailHero("", hero, true) +
		emailCopy(intro) +
		emailNotice(map[bool]string{true: "Si no fuiste tú", false: "If this wasn't you"}[es], warn)

	htmlBody := emailShell(subject, intro, fmtDateEs(), body)

	if err := sendEmail(cfg, toEmail, subject, htmlBody); err != nil {
		slog.Error("Email: 2FA disabled send error", "to", toEmail, "error", err)
	} else {
		slog.Info("Email: 2FA disabled notification sent", "to", toEmail)
	}
}

// sendTwoFactorEnabledEmail confirma que el 2FA se activó correctamente —
// no es una alerta de seguridad como la de arriba, solo una confirmación
// de que el cambio se aplicó (si alguien más lo activó sin permiso, esto
// también sirve para que el dueño real lo note).
func sendTwoFactorEnabledEmail(cfg types.EnvConfig, toEmail, username, lang string) {
	if !hasEmailProvider(cfg) {
		return
	}
	username = esc(username)
	es := lang != "en"

	subject := "KidStorePeru — "
	intro, hero := "", ""
	if es {
		subject += "Verificación en dos pasos activada"
		intro = fmt.Sprintf("Hola %s, la verificación en dos pasos de tu cuenta se activó correctamente.", username)
		hero = "2FA activado"
	} else {
		subject += "Two-factor verification enabled"
		intro = fmt.Sprintf("Hi %s, two-factor verification was successfully enabled on your account.", username)
		hero = "2FA enabled"
	}

	body := emailEyebrow(map[bool]string{true: "Seguridad de la cuenta", false: "Account security"}[es]) +
		emailHero("", hero, true) +
		emailCopy(intro)

	htmlBody := emailShell(subject, intro, fmtDateEs(), body)

	if err := sendEmail(cfg, toEmail, subject, htmlBody); err != nil {
		slog.Error("Email: 2FA enabled send error", "to", toEmail, "error", err)
	} else {
		slog.Info("Email: 2FA enabled notification sent", "to", toEmail)
	}
}
