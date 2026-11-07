#!/usr/bin/env python3
"""Regressione della scoperta: import di manifesti agente (POST /api/agents/import).

Controlli: accesso senza sessione, creazione dei mancanti, salto dei duplicati (già presenti,
ripetuti nello stesso elenco, con maiuscole diverse), errori per singolo elemento senza
bloccare il lotto, rifiuto dei campi sconosciuti e della lista vuota, reimport idempotente.
Dati di test creati e rimossi.
"""
import json
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from session import call3, raw_call, new_opener, API  # noqa: E402
import urllib.request  # noqa: E402

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


def post_raw(data):
    """Invio grezzo sulla sessione di test: restituisce (status, payload)."""
    from session import send_raw
    return send_raw("POST", "/agents/import", data=data, headers={"Content-Type": "application/json"})


def main():
    tag = f"DISC-{int(time.time())}"
    created_ids = []

    section("Accesso")
    s, _, _ = raw_call(new_opener(), "POST", "/agents/import", [{"name": tag + "-anon", "origin": "internal"}])
    check("senza sessione -> 401", s == 401, s)

    # Agente già presente, usato per il controllo del duplicato con maiuscole diverse.
    s, b, _ = call3("POST", "/agents", {"name": f"{tag}-Presente", "origin": "internal"})
    existing = b["id"]
    created_ids.append(existing)

    section("Elenco vuoto e campi sconosciuti")
    st, p = post_raw(b"[]")
    check("elenco vuoto -> 400", st == 400, (st, p))
    st, p = post_raw(json.dumps([{"name": tag + "-x", "origin": "internal", "sconosciuto": 1}]).encode())
    check("campo sconosciuto -> 400", st == 400, (st, p))
    st, p = post_raw(json.dumps({"name": "non è un elenco"}).encode())
    check("oggetto invece di elenco -> 400", st == 400, (st, p))

    section("Import: creati, saltati, errori")
    manifests = [
        {"name": f"{tag}-A", "origin": "internal", "owner": "team-a", "framework": "LangGraph",
         "version": "1.0", "endpoint": "https://agenti.example.test/a"},
        {"name": f"{tag}-A", "origin": "internal"},            # duplicato nello stesso elenco
        {"name": f"{tag}-presente", "origin": "marketplace"},  # stesso nome del presente, maiuscole diverse
        {"name": f"{tag}-C", "origin": "origine-sbagliata"},   # non valido
    ]
    st, p = post_raw(json.dumps(manifests).encode())
    check("risposta 200", st == 200, (st, p))
    check("creato 1", p and p.get("created") == 1, p)
    check("saltati 2 (ripetuto e già presente)", p and p.get("skipped") == 2, p)
    check("errori 1 (origine non valida)", p and p.get("errors") == 1, p)
    items = (p or {}).get("items", [])
    check("un elemento per manifesto", len(items) == 4, items)
    by_index = {i.get("index"): i for i in items}
    check("il creato ha un id", by_index.get(0, {}).get("result") == "created" and by_index[0].get("id"), by_index.get(0))
    check("il duplicato è saltato", by_index.get(1, {}).get("result") == "skipped", by_index.get(1))
    check("il nome con maiuscole diverse è saltato", by_index.get(2, {}).get("result") == "skipped", by_index.get(2))
    check("l'origine errata è segnalata", by_index.get(3, {}).get("result") == "error"
          and "origin" in by_index[3].get("message", ""), by_index.get(3))
    if by_index.get(0, {}).get("id"):
        created_ids.append(by_index[0]["id"])

    section("Dati importati")
    if by_index.get(0, {}).get("id"):
        s, a, _ = call3("GET", f"/agents/{by_index[0]['id']}")
        check("owner importato", a.get("owner") == "team-a", a)
        check("framework importato", a.get("framework") == "LangGraph", a)
        check("versione importata", a.get("version") == "1.0", a)
        check("endpoint importato", a.get("endpoint") == "https://agenti.example.test/a", a)
    s, lst, _ = call3("GET", f"/agents?q={tag}")
    names = [x.get("name") for x in (lst or [])]
    check("l'agente errato non è stato creato", f"{tag}-C" not in names, names)

    section("Reimport idempotente")
    st, p = post_raw(json.dumps(manifests[:1]).encode())
    check("reimport: nessun nuovo agente", st == 200 and p.get("created") == 0 and p.get("skipped") == 1, (st, p))

    section("Pulizia")
    for aid in created_ids:
        call3("DELETE", f"/agents/{aid}")

    total = passed + len(failed)
    print(f"\nRisultato: {passed}/{total} controlli superati")
    if failed:
        print("Falliti:")
        for f in failed:
            print(f"  - {f}")
        sys.exit(1)


if __name__ == "__main__":
    main()
