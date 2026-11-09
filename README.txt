OPEN-GENCUT - PIATTAFORMA AMP PER LA GOVERNANCE DEGLI AGENTI IA
----------------------------------------------------------------

Autore: Quirino Scaramastra
Progetto a scopo didattico.

Versione alfa per il laboratorio. Non usare in produzione.

COSA FA
-------
Open-GenCut governa gli agenti IA di un'azienda da un unico punto:
- registro di tutti gli agenti (interni, di terze parti, dal marketplace), con scoperta da manifesti;
- controllo di accesso per agente a modelli LLM, API, server MCP, fonti dati e gateway AI, con allowlist, limiti, approvazioni umane e registro accessi;
- ciclo di vita: versioni, deploy e promozioni tra sviluppo, staging e produzione, valutazioni, sospensione, dismissione, identita' per agente;
- conformita': politiche, misure di sicurezza dei dati, quadri e controlli normativi;
- guardrail su ingresso e uscita: mascheramento e blocco dei dati personali, parole vietate, lunghezza, frequenza, prompt injection;
- valutazione: tracce, regole, deriva, deviazioni nei ragionamenti, giudizio con un modello, ingestione OpenTelemetry;
- marketplace e FinOps: inserzioni, richieste, provisioning, costi, esiti e ROI.

Ha un'interfaccia web in italiano, inglese, francese e tedesco, con guida in linea.

REQUISITI
---------
Vedi il file PREREQUISITES.txt.

INSTALLAZIONE E AVVIO
---------------------
1. Avvia SQL Server 2022 in un container, con un database chiamato geniuscut.
   La password dell'amministratore del database va messa in un file .env, mai nei file del progetto.

2. Crea lo schema del database. Dalla cartella del progetto:
     docker cp db/schema.sql <contenitore-sql>:/tmp/schema.sql
     docker exec <contenitore-sql> bash -c 'sqlcmd -S localhost -U sa -P "$MSSQL_SA_PASSWORD" -C -No -b -i /tmp/schema.sql'

3. Avvia il server. Dalla cartella docker/:
     export MSSQL_SA_PASSWORD='...'
     docker compose up -d

4. Apri il browser su:
     http://localhost:8080      (accesso in chiaro, rete di laboratorio)
     https://localhost:8443     (accesso cifrato, certificato auto-firmato da accettare)

5. Al primo accesso l'utente admin ha la password iniziale 'admin' e deve cambiarla.
   Questa e' una credenziale di laboratorio: cambiarla subito.

INDIRIZZI PRINCIPALI
--------------------
- /                  registro degli agenti
- /integrations.html risorse, allowlist, limiti, approvazioni, strumenti MCP, verifica accesso
- /lifecycle.html    versioni, deploy, promozioni, valutazioni, identita'
- /compliance.html   conformita'
- /guardrails.html   guardrail
- /evaluation.html   valutazione
- /marketplace.html  marketplace
- /finops.html       FinOps
- /chat.html         test chatbot verso OpenAI o Anthropic
- /users.html        utenti (solo amministratore)
- /help.html         guida in linea

CHIAMATE PER GLI AGENTI
-----------------------
Un agente o la sua applicazione chiama l'API prima di ogni accesso a una risorsa
(POST /api/integrations/check) e registra ogni chiamata (POST /api/lifecycle/agents/{id}/calls).
Un SDK Python e' in sdk/opengencut.py; gli adattatori per i framework sono in sdk/adapters.py.
Il contratto completo delle API e' in docs/API.txt.

VERIFICA
--------
Le suite di regressione sono in tests/. Si lanciano con:
     docker/run-tests.sh
che esegue tutte le suite in un container Python sulla rete del server.
Le suite che richiedono un accesso usano le variabili GENCUT_TEST_USER, GENCUT_TEST_PASSWORD
e, per le sezioni admin, GENCUT_ADMIN_PASSWORD: si impostano nell'ambiente, non nei file.
Il file tests/setup_test_user.py prepara l'utente di test.

ARRESTO
-------
Dalla cartella docker/:
     docker compose down
Per fermare il proxy HTTPS:
     docker rm -f opengencut-https

MANUALI
-------
I manuali operativi e le guide utente sono in docs/manuali/ (italiano), con le traduzioni in
docs/manuali/en, docs/manuali/fr e docs/manuali/de.

LICENZA
-------
Apache License 2.0, vedi i file LICENSE e NOTICE.
