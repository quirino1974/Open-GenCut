#!/usr/bin/env python3
"""Regressione dell'aggiornamento dei quadri da bundle (POST /api/compliance/frameworks/import).

Controlli: accesso senza sessione, aggiornamento di versione, fonte e data di un quadro
esistente (riconosciuto per codice), creazione e aggiornamento dei controlli, codice sconosciuto
senza modifiche, validazione del bundle. Il quadro GDPR e il controllo ART5 vengono ripristinati.
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


def find_framework(code):
    s, fws, _ = call3("GET", "/compliance/frameworks")
    for f in fws or []:
        if f.get("code") == code:
            return f
    return None


def main():
    tag = f"CIMP-{int(time.time())}"
    original = find_framework("GDPR")
    check("quadro GDPR presente", original is not None)
    if original is None:
        sys.exit(1)
    art5 = next((c for c in original["controls"] if c["code"] == "ART5"), None)
    created_ctrl = None

    section("Accesso")
    s, _, _ = raw_call(new_opener(), "POST", "/compliance/frameworks/import",
                       {"framework": {"code": "GDPR"}, "controls": []})
    check("senza sessione -> 401", s == 401, s)

    section("Validazione del bundle")
    s, b, _ = call3("POST", "/compliance/frameworks/import", {"controls": []})
    check("framework.code mancante -> 400", s == 400, (s, b))
    s, b, _ = call3("POST", "/compliance/frameworks/import",
                    {"framework": {"code": "GDPR"}, "controls": [{"code": "", "title": "x"}]})
    check("controllo senza code -> 400", s == 400, (s, b))
    s, b, _ = call3("POST", "/compliance/frameworks/import",
                    {"framework": {"code": "GDPR"}, "controls": [{"code": "X1", "title": ""}]})
    check("controllo senza title -> 400", s == 400, (s, b))
    s, b, _ = call3("POST", "/compliance/frameworks/import", {"framework": {"code": "GDPR"}, "extra": 1})
    check("campo sconosciuto -> 400", s == 400, (s, b))

    section("Codice sconosciuto: nessuna modifica")
    s, b, _ = call3("POST", "/compliance/frameworks/import",
                    {"framework": {"code": f"NON-ESISTE-{tag}", "name": "x", "version": "1"}, "controls": []})
    check("quadro non presente -> 404", s == 404, (s, b))
    check("nessun quadro creato", find_framework(f"NON-ESISTE-{tag}") is None)

    section("Aggiornamento di GDPR con versione, fonte e data")
    version = f"TEST-{tag}"
    source = f"https://example.test/{tag}"
    bundle = {
        "framework": {"code": "GDPR", "version": version, "source_url": source},
        "controls": [
            {"code": "ART5", "title": f"Principi (aggiornato {tag})", "description": "Testo di prova."},
            {"code": f"CTRL-{tag}", "title": "Controllo di prova", "description": "Solo per il test."},
        ],
    }
    s, b, raw = call3("POST", "/compliance/frameworks/import", bundle)
    check("risposta 200", s == 200, (s, b))
    check("controllo nuovo creato: 1", b and b.get("controls_created") == 1, b)
    check("controllo esistente aggiornato: 1", b and b.get("controls_updated") == 1, b)
    fw = (b or {}).get("framework", {})
    check("versione aggiornata", fw.get("version") == version, fw.get("version"))
    check("fonte aggiornata", fw.get("source_url") == source, fw.get("source_url"))
    check("data di aggiornamento registrata", bool(fw.get("updated_at")), fw.get("updated_at"))
    check("il nome non cambia se non fornito", fw.get("name") == original["name"], fw.get("name"))

    now = find_framework("GDPR")
    ctrl = next((c for c in now["controls"] if c["code"] == f"CTRL-{tag}"), None)
    created_ctrl = ctrl["id"] if ctrl else None
    check("il controllo nuovo è nel quadro", ctrl is not None)
    art5_now = next((c for c in now["controls"] if c["code"] == "ART5"), None)
    check("ART5 aggiornato", art5_now and art5_now["title"] == f"Principi (aggiornato {tag})", art5_now)

    section("Reimport: nessun controllo nuovo")
    s, b, _ = call3("POST", "/compliance/frameworks/import", bundle)
    check("reimport: nessun controllo creato", s == 200 and b.get("controls_created") == 0, (s, b))
    check("reimport: i controlli sono aggiornati", s == 200 and b.get("controls_updated") == 2, (s, b))

    section("Ripristino")
    restore = {"framework": {"code": "GDPR", "version": original.get("version") or "",
                             "source_url": original.get("source_url") or ""},
               "controls": [{"code": "ART5", "title": art5["title"], "description": art5.get("description") or ""}]
               if art5 else []}
    if not restore["framework"]["version"]:
        restore["framework"].pop("version")
    if not restore["framework"]["source_url"]:
        restore["framework"].pop("source_url")
    call3("POST", "/compliance/frameworks/import", restore)
    if created_ctrl:
        call3("DELETE", f"/compliance/controls/{created_ctrl}")
    back = find_framework("GDPR")
    check("ripristino: ART5 come prima", art5 and any(
        c["code"] == "ART5" and c["title"] == art5["title"] for c in back["controls"]))
    check("ripristino: controllo di prova rimosso", not any(c["code"] == f"CTRL-{tag}" for c in back["controls"]))

    total = passed + len(failed)
    print(f"\nRisultato: {passed}/{total} controlli superati")
    if failed:
        print("Falliti:")
        for f in failed:
            print(f"  - {f}")
        sys.exit(1)


if __name__ == "__main__":
    main()
