package fortnite

// Pruebas de la lectura del perfil common_core de una cuenta bot (V-Bucks y
// regalos enviados en las últimas 24 horas) — siempre con respuestas fijas
// o un servidor simulado, nunca contra la API real de Epic Games.

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

var profileNow = time.Date(2026, 10, 1, 18, 0, 0, 0, time.UTC)

func commonCoreBody(giftHistory string) []byte {
	attrs := ""
	if giftHistory != "" {
		attrs = `"gift_history": ` + giftHistory
	}
	return []byte(fmt.Sprintf(`{"profileRevision": 7, "profileId": "common_core", "profileChanges": [{"changeType": "fullProfileUpdate", "profile": {
		"items": {
			"a": {"templateId": "Currency:MtxPurchased", "quantity": 12000},
			"b": {"templateId": "Currency:MtxGiveaway", "quantity": 600},
			"c": {"templateId": "Token:something", "quantity": 99}
		},
		"stats": {"attributes": {%s}}
	}}]}`, attrs))
}

func hoursAgo(h float64) string {
	return profileNow.Add(-time.Duration(h * float64(time.Hour))).Format("2006-01-02T15:04:05.000Z")
}

func TestParseCommonCore_CuentaRegalosDeLasUltimas24Horas(t *testing.T) {
	// Dos regalos recientes (por ejemplo, enviados desde el juego con la
	// misma cuenta) y uno de hace dos días que ya no cuenta para el límite.
	body := commonCoreBody(fmt.Sprintf(`{"num_sent": 3, "sentTo": {}, "gifts": [
		{"offerId": "o1", "toAccountId": "x", "date": %q},
		{"offerId": "o2", "toAccountId": "y", "date": %q},
		{"offerId": "o3", "toAccountId": "z", "date": %q}
	]}`, hoursAgo(1), hoursAgo(23.5), hoursAgo(48)))

	p, err := parseCommonCoreProfile(body, profileNow)
	if err != nil {
		t.Fatal(err)
	}
	if p.VBucks != 12600 {
		t.Errorf("V-Bucks = %d, se esperaba 12600 (comprados + regalados por Epic)", p.VBucks)
	}
	remaining, ok := p.RemainingGifts()
	if !ok || remaining != 3 {
		t.Errorf("regalos disponibles = %d (conocido=%v), se esperaba 3", remaining, ok)
	}
}

func TestParseCommonCore_NuncaBajaDeCero(t *testing.T) {
	var gifts []string
	for i := 0; i < 7; i++ {
		gifts = append(gifts, fmt.Sprintf(`{"date": %q}`, hoursAgo(float64(i+1))))
	}
	body := commonCoreBody(`{"num_sent": 7, "gifts": [` + strings.Join(gifts, ",") + `]}`)
	p, err := parseCommonCoreProfile(body, profileNow)
	if err != nil {
		t.Fatal(err)
	}
	if remaining, ok := p.RemainingGifts(); !ok || remaining != 0 {
		t.Errorf("regalos disponibles = %d (conocido=%v), se esperaba 0", remaining, ok)
	}
}

func TestParseCommonCore_UsaSentToSiNoVieneGifts(t *testing.T) {
	body := commonCoreBody(fmt.Sprintf(`{"num_sent": 2, "sentTo": {"x": %q, "y": %q}}`, hoursAgo(2), hoursAgo(30)))
	p, err := parseCommonCoreProfile(body, profileNow)
	if err != nil {
		t.Fatal(err)
	}
	if remaining, ok := p.RemainingGifts(); !ok || remaining != 4 {
		t.Errorf("regalos disponibles = %d (conocido=%v), se esperaba 4", remaining, ok)
	}
}

func TestParseCommonCore_CuentaQueNuncaRegalo(t *testing.T) {
	p, err := parseCommonCoreProfile(commonCoreBody(`{"num_sent": 0}`), profileNow)
	if err != nil {
		t.Fatal(err)
	}
	if remaining, ok := p.RemainingGifts(); !ok || remaining != 5 {
		t.Errorf("regalos disponibles = %d (conocido=%v), se esperaba 5", remaining, ok)
	}
}

// Si Epic no manda el historial, o lo manda en un formato que no se
// reconoce, no se adivina: remaining_gifts se deja como está.
func TestParseCommonCore_SinHistorialConfiableNoSeTocaNada(t *testing.T) {
	cases := map[string]string{
		"sin gift_history":       "",
		"envíos sin fechas":      `{"num_sent": 4}`,
		"fecha con otro formato": `{"num_sent": 1, "gifts": [{"date": "01/10/2026"}]}`,
	}
	for name, history := range cases {
		t.Run(name, func(t *testing.T) {
			p, err := parseCommonCoreProfile(commonCoreBody(history), profileNow)
			if err != nil {
				t.Fatal(err)
			}
			if p.VBucks != 12600 {
				t.Errorf("los V-Bucks se deben seguir leyendo igual, obtuve %d", p.VBucks)
			}
			if _, ok := p.RemainingGifts(); ok {
				t.Error("sin un historial confiable no se deberían reportar regalos disponibles")
			}
		})
	}
}

func TestParseCommonCore_RespuestaInvalida(t *testing.T) {
	if _, err := parseCommonCoreProfile([]byte(`{"profileChanges": []}`), profileNow); err == nil {
		t.Error("un perfil vacío debería ser un error")
	}
	if _, err := parseCommonCoreProfile([]byte(`no es json`), profileNow); err == nil {
		t.Error("una respuesta que no es JSON debería ser un error")
	}
}

func TestGetBotProfile_ConsultaElPerfilCommonCore(t *testing.T) {
	var gotPath, gotQuery, gotAuth string
	withMockEpic(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotAuth = r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		w.Write(commonCoreBody(fmt.Sprintf(`{"num_sent": 1, "gifts": [{"date": %q}]}`,
			time.Now().Add(-time.Hour).UTC().Format(time.RFC3339))))
	})

	account := newTestAccount()
	p, err := GetBotProfile(nil, account)
	if err != nil {
		t.Fatal(err)
	}
	wantPath := "/fortnite/api/game/v2/profile/" + strings.ReplaceAll(account.ID.String(), "-", "") + "/client/QueryProfile"
	if gotPath != wantPath || gotQuery != "profileId=common_core" {
		t.Errorf("se consultó %s?%s, se esperaba %s?profileId=common_core", gotPath, gotQuery, wantPath)
	}
	if gotAuth != "Bearer test-token" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if remaining, ok := p.RemainingGifts(); !ok || remaining != 4 || p.VBucks != 12600 {
		t.Errorf("perfil = %+v, regalos disponibles = %d (conocido=%v)", p, remaining, ok)
	}

	vbucks, err := GetRealVBucksBalance(nil, account)
	if err != nil || vbucks != 12600 {
		t.Errorf("GetRealVBucksBalance = %d, %v; se esperaba 12600", vbucks, err)
	}
}

func TestGetBotProfile_ErrorDeEpic(t *testing.T) {
	withMockEpic(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"errorCode": "errors.com.epicgames.common.server_error"}`))
	})
	if _, err := GetBotProfile(nil, newTestAccount()); err == nil {
		t.Error("un 500 de Epic debería devolver error (y no tocar ni V-Bucks ni regalos)")
	}
}
