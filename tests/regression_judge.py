#!/usr/bin/env python3
"""Regressione del giudizio automatico (POST /api/evaluation/agents/{id}/judge).

Il giudice passa dal proxy della chat: stessi controlli di accesso e guardrail. La chiave API
serve solo alla chiamata e non deve comparire in nessuna risposta né nelle tracce.

I controlli che richiedono un fornitore finto girano solo se CHAT_TEST_PROVIDER_URL è impostata
e il backend è avviato con CHAT_ANTHROPIC_URL puntato allo stesso fornitore (vedi regression_chat.py).
Gli altri controlli (validazione, accesso negato, traccia di un altro agente) girano sempre.
"""
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from session import call3, raw_call, new_opener  # noqa: E402

PROVIDER_TEST = os.environ.get("CHAT_TEST_PROVIDER_URL")
CANARY = "chiave-giudice-canary-9d1"

passed = 0
failed = []
skipped = []


def check(name, cond, detail=""):
    global passed
    if cond:
        passed += 1
        print(f"  OK   {name}")
    else:
        failed.append(name)
        print(f"  FAIL {name} {detail}")


def skip(name, why):
    skipped.append(name)
    print(f"  SKIP {name} ({why})")


def section(title):
    print(f"\n== {title}")


def step(t, name):
    return {"type": t, "name": name, "tokens": 10, "latency_ms": 5, "ok": True}


def judge_body(trace, resource, rubric, **extra):
    body = {"trace_ref": trace, "provider": "anthropic", "api_key": CANARY, "model": "claude-haiku-5-5",
            "rubric": rubric, "resource_id": resource}
    body.update(extra)
    return body


def score_count(agent):
    s, sm, _ = call3("GET", f"/evaluation/agents/{agent}/summary")
    return sm.get("scores", -1) if s == 200 else -1


def main():
    tag = f"JUD-{int(time.time())}"
    agents, resources = [], []

    s, b, _ = call3("POST", "/agents", {"name": f"{tag}-agente", "origin": "internal", "status": "active"})
    ag = b["id"]
    agents.append(ag)
    s, b, _ = call3("POST", "/agents", {"name": f"{tag}-altro", "origin": "internal"})
    other = b["id"]
    agents.append(other)

    s, b, _ = call3("POST", "/integrations/resources", {"name": f"{tag}-llm", "kind": "llm_model", "status": "enabled"})
    rs = b["id"]
    resources.append(rs)
    call3("POST", "/integrations/allowlist", {"agent_id": ag, "resource_id": rs})

    s, b, _ = call3("POST", "/integrations/resources", {"name": f"{tag}-llm-negato", "kind": "llm_model", "status": "enabled"})
    rs_denied = b["id"]
    resources.append(rs_denied)

    s, b, _ = call3("POST", "/evaluation/traces", {"agent_id": ag, "trace_id": f"{tag}-t1", "outcome": "success",
                                                    "goal_met": True, "cost": 0.01,
                                                    "steps": [step("llm", "classifica"), step("reasoning", "decide")]})
    trace = b["id"]
    s, b, _ = call3("POST", "/evaluation/traces", {"agent_id": other, "trace_id": f"{tag}-t2", "outcome": "success",
                                                    "goal_met": True, "cost": 0,
                                                    "steps": [step("llm", "altro")]})
    other_trace = b["id"]

    section("Accesso")
    s, _, _ = raw_call(new_opener(), "POST", f"/evaluation/agents/{ag}/judge", judge_body(trace, rs, "x"))
    check("senza sessione -> 401", s == 401, s)

    section("Validazione")
    s, b, raw = call3("POST", f"/evaluation/agents/{ag}/judge", judge_body(trace, rs, ""))
    check("rubrica vuota -> 400", s == 400, (s, b))
    s, b, raw = call3("POST", f"/evaluation/agents/{ag}/judge", judge_body(0, rs, "x"))
    check("trace_ref mancante -> 400", s == 400, (s, b))
    s, b, raw = call3("POST", f"/evaluation/agents/{ag}/judge", judge_body(trace, rs, "x", provider="altro"))
    check("fornitore non valido -> 400", s == 400, (s, b))
    s, b, raw = call3("POST", f"/evaluation/agents/{ag}/judge", judge_body(trace, rs, "x", api_key=""))
    check("chiave mancante -> 400", s == 400, (s, b))
    check("nessuna risposta di validazione contiene la chiave", CANARY not in raw, raw[:120])

    section("Traccia di un altro agente")
    s, b, raw = call3("POST", f"/evaluation/agents/{ag}/judge", judge_body(other_trace, rs, "GIUDICE_4"))
    check("traccia di un altro agente -> 404", s == 404, (s, b))

    section("Accesso negato: nessun punteggio")
    before = score_count(ag)
    s, b, raw = call3("POST", f"/evaluation/agents/{ag}/judge", judge_body(trace, rs_denied, "GIUDICE_4"))
    check("risorsa non in allowlist: nessun punteggio", s == 200 and b.get("scored") is False
          and b.get("decision") != "allowed", (s, b))
    check("accesso negato: nessun punteggio registrato", score_count(ag) == before, (before, score_count(ag)))

    section("Fornitore finto (solo se configurato)")
    if PROVIDER_TEST:
        before = score_count(ag)
        s, b, raw = call3("POST", f"/evaluation/agents/{ag}/judge", judge_body(trace, rs, "GIUDICE_4 Rubrica di prova."))
        check("punteggio 4 registrato: 201", s == 201 and b.get("score") == 4, (s, b))
        check("giudice registrato come llm:<modello>", b and b.get("judge") == "llm:claude-haiku-5-5", b)
        check("la chiave non compare nella risposta", CANARY not in raw, raw[:120])
        check("un punteggio in più nel riepilogo", score_count(ag) == before + 1, (before, score_count(ag)))

        s, b, raw = call3("POST", f"/evaluation/agents/{ag}/judge", judge_body(trace, rs, "GIUDICE_JSON Rubrica."))
        check("risposta in forma JSON {\"score\": n}: punteggio 5", s == 201 and b.get("score") == 5, (s, b))

        before = score_count(ag)
        s, b, raw = call3("POST", f"/evaluation/agents/{ag}/judge", judge_body(trace, rs, "GIUDICE_TESTO Rubrica."))
        check("risposta senza punteggio puro: 502 judge_invalid_reply",
              s == 502 and b.get("error") == "judge_invalid_reply", (s, b))
        check("risposta non valida: nessun punteggio salvato", score_count(ag) == before, (before, score_count(ag)))
        check("la chiave non compare nell'errore", CANARY not in raw, raw[:120])

        s, b, raw = call3("POST", f"/evaluation/agents/{ag}/judge", judge_body(trace, rs, "ERRORE_FORNITORE"))
        check("errore del fornitore: 502 provider_error", s == 502 and b.get("error") == "provider_error", (s, b))
        check("errore del fornitore: la chiave non compare", CANARY not in raw, raw[:120])
    else:
        for name in ["punteggio 4 registrato", "risposta JSON punteggio 5", "risposta non valida 502",
                     "errore del fornitore"]:
            skip(name, "CHAT_TEST_PROVIDER_URL non impostata")

    section("Pulizia")
    for r in resources:
        call3("DELETE", f"/integrations/resources/{r}")
    for a in agents:
        call3("DELETE", f"/agents/{a}")

    total = passed + len(failed)
    print(f"\nRisultato: {passed}/{total} controlli superati, {len(skipped)} saltati")
    if failed:
        print("Falliti:")
        for f in failed:
            print(f"  - {f}")
        sys.exit(1)


if __name__ == "__main__":
    main()
