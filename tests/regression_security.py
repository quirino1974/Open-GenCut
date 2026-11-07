#!/usr/bin/env python3
"""Regressione della sicurezza: utenti, sessioni, ruoli, password (docs/SICUREZZA.txt).

Stessi criteri delle altre suite: esito 0 se tutti i controlli passano, dati di test creati
e rimossi dalla suite. Usa l'admin (GENCUT_ADMIN_PASSWORD) per creare gli utenti di prova.

Variabili d'ambiente: GENCUT_ADMIN_PASSWORD (obbligatoria), GENCUT_BASE_URL (opzionale).
"""
import os
import sys
import time
import urllib.request
import urllib.error

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from session import API, login, new_opener, raw_call, call  # noqa: E402

passed = 0
failed = []


def check(name, cond, detail=""):
    global passed
    if cond:
        passed += 1
        print(f"  OK   {name}")
    else:
        failed.append(name)
        print(f"  FAIL {name} {detail}")


def section(title):
    print(f"\n== {title}")


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def page_status(opener, path):
    """Stato di una pagina senza seguire i redirect: restituisce (status, location)."""
    op = urllib.request.build_opener(NoRedirect)
    try:
        resp = op.open(urllib.request.Request(API.replace("/api", "") + path), timeout=20)
        return resp.status, None
    except urllib.error.HTTPError as e:
        return e.code, e.headers.get("Location")


def tester_checks():
    """Verifiche di sicurezza possibili con l'utente di test (manager), senza admin.
    Non tenta mai password errate: il blocco per tentativi è verificato solo nella modalità admin."""
    from session import TEST_USER, call as tcall, raw_call as trc
    section("Utente di test (senza admin)")
    op = new_opener()
    st, p, _ = raw_call(op, "GET", "/agents")
    check("API senza sessione -> 401 unauthenticated", st == 401 and p.get("error") == "unauthenticated", (st, p))
    st, p, _ = raw_call(op, "GET", "/health")
    check("health resta pubblico", st == 200, (st, p))
    st, loc = page_status(op, "/index.html")
    check("pagina senza sessione -> redirect a /login.html", st == 302 and loc and loc.endswith("/login.html"), (st, loc))
    st, loc = page_status(op, "/login.html")
    check("pagina di login accessibile senza sessione", st == 200, (st, loc))
    tpwd = os.environ.get("GENCUT_TEST_PASSWORD")
    lo = new_opener()
    req = urllib.request.Request(API + "/auth/login", data=('{"username":"%s","password":"%s"}' % (TEST_USER, tpwd)).encode(),
                                 method="POST", headers={"Content-Type": "application/json"})
    resp = lo.open(req, timeout=30)
    sc = resp.headers.get("Set-Cookie", "")
    check("cookie di sessione HttpOnly e SameSite=Strict", "HttpOnly" in sc and "SameSite=Strict" in sc, sc)
    check("cookie di sessione con scadenza di 8 ore", "Max-Age=28800" in sc, sc)
    check("su HTTP il cookie non ha Secure", "Secure" not in sc, sc)
    tl = new_opener()
    pr = tl.open(urllib.request.Request(API + "/auth/login", data=('{"username":"%s","password":"%s"}' % (TEST_USER, tpwd)).encode(),
                                        method="POST", headers={"Content-Type": "application/json", "X-Forwarded-Proto": "https"}), timeout=30)
    psc = pr.headers.get("Set-Cookie", "")
    check("dietro il proxy HTTPS (X-Forwarded-Proto: https) il cookie ha Secure", "Secure" in psc and "HttpOnly" in psc, psc)
    st, p, _ = raw_call(new_opener(), "POST", "/auth/login", {"username": f"nessuno-{int(time.time())}", "password": "x"})
    check("utente inesistente -> 401 invalid_credentials", st == 401 and p.get("error") == "invalid_credentials", (st, p))
    st, p, _ = trc(lo, "GET", "/auth/me")
    check("me restituisce il ruolo manager dell'utente di test", st == 200 and p.get("role") == "manager", (st, p))
    st, p, _ = trc(lo, "GET", "/users")
    check("manager non gestisce gli utenti -> 403", st == 403 and p.get("error") == "forbidden", (st, p))
    st, p, _ = trc(lo, "POST", "/users", {"username": "sec-x", "role": "viewer"})
    check("manager non crea utenti -> 403", st == 403, (st, p))
    st, p, _ = trc(lo, "GET", "/audit?limit=5")
    check("manager legge il tracciamento", st == 200, (st, p))
    st, p, _ = trc(lo, "POST", "/integrations/resources", {"name": f"sec-tester-{int(time.time())}", "kind": "api", "status": "enabled"})
    if st == 201:
        trc(lo, "DELETE", f"/integrations/resources/{p['id']}")
        st2, aud, _ = trc(lo, "GET", "/audit?limit=20")
        mine = [e for e in aud if e.get("actor") == TEST_USER]
        check("il tracciamento registra qa-tester come actor", len(mine) > 0, aud[:2])
    st, p, _ = trc(lo, "POST", "/auth/logout")
    check("logout 204", st == 204, st)
    st, p, _ = trc(lo, "GET", "/auth/me")
    check("dopo il logout la sessione non vale più", st == 401, (st, p))
    skip_admin_sections = ["Ruoli: permessi per tutti i ruoli", "Cambio password obbligatorio",
                           "Reset da admin", "Protezione dell'amministrazione", "Blocco dopo tentativi falliti"]
    for name in skip_admin_sections:
        skipped.append(name)
        print(f"  SKIP sezione '{name}': richiede GENCUT_ADMIN_PASSWORD")


