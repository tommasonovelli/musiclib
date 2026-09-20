# Stato di implementazione

Tracciamento di cosa è stato implementato rispetto a `DESIGN.md`.
Ordine di riferimento: DESIGN.md §13.1.

Dubbi, bug e incertezze stanno in **`NOTES.md`**, non qui.

**Legenda:** `[ ]` da fare · `[~]` parziale · `[x]` fatto e testato

---

## Fase 1 — Fondamenta

- [x] Modulo Go (`musiclib`, Go 1.25.0, `golang.org/x/text v0.41.0` pinnata)
- [x] Normalizzazione: testo, segmenti, troncamento, chiavi, path relativi (§5.2) — `internal/names`
- [ ] Struttura completa del repository (§2.3): restano gli altri package
- [ ] Docker Compose: `app` + PostgreSQL 17, digest fissati (§2.1, §11.1)
- [ ] Migrazioni `goose` dello schema normativo (§4.2)
- [ ] Query `sqlc` (§2.1)
- [ ] `internal/fsops`: primitive confinate (`openat2 RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS`, `renameat2`, fsync) (§10.4)
- [ ] Lock del volume (`flock` su `/data/.lock`) e identità volume (`.musiclib-store` ↔ `settings.store_id`) (§2.2, §11.1)
- [ ] `internal/blobstore`: put verificato a 5 passi, dedup, `corrupt_blob` (§7.5)
- [ ] Verifiche di boot: stesso filesystem, `RENAME_EXCHANGE` disponibile (§3.1)

## Fase 2 — Prima fetta verticale (un album FLAC)

- [ ] `internal/media`: adapter `ffprobe` / `ffmpeg`, `AudioDigest` (§8.4)
- [ ] `native/musiclib-tags`: helper C++ TagLib (inspect / extract-images / write-managed-tags) (§8.1)
- [ ] Mapping dei tag gestiti e rimozione alias (§8.2, §8.3)
- [ ] `internal/importer`: import di un singolo candidato album
- [ ] `internal/catalog`: transazioni di dominio, revisioni, prenotazioni, enqueue (§4.3, §5.3)
- [ ] `internal/render`: snapshot → piano puro → costruzione in staging (§9.1)
- [ ] Ricevuta `.musiclib.json` (§9.2)
- [ ] `internal/publish`: PREPARE / INSTALL / FINALIZE + journal (§9.3)
- [ ] Recovery del journal all'avvio (§9.4)
- [ ] `internal/jobs`: claim, pool, completamento (§6.2, §6.4)

## Fase 3 — Concorrenza

- [ ] Coalescenza dei render su riga unica per album (§6.3)
- [ ] Snapshot `REPEATABLE READ` (§6.2)
- [ ] `path_claims` e `pg_advisory_xact_lock` globale (§5.3)
- [ ] API condizionali: ETag forte, `If-Match`, 412/428 (§10.1)
- [ ] Rinomina artista e riassegnazione album (§4.3)
- [ ] Failpoint nominati e matrice di guasto (§12.2)

## Fase 4 — Formati e contenuti

- [ ] MP3 (ID3v2.4, APE, migrazione ID3v1), M4A AAC/ALAC (§8.1–8.3)
- [ ] Cover: selezione, limiti, upload, rimozione (§7.4, §8.5)
- [ ] LRC associati alle tracce (§7.4)
- [ ] Allegati sotto `Extras/` (§5.1, §7.4)
- [ ] Verifica e preservazione dei tag non gestiti (§8.3)

## Fase 5 — Esperienza completa

- [ ] Scansione ricorsiva e raggruppamento multidisco (§7.2)
- [ ] Metadati iniziali dedotti e override d'import (§7.3)
- [ ] Fingerprint e riconoscimento duplicati (§7.6)
- [ ] API HTTP complete (§10.2)
- [ ] UI: Libreria, Album, Import, Attività (§10.3)
- [ ] Confine di sicurezza HTTP: `PUBLIC_ORIGIN`, `X-Musiclib-Request` (§10.4)
- [ ] Cestino, ripristino, retry (§4.3, §6.4)

## Fase 6 — Operatività

- [ ] `doctor` normale e `--deep` (§11.3)
- [ ] `rebuild` con marker di manutenzione (§11.3)
- [ ] `backup` / `restore` (§11.4)
- [ ] Budget di spazio e controllo `statfs` (§11.2)
- [ ] Guida operativa con esempi Compose (§11)

---

---

