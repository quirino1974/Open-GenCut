"""Client Python per Open-GenCut: controllo accessi, monitoraggio e guardrail dalle applicazioni che ospitano gli agenti.

Usa solo la libreria standard. Esempio:

    from opengencut import Client, Blocked
    c = Client("http://localhost:8080")
    c.login("utente-integrazione", "password")
    c.guard_input(agent_id=420, text="messaggio dell'utente")     # solleva Blocked se vietato
    decision = c.check(agent_id=420, resource_id=12, cost=0.02)  # {'decision': 'allowed', ...}
    if decision["decision"] == "allowed":
        ... chiamata al modello ...
        c.record_call(agent_id=420, success=True, latency_ms=410, tokens=1200, cost=0.02)

La sessione (cookie) e' tenuta in memoria dal client; nessuna credenziale viene salvata su disco.
"""
import http.cookiejar
import json
import urllib.error
import urllib.request


class OpenGenCutError(Exception):
    """Errore restituito dalla piattaforma."""

    def __init__(self, status, error, message):
        super().__init__(f"{status} {error}: {message}")
        self.status = status
        self.error = error
        self.message = message


class Blocked(Exception):
    """Il testo e' stato bloccato dai guardrail."""

    def __init__(self, violations):
        super().__init__("messaggio bloccato dai guardrail")
        self.violations = violations


class Client:
    def __init__(self, base_url, timeout=30):
        self.base = base_url.rstrip("/") + "/api"
        self.timeout = timeout
        self._jar = http.cookiejar.CookieJar()
        self._opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self._jar))

    def _request(self, method, path, body=None):
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(self.base + path, data=data, method=method,
                                     headers={"Content-Type": "application/json", "Accept": "application/json"})
        try:
            with self._opener.open(req, timeout=self.timeout) as r:
                txt = r.read().decode()
                return json.loads(txt) if txt else None
        except urllib.error.HTTPError as e:
            txt = e.read().decode()
            try:
                d = json.loads(txt) if txt else {}
            except ValueError:
                d = {}
            raise OpenGenCutError(e.code, d.get("error", "http_error"), d.get("message", e.reason))

    def login(self, username, password):
        """Apre la sessione. Con password da cambiare solleva OpenGenCutError (403 password_change_required)."""
        return self._request("POST", "/auth/login", {"username": username, "password": password})

    def logout(self):
        self._request("POST", "/auth/logout")

    def check(self, agent_id, resource_id, cost=0.0, tool=None):
        """Chiede il permesso prima di usare una risorsa. Non chiama il fornitore."""
        body = {"agent_id": agent_id, "resource_id": resource_id, "cost": cost}
        if tool:
            body["tool"] = tool
        return self._request("POST", "/integrations/check", body)

    def record_call(self, agent_id, success=True, latency_ms=0, tokens=0, cost=0.0, environment="production"):
        """Registra una chiamata di monitoraggio (esito, latenza, token, costo)."""
        return self._request("POST", f"/lifecycle/agents/{agent_id}/calls", {
            "environment": environment, "success": success, "latency_ms": latency_ms, "tokens": tokens, "cost": cost})

    def guard(self, agent_id, text, direction="input", resource_id=None):
        """Valuta un testo con i guardrail. Restituisce {decision, text, violations} senza sollevare eccezioni."""
        body = {"agent_id": agent_id, "direction": direction, "text": text}
        if resource_id is not None:
            body["resource_id"] = resource_id
        return self._request("POST", "/guardrails/evaluate", body)

    def guard_input(self, agent_id, text, resource_id=None):
        """Restituisce il testo da inviare (mascherato se serve) oppure solleva Blocked."""
        r = self.guard(agent_id, text, "input", resource_id)
        if r["decision"] == "block":
            raise Blocked(r.get("violations", []))
        return r["text"]

    def guard_output(self, agent_id, text, resource_id=None):
        """Restituisce il testo da mostrare (mascherato se serve) oppure solleva Blocked."""
        r = self.guard(agent_id, text, "output", resource_id)
        if r["decision"] == "block":
            raise Blocked(r.get("violations", []))
        return r["text"]
