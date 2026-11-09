package email

import (
	"fmt"
	"html"
	"strings"
)

// MagicLinkTemplate returns a branded magic-link email in both text and HTML form.
func MagicLinkTemplate(appName, link string, expiresMinutes int) (subject, textBody, htmlBody string) {
	if strings.TrimSpace(appName) == "" {
		appName = "Kigae"
	}
	if expiresMinutes <= 0 {
		expiresMinutes = 15
	}

	safeApp := html.EscapeString(appName)
	safeLink := html.EscapeString(link)
	subject = fmt.Sprintf("Your %s sign-in link", appName)
	textBody = fmt.Sprintf(
		"Sign in to %s:\n\n%s\n\nThis link expires in %d minutes.\nIf you did not request this email, you can ignore it.",
		appName, link, expiresMinutes,
	)
	htmlBody = fmt.Sprintf(`<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    <title>%s sign-in link</title>
  </head>
  <body style="margin:0;background:#f4f7f8;padding:32px 16px;font-family:Inter,ui-sans-serif,system-ui,-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;color:#0f172a;">
    <div style="max-width:640px;margin:0 auto;background:#ffffff;border:1px solid #e2e8f0;border-radius:20px;overflow:hidden;box-shadow:0 12px 40px rgba(15,23,42,0.08);">
      <div style="padding:32px 32px 20px;border-bottom:1px solid #e2e8f0;background:linear-gradient(135deg,#f0fdf4 0%%,#ecfeff 100%%);">
        <div style="display:inline-block;padding:6px 12px;border-radius:999px;background:#d1fae5;color:#047857;font-size:12px;font-weight:600;letter-spacing:0.02em;margin-bottom:16px;">Secure sign-in</div>
        <h1 style="margin:0;font-size:28px;line-height:1.2;font-weight:700;color:#0f172a;">Your %s sign-in link</h1>
        <p style="margin:12px 0 0;font-size:16px;line-height:1.6;color:#334155;">Use the button below to sign in. This link expires in %d minutes and can only be used once.</p>
      </div>
      <div style="padding:32px;">
        <div style="text-align:center;margin:8px 0 28px;">
          <a href="%s" style="display:inline-block;padding:14px 22px;border-radius:12px;background:#059669;color:#ffffff;text-decoration:none;font-weight:700;font-size:15px;">Sign in to %s</a>
        </div>
        <p style="margin:0 0 8px;font-size:14px;line-height:1.6;color:#475569;">If the button does not work, copy and paste this link into your browser:</p>
        <p style="margin:0 0 24px;font-size:13px;line-height:1.6;word-break:break-all;color:#0f172a;background:#f8fafc;border:1px solid #e2e8f0;padding:12px 14px;border-radius:10px;">%s</p>
        <p style="margin:0;font-size:14px;line-height:1.6;color:#64748b;">If you did not request this email, you can safely ignore it.</p>
      </div>
    </div>
  </body>
</html>`,
		safeApp,
		safeApp,
		expiresMinutes,
		safeLink,
		safeApp,
		safeLink,
	)
	return subject, textBody, htmlBody
}
