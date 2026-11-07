"""Flusso di esempio per un agente registrato (agente di esempio id 420).

Simula cio' che fa l'applicazione che ospita l'agente:
1. chiede il permesso prima di usare una risorsa (/integrations/check);
2. registra la chiamata con esito, latenza, token e costo (/lifecycle/agents/{id}/calls);
3. passa il testo in uscita dai guardrail (/guardrails/evaluate).

Crea una risorsa di prova e la cancella alla fine. Uso: python3 -I tests/demo_agent.py [base_url] [agent_id]
"""
import json
import sys
import urllib.error
import urllib.request

BASE = (sys.argv[1] if len(sys.argv) > 1 else "http://localhost:8080").rstrip("/") + "/api"
AGENT = int(sys.argv[2]) if len(sys.argv) > 2 else 420


def call(method, path, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(BASE + path, data=data, method=method,
                                 headers={"Content-Type": "application/json", "Accept": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=10) as r:
            txt = r.read().decode()
            return r.status, (json.loads(txt) if txt else None)
    except urllib.error.HTTPError as e:
        txt = e.read().decode()
        return e.code, (json.loads(txt) if txt else None)


results = []


def check(name, ok, detail=""):
    results.append(ok)
    print(("OK  " if ok else "FAIL") + "  " + name + (("  (" + str(detail) + ")") if detail else ""))


# 0. l'agente deve esistere e essere attivo
st, ag = call("GET", f"/agents/{AGENT}")
check("agente di esempio presente", st == 200, ag and ag.get("name"))
check("agente attivo", ag and ag.get("status") == "active", ag and ag.get("status"))

# 1. risorsa di prova (fornitore di modelli) e allowlist
st, res = call("POST", "/integrations/resources", {"name": "demo-api-modello", "kind": "llm_model", "endpoint": "https://example.invalid/v1", "status": "enabled"})
check("crea risorsa di prova", st == 201, st)
rid = res["id"] if st == 201 else None

st, al = call("POST", "/integrations/allowlist", {"agent_id": AGENT, "resource_id": rid, "requires_approval": False})
check("consenti la risorsa all'agente", st == 201, st)
allow_id = al["id"] if st == 201 else None

# 2. controllo prima dell'accesso: deve essere consentito
st, dec = call("POST", "/integrations/check", {"agent_id": AGENT, "resource_id": rid, "cost": 0.02})
check("accesso consentito alla risorsa autorizzata", st == 200 and dec.get("decision") == "allowed", dec and dec.get("reason"))

# 3. registra la chiamata con esito e metriche
st, _ = call("POST", f"/lifecycle/agents/{AGENT}/calls", {"environment": "production", "success": True, "latency_ms": 420, "tokens": 1350, "cost": 0.02})
check("registra la chiamata di monitoraggio", st == 201, st)

# 4. risorsa non autorizzata: deve essere negata e registrata nel registro accessi
st, res2 = call("POST", "/integrations/resources", {"name": "demo-risorsa-non-autorizzata", "kind": "api", "endpoint": "", "status": "enabled"})
rid2 = res2["id"] if st == 201 else None
st, dec2 = call("POST", "/integrations/check", {"agent_id": AGENT, "resource_id": rid2, "cost": 0.01})
check("accesso negato a risorsa non in allowlist", st == 200 and dec2.get("decision") == "denied" and dec2.get("reason") == "not_allowed", dec2 and dec2.get("reason"))

# 5. guardrail sul testo in uscita: i dati personali vanno mascherati
st, g = call("POST", "/guardrails/evaluate", {"agent_id": AGENT, "direction": "output", "text": "Scrivi a mario.rossi@example.com per i dettagli."})
check("guardrail valuta il testo in uscita", st == 200 and g.get("decision") in ("allow", "mask", "block"), g and g.get("decision"))

# 6. pulizia: rimuove i dati di prova (la risorsa cancella allowlist e limiti collegati)
if rid2:
    call("DELETE", f"/integrations/resources/{rid2}")
if rid:
    call("DELETE", f"/integrations/resources/{rid}")
st, _ = call("GET", f"/integrations/resources/{rid}") if rid else (404, None)
check("pulizia: risorsa di prova rimossa", st == 404, st)

print()
print(f"Risultato: {sum(results)}/{len(results)} controlli superati")
sys.exit(0 if all(results) else 1)
