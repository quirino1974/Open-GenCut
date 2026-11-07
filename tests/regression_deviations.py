#!/usr/bin/env python3
"""Regressione delle deviazioni dei ragionamenti (docs/API.txt, sezione Deviazioni).

Controlli: lettura e sostituzione della sequenza di riferimento, traccia conforme, passi fuori
sequenza, ordine invertito, strumenti non consentiti dall'allowlist, agente senza riferimento
(409), traccia inesistente (404), validazioni. Dati di test creati e rimossi.
"""
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from session import call3, raw_call, new_opener  # noqa: E402

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


def step(t, name):
    return {"type": t, "name": name, "tokens": 10, "latency_ms": 5, "ok": True}


def make_trace(agent, tag, label, steps):
    s, b, raw = call3("POST", "/evaluation/traces", {
        "agent_id": agent, "trace_id": f"{tag}-{label}", "outcome": "success", "goal_met": True,
        "cost": 0, "steps": steps})
    return b.get("id") if s in (200, 201) else None, (s, b)


def kinds(report):
    return [(d.get("kind"), d.get("name")) for d in report.get("deviations", [])]


def main():
    tag = f"DEV-{int(time.time())}"
    agents, traces, resources = [], [], []

    s, b, _ = call3("POST", "/agents", {"name": f"{tag}-agente", "origin": "internal"})
    ag = b["id"]
    agents.append(ag)
    s, b, _ = call3("POST", "/agents", {"name": f"{tag}-senza-riferimento", "origin": "internal"})
    ag_noref = b["id"]
    agents.append(ag_noref)

    section("Accesso")
    s, _, _ = raw_call(new_opener(), "POST", "/evaluation/deviations", {"trace_ref": 1})
    check("senza sessione -> 401", s == 401, s)

    section("Sequenza di riferimento")
    ref = [{"type": "llm", "name": "classifica"}, {"type": "tool", "name": "cerca"},
           {"type": "reasoning", "name": "decide"}]
    s, b, _ = call3("PUT", f"/evaluation/agents/{ag}/reference", {"steps": ref})
    check("PUT riferimento -> 200", s == 200 and len(b.get("steps", [])) == 3, (s, b))
    s, b, _ = call3("GET", f"/evaluation/agents/{ag}/reference")
    check("GET riferimento: stessi passi in ordine", s == 200 and [x["name"] for x in b["steps"]] ==
          ["classifica", "cerca", "decide"], (s, b))
    s, b, _ = call3("PUT", f"/evaluation/agents/{ag}/reference", {"steps": [{"type": "boh", "name": "x"}]})
    check("tipo di passo non valido -> 400", s == 400, (s, b))
    s, b, _ = call3("PUT", f"/evaluation/agents/{ag}/reference", {"steps": []})
    check("riferimento vuoto -> 400", s == 400, (s, b))

    section("Strumenti: server MCP e allowlist")
    s, b, _ = call3("POST", "/integrations/resources",
                    {"name": f"{tag}-mcp", "kind": "mcp_server", "endpoint": "http://mcp-fake.example.test"})
    res = b.get("id") if s in (200, 201) else None
    if res:
        resources.append(res)
        s, t, _ = call3("POST", f"/integrations/resources/{res}/tools", {"name": "cerca"})
        tool = t.get("id") if s in (200, 201) else None
        if tool:
            # Il permesso per strumento richiede la coppia agente-server in allowlist (docs/MCP.txt).
            call3("POST", "/integrations/allowlist", {"agent_id": ag, "resource_id": res, "requires_approval": False})
            s, _, _ = call3("POST", "/integrations/tool-allowlist", {"agent_id": ag, "tool_id": tool})
            check("strumento 'cerca' consentito all'agente", s in (200, 201), s)
    else:
        print("  NOTE strumenti non creati: controllo dello strumento consentito saltato")

    section("Traccia conforme")
    t1, info = make_trace(ag, tag, "conforme", [step("llm", "classifica"), step("tool", "cerca"),
                                                 step("reasoning", "decide")])
    traces.append(t1)
    check("traccia conforme creata", t1 is not None, info)
    if t1:
        s, rep, _ = call3("POST", "/evaluation/deviations", {"trace_ref": t1})
        check("risposta 200", s == 200, (s, rep))
        check("conforme: nessuna deviazione", rep and rep.get("compliant") is True and rep.get("deviations") == [],
              rep)

    section("Passi fuori sequenza e strumento non consentito")
    t2, info = make_trace(ag, tag, "devia", [step("llm", "classifica"), step("reasoning", "decide"),
                                              step("tool", "cerca"), step("tool", "cancella")])
    traces.append(t2)
    if t2:
        s, rep, _ = call3("POST", "/evaluation/deviations", {"trace_ref": t2})
        check("non conforme", rep and rep.get("compliant") is False, rep)
        ks = kinds(rep or {})
        check("passo 'cancella' fuori sequenza", ("unexpected_step", "cancella") in ks, ks)
        check("strumento 'cancella' non consentito", ("tool_not_allowed", "cancella") in ks, ks)
        check("strumento 'cerca' consentito non segnalato", ("tool_not_allowed", "cerca") not in ks, ks)
        check("ordine: un passo di riferimento mancante o fuori posto",
              any(k == "missing_step" for k, _ in ks), ks)

    section("Ordine invertito")
    t3, info = make_trace(ag, tag, "ordine", [step("reasoning", "decide"), step("llm", "classifica")])
    traces.append(t3)
    if t3:
        s, rep, _ = call3("POST", "/evaluation/deviations", {"trace_ref": t3})
        ks = kinds(rep or {})
        check("ordine invertito: non conforme", rep and rep.get("compliant") is False, rep)
        check("ordine invertito: passi fuori posto segnalati",
              any(k in ("unexpected_step", "missing_step") for k, _ in ks), ks)

    section("Errori")
    if traces and traces[0]:
        t4, _ = make_trace(ag_noref, tag, "senza-rif", [step("llm", "x")])
        if t4:
            traces.append(t4)
            s, b, _ = call3("POST", "/evaluation/deviations", {"trace_ref": t4})
            check("agente senza riferimento -> 409", s == 409, (s, b))
    s, b, _ = call3("POST", "/evaluation/deviations", {"trace_ref": 999999999})
    check("traccia inesistente -> 404", s == 404, (s, b))
    s, b, _ = call3("POST", "/evaluation/deviations", {"trace_ref": 0})
    check("trace_ref mancante -> 400", s == 400, (s, b))

    section("Pulizia")
    for r in resources:
        call3("DELETE", f"/integrations/resources/{r}")
    for a in agents:
        call3("DELETE", f"/agents/{a}")

    total = passed + len(failed)
    print(f"\nRisultato: {passed}/{total} controlli superati")
    if failed:
        print("Falliti:")
        for f in failed:
            print(f"  - {f}")
        sys.exit(1)


if __name__ == "__main__":
    main()
