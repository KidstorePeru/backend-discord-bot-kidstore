package store

import (
	"testing"

	"github.com/go-playground/validator/v10"
)

// TestFriendlyBindError cubre el punto 4: un error de validación de
// contraseña corta debe traducirse a un mensaje comprensible en vez del
// texto crudo del validador de Gin (del estilo "Key: 'X.Password' Error:
// Field validation for 'Password' failed on the 'min' tag").
func TestFriendlyBindError(t *testing.T) {
	type registerLike struct {
		Password string `binding:"required,min=8"`
	}
	type resetLike struct {
		NewPassword string `binding:"omitempty,min=8"`
	}
	type emailLike struct {
		Email string `binding:"required,email"`
	}

	v := validator.New()
	v.SetTagName("binding") // gin usa el tag "binding", no "validate"

	t.Run("contraseña corta en registro", func(t *testing.T) {
		err := v.Struct(registerLike{Password: "1234"})
		if err == nil {
			t.Fatal("se esperaba un error de validación")
		}
		got := friendlyBindError(err)
		want := "La contraseña debe tener al menos 8 caracteres."
		if got != want {
			t.Errorf("friendlyBindError = %q, want %q", got, want)
		}
	})

	t.Run("contraseña nueva corta en cambio de contraseña", func(t *testing.T) {
		err := v.Struct(resetLike{NewPassword: "abc"})
		if err == nil {
			t.Fatal("se esperaba un error de validación")
		}
		got := friendlyBindError(err)
		want := "La contraseña debe tener al menos 8 caracteres."
		if got != want {
			t.Errorf("friendlyBindError = %q, want %q", got, want)
		}
	})

	t.Run("correo inválido no expone detalles técnicos", func(t *testing.T) {
		err := v.Struct(emailLike{Email: "no-es-un-correo"})
		if err == nil {
			t.Fatal("se esperaba un error de validación")
		}
		got := friendlyBindError(err)
		if got == "" || got == err.Error() {
			t.Errorf("friendlyBindError no debe devolver el error crudo del validador: %q", got)
		}
	})
}
