# Vibrance MusicLib

**Specifica implementativa — versione 1.0 — 2026-09-20**

Questo documento sostituisce integralmente i design precedenti. Le decisioni qui descritte sono quelle della v1: non sono alternative fra cui scegliere durante lo sviluppo.

> **Poche funzioni, fatte bene.** Importare una collezione, correggerla da una UI e ottenere cartelle e tag ordinati, senza alterare gli originali. La semplicità è un requisito permanente, non una fase prima di un'architettura più grande.

---

## 1. Il prodotto

### 1.1 Risultato atteso

L'utente possiede album musicali, copertine, booklet, scansioni e testi. L'applicazione:

1. importa cartelle da un percorso locale montato in sola lettura;
2. conserva i file originali in uno storage immutabile;
3. permette di correggere artista, album, tracce, numerazione, genere e cover;
4. genera una libreria utilizzabile da un normale player;
5. mostra chiaramente import riusciti, errori e modifiche ancora da applicare.

Esempio di output:

```text
library/
  Miles Davis/
    Kind of Blue/
      .musiclib.json
      cover.jpg
      01 - So What.flac
      01 - So What.lrc
      02 - Freddie Freeloader.flac
      Extras/
        booklet.pdf
        Scans/front.jpg
        rip.log
```

`.musiclib.json` è una ricevuta tecnica generata, non un file di configurazione né una seconda fonte di metadati.

### 1.2 Funzionalità v1

- Import ricorsivo, multidisco, resoconto per album e riconoscimento degli import identici.
- FLAC, MP3, M4A con AAC o ALAC: lettura, tag, cover e verifica dell'audio.
- Elenco e ricerca di artisti/album; editor dell'album con tabella delle tracce.
- Rinomina di un artista e riassegnazione di un album a un altro artista.
- Cover JPEG/PNG: scelta da immagini importate, caricamento, sostituzione, rimozione.
- Allegati conservati, scaricabili, aggiungibili e rimovibili; LRC associati alle tracce.
- Cestino degli album, ripristino, rigenerazione di un album o di tutta la libreria.
- Verifica dell'integrità, backup e restore documentati e testati.

### 1.3 Fuori scope, anche nell'implementazione

Niente player, streaming, transcodifica dei file pubblicati, ricerca online, riconoscimento musicale, sincronizzazione bidirezionale, watcher, utenti/ruoli, plugin, storico generale, merge automatici o editor di tutti i tag possibili.

Niente import `move`, caricamento di intere librerie dal browser, immagini degli artisti, eliminazione definitiva dei blob o garbage collector nella v1. L'import avviene da `/import`; il browser carica solo cover, allegati e LRC.

Il featuring resta nel testo dell'artista della traccia. Non esistono parser euristici di `feat.`, entità degli ospiti o riscritture automatiche dei titoli.

---

## 2. Architettura scelta

### 2.1 Stack

| Area | Decisione |
|---|---|
| Ambiente supportato | Host Linux, Ubuntu 24.04 o successivo; volume dati locale ext4 |
| Deploy | Docker Compose: un container `app`, un container PostgreSQL |
| Backend | Go, libreria standard HTTP, `pgx`, SQL esplicito generato con `sqlc` |
| Database | **PostgreSQL 17**, migrazioni SQL con `goose` |
| UI | HTML server-rendered con `html/template`, CSS e piccoli moduli JavaScript; niente SPA e niente runtime Node |
| Tag | TagLib 2.x, dietro un piccolo eseguibile C++ interno `musiclib-tags` |
| Audio | `ffprobe` e `ffmpeg`, processi esterni senza shell |
| Parallelismo | Pool limitato di worker; album diversi elaborati in parallelo |
| Pubblicazione | Sostituzione della directory dell'album, con breve sezione critica globale e journal durevole |
| Identificativi | UUIDv7 generati dall'applicazione; sequenza PostgreSQL per i ticket dei job |

Versioni complete di Go, TagLib, ffmpeg e immagini base fissate nei file di build e nei digest Docker: mai `latest`. `render_version` è una costante di build che identifica codice del renderer, regole dei nomi, mapping dei tag e versioni dei tool. Ogni modifica a uno di questi ingressi cambia `render_version`.

L'helper TagLib isola crash, stato globale e thread-safety della libreria nativa. Non è un servizio: è un programma locale, piccolo, senza database né logica di dominio.

### 2.2 Un processo, più worker

```text
Browser ── HTTP ── app Go ───────────── PostgreSQL
                    │                   catalogo + coda + journal
                    ├── worker 1 ──┐
                    ├── worker 2 ──┼── copia / tag / verifica in work/
                    └── worker N ──┘                 │
                                             pubblicazione breve
                                                    │
                                                 library/

/import  sola lettura       originals/  immutabile dopo il put
```

Si parallelizza il lavoro costoso, non si fanno scrivere due job nello stesso album. La sezione critica di pubblicazione **non** contiene copie, tag, decodifiche, hashing dei media o cancellazioni ricorsive; legge soltanto piccole ricevute e metadati del filesystem.

Una sola istanza dell'applicazione per volume e database. Non è un'applicazione distribuita e non supporta repliche. Un `flock` esclusivo su `/data/.lock` viene acquisito prima di avviare worker o manutenzione e mantenuto fino alla terminazione. Il database contiene l'identità del volume, confrontata con `/data/.musiclib-store` per rifiutare accoppiamenti sbagliati.

### 2.3 Struttura del repository

```text
cmd/musiclibd/             server e sottocomandi di manutenzione
internal/http/            handler HTML/JSON, validazione delle richieste
internal/catalog/         transazioni di dominio, revisioni, prenotazioni, enqueue
internal/store/           query sqlc e migrazioni; nessun accesso ai media
internal/importer/        scansione, raggruppamento, import di un album
internal/render/          snapshot -> piano puro -> costruzione in staging
internal/publish/         journal, rename delle directory, recovery
internal/jobs/            claim, pool, cancellazione e completamento
internal/media/           adapter di TagLib, ffprobe e ffmpeg
internal/blobstore/       put e lettura degli originali
internal/fsops/           primitive Linux confinate alle root
internal/maintenance/     doctor, rebuild, backup, restore
native/musiclib-tags/     helper C++ e test dei formati
web/                      template, CSS, JavaScript; incorporati con go:embed
migrations/               SQL versionato
sql/                      query sorgenti di sqlc
scripts/                  comandi operativi e test end-to-end
```

Niente ORM, repository generici, event bus, CQRS, framework di job o contenitore di dependency injection. Le dipendenze vengono passate esplicitamente nei costruttori.

---

## 3. Contratto e storage

### 3.1 Dove vive la verità

- **PostgreSQL** contiene il catalogo desiderato: nomi, relazioni, tag gestiti, selezione delle cover e allegati.
- **`originals/`** contiene i byte immutabili dei file importati o caricati.
- **`library/`** è esclusivamente output dell'applicazione. Non contiene dati da importare al ritorno.
- **`work/`** contiene costruzioni temporanee e directory sostituite da ripulire.

```text
/data/
  .lock
  .musiclib-store
  originals/ab/cd/<sha256>
  library/<artista>/<album>/...
  work/
    blobs/<uuid>.tmp
    render/<build-id>/album/...
    retired/<build-id>/...
```

Tutto `/data` è sullo stesso filesystem. Nessun mount annidato in `originals`, `library` o `work`; niente hardlink, symlink o reflink fra originali e output. `/import` è un mount separato, in sola lettura, esterno a `/data`.

L'applicazione verifica al boot che `library` e `work` condividano il filesystem e che `renameat2(RENAME_EXCHANGE)` funzioni. ext4 e Linux sono requisiti: non si implementa un fallback con garanzie inferiori per NTFS, NFS o Docker Desktop.

### 3.2 Garanzie

1. La sorgente di un import non viene mai modificata o cancellata.
2. Nessun blob già fissato viene sovrascritto o cancellato dall'applicazione.
3. I tag vengono scritti solo su copie in staging.
4. Un album nuovo viene pubblicato soltanto dopo la verifica di **tutti** i suoi file.
5. Una sostituzione sullo stesso percorso mostra la vecchia o la nuova directory completa, mai una directory costruita progressivamente.
6. Metadati e richiesta di render si salvano nella stessa transazione.
7. Una pubblicazione iniziata è recuperabile dopo un crash senza ricordare lo stato del processo.
8. Le revisioni impediscono che un calcolo obsoleto sovrascriva una pubblicazione più recente.

