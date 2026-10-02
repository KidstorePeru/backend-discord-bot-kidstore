package store

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestQuoteManual_PrecioLoCalculaElServidor(t *testing.T) {
	q, err := quoteManual("gamer", 0, "yape", 0.25)
	if err != nil || q.KC != 2400 || q.Amount != 31.20 || q.Currency != "PEN" || q.AmountPEN != 31.20 {
		t.Errorf("gamer/yape = %+v, %v", q, err)
	}
	q, err = quoteManual("custom", 1500, "plin", 0.25)
	if err != nil || q.KC != 1500 || q.Amount != 19.50 {
		t.Errorf("1500 KC = %+v, %v (1500 × S/0.013 = S/19.50)", q, err)
	}
	// Bizum: en euros + 1.5 %, redondeado hacia arriba al céntimo (igual que la web).
	q, err = quoteManual("starter", 0, "bizum", 0.25)
	if err != nil || q.Currency != "EUR" || q.Amount != 2.64 { // 10.40 × 0.25 = 2.60 → × 1.015 = 2.639 → 2.64
		t.Errorf("starter/bizum = %+v, %v", q, err)
	}
	for _, bad := range []struct {
		pkg    string
		custom int
		method string
	}{
		{"gamer", 0, "efectivo"},
		{"no-existe", 0, "yape"},
		{"custom", 50, "yape"},
		{"custom", 99_999_999, "yape"},
	} {
		if _, err := quoteManual(bad.pkg, bad.custom, bad.method, 0.25); err == nil {
			t.Errorf("%+v debería rechazarse", bad)
		}
	}
	if _, err := quoteManual("gamer", 0, "bizum", 0); err == nil {
		t.Error("sin tipo de cambio no se puede cotizar en euros")
	}
}

// jpegWithExif arma un JPEG con un bloque EXIF (como el GPS de una foto del celular).
func jpegWithExif(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		img.Set(x, h/2, color.RGBA{255, 0, 0, 255})
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	exif := append([]byte{0xFF, 0xE1, 0x00, 0x16}, []byte("Exif\x00\x00GPS-LIMA-12.04-77.03")...)
	return append(append(append([]byte{}, raw[:2]...), exif...), raw[2:]...)
}

