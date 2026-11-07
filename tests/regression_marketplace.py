#!/usr/bin/env python3
"""Regressione del punto 7 della specifica: marketplace degli agenti.

Stessi criteri di regression_backend.py: solo HTTP e libreria standard, dati di test
creati e rimossi dalla suite, esito 0 se tutti i controlli passano.

Copre: inserzioni (validazione, CRUD), richieste (condizioni di ammissibilità), decisione
(approva, rifiuta, transizioni), provisioning (origine marketplace, costo da prezzo,
una sola volta), filtri, audit.

Uso:  python3 tests/regression_marketplace.py [base_url]
"""
import json
import os
import sys
import urllib.error
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from session import call  # noqa: E402

BASE = (sys.argv[1] if len(sys.argv) > 1 else "http://localhost:8080").rstrip("/")
API = BASE + "/api"

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


def listing_body(**kw):
    body = {"provider": "Fornitore Test", "name": "mkt-agent", "version": "1.0", "framework": "LangGraph",
            "price_model": "subscription", "price": 12.5, "currency": "EUR", "certified": True}
    body.update(kw)
    return body


def main():
    listings = []
    agents = []

    # ------------------------------------------------------------------
    section("Inserzioni: validazione")
    s, b = call("POST", "/marketplace/listings", listing_body(provider=""))
    check("fornitore mancante -> 400", s == 400, (s, b))
    s, b = call("POST", "/marketplace/listings", listing_body(price_model="gratis"))
    check("modello di prezzo non valido -> 400", s == 400, (s, b))
    s, b = call("POST", "/marketplace/listings", listing_body(price_model="free", price=5))
    check("gratuito con prezzo -> 400", s == 400, (s, b))
    s, b = call("POST", "/marketplace/listings", listing_body(price=-1))
    check("prezzo negativo -> 400", s == 400, (s, b))
    s, b = call("POST", "/marketplace/listings", listing_body(price=1e20))
    check("prezzo oltre il limite del database -> 400", s == 400, (s, b))
    s, b = call("POST", "/marketplace/listings", listing_body(currency="GBP"))
    check("valuta non valida -> 400", s == 400, (s, b))
    s, b = call("POST", "/marketplace/listings", listing_body(security_review="forse"))
    check("revisione di sicurezza non valida -> 400", s == 400, (s, b))

    # ------------------------------------------------------------------
    section("Inserzioni: CRUD")
    s, b = call("POST", "/marketplace/listings", listing_body())
    check("inserzione creata 201 con valori predefiniti", s == 201 and b.get("security_review") == "pending"
          and b.get("status") == "hidden", (s, b))
    lid = b["id"]
    listings.append(lid)
    s, b = call("GET", f"/marketplace/listings/{lid}")
    check("dettaglio inserzione 200", s == 200 and b.get("name") == "mkt-agent", (s, b))
    s, b = call("PUT", f"/marketplace/listings/{lid}", listing_body(name="mkt-agent-2", security_review="approved",
                                                                   status="listed"))
    check("modifica inserzione 200", s == 200 and b.get("name") == "mkt-agent-2", (s, b))
    s, b = call("GET", "/marketplace/listings/999999")
    check("inserzione inesistente -> 404", s == 404, (s, b))
    s, lst = call("GET", "/marketplace/listings")
    check("elenco inserzioni", s == 200 and any(x["id"] == lid for x in lst), (s, lst))

    # ------------------------------------------------------------------
    section("Richieste: condizioni di ammissibilità")
    s, b = call("POST", "/marketplace/requests", {"listing_id": 999999, "requested_by": "qa"})
    check("richiesta su inserzione inesistente -> 404", s == 404, (s, b))
    s, b = call("POST", "/marketplace/requests", {"listing_id": lid, "requested_by": ""})
    check("richiesta senza richiedente -> 400", s == 400, (s, b))
    # Inserzione per il test di ammissibilità: non pubblicata.
    s, hidden = call("POST", "/marketplace/listings", listing_body(name="mkt-hidden", security_review="approved",
                                                                    status="hidden"))
    listings.append(hidden["id"])
    s, b = call("POST", "/marketplace/requests", {"listing_id": hidden["id"], "requested_by": "qa"})
    check("inserzione nascosta -> 409 invalid_state", s == 409 and b.get("error") == "invalid_state", (s, b))
    s, pending = call("POST", "/marketplace/listings", listing_body(name="mkt-pending", security_review="pending",
                                                                     status="listed"))
    listings.append(pending["id"])
    s, b = call("POST", "/marketplace/requests", {"listing_id": pending["id"], "requested_by": "qa"})
    check("revisione non approvata -> 409 invalid_state", s == 409, (s, b))

    # ------------------------------------------------------------------
    section("Richieste: decisione")
    s, b = call("POST", "/marketplace/requests", {"listing_id": lid, "requested_by": "qa", "justification": "test"})
    check("richiesta creata pending 201", s == 201 and b.get("status") == "pending", (s, b))
    r1 = b["id"]
    s, b = call("POST", f"/marketplace/requests/{r1}/provision", {})
    check("provisioning da pending -> 409", s == 409, (s, b))
    s, b = call("POST", f"/marketplace/requests/{r1}/approve", {"note": "ok"})
    check("approvazione 200", s == 200 and b.get("status") == "approved", (s, b))
    s, b = call("POST", f"/marketplace/requests/{r1}/approve", {})
    check("approvare di nuovo -> 409", s == 409, (s, b))
    s, b = call("POST", f"/marketplace/requests/{r1}/reject", {})
    check("rifiutare una approvata -> 409", s == 409, (s, b))
    s, b = call("POST", "/marketplace/requests", {"listing_id": lid, "requested_by": "qa2"})
    r2 = b["id"]
    s, b = call("POST", f"/marketplace/requests/{r2}/reject", {"note": "no"})
    check("rifiuto 200", s == 200 and b.get("status") == "rejected", (s, b))
    s, b = call("POST", f"/marketplace/requests/{r2}/approve", {})
    check("approvare una rifiutata -> 409", s == 409, (s, b))
    s, b = call("POST", "/marketplace/requests/999999/approve", {})
    check("richiesta inesistente -> 404", s == 404, (s, b))
    s, b = call("GET", "/marketplace/requests?status=bogus")
    check("filtro stato non valido -> 400", s == 400, (s, b))
    s, b = call("GET", "/marketplace/requests?status=approved")
    check("filtro per stato", s == 200 and all(q["status"] == "approved" for q in b), (s, b))

    # ------------------------------------------------------------------
    section("Provisioning")
    s, ag = call("POST", f"/marketplace/requests/{r1}/provision", {})
    check("provisioning 201 con l'agente", s == 201 and ag.get("origin") == "marketplace"
          and ag.get("status") == "registered", (s, ag))
    if s == 201:
        agents.append(ag["id"])
        check("provisioning: fornitore e versione dall'inserzione",
              ag.get("vendor") == "Fornitore Test" and ag.get("version") == "1.0", ag)
        check("provisioning: costo dal prezzo per abbonamento", ag.get("cost_monthly") == 12.5
              and ag.get("currency") == "EUR", ag)
    s, b = call("POST", f"/marketplace/requests/{r1}/provision", {})
    check("provisioning una sola volta -> 409", s == 409, (s, b))
    s, q = call("GET", f"/marketplace/requests/{r1}")
    check("richiesta provisioned con agente collegato", q.get("status") == "provisioned"
          and q.get("agent_id") == (ag or {}).get("id"), q)

    s, per = call("POST", "/marketplace/listings", listing_body(name="mkt-peruse", price_model="per_use",
                                                                price=0.02, security_review="approved",
                                                                status="listed"))
    listings.append(per["id"])
    s, rq = call("POST", "/marketplace/requests", {"listing_id": per["id"], "requested_by": "qa"})
    call("POST", f"/marketplace/requests/{rq['id']}/approve", {})
    s, ag2 = call("POST", f"/marketplace/requests/{rq['id']}/provision", {})
    check("per uso: costo mensile zero", s == 201 and ag2.get("cost_monthly") == 0, (s, ag2))
    if s == 201:
        agents.append(ag2["id"])

    # ------------------------------------------------------------------
    section("Audit")
    s, aud = call("GET", "/audit?limit=200")
    keys = []
    for e in aud:
        try:
            keys.append(json.loads(e["detail"]).get("key"))
        except (TypeError, json.JSONDecodeError, AttributeError):
            pass
    check("audit: chiavi del marketplace presenti",
          {"listing_saved", "request_created", "request_approved", "request_provisioned"} <= set(keys), keys[:12])

    # ------------------------------------------------------------------
    section("Pulizia")
    for aid in agents:
        call("DELETE", f"/agents/{aid}")
    for lid2 in listings:
        call("DELETE", f"/marketplace/listings/{lid2}")
    s, lst = call("GET", "/marketplace/listings")
    check("inserzioni di test rimosse", s == 200 and not any(x["id"] in listings for x in lst), lst)

    total = passed + len(failed)
    print(f"\nRisultato: {passed}/{total} controlli superati")
    if failed:
        print("Falliti:")
        for f in failed:
            print(f"  - {f}")
        sys.exit(1)


if __name__ == "__main__":
    main()