Non esiste una transazione unica fra PostgreSQL e filesystem. Il journal descritto in §9 è precisamente il protocollo che collega i due.

Il filesystem garantisce la sostituzione del namespace, non uno snapshot ai player: un programma che mantiene file aperti può continuare a leggere la versione precedente. Una rinomina fra due percorsi può mostrare brevemente entrambe le cartelle; mai un album nuovo incompleto.

### 3.3 `library/` non è una cartella di lavoro dell'utente

I player devono accedervi in sola lettura. Le modifiche manuali non vengono importate; il render sostituisce l'intera cartella gestita, compresi eventuali file aggiunti al suo interno.

Un percorso nuovo già occupato sul disco ma non pubblicato per quell'album causa un conflitto: non viene adottato né cancellato automaticamente. Le directory dell'artista vengono rimosse solo con `rmdir`, se vuote.

Nessuna promessa di sopravvivenza a un guasto del disco senza backup. Immutabilità, integrità verificata e backup sono protezioni diverse.

---

## 4. Dominio e modello dati

### 4.1 L'album è l'aggregato

Una traccia appartiene a un solo album. Due tracce possono referenziare lo stesso blob: si condividono i byte, non la loro identità o le correzioni.

L'artista dell'album decide la cartella e il tag `album artist`. L'artista della traccia è un override testuale: `NULL` significa eredita dall'album. `Simon & Garfunkel` e `A feat. B` restano stringhe intere.

Ogni edizione è un album distinto. L'edizione sta nel titolo, per esempio `Kind of Blue (Mono)`, anche nei tag. Nessun modello opera/release.

`compilation` è un booleano indipendente dall'artista. `Various Artists` è una normale riga di `artists`, non un'identità magica o non modificabile.

`albums.genre` è il default; `tracks.genre = NULL` eredita, stringa vuota significa esplicitamente nessun genere. Per l'artista della traccia la stringa vuota non è valida: si usa `NULL` per ereditare.

### 4.2 Schema normativo

I nomi seguenti sono i nomi da usare nelle migrazioni. Campi `NOT NULL` salvo indicazione `?`. Timestamp `timestamptz`, hash SHA-256 lowercase di 64 caratteri, percorsi relativi UTF-8 con separatore `/`.

```text
settings
  id smallint PK CHECK(id = 1)
  store_id uuid UNIQUE

blobs
  hash text PK
  size bigint CHECK(size >= 0)
  format text?                  # flac | mp3 | m4a-aac | m4a-alac | jpeg | png; NULL = altro
  created_at timestamptz

artists
  id uuid PK
  name text
  folder_key text UNIQUE
  revision bigint CHECK(revision > 0)

albums
  id uuid PK
  artist_id uuid FK artists
  title text
  folder_key text
  year integer? CHECK(year BETWEEN 1 AND 9999)
  genre text?
  compilation boolean DEFAULT false
  cover_hash text? FK blobs
  revision bigint CHECK(revision > 0)
  deleted_at timestamptz?
  import_fingerprint text? UNIQUE
  published_path text?
  published_revision bigint DEFAULT 0
  published_renderer text?
  published_build uuid?
  published_receipt_hash text?

tracks
  id uuid PK
  album_id uuid FK albums
  disc integer CHECK(disc BETWEEN 1 AND 99)
  no integer CHECK(no BETWEEN 1 AND 999)
  title text
  artist text?
  genre text?
  blob_hash text FK blobs
  source_path text
  lyrics_hash text? FK blobs

attachments
  id uuid PK
  album_id uuid FK albums
  rel_path text
  path_key text
  blob_hash text FK blobs

path_claims
  path_key text PK
  path text
  album_id uuid FK albums

import_batches
  id uuid PK                   # idempotency key fornita dal client
  root_rel text
  created_at timestamptz

jobs
  id uuid PK
  kind text                   # scan | import | render
  album_id uuid? FK albums     # solo render
  batch_id uuid? FK import_batches  # scan/import
  source_rel text?             # solo import; relativo a /import
  overrides jsonb             # {} o {artist, title}, solo import
  requested bigint            # nextval(job_ticket)
  claimed bigint?
  state text                  # pending | running | done | skipped | failed
  result_album_id uuid? FK albums  # solo risultato dell'import
  error_code text?
  error_message text?
  warnings jsonb              # array di messaggi strutturati
  queued_at timestamptz
  updated_at timestamptz

publication
  id smallint PK CHECK(id = 1) # zero o una riga
  album_id uuid FK albums
  ticket bigint
  revision bigint
  renderer text
  build_id uuid
  receipt_hash text?           # NULL solo per rimozione
  old_path text?
  old_build uuid?
  new_path text?               # NULL = togliere l'album dalla library
```

Vincoli e indici obbligatori:

- `UNIQUE (artist_id, folder_key) WHERE deleted_at IS NULL` sugli album.
- `UNIQUE (album_id, disc, no) DEFERRABLE INITIALLY DEFERRED` sulle tracce: un riordino avviene in una transazione, senza numeri temporanei.
- `UNIQUE (album_id, path_key)` sugli allegati. Il dominio controlla anche le collisioni file/directory.
- `UNIQUE (album_id) WHERE kind = 'render'` sui job, **indipendente dallo stato**.
- `UNIQUE (batch_id) WHERE kind = 'scan'` e `UNIQUE (batch_id, source_rel) WHERE kind = 'import'`.
- `CHECK` per enum, hash, revisioni e combinazioni di colonne dei job; `overrides` e `warnings` validati anche da tipi Go chiusi. Default rispettivamente `{}` e `[]`; solo `running` ammette e richiede `claimed` non NULL. I render ammettono soltanto pending/running/failed.
- `CHECK (published_revision BETWEEN 0 AND revision)` sugli album; `published_renderer` presente se e solo se `published_revision > 0`.
- Indici su tutte le FK usate per ricerca e su `jobs (state, kind, queued_at, id)`.
- FK `RESTRICT`: niente cascata distruttiva implicita sui dati dell'utente.
- Le colonne `published_*` devono formare uno stato coerente: path/build/receipt presenti insieme; a output assente tutti e tre NULL. La revisione pubblicata può indicare anche una cancellazione completata.

`format` non è dedotto dall'estensione della sorgente. I vincoli di dominio richiedono blob audio per le tracce e JPEG/PNG validati per le cover. Sono verificati nei servizi di scrittura, non mediante trigger.

### 4.3 Revisioni e cancellazioni

Qualunque cambiamento che modifica l'output di un album incrementa `albums.revision` e accoda il suo render, nella stessa transazione. Un salvataggio senza cambiamenti effettivi non incrementa la revisione.

Rinominare un artista incrementa la revisione dell'artista e quella di tutti i suoi album; i render sono per album, non esiste `render_artist`. Il cambiamento è atomico nel DB ma viene materializzato album per album.

Eliminare un album imposta `deleted_at`: metadati e riferimenti restano. Ripristinare lo riattiva, con normale controllo dei conflitti di nome. Gli album nel cestino possono essere rinominati prima del ripristino.

Rimuovere una traccia o un allegato elimina la relativa riga dopo conferma e accoda il render. Non c'è undo di questa operazione; il blob resta. Non si permette un album attivo senza tracce. Aggiungere tracce a un album già importato è fuori scope v1; l'editor modifica o rimuove quelle presenti.

Gli artisti senza album non vengono mostrati nell'elenco principale. Non c'è un comando separato per eliminarli nella v1.

---

## 5. Nomi, collisioni e proprietà dei percorsi

### 5.1 Schema fisso

```text
<artista>/<album>/<NN> - <titolo>.<ext>
<artista>/<album>/Disc <D>/<NN> - <titolo>.<ext>   # multidisco
<artista>/<album>/cover.jpg oppure cover.png
<artista>/<album>/Extras/<percorso-allegato>
```

Multidisco significa più di un valore di `disc`, oppure un valore diverso da 1. Le cover restano nella radice dell'album. LRC accanto alla traccia con lo stesso basename. Numero con almeno due cifre, senza zeri aggiuntivi oltre quelli necessari.

**Tutti gli allegati stanno sotto `Extras/`.** Il piccolo cambiamento estetico elimina alla radice collisioni con cover, tracce, directory dei dischi e ricevuta tecnica. Non si aggiungono suffissi casuali e non si nascondono conflitti.

### 5.2 Un solo algoritmo di normalizzazione

