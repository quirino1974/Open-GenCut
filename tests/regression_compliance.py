#!/usr/bin/env python3
"""Regressione del punto 4 della specifica: conformità.

Stessi criteri di regression_backend.py: solo HTTP, nessuna libreria esterna,
dati di test creati e rimossi dalla suite, esito 0 se tutti i controlli passano.

Copre: gateway AI come risorse, misure di sicurezza dei dati, politiche con le quattro
regole, verifica per agente, librerie di framework e controlli (con seed), valutazione dei
controlli per agente, vista d'insieme.

Uso:  python3 tests/regression_compliance.py [base_url]
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


def find(rows, **kw):
    for r in rows:
        if all(r.get(k) == v for k, v in kw.items()):
            return r
    return None


def precleanup():
    """Rimuove i residui di esecuzioni interrotte (prefissi cmp- e CMP_)."""
    s, pols = call("GET", "/compliance/policies")
    for p in pols or []:
        if p["name"].startswith("cmp-"):
            call("DELETE", f"/compliance/policies/{p['id']}")
    s, fws = call("GET", "/compliance/frameworks")
    for f in fws or []:
        if f["code"].startswith("CMP_"):
            call("DELETE", f"/compliance/frameworks/{f['id']}")
    s, res = call("GET", "/integrations/resources")
    for r in res or []:
        if r["name"].startswith("cmp-"):
            call("DELETE", f"/integrations/resources/{r['id']}")
    s, ags = call("GET", "/agents")
    for a in ags or []:
        if a["name"].startswith("cmp-"):
            call("DELETE", f"/agents/{a['id']}")


def main():
    precleanup()
    agents = []
    resources = []
    policies = []
    frameworks = []

    # ------------------------------------------------------------------
    section("Gateway AI come risorse")
    s, b = call("POST", "/integrations/resources", {"name": "cmp-gw", "kind": "ai_gateway", "status": "enabled"})
    check("gateway AI registrato come risorsa ai_gateway 201", s == 201 and b.get("kind") == "ai_gateway", (s, b))
    gw = b["id"] if s == 201 else None
    if gw:
        resources.append(gw)
    s, b = call("POST", "/integrations/resources", {"name": "cmp-bad", "kind": "gateway_x"})
    check("tipo di risorsa non previsto -> 400", s == 400, (s, b))

    # ------------------------------------------------------------------
    section("Misure di sicurezza dei dati")
    s, b = call("GET", f"/compliance/resources/{gw}/security")
    check("senza misure: valori di default", s == 200 and b.get("classification") == "internal"
          and b.get("pii_allowed") is False and b.get("retention_days") is None, (s, b))
    s, b = call("PUT", f"/compliance/resources/{gw}/security", {"classification": "segreto"})
    check("classificazione non valida -> 400", s == 400, (s, b))
    s, b = call("PUT", f"/compliance/resources/{gw}/security", {"classification": "internal", "retention_days": -1})
    check("conservazione negativa -> 400", s == 400, (s, b))
    s, b = call("PUT", f"/compliance/resources/{gw}/security",
                {"classification": "confidential", "encrypted_at_rest": True, "encrypted_in_transit": True,
                 "pii_allowed": True, "retention_days": 90})
    check("misure salvate 200", s == 200 and b.get("classification") == "confidential"
          and b.get("retention_days") == 90, (s, b))
    s, b = call("GET", f"/compliance/resources/{gw}/security")
    check("misure rilette", s == 200 and b.get("pii_allowed") is True and b.get("encrypted_at_rest") is True, (s, b))
    s, b = call("GET", "/compliance/resources/999999/security")
    check("sicurezza di risorsa inesistente -> 404", s == 404, (s, b))

    # ------------------------------------------------------------------
    section("Politiche: validazione e CRUD")
    s, b = call("POST", "/compliance/policies", {"name": "cmp-p-bad", "rule_type": "magia"})
    check("tipo di regola non valido -> 400", s == 400, (s, b))
    s, b = call("POST", "/compliance/policies", {"name": "cmp-p-bad2", "rule_type": "transparency_required",
                                                  "params": [1, 2]})
    check("params non oggetto -> 400", s == 400, (s, b))
    s, b = call("POST", "/compliance/policies", {"name": "cmp-p-bad3", "rule_type": "max_classification",
                                                  "params": {"max": "segreto"}})
    check("max_classification con max non valido -> 400", s == 400, (s, b))

    s, p_tr = call("POST", "/compliance/policies", {"name": "cmp-trasparenza", "rule_type": "transparency_required",
                                                     "description": "test"})
    check("politica transparency_required 201", s == 201 and p_tr.get("enabled") is True, (s, p_tr))
    if s == 201:
        policies.append(p_tr["id"])
    s, p_max = call("POST", "/compliance/policies", {"name": "cmp-max", "rule_type": "max_classification",
                                                      "params": {"max": "internal"}})
    check("politica max_classification 201", s == 201, (s, p_max))
    if s == 201:
        policies.append(p_max["id"])
    s, p_pii = call("POST", "/compliance/policies", {"name": "cmp-pii", "rule_type": "pii_requires_approval"})
    check("politica pii_requires_approval 201", s == 201, (s, p_pii))
    if s == 201:
        policies.append(p_pii["id"])
    s, p_eval = call("POST", "/compliance/policies", {"name": "cmp-eval", "rule_type": "evaluation_before_production"})
    check("politica evaluation_before_production 201", s == 201, (s, p_eval))
    if s == 201:
        policies.append(p_eval["id"])

    s, b = call("POST", "/compliance/policies", {"name": "cmp-trasparenza", "rule_type": "transparency_required"})
    check("nome di politica duplicato -> 409", s == 409, (s, b))
    s, b = call("GET", f"/compliance/policies/{p_tr['id']}")
    check("dettaglio politica 200", s == 200 and b.get("rule_type") == "transparency_required", (s, b))
    s, b = call("PUT", f"/compliance/policies/{p_tr['id']}",
                {"name": "cmp-trasparenza", "rule_type": "transparency_required", "enabled": False})
    check("modifica politica 200 e disattivazione", s == 200 and b.get("enabled") is False, (s, b))
    s, b = call("PUT", f"/compliance/policies/{p_tr['id']}",
                {"name": "cmp-trasparenza", "rule_type": "transparency_required", "enabled": True})
    s, lst = call("GET", "/compliance/policies")
    check("elenco politiche contiene le nuove", s == 200 and find(lst, id=p_tr["id"]) is not None, (s, lst))
    s, b = call("GET", "/compliance/policies/999999")
    check("politica inesistente -> 404", s == 404, (s, b))

    # ------------------------------------------------------------------
    section("Verifica delle politiche per agente")
    s, b = call("POST", "/agents", {"name": "cmp-agent", "origin": "internal", "status": "active",
                                     "interacts_with_people": False})
    ag = b["id"]
    agents.append(ag)
    s, res = call("GET", f"/compliance/agents/{ag}/policies")
    tr = find(res, policy_id=p_tr["id"])
    check("non interagisce con persone -> transparency superata", s == 200 and tr and tr["pass"] is True, (s, res))
    # Il requisito R4 impedisce di salvare un agente che interagisce senza lingua (400):
    # la regola resta come controllo difensivo e per questo non può fallire via API.
    s, b = call("PUT", f"/agents/{ag}", {"name": "cmp-agent", "origin": "internal", "status": "active",
                                          "interacts_with_people": True})
    check("interagisce senza lingua -> rifiutato da R4 (400)", s == 400, (s, b))
    s, b = call("PUT", f"/agents/{ag}", {"name": "cmp-agent", "origin": "internal", "status": "active",
                                          "interacts_with_people": True, "interaction_language": "it"})
    s, res = call("GET", f"/compliance/agents/{ag}/policies")
    tr = find(res, policy_id=p_tr["id"])
    check("lingua dichiarata -> transparency superata", tr and tr["pass"] is True, res)

    s, b = call("POST", "/integrations/resources", {"name": "cmp-data", "kind": "data_source", "status": "enabled"})
    rd = b["id"]
    resources.append(rd)
    call("PUT", f"/compliance/resources/{rd}/security", {"classification": "restricted", "pii_allowed": True})
    call("POST", "/integrations/allowlist", {"agent_id": ag, "resource_id": rd, "requires_approval": False})
    s, res = call("GET", f"/compliance/agents/{ag}/policies")
    mx = find(res, policy_id=p_max["id"])
    check("risorsa restricted oltre max internal -> non superata", mx and mx["pass"] is False
          and "cmp-data" in mx["reason"], res)
    pii = find(res, policy_id=p_pii["id"])
    check("PII senza approvazione obbligatoria -> non superata", pii and pii["pass"] is False, res)

    s, al = call("GET", f"/integrations/allowlist?agent_id={ag}")
    entry = al[0]
    call("DELETE", f"/integrations/allowlist/{entry['id']}")
    call("POST", "/integrations/allowlist", {"agent_id": ag, "resource_id": rd, "requires_approval": True})
    s, res = call("GET", f"/compliance/agents/{ag}/policies")
    pii = find(res, policy_id=p_pii["id"])
    check("PII con approvazione obbligatoria -> superata", pii and pii["pass"] is True, res)

    call("PUT", f"/compliance/resources/{rd}/security", {"classification": "public", "pii_allowed": False})
    s, res = call("GET", f"/compliance/agents/{ag}/policies")
    mx = find(res, policy_id=p_max["id"])
    check("risorsa public entro max internal -> superata", mx and mx["pass"] is True, res)

    s, b = call("POST", f"/lifecycle/agents/{ag}/versions", {"version_label": "1.0"})
    call("POST", f"/lifecycle/agents/{ag}/deploy", {"environment": "development", "version_label": "1.0"})
    call("POST", f"/lifecycle/agents/{ag}/evaluations", {"version_label": "1.0", "result": "passed"})
    call("POST", f"/lifecycle/agents/{ag}/promote", {"to_environment": "staging"})
    call("POST", f"/lifecycle/agents/{ag}/promote", {"to_environment": "production"})
    s, res = call("GET", f"/compliance/agents/{ag}/policies")
    ev = find(res, policy_id=p_eval["id"])
    check("produzione con valutazione superata -> superata", ev and ev["pass"] is True, res)

    call("POST", f"/lifecycle/agents/{ag}/versions", {"version_label": "1.1"})
    call("POST", f"/lifecycle/agents/{ag}/deploy", {"environment": "development", "version_label": "1.1"})
    call("POST", f"/lifecycle/agents/{ag}/evaluations", {"version_label": "1.1", "result": "failed"})
    s, res = call("GET", f"/compliance/agents/{ag}/policies")
    ev = find(res, policy_id=p_eval["id"])
    check("ultima valutazione fallita -> non superata", ev and ev["pass"] is False, res)

    s, b = call("GET", "/compliance/agents/999999/policies")
    check("politiche di agente inesistente -> 404", s == 404, (s, b))

    # ------------------------------------------------------------------
    section("Framework e controlli (seed)")
    s, fws = call("GET", "/compliance/frameworks")
    check("seed presente: quattro framework", s == 200 and len(fws) >= 4, (s, fws))
    gdpr = find(fws, code="GDPR")
    check("seed marcato da validare", gdpr and "validare" in (gdpr.get("validation_note") or ""), gdpr)
    check("seed con controlli", gdpr and len(gdpr["controls"]) >= 3, gdpr)

    s, b = call("POST", "/compliance/frameworks", {"code": "CMP_TEST", "name": "Test framework", "version": "1"})
    check("framework creato 201", s == 201 and b.get("code") == "CMP_TEST", (s, b))
    fw = b["id"] if s == 201 else None
    if fw:
        frameworks.append(fw)
    s, b = call("POST", "/compliance/frameworks", {"code": "CMP_TEST", "name": "Doppione"})
    check("codice framework duplicato -> 409", s == 409, (s, b))
    s, b = call("POST", "/compliance/frameworks", {"code": "", "name": "x"})
    check("framework senza codice -> 400", s == 400, (s, b))
    s, b = call("PUT", f"/compliance/frameworks/{fw}", {"code": "CMP_TEST", "name": "Test aggiornato", "version": "2"})
    check("framework aggiornato 200", s == 200 and b.get("version") == "2", (s, b))

    s, c1 = call("POST", "/compliance/controls", {"framework_id": fw, "code": "C1", "title": "Controllo uno"})
    check("controllo creato 201", s == 201 and c1.get("framework_id") == fw, (s, c1))
    s, b = call("POST", "/compliance/controls", {"framework_id": fw, "code": "C1", "title": "Duplicato"})
    check("controllo duplicato nel framework -> 409", s == 409, (s, b))
    s, b = call("POST", "/compliance/controls", {"framework_id": 999999, "code": "C2", "title": "x"})
    check("controllo con framework inesistente -> 400", s == 400, (s, b))
    s, b = call("PUT", f"/compliance/controls/{c1['id']}", {"framework_id": fw, "code": "C1", "title": "Titolo nuovo"})
    check("controllo aggiornato 200", s == 200 and b.get("title") == "Titolo nuovo", (s, b))
    s, b = call("DELETE", f"/compliance/controls/{c1['id']}")
    check("controllo cancellato 204", s == 204, (s, b))
    s, b = call("DELETE", f"/compliance/controls/{c1['id']}")
    check("controllo già cancellato -> 404", s == 404, (s, b))

    # ------------------------------------------------------------------
    section("Valutazione dei controlli per agente")
    s, lst = call("GET", f"/compliance/agents/{ag}/assessments")
    check("valutazioni: un elemento per controllo, default not_assessed",
          s == 200 and len(lst) >= 10 and all(a["status"] == "not_assessed" for a in lst), (s, lst[:2] if lst else lst))
    ctrl = lst[0]["control_id"]
    s, b = call("PUT", f"/compliance/agents/{ag}/assessments/{ctrl}", {"status": "confermato"})
    check("stato non valido -> 400", s == 400, (s, b))
    s, b = call("PUT", f"/compliance/agents/{ag}/assessments/999999", {"status": "compliant"})
    check("controllo inesistente -> 404", s == 404, (s, b))
    s, b = call("PUT", f"/compliance/agents/999999/assessments/{ctrl}", {"status": "compliant"})
    check("agente inesistente -> 404", s == 404, (s, b))
    s, b = call("PUT", f"/compliance/agents/{ag}/assessments/{ctrl}",
                {"status": "compliant", "evidence_note": "verifica di prova"})
    check("valutazione salvata 200", s == 200 and b.get("status") == "compliant", (s, b))
    s, lst = call("GET", f"/compliance/agents/{ag}/assessments")
    row = find(lst, control_id=ctrl)
    check("valutazione riletta nella lista", row and row["status"] == "compliant"
          and row["evidence_note"] == "verifica di prova", row)

    # ------------------------------------------------------------------
    section("Vista d'insieme")
    s, ov = call("GET", "/compliance/overview")
    check("overview con framework e campi", s == 200 and isinstance(ov.get("frameworks"), list)
          and isinstance(ov.get("policies_failing"), int), (s, ov))
    ok_pct = all(0 <= f["compliant_percent"] <= 100 for f in ov["frameworks"]) if s == 200 else False
    check("percentuali di conformità tra 0 e 100", ok_pct, ov)
    s, b = call("POST", "/compliance/frameworks", {"code": "CMP_OVR", "name": "Overview test"})
    frameworks.append(b["id"])
    call("POST", "/compliance/controls", {"framework_id": b["id"], "code": "O1", "title": "Uno"})
    s, ov = call("GET", "/compliance/overview")
    fo = find(ov["frameworks"], code="CMP_OVR")
    check("framework senza valutazioni: nessun agente conforme",
          fo and fo["agents_compliant"] == 0 and fo["controls_total"] == 1, fo)
    s, lst = call("GET", f"/compliance/agents/{ag}/assessments")
    ovr_ctrl = find(lst, framework_code="CMP_OVR")["control_id"]
    call("PUT", f"/compliance/agents/{ag}/assessments/{ovr_ctrl}", {"status": "not_applicable"})
    s, ov = call("GET", "/compliance/overview")
    fo = find(ov["frameworks"], code="CMP_OVR")
    check("controllo non applicabile conta come conforme", fo and fo["agents_compliant"] == 1, fo)

    # ------------------------------------------------------------------
    section("Audit con chiave tradotta")
    s, aud = call("GET", "/audit?limit=50")
    keys = []
    for e in aud:
        try:
            keys.append(json.loads(e["detail"]).get("key"))
        except (TypeError, json.JSONDecodeError, AttributeError):
            pass
    check("audit: chiavi di conformità presenti",
          {"policy_saved", "framework_saved", "assessment_saved", "resource_security_saved"} <= set(keys), keys)

    # ------------------------------------------------------------------
    section("Pulizia")
    for pid in policies:
        call("DELETE", f"/compliance/policies/{pid}")
    for fid in frameworks:
        call("DELETE", f"/compliance/frameworks/{fid}")
    for rid in resources:
        call("DELETE", f"/integrations/resources/{rid}")
    for aid in agents:
        call("DELETE", f"/agents/{aid}")
    s, b = call("GET", "/compliance/frameworks")
    check("seed conservato dopo la pulizia", s == 200 and find(b, code="GDPR") is not None, (s, b))

    total = passed + len(failed)
    print(f"\nRisultato: {passed}/{total} controlli superati")
    if failed:
        print("Falliti:")
        for f in failed:
            print(f"  - {f}")
        sys.exit(1)


if __name__ == "__main__":
    main()
