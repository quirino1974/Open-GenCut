package main

import (
	"net"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// Dati personali: formati comuni che devono essere mascherati e casi che non devono esserlo.
func TestScanPIIFormats(t *testing.T) {
	mask := func(in string) (string, map[string]int) {
		return scanPII(normalizeForDetection(in), nil, true)
	}
	masked := []struct{ name, in string }{
		{"cellulare con spazio", "333 1234567"},
		{"cellulare senza spazi", "3331234567"},
		{"cellulare a gruppi 3-3-4", "333 123 4567"},
		{"cellulare con trattini", "333-123-4567"},
		{"internazionale compatto", "+39 3331234567"},
		{"internazionale a gruppi", "+39 333 123 4567"},
		{"prefisso 0039", "0039 333 1234567"},
		{"fisso 2+8", "06 12345678"},
		{"fisso a gruppi", "06 1234 5678"},
		{"fisso compatto", "0612345678"},
		{"codice fiscale minuscolo", "rssmra85t10a562s"},
		{"codice fiscale maiuscolo", "RSSMRA85T10A562S"},
		{"IBAN minuscolo", "it60x0542811101000000123456"},
		{"IBAN con spazi", "IT60 X054 2811 1010 0000 0123 456"},
		{"carta con spazio non separabile", "4111 1111 1111 1111"},
		{"carta con caratteri invisibili", "4111​1111​1111​1111"},
		{"carta a larghezza piena", "４１１１ １１１１ １１１１ １１１１"},
		{"email", "mario.rossi+x@example.co.uk"},
	}
	for _, c := range masked {
		t.Run("maschera "+c.name, func(t *testing.T) {
			out, counts := mask(c.in)
			if len(counts) == 0 {
				t.Fatalf("non mascherato: %q -> %q", c.in, out)
			}
			if strings.ContainsAny(out, "0123456789") && !strings.Contains(out, "[PII:") {
				t.Fatalf("restano cifre in chiaro: %q", out)
			}
		})
	}
	notMasked := []struct{ name, in string }{
		{"carta con Luhn non valido", "numero 4539 1488 0343 6468 ok"},
		{"IBAN con checksum errato", "IT61X0542811101000000123456"},
		{"16 cifre senza Luhn", "4111 1111 1111 1112"},
		{"data", "il 2026-11-20 alle 17:04"},
		{"testo normale", "ciao mondo, ci vediamo domani"},
	}
	for _, c := range notMasked {
		t.Run("non maschera "+c.name, func(t *testing.T) {
			out, counts := mask(c.in)
			if len(counts) != 0 {
				t.Fatalf("mascherato per errore: %q -> %q (%v)", c.in, out, counts)
			}
		})
	}
}

// La normalizzazione non deve alterare il testo quando non c'è niente da mascherare:
// qui si controlla solo che tolga davvero invisibili e spazi speciali.
func TestNormalizeForDetection(t *testing.T) {
	got := normalizeForDetection("a​b­c d１")
	if got != "abc d1" {
		t.Fatalf("normalizzazione inattesa: %q", got)
	}
}

// Costi: le somme in decimillesimi interi non hanno gli errori della virgola mobile.
func TestCostUnits(t *testing.T) {
	if got := costUnits(0.1) + costUnits(0.1) + costUnits(0.1); got != costUnits(0.3) {
		t.Fatalf("0,1+0,1+0,1 = %d decimillesimi, atteso %d", got, costUnits(0.3))
	}
	if costUnits(0.0001) != 1 || costUnits(1.5) != 15000 || costUnits(0) != 0 {
		t.Fatalf("conversione errata")
	}
}

// Rete in uscita: gli indirizzi dei metadati e locali al collegamento sono sempre bloccati,
// le reti private solo se non c'è il consenso esplicito.
func TestIPBlocked(t *testing.T) {
	old := os.Getenv("GATEWAY_ALLOW_PRIVATE_NETWORKS")
	defer os.Setenv("GATEWAY_ALLOW_PRIVATE_NETWORKS", old)

	always := []string{"169.254.169.254", "169.254.0.1", "fe80::1", "0.0.0.0", "224.0.0.1", "255.255.255.255"}
	private := []string{"127.0.0.1", "10.1.2.3", "192.168.50.20", "172.16.5.5", "::1", "100.64.0.1"}
	public := []string{"8.8.8.8", "93.184.216.34", "2606:4700:4700::1111"}

	os.Setenv("GATEWAY_ALLOW_PRIVATE_NETWORKS", "")
	for _, s := range always {
		if !ipBlocked(net.ParseIP(s)) {
			t.Errorf("%s dovrebbe essere sempre bloccato", s)
		}
	}
	for _, s := range private {
		if !ipBlocked(net.ParseIP(s)) {
			t.Errorf("%s dovrebbe essere bloccato senza consenso", s)
		}
	}
	for _, s := range public {
		if ipBlocked(net.ParseIP(s)) {
			t.Errorf("%s non dovrebbe essere bloccato", s)
		}
	}

	os.Setenv("GATEWAY_ALLOW_PRIVATE_NETWORKS", "true")
	for _, s := range private {
		if ipBlocked(net.ParseIP(s)) {
			t.Errorf("%s dovrebbe essere consentito con il consenso", s)
		}
	}
	for _, s := range always {
		if !ipBlocked(net.ParseIP(s)) {
			t.Errorf("%s resta bloccato anche con il consenso", s)
		}
	}
}

// checkEndpointPolicy ferma in anticipo gli indirizzi scritti come numeri.
func TestCheckEndpointPolicy(t *testing.T) {
	os.Setenv("GATEWAY_ALLOW_PRIVATE_NETWORKS", "true")
	if err := checkEndpointPolicy("http://169.254.169.254/latest/meta-data/"); err == nil {
		t.Error("l'indirizzo dei metadati dovrebbe essere rifiutato")
	}
	if err := checkEndpointPolicy("http://[fe80::1]:8080/x"); err == nil {
		t.Error("indirizzo IPv6 locale al collegamento da rifiutare")
	}
	if err := checkEndpointPolicy("http://127.0.0.1:8082/mcp"); err != nil {
		t.Errorf("con il consenso il loopback va bene: %v", err)
	}
	if err := checkEndpointPolicy("https://mcp.example.test/mcp"); err != nil {
		t.Errorf("un nome host non si rifiuta in anticipo: %v", err)
	}
	if err := checkEndpointPolicy(""); err != nil {
		t.Errorf("endpoint vuoto ammesso: %v", err)
	}
}

// Errori del database: i valori fuori intervallo diventano errori di validazione, non 500.
func TestDbErrNumberRange(t *testing.T) {
	if dbErrNumber(nil) != 0 {
		t.Error("nil dovrebbe dare 0")
	}
}

// clientIP: senza proxy fidati X-Forwarded-For si ignora; con proxy fidato si prende il primo
// indirizzo non fidato da destra, così un client non può inventarsi l'indirizzo.
func TestClientIP(t *testing.T) {
	old := os.Getenv("TRUSTED_PROXY_CIDRS")
	defer os.Setenv("TRUSTED_PROXY_CIDRS", old)
	mk := func(remote, xff string) *httptest.ResponseRecorder { return nil }
	_ = mk
	req := func(remote, xff string) string {
		r := httptest.NewRequest("POST", "/api/auth/login", nil)
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return clientIP(r)
	}
	os.Setenv("TRUSTED_PROXY_CIDRS", "")
	if got := req("203.0.113.9:5555", "1.2.3.4"); got != "203.0.113.9" {
		t.Errorf("senza proxy fidati l'intestazione va ignorata: %s", got)
	}
	os.Setenv("TRUSTED_PROXY_CIDRS", "172.18.0.0/16")
	if got := req("172.18.0.2:4444", "198.51.100.7"); got != "198.51.100.7" {
		t.Errorf("da proxy fidato si legge l'intestazione: %s", got)
	}
	if got := req("172.18.0.2:4444", "6.6.6.6, 198.51.100.7"); got != "198.51.100.7" {
		t.Errorf("si prende l'indirizzo più a destra non fidato, non quello falsificato a sinistra: %s", got)
	}
	if got := req("203.0.113.9:5555", "1.2.3.4"); got != "203.0.113.9" {
		t.Errorf("da un indirizzo non fidato l'intestazione resta ignorata: %s", got)
	}
	if got := req("172.18.0.2:4444", ""); got != "172.18.0.2" {
		t.Errorf("senza intestazione si usa l'indirizzo del proxy: %s", got)
	}
}
