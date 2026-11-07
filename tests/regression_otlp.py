#!/usr/bin/env python3
"""Regressione dell'ingestione OpenTelemetry in JSON (POST /api/evaluation/otlp).

Stessi criteri delle altre suite: esito 0 se tutti i controlli passano, dati di test
creati e rimossi. Il contenuto testuale degli attributi (prompt, risposte) non deve mai
comparire nelle risposte né nelle tracce salvate.
"""
import json
import os
import sys
import time
import urllib.error
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from session import call, _session, API  # noqa: E402

passed = 0
failed = []
SECRET_TEXT = "prompt-segreto-otlp-7f3a"


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


def post_raw(agent_id, body_bytes):
    """Invio grezzo dell'OTLP: restituisce (status, payload) con la sessione di test."""
    req = urllib.request.Request(f"{API}/evaluation/otlp?agent_id={agent_id}", data=body_bytes, method="POST",
                                 headers={"Content-Type": "application/json"})
    try:
        with _session().open(req, timeout=60) as resp:
            raw = resp.read()
            status = resp.status
    except urllib.error.HTTPError as e:
        raw = e.read()
        status = e.code
    try:
        return status, json.loads(raw) if raw else None, raw.decode("utf-8", "replace")
    except json.JSONDecodeError:
        return status, raw.decode("utf-8", "replace"), raw.decode("utf-8", "replace")


def kv_str(k, v):
    return {"key": k, "value": {"stringValue": v}}


def kv_int(k, v):
    return {"key": k, "value": {"intValue": str(v)}}


def kv_bool(k, v):
    return {"key": k, "value": {"boolValue": v}}


def kv_dbl(k, v):
    return {"key": k, "value": {"doubleValue": v}}


def span(trace, sid, parent, name, start, end, attrs=(), error=False):
    return {"traceId": trace, "spanId": sid, "parentSpanId": parent, "name": name,
            "startTimeUnixNano": str(start), "endTimeUnixNano": str(end),
            "status": {"code": 2 if error else 1}, "attributes": list(attrs)}


def request(spans, env=None):
    res = [kv_str("service.name", "agente-test")]
    if env:
        res.append(kv_str("deployment.environment", env))
    return {"resourceSpans": [{"resource": {"attributes": res},
                               "scopeSpans": [{"spans": spans}]}]}


def main():
    stamp = int(time.time())
    agents = []
    s, b = call("POST", "/agents", {"name": f"otlp-{stamp}", "origin": "internal", "status": "active"})
    ag = b["id"]
    agents.append(ag)
    t1 = f"{stamp:032x}"[-32:]
    t2 = f"{stamp + 1:032x}"[-32:]
    t3 = f"{stamp + 2:032x}"[-32:]

    section("Validazione")
    st, p, raw = post_raw(ag, b"{non json")
    check("JSON non valido -> 400", st == 400, (st, p))
    st, p, raw = post_raw(0, json.dumps(request([])).encode())
    check("agent_id mancante -> 400", st == 400, (st, p))
    st, p, raw = post_raw(999999, json.dumps(request([span(t3, "a", "", "x", 1, 2)])).encode())
    check("agente inesistente -> 404", st == 404, (st, p))
    st, p, raw = post_raw(ag, json.dumps(request([{"name": "senza-trace", "spanId": "b"}])).encode())
    check("span senza traceId -> 400", st == 400, (st, p))

    section("Mappatura delle tracce")
    t0 = 1_000_000_000
    spans1 = [
        span(t1, "r1", "", "agente.richiesta", t0, t0 + 900_000_000,
             [kv_bool("gencut.goal_met", True), kv_dbl("gencut.cost", 0.25),
              kv_str("gen_ai.prompt", SECRET_TEXT)]),
        span(t1, "c1", "r1", "chiamata.modello", t0, t0 + 200_000_000,
             [kv_str("gen_ai.request.model", "claude-haiku-5-5"), kv_int("gen_ai.usage.input_tokens", 100),
              kv_int("gen_ai.usage.output_tokens", 50), kv_str("gen_ai.completion", SECRET_TEXT)]),
        span(t1, "t1", "r1", "strumento.ricerca", t0, t0 + 50_000_000, [], error=True),
    ]
    spans2 = [span(t2, "r2", "", "richiesta.fallita", t0, t0 + 10_000_000, [], error=True)]
    spans3 = [
        span(t3, "r3", "", "richiesta.ok", t0, t0 + 5_000_000, []),
        span(t3, "m3", "r3", "memoria.lettura", t0, t0 + 1_000_000,
             [kv_str("gencut.step_type", "memory"), kv_int("gencut.memory_age_days", 5)]),
    ]
    body = request(spans1 + spans2 + spans3, env="production")
    st, p, raw = post_raw(ag, json.dumps(body).encode())
    check("batch accettato 200 con tracce create", st == 200 and p.get("traces_created") == 3
          and p.get("spans") == 6, (st, p))
    check("il testo degli attributi non compare nella risposta", SECRET_TEXT not in raw, raw[:150])

    s, lst = call("GET", f"/evaluation/traces?agent_id={ag}&limit=10")
    by_tid = {t["trace_id"]: t for t in lst}
    tr1 = by_tid.get(t1, {})
    check("esito parziale: un passo in errore", tr1.get("outcome") == "partial", tr1)
    check("obiettivo raggiunto dallo span radice", tr1.get("goal_met") is True, tr1)
    check("costo dallo span radice", abs(tr1.get("cost", 0) - 0.25) < 1e-9, tr1)
    check("token: input più output", tr1.get("total_tokens") == 150, tr1)
    check("passi: tre span registrati", tr1.get("steps") == 3, tr1)
    check("errore del passo contato", tr1.get("step_errors") == 1, tr1)
    check("ambiente dal resource", tr1.get("environment") == "production", tr1)
    check("esito di fallimento: radice in errore", by_tid.get(t2, {}).get("outcome") == "failure", by_tid.get(t2))
    check("esito di successo senza errori", by_tid.get(t3, {}).get("outcome") == "success", by_tid.get(t3))
    check("il testo non è salvato nelle tracce", SECRET_TEXT not in json.dumps(lst), "")

    section("Deduplicazione")
    st, p, raw = post_raw(ag, json.dumps(body).encode())
    check("stesso batch: tracce già presenti saltate", st == 200 and p.get("traces_created") == 0
          and p.get("traces_skipped") == 3, (st, p))

    section("Pulizia")
    for a in agents:
        call("DELETE", f"/agents/{a}")

    total = passed + len(failed)
    print(f"\nRisultato: {passed}/{total} controlli superati")
    if failed:
        print("Falliti:")
        for f in failed:
            print(f"  - {f}")
        sys.exit(1)


if __name__ == "__main__":
    main()