- Testo dei metadati: NFC, trim esterno, rifiuto dei caratteri di controllo. Nomi e titoli obbligatori non vuoti; massimo 1.024 caratteri.
- Segmento di percorso: NFC, caratteri `/ \\ : * ? " < > |` sostituiti con `_`, trim di spazi/punti esterni, vuoto diventa `_`.
- Nomi DOS riservati (`CON`, `NUL`, `PRN`, `AUX`, `COM1..9`, `LPT1..9`, anche con estensione): prefisso `_`.
- Limite 180 byte UTF-8 per componente, inclusi prefissi ed estensione. Se necessario: troncamento su confine UTF-8 e suffisso `~` più i primi 8 caratteri SHA-256 del segmento completo normalizzato. L'estensione viene preservata.
- Chiave di confronto: `NFC(casefold(segmento_finale))`, usando `golang.org/x/text`, non `lower()` SQL.
- Chiave del percorso: chiavi dei singoli segmenti unite da `/`.

Le colonne `folder_key` sono calcolate da questo algoritmo, incluse sanitizzazione e troncamento. Una collisione resta un errore anche se dovuta al suffisso hash. Nessuna dipendenza dall'ordine dei job. L'algoritmo è congelato nella v1: un futuro cambiamento richiede migrazione delle chiavi e verifica preventiva dei conflitti, non soltanto un bump di `render_version`.

Per i percorsi relativi: rifiutare path assoluti, `.` e `..`, segmenti vuoti e UTF-8 invalido **prima** della sanitizzazione. Massimo 16 livelli e 1.024 byte dopo la trasformazione. La scansione rifiuta symlink e file speciali; non li segue. I percorsi delle sorgenti (`source_rel`, `source_path`, `root_rel`) vengono validati ma mantenuti esattamente come sul disco: NFC e sanitizzazione si applicano all'output, non al nome da aprire. La stringa vuota è ammessa soltanto per indicare la root `/import` nei percorsi di selezione/candidato.

Gli allegati conservano in `rel_path` il percorso relativo originale o scelto dall'utente; il piano lo sanitizza per segmento. Collisioni dopo la normalizzazione, comprese quelle fra un file e una directory, producono un errore esplicito con entrambi i nomi. L'utente deve correggerle; non si perde un file per deduplicazione dei nomi.

### 5.3 Prenotazioni, non tentativi sul filesystem

Il vincolo sul titolo non basta: un vecchio nome può essere ancora materializzato durante una rinomina.

Per ogni album `path_claims` contiene esattamente l'unione di:

1. percorso desiderato, se l'album è attivo;
2. percorso attualmente pubblicato, se presente;
3. `old_path` e `new_path` di una pubblicazione in corso per quell'album.

Un percorso può appartenere a **un solo album**. Una mutazione prenota i nuovi nomi prima del commit e libera soltanto le prenotazioni non più appartenenti a questa unione. L'unione è sulle chiavi normalizzate: varianti solo maiuscole/minuscole dello stesso album occupano una riga; `path` preferisce il nome desiderato, poi quello del journal, poi quello pubblicato. I percorsi fisici delle operazioni vengono invece dai campi esatti di album e journal: su ext4 `Abba/X` e `ABBA/X` sono percorsi diversi.

Tutte le mutazioni del catalogo, comprese import, preparazione/completamento del journal e riordini, usano lo stesso `pg_advisory_xact_lock` globale con chiave costante. È una serializzazione breve delle decisioni SQL; non racchiude I/O sui media. Non ci sono lock per cartella o ordini di acquisizione gerarchici.

Esempio: A viene rinominato da `X` a `Y`. `X` resta prenotato da A finché la vecchia directory non è ritirata. B che chiede `X` riceve `409 path_reserved`, con indicazione dell'album A. Può riprovare dopo il completamento. Nessun worker può rubare la prenotazione.

Una rinomina di artista prenota tutti i percorsi coinvolti nella stessa transazione: tutto accettato oppure niente. Scambi di nomi fra album e merge di artisti non sono automatici; si usa un nome temporaneo o si riassegnano gli album esplicitamente.

---

## 6. Parallelismo e coda durevole

### 6.1 Limiti semplici

`WORKERS` vale per default `max(1, min(4, CPU disponibili))`, modificabile tra 1 e 16.

Ogni worker esegue un job alla volta. Import e render lavorano in parallelo su album diversi; le tracce di un singolo album vengono elaborate sequenzialmente. Niente pool annidati.

I subprocessi ffmpeg usano un thread di codec e un thread di filtro. Un semaforo globale di capacità `WORKERS` limita i tool nativi, anche quando invocati fuori dal pool. Le richieste HTTP di upload sono limitate a due copie simultanee. I file si copiano e si verificano in streaming, non caricandoli interamente in RAM.

I job render hanno precedenza sugli import, poi si ordina per `queued_at, id`; la scansione ha precedenza sugli import dei singoli album. È una politica fissa, non un sistema configurabile di priorità. Una sequenza continua di modifiche può ritardare gli import: limite accettato per uso personale.

### 6.2 Claim e snapshot

Ogni worker reclama una riga pending con `SELECT ... FOR UPDATE SKIP LOCKED`, imposta `running` e copia `requested` in `claimed`. Il claim è una transazione breve.

Per un render, claim e caricamento di artista/album/tracce/allegati avvengono in una transazione `REPEATABLE READ`: lo snapshot è coerente. Nessuna transazione resta aperta durante la costruzione dei file. Un errore di serializzazione ritenta la transazione, non il lavoro sul disco.

Il piano è una funzione pura dello snapshot e di `render_version`. Non fa query né accessi al filesystem.

### 6.3 Coalescenza senza job duplicati

`enqueueRender(album)` esegue un upsert sulla riga unica del suo album:

```text
requested = nextval(job_ticket)
state = running, se era già running; altrimenti pending
error = NULL
queued_at = adesso
```

Un job running conserva il suo `claimed`: il ticket nuovo significa che esiste altro lavoro dopo quello in corso. Qualunque transizione fuori da running azzera `claimed`. I completamenti sono condizionati da ID, stato running e ticket reclamato: un risultato vecchio non può completare un tentativo diverso.

Prima di preparare la pubblicazione si ricontrollano, sotto il lock del catalogo:

- `requested == claimed` del lavoro costruito;
- revisione dell'album uguale a quella dello snapshot;
- versione del renderer uguale a quella corrente;
- validità delle prenotazioni.

Se il lavoro è superato, lo staging si scarta e il job torna pending. Non viene pubblicato.

Dopo la preparazione del journal le modifiche API sono ancora ammesse. La pubblicazione preparata termina sulla propria revisione; eventuali modifiche successive restano pending. **Non** si scrive mai `published_revision = revisione corrente` senza usare quella effettivamente costruita.

### 6.4 Completamento, errori e riavvio

- Render riuscito: se il ticket è ancora quello eseguito, cancellare il job; altrimenti riportarlo pending.
- Render fallito prima del journal: `failed` soltanto se il ticket è ancora corrente; altrimenti pending per la richiesta nuova.
- Import e scan riusciti: mantenere `done`/`skipped` per il resoconto. Esiti conservati 90 giorni; non si eliminano batch con job non terminali.
- Errori di contenuto, spazio o permessi: nessun retry automatico; errore visibile, pulsanti **Riprova** e **Riprova falliti**. Un cambiamento successivo dell'album riattiva il render.
- Polling ogni 2 secondi; un segnale in memoria dopo il commit riduce la latenza, ma non è necessario alla correttezza.
- Al boot: prima recuperare l'eventuale pubblicazione, poi `running -> pending`. Una riga per album evita conflitti fra un running recuperato e un pending separato.

Un risultato incerto del commit o la perdita della connessione al DB non diventano un normale errore di file. L'applicazione interrompe le nuove mutazioni e i worker, termina i processi figli e si riavvia tramite Docker. Il nuovo processo aspetta PostgreSQL, acquisisce il lock del volume ed esegue il normale recovery. Questa scelta evita una seconda macchina a stati per riconnettere worker parzialmente attivi.

Errori SQL attesi, come unicità, non provocano riavvii. `40001` e deadlock ritentano solo la breve transazione, fino a tre volte con jitter. Non si ritenta alla cieca un commit dall'esito sconosciuto.

---

## 7. Import

### 7.1 Un input solo: una directory sotto `/import`

La UI esplora esclusivamente `/import` e invia un percorso relativo. L'API non accetta path arbitrari dell'host. La root non può essere `/data` né contenere symlink verso di essa.

`POST /api/imports` riceve un UUID di richiesta e crea `import_batches` più un job `scan` in transazione. Ripetere lo stesso UUID con lo stesso percorso restituisce lo stesso batch; con parametri diversi restituisce `409`.

