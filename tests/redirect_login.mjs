// Prova della destinazione dopo il login: node tests/redirect_login.mjs web/login.html  (e web/account.html)
import { readFileSync } from "node:fs";
// Estrae la funzione next() da login.html e la prova con indirizzi ostili.
const html = readFileSync(process.argv[2], "utf8");
const m = html.match(/function next\(\) \{[\s\S]*?\n  \}/);
const origin = "https://lab.example";
const run = (search) => {
  const location = { origin, search };
  const URLSearchParams_ = URLSearchParams;
  const f = new Function("location", "URLSearchParams", "URL", m[0] + "; return next();");
  return f(location, URLSearchParams_, URL);
};
const casi = [
  ["?next=/lifecycle.html", "/lifecycle.html"],
  ["?next=/index.html%3Fq%3D1%23x", "/index.html?q=1#x"],
  ["?next=//evil.com", "/"],
  ["?next=/%5Cevil.com", "/"],            // /\evil.com
  ["?next=/%09/evil.com", "/"],            // /<tab>/evil.com
  ["?next=/%0a/evil.com", "/"],            // /<a capo>/evil.com
  ["?next=https://evil.com", "/"],
  ["?next=javascript:alert(1)", "/"],
  ["?next=/\\evil.com", "/"],
  ["?next=", "/"],
  ["", "/"],
];
let ok = 0;
for (const [s, atteso] of casi) {
  const got = run(s);
  const bene = got === atteso;
  ok += bene ? 1 : 0;
  console.log((bene ? "OK  " : "FAIL") + ` ${JSON.stringify(s)} -> ${got}${bene ? "" : " (atteso " + atteso + ")"}`);
}
console.log(`${ok}/${casi.length}`);
process.exit(ok === casi.length ? 0 : 1);
