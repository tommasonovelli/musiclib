# Registro di dubbi, bug e incertezze

Tutto ciò che emerge durante l'implementazione e che **non** è una normale
scelta di codice finisce qui: bug in dipendenze esterne, punti ambigui di
`DESIGN.md`, rischi noti, decisioni prese in assenza di indicazioni.

Ogni voce ha uno stato: `APERTO` · `RISOLTO` · `DA CONFERMARE` (serve una
decisione di prodotto) · `ACCETTATO` (rischio noto e consapevole).

Riferimenti incrociati: `PROGRESS.md` per lo stato di avanzamento.

---

## Bug trovati in dipendenze esterne

### N-001 · `cases.Fold()` di x/text non è idempotente sulle lettere cherokee — RISOLTO
*Trovato il 2026-09-20 dal fuzzing di `FuzzKey`, in `internal/names`.*

`golang.org/x/text/cases.Fold()` v0.41.0 esegue `fold(U+ABB8) = U+13E8` **e**
`fold(U+13E8) = U+ABB8`: il folding oscilla con periodo 2. `CaseFolding.txt` di
Unicode mappa invece `AB70..ABBF -> 13A0..13EF` e `13F8..13FD -> 13F0..13F5` (le
maiuscole cherokee sono il target canonico, perché codificate per prime).

**Impatto sul dominio:** due album il cui nome differisce solo per il case
avrebbero ottenuto `folder_key` diverse, quindi due righe distinte in
`path_claims`, contro la §5.3 ("varianti solo maiuscole/minuscole dello stesso
album occupano una riga").

**Risoluzione:** `names.Key` applica la mappatura corretta dopo il folding
(`foldCherokee`). L'input incriminato resta nel corpus di fuzzing come test di
regressione; `TestKeyCopreTuttoUnicode` enumera tutti i code point e verifica
che non esistano altri casi di non idempotenza o di orbite di case spezzate.

**Da rivalutare** a ogni aggiornamento di `golang.org/x/text`: se il bug viene
corretto a monte, `foldCherokee` diventa un no-op ma **non va rimossa senza
migrazione delle chiavi**, perché l'algoritmo è congelato (§5.2).

---

## Ambiguità di `DESIGN.md`

### N-002 · "Trim di spazi/punti esterni": entrambi i lati o solo in coda? — DA CONFERMARE
§5.2. L'implementazione applica il trim **a entrambe le estremità**, come dice la
lettera della spec. Conseguenza visibile: un file importato `.nascosto` si
materializza come `Extras/nascosto`.

- A favore: nessun allegato può creare file nascosti nell'output; nessun
  allegato può collidere con `.musiclib.json`.
- Contro: la §7.2 dice "File nascosti diversi da questi vengono conservati", che
  potrebbe riferirsi alla conservazione del *contenuto* (che avviene) o anche del
  *nome* (che non avviene).

La regola Windows da cui deriva il vincolo riguarda solo spazi/punti **finali**.
Se si preferisce conservare il punto iniziale, la modifica è di una riga in
`names.sanitize`, ma cambia le `folder_key`: va fatta **ora**, non dopo il primo
import.

### N-003 · Estensione più lunga del budget di troncamento — DECISO
§5.2 impone 180 byte per componente e dice "L'estensione viene preservata", senza
definire il caso in cui l'estensione da sola non lasci spazio allo stem.
Implementato: si rinuncia a preservare l'estensione anziché superare i 180 byte.
Il limite di lunghezza è un invariante, la preservazione dell'estensione no.

### N-004 · "Pareggio lessicografico" per il genere dell'album — APERTO
§7.3. Non è specificato su cosa si ordini: byte UTF-8 grezzi, forma NFC, o chiave
casefold. Va deciso quando si implementa §7.3, e fissato in un test: è un input
del determinismo dell'import.

### N-005 · Ordine dei controlli su profondità e lunghezza dei percorsi — DECISO
§5.2 dice "Massimo 16 livelli e 1.024 byte **dopo la trasformazione**". La
profondità è controllata prima della sanitizzazione perché la sanitizzazione non
cambia il numero di segmenti (è per-segmento e non introduce né rimuove `/`); i
1.024 byte sono controllati dopo. Equivalente, ma con messaggi d'errore migliori.

---

## Scostamenti consapevoli dalla spec

### N-006 · Caratteri di controllo aggiunti all'insieme dei caratteri vietati — ACCETTATO
§5.2 elenca `/ \ : * ? " < > |`. L'implementazione sostituisce con `_` anche i
caratteri di controllo (C0, C1, DEL): non possono comparire in un nome di file
dell'output né in un header HTTP `Content-Disposition`. La §5.2 li rifiuta già
nei testi dei metadati, quindi è coerente con l'intento.

### N-007 · Byte NUL rifiutato esplicitamente nei percorsi relativi — ACCETTATO
`names.SplitRelPath` restituisce `path_nul_byte` invece di lasciar fallire la
syscall con un errore opaco.

### N-008 · Package `internal/names` non elencato in §2.3 — ACCETTATO
La §2.3 non assegna un package alla normalizzazione, ma la §13.2 impone una sola
implementazione e i consumatori sono `catalog`, `importer` e `render`.

---

## Rischi e incertezze aperte

### N-009 · Toolchain Go 1.25 e versioni delle dipendenze — ACCETTATO
`golang.org/x/text` è pinnata a `v0.41.0`, l'ultima compatibile con la direttiva
`go 1.25.0`; `v0.42.0` richiede Go 1.26. La §2.1 impone versioni fissate e mai
`latest`. Quando si scriverà il `Dockerfile`, la versione di Go, di TagLib e di
ffmpeg vanno fissate lì con lo stesso criterio, e concorrono a `render_version`.

### N-010 · `render_version` non è ancora definita — APERTO
§2.1 la descrive come costante di build che copre codice del renderer, regole dei
nomi, mapping dei tag e versioni dei tool. Va introdotta prima del primo render,
e deve includere un identificatore della versione dell'algoritmo di `internal/names`.

### N-011 · Nessuna verifica in CI su ext4 — APERTO
§12.1 richiede test delle primitive reali su un volume ext4 di test. Al momento i
test girano sul filesystem dello sviluppatore, senza garanzia che sia ext4.
Da affrontare con `internal/fsops` e con gli script di CI.