Il mount di origine deve rimanere disponibile e invariato fino al completamento. L'applicazione rileva aggiunte/rimozioni e variazioni di identità, size o mtime durante un import e in tal caso lo rifiuta. Non promette uno snapshot di una sorgente modificata da un altro programma. Non cancella comunque mai la sorgente.

### 7.2 Scansione e unità d'importazione

La scansione è ricorsiva, deterministica, senza symlink. Ignora soltanto `.DS_Store`, `Thumbs.db`, `desktop.ini`, con confronto case-insensitive. File nascosti diversi da questi vengono conservati.

Il riconoscimento finale usa il contenuto con `ffprobe`, non il nome. Un probe che trova audio definisce un file audio; formati non supportati e file corrotti con estensione audio nota sono errori dell'album, non allegati silenziosi. La lista delle estensioni audio note è fissata nei test: flac, mp3, m4a, mp4, aac, wav, aif, aiff, ogg, opus, wma, ape, wv, dsf, dff.

Un file senza stream audio e senza identità di audio corrotto è un allegato. Immagini, PDF, CUE e LOG non richiedono un probe audio riuscito.

Regole di raggruppamento:

1. Una directory con file audio diretti è un candidato album.
2. Una directory senza audio diretto, i cui discendenti con audio sono solo figli diretti chiamati `CD<N>` o `Disc <N>` con N positivo, è un solo album multidisco. Case-insensitive; gli zeri iniziali sono ammessi.
3. Due directory disco con lo stesso numero sono un errore. Altre sottocartelle senza audio sono allegati del candidato.
4. Audio diretto insieme ad altro audio discendente, oppure una struttura che produce candidati sovrapposti, è ambiguo: errore per quel ramo, senza import parziale del ramo.
5. Nei restanti rami si cercano ricorsivamente album indipendenti. File esterni a qualsiasi candidato vengono elencati come non assegnati nel resoconto, non scartati senza avviso.

Dentro un candidato, tag `album` non vuoti discordanti sono un errore `mixed_album`. La v1 non trasforma una cartella di singoli in album indovinati.

Lo scan costruisce l'elenco dei candidati, poi inserisce tutti i job import e il proprio esito in una transazione. Per un candidato/ramo ambiguo inserisce un job import già failed, con percorso e motivo; i file non assegnati sono warnings del job scan. Il vincolo `(batch_id, source_rel)` rende sicuro ripeterlo dopo un crash. Errori di un candidato non impediscono gli altri. Un retry import rivalida sempre il candidato corrente, non assume valida la vecchia scansione. Nessun candidato valido significa batch completato con spiegazione, non successo vuoto.

Massimo 1.000 tracce e 10.000 file per candidato, verificati prima dell'import; nessun file eccedente viene ignorato. Questi limiti proteggono anche la dimensione dell'editor e delle ricevute.

### 7.3 Metadati iniziali

Tutto viene letto nuovamente dalle copie verificate, non assunto dai risultati dello scan.

| Campo | Regola |
|---|---|
| Album | unico tag album non vuoto; altrimenti nome della directory radice del candidato |
| Artista album | unico `album artist` non vuoto; altrimenti unico artista non vuoto delle tracce; se più di uno `Various Artists`; se tutti assenti `Unknown Artist` |
| `album artist` discordanti | import rifiutato come ambiguo |
| Artista traccia | tag artist completo; NULL se assente o uguale all'artista album normalizzato |
| Titolo traccia | tag title non vuoto; altrimenti basename senza estensione |
| Disco | numero della directory disco; senza directory, tag disc positivo o 1 |
| Numero traccia | tag track, se tutti i valori del disco sono positivi e distinti; altrimenti tutto il disco viene numerato per ordine naturale dei basename, con avviso |
| Anno | valore valido più frequente, pareggio risolto con il più piccolo; valori discordanti producono avviso |
| Genere album | genere non vuoto più frequente, pareggio lessicografico; differenze conservate come override traccia |
| Compilation | true se almeno un tag compilation è true o se è stato scelto automaticamente Various Artists |

L'ordine naturale confronta sequenze di cifre come interi; i pareggi usano il percorso UTF-8 completo. Directory e file vengono sempre ordinati esplicitamente, mai secondo l'ordine restituito dal filesystem. Numeri di disco/traccia fuori dai limiti dello schema producono errore, non overflow o troncamento. Tag testuali multivalore vengono rappresentati come un'unica stringa, unendo i valori nell'ordine originale con `; `; non si perdono valori scegliendo arbitrariamente il primo.

`overrides.artist` e `overrides.title`, inseribili dalla UI quando un import fallisce, sostituiscono i valori dedotti. L'artista esplicito risolve gli album artist discordanti; il titolo esplicito permette anche di risolvere `mixed_album`. Non ci sono altri override d'import: il resto si corregge nell'editor.

### 7.4 File extra, LRC e cover

- Ogni file non audio, eccetto un LRC associato, diventa un allegato e si materializza sotto `Extras/` mantenendo il percorso relativo.
- Un `.lrc` si associa soltanto se nella stessa directory esiste esattamente una traccia con lo stesso stem, confrontato con NFC e casefold. Ambiguità: errore esplicito. Senza corrispondenza resta un allegato.
- La cover viene scelta fra JPEG/PNG validi: nella radice `cover.*`, poi `folder.*`, poi `front.*`; quindi la front cover incorporata più frequente. In assenza di front cover, la prima immagine incorporata valida. Pareggi per percorso normalizzato o hash, non per ordine di scansione.
- Se non si trova una cover valida, `cover_hash = NULL`.
- Un'immagine esterna scelta come cover resta anche allegato: `cover.jpg` è la cover gestita; `Extras/...` conserva il file ricevuto. Questa piccola duplicazione dell'output evita eccezioni e perdita delle scansioni originali.
- Le cover incorporate estratte e selezionate passano dal blobstore; tutte le immagini originali restano comunque nel blob audio intatto.
- `.cue` viene conservato come documento, non riscritto: può riferirsi ai nomi originali del rip.

### 7.5 Put dei blob

Un'unica primitiva, usata anche dagli upload:

1. copia in `work/blobs/<uuid>.tmp`, calcolando SHA-256 e size;
2. completa la scrittura, esegue `fsync` sul descriptor aperto, controlla `close`, rilegge e verifica hash/size;
3. crea le directory shard, sincronizzando le nuove directory e i relativi parent;
4. fissa il blob con `renameat2(RENAME_NOREPLACE)`;
5. sincronizza directory sorgente e destinazione.

Se il blob esiste già, verificare il contenuto esistente prima di scartare il temp. Se non corrisponde all'hash, restituire `corrupt_blob`: non sostituirlo automaticamente. Anche nel ramo già-esistente si sincronizzano file e directory prima di restituire successo: un put concorrente potrebbe aver appena eseguito il rename e non ancora gli fsync. L'assenza del GC rende innocua la concorrenza fra due put dello stesso hash.

La rilettura verifica i byte scritti e intercetta errori di copia; non è una prova che il supporto fisico sia sano, perché può leggere dalla cache.

I blob diventano durevoli **prima** che una transazione li referenzi. Un crash precedente al commit può lasciare blob non referenziati: restano, non si tenta una cancellazione automatica.

### 7.6 Commit dell'album e duplicati

L'import è atomico per album, non per intera collezione. Prima del commit ogni traccia viene decodificata completamente con `AudioDigest`, oltre a verificarne formato e tag: un file corrotto non entra come traccia supportata. Il digest è transitorio.

Una transazione sotto il lock del catalogo:

1. ricontrolla che il job non sia già completato;
2. registra i blob, risolve o crea l'artista;
3. crea album, tracce e allegati, revision 1;
4. prenota il percorso e accoda il render;
5. scrive `result_album_id` e marca il job import `done`.

L'artista esistente viene riusato solo se il nome è uguale dopo NFC, trim e casefold. Se nomi diversi collidono soltanto a causa della sanitizzazione del percorso, l'import fallisce con i due nomi: non si fondono artisti implicitamente.

Il fingerprint è SHA-256 del JSON UTF-8 compatto di una lista ordinata di triple `[percorso relativo originale, size intera, hash]` di tutti i file non ignorati del candidato, ordinata per byte UTF-8 del percorso. Serializzatore comune senza escape HTML, senza newline finale. Non include la root assoluta né i metadati corretti dopo l'import.

