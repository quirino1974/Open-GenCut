#!/usr/bin/env python3
"""Regressione del ciclo di vita Open-GenCut (punto 3).

Stessi criteri di regression_backend.py: solo chiamate HTTP, nessuna libreria esterna,
dati di test creati e rimossi dalla suite, esito 0 se tutti i controlli passano.

Copre: versioni, deploy, promozioni, registro, sospensione e riattivazione,
deploy e promozione bloccati su agente sospeso (409 invalid_state), dismissione
definitiva, blocco accessi su agente dismesso (agent_retired), monitoraggio.

Uso:  python3 tests/regression_lifecycle.py [base_url]
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


def new_agent(name, status="active"):
    s, b = call("POST", "/agents", {"name": name, "origin": "internal", "status": status})
    return b["id"] if s == 201 and isinstance(b, dict) else None


def main():
    created_agents = []
    created_resources = []

    # ------------------------------------------------------------------
    section("Versioni")
    a = new_agent("LC main")
    created_agents.append(a)
    check("agente di test creato", a is not None)
    s, b = call("POST", f"/lifecycle/agents/{a}/versions", {"version_label": "1.0.0"})
    check("registra versione 201", s == 201, (s, b))
    s, b = call("POST", f"/lifecycle/agents/{a}/versions", {"version_label": "1.0.0"})
    check("versione duplicata -> 409 conflict", s == 409 and b.get("error") == "conflict", (s, b))
    s, b = call("POST", f"/lifecycle/agents/{a}/versions", {"version_label": "  "})
    check("versione vuota -> 400", s == 400, (s, b))
    call("POST", f"/lifecycle/agents/{a}/versions", {"version_label": "1.1.0"})
    s, b = call("GET", f"/lifecycle/agents/{a}/versions")
    check("elenco versioni", s == 200 and b == ["1.0.0", "1.1.0"], (s, b))
    s, b = call("GET", f"/lifecycle/agents/{a}")
    check("stato: campi di contratto", s == 200 and all(
        k in b for k in ("agent_id", "status", "environments", "versions")), (s, b))
    check("stato: ambienti vuoti all'inizio", s == 200 and all(
        v is None for v in b["environments"].values()), b)

    # ------------------------------------------------------------------
    section("Deploy e promozioni")
    s, b = call("POST", f"/lifecycle/agents/{a}/deploy", {"environment": "development", "version_label": "9.9"})
    check("deploy di versione non registrata -> 400", s == 400, (s, b))
    s, b = call("POST", f"/lifecycle/agents/{a}/deploy", {"environment": "moon", "version_label": "1.0.0"})
    check("ambiente non valido -> 400", s == 400, (s, b))
    s, b = call("POST", f"/lifecycle/agents/{a}/deploy",
                {"environment": "development", "version_label": "1.0.0", "note": "primo deploy"})
    check("deploy in development 201", s == 201 and b.get("action") == "deploy", (s, b))
    s, b = call("POST", f"/lifecycle/agents/{a}/promote", {"to_environment": "production"})
    check("promozione senza versione in staging -> 409 invalid_state",
          s == 409 and b.get("error") == "invalid_state", (s, b))
    # Gate di valutazione (R3): nessuna promozione verso staging senza valutazione superata.
    s, b = call("POST", f"/lifecycle/agents/{a}/promote", {"to_environment": "staging"})
    check("gate: promozione senza valutazione -> 409 invalid_state",
          s == 409 and b.get("error") == "invalid_state", (s, b))
    s, b = call("POST", f"/lifecycle/agents/{a}/deploy", {"environment": "staging", "version_label": "1.0.0"})
    check("gate: deploy diretto in staging senza valutazione -> 409", s == 409, (s, b))
    s, b = call("POST", f"/lifecycle/agents/{a}/evaluations", {"version_label": "1.0.0", "result": "failed"})
    check("valutazione failed registrata 201", s == 201, (s, b))
    s, b = call("POST", f"/lifecycle/agents/{a}/promote", {"to_environment": "staging"})
    check("gate: ultima valutazione failed -> 409", s == 409, (s, b))
    s, b = call("POST", f"/lifecycle/agents/{a}/evaluations", {"version_label": "1.0.0", "result": "passed"})
    check("valutazione passed registrata 201", s == 201, (s, b))
    s, b = call("POST", f"/lifecycle/agents/{a}/evaluations", {"version_label": "1.0.0", "result": "ok"})
    check("valutazione con esito non valido -> 400", s == 400, (s, b))
    s, b = call("POST", f"/lifecycle/agents/{a}/evaluations", {"version_label": "9.9", "result": "passed"})
    check("valutazione di versione non registrata -> 400", s == 400, (s, b))
    s, b = call("GET", f"/lifecycle/agents/{a}/evaluations?limit=5")
    check("elenco valutazioni, dal più recente", s == 200 and [e["result"] for e in b][:2] == ["passed", "failed"], (s, b))

    s, b = call("POST", f"/lifecycle/agents/{a}/promote", {"to_environment": "staging"})
    check("promozione development -> staging 201 con valutazione superata",
          s == 201 and b.get("from_environment") == "development", (s, b))
    s, b = call("POST", f"/lifecycle/agents/{a}/promote", {"to_environment": "production"})
    check("promozione staging -> production 201", s == 201 and b.get("from_environment") == "staging", (s, b))

    s, b = call("GET", f"/lifecycle/agents/{a}")
    envs = b.get("environments", {})
    check("stato: tre ambienti con la stessa versione", s == 200 and all(
        envs.get(e) and envs[e]["version_label"] == "1.0.0"
        for e in ("development", "staging", "production")), envs)
    s, b = call("GET", f"/lifecycle/agents/{a}/deployments?limit=10")
    check("registro: tre voci, dal più recente",
          s == 200 and [d["action"] for d in b] == ["promote", "promote", "deploy"], (s, b))

    # ------------------------------------------------------------------
    section("Sospensione e riattivazione")
    s, b = call("POST", f"/lifecycle/agents/{a}/suspend", {"note": "test"})
    check("sospensione 200 con stato sospeso", s == 200 and b.get("status") == "suspended", (s, b))
    s, b = call("POST", f"/lifecycle/agents/{a}/suspend", {})
    check("sospendi di nuovo -> 409 invalid_state", s == 409 and b.get("error") == "invalid_state", (s, b))
    s, b = call("POST", f"/lifecycle/agents/{a}/deploy", {"environment": "development", "version_label": "1.1.0"})
    check("deploy su agente sospeso -> 409 invalid_state",
          s == 409 and b.get("error") == "invalid_state", (s, b))
    s, b = call("POST", f"/lifecycle/agents/{a}/promote", {"to_environment": "staging"})
    check("promozione su agente sospeso -> 409 invalid_state",
          s == 409 and b.get("error") == "invalid_state", (s, b))
    s, b = call("GET", f"/lifecycle/agents/{a}/deployments?limit=10")
    check("nessun deploy registrato mentre sospeso", s == 200 and len(b) == 3, (s, b))
    s, b = call("POST", f"/lifecycle/agents/{a}/reactivate", {})
    check("riattivazione 200 con stato attivo", s == 200 and b.get("status") == "active", (s, b))
    s, b = call("POST", f"/lifecycle/agents/{a}/reactivate", {})
    check("riattiva un agente già attivo -> 409", s == 409, (s, b))
    s, b = call("POST", f"/lifecycle/agents/{a}/deploy", {"environment": "development", "version_label": "1.1.0"})
    check("deploy dopo riattivazione 201", s == 201, (s, b))

    # ------------------------------------------------------------------
    section("Monitoraggio")
    calls = [
        {"environment": "production", "success": True, "latency_ms": 120, "tokens": 300, "cost": 0.02},
        {"environment": "production", "success": False, "latency_ms": 900, "tokens": 10, "cost": 0},
        {"environment": "production", "latency_ms": 200, "tokens": 50, "cost": 0.01},
    ]
    codes = [call("POST", f"/lifecycle/agents/{a}/calls", c)[0] for c in calls]
    check("registrazione chiamate 201", codes == [201, 201, 201], codes)
    s, b = call("POST", f"/lifecycle/agents/{a}/calls", {"environment": "moon"})
    check("chiamata con ambiente non valido -> 400", s == 400, (s, b))
    s, p = call("GET", f"/lifecycle/agents/{a}/performance?environment=production")
    check("performance: 3 chiamate, 1 errore", s == 200 and p.get("calls") == 3 and p.get("errors") == 1, (s, p))
    check("performance: tasso errore 1/3", s == 200 and abs(p.get("error_rate", 0) - 1 / 3) < 1e-9, p)
    check("performance: latenza media e massima",
          s == 200 and abs(p.get("avg_latency_ms", 0) - 1220 / 3) < 1e-6 and p.get("max_latency_ms") == 900, p)
    check("performance: token e costo", s == 200 and p.get("tokens") == 360 and abs(p.get("cost", 0) - 0.03) < 1e-9, p)
    s, p = call("GET", f"/lifecycle/agents/{a}/performance?environment=staging")
    check("performance per ambiente senza chiamate: zero", s == 200 and p.get("calls") == 0, p)
    s, b = call("GET", f"/lifecycle/agents/{a}/performance?from=non-data")
    check("performance con data non valida -> 400", s == 400, (s, b))

    # ------------------------------------------------------------------
    section("Gate di valutazione: versione valutata diversa da quella promossa")
    g = new_agent("LC gate versione")
    created_agents.append(g)
    call("POST", f"/lifecycle/agents/{g}/versions", {"version_label": "1.0"})
    call("POST", f"/lifecycle/agents/{g}/versions", {"version_label": "1.1"})
    call("POST", f"/lifecycle/agents/{g}/evaluations", {"version_label": "1.0", "result": "passed"})
    call("POST", f"/lifecycle/agents/{g}/deploy", {"environment": "development", "version_label": "1.0"})
    s, b = call("POST", f"/lifecycle/agents/{g}/promote", {"to_environment": "staging"})
    check("gate: staging con valutazione 1.0 per la versione 1.0 -> 201", s == 201, (s, b))
    # Sviluppo passa a 1.1: la promozione verso staging porta 1.1, ma l'ultima valutazione è su 1.0.
    call("POST", f"/lifecycle/agents/{g}/deploy", {"environment": "development", "version_label": "1.1"})
    s, b = call("POST", f"/lifecycle/agents/{g}/promote", {"to_environment": "staging"})
    check("gate: staging di 1.1 con ultima valutazione su 1.0 -> 409",
          s == 409 and b.get("error") == "invalid_state", (s, b))
    call("POST", f"/lifecycle/agents/{g}/evaluations", {"version_label": "1.1", "result": "passed"})
    s, b = call("POST", f"/lifecycle/agents/{g}/promote", {"to_environment": "staging"})
    check("gate: staging di 1.1 dopo valutazione superata 1.1 -> 201", s == 201, (s, b))
    s, b = call("POST", f"/lifecycle/agents/{g}/promote", {"to_environment": "production"})
    check("gate: production di 1.1 con ultima valutazione su 1.1 -> 201", s == 201, (s, b))

    # ------------------------------------------------------------------
    section("Dismissione")
    b2 = new_agent("LC dismesso")
    created_agents.append(b2)
    call("POST", f"/lifecycle/agents/{b2}/versions", {"version_label": "2.0"})
    call("POST", f"/lifecycle/agents/{b2}/deploy", {"environment": "development", "version_label": "2.0"})
    s, b = call("POST", f"/lifecycle/agents/{b2}/retire", {"note": "fine vita"})
    check("dismissione 200 con stato retired", s == 200 and b.get("status") == "retired", (s, b))

    res = call("POST", "/integrations/resources", {"name": "lc-res", "kind": "api", "status": "enabled"})
    rid = res[1]["id"] if res[0] == 201 else None
    if rid:
        created_resources.append(rid)
    call("POST", "/integrations/allowlist", {"agent_id": b2, "resource_id": rid})
    s, c = call("POST", "/integrations/check", {"agent_id": b2, "resource_id": rid})
    check("check su agente dismesso -> denied agent_retired",
          s == 200 and c.get("decision") == "denied" and c.get("reason") == "agent_retired", c)

    s, b = call("POST", f"/lifecycle/agents/{b2}/deploy", {"environment": "staging", "version_label": "2.0"})
    check("deploy su agente dismesso -> 409 invalid_state", s == 409 and b.get("error") == "invalid_state", (s, b))
    s, b = call("POST", f"/lifecycle/agents/{b2}/promote", {"to_environment": "staging"})
    check("promozione su agente dismesso -> 409 invalid_state", s == 409, (s, b))
    s, b = call("POST", f"/lifecycle/agents/{b2}/reactivate", {})
    check("riattivazione dismesso -> 409 (definitiva)", s == 409, (s, b))
    s, b = call("POST", f"/lifecycle/agents/{b2}/suspend", {})
    check("sospensione dismesso -> 409", s == 409, (s, b))
    s, b = call("POST", f"/lifecycle/agents/{b2}/retire", {})
    check("dismissione di nuovo -> 409", s == 409, (s, b))

    # ------------------------------------------------------------------
    section("Audit e pulizia")
    s, b = call("GET", "/audit?limit=50")
    check("audit contiene deploy e cambi di stato",
          s == 200 and any(e.get("action") == "retire" for e in b) and any(e.get("action") == "suspend" for e in b),
          s)

    for rid in created_resources:
        call("DELETE", f"/integrations/resources/{rid}")
    for aid in created_agents:
        call("DELETE", f"/agents/{aid}")
    s, b = call("GET", f"/agents/{a}")
    check("dati di test rimossi", s == 404, (s, b))

    total = passed + len(failed)
    print(f"\nRisultato: {passed}/{total} controlli superati")
    if failed:
        print("Falliti:")
        for f in failed:
            print(f"  - {f}")
        sys.exit(1)


if __name__ == "__main__":
    main()
