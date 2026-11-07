/* Barra di navigazione comune di Open-GenCut.
 * - inserisce la barra nel contenitore #gc-nav e la traduce con la lingua scelta (localStorage gencut.lang);
 * - controlla la sessione: senza accesso porta a /login.html; con password da cambiare porta a /account.html;
 * - mostra utente e profilo, il pulsante Esci e, solo per l'amministratore, la voce Utenti. */
(function () {
  "use strict";
  var ITEMS = [
    { href: "/", key: "home", label: { it: "Registro agenti", en: "Agent registry", fr: "Registre des agents", de: "Agentenregister" } },
    { href: "/integrations.html", key: "integrations", label: { it: "Integrazioni", en: "Integrations", fr: "Intégrations", de: "Integrationen" } },
    { href: "/lifecycle.html", key: "lifecycle", label: { it: "Ciclo di vita", en: "Lifecycle", fr: "Cycle de vie", de: "Lebenszyklus" } },
    { href: "/compliance.html", key: "compliance", label: { it: "Conformità", en: "Compliance", fr: "Conformité", de: "Konformität" } },
    { href: "/guardrails.html", key: "guardrails", label: { it: "Guardrail", en: "Guardrails", fr: "Garde-fous", de: "Guardrails" } },
    { href: "/evaluation.html", key: "evaluation", label: { it: "Valutazione", en: "Evaluation", fr: "Évaluation", de: "Bewertung" } },
    { href: "/marketplace.html", key: "marketplace", label: { it: "Marketplace", en: "Marketplace", fr: "Marketplace", de: "Marketplace" } },
    { href: "/finops.html", key: "finops", label: { it: "FinOps", en: "FinOps", fr: "FinOps", de: "FinOps" } },
    { href: "/chat.html", key: "chat", label: { it: "Test chatbot", en: "Chatbot test", fr: "Test chatbot", de: "Chatbot-Test" } },
    { href: "/about.html", key: "about", label: { it: "Info", en: "About", fr: "À propos", de: "Über" } },
    { href: "/help.html", key: "help", label: { it: "Guida", en: "Help", fr: "Aide", de: "Hilfe" } },
    { href: "/users.html", key: "users", admin: true, label: { it: "Utenti", en: "Users", fr: "Utilisateurs", de: "Benutzer" } }
  ];
  var X = {
    it: { password: "Password", logout: "Esci", role: { admin: "Amministratore", manager: "Responsabile", operator: "Operatore", auditor: "Controllo", viewer: "Consultazione", integration: "Sistema" }, aria: "Navigazione" },
    en: { password: "Password", logout: "Sign out", role: { admin: "Administrator", manager: "Manager", operator: "Operator", auditor: "Auditor", viewer: "Viewer", integration: "System" }, aria: "Navigation" },
    fr: { password: "Mot de passe", logout: "Quitter", role: { admin: "Administrateur", manager: "Responsable", operator: "Opérateur", auditor: "Contrôle", viewer: "Consultation", integration: "Système" }, aria: "Navigation" },
    de: { password: "Passwort", logout: "Abmelden", role: { admin: "Administrator", manager: "Verantwortlicher", operator: "Bediener", auditor: "Prüfung", viewer: "Ansicht", integration: "System" }, aria: "Navigation" }
  };
  var CSS = [
    ".gc-nav{display:flex;flex-wrap:wrap;align-items:center;gap:8px;padding:10px 16px;background:var(--panel,#fff);border-bottom:1px solid var(--line,#d9e0e7);}",
    ".gc-nav a{display:inline-block;padding:7px 14px;border-radius:999px;border:1px solid var(--line,#d9e0e7);background:var(--bg,#f5f7fa);color:var(--ink,#1c2530);text-decoration:none;font:14px/1.2 system-ui,'Segoe UI',Roboto,sans-serif;font-weight:500;}",
    ".gc-nav a:hover{border-color:var(--accent,#1f6feb);color:var(--accent,#1f6feb);}",
    ".gc-nav a.gc-active{background:var(--accent,#1f6feb);border-color:var(--accent,#1f6feb);color:var(--accent-ink,#fff);}",
    ".gc-nav a.gc-home{font-weight:700;}",
    ".gc-user{margin-left:auto;display:flex;align-items:center;gap:8px;flex-wrap:wrap;font:13px/1.2 system-ui,'Segoe UI',Roboto,sans-serif;color:var(--muted,#55657a);}",
    ".gc-user b{color:var(--ink,#1c2530);}",
    ".gc-user button{padding:6px 12px;border-radius:999px;border:1px solid var(--line,#d9e0e7);background:var(--panel,#fff);color:var(--ink,#1c2530);font:inherit;font-weight:600;cursor:pointer;}",
    ".gc-user button:hover{border-color:var(--danger,#b91c1c);color:var(--danger,#b91c1c);}"
  ].join("");
  var ME = null;

  function lang() {
    var l = "it";
    try { var s = localStorage.getItem("gencut.lang"); if (s && X[s]) l = s; } catch (e) { /* ignorato */ }
    return l;
  }
  function currentKey() {
    var p = location.pathname.replace(/\/+$/, "");
    if (p === "" || p === "/" || p === "/index.html") return "home";
    for (var i = 0; i < ITEMS.length; i++) { if (p === ITEMS[i].href) return ITEMS[i].key; }
    return "";
  }
  function esc(v) { return String(v === null || v === undefined ? "" : v).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;"); }
  function render() {
    var box = document.getElementById("gc-nav");
    if (!box) return;
    var l = lang(), cur = currentKey(), x = X[l];
    var isAdmin = ME && ME.role === "admin";
    var links = ITEMS.filter(function (it) { return !it.admin || isAdmin; }).map(function (it) {
      var cls = it.key === "home" ? "gc-home" : "";
      if (cur === it.key) cls = (cls + " gc-active").trim();
      return '<a href="' + it.href + '"' + (cls ? ' class="' + cls + '"' : "") + ">" + esc(it.label[l]) + "</a>";
    }).join("");
    var user = "";
    if (ME && ME.username) {
      user = '<div class="gc-user"><span><b>' + esc(ME.username) + "</b> · " + esc(x.role[ME.role] || ME.role) + "</span>" +
        '<a href="/account.html">' + esc(x.password) + "</a>" +
        '<button type="button" id="gc-logout">' + esc(x.logout) + "</button></div>";
    }
    box.innerHTML = '<nav class="gc-nav" aria-label="' + esc(x.aria) + '">' + links + user + "</nav>";
    var out = document.getElementById("gc-logout");
    if (out) out.addEventListener("click", logout);
  }
  function logout() {
    fetch("/api/auth/logout", { method: "POST", credentials: "same-origin" }).finally(function () { location.href = "/login.html"; });
  }
  function guard() {
    var p = location.pathname;
    if (p === "/login.html" || p === "/account.html") return Promise.resolve(true);
    return fetch("/api/auth/me", { credentials: "same-origin", headers: { "Accept": "application/json" } }).then(function (r) {
      if (r.status === 401) { location.href = "/login.html?next=" + encodeURIComponent(p); return false; }
      return r.json().then(function (me) {
        ME = me;
        if (me.must_change_password) { location.href = "/account.html"; return false; }
        render();
        return true;
      });
    }).catch(function () { return true; });
  }
  function init() {
    if (!document.getElementById("gc-style")) {
      var st = document.createElement("style"); st.id = "gc-style"; st.textContent = CSS; document.head.appendChild(st);
    }
    render();
    var sel = document.getElementById("lang");
    if (sel) sel.addEventListener("change", function () { setTimeout(render, 0); });
    guard();
  }
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", init); else init();
})();