- Fingerprint già presente: `skipped` con riferimento all'album esistente; se nel cestino, proporre il ripristino.
- Stessi byte di un singolo file: deduplicazione silenziosa del blob.
- Nome di cartella occupato da altro album: import fallito, richiesta di titolo diverso. Nessun merge.
- Audio uguale ma tag o allegati differenti: non si deduce automaticamente un duplicato.

`import_fingerprint` rimane quello dell'import iniziale anche dopo le modifiche: riconosce la provenienza, non il catalogo corrente. Non esiste una funzione per clonare intenzionalmente lo stesso import identico nella v1.

Una conferma del commit persa non causa un secondo album: esito del job e creazione dell'album sono nella stessa transazione.

---

## 8. Audio, tag e immagini

### 8.1 File supportati

FLAC, MP3 e contenitori M4A con un solo stream audio AAC o ALAC. Niente DRM, audio multistream o video reale; immagini attached-picture sono ammesse. Un file che non si decodifica interamente non è supportato, anche se l'estensione è corretta.

L'helper TagLib offre tre operazioni tipizzate: inspect, extract-images, write-managed-tags. Input JSON limitato; output JSON o file estratti in una directory assegnata. Path sempre passati come argomenti, mai interpolati in una shell. L'helper non sceglie percorsi di output della libreria.

Lettura dei campi gestiti MP3: primo valore non vuoto in ID3v2, poi APE, poi ID3v1; conflitti vengono segnalati. Negli altri formati prevale il campo canonico della tabella seguente sugli alias. Non si dipende da precedenze implicite e modificabili della libreria.

### 8.2 Tag gestiti

| Significato | FLAC/Vorbis | MP3 ID3v2.4 | M4A |
|---|---|---|---|
| Titolo | TITLE | TIT2 | ©nam |
| Artista | ARTIST | TPE1 | ©ART |
| Artista album | ALBUMARTIST | TPE2 | aART |
| Album | ALBUM | TALB | ©alb |
| Traccia / totale | TRACKNUMBER / TRACKTOTAL | TRCK | trkn |
| Disco / totale | DISCNUMBER / DISCTOTAL | TPOS | disk |
| Anno | DATE | TDRC | ©day |
| Genere | GENRE | TCON | ©gen |
| Compilation | COMPILATION | TCMP | cpil |
| Cover | PICTURE front cover | APIC front cover | covr |

Totale tracce = massimo numero presente sul disco; totale dischi = massimo numero presente nell'album. I buchi nella numerazione sono ammessi e mostrati nell'editor; non si inventano tracce mancanti.

I campi gestiti sono riscritti completamente. Valore assente significa rimozione, non conservazione del tag originale. Una cover assente nel DB significa nessuna immagine incorporata nell'output.

Vengono rimossi gli alias noti dei campi gestiti per non lasciare valori discordanti: per esempio `ALBUM ARTIST`, `TOTALTRACKS`, `TOTALDISCS`, vecchi campi ID3 anno/data, genere numerico MP4 quando si scrive quello testuale. Vengono rimossi anche i campi di ordinamento dei quattro nomi gestiti: title, artist, album artist, album. La lista precisa per formato è una tabella costante dell'adapter, coperta da fixture; non è configurazione dell'utente.

### 8.3 Tag non gestiti

Non si ricostruisce il file da zero e non si azzera la mappa dei tag. Testi incorporati, ReplayGain, compositore, commenti, identificativi e altri campi restano dove sono, salvo le regole esplicite seguenti.

Per MP3:

- ID3v2 viene scritto in versione 2.4.
- Gli eventuali APE vengono conservati per i campi non gestiti; si eliminano soltanto chiavi gestite, cover e sort corrispondenti. Non si elimina indiscriminatamente il contenitore APE.
- ID3v1 viene rimosso. Il suo commento non vuoto viene prima mantenuto in un frame COMM: se identico a uno già presente non si duplica, altrimenti descrizione `legacy-id3v1`. Gli altri campi ID3v1 sono gestiti dal DB.

La preservazione è semantica per i campi decodificati, non una promessa di identici byte del contenitore tag. I campi opachi che l'adapter non può risalvare senza perdita provocano un errore di render, non una perdita silenziosa. Il confronto dei tag non gestiti prima/dopo esclude soltanto campi gestiti, sort rimossi e migrazione ID3v1 dichiarata.

Non si promette compatibilità ID3v2.4 con vecchie autoradio. È una scelta di prodotto, non un toggle per formato.

### 8.4 Verifica audio: campioni, non soltanto pacchetti compressi

Non si usa l'hash dei soli packet come prova di identità del suono.

La funzione `AudioDigest`:

1. valida un singolo stream e ne acquisisce sample rate, canali e layout normalizzato;
2. decodifica con la versione fissata di ffmpeg in PCM `f64le`, senza cambiare rate, canali o layout e senza filtri audio;
3. calcola in streaming SHA-256 dei campioni e numero di frame;
4. restituisce `(sample_rate, channels, layout, frame_count, pcm_sha256)`.

Si usa `pcm_f64le` per non ridurre i file interi a 16 bit o introdurre una quantizzazione a 32 bit dei campioni float. Errori di decodifica o output vuoto sono errori. Il comando usa `-nostdin`, log di errore, `-xerror`, `-err_detect explode`, `-map 0:a:0`, `-vn -sn -dn`, `-c:a pcm_f64le`, `-f f64le`; codec e filtri limitati a un thread. Non si usano `-ar`, `-ac` o normalizzazioni.

La garanzia è **identica sequenza di campioni e parametri decodificati da quel decoder**, compreso il numero di campioni. Non è una garanzia sul comportamento di qualsiasi player o sui byte del contenitore. I test includono AAC/MP3 gapless e ALAC.

Per ogni render si confrontano digest della copia prima e dopo i tag, usando la stessa versione dei tool e gli stessi parametri. Il numero di frame è `byte PCM / (8 × canali)` e deve essere intero; un layout non dichiarato si rappresenta con un valore esplicito `unknown:<canali>`, uguale in entrambi i confronti. Non si salvano digest audio persistenti nel DB: niente migrazioni degli hash né cache da invalidare. Il costo di due decodifiche è accettato e distribuito fra i worker.

### 8.5 Cover e limiti

Cover accettate: JPEG e PNG, massimo 20 MiB e 40 milioni di pixel, decodifica valida. I byte dell'immagine vengono conservati senza conversioni e usati sia come file esterno sia come unica cover incorporata.

Altri formati immagine rimangono allegati, non cover selezionabili. Nessuna generazione di miniature o download online nella v1.

I subprocessi hanno cancellazione tramite context, timeout di 30 secondi per inspect/tag/immagini e 30 minuti per decodifica di una traccia, stderr limitato a 64 KiB. Decoder, encoder e filtri ffmpeg ricevono ciascuno il proprio limite a un thread. Un timeout è un errore esplicito del job. Non si interpreta stderr per dichiarare successo dopo un exit code non zero.

Ogni tool ha un process group controllato e death signal Linux alla perdita del parent. Si termina e si attende tutto il gruppo su cancellazione. I tool ricevono solo i descriptor necessari e non ereditano il flock. Anche un arresto brutale dell'app non deve lasciare helper che lavorano sullo staging del tentativo precedente.

---

## 9. Render e pubblicazione

### 9.1 Costruire una directory completa

Ogni tentativo riceve un `build_id` nuovo e usa `work/render/<build_id>/album`.

1. Caricare lo snapshot coerente (§6).
2. Calcolare il piano: path, tag attesi, blob, allegati, cover, LRC. Validare tutti i conflitti prima delle copie.
3. Se l'album è cancellato, il piano di pubblicazione è una rimozione e non contiene file nuovi.
4. Altrimenti costruire **tutto** l'album. Non leggere mai `library/` come sorgente.
5. Per ogni copia calcolare il SHA-256 del blob letto e confrontarlo con il suo nome. Un originale corrotto interrompe il lavoro.
6. Per ogni traccia: copia verificata -> digest audio prima -> scrittura tag -> rilettura dei tag gestiti e non gestiti -> digest audio dopo. Devono coincidere i valori attesi e l'audio.
7. Per cover, allegati e LRC: copia byte per byte, verificata.
8. Calcolare hash completi e size dei file finali, scrivere la ricevuta; `fsync` di tutti i file e directory nuove, dal basso verso l'alto, incluso il parent dello staging.
9. Entrare nel protocollo di pubblicazione.

Un fallimento prima del journal lascia intatta la directory pubblicata. Lo staging può essere rimosso.