## Dettaglio di ciò che è fatto

### `internal/names` — normalizzazione (§5.2) ✔

API pubblica:

| Funzione | Ruolo |
|---|---|
| `NormalizeText` / `NormalizeRequiredText` | testo dei metadati: NFC, trim, niente caratteri di controllo, max 1.024 caratteri |
| `Segment` | segmento di directory sanitizzato |
| `FileSegment` | come `Segment`, ma preserva l'estensione nel troncamento |
| `Key` | chiave di confronto `NFC(casefold(segmento_finale))` |
| `FolderKey` | `Key(Segment(nome))`: valore di `artists.folder_key` e `albums.folder_key` |
| `PathKey` | chiavi dei segmenti unite da `/`: valore di `attachments.path_key` |
| `SplitRelPath` / `SplitRelPathOrRoot` | validazione di un percorso relativo, segmenti **non** sanitizzati (nomi da aprire sul disco) |
| `SanitizeRelFilePath` | percorso relativo dell'output: segmenti sanitizzati, `Path` e `Key` |
| `Error` / `Code` | errori tipizzati con codice stabile per il corpo `{code, message, details}` dell'API |

Test: tabellari, idempotenza, fuzzing (`FuzzSegment`, `FuzzKey`, `FuzzNormalizeText`,
`FuzzSplitRelPath`) ed enumerazione esaustiva di tutti i code point Unicode per le
proprietà di `Key`. Copertura 99,2%; `go vet` e `go test -race` puliti.

```sh
go test ./...                                    # suite completa (~0,4 s)
go test ./internal/names/ -run=XXX -fuzz=FuzzSegment -fuzztime=60s
```

---

## Decisioni prese durante l'implementazione

1. **Package `internal/names`.** La §2.3 non assegna un package alla normalizzazione,
   ma la §13.2 impone una sola implementazione e i consumatori sono `catalog`,
   `importer` e `render`: sta quindi in un package proprio, puro, senza I/O né DB.
2. **Due entry point per i segmenti.** `Segment` per le directory, `FileSegment` per
   i file: solo il secondo preserva l'estensione quando tronca. Un'unica funzione
   avrebbe dovuto indovinare se `Kind of Blue (feat. J.C.)` ha un'estensione.
3. **Estensione ingombrante.** Se l'estensione non lascia spazio allo stem si
   rinuncia a preservarla, anziché produrre un nome oltre i 180 byte.
4. **Versioni fissate.** `golang.org/x/text` è pinnata a `v0.41.0` (§2.1: mai
   `latest`); è l'ultima versione compatibile con la direttiva `go 1.25.0`.

## Scostamenti dalla spec

1. **Caratteri di controllo nei segmenti.** La §5.2 elenca `/ \ : * ? " < > |` come
   caratteri da sostituire con `_`. Sono stati aggiunti i caratteri di controllo,
   sostituiti allo stesso modo: non possono comparire in un nome di file
   dell'output né in un header HTTP. La §5.2 li rifiuta già nei testi dei metadati.
2. **Byte NUL nei percorsi relativi.** `SplitRelPath` lo rifiuta esplicitamente
   (`path_nul_byte`) invece di lasciare che fallisca la syscall.
3. **Trim dei punti esterni su entrambi i lati.** "Trim di spazi/punti esterni" è
   applicato letteralmente, quindi un file importato `.nascosto` si materializza
   come `Extras/nascosto`. Il contenuto è conservato, il nome no. È anche la
   ragione per cui un allegato non può creare file nascosti nell'output.
4. **Correzione del case folding cherokee.** `cases.Fold()` di x/text v0.41.0 non
   è idempotente su quello script: `fold(U+ABB8) = U+13E8` e `fold(U+13E8) =
   U+ABB8`, contro `CaseFolding.txt` che mappa `AB70..ABBF -> 13A0..13EF` e
   `13F8..13FD -> 13F0..13F5`. Senza correzione due album che differiscono solo
   per il case avrebbero `folder_key` diverse e occuperebbero due righe di
   `path_claims`. `Key` applica la mappatura corretta dopo il folding; il bug è
   stato trovato dal fuzzing e l'assenza di altri casi è verificata enumerando
   tutti i code point.

## Domande aperte per la spec

Vedi `NOTES.md`. Le più urgenti, perché cambiano le chiavi già scritte su disco:

- **N-002** trim del punto iniziale: `.nascosto` diventa `nascosto`. Da confermare
  **prima** del primo import reale.
