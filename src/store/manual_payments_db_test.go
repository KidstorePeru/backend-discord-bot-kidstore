package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"KidStoreStore/src/db"
	"KidStoreStore/src/discordbot"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// memProofStore — almacenamiento en memoria (nunca se sube nada real).
type memProofStore struct {
	mu   sync.Mutex
	objs map[string][]byte
}

func (m *memProofStore) Put(_ context.Context, k string, d []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[k] = append([]byte(nil), d...)
	return nil
}
func (m *memProofStore) Get(_ context.Context, k string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.objs[k]
	if !ok {
		return nil, errors.New("no existe")
	}
	return d, nil
}
func (m *memProofStore) Delete(_ context.Context, k string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objs, k)
	return nil
}
func (m *memProofStore) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.objs)
}

func setupManualPayments(t *testing.T) (*memProofStore, chan discordbot.ManualPaymentAlert) {
	t.Helper()
	mem := &memProofStore{objs: map[string][]byte{}}
	SetManualPaymentsConfig(mem, "clave-de-cifrado-de-prueba", "secreto-de-prueba", "https://api.example.com")
	alerts := make(chan discordbot.ManualPaymentAlert, 10)
	prev := alertManualPayment
	alertManualPayment = func(a discordbot.ManualPaymentAlert) { alerts <- a }
	t.Cleanup(func() {
		SetManualPaymentsConfig(nil, "", "", "")
		alertManualPayment = prev
	})
	return mem, alerts
}

