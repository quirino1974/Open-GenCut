#!/usr/bin/env python3
"""Setup di sicurezza per le suite di regressione (docs/SICUREZZA.txt).

Passi (una sola volta, poi è idempotente):
  1. login dell'admin: se è ancora "admin"/"admin", cambia la password in GENCUT_ADMIN_PASSWORD;
  2. crea l'utente di test (ruolo manager) se manca, con password temporanea;
  3. cambia la password temporanea in GENCUT_TEST_PASSWORD (il cambio è obbligatorio al primo accesso).

Variabili d'ambiente (nessuna credenziale è scritta nei file):
  GENCUT_ADMIN_PASSWORD  nuova password dell'admin (obbligatoria, minimo 8 caratteri)
  GENCUT_TEST_PASSWORD   password dell'utente di test (obbligatoria)
  GENCUT_TEST_USER       nome dell'utente di test (predefinito: qa-tester)
  GENCUT_BASE_URL        indirizzo dell'API (predefinito: http://localhost:8080)

Uso:
  export GENCUT_ADMIN_PASSWORD='...' GENCUT_TEST_PASSWORD='...'
  python3 tests/setup_test_user.py
Poi le suite si lanciano con la sola GENCUT_TEST_PASSWORD esportata.
"""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from session import TEST_USER, login, new_opener, raw_call  # noqa: E402


def need(name):
    v = os.environ.get(name)
    if not v:
        sys.exit(f"Manca la variabile {name}.")
    return v


def main():
    admin_pwd = need("GENCUT_ADMIN_PASSWORD")
    test_pwd = need("GENCUT_TEST_PASSWORD")

    # 1. admin: prima la password attuale (GENCUT_ADMIN_PASSWORD); solo se fallisce la password iniziale.
    # Ogni tentativo errato conta per il blocco: si evita di provare "admin" se non serve.
    opener, status, payload = login("admin", admin_pwd)
    if status == 200:
        print("admin: accesso con GENCUT_ADMIN_PASSWORD")
    else:
        opener, status, payload = login("admin", "admin")
        if status != 200:
            sys.exit("admin: accesso non riuscito (né GENCUT_ADMIN_PASSWORD né la password iniziale)")
        s, _, _ = raw_call(opener, "POST", "/auth/change-password",
                           {"old_password": "admin", "new_password": admin_pwd})
        if s != 204:
            sys.exit(f"admin: cambio password fallito ({s})")
        print("admin: password iniziale cambiata")

    # 2. utente di test: la password attuale è quella temporanea (appena creata o da reset)
    s, users, _ = raw_call(opener, "GET", "/users")
    if s != 200:
        sys.exit(f"elenco utenti: {s}")
    existing = [u for u in users if u["username"] == TEST_USER]
    if existing:
        print(f"utente {TEST_USER} già presente")
        _, status, payload = login(TEST_USER, test_pwd)
        if status == 200 and not payload.get("must_change_password"):
            print("utente di test: password già impostata")
            return
        s, body, _ = raw_call(opener, "POST", f"/users/{existing[0]['id']}/reset-password")
        if s != 200:
            sys.exit(f"reset password utente di test: {s}")
        current = body["temporary_password"]
    else:
        s, body, _ = raw_call(opener, "POST", "/users", {"username": TEST_USER, "role": "manager"})
        if s != 201:
            sys.exit(f"creazione utente di test: {s}")
        current = body["temporary_password"]
        print(f"utente {TEST_USER} creato (ruolo manager)")

    # 3. cambio obbligatorio: dalla temporanea alla password di test
    t_opener, status, payload = login(TEST_USER, current)
    if status != 200:
        sys.exit(f"accesso con la password temporanea: {status}")
    s, _, _ = raw_call(t_opener, "POST", "/auth/change-password",
                       {"old_password": current, "new_password": test_pwd})
    if s != 204:
        sys.exit(f"cambio password utente di test: {s}")
    print("utente di test: password impostata")
    print("setup completato: esporta GENCUT_TEST_PASSWORD e lancia le suite")


if __name__ == "__main__":
    main()
