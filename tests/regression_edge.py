#!/usr/bin/env python3
"""Casi limite e concorrenza del backend Open-GenCut (ciclo di test finale, fase 1).

Stessi criteri di regression_backend.py: solo HTTP, nessuna libreria esterna, dati propri
creati e rimossi dalla suite, esito 0 se tutti i controlli passano.

Copre: valori estremi, campi mancanti, ID inesistenti, date errate, JSON malformato,
lunghezze in caratteri (non in byte), richieste concorrenti su limiti, approvazioni e audit.

Uso:  python3 tests/regression_edge.py [base_url]
"""
import json
import os
import sys
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from session import call, send_raw  # noqa: E402

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


def main():
    created_agents = []
    created_resources = []
    created_configs = []

    # ------------------------------------------------------------------
    section("Valori estremi e limiti numerici")
    s, b = call("POST", "/agents", {"name": "edge", "origin": "internal", "cost_monthly": 1e20})
    check("costo mensile enorme -> 400 (non 500)", s == 400, (s, b))
    if s == 201:
        created_agents.append(b["id"])
    s, b = call("POST", "/agents", {"name": "edge2", "origin": "internal", "cost_monthly": 99999999999999})
    check("costo al limite DECIMAL(18,4) accettato", s == 201, (s, b))
    if s == 201:
        created_agents.append(b["id"])

    s, b = call("POST", "/agents", {"name": "edge3", "origin": "internal"})
    a = b["id"]
    created_agents.append(a)
    s, b = call("POST", "/integrations/resources", {"name": "edge-res", "kind": "api", "status": "enabled"})
    r = b["id"]
    created_resources.append(r)
    call("POST", "/integrations/allowlist", {"agent_id": a, "resource_id": r})

    s, b = call("POST", "/integrations/limits", {"agent_id": a, "resource_id": r, "max_calls_per_day": 3000000000})
    check("max chiamate/giorno oltre INT -> 400", s == 400, (s, b))
    s, b = call("POST", "/integrations/limits", {"agent_id": a, "resource_id": r, "max_cost_per_month": 1e20})
    check("max costo/mese oltre DECIMAL -> 400", s == 400, (s, b))
    s, b = call("POST", "/integrations/limits", {"agent_id": a, "resource_id": r, "max_calls_per_day": 0})
    check("max chiamate 0 -> 400", s == 400, (s, b))
    s, b = call("POST", "/integrations/limits", {"agent_id": a, "resource_id": r, "max_calls_per_day": 2147483647})
    check("max chiamate al limite INT accettato", s == 201, (s, b))
    if s == 201:
        lim = b["id"]
        call("DELETE", f"/integrations/limits/{lim}")

    s, b = call("POST", "/integrations/check", {"agent_id": a, "resource_id": r, "cost": 1e20})
    check("costo dichiarato enorme in check -> 400", s == 400, (s, b))
    s, b = call("POST", "/integrations/check", {"agent_id": a, "resource_id": r, "cost": -1})
    check("costo negativo in check -> 400", s == 400, (s, b))

    s, b = call("POST", f"/lifecycle/agents/{a}/versions", {"version_label": "1.0"})
    s, b = call("POST", f"/lifecycle/agents/{a}/calls", {"tokens": 3000000000})
    check("token oltre INT -> 400", s == 400, (s, b))
    s, b = call("POST", f"/lifecycle/agents/{a}/calls", {"latency_ms": 3000000000})
    check("latenza oltre INT -> 400", s == 400, (s, b))
    s, b = call("POST", f"/lifecycle/agents/{a}/calls", {"cost": 1e20})
    check("costo chiamata enorme -> 400", s == 400, (s, b))

    # ------------------------------------------------------------------
    section("Lunghezze in caratteri, non in byte")
    s, b = call("POST", "/agents", {"name": "è" * 150, "origin": "internal"})
    check("nome di 150 caratteri accentati (300 byte) accettato", s == 201, (s, b))
    if s == 201:
        created_agents.append(b["id"])
    s, b = call("POST", "/agents", {"name": "x", "origin": "internal", "description": "è" * 1000})
    check("descrizione di 1000 caratteri accentati accettata", s == 201, (s, b))
    if s == 201:
        created_agents.append(b["id"])

    # ------------------------------------------------------------------
    section("ID inesistenti e riferimenti errati")
    s, b = call("GET", "/agents/99999999999")
    check("ID enorme -> 404", s == 404, (s, b))
    s, b = call("POST", "/configurations", {"key": "edge.noagent", "value": "v", "scope": "agent", "agent_id": 999999})
    check("configurazione per agente inesistente -> 400", s == 400, (s, b))
    s, b = call("POST", "/integrations/limits", {"agent_id": 999999, "resource_id": r, "max_calls_per_day": 1})
    check("limite per agente inesistente -> 400", s == 400, (s, b))
    s, b = call("POST", "/integrations/approvals", {"agent_id": 999999, "resource_id": r})
    check("approvazione per coppia inesistente -> 400", s == 400, (s, b))
    s, b = call("POST", "/integrations/approvals/999999/approve", {})
    check("approve di richiesta inesistente -> 404", s == 404, (s, b))
    s, b = call("POST", "/integrations/check", {"agent_id": 999999, "resource_id": r})
    check("check con agente inesistente -> 404", s == 404, (s, b))
    s, b = call("POST", f"/lifecycle/agents/{a}/deploy", {"environment": "development", "version_label": "9"})
    check("deploy di versione inesistente -> 400", s == 400, (s, b))
    s, b = call("DELETE", "/integrations/resources/999999")
    check("cancellazione risorsa inesistente -> 404", s == 404, (s, b))

    # ------------------------------------------------------------------
    section("JSON e campi")
    s, b = send_raw("POST", "/agents", b'{"name":"x","origin":"internal"} spazzatura',
               {"Content-Type": "application/json"})
    check("JSON con dati dopo l'oggetto -> 400", s == 400, (s, b))
    s, b = send_raw("POST", "/agents", b'', {"Content-Type": "application/json"})
    check("corpo vuoto -> 400", s == 400, (s, b))
    s, b = call("POST", "/agents", {"origin": "internal"})
    check("nome mancante -> 400", s == 400, (s, b))
    s, b = call("POST", "/configurations", {"key": "k", "value": "v"})
    check("scope mancante -> 400", s == 400, (s, b))

    # ------------------------------------------------------------------
    section("Date e parametri di query")
    s, b = call("GET", "/metrics/usage?from=2026-02-30")
    check("data inesistente (30 febbraio) -> 400", s == 400, (s, b))
    s, b = call("GET", "/metrics/usage?agent_id=abc")
    check("agent_id non numerico -> 400", s == 400, (s, b))
    s, b = call("GET", "/metrics/usage?from=2026-06-01&to=2026-01-01")
    check("intervallo invertito -> 200 vuoto", s == 200 and b == [], (s, b))
    s, b = call("GET", f"/lifecycle/agents/{a}/performance?from=2026/01/01")
    check("performance con data nel formato errato -> 400", s == 400, (s, b))
    s, b = call("GET", "/audit?limit=abc")
    check("audit limit non numerico -> 400", s == 400, (s, b))
    s, b = call("GET", "/integrations/access-log?agent_id=-5")
    check("access-log con agent_id negativo -> 400", s == 400, (s, b))
    s, b = call("GET", "/integrations/resources?kind=alieno")
    check("risorse con kind non valido -> 400", s == 400, (s, b))

    # ------------------------------------------------------------------
    section("Stati e transizioni")
    s, b = call("POST", "/agents", {"name": "edge-ret", "origin": "internal", "status": "active"})
    er = b["id"]
    created_agents.append(er)
    call("POST", f"/lifecycle/agents/{er}/retire", {})
    s, b = call("PUT", f"/agents/{er}", {"name": "edge-ret", "origin": "internal", "status": "active"})
    check("PUT non riporta a un altro stato un agente dismesso -> 409", s == 409 and b.get("error") == "invalid_state", (s, b))
    s, b = call("PUT", f"/agents/{er}", {"name": "edge-ret", "origin": "internal", "status": "retired"})
    check("PUT mantiene lo stato dismesso -> 200", s == 200, (s, b))

    # ------------------------------------------------------------------
    section("Concorrenza: limite giornaliero")
    s, b = call("POST", "/agents", {"name": "conc", "origin": "internal", "status": "active"})
    ca = b["id"]
    created_agents.append(ca)
    s, b = call("POST", "/integrations/resources", {"name": "conc-res", "kind": "api", "status": "enabled"})
    cr = b["id"]
    created_resources.append(cr)
    call("POST", "/integrations/allowlist", {"agent_id": ca, "resource_id": cr})
    call("POST", "/integrations/limits", {"agent_id": ca, "resource_id": cr, "max_calls_per_day": 5})
    with ThreadPoolExecutor(max_workers=20) as pool:
        res = list(pool.map(lambda _: call("POST", "/integrations/check",
                                           {"agent_id": ca, "resource_id": cr, "cost": 0}), range(20)))
    allowed = sum(1 for s, b in res if s == 200 and b.get("decision") == "allowed")
    check("20 check concorrenti su limite 5: esattamente 5 permessi", allowed == 5, allowed)

    section("Concorrenza: limite di costo mensile")
    s, b = call("POST", "/integrations/limits", {"agent_id": ca, "resource_id": cr, "max_cost_per_month": 0.5})
    # la coppia ha già un limite: lo sostituiamo con quello di costo
    lims = call("GET", f"/integrations/limits?agent_id={ca}")[1]
    lid = lims[0]["id"]
    call("PUT", f"/integrations/limits/{lid}", {"agent_id": ca, "resource_id": cr, "max_cost_per_month": 0.5,
                                                 "currency": "EUR"})
    with ThreadPoolExecutor(max_workers=20) as pool:
        res = list(pool.map(lambda _: call("POST", "/integrations/check",
                                           {"agent_id": ca, "resource_id": cr, "cost": 0.1}), range(20)))
    allowed_cost = sum(1 for s, b in res if s == 200 and b.get("decision") == "allowed")
    check("20 check da 0,10 su budget 0,50: esattamente 5 permessi", allowed_cost == 5, allowed_cost)

    # ------------------------------------------------------------------
    section("Concorrenza: approvazione usata una sola volta")
    s, b = call("POST", "/integrations/resources", {"name": "appr-res", "kind": "mcp_server", "status": "enabled"})
    ar = b["id"]
    created_resources.append(ar)
    call("POST", "/integrations/allowlist", {"agent_id": ca, "resource_id": ar, "requires_approval": True})
    s, b = call("POST", "/integrations/check", {"agent_id": ca, "resource_id": ar})
    apid = b.get("approval_id")
    call("POST", f"/integrations/approvals/{apid}/approve", {})
    with ThreadPoolExecutor(max_workers=10) as pool:
        res = list(pool.map(lambda _: call("POST", "/integrations/check",
                                           {"agent_id": ca, "resource_id": ar}), range(10)))
    ok_appr = sum(1 for s, b in res if b.get("reason") == "ok_approved")
    check("10 check concorrenti con una sola approvazione: esattamente 1 permesso", ok_appr == 1, ok_appr)

    section("Concorrenza: richieste pending duplicate")
    s, b = call("POST", "/integrations/resources", {"name": "pend-res", "kind": "data_source", "status": "enabled"})
    pr = b["id"]
    created_resources.append(pr)
    call("POST", "/integrations/allowlist", {"agent_id": ca, "resource_id": pr, "requires_approval": True})
    with ThreadPoolExecutor(max_workers=10) as pool:
        res = list(pool.map(lambda _: call("POST", "/integrations/check",
                                           {"agent_id": ca, "resource_id": pr}), range(10)))
    s, pend = call("GET", f"/integrations/approvals?status=pending&agent_id={ca}")
    n_pend = sum(1 for p in pend if p["resource_id"] == pr)
    check("10 check concorrenti senza approvazione: una sola richiesta pending", n_pend == 1, n_pend)

    # ------------------------------------------------------------------
    section("Concorrenza: chiave di configurazione e catena di audit")
    key = "edge.concurrent.key"
    with ThreadPoolExecutor(max_workers=10) as pool:
        res = list(pool.map(lambda _: call("POST", "/configurations",
                                           {"key": key, "value": "v", "scope": "global"}), range(10)))
    created = [b["id"] for s, b in res if s == 201]
    created_configs.extend(created)
    check("10 creazioni concorrenti della stessa chiave: esattamente 1 riuscita",
          len(created) == 1, len(created))

    with ThreadPoolExecutor(max_workers=10) as pool:
        res = list(pool.map(lambda i: call("POST", "/agents", {"name": f"chain-{i}", "origin": "internal"}), range(10)))
    for s, b in res:
        if s == 201:
            created_agents.append(b["id"])
    s, v = call("GET", "/audit/verify")
    check("catena di audit valida dopo scritture concorrenti", s == 200 and v.get("valid") is True, v)

    # ------------------------------------------------------------------
    section("Pulizia")
    for rid in created_resources:
        call("DELETE", f"/integrations/resources/{rid}")
    for cid in created_configs:
        call("DELETE", f"/configurations/{cid}")
    for aid in created_agents:
        call("DELETE", f"/agents/{aid}")

    total = passed + len(failed)
    print(f"\nRisultato: {passed}/{total} controlli superati")
    if failed:
        print("Falliti:")
        for f in failed:
            print(f"  - {f}")
        sys.exit(1)


if __name__ == "__main__":
    main()