Niente `file_map`, `spec_hash`, DIFF per file o riuso di tracce taggate. Si riscrive un album intero solo quando cambia, viene forzato o cambia `render_version`. Il costo è prevedibile; l'assenza di stati intermedi per singolo file è il vantaggio principale del design.

### 9.2 Ricevuta derivata

`.musiclib.json` contiene esclusivamente:

```text
schema_version = 1
album_id
build_id
album_revision
render_version
files = [{relative_path, size, sha256}, ...] ordinati per path
```

Niente nomi di artisti, titoli o metadati modificabili. La ricevuta non elenca se stessa e non contiene timestamp. Il suo SHA-256 è registrato nel journal e, a pubblicazione completata, nell'album.

Serve a riconoscere una directory già installata dopo un crash e a verificare l'output. Si rigenera come gli altri file; non è una fonte di verità del catalogo. I file musicali devono essere deterministici a parità di input/versione; il `build_id` della ricevuta cambia a ogni tentativo.

### 9.3 Tre fasi, una sola pubblicazione alla volta

Ogni worker può costruire in parallelo. Per pubblicare acquisisce `publishMu`, un mutex di processo. L'ordine dei lock è sempre `publishMu -> transazione con catalog lock`; le API acquisiscono solo il catalog lock, mai `publishMu`.

Prima del journal, un preflight sotto `publishMu` controlla staging, ricevuta, destinazioni e ownership osservabile sul disco. Un conflitto già rilevabile fallisce il solo job, senza impegnare il journal globale. La successiva transazione ricontrolla che lo snapshot sia ancora attuale.

**A. PREPARE — transazione DB breve**

- Ricontrollare ticket, revisione, versione e prenotazioni.
- Se superato: rimettere pending, non creare il journal, scartare la build.
- Altrimenti inserire la singola riga `publication`, con vecchio e nuovo percorso, revisione costruita, build e hash della ricevuta.
- Commit. Solo dopo commit certo si possono modificare i percorsi pubblicati.

**B. INSTALL — filesystem, nessuna transazione SQL aperta**

- Nuovo percorso assente: creare/sincronizzare i parent e spostare lo staging con `RENAME_NOREPLACE`.
- Stesso percorso già pubblicato dall'album: scambiare directory finale e staging con `RENAME_EXCHANGE`. Il vecchio album ora è nello staging.
- Rinomina a percorso diverso: prima installare il nuovo album con `RENAME_NOREPLACE`, poi spostare la vecchia directory in `work/retired/<build_id>`.
- Cancellazione: spostare soltanto la directory vecchia in `work/retired/<build_id>`.
- Se la vecchia directory manca già, il suo ritiro è completato. Non è un motivo per cancellare altro.
- Usare `RENAME_NOREPLACE` anche per il ritiro. `old_path == new_path` si confronta sui path esatti, non sulle chiavi casefold.
- Tentare `rmdir` della vecchia directory artista, se ora vuota, ancora sotto `publishMu`: nessuna pulizia di parent nella libreria può correre contro un altro publisher. Un parent non vuoto resta; errori di questa pulizia opzionale sono avvisi.
- Sincronizzare tutte le directory coinvolte nei rename/rmdir, incluse quelle nuove e i relativi parent.

Un conflitto con una directory nuova estranea o con una ricevuta di un altro album non autorizza la sostituzione. Nel percorso già registrato come pubblicato per l'album, una ricevuta mancante o un file alterato sono invece danni dell'output: la build può sostituire l'output posseduto. Symlink e file speciali sono sempre rifiutati.

**C. FINALIZE — transazione DB breve**

- Registrare `published_*` usando **i valori del journal**, o path/build/receipt NULL per la rimozione.
- Completare il job secondo il confronto dei ticket (§6).
- Eliminare la riga `publication` e riallineare le prenotazioni alla loro unione attuale.
- Commit certo, poi rilasciare `publishMu`.

Solo adesso eliminare ricorsivamente staging vecchio e directory ritirata, fuori dal mutex. Un errore di questa pulizia è un avviso operativo, non rende fallita una pubblicazione già riuscita. La pulizia viene ripresa al boot.

### 9.4 Recovery: completare in avanti, non scambiare alla cieca

All'avvio nessun worker parte prima del recovery. L'eventuale journal è la prima cosa da risolvere.

- Se `new_path` è NULL, è una rimozione: non si cerca uno staging, si passa direttamente al ritiro del vecchio percorso.
- Altrimenti, se `new_path` contiene una ricevuta con `build_id` e hash attesi, il nuovo album è già installato: **non ripetere l'exchange**.
- Negli altri casi deve esistere nello staging la build attesa. Si esegue l'installazione prevista, controllando ownership e destinazioni.
- Per il vecchio percorso, assenza significa già ritirato. Se presente e diverso dal nuovo, ritirarlo una sola volta. Una directory retired già presente non viene sovrascritta.
- Ripetere gli fsync necessari e FINALIZE. È lecito eseguire recovery più volte.
- Solo dopo il journal risolto, pulire `work/` e recuperare i job running.

Se lo stato osservato non corrisponde a nessuna transizione lecita, la pubblicazione viene sospesa e l'errore esposto. Non si indovina quale directory cancellare. `rebuild` offline (§11) è il recupero universale dell'output derivato.

Un errore dopo PREPARE non diventa un semplice `job failed` seguito da altre pubblicazioni: il journal resta prioritario fino al completamento o al rebuild esplicito. Questo evita che una seconda operazione renda ambigua la prima.

### 9.5 Concorrenze risolte

| Caso | Comportamento |
|---|---|
| Due worker reclamano lo stesso render | Impossibile: una riga per album, claim con row lock e stato running |
| API modifica un album durante la copia | Ticket/revisione cambiano; build vecchia scartata prima di PREPARE |
| API modifica dopo PREPARE | Journal completato sulla sua revisione; richiesta successiva resta pending |
| Due album chiedono lo stesso nome | Transazione e `path_claims` accettano un solo owner |
| Un nome viene riusato durante una rinomina | Resta prenotato fino al ritiro della vecchia directory |
| Artista rinominato mentre si costruiscono più album | Tutte le revisioni cambiano; ciascun album converge tramite il proprio job |
| Album cancellato e subito ripristinato | Ticket più recente prevale prima di PREPARE, oppure segue la cancellazione già preparata |
| PostgreSQL cade fra rename e FINALIZE | Journal già durevole; recovery riconosce la build installata |
| Processo muore dopo exchange | Ricevuta distingue il nuovo dal vecchio; niente secondo exchange inverso |

---

## 10. API e interfaccia

### 10.1 Convenzioni

API sotto `/api`, JSON UTF-8, errori `{code, message, details}`. Nessun path assoluto in richieste o risposte. Payload JSON massimo 16 MiB; rifiutare campi sconosciuti, ID duplicati e più oggetti JSON nello stesso body.

Le rappresentazioni del catalogo artista e album espongono una revisione e un ETag forte della forma `"album:<uuid>:<revision>"` o `"artist:<uuid>:<revision>"`. Contengono solo dati desiderati; lo stato mutevole di elaborazione è una risorsa separata, senza ETag di catalogo. L'output JSON del dettaglio ha ordine deterministico. Ogni modifica a una risorsa esistente richiede `If-Match`; senza precondizione `428`, revisione vecchia `412` con invito a ricaricare. Il controllo avviene nella stessa transazione della modifica. Niente last-write-wins fra due schede del browser.

`409` per conflitti di nomi/prenotazioni, `422` per metadati o contenuti non validi, `413` per limiti, `503` durante boot/recovery o indisponibilità del DB. Non esporre query SQL e stderr completo all'utente.

### 10.2 Endpoint v1

