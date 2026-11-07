"""Popola il registro con dati dimostrativi: 4 agenti con attivita' in ogni modulo.

Uso: python3 -I tests/seed_demo.py [base_url]            crea i dati
     python3 -I tests/seed_demo.py [base_url] --clean    rimuove i dati creati (nomi che iniziano per "Demo - ")
Tutti i dati hanno nomi che iniziano con "Demo - ". Non tocca altri dati.
"""
import json
import random
import sys
import urllib.error
import urllib.request

ARGS = [a for a in sys.argv[1:] if not a.startswith("--")]
CLEAN = "--clean" in sys.argv
BASE = (ARGS[0] if ARGS else "http://localhost:8080").rstrip("/") + "/api"
PFX = "Demo - "
random.seed(7)
errors = []


def call(method, path, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(BASE + path, data=data, method=method,
                                 headers={"Content-Type": "application/json", "Accept": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=15) as r:
            txt = r.read().decode()
            return r.status, (json.loads(txt) if txt else None)
    except urllib.error.HTTPError as e:
        txt = e.read().decode()
        try:
            return e.code, json.loads(txt) if txt else None
        except ValueError:
            return e.code, None


def step(label, st, body=None, ok=(200, 201, 204)):
    if st not in ok:
        errors.append((label, st, body))
        print("  ATTENZIONE " + label + ": " + str(st) + " " + str(body and body.get("message", "")))
    return body if st in ok else None


def clean():
    removed = 0
    for path, key in [("/agents", "name"), ("/integrations/resources", "name"), ("/guardrails", "name"),
                      ("/compliance/policies", "name"), ("/marketplace/listings", "name")]:
        st, rows = call("GET", path)
        for r in (rows if isinstance(rows, list) else []):
            if str(r.get(key, "")).startswith(PFX):
                call("DELETE", path + "/" + str(r["id"]))
                removed += 1
    print("Dati dimostrativi rimossi: " + str(removed))


if CLEAN:
    clean()
    sys.exit(0)

print("Creo i dati dimostrativi su " + BASE)

# 1. risorse
RES = {}
for key, name, kind, endpoint in [
    ("llm", "Modello LLM", "llm_model", "https://api.anthropic.com/v1/messages"),
    ("gw", "Gateway AI", "ai_gateway", "https://gateway.example.invalid"),
    ("crm", "API CRM", "api", "https://crm.example.invalid/api"),
    ("mcp", "MCP documenti", "mcp_server", "https://mcp.example.invalid"),
    ("data", "Fonte dati clienti", "data_source", "postgres://dati.example.invalid/clienti")]:
    b = step("risorsa " + name, *call("POST", "/integrations/resources", {"name": PFX + name, "kind": kind, "endpoint": endpoint, "status": "enabled"}))
    if b:
        RES[key] = b["id"]

# 2. agenti
AG = {}
agents = [
    ("asst", "Assistente clienti", "internal", "Quirino", "active", True, "it", "LangGraph"),
    ("contr", "Analisi contratti", "third_party", "Fornitore esterno", "active", False, "", "CrewAI"),
    ("rep", "Agente report", "internal", "Quirino", "active", False, "", "AutoGen"),
    ("vecchio", "Agente di prova", "internal", "Quirino", "suspended", False, "", "Google ADK"),
]
for key, name, origin, owner, status, people, lang, fw in agents:
    body = {"name": PFX + name, "description": "Agente dimostrativo", "origin": origin, "vendor": "Demo", "version": "1.0",
            "owner": owner, "status": "registered", "model": "modello-demo", "endpoint": "", "cost_monthly": 0,
            "currency": "EUR", "interacts_with_people": people, "interaction_language": lang or None, "framework": fw}
    b = step("agente " + name, *call("POST", "/agents", body))
    if b:
        AG[key] = b["id"]
        if status == "active":
            step("attiva " + name, *call("PUT", "/agents/%d" % b["id"], dict(body, status="active")))

# 3. allowlist, limiti, verifiche d'accesso
def allow(a, r, sens=False):
    return step("allowlist", *call("POST", "/integrations/allowlist", {"agent_id": AG[a], "resource_id": RES[r], "requires_approval": sens}), ok=(200, 201, 409))

allow("asst", "llm"); allow("asst", "gw"); allow("asst", "crm", sens=True)
allow("contr", "llm"); allow("rep", "llm"); allow("rep", "data")
step("limite", *call("POST", "/integrations/limits", {"agent_id": AG["asst"], "resource_id": RES["llm"], "max_calls_per_day": 500, "max_cost_per_month": 50, "currency": "EUR"}), ok=(200, 201, 409))
for _ in range(12):
    a, r = random.choice([("asst", "llm"), ("asst", "gw"), ("contr", "llm"), ("rep", "llm"), ("rep", "data"), ("asst", "crm")])
    call("POST", "/integrations/check", {"agent_id": AG[a], "resource_id": RES[r], "cost": round(random.uniform(0.01, 0.2), 2)})

# 4. approvazioni (una pendente, una approvata)
b = step("richiesta approvazione", *call("POST", "/integrations/approvals", {"agent_id": AG["asst"], "resource_id": RES["crm"], "justification": "Consultazione anagrafica cliente per l'assistenza"}))
b2 = step("richiesta approvazione 2", *call("POST", "/integrations/approvals", {"agent_id": AG["asst"], "resource_id": RES["crm"], "justification": "Aggiornamento recapiti"}))
if b2:
    step("approva", *call("POST", "/integrations/approvals/%d/approve" % b2["id"], {"note": "ok per la prova"}))

# 5. versioni, valutazioni, deploy e promozioni
for ver in ["1.0", "1.1"]:
    step("versione", *call("POST", "/lifecycle/agents/%d/versions" % AG["asst"], {"version_label": ver}))
step("valutazione", *call("POST", "/lifecycle/agents/%d/evaluations" % AG["asst"], {"version_label": "1.1", "result": "passed", "note": "test di regressione"}))
step("deploy sviluppo", *call("POST", "/lifecycle/agents/%d/deploy" % AG["asst"], {"environment": "development", "version_label": "1.1", "note": "prima distribuzione"}))
step("promozione staging", *call("POST", "/lifecycle/agents/%d/promote" % AG["asst"], {"to_environment": "staging", "note": "pronto per il test"}))
step("versione contr", *call("POST", "/lifecycle/agents/%d/versions" % AG["contr"], {"version_label": "2.0"}))
step("deploy contr", *call("POST", "/lifecycle/agents/%d/deploy" % AG["contr"], {"environment": "development", "version_label": "2.0"}))
step("versione rep", *call("POST", "/lifecycle/agents/%d/versions" % AG["rep"], {"version_label": "1.0"}))

# 6. monitoraggio e tracce di valutazione
for n in range(20):
    ag = random.choice(["asst", "contr", "rep"])
    ok = random.random() > 0.15
    tokens = random.randint(300, 900)
    step("chiamata", *call("POST", "/lifecycle/agents/%d/calls" % AG[ag], {"environment": "development", "success": ok, "latency_ms": random.randint(120, 1500), "tokens": tokens, "cost": round(tokens * 0.00001, 4)}))
for n in range(12):
    ag = random.choice(["asst", "rep"])
    outcome = random.choice(["success", "success", "failure", "partial"])
    step("traccia", *call("POST", "/evaluation/traces", {
        "agent_id": AG[ag], "trace_id": "demo-%s-%03d" % (ag, n), "outcome": outcome, "goal_met": outcome == "success",
        "cost": round(random.uniform(0.01, 0.08), 3), "environment": "development",
        "steps": [{"type": "llm", "name": "risposta", "tokens": random.randint(200, 900), "latency_ms": random.randint(150, 1200), "ok": outcome != "failure"},
                  {"type": "tool", "name": "ricerca", "tokens": 0, "latency_ms": random.randint(40, 300), "ok": True},
                  {"type": "memory", "name": "contesto", "tokens": 0, "latency_ms": 5, "ok": True, "memory_age_days": random.choice([0, 2, 9])}]}), ok=(201, 409))
step("regola token", *call("POST", "/evaluation/rules", {"type": "max_tokens_per_trace", "params": {"max": 700}, "enabled": True}), ok=(200, 201, 409))
step("regola latenza", *call("POST", "/evaluation/rules", {"type": "max_latency_ms", "params": {"max": 1000}, "enabled": True}), ok=(200, 201, 409))
step("giudizio", *call("POST", "/evaluation/agents/%d/scores" % AG["asst"], {"score": 4, "judge": "manual", "note": "risposte corrette"}))

# 7. guardrail e valutazione di testi
step("guardrail pii", *call("POST", "/guardrails", {"name": PFX + "Mascheramento dati personali", "type": "pii_mask", "params": {}, "scope": "agent", "agent_id": AG["asst"], "enabled": True}))
step("guardrail parole", *call("POST", "/guardrails", {"name": PFX + "Parole vietate", "type": "keyword_block", "params": {"terms": ["password", "carta di credito"]}, "scope": "global", "enabled": True}))
for text in ["Scrivi a mario.rossi@example.com", "Mi serve la password dell'account", "Ciao, come va?", "Il mio codice fiscale e' RSSMRA80A01H501U"]:
    call("POST", "/guardrails/evaluate", {"agent_id": AG["asst"], "direction": "input", "text": text})
    call("POST", "/guardrails/evaluate", {"agent_id": AG["rep"], "direction": "output", "text": text})

# 8. politiche e controlli di conformita'
step("politica trasparenza", *call("POST", "/compliance/policies", {"name": PFX + "Trasparenza dichiarata", "description": "Gli agenti che parlano con persone dichiarano la lingua", "rule_type": "transparency_required", "params": {}, "enabled": True}))
step("politica valutazione", *call("POST", "/compliance/policies", {"name": PFX + "Valutazione prima della produzione", "description": "", "rule_type": "evaluation_before_production", "params": {}, "enabled": True}))
st, fws = call("GET", "/compliance/frameworks")
controls = [c for f in (fws or []) for c in (f.get("controls") or [])][:4]
for i, c in enumerate(controls):
    status = ["compliant", "non_compliant", "not_assessed", "compliant"][i % 4]
    step("controllo", *call("PUT", "/compliance/agents/%d/assessments/%d" % (AG["asst"], c["id"]), {"status": status, "evidence_note": "verifica dimostrativa"}))
for key, cls in [("crm", "confidential"), ("data", "restricted"), ("llm", "internal")]:
    step("sicurezza dati", *call("PUT", "/compliance/resources/%d/security" % RES[key], {"classification": cls, "encrypted_at_rest": True, "encrypted_in_transit": True, "pii_allowed": key == "data", "retention_days": 365}))

# 9. marketplace: inserzione, richiesta, approvazione, provisioning
st, lst = call("POST", "/marketplace/listings", {"provider": "Fornitore esterno", "name": PFX + "Agente analisi contratti", "version": "2.0", "framework": "CrewAI",
    "price_model": "subscription", "price": 29, "currency": "EUR", "certified": True, "security_review": "approved", "status": "listed"})
lst = step("inserzione", st, lst)
if lst:
    step("inserzione 2", *call("POST", "/marketplace/listings", {"provider": "Terze parti srl", "name": PFX + "Chatbot FAQ", "version": "1.2", "framework": "LangGraph",
        "price_model": "free", "price": 0, "currency": "EUR", "certified": False, "security_review": "pending", "status": "hidden"}))
    b = step("richiesta marketplace", *call("POST", "/marketplace/requests", {"listing_id": lst["id"], "requested_by": "Quirino", "justification": "Analisi dei contratti del reparto legale"}))
    if b:
        step("approva richiesta", *call("POST", "/marketplace/requests/%d/approve" % b["id"], {"note": "revisione completata"}))
        step("provisiona", *call("POST", "/marketplace/requests/%d/provision" % b["id"], {}))

# 10. FinOps: esiti per periodo
for ag in ["asst", "contr", "rep"]:
    for outcome, value in [("success", 120.0), ("success", 80.0), ("failure", 0.0)]:
        step("esito finops", *call("POST", "/finops/outcomes", {"agent_id": AG[ag], "outcome": outcome, "value_eur": value, "period": "2026-11-01", "note": "dato dimostrativo"}))

print()
print("Agenti creati: " + str(len(AG)) + ", risorse: " + str(len(RES)))
print("Errori: " + str(len(errors)))
for e in errors[:10]:
    print("  - " + e[0] + ": " + str(e[1]))
