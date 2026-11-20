package main

// Regole di rete per le richieste che il server fa verso indirizzi scelti dagli utenti
// (gateway verso API e server MCP, sincronizzazione degli strumenti MCP).
//
// Sempre bloccati: indirizzi locali al collegamento (169.254.0.0/16, fe80::/10, comprese le
// interfacce dei metadati dei cloud), non specificati, multicast e broadcast.
// Bloccati salvo consenso esplicito: loopback, reti private e CGNAT. Il consenso si dà con
// la variabile GATEWAY_ALLOW_PRIVATE_NETWORKS=true, necessaria quando i server MCP stanno su
// una rete interna (come nel laboratorio).
//
// Il controllo avviene al momento della connessione, sull'indirizzo reale a cui ci si collega:
// resiste ai nomi che cambiano indirizzo dopo la verifica e ai reindirizzamenti.

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

var errBlockedAddress = errors.New("indirizzo non consentito dalla policy di rete")

// cgnat è la rete 100.64.0.0/10 (Carrier-Grade NAT), usata anche da alcuni servizi interni.
var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// allowPrivateNetworks dice se il gateway può raggiungere loopback e reti private.
func allowPrivateNetworks() bool {
	return strings.EqualFold(os.Getenv("GATEWAY_ALLOW_PRIVATE_NETWORKS"), "true")
}

// ipBlocked dice se il gateway non deve collegarsi a questo indirizzo.
func ipBlocked(ip net.IP) bool {
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
	}
	switch {
	case ip.IsUnspecified(), ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(), ip.IsMulticast(),
		ip.IsInterfaceLocalMulticast():
		return true
	}
	if ip4 := ip.To4(); ip4 != nil && ip4.Equal(net.IPv4bcast) {
		return true
	}
	if allowPrivateNetworks() {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || cgnat.Contains(ip)
}

// guardedControl è chiamata dal dialer prima di ogni connessione, con l'indirizzo già risolto.
func guardedControl(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errBlockedAddress
	}
	ip := net.ParseIP(host)
	if ip == nil || ipBlocked(ip) {
		return errBlockedAddress
	}
	return nil
}

// checkEndpointPolicy controlla in anticipo gli indirizzi scritti come numeri (nome host non risolto).
func checkEndpointPolicy(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return nil // la validazione dello schema e dell'host è altrove
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && ipBlocked(ip) {
		return errBlockedAddress
	}
	return nil
}

// safeHTTPClient restituisce un client che non segue più di 3 reindirizzamenti, ignora i proxy
// dell'ambiente e verifica l'indirizzo a ogni connessione.
func safeHTTPClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: guardedControl}
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: timeout,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("troppi reindirizzamenti")
			}
			return nil
		},
	}
}
