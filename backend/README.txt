OPEN-GENCUT - BACKEND (API V0.1, PUNTO 1)
-----------------------------------------

Server Go che implementa il contratto di docs/API.txt sul database geniuscut
(SQL Server 2022, container mon_mssql_express, porta 1433). Serve anche la GUI
statica di web/ sulla stessa origine.

Autore: Quirino Scaramastra.

Avvio
-----

Il server gira nel container golang:1.23-bookworm definito in docker/docker-compose.yml.
La password SA viene letta dalla variabile MSSQL_SA_PASSWORD (non va scritta nel compose):

    cd <cartella del progetto>/docker
    export MSSQL_SA_PASSWORD="$(grep '^MSSQL_SA_PASSWORD=' <cartella dello stack del database>/.env | cut -d= -f2- | tr -d '\"')"
    docker compose up -d

Porta: 8080 (http://localhost:8080). Log: docker logs -f opengencut-api.
Al primo avvio il container compila il modulo (circa 30-40 secondi).

Database
--------

Lo schema è in db/schema.sql (idempotente). Applicazione manuale:

    docker cp db/schema.sql mon_mssql_express:/tmp/schema.sql
    docker exec mon_mssql_express bash -c 'SQLCMD=$(ls /opt/mssql-tools18/bin/sqlcmd /opt/mssql-tools/bin/sqlcmd | head -1); "$SQLCMD" -S localhost -U sa -P "$MSSQL_SA_PASSWORD" -C -No -b -i /tmp/schema.sql'

Tabelle punto 1: agent, configuration, audit, usage_log.
Tabelle punto 2: integration_resource, agent_access, access_limit, access_event.

Struttura
---------

File  Contenuto
main.go  server, rotte, handler, validazione, audit, metriche
integrations.go  punto 2: risorse, allowlist, limiti, controllo accesso, registro accessi
go.mod, go.sum  modulo; unica dipendenza github.com/microsoft/go-mssqldb
../web/  GUI statica, servita su / (WEB_DIR, default /web)

Rotte
-----

Metodo  Percorso  Note
GET  /api/health  {"status":"ok","db":"ok"}; 503 se il database non risponde
GET  /api/agents?q=&origin=&status=&owner=  filtri opzionali; q cerca in nome, descrizione, fornitore, modello, proprietario
POST  /api/agents  201 con l'agente creato
GET  /api/agents/{id}
PUT  /api/agents/{id}  sostituzione completa: i campi omessi tornano vuoti/0
DELETE  /api/agents/{id}  204; cancella in cascata configurazioni e log di utilizzo
GET  /api/configurations  ordinate per chiave
POST  /api/configurations  201; key unica nel registro
PUT  /api/configurations/{id}  sostituzione completa
DELETE  /api/configurations/{id}  204
GET  /api/metrics/summary  conteggi per origine e stato; costi (vedi sotto)
GET  /api/metrics/usage?agent_id=&from=&to=  aggregato per agente e giorno; from/to in YYYY-MM-DD (estremi inclusi)
GET  /api/audit?limit=50  dal più recente; massimo 500

Valori ammessi: origin = internal | marketplace | third_party; status = registered | active | suspended | retired;
currency = EUR | USD; scope = global | agent (con agent_id obbligatorio solo per agent).

Punto 2 - controllo accessi (`/api/integrations`)
-------------------------------------------------

Metodo  Percorso  Note
GET  /api/integrations/resources?kind=  risorse: llm_model, api, mcp_server, data_source
POST  /api/integrations/resources  201
GET, PUT, DELETE  /api/integrations/resources/{id}  PUT sostituisce tutto; DELETE 204 e cascata su allowlist e limiti
GET  /api/integrations/allowlist?agent_id=  una riga = l'agente può usare la risorsa
POST  /api/integrations/allowlist  {agent_id, resource_id}; 201, duplicato 409
DELETE  /api/integrations/allowlist/{id}  204
GET  /api/integrations/limits?agent_id=
POST  /api/integrations/limits  {agent_id, resource_id, max_calls_per_day, max_cost_per_month, currency}; campi null = nessun limite
PUT, DELETE  /api/integrations/limits/{id}
POST  /api/integrations/check  {agent_id, resource_id, cost?}; risponde 200 sempre, con decision allowed/denied e reason
GET  /api/integrations/access-log?agent_id=&limit=50  storico delle decisioni, dal più recente

Ordine dei controlli in check (il primo che nega vince):
resource_disabled → agent_not_active → not_allowed → daily_call_limit → monthly_cost_limit.
Il costo si conta solo sugli accessi permessi. Il limite giornaliero conta le chiamate permesse del giorno UTC;
il limite mensile somma i costi permessi del mese UTC. Il blocco sulla riga del limite evita che due chiamate
simultanee superino il limite insieme. Un errore del database durante il controllo nega l'accesso con reason: check_error.

Errori
------

Forma uniforme {"error": "codice", "message": "testo"}:

HTTP  error  Quando
400  validation_error  campi non validi, violazione di CHECK o di chiave esterna
400  bad_request  JSON malformato, campi sconosciuti, id non numerico
404  not_found  risorsa inesistente
409  conflict  chiave duplicata (configurazioni)
500  internal_error  errore interno; il dettaglio va solo nel log
503  —  /api/health quando il database non risponde

Scelte tecniche
---------------

- Audit: ogni scrittura scrive una riga in dbo.audit nella stessa transazione. actor è api, perché non c'è autenticazione in questa fase.
- Costi: cost_monthly_total somma solo gli agenti in EUR; quelli in USD compaiono in cost_monthly_other. Serve un tasso di cambio per sommarli.
- Cancellazione: fisica, con cascata. L'audit conserva la riga di cancellazione.
- Limiti: corpo richiesta massimo 1 MB; campi sconosciuti rifiutati; lunghezze come da schema.
- Nessun controllo di accesso (sandbox di laboratorio, come da docs/API.txt).
