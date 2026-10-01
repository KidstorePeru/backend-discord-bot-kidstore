package store

import (
	"database/sql"
	"errors"
	"strings"

	"KidStoreStore/src/db"
	"KidStoreStore/src/discordbot"
	"KidStoreStore/src/types"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// ErrAccountAlreadyExists: ya hay una cuenta con ese correo o usuario de Epic.
var ErrAccountAlreadyExists = errors.New("ya existe una cuenta con ese correo o usuario de Epic")

// ActivatePendingRegistration crea la cuenta real a partir de un registro
// pendiente — lo mismo que hace el enlace del correo de verificación
// (HandlerVerifyEmail), pero lo dispara un admin desde el panel cuando el correo
// no le llega al cliente (p. ej. el proveedor de correo está fallando). La
// contraseña es la que el cliente eligió al registrarse; el registro pendiente
// se borra para que su enlace ya no sirva.
func ActivatePendingRegistration(database *sql.DB, pending db.PendingRegistration) (types.Customer, error) {
	email := pending.Email
	customer := types.Customer{
		ID:           uuid.New(),
		EpicUsername: pending.EpicUsername,
		Email:        &email,
		PasswordHash: pending.PasswordHash,
		HasPassword:  true,
		IsVerified:   true,
	}
	if err := db.CreateVerifiedCustomer(database, customer); err != nil {
		if isUniqueViolation(err) {
			return types.Customer{}, ErrAccountAlreadyExists
		}
		return types.Customer{}, err
	}
	db.DeletePendingRegistration(database, pending.VerificationToken)
	discordbot.NotifyWelcome(customer)
	return customer, nil
}

// isUniqueViolation: clave única duplicada (código 23505 de Postgres). Se mira el
// código y no el texto, que cambia con el idioma del servidor ("duplicate key"
// en inglés, "llave duplicada" en español).
func isUniqueViolation(err error) bool {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		return pqErr.Code == "23505"
	}
	return strings.Contains(err.Error(), "unique") || strings.Contains(err.Error(), "duplicate")
}