| Endpoint | Significato |
|---|---|
| `GET /api/albums` | ricerca per titolo/artista, filtro artista/cestino, paginazione 50 max 200 |
| `GET /api/albums/{id}` | aggregato desiderato completo, revisione ed ETag |
| `GET /api/albums/{id}/status` | revisione desiderata/pubblicata, versione renderer, job; Cache-Control: no-store |
| `PUT /api/albums/{id}` | metadati album e tracce in una transazione; blob e ID non sono modificabili dal body |
| `DELETE /api/albums/{id}` | cestino |
| `POST /api/albums/{id}/restore` | ripristino |
| `POST /api/albums/{id}/render` | enqueue forzato senza cambiare metadati |
| `DELETE /api/albums/{id}/tracks/{track}` | rimozione confermata di una traccia, minimo una rimanente |
| `GET /api/artists` / `POST /api/artists` | elenco/creazione; conflitto restituisce anche l'artista esistente |
| `GET /api/artists/{id}` | nome, revisione ed ETag, senza contatori derivati |
| `PUT /api/artists/{id}` | rinomina atomica nel catalogo, enqueue degli album |
| `PUT /api/albums/{id}/cover` | upload JPEG/PNG oppure scelta di un allegato immagine dello stesso album |
| `DELETE /api/albums/{id}/cover` | cover assente, anche nei tag futuri |
| `POST /api/albums/{id}/attachments` | upload di un allegato con percorso relativo |
| `DELETE /api/albums/{id}/attachments/{attachment}` | rimozione dal catalogo/output |
| `PUT` / `DELETE /api/albums/{id}/tracks/{track}/lyrics` | assegna/carica o rimuove LRC |
| `GET /api/import-source?path=...` | elenco confinato a /import, senza leggere arbitrariamente l'host |
| `POST /api/imports` | crea/riprende batch tramite UUID di richiesta |
| `GET /api/imports/{id}` | resoconto scan e import per candidato |
| `GET /api/jobs` | pending/running/failed con errori comprensibili |
| `POST /api/jobs/{id}/retry` | nuovo ticket; per import ammette gli override definiti in §7 |
| `POST /api/jobs/retry-failed` | ritenta tutti i falliti, senza duplicare running |
| `POST /api/render-all` | accoda tutti gli album attivi e cancellazioni ancora da materializzare |

`PUT /api/albums/{id}` accetta `{artist_id, title, year, genre, compilation, tracks: [{id, disc, no, title, artist, genre}]}`. La lista deve contenere esattamente le tracce correnti: non è un comando implicito di aggiunta/cancellazione. Per album attivi il dominio valida l'intero aggregato prima del commit.

Gli endpoint relativi a cover/allegati/tracce richiedono l'ETag **dell'album**. I render manuali richiedono anch'essi la revisione vista, pur non incrementandola. Le operazioni di retry non modificano il catalogo e sono idempotenti mentre il job è già pending/running.

Upload: il blob viene fissato prima della transazione con If-Match. Se il salvataggio fallisce resta un blob non referenziato, non un riferimento rotto. Allegati massimo 256 MiB, LRC 2 MiB e testo UTF-8 valido. Il limite è dichiarato nella UI prima dell'invio.

Per scaricare originali/allegati si usano endpoint per ID dell'entità, mai un endpoint che legge un pathname ricevuto dal client. I file arbitrari sono serviti come attachment, con `nosniff`; solo JPEG/PNG validati sono mostrati inline. PDF scaricabile e apribile nel browser, senza viewer incorporato dedicato.

### 10.3 UI minima

Quattro viste: **Libreria**, **Album**, **Import**, **Attività**. Il cestino è un filtro della libreria.

- Editor album: campi album, cover, tabella tracce, elenco allegati. Un pulsante Salva per i metadati; nessun autosave campo per campo.
- Il selettore artista permette di scegliere una riga esistente o crearne una. Un conflitto non avvia un merge.
- Campi ereditati riconoscibili e comando esplicito «eredita» per artista/genere.
- Stato: Allineato / In coda / In elaborazione / Errore. Per album attivi, Allineato richiede revisione e renderer pubblicati correnti e nessun job. Un album nel cestino senza percorso pubblicato e senza job è Archiviato; altrimenti mostra l'attività di rimozione.
- «Allineato» descrive il lavoro dell'applicazione, non una verifica continua del disco: quella spetta a doctor.
- Polling ogni 2 secondi soltanto nelle pagine con attività; niente WebSocket o SSE.
- Errori mantengono i valori del form; `412` non sovrascrive i dati più recenti.
- Cancellazioni e rimozioni richiedono conferma; per gli album si esplicita il ripristino, per tracce/allegati l'assenza di undo.

### 10.4 Confine di sicurezza

Servizio per un singolo utente su macchina/rete fidata. Compose pubblica per default su `127.0.0.1`; l'accesso LAN richiede un binding esplicito. Niente esposizione Internet senza un reverse proxy autenticato, fuori scope dell'app.

`PUBLIC_ORIGIN` è obbligatorio e valida Host e, se presente, Origin; `Origin: null` o un'origine diversa vengono rifiutati. Client non-browser possono omettere Origin, non l'header richiesto. Le richieste mutanti richiedono un header `X-Musiclib-Request: 1`; la UI lo imposta con fetch. Non si abilita CORS. I template fanno escaping; i nomi dei file non diventano HTML.

L'accesso al filesystem è confinato alle root: primitive basate su directory descriptor e `openat2` con `RESOLVE_BENEATH | RESOLVE_NO_SYMLINKS`, oppure primitive equivalenti con la stessa garanzia testata. Niente `Join(root, input)` seguito da operazioni fiduciose sul path. La sanitizzazione dei nomi non sostituisce il confinamento.

---

## 11. Operatività

### 11.1 Configurazione e avvio

Configurazione solo da ambiente: `DATABASE_URL`, `PUBLIC_ORIGIN`, `HTTP_ADDR` (container default `:8080`), `WORKERS`. Percorsi interni fissi `/data` e `/import`. UID/GID del processo scelti nel Compose; niente esecuzione come root, umask 022. Compose usa `init: true` e `restart: unless-stopped` per l'app.

PostgreSQL mantiene `fsync`, `full_page_writes` e `synchronous_commit` attivi. Il pool pgx ha al massimo `WORKERS + 8` connessioni. Le transazioni sui cataloghi devono essere brevi e senza tool/media I/O.

Ordine di avvio:

1. aprire HTTP con readiness negativa e acquisire il flock;
2. attendere PostgreSQL, applicare migrazioni prima dei worker;
3. verificare identità del volume, assenza del marker di manutenzione, root, permessi, st_dev e primitive richieste;
4. recuperare il journal;
5. pulire work non più referenziato, recuperare job running;
6. accodare album attivi il cui renderer pubblicato differisce da quello corrente, solo se non hanno già un job; un failed esistente resta esplicito, non viene ritentato a ogni boot;
7. avviare pool e rendere positiva la readiness.

Alla prima inizializzazione si crea `settings.store_id` in transazione e poi il marker di volume con scrittura durevole no-replace. Un marker assente si può completare soltanto su storage media vuoto; un marker discordante non viene mai riscritto automaticamente. La procedura è ripetibile se il primo avvio si interrompe.

`/health/live` dice che il processo risponde; `/health/ready` verifica boot/recovery completati e disponibilità DB. Log JSON con album_id, job_id e build_id. Niente contenuto dei tag o credenziali nei log per default.

Shutdown: smettere di accettare mutazioni/claim, cancellare le costruzioni, terminare i processi figli; una pubblicazione già preparata tenta di concludere entro 30 secondi, altrimenti lascia il journal al recovery. Non si libera il flock prima che nessun worker possa più pubblicare.

### 11.2 Spazio

A regime: originali + libreria, circa 2 volte i media, più spazio esterno per il backup. Il picco aggiunge staging degli album simultanei, vecchie directory non ancora ripulite, cover e tag.

Prima di import/build, stima conservativa e controllo `statfs` con margine di 1 GiB. Un budget di spazio di processo prenota atomicamente le stime dei lavori attivi, così due worker non spendono lo stesso spazio libero. Le prenotazioni sono in memoria: dopo un crash si ricostruiscono dopo la pulizia dei temp.

Non è una garanzia contro scritture esterne o dimensioni finali inattese: ogni write gestisce comunque `ENOSPC`. Nessun file pubblicato viene rimosso per tentare di fare spazio.

### 11.3 Manutenzione offline

`doctor`, `rebuild`, `backup` e `restore` richiedono il lock esclusivo del volume: si eseguono con l'app fermata e PostgreSQL acceso. Nessuna seconda istanza di worker, nessuna scansione che confonda stati transitori con corruzione.

```text
docker compose stop app
docker compose run --rm app doctor --deep
docker compose start app
```

I sottocomandi non avviano il server. Doctor non risolve né cancella il journal: se presente lo segnala e indica di avviare il recovery o scegliere rebuild.

**Doctor normale:** esistenza e size dei blob referenziati; integrità strutturale di DB, prenotazioni, ricevute e file attesi; ricevuta confrontata con l'hash nel DB; file extra nell'output segnalati; job/renderer non allineati segnalati come lavoro pendente, non come danno.

**Doctor deep:** anche SHA-256 di tutti i blob presenti e di tutti i file elencati nelle ricevute. Include tag, cover e allegati, non soltanto l'audio. Non serve ridecodificare la libreria: gli hash finali ancorati nel DB rilevano alterazioni successive alla verifica del render.

