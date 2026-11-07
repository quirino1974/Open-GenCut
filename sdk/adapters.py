"""Adattatori per i framework di agenti: applicano la governance di Open-GenCut senza dipendere dal framework.

Ogni adattatore avvolge una funzione che esegue un passo dell'agente (chiamata a modello, strumento o
nodo) e fa tre cose: controllo di accesso prima, guardrail sull'ingresso e sull'uscita, registrazione della
chiamata dopo. Non importa nessun framework: il punto di aggancio è indicato per ciascuno.

    from opengencut import Client
    from adapters import governed

    gov = governed(Client(url), agent_id=687, resource_id=240, framework="LangGraph")

    @gov.step(tool="echo")                    # oppure gov.call(...)
    def cerca(testo): ...

Punti di aggancio nei framework (da collegare nel codice dell'agente):
  - LangGraph:        wrapper del nodo (add_node) o del tool
  - CrewAI:           wrapper della funzione del tool (@tool) o del metodo execute
  - AutoGen:          hook di reply (register_reply) o wrapper della funzione registrata
  - Google ADK:       before_tool_callback / after_tool_callback
  - AWS AgentCore:    wrapper dell'entrypoint o del tool invocato
  - Microsoft Foundry: wrapper della chiamata di funzione o dello strumento

Limite: questi punti di aggancio sono verificati come forma d'uso, non con i pacchetti reali dei framework.
"""
import time

from opengencut import Blocked, OpenGenCutError


class Governed:
    def __init__(self, client, agent_id, resource_id, framework="custom", environment="production", cost_per_call=0.0):
        self.c = client
        self.agent_id = agent_id
        self.resource_id = resource_id
        self.framework = framework
        self.environment = environment
        self.cost = cost_per_call

    def call(self, fn, *args, tool=None, text_arg=None, **kwargs):
        """Esegue fn con governance. text_arg: nome dell'argomento testuale da controllare sui guardrail."""
        decision = self.c.check(self.agent_id, self.resource_id, cost=self.cost, tool=tool)
        if decision.get("decision") != "allowed":
            raise PermissionError(f"accesso negato: {decision.get('reason')}")
        if text_arg is not None and text_arg in kwargs:
            kwargs[text_arg] = self.c.guard_input(self.agent_id, kwargs[text_arg], self.resource_id)
        t0 = time.monotonic()
        ok = True
        try:
            out = fn(*args, **kwargs)
        except Exception:
            ok = False
            raise
        finally:
            ms = int((time.monotonic() - t0) * 1000)
            self.c.record_call(self.agent_id, success=ok, latency_ms=ms, cost=self.cost, environment=self.environment)
        if isinstance(out, str):
            out = self.c.guard_output(self.agent_id, out, self.resource_id)
        return out

    def step(self, tool=None, text_arg=None):
        """Decoratore per una funzione dell'agente."""
        def deco(fn):
            def wrapper(*a, **k):
                return self.call(fn, *a, tool=tool, text_arg=text_arg, **k)
            wrapper.__name__ = fn.__name__
            return wrapper
        return deco


def governed(client, agent_id, resource_id, framework="custom", **kw):
    return Governed(client, agent_id, resource_id, framework=framework, **kw)