func TestProcessProof_LimpiaYValida(t *testing.T) {
	// Foto con datos ocultos: se vuelve a codificar sin ellos.
	in := jpegWithExif(t, 400, 300)
	if !bytes.Contains(in, []byte("GPS-LIMA")) {
		t.Fatal("la imagen de prueba debería traer EXIF")
	}
	out, ct, err := processProof(in)
	if err != nil || ct != "image/jpeg" {
		t.Fatalf("processProof = %v %v", ct, err)
	}
	if bytes.Contains(out, []byte("GPS-LIMA")) || bytes.Contains(out, []byte("Exif")) {
		t.Error("los datos ocultos (EXIF/GPS) deben eliminarse")
	}

	// Captura PNG grande con transparencia: se reduce y queda en JPEG.
	big := image.NewNRGBA(image.Rect(0, 0, 3000, 1500))
	var pbuf bytes.Buffer
	png.Encode(&pbuf, big)
	out, ct, err = processProof(pbuf.Bytes())
	if err != nil || ct != "image/jpeg" {
		t.Fatalf("PNG = %v %v", ct, err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil || cfg.Width != 2000 || cfg.Height != 1000 {
		t.Errorf("tamaño final = %dx%d (%v), se esperaba 2000x1000", cfg.Width, cfg.Height, err)
	}

	// PDF: se acepta tal cual.
	pdf := []byte("%PDF-1.4\n1 0 obj << >> endobj\ntrailer << >>\n%%EOF")
	if out, ct, err := processProof(pdf); err != nil || ct != "application/pdf" || !bytes.Equal(out, pdf) {
		t.Errorf("PDF = %v %v", ct, err)
	}

	// Rechazos.
	if _, _, err := processProof([]byte("hola, esto no es una imagen")); err != errProofUnsupported {
		t.Errorf("texto: %v", err)
	}
	if _, _, err := processProof([]byte("<html><script>alert(1)</script></html>")); err != errProofUnsupported {
		t.Errorf("HTML: %v", err)
	}
	if _, _, err := processProof(nil); err != errProofEmpty {
		t.Errorf("vacío: %v", err)
	}
	if _, _, err := processProof(make([]byte, maxProofUpload+1)); err != errProofTooLarge {
		t.Errorf("muy grande: %v", err)
	}
	// PNG que dice medir 20000×20000 (bomba de descompresión): se rechaza sin decodificarlo.
	var hdr bytes.Buffer
	png.Encode(&hdr, image.NewGray(image.Rect(0, 0, 1, 1)))
	b := hdr.Bytes()
	copy(b[16:20], []byte{0x00, 0x00, 0x4E, 0x20})
	copy(b[20:24], []byte{0x00, 0x00, 0x4E, 0x20})
	if _, _, err := processProof(b); err != errProofDamaged {
		t.Errorf("dimensiones absurdas: %v", err)
	}
	// Imagen rota (cabecera JPEG y basura).
	if _, _, err := processProof(append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, []byte("basura basura basura")...)); err != errProofDamaged {
		t.Errorf("JPEG roto: %v", err)
	}
}

func TestComprobante_CifradoYEnlaceFirmado(t *testing.T) {
	SetManualPaymentsConfig(nil, "clave-de-cifrado-de-prueba", "secreto-de-prueba", "https://api.example.com")
	t.Cleanup(func() { SetManualPaymentsConfig(nil, "", "", "") })

	enc, err := encryptProof([]byte("contenido del comprobante"))
	if err != nil || bytes.Contains(enc, []byte("contenido")) {
		t.Fatalf("cifrado: %v", err)
	}
	if plain, err := decryptProof(enc); err != nil || string(plain) != "contenido del comprobante" {
		t.Errorf("descifrado: %q %v", plain, err)
	}
	enc[len(enc)-1] ^= 1
	if _, err := decryptProof(enc); err == nil {
		t.Error("un archivo alterado no debería descifrarse")
	}

	id := uuid.New()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	link := ProofViewURL(id, now)
	if !strings.HasPrefix(link, "https://api.example.com/store/manual-payments/"+id.String()+"/proof?") {
		t.Fatalf("enlace = %s", link)
	}
	exp := strings.Split(strings.Split(link, "exp=")[1], "&")[0]
	sig := strings.Split(link, "sig=")[1]
	if !validProofLink(id, exp, sig, now.Add(time.Hour)) {
		t.Error("un enlace vigente debería ser válido")
	}
	if validProofLink(id, exp, sig, now.Add(25*time.Hour)) {
		t.Error("el enlace vence a las 24 h")
	}
	if validProofLink(uuid.New(), exp, sig, now) || validProofLink(id, exp, sig[:len(sig)-1]+"0", now) {
		t.Error("un enlace con otro ID o firma alterada no debe valer")
	}
}

func TestSoporteRechazo_CodigoYWhatsApp(t *testing.T) {
	id := uuid.MustParse("a1b2c3d4-0000-4000-8000-000000000001")
	if got := ManualSupportCode(id); got != "A1B2C3D4" {
		t.Errorf("código = %q", got)
	}
	es := manualSupportWhatsApp(id, true)
	if !strings.HasPrefix(es, "https://wa.me/51983454837?text=") || !strings.Contains(es, "A1B2C3D4") {
		t.Errorf("enlace ES = %q", es)
	}
	// Los espacios van como %20 (no '+') para que WhatsApp los muestre bien.
	if strings.Contains(es, "+") || !strings.Contains(es, "Hola%2C%20mi%20comprobante") {
		t.Errorf("texto mal codificado: %q", es)
	}
	if en := manualSupportWhatsApp(id, false); !strings.Contains(en, "was%20rejected") {
		t.Errorf("enlace EN = %q", en)
	}
}