func uploadProof(t *testing.T, conn interface{}, customerID uuid.UUID, fields map[string]string, file []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for k, v := range fields {
		w.WriteField(k, v)
	}
	if file != nil {
		fw, _ := w.CreateFormFile("proof", "captura.jpg")
		fw.Write(file)
	}
	w.Close()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("customer_id", customerID.String()); c.Next() })
	r.POST("/store/manual-payments", HandlerCreateManualPayment(shopTestDB))
	req := httptest.NewRequest(http.MethodPost, "/store/manual-payments", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestPagoManual_FlujoCompleto(t *testing.T) {
	conn := setupShopTestDB(t)
	mem, alerts := setupManualPayments(t)
	custID, cleanup := newShopTestCustomer(t, conn, 100)
	t.Cleanup(cleanup)
	t.Cleanup(func() { conn.Exec(`DELETE FROM manual_payment_requests WHERE customer_id=$1`, custID) })
	conn.Exec(`DELETE FROM kc_recharges WHERE customer_id=$1`, custID)
	t.Cleanup(func() { conn.Exec(`DELETE FROM kc_recharges WHERE customer_id=$1`, custID) })
	photo := jpegWithExif(t, 600, 800)

	// Sin confirmar que pagó: se rechaza.
	if w := uploadProof(t, conn, custID, map[string]string{"package_id": "gamer", "method": "yape"}, photo); w.Code != http.StatusBadRequest {
		t.Errorf("sin confirmar: %d", w.Code)
	}
	// Sin archivo: se rechaza.
	if w := uploadProof(t, conn, custID, map[string]string{"package_id": "gamer", "method": "yape", "confirm": "true"}, nil); w.Code != http.StatusBadRequest {
		t.Errorf("sin archivo: %d", w.Code)
	}
	// Archivo que no es imagen: se rechaza.
	if w := uploadProof(t, conn, custID, map[string]string{"package_id": "gamer", "method": "yape", "confirm": "true"}, []byte("<script>x</script>")); w.Code != http.StatusBadRequest {
		t.Errorf("no imagen: %d", w.Code)
	}

	// Envío válido: el monto lo pone el servidor aunque el cliente mande otro.
	w := uploadProof(t, conn, custID, map[string]string{
		"package_id": "gamer", "method": "yape", "confirm": "true", "operation_number": " 0123 4567 ",
		"amount": "0.01", "kc_amount": "999999",
	}, photo)
	if w.Code != http.StatusOK {
		t.Fatalf("envío: %d %s", w.Code, w.Body.String())
	}
	var res struct{ Request db.ManualPaymentRequest }
	json.Unmarshal(w.Body.Bytes(), &res)
	first := res.Request
	if first.KCAmount != 2400 || first.Amount != 31.20 || first.Currency != "PEN" || first.Status != "pending" || first.OperationNumber != "01234567" {
		t.Errorf("solicitud = %+v", first)
	}
	if mem.count() != 1 {
		t.Fatalf("debería haber 1 comprobante guardado, hay %d", mem.count())
	}
	// Lo guardado está cifrado (no es el JPEG) y sin los datos ocultos.
	for _, data := range mem.objs {
		if bytes.HasPrefix(data, []byte{0xFF, 0xD8}) {
			t.Error("el comprobante debe guardarse cifrado")
		}
	}
	stored, _ := db.GetManualPaymentRequest(conn, first.ID)
	plain, ct, err := LoadProof(context.Background(), stored)
	if err != nil || ct != "image/jpeg" || bytes.Contains(plain, []byte("GPS-LIMA")) {
		t.Errorf("comprobante guardado: %v %v", ct, err)
	}
	select {
	case a := <-alerts:
		if a.ID != first.ID || a.Amount != "S/ 31.20" || a.Method != "Yape" || a.ViewURL == "" {
			t.Errorf("aviso al admin = %+v", a)
		}
	case <-time.After(2 * time.Second):
		t.Error("no llegó el aviso al admin")
	}

	// Segundo envío con el MISMO número de operación: el aviso lo marca como posible repetido.
	if w := uploadProof(t, conn, custID, map[string]string{"package_id": "starter", "method": "plin", "confirm": "true", "operation_number": "01234567"}, photo); w.Code != http.StatusOK {
		t.Fatalf("segundo envío: %d", w.Code)
	}
	if a := <-alerts; a.DuplicateOps != 1 {
		t.Errorf("debería avisar que el número de operación se repite: %+v", a)
	}
	// Tercero: ya hay 2 en revisión → no se acepta y no queda archivo huérfano.
	if w := uploadProof(t, conn, custID, map[string]string{"package_id": "pro", "method": "bcp", "confirm": "true"}, photo); w.Code != http.StatusConflict {
		t.Errorf("tercer envío: %d", w.Code)
	}
	if mem.count() != 2 {
		t.Errorf("archivos guardados = %d, se esperaban 2", mem.count())
	}

	// Aprobar dos veces a la vez: se acredita UNA sola vez.
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := ApproveManualPayment(conn, first.ID, "test", false); results <- err }()
	}
	wg.Wait()
	close(results)
	okCount := 0
	for err := range results {
		if err == nil {
			okCount++
		} else if !errors.Is(err, db.ErrManualAlreadyReviewed) {
			t.Errorf("error inesperado: %v", err)
		}
	}
	if okCount != 1 {
		t.Errorf("aprobaciones exitosas = %d, se esperaba 1", okCount)
	}
	var balance, recharges int
	conn.QueryRow(`SELECT kc_balance FROM customers WHERE id=$1`, custID).Scan(&balance)
	conn.QueryRow(`SELECT COUNT(*) FROM kc_recharges WHERE customer_id=$1`, custID).Scan(&recharges)
	if balance != 2500 || recharges != 1 {
		t.Errorf("saldo = %d (se esperaba 100 + 2400), recargas = %d", balance, recharges)
	}
	if _, err := RejectManualPayment(conn, first.ID, "x", "test"); !errors.Is(err, db.ErrManualAlreadyReviewed) {
		t.Errorf("rechazar una aprobada: %v", err)
	}

	// Rechazar la otra con motivo: el cliente recibe el aviso.
	list, _ := db.ListManualPaymentsByCustomer(conn, custID, 10)
	var second db.ManualPaymentRequest
	for _, m := range list {
		if m.Status == "pending" {
			second = m
		}
	}
	rejected, err := RejectManualPayment(conn, second.ID, "El monto no coincide", "test")
	if err != nil || rejected.Status != "rejected" || rejected.RejectReason == nil || *rejected.RejectReason != "El monto no coincide" {
		t.Errorf("rechazo = %+v %v", rejected, err)
	}
	if countNotifications(t, custID, db.NotifManualRejected) != 1 || countNotifications(t, custID, db.NotifKCCredited) != 1 {
		t.Error("el cliente debería tener un aviso de acreditación y otro de rechazo")
	}

	// Si el rechazo fue un error (el cliente habló con soporte), solo se aprueba
	// pidiéndolo explícitamente desde el panel, y se acredita una sola vez.
	if _, err := ApproveManualPayment(conn, second.ID, "test", false); !errors.Is(err, db.ErrManualAlreadyReviewed) {
		t.Errorf("aprobar un rechazado sin pedirlo: %v", err)
	}
	fixed, err := ApproveManualPayment(conn, second.ID, "Panel: soporte", true)
	if err != nil || fixed.Status != "approved" || fixed.RechargeID == nil {
		t.Fatalf("aprobar tras rechazo = %+v %v", fixed, err)
	}
	if _, err := ApproveManualPayment(conn, second.ID, "test", true); !errors.Is(err, db.ErrManualAlreadyReviewed) {
		t.Errorf("una aprobada no debe acreditarse otra vez: %v", err)
	}
	conn.QueryRow(`SELECT kc_balance FROM customers WHERE id=$1`, custID).Scan(&balance)
	conn.QueryRow(`SELECT COUNT(*) FROM kc_recharges WHERE customer_id=$1`, custID).Scan(&recharges)
	if balance != 2500+second.KCAmount || recharges != 2 {
		t.Errorf("tras aprobar el rechazado: saldo = %d, recargas = %d", balance, recharges)
	}
	var note string
	conn.QueryRow(`SELECT note FROM kc_recharges WHERE id=$1`, *fixed.RechargeID).Scan(&note)
	if !strings.Contains(note, "aprobado tras rechazo") {
		t.Errorf("la recarga debería indicar que se aprobó tras un rechazo: %q", note)
	}

	// A los 30 días se borran las imágenes (el registro queda).
	conn.Exec(`UPDATE manual_payment_requests SET created_at = NOW() - INTERVAL '31 days' WHERE customer_id=$1`, custID)
	if n := purgeExpiredProofs(conn, time.Now()); n != 2 {
		t.Errorf("borrados = %d, se esperaban 2", n)
	}
	if mem.count() != 0 {
		t.Errorf("deberían quedar 0 archivos, quedan %d", mem.count())
	}
	after, _ := db.GetManualPaymentRequest(conn, first.ID)
	if !after.ProofDeleted || after.ProofKey != nil || after.Status != "approved" {
		t.Errorf("después de borrar: %+v", after)
	}
	if _, _, err := LoadProof(context.Background(), after); err == nil {
		t.Error("un comprobante borrado no debería poder abrirse")
	}
}

func TestPagoManual_DesactivadoSinAlmacenamiento(t *testing.T) {
	conn := setupShopTestDB(t)
	SetManualPaymentsConfig(nil, "", "", "")
	custID, cleanup := newShopTestCustomer(t, conn, 0)
	t.Cleanup(cleanup)
	w := uploadProof(t, conn, custID, map[string]string{"package_id": "gamer", "method": "yape", "confirm": "true"}, []byte("x"))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("sin almacenamiento debería responder 503, obtuve %d", w.Code)
	}
}