Blob non referenziati: informazione, non errore, non cancellazione. Un danno in `originals/` richiede una copia buona dal backup; rigenerare l'output non ripara il blob.

**Rebuild:** dopo conferma esplicita dello `store_id`, crea e sincronizza `/data/.maintenance` con operazione e store_id. Poi elimina esclusivamente `library/` e `work/` nel volume riconosciuto; in una transazione azzera journal e campi pubblicati, elimina i vecchi job render e tutte le prenotazioni, ricrea le prenotazioni desiderate e accoda nuovi render degli album attivi. Ricrea/sincronizza le directory vuote e solo alla fine rimuove durevolmente il marker. Catalogo, blob e resoconti import restano. Dopo un'interruzione si ripete lo stesso comando; finché esiste il marker il boot non avvia i worker. Non è un rollback dei metadati.

### 11.4 Backup e restore

La v1 sceglie il **backup offline dell'applicazione**, non un protocollo di snapshot distribuito. Per una libreria personale, una finestra di manutenzione è più semplice e verificabile.

`backup --to /backup/<nome>` con app ferma:

1. acquisire flock; creare una directory temporanea nuova nella destinazione;
2. eseguire `pg_dump` in formato custom, senza owner/ACL;
3. copiare tutti gli originali, verificandone hash e size durante la copia;
4. scrivere manifesto con store_id, versione schema, versione app, hash del dump ed elenco dei blob;
5. sincronizzare e rinominare la directory di backup a nome definitivo solo a successo completo.

Niente overwrite di un backup precedente. La destinazione deve essere esterna a `/data`, preferibilmente su un altro dispositivo. Retention delle copie esterne amministrata dall'utente: mantenere più generazioni, non una sola copia continuamente sovrascritta.

Il backup protegge **import completati**, non sorgenti esterne ancora da importare. Job scan/import non terminali inclusi nel dump saranno marcati failed al restore con richiesta di verificare/rimontare la sorgente e riprovare. Non si copiano `/import`, `library` o `work`.

`restore --from ...` richiede database e volume dati nuovi/vuoti, escluso il file del lock; rifiuta di sovrascrivere un'installazione esistente. Crea il marker di manutenzione, verifica manifesto e file, ripristina il dump e gli originali, ricrea l'identità del volume, applica solo migrazioni forward supportate, azzera lo stato derivato come rebuild e prepara i render. Rimuove il marker soltanto dopo tutti i controlli. Se si interrompe, non si avvia l'app: si ricreano le destinazioni nuove/vuote e si ripete il restore, senza un secondo protocollo di resume.

Gli script Compose forniscono stop/run/start con quoting corretto e gestione degli errori. Il restore end-to-end su volumi nuovi è parte dei test di rilascio, non soltanto documentazione.

---

## 12. Test obbligatori

### 12.1 Livelli

- Unit test: normalizzazione, collisioni, planner, metadati ereditati, ordinamento naturale, fingerprint, mapping tag.
- PostgreSQL reale con testcontainers: vincoli, precondizioni, claim concorrenti, revisioni, prenotazioni e commit degli import. Non sostituire PostgreSQL con SQLite nei test.
- Test media con fixture redistribuibili: FLAC, MP3, AAC e ALAC, senza tag, cover multiple, ID3v1/APE, Unicode, date, tag non gestiti, gapless e input corrotti.
- Test Linux/ext4 delle primitive effettive: non mockare tutti i rename. In CI usare un volume ext4 di test per la suite di durabilità/recovery.
- Test UI essenziali: import, modifica con conflitto ETag, cover, cestino/ripristino, retry.
- `go test -race` per codice concorrente e shutdown. Il race detector non sostituisce i test di protocollo.

### 12.2 Matrice di guasto

Inserire failpoint nominati nel protocollo e testare arresti reali del processo, non soltanto errori restituiti dalle funzioni.

| Prova | Proprietà richiesta |
|---|---|
| Due worker sullo stesso job | Uno solo ottiene il claim |
| Due import dello stesso candidato / commit dalla risposta persa | Un album e un esito durevole, non duplicati |
| Stesso blob fissato contemporaneamente | Un file integro, nessun overwrite |
| Blob esistente corrotto | Errore, nessuna fiducia nel solo nome |
| Modifica API durante un build | Build obsoleta scartata, nuova richiesta conservata |
| Modifica fra PREPARE e FINALIZE | Revisioni distinte e secondo render ancora necessario |
| Riuso di un vecchio nome non ancora ritirato | Conflitto di prenotazione, mai furto di ownership |
| Scambio numeri traccia | Transazione valida senza collisioni intermedie |
| Crash prima/dopo PREPARE | Vecchio output valido oppure journal recuperabile |
| Crash subito dopo exchange, prima degli fsync, prima/dopo FINALIZE | Recovery non esegue uno scambio inverso |
| Crash tra installazione del nuovo path e ritiro del vecchio | Entrambi completi, recovery rimuove solo il vecchio owner |
| Disco pieno durante build | Vecchio album e originali intatti |
| Errore DB dopo rename | Journal completato al riavvio |
| Tag writer altera i campioni o perde un tag non gestito | Album non pubblicato |
| Modifica dell'output con size/mtime invariati | Doctor deep la rileva e render/rebuild la ripara |
| Symlink, path assoluti, traversal, collisione file/directory | Nessuna operazione fuori root, errore prima della pubblicazione |
| Due finestre UI salvano revisioni diverse | Una riceve 412, nessuna modifica persa silenziosamente |
| SIGTERM/SIGKILL con più worker e helper attivi | Nessun helper del vecchio tentativo resta in attività |
| Crash durante rebuild o restore | Marker impedisce il boot su una manutenzione incompleta |
| Backup, perdita DB/volume, restore su volumi nuovi | Catalogo e originali recuperati, output rigenerato verificabile |

### 12.3 Criterio di completamento

La v1 è pronta quando una collezione di test con album normali, multidisco, compilation, edizioni distinte, cover e allegati può essere importata, modificata, interrotta e ripristinata senza correzioni manuali del DB o dei registri interni.

Nessuna funzionalità viene dichiarata supportata solo perché la libreria sottostante espone un metodo. Prima passa i test del contratto.

---

## 13. Ordine di implementazione e stile

### 13.1 Sequenza

1. **Fondamenta:** Compose/PostgreSQL, migrazioni, lock del volume, normalizzazione, blobstore e test.
2. **Prima fetta verticale:** un album FLAC, import da `/import`, catalogo, build completa, journal, recovery. Già con due worker nei test.
3. **Concorrenza:** coalescenza, snapshot, prenotazioni, API condizionali, rinomine e failpoint. Nessuna UI ricca prima che il protocollo regga.
4. **Formati e contenuti:** MP3/AAC/ALAC, cover, LRC, allegati, verifica e preservazione dei tag.
5. **Esperienza completa:** scansione ricorsiva, resoconti, UI, ricerca, cestino e retry.
6. **Operatività:** doctor, rebuild, backup/restore e guida pratica con esempi Compose.

Ogni fase lascia un percorso end-to-end testabile. La prima fetta non introduce un renderer provvisorio incompatibile con il protocollo finale.

### 13.2 Regole per chi implementa

- Funzioni piccole con input/output espliciti; strutture concrete, interfacce soltanto ai confini utili ai test.
- Una sola implementazione di normalizzazione, put dei blob, enqueue e pubblicazione.
- Il planner non fa I/O. Lo store non conosce percorsi assoluti. L'helper tag non conosce il dominio.
- Tutte le scritture di catalogo passano dai servizi transazionali, non da handler o worker con SQL improvvisato.
- Errori tipizzati e contesto aggiunto lungo la catena; niente errori ignorati, in particolare close, fsync, rename e commit.
- Context e shutdown fanno parte delle firme delle operazioni lunghe.
- Non aggiungere cache, parallelismo interno alle tracce, microservizi, code esterne o configurazioni speculative.
- Le ottimizzazioni devono mantenere il protocollo e nascere da misure. Non reintrodurre un DIFF per file per risparmiare una copia prima di dimostrare che è necessario.
- Test e documentazione cambiano insieme a qualsiasi modifica delle garanzie.

**Idea guida finale:** PostgreSQL decide cosa deve esistere; worker paralleli preparano album completi; un protocollo piccolo e recuperabile li rende visibili. Gli originali restano fuori da ogni operazione di correzione. È questa separazione, non il numero di meccanismi, a rendere il sistema semplice e affidabile.
