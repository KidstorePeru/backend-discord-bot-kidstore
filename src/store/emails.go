package store

import (
	"KidStoreStore/src/types"
	"fmt"
	"log/slog"
	"net/smtp"
)

// ==================== SMTP CONFIG ====================

var smtpConfig types.EnvConfig

func SetSMTPConfig(cfg types.EnvConfig) {
	smtpConfig = cfg
}

// ==================== PAYMENT APPROVED EMAIL ====================

func SendPaymentApprovedEmail(cfg types.EnvConfig, toEmail, productName string, amountPEN float64, kcAmount int, gateway, lang, activationCode string) {
	if cfg.SMTPHost == "" { return }
	es := lang != "en"

	subject := "KidStorePeru — "
	if es { subject += "Pago aprobado" } else { subject += "Payment approved" }

	kcLine := ""
	if kcAmount > 0 {
		label := "KC acreditados"
		if !es { label = "KC credited" }
		kcLine = fmt.Sprintf(`
			<tr>
				<td style="padding:8px 16px;color:#555;">%s</td>
				<td style="padding:8px 16px;font-weight:bold;color:#2ecc71;">%d KC</td>
			</tr>`, label, kcAmount)
	}

	activationLine := ""
	if activationCode != "" {
		codeLabel := "Codigo de activacion"
		codeInstr := "Para activar tu producto escribe en el chatbot de la web: <strong>!activar " + activationCode + "</strong> o en Discord: <strong>/activar " + activationCode + "</strong>"
		if !es {
			codeLabel = "Activation code"
			codeInstr = "To activate your product type in the web chatbot: <strong>!activar " + activationCode + "</strong> or on Discord: <strong>/activar " + activationCode + "</strong>"
		}
		activationLine = fmt.Sprintf(`
			<tr>
				<td style="padding:8px 16px;color:#555;">%s</td>
				<td style="padding:8px 16px;font-weight:bold;color:#6c5ce7;font-size:18px;letter-spacing:2px;">%s</td>
			</tr>
			<tr>
				<td colspan="2" style="padding:8px 16px;color:#888;font-size:12px;">%s</td>
			</tr>`, codeLabel, activationCode, codeInstr)
	}

	title := "Pago Aprobado"
	intro := "Tu pago ha sido procesado exitosamente!"
	productLabel := "Producto"
	amountLabel := "Monto pagado"
	gatewayLabel := "Pasarela"
	footer := "Si tienes alguna consulta, contactanos por Discord."
	if !es {
		title = "Payment Approved"
		intro = "Your payment has been successfully processed!"
		productLabel = "Product"
		amountLabel = "Amount paid"
		gatewayLabel = "Gateway"
		footer = "If you have any questions, contact us on Discord."
	}

	htmlBody := fmt.Sprintf(`<!DOCTYPE html>
<html>
<head><meta charset="utf-8"></head>
<body style="margin:0;padding:0;background:#f4f4f4;font-family:Arial,Helvetica,sans-serif;">
<table width="100%%" cellpadding="0" cellspacing="0" style="background:#f4f4f4;padding:32px 0;">
<tr><td align="center">
<table width="560" cellpadding="0" cellspacing="0" style="background:#ffffff;border-radius:12px;overflow:hidden;box-shadow:0 2px 8px rgba(0,0,0,0.08);">
	<tr><td style="background:linear-gradient(135deg,#6c5ce7,#a29bfe);padding:28px;text-align:center;">
		<h1 style="margin:0;color:#fff;font-size:22px;">%s</h1>
	</td></tr>
	<tr><td style="padding:28px;">
		<p style="margin:0 0 16px;color:#333;font-size:16px;">%s</p>
		<table width="100%%" cellpadding="0" cellspacing="0" style="background:#f9f9f9;border-radius:8px;margin:16px 0;">
			<tr>
				<td style="padding:8px 16px;color:#555;">%s</td>
				<td style="padding:8px 16px;font-weight:bold;color:#333;">%s</td>
			</tr>
			<tr>
				<td style="padding:8px 16px;color:#555;">%s</td>
				<td style="padding:8px 16px;font-weight:bold;color:#333;">S/ %.2f</td>
			</tr>
			<tr>
				<td style="padding:8px 16px;color:#555;">%s</td>
				<td style="padding:8px 16px;color:#333;">%s</td>
			</tr>%s%s
		</table>
		<p style="margin:16px 0 0;color:#888;font-size:13px;">%s</p>
	</td></tr>
	<tr><td style="background:#fafafa;padding:16px;text-align:center;">
		<p style="margin:0;color:#aaa;font-size:12px;">KidStorePeru</p>
	</td></tr>
</table>
</td></tr>
</table>
</body>
</html>`, title, intro, productLabel, productName, amountLabel, amountPEN, gatewayLabel, gateway, kcLine, activationLine, footer)

	msg := fmt.Sprintf("From: KidStorePeru <%s>\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/html; charset=utf-8\r\n\r\n%s",
		cfg.SMTPFrom, toEmail, subject, htmlBody)

	auth := smtp.PlainAuth("", cfg.SMTPUser, cfg.SMTPPassword, cfg.SMTPHost)
	if err := smtp.SendMail(fmt.Sprintf("%s:%d", cfg.SMTPHost, cfg.SMTPPort), auth, cfg.SMTPFrom, []string{toEmail}, []byte(msg)); err != nil {
		slog.Error("Email: payment approved send error", "to", toEmail, "error", err)
	} else {
		slog.Info("Email: payment approved sent", "to", toEmail)
	}
}

