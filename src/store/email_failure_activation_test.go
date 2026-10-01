package store

// Regresiones: cuando el proveedor de correo rechazaba los envíos (p. ej. dominio
// sin verificar en Resend), el error solo quedaba en los logs y los clientes no
// podían registrarse. Ahora se avisa al admin y el admin puede activar a mano un
// registro pendiente.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"KidStoreStore/src/db"
	"KidStoreStore/src/types"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

func TestEmailErrorSummary_ExtraeElMotivoDeResend(t *testing.T) {
	err := errors.New(`resend error 403: {"statusCode":403,"message":"The kidstoreperu.com domain is not verified. Please, add and verify your domain on https://resend.com/domains","name":"validation_error"}`)
	got := emailErrorSummary(err)
	if !strings.Contains(got, "resend error 403") || !strings.Contains(got, "domain is not verified") || strings.Contains(got, "statusCode") {
		t.Errorf("resumen = %q", got)
	}
	long := errors.New(strings.Repeat("x", 500))
	if n := len([]rune(emailErrorSummary(long))); n > 302 {
		t.Errorf("el resumen debe recortarse, tiene %d caracteres", n)
	}
}

// captureEmailFailures reemplaza el aviso por Discord durante la prueba.
func captureEmailFailures(t *testing.T) *[]string {
	t.Helper()
	var mu sync.Mutex
	got := []string{}
	prev := onEmailFailure
	onEmailFailure = func(summary, from string) {
		mu.Lock()
		got = append(got, summary+" | from="+from)
		mu.Unlock()
	}
	t.Cleanup(func() { onEmailFailure = prev })
	return &got
}

func fakeResend(t *testing.T, status int, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	prev := resendAPIURL
	resendAPIURL = srv.URL
	t.Cleanup(func() { resendAPIURL = prev })
}

func TestSendEmail_AvisaCuandoElProveedorRechaza(t *testing.T) {
	got := captureEmailFailures(t)
	fakeResend(t, 403, `{"statusCode":403,"message":"The kidstoreperu.com domain is not verified.","name":"validation_error"}`)
	cfg := types.EnvConfig{ResendAPIKey: "re_test", SMTPFrom: "no-reply@kidstoreperu.com"}

	if err := sendEmail(cfg, "cliente@example.com", "Asunto", "<p>hola</p>"); err == nil {
		t.Fatal("un 403 de Resend debe devolver error")
	}
	if len(*got) != 1 || !strings.Contains((*got)[0], "domain is not verified") || !strings.Contains((*got)[0], "from=no-reply@kidstoreperu.com") {
		t.Fatalf("aviso = %v", *got)
	}
	if strings.Contains((*got)[0], "cliente@example.com") {
		t.Error("el aviso no debe incluir el correo del cliente")
	}
}

func TestSendEmail_NoAvisaSiSeEnvia(t *testing.T) {
	got := captureEmailFailures(t)
	fakeResend(t, 200, `{"id":"abc"}`)
	cfg := types.EnvConfig{ResendAPIKey: "re_test", SMTPFrom: "no-reply@kidstoreperu.com"}
	if err := sendEmail(cfg, "cliente@example.com", "Asunto", "<p>hola</p>"); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 0 {
		t.Errorf("no debe avisar si el correo se envió: %v", *got)
	}
}

func TestSendEmail_AvisaSinProveedorConfigurado(t *testing.T) {
	got := captureEmailFailures(t)
	if err := sendEmail(types.EnvConfig{}, "cliente@example.com", "Asunto", "<p>hola</p>"); err == nil {
		t.Fatal("sin proveedor debe devolver error")
	}
	if len(*got) != 1 || !strings.Contains((*got)[0], "no email provider configured") {
		t.Errorf("aviso = %v", *got)
	}
}

// El admin activa un registro pendiente: se crea la cuenta verificada con la
// contraseña que eligió el cliente y el enlace del correo deja de servir.
func TestActivatePendingRegistration(t *testing.T) {
	conn := setupShopTestDB(t)
	suffix := uuid.NewString()[:8]
	email := "pendiente-" + suffix + "@example.com"
	epic := "pend_" + suffix
	hash, _ := bcrypt.GenerateFromPassword([]byte("clave-del-cliente"), bcrypt.MinCost)
	token := "tok-" + uuid.NewString()
	if err := db.CreatePendingRegistration(conn, epic, email, string(hash), token, "es"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		conn.Exec(`DELETE FROM pending_registrations WHERE email=$1`, email)
		conn.Exec(`DELETE FROM customers WHERE email=$1`, email)
	})

	list, err := db.ListPendingRegistrations(conn)
	if err != nil {
		t.Fatal(err)
	}
	var id uuid.UUID
	for _, p := range list {
		if p.Email == email {
			id = p.ID
		}
	}
	if id == uuid.Nil {
		t.Fatal("el registro pendiente debe aparecer en la lista del admin")
	}
	pending, err := db.GetPendingRegistrationByID(conn, id)
	if err != nil {
		t.Fatal(err)
	}

	customer, err := ActivatePendingRegistration(conn, pending)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := db.GetCustomerByEmail(conn, email)
	if err != nil || saved.ID != customer.ID || !saved.IsVerified || saved.EpicUsername != epic {
		t.Fatalf("cuenta creada incorrecta: %+v err=%v", saved, err)
	}
	if bcrypt.CompareHashAndPassword([]byte(saved.PasswordHash), []byte("clave-del-cliente")) != nil {
		t.Error("la cuenta debe conservar la contraseña que eligió el cliente")
	}
	if _, err := db.GetPendingRegistration(conn, token); err == nil {
		t.Error("el registro pendiente debe borrarse (su enlace ya no sirve)")
	}

	// Un segundo intento (doble clic o el cliente verificó a la vez) no duplica la cuenta.
	if err := db.CreatePendingRegistration(conn, epic, email, string(hash), token+"b", "es"); err != nil {
		t.Fatal(err)
	}
	again, _ := db.GetPendingRegistration(conn, token+"b")
	if _, err := ActivatePendingRegistration(conn, again); !errors.Is(err, ErrAccountAlreadyExists) {
		t.Errorf("esperaba ErrAccountAlreadyExists, obtuve %v", err)
	}
}
