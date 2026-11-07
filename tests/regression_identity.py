#!/usr/bin/env python3
"""Regressione dell'identità per agente con revoca.

Stessi criteri di regression_backend.py: solo HTTP e libreria standard, dati di test
creati e rimossi dalla suite, esito 0 se tutti i controlli passano.

Copre: UUID alla creazione e univocità, lettura dell'identità, revoca definitiva (409 alla
seconda), negazione in /check con agent_revoked, ordine dei controlli (risorsa disattivata
prima), indipendenza da sospensione, audit.

Uso:  python3 tests/regression_identity.py [base_url]
"""
import json
import os
import re
import sys
import urllib.error
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from session import call  # noqa: E402

BASE = (sys.argv[1] if len(sys.argv) > 1 else "http://localhost:8080").rstrip("/")
API = BASE + "/api"
UUID_RE = re.compile(r"^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$")

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
    agents, resources = [], []

    section("Identità alla creazione")
    s, a = call("POST", "/agents", {"name": "id-a", "origin": "internal", "status": "active"})
    s2, b = call("POST", "/agents", {"name": "id-b", "origin": "internal", "status": "active"})
    agents += [a["id"], b["id"]]
    check("UUID valido alla creazione", s == 201 and UUID_RE.match(a.get("identity_id", "")) is not None, a)
    check("identità distinte per agenti diversi", a.get("identity_id") != b.get("identity_id"), (a, b))
    s, idn = call("GET", f"/identity/agents/{a['id']}")
    check("lettura identità: non revocata", s == 200 and idn.get("revoked") is False
          and idn.get("identity_id") == a["identity_id"], (s, idn))
    s, b2 = call("GET", "/identity/agents/999999")
    check("identità di agente inesistente -> 404", s == 404, (s, b2))

    section("Accesso prima della revoca")
    s, r = call("POST", "/integrations/resources", {"name": "id-res", "kind": "api", "status": "enabled"})
    resources.append(r["id"])
    call("POST", "/integrations/allowlist", {"agent_id": a["id"], "resource_id": r["id"]})
    s, c = call("POST", "/integrations/check", {"agent_id": a["id"], "resource_id": r["id"]})
    check("prima della revoca: allowed", c.get("decision") == "allowed", c)

    section("Revoca")
    s, rv = call("POST", f"/identity/agents/{a['id']}/revoke", {})
    check("revoca 200 con identità revocata", s == 200 and rv.get("revoked") is True
          and rv.get("revoked_at") is not None, (s, rv))
    s, b3 = call("POST", f"/identity/agents/{a['id']}/revoke", {})
    check("seconda revoca -> 409 invalid_state", s == 409 and b3.get("error") == "invalid_state", (s, b3))
    s, idn = call("GET", f"/identity/agents/{a['id']}")
    check("lettura dopo la revoca: revocata con data", idn.get("revoked") is True
          and idn.get("revoked_at") is not None, idn)
    s, ag = call("GET", f"/agents/{a['id']}")
    check("l'agente resta nel registro, con l'identità revocata", s == 200
          and ag.get("identity_revoked_at") is not None and ag.get("status") == "active", ag)

    section("Accesso dopo la revoca")
    s, c = call("POST", "/integrations/check", {"agent_id": a["id"], "resource_id": r["id"]})
    check("dopo la revoca: denied agent_revoked", c.get("decision") == "denied"
          and c.get("reason") == "agent_revoked", c)
    call("PUT", f"/integrations/resources/{r['id']}", {"name": "id-res", "kind": "api", "status": "disabled"})
    s, c = call("POST", "/integrations/check", {"agent_id": a["id"], "resource_id": r["id"]})
    check("ordine: risorsa disattivata prima di agent_revoked", c.get("reason") == "resource_disabled", c)
    call("PUT", f"/integrations/resources/{r['id']}", {"name": "id-res", "kind": "api", "status": "enabled"})

    section("Indipendenza da sospensione e altri agenti")
    s, c = call("POST", "/integrations/allowlist", {"agent_id": b["id"], "resource_id": r["id"]})
    s, c = call("POST", "/integrations/check", {"agent_id": b["id"], "resource_id": r["id"]})
    check("l'altro agente non è colpito dalla revoca", c.get("decision") == "allowed", c)
    s, sus = call("POST", f"/lifecycle/agents/{b['id']}/suspend", {})
    s, c = call("POST", "/integrations/check", {"agent_id": b["id"], "resource_id": r["id"]})
    check("sospensione: agent_not_active, la revoca resta separata", c.get("reason") == "agent_not_active", c)

    section("Audit")
    s, aud = call("GET", "/audit?limit=100")
    keys = []
    for e in aud:
        try:
            keys.append(json.loads(e["detail"]).get("key"))
        except (TypeError, json.JSONDecodeError, AttributeError):
            pass
    check("audit: identity_revoked presente", "identity_revoked" in keys, keys[:8])

    section("Pulizia")
    for rid in resources:
        call("DELETE", f"/integrations/resources/{rid}")
    for aid in agents:
        call("DELETE", f"/agents/{aid}")

    total = passed + len(failed)
    print(f"\nRisultato: {passed}/{total} controlli superati")
    if failed:
        print("Falliti:")
        for f in failed:
            print(f"  - {f}")
        sys.exit(1)


if __name__ == "__main__":
    main()