skipped = []


def main():
    admin_pwd = os.environ.get("GENCUT_ADMIN_PASSWORD")
    if not admin_pwd:
        tester_checks()
        total = passed + len(failed)
        print(f"\nRisultato parziale: {passed}/{total} controlli superati, {len(skipped)} sezioni saltate (serve admin)")
        if failed:
            print("Falliti:")
            for f in failed:
                print(f"  - {f}")
            sys.exit(1)
        return
    stamp = int(time.time())
    created_users = []
    created_ids = []

    a_op, s, p = login("admin", admin_pwd)
    check("admin accede", s == 200, (s, p))
    admin_me = p
    check("admin ha ruolo admin", admin_me.get("role") == "admin", admin_me)

    def admin(method, path, body=None):
        st, payload, _ = raw_call(a_op, method, path, body)
        return st, payload

    # ------------------------------------------------------------------
    section("Accesso senza sessione")
    op = new_opener()
    s, p, _ = raw_call(op, "GET", "/agents")
    check("API senza sessione -> 401 unauthenticated", s == 401 and p.get("error") == "unauthenticated", (s, p))
    s, p, _ = raw_call(op, "GET", "/health")
    check("health resta pubblico", s == 200, (s, p))
    s, p = None, None
    st, loc = page_status(op, "/index.html")
    check("pagina senza sessione -> redirect a /login.html", st == 302 and loc and loc.endswith("/login.html"), (st, loc))
    st, loc = page_status(op, "/login.html")
    check("pagina di login accessibile senza sessione", st == 200, (st, loc))

    # ------------------------------------------------------------------
    section("Login: cookie e messaggi generici")
    lo = new_opener()
    req = urllib.request.Request(API + "/auth/login", data=b'{"username":"admin","password":"' + admin_pwd.encode() + b'"}',
                                 method="POST", headers={"Content-Type": "application/json"})
    resp = lo.open(req, timeout=30)
    sc = resp.headers.get("Set-Cookie", "")
    check("cookie di sessione HttpOnly", "HttpOnly" in sc and "gencut_session=" in sc, sc)
    check("cookie di sessione SameSite=Strict", "SameSite=Strict" in sc, sc)
    check("cookie di sessione con scadenza di 8 ore", "Max-Age=28800" in sc, sc)
    # Credenziali errate su un utente di prova: non si tentano password sbagliate sull'admin
    # (ogni fallimento conta per il blocco e bloccherebbe l'amministratore).
    wrong_user = f"sec-wrong-{stamp}"
    st_w, body_w = admin("POST", "/users", {"username": wrong_user, "role": "viewer", "password": "Errore-Prova-2026"})
    created_users.append(wrong_user)
    created_ids.append(body_w["user"]["id"])
    s, p, _ = raw_call(new_opener(), "POST", "/auth/login", {"username": wrong_user, "password": "sbagliata"})
    s2, p2, _ = raw_call(new_opener(), "POST", "/auth/login", {"username": f"nessuno-{stamp}", "password": "sbagliata"})
    check("credenziali errate -> 401 invalid_credentials", s == 401 and p.get("error") == "invalid_credentials", (s, p))
    check("utente inesistente: stesso messaggio generico", s2 == 401 and p2 == p, (s2, p2))

    # ------------------------------------------------------------------
    section("Utenti di prova per ruolo")
    roles = ["viewer", "auditor", "operator", "integration", "manager"]
    users = {}
    for role in roles:
        uname = f"sec-{role}-{stamp}"
        st, body = admin("POST", "/users", {"username": uname, "role": role})
        check(f"creazione utente {role} 201 con password temporanea", st == 201 and body.get("temporary_password"), (st, body))
        created_users.append(uname)
        created_ids.append(body["user"]["id"])
        temp = body.get("temporary_password")
        # primo accesso: cambio obbligatorio
        uo, st2, pl = login(uname, temp)
        new_pwd = f"Nuova-{role}-2026x"
        st3, _, _ = raw_call(uo, "POST", "/auth/change-password", {"old_password": temp, "new_password": new_pwd})
        check(f"{role}: cambio password al primo accesso", st3 == 204, st3)
        uo2, st4, _ = login(uname, new_pwd)
        users[role] = uo2
        check(f"{role}: login con la nuova password", st4 == 200, st4)

    def as_role(role, method, path, body=None):
        st, payload, _ = raw_call(users[role], method, path, body)
        return st, payload

    section("Ruoli: permessi consentiti e negati")
    st, p = as_role("viewer", "GET", "/agents")
    check("viewer legge gli agenti", st == 200, (st, p))
    st, p = as_role("viewer", "POST", "/agents", {"name": "x", "origin": "internal"})
    check("viewer non crea agenti -> 403", st == 403 and p.get("error") == "forbidden", (st, p))
    st, p = as_role("viewer", "GET", "/audit")
    check("viewer non legge il tracciamento -> 403", st == 403, (st, p))
    st, p = as_role("auditor", "GET", "/audit")
    check("auditor legge il tracciamento", st == 200, (st, p))
    st, p = as_role("auditor", "POST", "/agents", {"name": "x", "origin": "internal"})
    check("auditor non scrive -> 403", st == 403, (st, p))
    st, p = as_role("operator", "POST", "/agents", {"name": "sec-op-agent", "origin": "internal"})
    check("operatore crea agenti", st == 201, (st, p))
    if st == 201:
        created_ids.append(("agent", p["id"]))
    st, p = as_role("operator", "POST", "/integrations/approvals/999999/approve", {})
    check("operatore non approva -> 403 (prima del controllo di esistenza)", st == 403, (st, p))
    st, p = as_role("operator", "GET", "/users")
    check("operatore non gestisce gli utenti -> 403", st == 403, (st, p))
    st, p = as_role("manager", "GET", "/users")
    check("manager non gestisce gli utenti -> 403", st == 403, (st, p))
    st, p = as_role("integration", "GET", "/agents")
    check("integration non legge il registro -> 403", st == 403, (st, p))
    st, p = as_role("integration", "POST", "/integrations/check", {"agent_id": 999999, "resource_id": 999999})
    check("integration può chiamare /check (qui: 404 per risorse inesistenti)", st == 404, (st, p))
    st, p = as_role("integration", "POST", "/guardrails", {"name": "x", "type": "prompt_injection", "scope": "global"})
    check("integration non gestisce le guardrail -> 403", st == 403, (st, p))
    st, p = admin("GET", "/users")
    check("admin gestisce gli utenti", st == 200 and isinstance(p, list), (st, p))

    # ------------------------------------------------------------------
    section("Cambio password obbligatorio")
    tmp_user = f"sec-gate-{stamp}"
    st, body = admin("POST", "/users", {"username": tmp_user, "role": "viewer"})
    created_users.append(tmp_user)
    created_ids.append(body["user"]["id"])
    go, st2, pl = login(tmp_user, body["temporary_password"])
    check("primo accesso con password temporanea: must_change true", pl.get("must_change_password") is True, pl)
    st, p, _ = raw_call(go, "GET", "/agents")
    check("con cambio obbligatorio le API danno 403 password_change_required",
          st == 403 and p.get("error") == "password_change_required", (st, p))
    st, p, _ = raw_call(go, "GET", "/auth/me")
    check("me resta accessibile durante il cambio", st == 200 and p.get("must_change_password") is True, (st, p))
    st, p, _ = raw_call(go, "POST", "/auth/change-password", {"old_password": body["temporary_password"], "new_password": tmp_user})
    check("password uguale al nome utente -> 400", st == 400, (st, p))
    st, p, _ = raw_call(go, "POST", "/auth/change-password", {"old_password": body["temporary_password"], "new_password": "corta"})
    check("password troppo corta -> 400", st == 400, (st, p))
    st, p, _ = raw_call(go, "POST", "/auth/change-password", {"old_password": "sbagliata", "new_password": "Valida-2026-xx"})
    check("password attuale errata -> 401", st == 401, (st, p))
    st, p, _ = raw_call(go, "POST", "/auth/change-password", {"old_password": body["temporary_password"], "new_password": "Valida-2026-xx"})
    check("cambio password riuscito 204", st == 204, (st, p))
    st, p, _ = raw_call(go, "GET", "/agents")
    check("dopo il cambio le API rispondono", st == 200, (st, p))

    # ------------------------------------------------------------------
    section("Reset da admin")
    st, body = admin("POST", f"/users/{created_ids[-1]}/reset-password")
    check("reset password 200 con password temporanea", st == 200 and body.get("temporary_password"), (st, body))
    st, p, _ = raw_call(go, "GET", "/agents")
    check("il reset invalida la sessione precedente", st == 401, (st, p))
    ro, st2, pl = login(tmp_user, body["temporary_password"])
    check("dopo il reset: must_change di nuovo true", st2 == 200 and pl.get("must_change_password") is True, pl)

    # ------------------------------------------------------------------
    section("Protezione dell'amministrazione")
    st, me = admin("GET", "/auth/me")
    st2, users_list = admin("GET", "/users")
    my_id = next(u["id"] for u in users_list if u["username"] == "admin")
    st, p = admin("PUT", f"/users/{my_id}", {"role": "viewer", "active": True})
    check("admin non può demansionare se stesso -> 409", st == 409 and p.get("error") == "invalid_state", (st, p))
    st, p = admin("DELETE", f"/users/{my_id}")
    check("admin non può cancellare se stesso -> 409", st == 409, (st, p))
    st, p = admin("POST", "/users", {"username": f"sec-bad-{stamp}", "role": "superuser"})
    check("ruolo non previsto -> 400", st == 400, (st, p))
    st, p = admin("POST", "/users", {"username": "a!", "role": "viewer"})
    check("nome utente non valido -> 400", st == 400, (st, p))
    st, p = admin("GET", "/users")
    check("la lista utenti non contiene hash né password", "password_hash" not in str(p) and "temporary_password" not in str(p), "")

    # ------------------------------------------------------------------
    section("Blocco dopo tentativi falliti")
    lock_user = f"sec-lock-{stamp}"
    st, body = admin("POST", "/users", {"username": lock_user, "role": "viewer", "password": "Blocco-Prova-2026"})
    created_users.append(lock_user)
    created_ids.append(body["user"]["id"])
    codes = []
    for _ in range(5):
        st, p, _ = raw_call(new_opener(), "POST", "/auth/login", {"username": lock_user, "password": "sbagliata-x"})
        codes.append(st)
    check("cinque tentativi errati -> 401", codes == [401] * 5, codes)
    st, p, _ = raw_call(new_opener(), "POST", "/auth/login", {"username": lock_user, "password": "Blocco-Prova-2026"})
    check("sesto tentativo, anche con la password giusta -> 429 too_many_attempts",
          st == 429 and p.get("error") == "too_many_attempts", (st, p))

    # ------------------------------------------------------------------
    section("Logout")
    lo2, st, _ = login("admin", admin_pwd)
    st, p, _ = raw_call(lo2, "POST", "/auth/logout")
    check("logout 204", st == 204, st)
    st, p, _ = raw_call(lo2, "GET", "/auth/me")
    check("dopo il logout la sessione non vale più", st == 401, (st, p))

    # ------------------------------------------------------------------
    section("Attore del tracciamento")
    st, res = admin("POST", "/integrations/resources", {"name": f"sec-res-{stamp}", "kind": "api", "status": "enabled"})
    if st == 201:
        created_ids.append(("res", res["id"]))
    st, aud = admin("GET", "/audit?limit=20")
    mine = [e for e in aud if e.get("entity") == "integration_resource" and f"sec-res-{stamp}" in (e.get("detail") or "")]
    check("il tracciamento registra l'admin come actor", mine and mine[0]["actor"] == "admin", mine[:1])
    st, aud2 = as_role("auditor", "GET", "/audit?limit=20")
    check("auditor vede il tracciamento", st == 200, st)

    # ------------------------------------------------------------------
    section("Pulizia")
    for item in created_ids:
        if isinstance(item, tuple):
            kind, rid = item
            if kind == "agent":
                admin("DELETE", f"/agents/{rid}")
            elif kind == "res":
                admin("DELETE", f"/integrations/resources/{rid}")
    for uid in created_ids:
        if not isinstance(uid, tuple):
            admin("DELETE", f"/users/{uid}")
    st, lst = admin("GET", "/users")
    left = [u["username"] for u in lst if u["username"].startswith("sec-")]
    check("utenti di prova rimossi", left == [], left)

    total = passed + len(failed)
    print(f"\nRisultato: {passed}/{total} controlli superati")
    if failed:
        print("Falliti:")
        for f in failed:
            print(f"  - {f}")
        sys.exit(1)


if __name__ == "__main__":
    main()
