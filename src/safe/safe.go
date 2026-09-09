// Package safe da una red de seguridad contra panics en goroutines de
// fondo (workers periódicos, webhooks despachados con "go func(){}()").
//
// gin.Recovery() SOLO protege la goroutine que atiende cada request HTTP —
// no tiene ninguna visibilidad ni control sobre goroutines separadas que
// esa misma request (u otro código) lance aparte. Un panic sin recuperar en
// CUALQUIER goroutine tumba el proceso completo en Go, sin excepción — así
// que un solo nil pointer, índice fuera de rango, etc. en un webhook
// público (sin autenticar) o en el worker de pedidos podría cerrar todo el
// backend para todos los usuarios, no solo fallar esa request.
package safe

import (
	"log/slog"
	"runtime/debug"
)

// Run ejecuta fn recuperando cualquier panic — para goroutines "de una sola
// vez" (ej. el "go func(){}()" de un webhook). Si fn entra en pánico, se
// registra el error y el stack, y la goroutine simplemente termina ahí, en
// vez de tumbar todo el proceso.
func Run(name string, fn func()) {
	defer Recover(name)
	fn()
}

// Recover se usa con `defer safe.Recover("nombre")` al inicio de una
// goroutine o de una iteración dentro de un loop de ticker — permite que un
// worker periódico SIGA corriendo en el siguiente ciclo aunque una
// iteración puntual haya entrado en pánico, en vez de que la goroutine
// entera muera en silencio tras el primer panic (que sería peor que un
// crash: el sitio seguiría "arriba" pero ese worker dejaría de procesar
// pedidos/tokens/etc. para siempre hasta el próximo redeploy, sin ningún
// aviso).
func Recover(name string) {
	if r := recover(); r != nil {
		slog.Error("panic recuperado — el proceso sigue corriendo",
			"origen", name, "panic", r, "stack", string(debug.Stack()))
	}
}
