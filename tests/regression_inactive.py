#!/usr/bin/env python3
"""Regressione del rilevamento degli agenti senza attività (GET /api/agents/inactive).

Stessi criteri delle altre suite: esito 0 se tutti i controlli passano, dati di test
creati e rimossi. Le attività recenti (chiamate, decisioni di accesso, tracce) sono
generate dalla suite: per questo gli eventi "vecchi" non si possono creare, e i casi
con attività sono verificati con finestre di 1 giorno.
"""
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from session import call  # noqa: E402

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
    stamp = int(time.time())
    agents = []

    def new_agent(name, status="active"):
        s, b = call("POST", "/agents", {"name": f"{name}-{stamp}", "origin": "internal", "status": status})
        agents.append(b["id"])
        return b["id"]

    section("Parametri")
    s, b = call("GET", "/agents/inactive?days=0")
    check("days 0 -> 400", s == 400, (s, b))
    s, b = call("GET", "/agents/inactive?days=abc")
    check("days non numerico -> 400", s == 400, (s, b))
    s, b = call("GET", "/agents/inactive?days=99999")
    check("days oltre il limite -> 400", s == 400, (s, b))
    s, b = call("GET", "/agents/inactive")
    check("senza days: predefinito 30, risposta lista", s == 200 and isinstance(b, list), (s, b))

    section("Agente mai attivo")
    never = new_agent("ina-never")
    s, lst = call("GET", "/agents/inactive?days=30")
    row = next((x for x in lst if x["id"] == never), None)
    check("agente attivo senza attività: elencato", row is not None, lst[:2])
    check("agente mai attivo: ultima attività nulla", row is not None and row["last_activity_at"] is None, row)
    check("agente elencato: origine e stato", row is not None and row["status"] == "active"
          and row["origin"] == "internal", row)

    section("Attività recenti escluse")
    busy = new_agent("ina-busy")
    call("POST", f"/lifecycle/agents/{busy}/calls", {"environment": "production", "cost": 0, "tokens": 1})
    call("POST", "/evaluation/traces", {"agent_id": busy, "trace_id": f"ina-{stamp}", "outcome": "success",
                                        "steps": []})
    s, lst = call("GET", "/agents/inactive?days=1")
    check("agente con chiamata e traccia recenti: escluso", all(x["id"] != busy for x in lst), "")
    s, lst = call("GET", "/agents/inactive?days=1")
    check("agente mai attivo: ancora elencato con finestra di 1 giorno", any(x["id"] == never for x in lst), "")

    section("Stati esclusi")
    susp = new_agent("ina-susp")
    call("POST", f"/lifecycle/agents/{susp}/suspend", {})
    s, lst = call("GET", "/agents/inactive?days=30")
    check("agente sospeso: non elencato (non è attivo)", all(x["id"] != susp for x in lst), "")
    ret = new_agent("ina-ret")
    call("POST", f"/lifecycle/agents/{ret}/retire", {})
    s, lst = call("GET", "/agents/inactive?days=30")
    check("agente dismesso: non elencato", all(x["id"] != ret for x in lst), "")

    section("Pulizia")
    for a in agents:
        call("DELETE", f"/agents/{a}")
    s, lst = call("GET", "/agents/inactive?days=30")
    check("agenti di test rimossi", all(x["id"] not in agents for x in lst), "")

    total = passed + len(failed)
    print(f"\nRisultato: {passed}/{total} controlli superati")
    if failed:
        print("Falliti:")
        for f in failed:
            print(f"  - {f}")
        sys.exit(1)


if __name__ == "__main__":
    main()
