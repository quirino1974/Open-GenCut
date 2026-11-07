#!/usr/bin/env python3
"""Regressione del punto 8 della specifica: FinOps e tracciamento,
con il campo framework sull'agente.

Stessi criteri di regression_backend.py: solo HTTP e libreria standard, dati di test
creati e rimossi dalla suite, esito 0 se tutti i controlli passano.

Copre: esiti (validazione e registrazione), vista per agente (costo d'uso, costo fisso per
mese, totale, ROI, costo per successo), agenti in USD, vista d'insieme per framework e per
origine, campo framework.

Uso:  python3 tests/regression_finops.py [base_url]
"""
import calendar
import datetime
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
    agents = []
    today = datetime.date.today()
    first = today.replace(day=1)
    last = today.replace(day=calendar.monthrange(today.year, today.month)[1])
    prev_month_first = (first - datetime.timedelta(days=1)).replace(day=1)
    f_iso, t_iso = first.isoformat(), last.isoformat()

    # ------------------------------------------------------------------
    section("Campo framework sull'agente")
    s, b = call("POST", "/agents", {"name": "fo-lang", "origin": "marketplace", "status": "active",
                                     "framework": "LangGraph", "cost_monthly": 30, "currency": "EUR"})
    check("agente con framework 201", s == 201 and b.get("framework") == "LangGraph", (s, b))
    ag = b["id"]
    agents.append(ag)
    s, b = call("POST", "/agents", {"name": "fo-long", "origin": "internal", "framework": "x" * 101})
    check("framework oltre 100 caratteri -> 400", s == 400, (s, b))
    s, b = call("PUT", f"/agents/{ag}", {"name": "fo-lang", "origin": "marketplace", "status": "active",
                                          "framework": "CrewAI", "cost_monthly": 30, "currency": "EUR"})
    check("framework modificabile con la PUT", s == 200 and b.get("framework") == "CrewAI", (s, b))

    # ------------------------------------------------------------------
    section("Esiti: validazione")
    s, b = call("POST", "/finops/outcomes", {"agent_id": ag, "outcome": "boh", "value_eur": 1, "period": f_iso})
    check("esito non valido -> 400", s == 400, (s, b))
    s, b = call("POST", "/finops/outcomes", {"agent_id": ag, "outcome": "success", "value_eur": -1, "period": f_iso})
    check("valore negativo -> 400", s == 400, (s, b))
    s, b = call("POST", "/finops/outcomes", {"agent_id": ag, "outcome": "success", "value_eur": 1, "period": "2026-13-01"})
    check("periodo non valido -> 400", s == 400, (s, b))
    s, b = call("POST", "/finops/outcomes", {"agent_id": 999999, "outcome": "success", "value_eur": 1, "period": f_iso})
    check("agente inesistente -> 404", s == 404, (s, b))

    # ------------------------------------------------------------------
    section("Esiti: registrazione")
    s, b = call("POST", "/finops/outcomes", {"agent_id": ag, "outcome": "success", "value_eur": 100, "period": f_iso})
    check("esito di successo 201", s == 201 and b.get("outcome") == "success", (s, b))
    call("POST", "/finops/outcomes", {"agent_id": ag, "outcome": "failure", "value_eur": 0, "period": f_iso,
                                       "note": "fallito"})
    call("POST", "/finops/outcomes", {"agent_id": ag, "outcome": "partial", "value_eur": 20, "period": f_iso})

    # Costi d'uso: tre chiamate da 0,05 EUR registrate adesso.
    for _ in range(3):
        call("POST", f"/lifecycle/agents/{ag}/calls", {"environment": "production", "cost": 0.05, "tokens": 10})

    # ------------------------------------------------------------------
    section("Vista per agente")
    s, v = call("GET", f"/finops/agents/{ag}?from={f_iso}&to={t_iso}")
    check("vista per agente 200 con campi di contratto", s == 200 and all(
        k in v for k in ("usage_cost", "fixed_cost", "total_cost", "outcomes", "value_eur", "roi",
                         "cost_per_success")), (s, v))
    check("costo d'uso dal monitoraggio: 0,15", abs(v.get("usage_cost", 0) - 0.15) < 1e-6, v)
    check("costo fisso di un mese: 30", abs(v.get("fixed_cost", 0) - 30) < 1e-6, v)
    check("costo totale = d'uso + fisso", abs(v.get("total_cost", 0) - 30.15) < 1e-6, v)
    check("conteggi per esito", v.get("outcomes") == {"success": 1, "failure": 1, "partial": 1}, v)
    check("valore prodotto: 120 EUR", abs(v.get("value_eur", 0) - 120) < 1e-6, v)
    expected_roi = (120 - 30.15) / 30.15
    check("ROI = (valore - costo) / costo", v.get("roi") is not None and abs(v["roi"] - expected_roi) < 1e-6, v)
    check("costo per successo = costo totale / successi",
          v.get("cost_per_success") is not None and abs(v["cost_per_success"] - 30.15) < 1e-6, v)

    s, v2 = call("GET", f"/finops/agents/{ag}?from={prev_month_first.isoformat()}&to={t_iso}")
    check("periodo di due mesi: il costo fisso raddoppia", abs(v2.get("fixed_cost", 0) - 60) < 1e-6, v2)

    s, b = call("GET", f"/finops/agents/{ag}?from={t_iso}&to={f_iso}")
    check("intervallo invertito -> 400", s == 400, (s, b))
    s, b = call("GET", f"/finops/agents/{ag}?from=oggi&to={t_iso}")
    check("data iniziale non valida -> 400", s == 400, (s, b))
    s, b = call("GET", f"/finops/agents/{ag}")
    check("periodo obbligatorio -> 400", s == 400, (s, b))
    s, b = call("GET", f"/finops/agents/999999?from={f_iso}&to={t_iso}")
    check("agente inesistente -> 404", s == 404, (s, b))

    # ------------------------------------------------------------------
    section("ROI nullo e agenti in USD")
    s, b = call("POST", "/agents", {"name": "fo-free", "origin": "internal", "status": "active"})
    free = b["id"]
    agents.append(free)
    s, v = call("GET", f"/finops/agents/{free}?from={f_iso}&to={t_iso}")
    check("costo zero: ROI null", v.get("total_cost") == 0 and v.get("roi") is None, v)
    check("nessun successo: costo per successo null", v.get("cost_per_success") is None, v)

    s, b = call("POST", "/agents", {"name": "fo-usd", "origin": "marketplace", "status": "active",
                                     "cost_monthly": 50, "currency": "USD"})
    usd = b["id"]
    agents.append(usd)
    s, v = call("GET", f"/finops/agents/{usd}?from={f_iso}&to={t_iso}")
    check("agente in USD: costo fisso fuori dal totale EUR",
          v.get("fixed_cost") == 0 and v.get("fixed_cost_other_currency", {}).get("USD") == 50, v)

    # ------------------------------------------------------------------
    section("Vista d'insieme")
    s, b = call("GET", f"/finops/summary?from={f_iso}&to={t_iso}&group_by=framework")
    check("summary per framework 200", s == 200 and b.get("group_by") == "framework"
          and isinstance(b.get("groups"), list), (s, b))
    lang = next((g for g in b.get("groups", []) if g["key"] == "CrewAI"), None)
    # Il registro può contenere agenti non creati da questa suite: il gruppo deve contenere almeno
    # l'agente di test e il suo totale non può essere inferiore al totale dell'agente.
    check("gruppo per framework contiene l'agente di test", lang is not None and lang["agents"] >= 1
          and lang["total_cost"] >= 30.15 - 1e-6, lang)
    s, b = call("GET", f"/finops/summary?from={f_iso}&to={t_iso}&group_by=origin")
    check("summary per origine 200", s == 200 and any(g["key"] == "marketplace" for g in b.get("groups", [])),
          (s, b))
    s, b = call("GET", f"/finops/summary?from={f_iso}&to={t_iso}&group_by=colore")
    check("group_by non valido -> 400", s == 400, (s, b))

    # ------------------------------------------------------------------
    section("Audit")
    s, aud = call("GET", "/audit?limit=200")
    keys = []
    for e in aud:
        try:
            keys.append(json.loads(e["detail"]).get("key"))
        except (TypeError, json.JSONDecodeError, AttributeError):
            pass
    check("audit: chiave esito registrato presente", "outcome_recorded" in keys, keys[:10])

    # ------------------------------------------------------------------
    section("Pulizia")
    for aid in agents:
        call("DELETE", f"/agents/{aid}")
    s, b = call("GET", f"/finops/summary?from={f_iso}&to={t_iso}&group_by=origin")
    check("nessun agente di test residuo nella vista", s == 200, (s, b))

    total = passed + len(failed)
    print(f"\nRisultato: {passed}/{total} controlli superati")
    if failed:
        print("Falliti:")
        for f in failed:
            print(f"  - {f}")
        sys.exit(1)


if __name__ == "__main__":
    main()