// ==================== ORDER SENT EMAIL ====================

func SendOrderSentEmail(cfg types.EnvConfig, toEmail, epicUsername, itemName string, priceKC int, lang string) {
	if cfg.SMTPHost == "" { return }
	es := lang != "en"

	subject := "KidStorePeru — "
	if es { subject += "Pedido enviado" } else { subject += "Order sent" }

	title := "Pedido Enviado"
	intro := "Tu item de Fortnite ha sido enviado como regalo a tu cuenta Epic!"
	itemLabel := "Item"
	accountLabel := "Cuenta Epic"
	costLabel := "Costo"
	check := "Revisa tu cuenta de Fortnite para recibir el regalo."
	footer := "Si tienes alguna consulta, contactanos por Discord."
	if !es {
		title = "Order Sent"
		intro = "Your Fortnite item has been gifted to your Epic account!"
		itemLabel = "Item"
		accountLabel = "Epic Account"
		costLabel = "Cost"
		check = "Check your Fortnite account to receive the gift."
		footer = "If you have any questions, contact us on Discord."
	}

	htmlBody := fmt.Sprintf(`<!DOCTYPE html>
<html>
<head><meta charset="utf-8"></head>
<body style="margin:0;padding:0;background:#f4f4f4;font-family:Arial,Helvetica,sans-serif;">
<table width="100%%" cellpadding="0" cellspacing="0" style="background:#f4f4f4;padding:32px 0;">
<tr><td align="center">
<table width="560" cellpadding="0" cellspacing="0" style="background:#ffffff;border-radius:12px;overflow:hidden;box-shadow:0 2px 8px rgba(0,0,0,0.08);">
	<tr><td style="background:linear-gradient(135deg,#00b894,#55efc4);padding:28px;text-align:center;">
		<h1 style="margin:0;color:#fff;font-size:22px;">%s</h1>
	</td></tr>
	<tr><td style="padding:28px;">
		<p style="margin:0 0 16px;color:#333;font-size:16px;">%s</p>
		<table width="100%%" cellpadding="0" cellspacing="0" style="background:#f9f9f9;border-radius:8px;margin:16px 0;">
			<tr>
				<td style="padding:8px 16px;color:#555;">%s</td>
				<td style="padding:8px 16px;font-weight:bold;color:#333;">%s</td>
			</tr>
			<tr>
				<td style="padding:8px 16px;color:#555;">%s</td>
				<td style="padding:8px 16px;font-weight:bold;color:#333;">%s</td>
			</tr>
			<tr>
				<td style="padding:8px 16px;color:#555;">%s</td>
				<td style="padding:8px 16px;font-weight:bold;color:#6c5ce7;">%d KC</td>
			</tr>
		</table>
		<p style="margin:16px 0 4px;color:#333;font-size:14px;">%s</p>
		<p style="margin:4px 0 0;color:#888;font-size:13px;">%s</p>
	</td></tr>
	<tr><td style="background:#fafafa;padding:16px;text-align:center;">
		<p style="margin:0;color:#aaa;font-size:12px;">KidStorePeru</p>
	</td></tr>
</table>
</td></tr>
</table>
</body>
</html>`, title, intro, itemLabel, itemName, accountLabel, epicUsername, costLabel, priceKC, check, footer)

	msg := fmt.Sprintf("From: KidStorePeru <%s>\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/html; charset=utf-8\r\n\r\n%s",
		cfg.SMTPFrom, toEmail, subject, htmlBody)

	auth := smtp.PlainAuth("", cfg.SMTPUser, cfg.SMTPPassword, cfg.SMTPHost)
	if err := smtp.SendMail(fmt.Sprintf("%s:%d", cfg.SMTPHost, cfg.SMTPPort), auth, cfg.SMTPFrom, []string{toEmail}, []byte(msg)); err != nil {
		slog.Error("Email: order sent send error", "to", toEmail, "error", err)
	} else {
		slog.Info("Email: order sent notification sent", "to", toEmail)
	}
}
