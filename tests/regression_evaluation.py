#!/usr/bin/env python3
"""Regressione del punto 6 della specifica: valutazione degli agenti.

Stessi criteri di regression_backend.py: solo HTTP e libreria standard, dati di test
creati e rimossi dalla suite, esito 0 se tutti i controlli passano.

Le regole di valutazione sono globali e hanno un solo esemplare per tipo: la suite le
rimuove all'inizio e alla fine (sono dati di laboratorio).

Copre: tracce con passi, validazione, duplicati, regole (CRUD e validazione), violazioni
all'ingestione (token, latenza, memoria stale, tasso di errore, drift), punteggi del
giudice esterno, riepilogo, violazioni, audit.

Uso:  python3 tests/regression_evaluation.py [base_url]
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


def trace(agent, tid, outcome="success", goal=True, steps=None, cost=0.0):
    body = {"agent_id": agent, "trace_id": tid, "outcome": outcome, "goal_met": goal, "cost": cost,
            "steps": steps if steps is not None else [
                {"type": "llm", "name": "modello", "tokens": 10, "latency_ms": 100, "ok": True}]}
    return call("POST", "/evaluation/traces", body)


def clear_rules():
    s, rules = call("GET", "/evaluation/rules")
    for r in rules or []:
        call("DELETE", f"/evaluation/rules/{r['id']}")


def main():
    clear_rules()
    agents = []

    s, b = call("POST", "/agents", {"name": "ev-agent", "origin": "internal", "status": "active"})
    ag = b["id"]
    agents.append(ag)

    # ------------------------------------------------------------------
    section("Tracce: validazione")
    s, b = call("POST", "/evaluation/traces", {"agent_id": ag, "outcome": "success"})
    check("trace_id mancante -> 400", s == 400, (s, b))
    s, b = trace(ag, "t-x", outcome="magia")
    check("esito non valido -> 400", s == 400, (s, b))
    s, b = trace(ag, "t-x", steps=[{"type": "chat", "name": "x"}])
    check("tipo di passo non valido -> 400", s == 400, (s, b))
    s, b = trace(ag, "t-x", steps=[{"type": "tool", "name": ""}])
    check("passo senza nome -> 400", s == 400, (s, b))
    s, b = trace(ag, "t-x", cost=-1)
    check("costo negativo -> 400", s == 400, (s, b))
    s, b = trace(999999, "t-x")
    check("agente inesistente -> 404", s == 404, (s, b))

    # ------------------------------------------------------------------
    section("Tracce: registrazione")
    s, b = trace(ag, "t-1", outcome="success", goal=True, steps=[
        {"type": "llm", "name": "modello", "tokens": 120, "latency_ms": 300, "ok": True},
        {"type": "tool", "name": "ricerca", "tokens": 0, "latency_ms": 50, "ok": False},
        {"type": "memory", "name": "memoria", "tokens": 5, "latency_ms": 10, "ok": True, "memory_age_days": 3}],
        cost=0.02)
    check("traccia registrata 201 con totali", s == 201 and b.get("total_tokens") == 125
          and b.get("total_latency_ms") == 360 and b.get("step_errors") == 1 and b.get("steps") == 3, (s, b))
    check("traccia senza testo dei passi", b is not None and "text" not in json.dumps(b), b)
    s, b2 = trace(ag, "t-1")
    check("trace_id duplicato per l'agente -> 409", s == 409, (s, b2))
    s, lst = call("GET", f"/evaluation/traces?agent_id={ag}&limit=5")
    check("elenco tracce per agente", s == 200 and len(lst) == 1 and lst[0]["trace_id"] == "t-1", (s, lst))
    s, b = call("GET", "/evaluation/traces?limit=0")
    check("limite tracce fuori range -> 400", s == 400, (s, b))

    # ------------------------------------------------------------------
    section("Regole: CRUD e validazione")
    s, b = call("POST", "/evaluation/rules", {"type": "magia", "params": {}})
    check("tipo di regola non valido -> 400", s == 400, (s, b))
    s, b = call("POST", "/evaluation/rules", {"type": "max_tokens_per_trace", "params": {}})
    check("max_tokens_per_trace senza max -> 400", s == 400, (s, b))
    s, b = call("POST", "/evaluation/rules", {"type": "max_error_rate", "params": {"max": 2}})
    check("max_error_rate fuori da 0..1 -> 400", s == 400, (s, b))
    s, r_tok = call("POST", "/evaluation/rules", {"type": "max_tokens_per_trace", "params": {"max": 100}})
    check("regola max_tokens_per_trace 201", s == 201 and r_tok.get("enabled") is True, (s, r_tok))
    s, b = call("POST", "/evaluation/rules", {"type": "max_tokens_per_trace", "params": {"max": 5}})
    check("secondo esemplare dello stesso tipo -> 409", s == 409, (s, b))
    s, b = call("GET", f"/evaluation/rules/{r_tok['id']}")
    check("dettaglio regola 200", s == 200 and b.get("params") == {"max": 100}, (s, b))
    s, b = call("GET", "/evaluation/rules/999999")
    check("regola inesistente -> 404", s == 404, (s, b))

    # ------------------------------------------------------------------
    section("Violazioni all'ingestione")
    s, b = trace(ag, "t-big", steps=[{"type": "llm", "name": "m", "tokens": 500, "latency_ms": 10}])
    check("traccia oltre il limite di token: registrata", s == 201, (s, b))
    s, v = call("GET", f"/evaluation/agents/{ag}/violations?limit=10")
    tok = [x for x in v if x["rule_type"] == "max_tokens_per_trace"]
    check("violazione max_tokens_per_trace registrata con traccia",
          len(tok) == 1 and tok[0]["trace_ref"] is not None and "500" in tok[0]["detail"], v)
    check("params token: value e max", tok and tok[0]["params"] == {"value": 500, "max": 100}, tok)

    s, r_lat = call("POST", "/evaluation/rules", {"type": "max_latency_ms", "params": {"max": 1000}})
    s, b = trace(ag, "t-slow", steps=[{"type": "llm", "name": "m", "tokens": 1, "latency_ms": 2000}])
    s, v = call("GET", f"/evaluation/agents/{ag}/violations?limit=50")
    check("violazione max_latency_ms", any(x["rule_type"] == "max_latency_ms" for x in v), v[:2])
    lat = [x for x in v if x["rule_type"] == "max_latency_ms"]
    check("params latenza: value e max", lat and lat[0]["params"] == {"value": 2000, "max": 1000}, lat)

    s, r_mem = call("POST", "/evaluation/rules", {"type": "memory_stale_days", "params": {"max_days": 30}})
    s, b = trace(ag, "t-mem", steps=[{"type": "memory", "name": "vecchia", "tokens": 1, "latency_ms": 1,
                                       "memory_age_days": 60}])
    s, v = call("GET", f"/evaluation/agents/{ag}/violations?limit=50")
    check("memoria stale oltre la soglia: violazione", any(x["rule_type"] == "memory_stale_days" for x in v), v[:2])
    mem = [x for x in v if x["rule_type"] == "memory_stale_days"]
    check("params memoria: count e max_days", mem and mem[0]["params"] == {"count": 1, "max_days": 30}, mem)
    s, b = trace(ag, "t-mem2", steps=[{"type": "memory", "name": "fresca", "tokens": 1, "latency_ms": 1,
                                        "memory_age_days": 2}])
    s, v2 = call("GET", f"/evaluation/agents/{ag}/violations?limit=50")
    check("memoria recente: nessuna nuova violazione", len(v2) == len(v), (len(v), len(v2)))

    # Regole disattivate non producono violazioni.
    call("PUT", f"/evaluation/rules/{r_lat['id']}", {"type": "max_latency_ms", "params": {"max": 1000},
                                                       "enabled": False})
    before = len(call("GET", f"/evaluation/agents/{ag}/violations?limit=500")[1])
    trace(ag, "t-slow2", steps=[{"type": "llm", "name": "m", "tokens": 1, "latency_ms": 9000}])
    after = len(call("GET", f"/evaluation/agents/{ag}/violations?limit=500")[1])
    check("regola disattivata: nessuna violazione", after == before, (before, after))

    # ------------------------------------------------------------------
    section("Tasso di errore e drift")
    s, b = call("POST", "/agents", {"name": "ev-drift", "origin": "internal", "status": "active"})
    dr = b["id"]
    agents.append(dr)
    s, r_err = call("POST", "/evaluation/rules", {"type": "max_error_rate", "params": {"max": 0.3}})
    trace(dr, "d-1", outcome="failure")
    trace(dr, "d-2", outcome="failure")
    s, v = call("GET", f"/evaluation/agents/{dr}/violations?limit=50")
    check("tasso di errore oltre il massimo: violazione", any(x["rule_type"] == "max_error_rate" for x in v), v)
    er = [x for x in v if x["rule_type"] == "max_error_rate"]
    check("params tasso di errore: rate e max", er and er[0]["params"] == {"rate": 1.0, "max": 0.3}, er)
    for i in range(10):
        trace(dr, f"ok-{i}", outcome="success")
    for i in range(10):
        trace(dr, f"ko-{i}", outcome="failure")
    s, r_drift = call("POST", "/evaluation/rules", {"type": "drift_threshold", "params": {"max_delta": 0.2}})
    s, v = call("GET", f"/evaluation/agents/{dr}/violations?limit=500")
    s, sm = call("GET", f"/evaluation/agents/{dr}/summary")
    check("drift: flag quando le ultime 10 tracce peggiorano", sm.get("drift_flag") is True
          and sm.get("drift_delta", 0) < -0.9, sm)
    s, b = trace(dr, "ko-last", outcome="failure")
    dr_v = [x for x in call("GET", f"/evaluation/agents/{dr}/violations?limit=500")[1] if x["rule_type"] == "drift_threshold"]
    check("params drift: delta e max_delta", dr_v and set(dr_v[0]["params"]) == {"delta", "max_delta"}
          and dr_v[0]["params"]["max_delta"] == 0.2, dr_v[:1])
    check("drift: violazione registrata all'ingestione",
          any(x["rule_type"] == "drift_threshold" for x in call("GET", f"/evaluation/agents/{dr}/violations?limit=500")[1]))

    # ------------------------------------------------------------------
    section("Punteggi del giudice esterno")
    s, b = call("POST", f"/evaluation/agents/{ag}/scores", {"score": 0, "judge": "manual"})
    check("punteggio 0 -> 400", s == 400, (s, b))
    s, b = call("POST", f"/evaluation/agents/{ag}/scores", {"score": 6, "judge": "manual"})
    check("punteggio 6 -> 400", s == 400, (s, b))
    s, b = call("POST", f"/evaluation/agents/{ag}/scores", {"score": 4, "judge": ""})
    check("giudice mancante -> 400", s == 400, (s, b))
    s, b = call("POST", f"/evaluation/agents/{ag}/scores", {"score": 4, "judge": "giudice-esterno", "note": "ok"})
    check("punteggio registrato 201", s == 201 and b.get("score") == 4, (s, b))
    s, b = call("POST", f"/evaluation/agents/{ag}/scores", {"score": 2, "judge": "manual"})

    # ------------------------------------------------------------------
    section("Riepilogo")
    s, sm = call("GET", f"/evaluation/agents/{ag}/summary")
    keys = {"traces", "success_rate", "goal_rate", "avg_latency_ms", "avg_tokens_per_trace", "error_rate",
            "rule_violations", "avg_score", "drift_flag", "stale_memory_steps"}
    check("riepilogo: campi di contratto", s == 200 and keys <= set(sm), sm)
    check("riepilogo: punteggio medio 3", sm.get("avg_score") == 3.0, sm)
    check("riepilogo: tasso di successo tra 0 e 1", 0 <= sm.get("success_rate", -1) <= 1, sm)
    check("riepilogo: passi di memoria stale contati", sm.get("stale_memory_steps") == 1, sm)
    s, b = call("GET", "/evaluation/agents/999999/summary")
    check("riepilogo di agente inesistente -> 404", s == 404, (s, b))

    # ------------------------------------------------------------------
    section("Audit")
    s, aud = call("GET", "/audit?limit=100")
    keys = []
    for e in aud:
        try:
            keys.append(json.loads(e["detail"]).get("key"))
        except (TypeError, json.JSONDecodeError, AttributeError):
            pass
    check("audit: chiavi di valutazione presenti",
          {"trace_recorded", "eval_rule_saved", "score_recorded"} <= set(keys), keys[:10])

    # ------------------------------------------------------------------
    section("Pulizia")
    clear_rules()
    for aid in agents:
        call("DELETE", f"/agents/{aid}")
    s, rules = call("GET", "/evaluation/rules")
    check("nessuna regola residua", s == 200 and rules == [], rules)

    total = passed + len(failed)
    print(f"\nRisultato: {passed}/{total} controlli superati")
    if failed:
        print("Falliti:")
        for f in failed:
            print(f"  - {f}")
        sys.exit(1)


if __name__ == "__main__":
    main()
