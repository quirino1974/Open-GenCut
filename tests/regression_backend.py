#!/usr/bin/env python3
"""Regressione del backend Open-GenCut (punto 1 e punto 2).

Esegue solo chiamate HTTP alla API in ascolto (default http://localhost:8080).
Non richiede librerie esterne. Crea i propri dati di test e li rimuove alla fine.

Uso:  python3 tests/regression_backend.py [base_url]
Esce con codice 0 se tutti i controlli passano, 1 altrimenti.
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


def main():
    created_agents = []
    created_resources = []
    created_configs = []

    # ------------------------------------------------------------------
    section("Punto 1 - salute")
    s, b = call("GET", "/health")
    check("health 200", s == 200, s)
    check("health ok", b == {"status": "ok", "db": "ok"}, b)

    # ------------------------------------------------------------------
    section("Punto 1 - agenti")
    s, b = call("POST", "/agents", {"name": "Reg A", "origin": "internal", "status": "active",
                                     "cost_monthly": 5, "currency": "EUR", "owner": "reg"})
    check("crea agente 201", s == 201, (s, b))
    a1 = b["id"] if isinstance(b, dict) else None
    if a1:
        created_agents.append(a1)
    check("agente ha campi di contratto", isinstance(b, dict) and all(
        k in b for k in ("id", "name", "origin", "status", "cost_monthly", "currency", "created_at", "updated_at")), b)

    s, b = call("POST", "/agents", {"name": "Reg B", "origin": "marketplace", "currency": "USD", "cost_monthly": 2})
    check("crea agente USD 201", s == 201, (s, b))
    if isinstance(b, dict) and "id" in b:
        created_agents.append(b["id"])

    s, b = call("POST", "/agents", {"name": "x", "origin": "alieno"})
    check("origin non valida -> 400 validation_error", s == 400 and isinstance(b, dict)
          and b.get("error") == "validation_error", (s, b))
    s, b = call("POST", "/agents", {"name": "x", "origin": "internal", "status": "bogus"})
    check("status non valido -> 400", s == 400, (s, b))
    s, b = call("POST", "/agents", {"name": "x", "origin": "internal", "cost_monthly": -1})
    check("cost negativo -> 400", s == 400, (s, b))
    s, b = call("POST", "/agents", {"name": "x", "origin": "internal", "campo_ignoto": 1})
    check("campo sconosciuto -> 400 bad_request", s == 400 and b.get("error") == "bad_request", (s, b))

    s, b = call("GET", f"/agents/{a1}")
    check("dettaglio agente 200", s == 200 and b.get("name") == "Reg A", (s, b))
    s, b = call("GET", "/agents?origin=marketplace")
    check("filtro origin", s == 200 and all(x["origin"] == "marketplace" for x in b), (s, b))
    s, b = call("GET", "/agents?q=Reg")
    check("ricerca q", s == 200 and len(b) >= 2, (s, b))
    s, b = call("GET", "/agents?owner=reg")
    check("filtro owner", s == 200 and all(x["owner"] == "reg" for x in b), (s, b))
    s, b = call("GET", "/agents?status=nope")
    check("filtro status non valido -> 400", s == 400, (s, b))

    s, b = call("PUT", f"/agents/{a1}", {"name": "Reg A2", "origin": "internal", "status": "suspended",
                                         "cost_monthly": 7, "currency": "EUR"})
    check("PUT agente 200", s == 200 and b.get("name") == "Reg A2" and b.get("status") == "suspended", (s, b))
    s, b = call("PUT", "/agents/999999", {"name": "x", "origin": "internal"})
    check("PUT agente inesistente -> 404", s == 404, (s, b))
    s, b = call("GET", "/agents/abc")
    check("id non numerico -> 400", s == 400, (s, b))

    # ------------------------------------------------------------------
    section("Punto 1 - configurazioni")
    key = "reg.test.key"
    s, b = call("POST", "/configurations", {"key": key, "value": "1", "scope": "global"})
    check("crea configurazione globale 201", s == 201, (s, b))
    c1 = b["id"] if isinstance(b, dict) and "id" in b else None
    if c1:
        created_configs.append(c1)
    s, b = call("POST", "/configurations", {"key": key, "value": "2", "scope": "global"})
    check("chiave duplicata -> 409", s == 409 and b.get("error") == "conflict", (s, b))
    s, b = call("POST", "/configurations", {"key": "reg.test.agent", "value": "v", "scope": "agent"})
    check("scope agent senza agent_id -> 400", s == 400, (s, b))
    s, b = call("POST", "/configurations", {"key": "reg.test.agent2", "value": "v",
                                            "scope": "agent", "agent_id": a1})
    check("configurazione di agente 201", s == 201 and b.get("agent_id") == a1, (s, b))
    if isinstance(b, dict) and "id" in b:
        created_configs.append(b["id"])
    s, b = call("PUT", f"/configurations/{c1}", {"key": key, "value": "9", "scope": "global"})
    check("PUT configurazione 200", s == 200 and b.get("value") == "9", (s, b))
    s, b = call("GET", "/configurations")
    check("elenco configurazioni 200", s == 200 and isinstance(b, list), (s, b))

    # ------------------------------------------------------------------
    section("Punto 1 - metriche e audit")
    s, b = call("GET", "/metrics/summary")
    check("summary 200 con campi di contratto", s == 200 and all(
        k in b for k in ("agents_total", "by_origin", "by_status", "cost_monthly_total", "currency",
                         "cost_monthly_other")), (s, b))
    s, b = call("GET", "/metrics/usage?from=2000-01-01&to=2100-12-31")
    check("usage 200 lista", s == 200 and isinstance(b, list), (s, b))
    s, b = call("GET", "/metrics/usage?from=non-data")
    check("usage data non valida -> 400", s == 400, (s, b))
    s, b = call("GET", "/audit?limit=5")
    check("audit 200, al massimo 5 righe", s == 200 and isinstance(b, list) and len(b) <= 5, (s, b))
    s, b = call("GET", "/audit?limit=0")
    check("audit limit fuori range -> 400", s == 400, (s, b))
    s, b = call("GET", "/audit/verify")
    check("audit verify: catena valida", s == 200 and b.get("valid") is True, (s, b))

    # ------------------------------------------------------------------
    section("Punto 2 - risorse, allowlist, limiti")
    s, b = call("POST", "/integrations/resources", {"kind": "llm_model", "name": "reg-llm",
                                                    "endpoint": "https://llm.example", "status": "enabled"})
    check("crea risorsa 201", s == 201, (s, b))
    r1 = b["id"] if isinstance(b, dict) and "id" in b else None
    if r1:
        created_resources.append(r1)
    s, b = call("POST", "/integrations/resources", {"kind": "x", "name": "y"})
    check("kind non valido -> 400", s == 400, (s, b))
    s, b = call("POST", "/integrations/allowlist", {"agent_id": a1, "resource_id": r1})
    check("allowlist 201", s == 201, (s, b))
    s, b = call("POST", "/integrations/allowlist", {"agent_id": a1, "resource_id": r1})
    check("allowlist duplicata -> 409", s == 409, (s, b))
    s, b = call("GET", f"/integrations/allowlist?agent_id={a1}")
    check("allowlist elenco per agente", s == 200 and len(b) == 1, (s, b))

    # ------------------------------------------------------------------
    section("Punto 2 - ordine dei controlli in /check")
    # Agente a1 è "suspended" dopo il PUT: non attivo.
    s, b = call("POST", "/integrations/check", {"agent_id": a1, "resource_id": r1})
    check("check 200 anche se negato", s == 200, (s, b))
    check("agente non attivo -> agent_not_active", b.get("reason") == "agent_not_active", b)
    check("decision denied", b.get("decision") == "denied", b)

    # Riattiva l'agente per i test successivi.
    call("PUT", f"/agents/{a1}", {"name": "Reg A2", "origin": "internal", "status": "active",
                                  "cost_monthly": 7, "currency": "EUR"})

    # Risorsa disabilitata + agente non in allowlist: vince resource_disabled.
    s, b2 = call("POST", "/integrations/resources", {"kind": "api", "name": "reg-api-off", "status": "disabled"})
    r2 = b2["id"]
    created_resources.append(r2)
    s, b = call("POST", "/integrations/check", {"agent_id": a1, "resource_id": r2})
    check("risorsa disabilitata vince sulla allowlist", b.get("reason") == "resource_disabled", b)

    # Agente non attivo + risorsa disabilitata: vince resource_disabled (primo controllo).
    call("PUT", f"/agents/{a1}", {"name": "Reg A2", "origin": "internal", "status": "suspended"})
    s, b = call("POST", "/integrations/check", {"agent_id": a1, "resource_id": r2})
    check("ordine: resource_disabled prima di agent_not_active", b.get("reason") == "resource_disabled", b)
    call("PUT", f"/agents/{a1}", {"name": "Reg A2", "origin": "internal", "status": "active"})

    # Agente attivo, risorsa attiva, non in allowlist.
    s, b2 = call("POST", "/integrations/resources", {"kind": "data_source", "name": "reg-data"})
    r3 = b2["id"]
    created_resources.append(r3)
    s, b = call("POST", "/integrations/check", {"agent_id": a1, "resource_id": r3})
    check("non in allowlist -> not_allowed", b.get("reason") == "not_allowed", b)

    # ------------------------------------------------------------------
    section("Punto 2 - limiti e budget")
    s, b = call("POST", "/integrations/limits", {"agent_id": a1, "resource_id": r1,
                                                 "max_calls_per_day": 2, "max_cost_per_month": 0.25,
                                                 "currency": "EUR"})
    check("limite creato 201", s == 201, (s, b))
    lim1 = b["id"] if isinstance(b, dict) and "id" in b else None

    # Accesso negato per agente non attivo: non deve consumare budget.
    call("PUT", f"/agents/{a1}", {"name": "Reg A2", "origin": "internal", "status": "suspended"})
    s, b = call("POST", "/integrations/check", {"agent_id": a1, "resource_id": r1, "cost": 0.2})
    check("denied agent_not_active con costo dichiarato", b.get("decision") == "denied", b)
    call("PUT", f"/agents/{a1}", {"name": "Reg A2", "origin": "internal", "status": "active"})

    # Due accessi permessi da 0,10: il budget mensile resta a 0,20 su 0,25.
    s, b1 = call("POST", "/integrations/check", {"agent_id": a1, "resource_id": r1, "cost": 0.1})
    check("primo accesso permesso", b1.get("decision") == "allowed", b1)
    s, b2 = call("POST", "/integrations/check", {"agent_id": a1, "resource_id": r1, "cost": 0.1})
    check("secondo accesso permesso", b2.get("decision") == "allowed", b2)

    # Il limite giornaliero (2) è raggiunto: il terzo accesso si nega per daily_call_limit
    # e il suo costo non deve entrare nel budget.
    s, b3 = call("POST", "/integrations/check", {"agent_id": a1, "resource_id": r1, "cost": 0.1})
    check("terzo accesso negato per daily_call_limit", b3.get("reason") == "daily_call_limit", b3)

    # Alza il limite giornaliero: ora il budget residuo (0,05) non basta per 0,10 -> monthly_cost_limit.
    call("PUT", f"/integrations/limits/{lim1}", {"agent_id": a1, "resource_id": r1,
                                                 "max_calls_per_day": 100, "max_cost_per_month": 0.25,
                                                 "currency": "EUR"})
    s, b4 = call("POST", "/integrations/check", {"agent_id": a1, "resource_id": r1, "cost": 0.1})
    check("monthly_cost_limit con 0,20 spesi su 0,25", b4.get("reason") == "monthly_cost_limit", b4)

    # Il dinieghi non consumano budget: una chiamata da 0,05 deve passare (0,20 + 0,05 = 0,25, non superiore).
    s, b5 = call("POST", "/integrations/check", {"agent_id": a1, "resource_id": r1, "cost": 0.05})
    check("negati non consumano budget: 0,05 passa ancora", b5.get("decision") == "allowed", b5)

    # Registro: i negati hanno costo 0 e i permessi hanno il costo dichiarato.
    s, log = call("GET", f"/integrations/access-log?agent_id={a1}&limit=50")
    denied_costs = [e["cost"] for e in log if e["decision"] == "denied"]
    check("access_event: costo dei negati = 0", all(c == 0 for c in denied_costs), denied_costs)
    allowed_sum = sum(e["cost"] for e in log if e["decision"] == "allowed")
    check("access_event: somma permessi = 0,25", abs(allowed_sum - 0.25) < 1e-6, allowed_sum)

    # ------------------------------------------------------------------
    section("Punto 4 - R4 dichiarazione di trasparenza")
    s, b = call("POST", "/agents", {"name": "R4 interagisce", "origin": "internal", "interacts_with_people": True})
    check("interagisce senza lingua -> 400", s == 400, (s, b))
    s, b = call("POST", "/agents", {"name": "R4 lingua", "origin": "internal",
                                     "interacts_with_people": True, "interaction_language": "en-GB"})
    check("interagisce con lingua 201", s == 201 and b.get("interaction_language") == "en-GB", (s, b))
    r4 = b["id"] if s == 201 else None
    if r4:
        created_agents.append(r4)
    s, b = call("POST", "/agents", {"name": "R4 lingua errata", "origin": "internal", "interaction_language": "ital!"})
    check("lingua non valida -> 400", s == 400, (s, b))
    s, b = call("GET", "/agents/" + str(a1))
    check("default trasparenza: non interagisce, lingua nulla",
          s == 200 and b.get("interacts_with_people") is False and b.get("interaction_language") is None, b)

    # ------------------------------------------------------------------
    section("Punto 4 - R1 approvazione umana per azioni sensibili")
    s, b = call("POST", "/integrations/allowlist", {"agent_id": a1, "resource_id": r3, "requires_approval": True})
    check("coppia sensibile in allowlist 201", s == 201 and b.get("requires_approval") is True, (s, b))
    s, b = call("POST", "/integrations/check", {"agent_id": a1, "resource_id": r3})
    check("sensibile senza approvazione -> approval_required", b.get("reason") == "approval_required", b)
    apid = b.get("approval_id")
    s, b = call("POST", "/integrations/check", {"agent_id": a1, "resource_id": r3})
    check("secondo check riusa la richiesta pending", b.get("approval_id") == apid, b)
    s, b = call("POST", f"/integrations/approvals/{apid}/approve", {"note": "ok"})
    check("approvazione pending -> approved 200", s == 200 and b.get("status") == "approved", (s, b))
    s, b = call("POST", f"/integrations/approvals/{apid}/reject", {})
    check("rifiuto di richiesta approvata -> 409", s == 409, (s, b))
    s, b = call("POST", "/integrations/check", {"agent_id": a1, "resource_id": r3})
    check("approvazione valida -> allowed ok_approved", b.get("decision") == "allowed" and b.get("reason") == "ok_approved", b)
    s, b = call("POST", "/integrations/check", {"agent_id": a1, "resource_id": r3})
    check("approvazione usata una volta: di nuovo approval_required", b.get("reason") == "approval_required", b)
    s, b = call("POST", "/integrations/approvals", {"agent_id": a1, "resource_id": r1})
    check("richiesta su coppia non sensibile -> 400", s == 400, (s, b))

    # ------------------------------------------------------------------
    section("Pulizia")
    for rid in created_resources:
        call("DELETE", f"/integrations/resources/{rid}")
    for cid in created_configs:
        call("DELETE", f"/configurations/{cid}")
    for aid in created_agents:
        call("DELETE", f"/agents/{aid}")
    s, log = call("GET", "/integrations/access-log?limit=1")
    check("registro accessi conservato dopo la pulizia", s == 200, (s, log))

    # ------------------------------------------------------------------
    total = passed + len(failed)
    print(f"\nRisultato: {passed}/{total} controlli superati")
    if failed:
        print("Falliti:")
        for f in failed:
            print(f"  - {f}")
        sys.exit(1)


if __name__ == "__main__":
    main()
