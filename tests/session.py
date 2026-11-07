"""Sessione per le suite di regressione (docs/SICUREZZA.txt).

Le suite non hanno credenziali nel codice. Leggono l'utente di test dalle variabili d'ambiente:
  GENCUT_TEST_USER      nome utente di test (predefinito: qa-tester)
  GENCUT_TEST_PASSWORD  password dell'utente di test (obbligatoria)
L'utente di test si crea una volta con tests/setup_test_user.py.

Il login avviene alla prima chiamata; il cookie di sessione è tenuto da un CookieJar.
"""
import http.cookiejar
import json
import os
import sys
import urllib.error
import urllib.request

BASE = (os.environ.get("GENCUT_BASE_URL") or "http://localhost:8080").rstrip("/")
API = BASE + "/api"
TEST_USER = os.environ.get("GENCUT_TEST_USER", "qa-tester")

_state = {"opener": None}


def _password():
    pwd = os.environ.get("GENCUT_TEST_PASSWORD")
    if not pwd:
        sys.exit("Manca GENCUT_TEST_PASSWORD: vedi tests/setup_test_user.py (sezione Credenziali di test).")
    return pwd


def new_opener():
    """Apre un client con un proprio cookie jar (usato anche per i test di sessione)."""
    jar = http.cookiejar.CookieJar()
    return urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))


def raw_call(opener, method, path, body=None):
    """Chiamata grezza: restituisce (status, payload decodificato, testo grezzo)."""
    data = None
    headers = {}
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(API + path, data=data, method=method, headers=headers)
    try:
        with opener.open(req, timeout=90) as resp:
            raw = resp.read()
            status = resp.status
    except urllib.error.HTTPError as e:
        raw = e.read()
        status = e.code
    text = raw.decode("utf-8", "replace")
    try:
        payload = json.loads(raw) if raw else None
    except json.JSONDecodeError:
        payload = text
    return status, payload, text


def login(username, password):
    """Login: restituisce (opener, risposta). Non cambia la password."""
    opener = new_opener()
    status, payload, _ = raw_call(opener, "POST", "/auth/login", {"username": username, "password": password})
    return opener, status, payload


def _session():
    if _state["opener"] is None:
        opener, status, payload = login(TEST_USER, _password())
        if status != 200:
            sys.exit(f"Login dell'utente di test fallito ({status}). Esegui tests/setup_test_user.py.")
        if payload.get("must_change_password"):
            sys.exit("L'utente di test deve cambiare la password: esegui tests/setup_test_user.py.")
        _state["opener"] = opener
    return _state["opener"]


def call(method, path, body=None):
    """Come le suite storiche: restituisce (status, payload)."""
    status, payload, _ = raw_call(_session(), method, path, body)
    return status, payload


def call3(method, path, body=None):
    """Restituisce (status, payload, testo grezzo), per i controlli sul contenuto delle risposte."""
    return raw_call(_session(), method, path, body)


def send_raw(method, path, data=None, headers=None):
    """Invio con corpo grezzo (bytes) e intestazioni proprie, sulla sessione di test. Restituisce (status, payload)."""
    req = urllib.request.Request(API + path, data=data, method=method, headers=headers or {})
    try:
        with _session().open(req, timeout=90) as resp:
            body = resp.read()
            status = resp.status
    except urllib.error.HTTPError as e:
        body = e.read()
        status = e.code
    try:
        payload = json.loads(body) if body else None
    except json.JSONDecodeError:
        payload = body.decode("utf-8", "replace")
    return status, payload
