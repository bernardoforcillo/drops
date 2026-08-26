# Roadmap: rendere drops un'alternativa vera, restando una libreria Go

> Questo documento non è una lista di feature di Drizzle da inseguire. Drizzle è il
> metro delle **capacità**, mai della **forma dell'API**. Il competitor reale di drops
> è GORM, ent, sqlc, bun, sqlx e squirrel. La domanda che decide ogni voce qui dentro
> è una sola: **uno sviluppatore Go rifiuterebbe drops per questo?**

---

## 1. Dove siamo

### Cosa drops fa già meglio di Drizzle *e* degli incumbent Go

Questa parte non è marketing: è materiale verificato nel repository e va usato come
argomento di posizionamento, perché nessuno dei sei concorrenti Go ce l'ha.

| Area | Cosa c'è | Chi altro ce l'ha |
|---|---|---|
| **Zero dipendenze nel modulo root** | Enforced in CI (`go mod tidy` + `git diff --exit-code`), driver isolati in `integration/go.mod` | Nessuno. GORM, ent, bun trascinano alberi di dipendenze |
| **Pattern di produzione in-tree** | outbox transazionale con drain parallelo e advisory lock per-aggregato (`pg/outbox.go`), saga, event store, idempotency key, changefeed, backfill online con lag-gating, sharding, replica routing LSN-aware | Nessun ORM, in nessun ecosistema |
| **Migrazioni a DAG** | `pg.TreeMigrator`: branch, `-- drops:parents:`, checkout, heads, tamper detection SHA-256, advisory lock con re-check dentro il lock | goose/atlas/golang-migrate/ent sono tutti lineari. Drizzle 1.0 sa solo *rilevare* un fork |
| **Interop con drizzle-kit** | Snapshot v7 byte-compatibile + `DrizzleMigrator` che parla il protocollo `drizzle.__drizzle_migrations` | Nessuno |
| **Multi-tenancy e authz come predicati** | `ScopeByTenant`, `Guard` a un metodo, `OwnerGuard`/`MembershipGuard`/`AnyOf`/`AllOf`, `Unscoped()` esplicito | Solo ent, e con un intero pipeline di codegen |
| **Analisi di sicurezza delle migrazioni** | 12 regole pg + 12 mysql + 11 sqlite + 9 lock-risk, con `Rule` stabile e `Suggestion` | Solo atlas, come binario separato con un linguaggio di policy |
| **Normalizzazione via server** | `renormaliseExpressions`: le CHECK e i predicati di indice vengono rispellati da PostgreSQL prima del confronto | Nessuno. GORM non vede proprio le CHECK |
| **Paginazione keyset di serie** | cursori opachi base64, NULLS ordering esplicito, forward/backward, decode fallito che rende `FALSE` invece di iniettare | Tutti gli altri la fanno scrivere a mano, e quasi tutti sbagliano i NULL |
| **Ampiezza dialetti** | pg (+pgvector, PostGIS), mysql, sqlite, clickhouse (MergeTree/PREWHERE/FINAL/ASOF), qdrant, mirror pg→CH+Qdrant | Drizzle copre 3 motori relazionali; ent e GORM solo relazionale |
| **Diagnostica allo startup** | `mustIdent`, drift check bidirezionale di `NewEntity` che elenca le colonne non legate *e* la chiamata di deroga da scrivere | GORM fallisce alla richiesta sfortunata in produzione |
| **Doc comment con SQLSTATE** | 42601, 42P02, 42P10, 42704, MySQL 1170 citati nei commenti accanto al design che li previene | Nessuno |

### Le 4 cose che fanno chiudere il tab nei primi cinque minuti

1. **Non esiste un comando.** `cmd/drops/main.go` dispatcha esattamente `diagram`,
   `version`, `help`. `pg.Push`, `pg.Introspect`, `pg.NewMigrator`, `pg.GenerateMigration`
   sono raggiungibili solo scrivendo e mantenendo un proprio `main.go` — e il readme lo
   dice. Chi confronta drops con `goose up`, `atlas migrate diff` o `sqlc generate`
   smette qui, prima di giudicare l'API.

2. **`git tag -l` non restituisce nulla.** Zero tag, `changelog.md` che annuncia cinque
   release fino a `[0.6.0]`, e il binario che stampa `0.4.0`. Tre risposte diverse alla
   domanda "che versione è". Un modulo senza tag è indistinguibile da uno abbandonato,
   e `go get` risolve a una pseudo-versione.

3. **Feature costruite fino all'interfaccia e mai collegate.** `pg.Copier`, `pg.Listener`,
   `pg.PoolStatsProvider`: tre capacità opzionali dichiarate, **zero implementazioni nel
   repository** — e `PoolStatsProvider` è strutturalmente insoddisfacibile da `*sql.DB`
   mentre `pg/poolstats.go` documenta il contrario. `AnalyzeMigration`,
   `AnalyzeMigrationRisks`, `HasDangerousMigration`: zero chiamanti fuori dai test.
   `Cols<T>()` e `Bind<T>()` emessi dal generatore: zero consumatori. Chi legge il
   sorgente lo nota, e generalizza.

4. **Otto bug di silent-wrongness in feature di punta.** Le relazioni eager-loaded
   bypassano il tenant scoping — `ScopeByTenant`, venduto come "non puoi dimenticare la
   WHERE tenantId", la dimentica alla seconda query. Il fast-scan generato decodifica
   posizionalmente da un `SELECT *`. Un `false` su una colonna con `DEFAULT true` viene
   silenziosamente scartato. `Push` propone `DROP TABLE` per ogni tabella che il
   `Schema` Go non dichiara. Un rename diventa DROP+ADD. Nessuno di questi dà errore.

E un quinto problema che non è una feature: **zero benchmark** in 164 file di test,
dietro un generatore di codice giustificato dalla performance e otto commenti in prosa
sull'allocation-consciousness.

**Nessuna di queste quattro cose è profonda.** La somma dei P0 qui sotto è settimane,
non mesi. Il problema è che finché ci sono, il resto — che è ottimo — non viene letto.

---

## 2. Il filtro: siamo una libreria Go

Queste sono le regole che hanno *effettivamente deciso* voci in questo documento.
Sono qui in cima perché senza di loro la sezione 5 sembra una scusa.

1. **Esplicito sopra implicito.** Se dal call site non si vede da dove arriva un valore,
   è sbagliato. Nessun registro globale, nessun file di config che computa valori,
   nessuna risoluzione a runtime dove basta un argomento.
   *Ha ucciso:* `drizzle.config.ts`, `casing`, i preset di ruoli Neon/Supabase,
   `pgTableCreator`.

2. **Gli errori sono valori. `ctx context.Context` è il primo parametro.** Niente panic
   in library code — l'unica eccezione già stabilita in casa è la *dichiarazione* di
   schema in `init` (`mustIdent`, il drift check di `NewEntity`). Sentinel + `errors.Is`.
   *Ha ucciso:* i prompt interattivi di rename, il panic di `Exprf` su arity sbagliata
   (→ deferred error), `MustCompile` sul path di richiesta.

3. **Interfacce piccole.** `drops.Driver` ha **tre** metodi. Le capacità extra sono
   interfacce opzionali duck-typed (il pattern di `pg.Copier`), mai metodi nuovi su
   `Driver`.
   *Ha ucciso:* `internal/eager` con la sua `Dialect` a 4 tipi cross-dialetto —
   un sistema di tipi ombra sopra quattro package deliberatamente duplicati.

4. **Zero dipendenze nel root è un fossato, non uno slogan.** Una dipendenza pesante vive
   in un submodulo con il suo `go.mod` (il precedente è `integration/go.mod`).
   Corollario che *non* vale: lo slogan non giustifica riscrivere pacchetti
   dell'ecosistema.
   *Ha ucciso:* `drops/fake` (word list per non dipendere da gofakeit). *Ha creato:*
   `pgxdriver/` come submodulo tagged.

5. **Codegen sopra reflection.** L'inferenza di TypeScript non ha equivalente Go. La
   risposta è `go:generate` + `cmd/dropsgen` che emette Go leggibile e committato.
   *Ha ucciso:* `$inferSelect`/`$inferInsert`, l'espansione di `AutoTable`.

6. **Generics solo dove comprano sicurezza a compile time.** `Col[T]`, `Entity[T]` sì.
   Type parameter per simulare tipi condizionali o structural typing: no.
   *Ha ucciso:* il tipo di ritorno "ristretto" dalla proiezione — in Go un campo non
   selezionato resta al valore zero e lo si documenta in una riga.

7. **Struct tag sì, seconda lingua di schema no.** `drop:"col"` è idiomatico
   (`encoding/json` ha fatto scuola). Un'espressione SQL dentro un tag no.

8. **Composizione e functional option**, non struct di config piene di `*bool`.

9. **Stare in piedi sulla stdlib.** `io/fs`, `log/slog`, `database/sql`, `flag`,
   `embed`, `net/http`, `testing`, `math/rand/v2`, `encoding/json`.
   *Ha ucciso:* i `codecs` di Drizzle (esiste `driver.Valuer`/`sql.Scanner`), cobra,
   goreleaser, un framework di matcher per i test.

10. **Le catene builder ritornano tipi concreti e restano type-checked.** Un'API fluente
    che fallisce solo a runtime è un'abitudine TypeScript.
    *Ha ucciso:* `Stmt.One(db, ctx, dest, args ...any)` e il suo gemello `Compiled`.

11. **Il tooling è un singolo binario statico.** `embed.FS` + `net/http` se serve UI.
    Mai node_modules, mai un servizio ospitato.
    *Ha ridimensionato:* Studio → `drops diagram --format mermaid|dot`.

12. **La documentazione è godoc + `Example` eseguibili prima di tutto.** Un sito è
    additivo. Una prosa marcisce; un `Example` che smette di compilare no.

13. **Disciplina di compatibilità.** La promessa Go 1 è l'aspettativa culturale. Tag
    semver, niente breaking dentro una major, `/v2` quando inevitabile. **Corollario
    operativo:** ogni breaking change necessario va fatto *ora*, prima del primo tag.

14. **Sicurezza in concorrenza per costruzione.** Nessun goroutine leak, `ctx` onorato,
    stop func restituita al chiamante (il precedente è `StartPoolMetrics`).

**Regola zero, che non era nel set originale e che questo documento adotta:
si può anche togliere.** Novanta proposte tutte additive su 114k righe scritte in tre
mesi da un maintainer non sono una roadmap, sono un debito. Vedi §6.4.

---

## 3. I blocchi P0

Tredici voci, in tre blocchi. Il blocco A **non è una lista di feature: è un gate.
Nessun tag viene tagliato finché non è chiuso.**

### Blocco A — Gate di correttezza

> **Nota del 2026-08-21 — questa sezione è la specifica originale, non la
> documentazione dell'API.**
>
> Blocco A è stato scritto prima di implementare qualunque cosa, e sei round di
> lavoro sul dialetto pg lo hanno superato: gli sketch qui sotto in più punti
> nominano firme, tipi e nomi di funzione che non esistono o che si sono spostati
> (`writeOperand`, cancellato nel round 5, è solo il caso più visibile). Restano
> qui **apposta**: il valore del documento sta anche nel registrare che cosa era
> stato previsto, e riscriverli a posteriori cancellerebbe l'unica traccia di
> quali problemi erano stati visti in anticipo e quali no.
>
> Per sapere che cosa fa il codice **oggi** si leggono i doc comment del package,
> che sono la fonte di verità e vengono aggiornati insieme al codice:
> `pg/doc.go` per la mappa, `pg/tenant.go` per l'asse tenant (`ScopeByTenant`,
> `TenantFilter`, `ScopeWritesByTenant`), `pg/authz.go` per i `Guard`,
> `pg/table.go` per `DefaultFilter` / `ContextFilter` e per come vengono risolti,
> `pg/op.go` per la regola sugli operandi e `pg/select.go` (`resolveCtx`,
> `resolveExpr`) per il walk che porta lo scoping dentro le sotto-query.
>
> Gli **esiti utente** di ogni voce, invece, sono tuttora la specifica: sono
> formulati in termini di righe restituite, non di API, e non sono invecchiati.

#### P0-1. Il tenant scoping non arriva alle relazioni

**Esito utente.** Se un'entità dichiara `ScopeByTenant`, ogni riga che quell'operazione
restituisce appartiene al tenant sul `ctx` — comprese le righe raggiunte attraverso una
relazione. Oggi `UserEntity.Query(db).Load(UserPosts).All(ctx)` filtra gli utenti e
carica i post **di tutti i tenant**.

**Perché è il primo.** GORM, ent, sqlc, bun e squirrel non offrono tenant scoping.
`ScopeByTenant` + `Guard` + RLS è l'unica risposta strutturale di drops a "perché non
sqlc". Spedire un differenziatore che perde righe di altri tenant è peggio che non
spedirlo: un progetto di tre mesi, senza tag, con un autore, ha diritto a **un** evento
pubblico di credibilità, e "l'ORM multi-tenant ha fatto leak dei tenant" lo chiude
per sempre.

**Design.** `Table.DefaultFilter` è un `Expression` fisso iniettato in
`SelectBuilder.writeCore`, che non ha `ctx`. Serve un gemello risolto dagli *executor*,
che il `ctx` ce l'hanno. Poiché ogni query figlia di relazione passa da
`db.Select().From(rel.To)` e poi dagli stessi executor, un solo hook copre root,
relazioni, per-parent limit, UPDATE e DELETE.

```go
// pg/table.go

// ContextFilterFunc costruisce un predicato dal contesto di richiesta.
type ContextFilterFunc func(context.Context) (drops.Expression, error)

// ContextFilter registra un predicato risolto al momento dell'esecuzione.
//
// DefaultFilter viene reso in WriteSQL, che non ha ctx; questo viene
// risolto dagli executor (Rows/All/One/Count e i corrispettivi di
// UPDATE/DELETE) — ed è esattamente per questo che raggiunge le
// relazioni eager-loaded: le loro query figlie passano dallo stesso
// path All/Rows della query radice.
func (t *Table) ContextFilter(fn ContextFilterFunc) *Table {
	t.ctxFilters = append(t.ctxFilters, fn)
	return t
}

// pg/tenant.go

// TenantFilter è la ContextFilterFunc canonica: fallisce chiusa con
// ErrTenantMissing invece di eseguire una query non filtrata.
func TenantFilter(col ColRef) ContextFilterFunc {
	c := col.col()
	return func(ctx context.Context) (drops.Expression, error) {
		t, ok := TenantFrom(ctx)
		if !ok {
			return nil, ErrTenantMissing
		}
		return Eq(c, t), nil
	}
}
```

Uso, e SQL prodotto:

```go
Posts.ContextFilter(pg.TenantFilter(PostTenantID))

users, err := UserEntity.Query(db).Load(UserPosts).All(ctx)
// SELECT ... FROM "posts"
//   WHERE "userId" IN ($1,$2) AND "tenantId" = $3
```

`Unscoped()` deve azzerare anche i context filter, e dirlo nel doc comment.
Gli executor **copiano** `wheres` per esecuzione invece di appenderci sopra, o la regola
"immutable in spirit" salta (vedi P1-Q1). `ToSQL()` non mostra più lo statement completo:
va documentato e affiancato da `ToSQLCtx(ctx)`.

**File toccati.** `pg/table.go`, `pg/select.go`, `pg/update.go`, `pg/delete.go`,
`pg/tenant.go`, `pg/find.go`, `pg/entity.go`.
**Effort** L. **Rischio** la risoluzione si sposta da render time a execute time.
**Test obbligatorio:** asserzioni sull'**SQL renderizzato**, non round-trip — un
round-trip su fixture mono-tenant passa mentre perde.

**ent/sqlc/GORM.** ent copre le traversate con Interceptor + Privacy, ed è l'unico che
lo fa bene, al costo di un pipeline di codegen. GORM usa Scope globali per nome tabella,
aggirabili con una query raw. sqlc, bun e sqlx non hanno nulla.

#### P0-2. `Limit` su una relazione soft-deleted resuscita le righe cancellate

**Esito utente.** Aggiungere `p.Limit(5)` a un eager load non cambia *quali* righe sono
visibili. Oggi passa al path SQL scritto a mano di `buildPerParentLimitedSQL`, che non
applica mai i `defaultFilters` della tabella figlia.

```go
// pg/find.go — dentro buildPerParentLimitedSQL, dopo il predicato IN
if !node.unscoped {
	for _, df := range rel.To.defaultFilters {
		b.WriteString(" AND ")
		df.WriteSQL(b)
	}
}
for _, w := range node.wheres {
	b.WriteString(" AND ")
	w.WriteSQL(b)
}

// Unscoped rimuove i default filter della tabella target per questo
// solo arco. Lo scoping della query radice non è toccato: un arco
// Unscoped sotto un padre filtrato è un allargamento locale e voluto.
func (c *RelConfig) Unscoped() *RelConfig {
	c.node.unscoped = true
	return c
}
```

**File.** `pg/find.go`, `pg/rellimit_test.go`, `pg/mixin.go`. **Effort** S. **Rischio**
minimo. La cura profonda è esprimere il rewrite `ROW_NUMBER()` come `SelectBuilder` su
subquery, così eredita il filtraggio come tutto il resto — la stringa scritta a mano è
la causa radice.

**ent/sqlc/GORM.** bun costruisce le query di relazione come veri `*bun.SelectQuery`,
quindi il problema non esiste per costruzione. È la lezione da portare a casa.

#### P0-3. Il fast-scan generato legge posizionalmente da un `SELECT *`

**Esito utente.** Chi lancia `go generate` per avere lo scan senza reflection può fidarsi
che la riga sia finita nei campi giusti. Oggi `pg/entity.go` rende `db.Select().From(e.table)`
— cioè `SELECT *` — e passa le righe a `e.fastScan`, che decodifica per posizione. Un
`ALTER TABLE ADD COLUMN` in mezzo alla tabella sposta ogni valore successivo di un campo,
senza alcun errore. `Cols<T>()` è già emesso dal generatore ed è codice morto.

```go
// pg/entity.go

// SetFastScan registra uno scanner posizionale generato insieme alla
// lista di colonne per cui è stato generato. La lista è ciò che gli
// executor SELECT rendono, quindi l'ordine delle colonne della riga è
// quello del generatore e mai quello fisico della tabella: un
// ALTER TABLE ADD COLUMN non può più spostare un valore nel campo
// accanto.
//
// Va in panic quando un nome non è una colonna della tabella
// dell'entità: il file generato e lo schema sono divergenti, e
// l'alternativa è una riga scansionata nei campi sbagliati, non un
// errore.
func (e *Entity[T]) SetFastScan(cols []string, scan func(Scanner, *T) error) *Entity[T] {
	refs := make([]drops.Expression, 0, len(cols))
	for _, n := range cols {
		c := e.table.Col(n)
		if c == nil {
			panic(fmt.Sprintf(
				"drops/pg: SetFastScan on %s: table %q has no column %q; re-run go generate",
				e.table.Name(), e.table.Name(), n))
		}
		refs = append(refs, c)
	}
	e.fastCols, e.fastScan = refs, scan
	return e
}

// Get / getCached / Query.All / Query.One / Page.All / Stream:
sel := db.Select(e.fastCols...).From(e.table).Where(pred)

// cmd/dropsgen/emit.go
func RegisterUser(e *pg.Entity[User]) *pg.Entity[User] {
	return e.SetFastScan(ColsUser(), ScanUser)
}
```

**File.** `pg/entity.go`, `pg/page.go`, `cmd/dropsgen/emit.go`, il golden file.
**Effort** M. **Rischio** cambio di firma *breaking* — che è esattamente perché va fatto
adesso: zero tag, costo zero; dopo v1, impossibile.

**ent/sqlc/GORM.** sqlc non ha il problema per costruzione: genera lo scanner dalla
`SELECT` che hai scritto. ent genera query e scanner insieme. GORM e bun scansionano per
nome da `rows.Columns()` — più lento, ma non può sbagliare binding. drops è oggi l'unico
che combina scanner posizionale e lista colonne implicita: il peggio dei due.

#### P0-4. Due scanner che non sono d'accordo

**Esito utente.** Una struct che embedda una struct non esportata — la forma
`type User struct { audit; ID int64 }` che il template `pg.Audit` incoraggia — perde ogni
campo promosso quando viene scansionata da un builder pg. `pg/scan.go` salta i campi non
esportati *prima* di camminare le anonime; `scan.go` le cammina apposta, e il suo commento
spiega perché. In più `pg/scan.go` usa last-write-wins dove `scan.go` ha un tie-break per
profondità.

La correzione **toglie** codice:

```go
// pg/scan.go — dopo

// ScanOne scansiona la prima riga in dest. Delega a drops.ScanOne
// perché ogni dialetto risolva colonne→campi con le stesse regole:
// tag drop, poi nome del campo, poi la forma camelCase, con la
// profondità di embedding minore che vince i pareggi.
func ScanOne(rows drops.Rows, dest any) error { return drops.ScanOne(rows, dest) }
func ScanAll(rows drops.Rows, dest any) error { return drops.ScanAll(rows, dest) }

func scanOne(rows drops.Rows, dest any) error { return drops.ScanOne(rows, dest) }
func scanAll(rows drops.Rows, dest any) error { return drops.ScanAll(rows, dest) }
```

**File.** `pg/scan.go`, `pg/select.go`, e audit identico su `mysql/scan.go`,
`sqlite/scan.go`, `clickhouse/scan.go`. **Effort** S. **Rischio** lo scanner root è più
severo su colonne non mappate: diffare i due comportamenti prima di commutare e mettere
la differenza dietro l'opzione esistente, mai cambiando il default in silenzio.

#### P0-5. Un `false` non si può salvare su una colonna con `DEFAULT true`

**Esito utente.** `Active bool` su una colonna `DEFAULT true`: si imposta `false`, si
chiama `Create`, la riga torna `true`. `collectInsertBindings` salta il binding quando
`fv.IsZero()` e la colonna ha un default. Vale per stringa vuota, zero, timestamp zero.
Nessun errore. È il singolo reclamo più citato contro GORM, riprodotto identico.

Tre pezzi, e nessuno dei tre indovina l'intento — indovinare è il comportamento attuale
ed è ciò che si rompe.

```go
// pg/column.go

// AlwaysInsert fa sì che Create leghi questa colonna anche quando il
// campo Go è al valore zero. Senza, un valore zero su una colonna con
// DEFAULT viene omesso perché il server lo riempia — che è giusto per
// createdAt e sbagliato per un bool che vuole legittimamente essere
// false. Il marcatore sta sulla colonna, così chi legge lo schema vede
// quali colonne non vengono mai inferite.
//
// Un campo puntatore esprime la stessa cosa per riga: nil viene omesso,
// non-nil viene scritto anche quando punta al valore zero.
func (c *Col[T]) AlwaysInsert() *Col[T] {
	c.Column.alwaysInsert = true
	return c
}

var UserActive = pg.Add(Users, pg.Boolean("active").NotNull().Default("true").AlwaysInsert())

// pg/entity.go — la forma per-chiamata, accanto a PatchKey

// CreateCols inserisce r legando esattamente cols, ignorando del tutto
// la regola di skip sul valore zero.
func (e *Entity[T]) CreateCols(db *DB, ctx context.Context, r *T, cols ...ColRef) error
```

Il terzo pezzo è **documentare la convenzione puntatore che già funziona**: un `*bool`
non-nil che punta a `false` non è `IsZero`. È il precedente di `encoding/json`, gli
sviluppatori Go lo conoscono già, e oggi è un incidente non documentato.

**File.** `pg/column.go`, `pg/entity.go`, `pg/autotable.go`, `docs/entities.md`.
**Effort** S. **Ordine obbligatorio:** questo va prima di `SetFastBind` (P1-ORM), o il
generatore congela un bug di corruzione dati dentro codice committato.

#### P0-6. `Push` cancella le tabelle degli altri

**Esito utente.** `pg.Push` contro un database condiviso — Supabase, Neon, RDS con più
servizi, PostGIS — emette `DROP TABLE` per ogni tabella che lo `Schema` Go non dichiara.
È il modo più rapido perché uno strumento venga bandito da un'organizzazione, per sempre.

Non è un design: è un **port**. `sqlite/push.go` fa già `Diff(ownedBy(current, desired), desired)`.

```go
// pg/push.go
type PushOptions struct {
	// ...Schema, Safe, DryRun, DropUnmanagedIndexes...

	// DropUnmanagedTables permette a Push di cancellare una tabella che
	// esiste nel database ma non compare in nessuna tabella dello Schema
	// Go. È off di default, per la stessa ragione di
	// DropUnmanagedIndexes: il lato "precedente" di Push è un database,
	// dove una tabella che lo schema Go non nomina molto probabilmente
	// non è mai stata di drops — di un altro servizio, di un vendor, di
	// un'estensione. I DROP trattenuti sono riportati come notice
	// "unmanaged-table" in entrambi i casi.
	DropUnmanagedTables bool
}

// ownedBy restringe un'introspezione live alle tabelle dichiarate dallo
// Schema, restituendo quelle trattenute perché Push le riporti.
func ownedBy(live, declared *Snapshot) (*Snapshot, []string)
```

L'asimmetria va scritta nel doc comment: la restrizione **non** si applica a `Diff` né a
`GenerateMigration`, dove entrambi i lati sono dichiarazioni e una tabella mancante
significa davvero rimozione.

**File.** `pg/push.go`, `mysql/push.go`. **Effort** S.
**ent/sqlc/GORM.** GORM AutoMigrate non cancella mai nulla — sicuro e inutile, perché
non ti dice nemmeno che il modello è sparito. ent ha `WithDropColumn(false)` globale.
atlas usa uno scope HCL. La versione drops è migliore delle glob-string perché la
proprietà deriva da dichiarazioni Go già verificate dal compilatore.

#### P0-7. Rename espliciti e consenso tipizzato per il DDL distruttivo

**Esito utente.** Rinominare `users.email` in `users.email_address` produce un
`ALTER TABLE ... RENAME COLUMN`, non `DROP` + `ADD`. E `Push` non cancella una tabella
piena senza che qualcuno l'abbia nominata.

Due metà della stessa cosa: **drops non distrugge dati senza che l'intento sia scritto
da qualche parte che si può rivedere in una PR.** Nota che `pg/safety.go` ha già le
regex che *riconoscono* un rename nell'SQL altrui, e `pg/diff.go` non ne emette mai uno.

```go
// pg/rename.go

// Una colonna cancellata e una aggiunta sono indistinguibili per un
// diff; solo l'autore sa che la coppia è una colonna sotto un nome
// nuovo. drops lo chiede all'autore nell'unico posto dove chi legge
// guarderà — il call site — invece di indovinare, o di interrogare un
// terminale che la CI non ha.
type RenameKind string

const (
	RenameSchema RenameKind = "schema"
	RenameTable  RenameKind = "table"
	RenameColumn RenameKind = "column"
)

type Rename struct {
	Kind     RenameKind
	Table    string // richiesto per RenameColumn, ignorato altrimenti
	From, To string
}

// ErrRenameNotApplicable è restituito quando un rename dichiarato non
// corrisponde ad alcuna coppia drop/add nel diff — il rename è già
// stato generato, o nomina una colonna che non è mai esistita.
var ErrRenameNotApplicable = errors.New("drops/pg: rename matches no change in the diff")

res, err := pg.GenerateMigration(pg.GenerateOptions{
	Schema: app.Schema(),
	Dir:    "drizzle",
	Name:   "rename_email",
	Renames: []pg.Rename{
		{Kind: pg.RenameColumn, Table: "users", From: "email", To: "email_address"},
	},
})
```

```go
// pg/push.go — consenso per oggetto, non un --force

var ErrDestructivePush = errors.New(
	"drops/pg: push withheld destructive statements; see PushResult.DataLoss")

type DestructiveOp string

const (
	DropTable    DestructiveOp = "drop-table"
	DropColumn   DestructiveOp = "drop-column"
	RetypeColumn DestructiveOp = "retype-column"
)

// Destructive nomina una modifica che Push è autorizzato a fare.
// Nominare l'oggetto invece di passare un --force globale significa che
// il consenso di ieri non può autorizzare il DROP non correlato di oggi.
type Destructive struct {
	Op     DestructiveOp
	Table  string
	Object string // nome colonna; vuoto per DropTable
}

// DataLoss è una modifica distruttiva che Push ha trovato, con il suo
// costo. Rows è una STIMA letta da pg_class.reltuples: distinguere
// "vuota" da "non vuota" è tutto ciò che serve alla regola, e un
// COUNT(*) per tabella candidata è una seq scan dentro l'operazione che
// più vuoi veloce.
type DataLoss struct {
	Op         DestructiveOp
	Table      string
	Object     string
	Rows       int64
	SQL        string
	Suggestion string // "allow with pg.Destructive{Op: pg.DropColumn, ...}"
}
```

E finalmente si collegano gli analizzatori che esistono e non chiama nessuno:

```go
// pg/migrate.go, pg/drizzle.go, pg/treemigrate.go
func (m *Migrator) WithSafetyGate(min SafetySeverity) *Migrator
```

**File.** `pg/diff.go`, `pg/generate.go`, `pg/snapshot.go` (`SnapshotMeta`, allocato e
mai riempito), `pg/push.go`, `mysql/*`, `sqlite/*`. **Effort** M+M.
**Rischio** sqlite non ha rename in-place prima di 3.25 e il suo diff già ricostruisce la
tabella: il pre-pass di rename deve alimentare la mappa colonne del rebuild, o i dati
finiscono nella colonna sbagliata.

**ent/sqlc/GORM.** Niente in Go risolve il rename: goose e golang-migrate ti danno un file
vuoto (sicuro ma manuale), GORM crea la colonna nuova e lascia la vecchia per sempre,
atlas lo inferisce euristicamente e sbaglia sugli schemi simmetrici. Sul consenso, ent ha
booleani globali (`WithDropColumn(true)` autorizza *tutti* i drop per sempre); il
`Allow []Destructive` per oggetto è più fine e non richiede un linguaggio di policy.

### Blocco B — Capacità la cui assenza squalifica

#### P0-8. Isolation level, e la retry policy che finalmente ha qualcosa da ritentare

**Esito utente.** Un trasferimento di saldo può chiedere `SERIALIZABLE` e ritentare sul
conflitto. Oggi `stdlib` fa `BeginTx(ctx, nil)` e non c'è argomento, opzione o hook in
tutto il modulo per cambiarlo — quindi `pg.ErrSerializationFailure`,
`DefaultRetryPolicy` ed `ExponentialJitter` sono codice morto sotto il default
`READ COMMITTED` di PostgreSQL.

Il `Driver` resta a **tre metodi**. La capacità è opzionale e duck-typed come `pg.Copier`,
e il fallback SQL fa funzionare la feature su qualunque driver, anche un fake di test.

```go
// driver.go — Driver invariato.

type IsolationLevel int

const (
	DefaultIsolation IsolationLevel = iota
	ReadCommitted
	RepeatableRead
	Serializable
)

// TxOptions è una struct di config il cui valore zero significa
// "qualunque sia il default del server", che è ciò che Begin fa oggi.
type TxOptions struct {
	Isolation IsolationLevel
	ReadOnly  bool
}

// TxBeginner è la capacità opzionale che un driver implementa quando
// sa aprire una transazione con opzioni in un solo round trip.
type TxBeginner interface {
	BeginTx(ctx context.Context, opts TxOptions) (Tx, error)
}

// ErrTxOptionsNotSupported è restituito solo quando NESSUNA delle due
// strade è percorribile — né TxBeginner, né SET TRANSACTION (ClickHouse
// non ha transazioni). Eseguire in silenzio a READ COMMITTED quando il
// chiamante ha chiesto SERIALIZABLE è un bug di integrità, non un
// degrado.
var ErrTxOptionsNotSupported = errors.New(
	"drops: driver cannot honour the requested transaction options")
```

```go
// pg/tx.go — superset del dialetto: DEFERRABLE è solo PostgreSQL e
// non appartiene a drops.TxOptions.
type TxOptions struct {
	drops.TxOptions
	Deferrable bool
}

type TxOption func(*TxOptions)

// Serializable richiede SERIALIZABLE. Accoppialo a WithRetry: sotto
// questo livello PostgreSQL aborta le transazioni in conflitto con
// 40001, che è esattamente ciò che DefaultRetryPolicy ritenta.
func Serializable(o *TxOptions) { o.Isolation = drops.Serializable }
func ReadOnly(o *TxOptions)     { o.ReadOnly = true }
func Deferrable(o *TxOptions)   { o.Deferrable = true }

// InTx accetta opzioni variadiche, quindi ogni call site esistente
// continua a compilare. Quando il driver implementa drops.TxBeginner le
// opzioni viaggiano nel BEGIN; altrimenti InTx emette
// "SET TRANSACTION ISOLATION LEVEL ..." come primo statement dopo
// Begin, con il consueto evento sull'hook.
func (db *DB) InTx(ctx context.Context, fn func(*DB) error, opts ...TxOption) error
```

```go
err := db.WithRetry(pg.DefaultRetryPolicy()).
	InTx(ctx, func(tx *pg.DB) error {
		_, err := tx.Update(Accounts).
			Set(AccountBalance.Expr(pg.Minus(AccountBalance, 100))).
			Where(AccountID.Eq(from)).Exec(ctx)
		return err
	}, pg.Serializable)
```

**File.** `driver.go`, `db.go`, `stdlib/stdlib.go`, `pg/db.go`, `pg/tx.go` (nuovo),
`pg/retry.go`, `mysql/db.go`, `sqlite/db.go`. **Effort** M.
**Rischio** divergenza dialettale reale: SQLite ha solo `BEGIN DEFERRED/IMMEDIATE/EXCLUSIVE`,
ClickHouse non ha transazioni. Ogni dialetto ha bisogno di una decisione esplicita e di una
riga di doc, non di un no-op silenzioso.

**ent/sqlc/GORM.** `sql.TxOptions` esiste dal Go 1.8; GORM, ent, bun e sqlx lo passano
tutti dritto. drops è l'unico dei sei che non sa esprimerlo — ed è l'unico che spedisce
una retry policy per serialization failure, il che rende l'omissione vistosa.

#### P0-9. Una coda di lavoro non è scrivibile nel dialetto pg

**Esito utente.** `SELECT ... FOR UPDATE SKIP LOCKED` — la primitiva su cui poggiano
River, gue e ogni job queue Go artigianale. Oggi `pg/select.go` ha un `forUpdate bool` che
rende ` FOR UPDATE` e basta. La prova migliore è interna: `pg/outbox.go` scrive a mano
l'intero statement con una `fmt.Sprintf` perché il builder non lo esprime, mentre il
dialetto mysql ha già i metodi tipizzati.

```go
// pg/locking.go
type LockStrength string

const (
	LockUpdate      LockStrength = "UPDATE"
	LockNoKeyUpdate LockStrength = "NO KEY UPDATE"
	LockShare       LockStrength = "SHARE"
	LockKeyShare    LockStrength = "KEY SHARE"
)

type LockOption func(*lockClause)

// SkipLocked scavalca le righe che un'altra transazione già detiene. È
// l'opzione del worker di coda: N worker prendono ciascuno una riga
// diversa invece di serializzarsi dietro la prima.
func SkipLocked(l *lockClause) { l.wait = " SKIP LOCKED" }
func NoWait(l *lockClause)     { l.wait = " NOWAIT" }

// LockOf prende handle *Table, che già conoscono il proprio alias, così
// "FOR UPDATE OF u" resta corretto attraverso Table.As.
func LockOf(tables ...*Table) LockOption

func (s *SelectBuilder) For(strength LockStrength, opts ...LockOption) *SelectBuilder

// ForUpdate resta come alias documentato di For(LockUpdate).
```

```go
var job Job
err := db.Select(JobID, JobPayload).From(Jobs).
	Where(JobClaimedAt.IsNull()).
	OrderBy(JobID.Asc()).
	Limit(1).
	For(pg.LockUpdate, pg.SkipLocked).
	One(ctx, &job)
```

**Criterio di accettazione:** `Outbox.Drain` viene riscritto sul builder. Rimuove l'SQL
raw e regala alla feature un test di integrazione.

**File.** `pg/select.go`, `pg/locking.go` (nuovo), `pg/outbox.go`, `mysql/select.go`.
**Effort** S. **Rischio** `FOR UPDATE` con outer join o set operation è un errore
PostgreSQL: va superficiato come deferred `err` a `Rows()`, non lasciato al server.

**ent/sqlc/GORM.** ent ha `entsql.ForUpdate(entsql.WithLockAction(entsql.SkipLocked))`;
GORM ha `clause.Locking{Strength:"UPDATE", Options:"SKIP LOCKED"}`; bun prende una stringa
libera. Le costanti tipizzate + `LockOf` su handle di tabella battono tutte e tre.

### Blocco C — Distribuzione e credibilità

#### P0-10. Un comando

**Esito utente.** `drops migrate up`, `drops migrate status`, `drops pull`, `drops check`,
`drops diff`, `drops drift` da un terminale, da un Makefile e da una CI — senza scrivere
un programma.

**Il vincolo, e la sua soluzione.** Il CLI deve aprire un database; il modulo root non ha
driver, e quella proprietà è un fossato. Quindi: i verbi vivono in un **package libreria
zero-dep dentro il modulo root**, e solo il binario che linka i driver è un modulo a parte.
Effetto collaterale che è metà del valore: l'intera superficie di comando diventa
testabile in tabella, e i verbi offline girano in CI **senza server**.

> **Decisione, per chiudere tre design incompatibili.** Vince questo layout.
> `cmd/drops-pg` e un modulo `cli/` con `replace ../` sono chiusi come duplicati —
> `go install m/pkg@version` **rifiuta** un modulo il cui `go.mod` contiene direttive
> `replace`, quindi quella variante non installa affatto.

```go
// pg/pgcmd/pgcmd.go — modulo root, ancora zero dipendenze.
package pgcmd

// Env è tutto ciò che un comando non può decidere da solo. Open è una
// func e non un *pg.DB perché generate, check ed export devono girare
// in CI contro nessun server, e un campo nil lo dice meglio di una
// connessione aperta pigramente.
type Env struct {
	Schema *pg.Schema
	Dir    string // directory delle migrazioni; default "drizzle"
	Open   func(context.Context) (*pg.DB, error)
	Stdout io.Writer
	Stderr io.Writer
}

// Main esegue un sottocomando e restituisce l'exit code del processo.
// Non chiama mai os.Exit e non va mai in panic.
func Main(ctx context.Context, env Env, args []string) int
```

```go
// Il main.go dell'utente per intero: la scelta del driver è visibile
// al call site, drops non parsa mai un DSN.
func main() {
	os.Exit(pgcmd.Main(context.Background(), pgcmd.Env{
		Schema: app.Schema(),
		Dir:    "drizzle",
		Open: func(ctx context.Context) (*pg.DB, error) {
			pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
			if err != nil {
				return nil, err
			}
			return pg.New(pgxdriver.New(pool)), nil
		},
	}, os.Args[1:]))
}
```

Configurazione: **flag, poi env, e basta.** Nessun `drops.json`, nessuno scaffolder
`drops init` — il `main.go` sopra sta in `docs/cli.md` e si copia in trenta secondi.
Variare per ambiente è ciò per cui esistono `os.Getenv` e un secondo `main.go`.

Ogni sottocomando ha `--format text|json` e un contratto di exit code, perché il materiale
grezzo esiste già ed è inutilizzato — `SchemaNotice.Rule`, `SafetyWarning.Rule`/`Severity`,
`DriftReport`:

```go
// pg/pgcmd/envelope.go

// Envelope è l'unico oggetto JSON che una run --format=json stampa. È
// su una riga sola perché uno step di CI possa pipeare a jq senza
// bufferizzare, e non porta DSN, né argomenti legati, né valori di riga:
// un envelope che fa finire una connection string in un build log è
// peggio di nessun envelope.
type Envelope struct {
	Status     string             `json:"status"` // ok | no_changes | blocked | error
	Command    string             `json:"command"`
	Statements []string           `json:"statements,omitempty"`
	Notices    []pg.SchemaNotice  `json:"notices,omitempty"`
	Warnings   []pg.SafetyWarning `json:"warnings,omitempty"`
	Error      *Error             `json:"error,omitempty"`
}

const (
	ExitOK      = 0
	ExitError   = 1
	ExitUsage   = 2
	ExitBlocked = 3 // il safety analyzer ha trovato uno statement SeverityError
)
```

La redazione (DSN, argomenti legati, colonne `AsPII`) è **imposta da un test** che esegue
ogni comando con un DSN contenente una password sentinella e fa grep dell'output. Una
revisione a mano prima o poi ne perde uno.

**File.** `pg/pgcmd/` (nuovo), `mysql/mysqlcmd/`, `sqlite/sqlitecmd/`, `cmd/dropsctl/`
(modulo separato), `cmd/drops/main.go`, `Makefile`, `docs/cli.md`. **Effort** L.
**Rischio** due entrypoint (Main embeddato e binario precompilato) possono divergere:
ogni comando è un `func run<Name>(ctx, Env, args) error` e il binario è uno switch sottile.

**ent/sqlc/GORM.** ent tiene il lavoro schema-aware dietro `go:generate` e spedisce
`go run entgo.io/ent/cmd/ent` — stesso split. goose e golang-migrate linkano ogni driver
nel binario, ed è per quello che golang-migrate ha una matrice di build tag. sqlc è
offline e schiva la domanda.

#### P0-11. `pgxdriver`: quattro sottosistemi spediti e irraggiungibili

**Esito utente.** `pg.CopyFrom`, `pg.Listen`, `pg.Subscribe` e il changefeed a trigger
funzionano dopo un `go get`. Oggi **nessun driver nel repository** implementa `Copier`,
`Listener` o `PoolStatsProvider`: chi legge la promessa di "10-50x faster than INSERT" e
chiama `pg.CopyFrom` riceve `ErrCopyNotSupported`, e deve scriversi un adapter pgx da un
doc comment, senza test.

Questo è il pezzo che converte "zero dipendenze" da slogan a vantaggio reale: l'utente
importa *un* driver, il core resta senza dipendenze.

```go
// pgxdriver/go.mod:
//   module github.com/bernardoforcillo/drops/pgxdriver
//   require github.com/jackc/pgx/v5 v5.x
package pgxdriver

// New avvolge un *pgxpool.Pool. Il Driver restituito soddisfa
// drops.Driver, pg.Copier, pg.Listener, drops.PoolStatsProvider e
// drops.TxBeginner — ogni capacità opzionale che drops dichiara, in un
// solo tipo, così pg.Supports* rispondono tutte true.
func New(pool *pgxpool.Pool) *Driver

func (d *Driver) Copy(ctx context.Context, table string, cols []string, rows [][]any) (int64, error) {
	return d.pool.CopyFrom(ctx, pgx.Identifier{table}, cols, pgx.CopyFromRows(rows))
}

func (d *Driver) Listen(ctx context.Context, channel string) (<-chan pg.Notification, error)
func (d *Driver) BeginTx(ctx context.Context, opts drops.TxOptions) (drops.Tx, error)
func (d *Driver) PoolStats() drops.PoolStats
```

Cavaliere obbligatorio: `pg.CopyFrom` fa oggi `reflect.FieldByIndex` per riga per colonna,
il che smonta da solo l'argomento di throughput. Va agganciato al fastpath di dropsgen.

**File.** `pgxdriver/` (nuovo modulo), `pg/copy.go`, `pg/listen.go`, `pg/changefeed.go`,
`integration/pg_test.go`, `.github/workflows/ci.yml`. **Effort** L.
**Rischio** LISTEN ha bisogno di una connessione dedicata fuori dal pool e di semantiche di
riconnessione precise — è lì che questi adapter perdono goroutine.

**ent/sqlc/GORM.** sqlc genera helper `CopyFrom` su pgx e prende la feature gratis: è la
soglia. ent e GORM non sanno fare COPY. bun ha il bulk insert ma non COPY.

#### P0-12. Tag, contratto di compatibilità, e riduzione di superficie

**Esito utente.** `go get github.com/bernardoforcillo/drops@v0.6.0` risolve, pkg.go.dev
mostra una versione, e il team lead sa cosa può rompersi.

Non è un pomeriggio di faccende. È una decisione di prodotto in tre atti.

**Atto 1 — allineare i numeri.** `changelog.md` annuncia già cinque release fino a
`[0.6.0]`; taggare `v0.1.0` renderebbe il changelog retroattivamente fittizio. Si tagga
**`v0.6.0`**, e non si lascia mai più il changelog correre davanti a un tag.

```go
// cmd/drops/main.go
// version viene stampata dal workflow di release con -ldflags. Per un
// binario prodotto da `go install ...@v0.6.0` non c'è stamping, quindi
// si ricade sulla versione di modulo che la toolchain ha registrato.
// Una costante hardcoded era già indietro di due minor quando qualcuno
// se n'è accorto.
var version = "dev"

func buildVersion() string {
	if version != "dev" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok &&
		bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return version
}
```

**Atto 2 — il contratto, e l'audit che lo rende sostenibile.** `pg` da solo è 160 file e
~38k righe: nessuno può promettere di non romperlo, e una v1 che si rompe è peggio di una
v0 mai spedita. Prima del tag si fa un **audit della superficie esportata**: si contano i
simboli e si *de-esporta o si sposta sotto `internal/`* tutto ciò che non fa parte della
promessa.

```
docs/compatibility.md

Stabile in v0.x — breaking solo su bump minor, elencati in changelog.md:
  drops, drops/stdlib, drops/pg, drops/pgxdriver

Sperimentale — può cambiare in qualunque release:
  drops/mysql, drops/clickhouse, drops/mirror, drops/vector,
  drops/qdrant, drops/cache/...

Policy di deprecazione: un simbolo deprecato vive almeno una minor.
Versioni Go supportate: le due più recenti. (Oggi il repo ne dichiara
tre diverse: root go.mod 1.22, integration 1.25, CI 1.24 — va unificato.)
```

**Atto 3 — l'enforcement, senza il quale l'atto 2 è un paragrafo.** Un job CI che diffa la
superficie esportata dei package dichiarati stabili contro il tag precedente e fallisce la
PR su rimozioni o cambi di firma. Lo strumento porta una dipendenza: vive in un modulo suo,
come `analysis/`.

**File.** `cmd/drops/main.go`, `.github/workflows/release.yml` (tag-triggered, sei
`go build -trimpath -ldflags` in matrice, **niente goreleaser**), `docs/compatibility.md`,
`changelog.md`. **Effort** S per i tag, M per l'audit.
**Rischio** è una porta a senso unico: i P0 breaking (P0-3, P0-8, P0-5) vanno **prima**.

#### P0-13. Nessuna prova che drops sia veloce

**Esito utente.** Un numero riproducibile, con il comando per riprodurlo, prima di
impegnarsi. Oggi: **zero `func Benchmark`** in 164 file di test, nessun target `bench`,
nessun job CI, nessun ns/op in `readme.md` — dietro un generatore la cui unica ragione
d'esistere è la performance.

Due suite, e la seconda è quella che conta davvero.

**Suite interna** (modulo root, driver fake, nessun server):

```go
// pg/entity_bench_test.go

// BenchmarkScan confronta lo scanner a reflection con il fastpath
// Scan<T> generato sulle stesse righe. Il delta è l'intera ragione
// d'esistere del generatore; una claim non misurata non è una claim.
func BenchmarkScanReflect(b *testing.B) {
	db := pg.New(dropstest.New().Rows(usersCols, usersRows...))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := UserEntity.Query(db).All(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkScanGenerated(b *testing.B) { /* idem, con SetFastScan(ColsUser(), ScanUser) */ }
```

Coperti anche: render del builder (nessun I/O), overhead di `Hook`/otel con hook nil,
no-op e `Instrumentation` attaccata.

**Suite comparativa** (modulo `benchmarks/` separato, Postgres reale). **Questa è la metà
che i due proposal originali omettevano.** Misurare drops contro drops risponde a
"valeva la pena scrivere il generatore", che interessa solo al maintainer. La domanda che
interessa a chi valuta è drops vs **pgx a mano, sqlc, ent, bun, GORM** sulle stesse query.
Lo spazio database Go è insolitamente alfabetizzato sui benchmark; una tabella credibile è
l'unico artefatto che può far guardare due volte una libreria di tre mesi.

`make bench` + un job CI che li esegue con `-benchtime=10x` **solo per tenerli
compilanti** — non si mette un gate su wall-clock su runner condivisi.

**File.** `Makefile`, `.github/workflows/ci.yml`, `pg/*_bench_test.go`, `scan.go`,
`benchmarks/` (modulo separato), `docs/performance.md`. **Effort** M + una settimana.
**Dipendenza:** ha bisogno di `dropstest.Driver` (P1-RT2), che quindi va costruito prima.

---

## 4. Per area

Legenda effort: S ≤ 1 giorno, M ≈ 2–4 giorni, L ≈ 1–2 settimane.

### 4.1 Query builder

| # | Voce | Design | Effort | Rischio |
|---|---|---|---|---|
| **Q1** P1 | `Clone()` su ogni builder | Copia profonda: ogni slice copiata, `*DB` e `*Table` condivisi (read-only), `err` riportato. `Apply()` **non** si fa: è zucchero su un loop di due righe. Correggere `pg/doc.go:9` che promette il contrario | S | una slice nuova dimenticata in `Clone` reintroduce l'aliasing su una sola clausola. Guardia: test a reflection che fallisce quando `SelectBuilder` guadagna un campo non copiato |
| **Q2** P1 | JOIN su Expression | `joinClause.table *Table` → `src drops.Expression` (una riga: `*Table` implementa già `writeFrom`), + `JoinExpr`/`LeftJoinExpr`/`RightJoinExpr`/`FullJoinExpr`/`CrossJoin` e `Lateral` | S | verificare che `pg/find.go` e il tenant scoping non leggano `joins[].table`; se sì tenere un `tbl *Table` accanto |
| **Q3** P1 | `drops.Exprf` / `Ident` / `Join` | Ogni `?` consuma un argomento e lo **tiene in un campo**, adattato una volta sola alla costruzione — la regola di `operandExpr`, non più quella di `writeOperand`, che il round 5 ha cancellato insieme al render-time. Un operando deciso al render è un operando che nessun walk raggiunge: la decisione "è un'Expression o un valore?" va presa quando l'espressione si costruisce, così un `*SelectBuilder` passato a un `?` resta un nodo visitabile e porta lo scoping della tabella che legge (vedi i doc comment di `opExpr` e `funcExpr` in `pg/op.go`). Ne segue che `Exprf` **non** può essere una `drops.ExprFunc` che formatta al volo: serve un nodo che tenga testo e operandi separati, come `opExpr`. `??` è il `?` letterale (gli operatori jsonb ne hanno bisogno). **Mismatch di arity = deferred error**, non panic: si scrive dentro un handler, non in `init` | S | il `?` di jsonb va in testa al doc comment |
| **Q4** P1 | `FromSelect` + CTE sui write builder | `ctes` estratto in un `cteHolder` embeddabile, dato a Insert/Update/Delete. La lista colonne è **esplicita**, mai inferita dalla proiezione | M | referenziare le colonne di una CTE rompe la catena tipizzata proprio dove serve; accoppiare con un handle di colonna CTE tipizzato |
| **Q5** P1 | `InSavepoint` | vedi sketch sotto | M | l'interazione con `RetryPolicy` è il punto sottile |
| **Q6** P1 | ORDER BY: NULLS + espressioni | **Due tipi, non uno.** `OrderingColumn` resta cursor-safe (solo `ColRef`); un tipo separato per l'ordinamento generale in `SelectBuilder.OrderBy`. Fondere e restituire un deferred error quando `col` è nil converte un errore di compilazione in uno di runtime | S | — |
| **Q7** P1 | `TargetWhere` / `OnConflictConstraint` | Le due WHERE della grammatica hanno due nomi. `TargetWhere` rende letterali e rifiuta parametri (PostgreSQL non li accetta in un predicato di indice) con build error | S | `Entity.UpsertMany` costruisce la sua clausola e va trattata uguale |
| **Q8** P2 | Operatori mancanti in pg | `IsDistinctFrom`, `IsNotDistinctFrom`, `NotLike`, `NotILike`, `NotBetween`, `InSub`, `NotInSub` — che sqlite ha già. Doppia forma di casa: func package-level `any` + metodo `*Col[T]` | S | nessuno |
| **Q9** P2 | `pg.Row` + keyset a row-value | `ROW(a,b) > ROW(x,y)` quando l'indice composito può servirla. **Guardia a tre condizioni:** direzione uniforme, nessun NULLS esplicito, **e ogni colonna chiave `NOT NULL`** — il confronto row-value dà NULL se un membro è NULL, e senza la terza condizione la paginazione salta righe al bordo pagina | M | il fallimento peggiore possibile per un'API a cursori. Test su server reale che pagina in entrambe le direzioni e asserisce che l'unione è la tabella |
| **Q10** P2 | `WithinGroup` | Solo il wrapper generale. `PercentileCont`/`Disc`/`Mode` e il mirror ClickHouse **non si fanno**: `Exprf` copre la coda in una riga | S | il frazionamento va legato come parametro |
| **Q11** P2 | sqlite al pavimento | `Rows(ctx)`, `GroupBy`, `Having`, `Count`, `FromExpr`, `AsSubquery` — senza `Rows(ctx)` i generici `drops.All[T]`/`One[T]` pubblicizzati in `doc.go` **non compilano** contro sqlite. Il pavimento si scrive in `doc.go` + test per dialetto. **`internal/dialecttest` con interfacce cross-dialetto non si fa**: è un sistema di tipi ombra sopra quattro package duplicati apposta | M | scope creep: aggiungere DISTINCT ON a sqlite produce SQL che il motore rifiuta |
| **Q12** P2 | `Comment` SQLcommenter | **Rimandato oltre v1.** Quando arriva: `Comment(...Tag)` con `Tag` struct a due campi, mai variadic di stringhe accoppiate a mano (un numero dispari di argomenti fallirebbe solo a runtime). Validare, non escapare, `*/` | M | tag ad alta cardinalità gonfiano `pg_stat_statements` |
| — | ~~`Compile`/`Stmt`/`Param`~~ | **Cancellato.** Il pezzo costoso (prepare server-side) lo fa già pgx; resta il render di una stringa, non misurato. In cambio si butta via ogni garanzia di compilazione. Si riapre **solo** se P0-13 mostra che il render è una frazione misurabile di una richiesta | — | — |

**Q5 — savepoint, con la regola che manca ovunque:**

```go
// pg/db.go

// InSavepoint esegue fn dentro un SAVEPOINT sulla transazione a cui db
// è legato, rilasciandolo quando fn ritorna nil e facendo rollback
// altrimenti. Solo il lavoro del savepoint viene scartato — la
// transazione esterna resta viva, che è ciò che rende possibile
// "prova questo, prosegui se va in conflitto" su PostgreSQL, dove una
// violazione di vincolo non protetta aborta tutto.
//
// Restituisce ErrNotInTransaction quando db non è legato a una
// transazione, così il nome al call site non è mai una bugia.
//
// Il nome del savepoint deriva da un contatore di profondità non
// esportato: non è mai controllato dal chiamante, quindi non c'è
// superficie di injection.
func (db *DB) InSavepoint(ctx context.Context, fn func(*DB) error) error

// RetryPolicy NON si applica a profondità > 0: fare rollback a un
// savepoint non può disfare gli statement della transazione esterna,
// quindi ri-eseguire fn ritenterebbe contro uno stato già mutato.
// InTx salta il loop di retry quando è annidato, e lo dice a voce alta.
```

`InSavepoint` è l'ortografia primaria. La promozione automatica di `InTx` su un `*DB`
legato a transazione è **documentata, non il titolo**: un cambio di comportamento pilotato
da stato non esportato è la magia che questo pubblico punisce.

```go
err := db.InTx(ctx, func(tx *pg.DB) error {
	if err := tx.InSavepoint(ctx, func(sp *pg.DB) error {
		return sp.Insert(Users).Row(UserEmail.Val(email)).Exec(ctx)
	}); err != nil && !errors.Is(err, pg.ErrUniqueViolation) {
		return err
	}
	_, err := tx.Insert(AuditLog).Row(/* ... */).Exec(ctx)
	return err
})
```

### 4.2 Schema

| # | Voce | Design | Effort | Rischio |
|---|---|---|---|---|
| **S1** P1 | `PgExtension` dichiarabile | `pg.NewExtension("vector")`, `Schema.AddExtension`, `ExtensionSnapshot`, `diffExtensions` che gira **per primo** (prima di enum e CREATE TABLE). `Existing()` per pgvector installato dalla piattaforma. Notice `missing-extension` derivato dal tipo di colonna. **Motivo P1 alto: il quickstart vector non funziona su un database vuoto** — 42704 | S | drop ordering: prima versione emette solo create |
| **S2** P1 | Indici pgvector fedeli | `IndexSnapshot.OpClasses []string` + popolare il `With map[string]any` già dichiarato e mai riempito; `parseStorageParams`; `indexEqual` che confronta entrambi; join `pg_index.indclass → pg_opclass` e `pg_class.reloptions` in `readIntrospectIndexes`; regola notice `lossy-index`. **NB: `writeIndexCreate` emette già `"col" opclass`** — quel passo è fatto | M | bump di versione dello snapshot: un `With` vuoto pre-bump va trattato come "sconosciuto", non "assente" |
| **S3** P1 | Reader di introspezione mancanti | enum (in `enumsortorder`), sequence, view, policy, `relrowsecurity` invece del `false` hardcoded, **e FK multi-colonna** (oggi solo mono-colonna). Senza, `Push` non converge e `DetectDrift` segnala lavoro pendente per sempre proprio sugli oggetti che drops dichiara in esclusiva | L | i corpi delle view: PostgreSQL li riscrive. Riusare `renormaliseExpressions`, e se non si può sondare emettere un notice e non toccare — come già fa il path delle CHECK |
| **S4** P1 | `GeneratedAlways` | `GENERATED ALWAYS AS (expr) STORED`, implica `Managed()` (quindi il drift check di `NewEntity` lo salta), sopprime NOT NULL/DEFAULT che PG rifiuta, cambio d'espressione = DROP+ADD rifiutato in `Safe`. Lo schema FTS canonico tsvector+GIN diventa dichiarabile end-to-end | M | va spedito **insieme** al reader `attgenerated='s'`, o ogni push ripropone la colonna |
| **S5** P1 | Identity column | `IdentityAlways(opts ...SequenceOptions)` / `IdentityByDefault`, riusando `SequenceOptions` invece di una struct parallela. Postgres raccomanda IDENTITY da v10: uno schema tool che conosce solo `serial` legge datato, e ogni introspezione di un DB moderno tipizza male la chiave | M | `autoIncrement` di dropsgen resta `serial`; conversione serial↔identity rifiutata con notice nella prima versione |
| **S6** P1 | Modificatori di indice | `indexElem{expr, opClass, desc, nulls}` + `On(col)` come **unico** modo di attaccare un elemento modificato; il costruttore variadico resta senza modificatori (`NewIndex(n, t, a, b).Desc()` non dà a chi legge niente su cui contare). Non riusare `Col[T].Desc()`: rende un riferimento qualificato che PG rifiuta in una index column list (42601) | S | — |
| **S7** P1 | Indici sqlite | `sqlite/index.go` duplicato apposta (niente astrazione condivisa), `AddIndex`, popolare `IndexSnapshot`, `diffIndexes`. Omettere CONCURRENTLY/INCLUDE/opclass invece di accettarli e ignorarli. Correggere `sqlite/snapshot.go:56` e `docs/dialects.md:65` che dichiarano il limite come fatto | M | va con il path di rebuild: un rebuild che perde gli indici è una regressione |
| **S8** P1 | `Named` sui vincoli | `pg.Named("fk_orders_user")` nella famiglia `func(*FK)` già stabilita da `OnDelete`/`OnUpdate`, + `UniqueNamed`. **Promosso da P3:** "la prima cosa che drops fa in produzione è prendere un ACCESS EXCLUSIVE su ogni tabella per rinominare vincoli in modo cosmetico" è un blocco all'adozione | S | due nomi per un vincolo: test che asserisce che `BuildSnapshot` e `writeTableConstraints` producono lo stesso nome per ogni tipo |
| **S9** P2 | `Existing()` + `AddExisting` | Un marcatore, applicato coerentemente a `PgView`, `PgSequence`, `PgExtension`; per le tabelle `Schema.AddExisting(t)` (partecipa alle query, non ai diff). Precedenza rispetto a `DropUnmanagedTables` **decisa esplicitamente**, o una tabella marcata in due modi ha due risposte. `DetectDrift` deve comunque elencarla, in un terzo bucket | M | il marcatore non deve diventare un modo di nascondere problemi |
| **S10** P2 | `pg.Typed[T]` + tipi base esportati | `ColumnType` è esportata e nessun costruttore la accetta: i wrapper `TypeArray`/`TypeMap` di clickhouse sono irraggiungibili. La metà conversione **non si costruisce**: esistono `driver.Valuer`/`sql.Scanner` e tre esempi in-tree (`Money`, `Point`, `Secret[T]`) | S | — |
| **S11** P2 | `Deferrable` / `InitiallyDeferred` | Due opzioni nella famiglia `func(*FK)`, due campi snapshot, due keyword. Rende costruibile una forma di schema oggi **impossibile** (riferimento mutuo). `InitiallyDeferred` implica `Deferrable` nel costruttore | S | — |
| **S12** P2 | AutoTable vs generatore | Il commento di `cmd/dropsgen/schema.go` dice già "the two must agree, or a project that uses both would get two different schemas from one struct" — e poi `*string` compila nel generatore e va in panic in `AutoTable`. **Correggere per sottrazione:** il generatore è il path unico (risposta ent/sqlc, quella che gli sviluppatori Go accettano); `AutoTable` si riduce a comodità documentata per i test, o si deprecia adesso che deprecare è gratis | S | — |
| **S13** P3 | `Comment()` su pg | mysql e clickhouse ce l'hanno, il dialetto di riferimento no. `COMMENT ON` appeso da `CreateTableWithIndexes` | S | rumore al primo diff |
| — | ~~`PgRole`~~ | **Cancellato.** I ruoli sono un namespace co-posseduto dalla piattaforma; `CREATE ROLE` è cluster-wide ed è il DDL che il ruolo di migrazione tipicamente non può eseguire. La migrazione custom (M4) lo copre interamente | — | — |
| — | ~~`CRUDPolicies`~~ | **Cancellato.** Zucchero su quattro chiamate, con un tipo che simula una union TS e un `pg.Where` package-level che collide concettualmente con `.Where` di ogni builder. La metà di valore — *perché* UPDATE ha bisogno di USING **e** WITH CHECK — va nel doc comment di `Policy` e costa zero | — | — |
| — | ~~`GeneratedBy`~~ | **Cancellato.** Il doc comment proposto distrugge il proprio esempio di punta: `fn` è chiamata una volta per statement, quindi un `CreateMany` di tre righe riceve **un** ULID sulla colonna chiave primaria | — | — |

### 4.3 Migrazioni

| # | Voce | Design | Effort | Rischio |
|---|---|---|---|---|
| **M1** P1 | Lock di default sui migrator | `NewMigrator`/`NewDrizzleMigrator` prendono `pg_advisory_xact_lock` e **ri-leggono l'insieme applicato dentro il lock** prima di decidere — è la forma che `TreeMigrator.applyOne` usa già, quindi è un port. `WithoutLock()` come deroga nominata (precedente: `Seeder.WithoutTransaction`). `pg.WithAdvisoryLock` esiste in `pg/lock.go` e non lo chiama nessuno | S | sqlite non ha advisory lock: `BEGIN IMMEDIATE` + busy_timeout è un meccanismo diverso, merita il suo commento di divergenza, non un port finto |
| **M2** P1 | Verifica della history | `Verify(ctx) ([]HistoryProblem, error)` chiamata da `Up`. Per il runner Drizzle il formato è quello di drizzle-orm e non si tocca: si lavora sugli **hash orfani** (un hash applicato che nessun file corrente produce = file editato o cancellato). Per `Migrator`, `_drops_migrations` è di drops: colonna `checksum` additiva | M | i ledger esistenti non hanno checksum: `NULL` = "sconosciuto, non segnalare", o il primo upgrade fallisce ogni deploy |
| **M3** P1 | `WriteSnapshot` + ordinali | `pg/generate.go` è l'**unico** chiamante non-test di `Snapshot.Marshal()`: non c'è modo supportato di mettere un'introspezione dove `GenerateMigration` la legge. Gli ordinali di colonna viaggiano sotto il nodo `internal` dello snapshot (namespacati `internal.drops`), perché le mappe Go non hanno ordine e l'oggetto JSON sì | M | `internal` è di drizzle-kit: namespacare e dichiarare il rischio |
| **M4** P1 | `GenerateOptions.Custom` | Migrazione vuota numerata per l'SQL che il diff non esprime (estensione, trigger, funzione, backfill). Lo snapshot ripete lo stato precedente con ID nuovo e `PrevID` = ID precedente, così la catena resta intatta e il `generate` successivo diffa giusto. `Custom` + diff non vuoto = `ErrCustomWithChanges`. **È la valvola di sfogo che impedisce a un team di abbandonare drops la prima settimana**, e drops ne ha più bisogno di drizzle perché `Introspect` rilegge meno di quanto `BuildSnapshot` dichiari | S | una custom con DDL vera disallinea DB e catena: puntare a `DetectDrift` nel doc |
| **M5** P1 | `Baseline(ctx, throughTag)` | Scrive le righe di ledger fino a `throughTag` senza eseguire nulla, e **rifiuta** se il ledger non è vuoto (un baseline su una history viva è indistinguibile dal saltare una migrazione, e una delle due è sempre un errore). Con M3 l'adozione diventa due comandi | S | il caso "riparazione" è esplicitamente fuori scope: dirlo, invece di far crescere un gemello `Force` che reintroduce il problema di golang-migrate |
| **M6** P1 | `CheckChain` | Cinque regole offline: `missing-file`, `orphan-file`, `duplicate-idx`, `broken-chain`, `forked-chain`. Girano su una PR senza database. **La classificazione di commutatività non si fa:** "rami che toccano oggetti disgiunti si mergiano puliti" è falso in generale (enum condivisa, sequence condivisa, una FK che un ramo aggiunge a una colonna che l'altro ritipizza). Ogni fork è un fork: "avete entrambi ramificato da 0002, rigeneratene uno" è un messaggio completo e corretto | M | falsi positivi che addestrano la gente a passare il flag di bypass |
| **M7** P2 | `Down` sul runner Drizzle | `GenerateOptions.WithDown` produce oggi un file che **nessun runner legge**: `LoadEntries` legge solo `<tag>.sql`, e `Migrator.AddFS` pretende `<version>_<name>.up.sql` che il generatore non emette mai. `ErrDrizzleIrreversible` invece di uno skip silenzioso. Il file `.down.sql` è un'estensione drops che drizzle-kit ignora, quindi l'interop regge | S | incoraggia un'abitudine sbagliata in produzione: dirlo nel doc accanto al caveat che `WithDown` già fa |
| **M8** P2 | Render dello status | Tre shape di status (`Status`, `TreeStatus`, `DrizzleStatus`), nessuno `String()`, nessun formatter. Le funzioni `Render*` vivono in **`pgcmd`**, non nel package di dialetto: un dialetto che importa `text/tabwriter` per allineare una tabella da terminale è logica di presentazione in una libreria | S | — |
| **M9** P2 | Migrator MySQL | **Bloccato dalla decisione di tier (§6.4).** Se mysql resta supportato: port di `Migrator`/`DrizzleMigrator` con la divergenza dichiarata — il DDL MySQL non è transazionale, quindi riga di ledger scritta *prima* degli statement e completata dopo (un crash lascia una riga `Partial` visibile invece di un applica parziale silenzioso), `GET_LOCK`/`RELEASE_LOCK` su connessione pinnata, `Up` che ritorna il conteggio applicato accanto all'errore. Se mysql diventa sperimentale: **si smette di generare migrazioni mysql**. Ciò che non è difendibile è lasciare un generatore che emette file che nessuno esegue | M | `GET_LOCK` è session-scoped: un pool che rilascia su un'altra connessione fallisce in silenzio |

### 4.4 ORM

| # | Voce | Design | Effort | Rischio |
|---|---|---|---|---|
| **O1** P1 | `LoadInto` | **Promosso da P2: è il cuneo di adozione più importante del documento.** Gli sviluppatori Go non migrano ORM, compongono. `LoadInto` permette di tenere la propria query pgx/sqlc ottimizzata a mano e usare drops *solo* per il caricamento batched delle relazioni — la strada incrementale che porta una libreria in un codebase di produzione senza riscrittura. `loadRelationTree` opera già su una slice `reflect.Value`: il lavoro è separare la costruzione dell'albero dalla query radice | M | riusare `validateRelTree`; rifiutare un `dest` il cui elemento non c'entra con la tabella |
| **O2** P1 | `Columns` / `ExceptCols` | Proiezione su `EntityQuery[T]`, `FindBuilder`, `RelConfig`. **Nessun tentativo di restringere il tipo di ritorno** (servirebbero tipi condizionali): i campi non selezionati restano al valore zero e si documenta in una riga. Validazione a esecuzione: la proiezione deve contenere la PK quando la cache è attiva e la stitch key di ogni relazione in coda, con errore che nomina **la colonna e il metodo che ne ha bisogno** | M | la query cache è già chiavata su `sha256(SQL+args)` quindi la proiezione è nella chiave; la PK cache deve rifiutare di popolarsi da una query proiettata. Con P0-3: proiezione e `fastCols` sono due liste, vince la proiezione e si ricade sullo scanner a reflection |
| **O3** P1 | `Count` / `Exists` | Su `EntityQuery[T]` e `Entity[T]`, con la **stessa** iniezione tenant/guard/default filter di `All` — un count che non è d'accordo con la propria lista è peggio di nessun count. `ErrCountWithRelations` se ci sono relazioni in coda. `Exists` rende `SELECT EXISTS(SELECT 1 ... LIMIT 1)`. `Page[T].Total(ctx)` opt-in | S | saltare tenant/guard qui farebbe trapelare l'esistenza di righe attraverso un count: bug di sicurezza, serve un test |
| **O4** P1 | `HasRel` / `NotHasRel` / `CountRel` | **Free function che ritornano `drops.Expression`**, non metodi builder: compongono con `And`/`Or`/`Not` e funzionano in qualunque `Where` esista già, incluso `RelConfig.Where` un livello sotto, con zero superficie builder nuova. Handle `*Relation` → un typo è un errore di compilazione. Subquery costruita con `db.Select().From(rel.To)` quindi i default filter del figlio si applicano | M | correlazione corretta sotto `Table.As` (usare `Column.key()`); `MorphTo` non è correlabile e deve dare errore chiaro |
| **O5** P1 | `BindRelations` | Free function (Go non ha metodi generici) che risolve i campi riceventi a **tempo di dichiarazione**, verifica la forma contro il kind, va in panic con una diagnostica in stile `internal/drift` che nomina relazione, campo mancante e tag da aggiungere, e **memoizza** i path — oggi `relationTargetField` cammina il tipo per arco per query mentre `fieldMap` è in una `sync.Map`. In più: `pg.Relations.HasMany` sovrascrive in silenzio su nome duplicato mentre `mysql/relations.go` va in panic. Due dialetti in disaccordo sono un problema di credibilità a sé | M | deve restare **opzionale** o rompe ogni dichiarazione esistente |
| **O6** P1 | `Attach` / `Detach` / `Sync` | Basate su **chiavi**, mai sul cascade-save di un campo relazione popolato: "un save che riscrive righe di associazione perché una struct per caso le portava" è esattamente la sorpresa che questa API esiste per evitare — ed è il comportamento GORM. `Attach` idempotente con `ON CONFLICT DO NOTHING`, `Sync` in una transazione con la DELETE per ultima così due `Sync` concorrenti convergono | M | una junction con colonne payload NOT NULL deve dare errore chiaro, non fallire al driver |
| **O7** P1 | `SetFastBind` | Controparte in scrittura di `SetFastScan`; `Cols<T>()`/`Bind<T>()` sono oggi codice generato morto. Il generatore emette una func che produce `[]ColumnValue` **applicando già la regola di skip**, perché lo schema ce l'ha. Test differenziale obbligatorio: due path di binding, stesso struct, stesse binding. **Se non si riesce a renderli esattamente equivalenti, si cancellano `Bind<T>`/`Cols<T>` dal generatore** — codice generato morto è un costo di credibilità, un path di scrittura sottilmente diverso è un costo di corruzione dati | M | **dipende da P0-5**: la regola di skip è il bug |
| **O8** P2 | `BatchSize` | Chunking sequenziale delle chiavi padre (default 8192 su pg) — 100k padri superano il limite di 65535 parametri e falliscono con un errore opaco. Sequenziale apposta: il loader non deve moltiplicare in silenzio l'uso di connessioni del chiamante. In più: estendere `Budget.MaxArgs` alle query figlie, dove il controllo che esiste proprio per questo oggi non scatta | S | l'ordinamento resta per-padre, che è già così |
| **O9** P2 | `RelWhere` | Opzioni variadiche sui sei costruttori (source-compatibili: finiscono tutti con `ColRef`). `DefaultFilter` = "sempre, per questa tabella"; `RelWhere` = "sempre, per questo arco". Validare a dichiarazione che i predicati nominano solo colonne del target, panic con la colonna offendente | S | interazione con `RelConfig.Where` (AND, mai sostituzione) e con `Unscoped` da chiarire nel doc |
| **O10** P2 | Relazioni a chiave composita | `HasManyN`/`HasOneN`/`BelongsToN` con slice posizionali, come `ForeignKeyN`. Chiave tupla comparabile (`[]any` non è chiave di mappa) con encoding length-prefixed, e `IN` a row-value con fallback OR-di-AND documentato. **La contraddizione è il punto:** la storia di tenancy di drops spinge verso `(tenant_id, id)` e poi il layer di eager loading rifiuta di funzionarci | L | la collisione della chiave tupla: riusare il trucco già in `pg/cache.go` |
| **O11** P2 | `HasOne` deterministico | Primo-match vince (oggi vince l'ultimo, per assegnazione di mappa in un loop in avanti) **e** ORDER BY di default sulla PK del figlio quando il nodo non ne ha uno — senza, quale riga atterra sul padre è ciò che ha restituito il planner. È già quello che fa `buildPerParentLimitedSQL` per la stessa ragione. *Nota:* il doc dice "at most one row per parent", non "first match" — non c'è una promessa violata, c'è un comportamento indefinito | S | — |
| **O12** P3 | `Restore` su pg | sqlite ha `Entity.Restore`, pg no: il dialetto con il soft delete migliore ha la storia di recupero peggiore. Instradare per tenant/guard/audit/cache-invalidation è **l'argomento** per averlo sull'entità. Convergere le due firme su una sola forma, non spedirne due e chiamarle speculari | S | `Restore` non deve resuscitare una riga che il guard non avrebbe fatto cancellare: test |
| — | ~~`internal/eager`~~ | **Cancellato.** Il problema è verificato e grave (`mysql/relations.go` dichiara relazioni e non esiste alcun `FindBuilder` mysql: la dichiarazione compila, gira, e non fa nulla). Ma la `Dialect` proposta prende `Table`, `Column`, `Query` ed `Expr` cross-dialetto: non è un'interfaccia piccola, è un sistema di tipi ombra sopra quattro package duplicati apposta. **Si prende l'interim onesto: si cancella `mysql/relations.go` adesso.** Un'API assente batte un'API no-op | — | — |

### 4.5 Runtime

| # | Voce | Design | Effort | Rischio |
|---|---|---|---|---|
| **RT1** P1 | `PoolStats` che può funzionare | `drops.PoolStats` + `drops.PoolStatsProvider` nel root (così `stdlib` la soddisfa senza importare `pg`), metodo rinominato **`PoolStats()`** — non cosmetica: un driver che embedda `*sql.DB` non può soddisfare `Stats() pg.PoolStats` e `Stats() sql.DBStats` insieme. `stdlib` la implementa traducendo `sql.DBStats` campo per campo. Alias in `pg` per compatibilità. **Correggere le due affermazioni false in `pg/poolstats.go`.** Forward attraverso `Replicated` (per nodo) e `Sharded` (per shard) | S | l'interfaccia non è implementata da nulla nel tree tranne un fake di test |
| **RT2** P1 | `dropstest.Driver` | **Promosso: tre cose ne dipendono e niente dipende dal fatto che arrivi tardi** (i benchmark P0-13, i test su SQL renderizzato che P0-1 richiede, il test di redazione dell'envelope). Implementa i tre metodi di `drops.Driver`; stile *recorder*, non expectation-DSL: si asserisce con confronti Go normali. `sqlmock` è una dipendenza esterna che matcha SQL con regex e ha fama di test fragili; drops può spedire di meglio in ~150 righe **proprio perché `Driver` ha tre metodi** — e questo va detto nella documentazione, è un dividendo del design | S | `Statements()` mutex-guarded e documentata concurrent-safe |
| **RT3** P1 | `InTxAs` — RLS a runtime | **drops sa creare una policy e non sa soddisfarla.** `SET LOCAL ROLE` + `set_config(k, v, true)`: `LOCAL` è la parola portante, le impostazioni muoiono con la transazione, quindi una connessione pooled non può portare l'identità di una richiesta a quella dopo — che è il modo in cui ogni versione artigianale si rompe. Il ruolo non è parametrizzabile da PostgreSQL: si valida come identificatore e si rifiuta (`ErrInvalidRole`), non si interpola. **Compone con i Guard, non li sostituisce:** i predicati difendono l'app, le policy difendono il database. Da presentare insieme a P0-1 come una sola storia "Postgres multi-tenant" | M | sbagliarlo è un leak cross-tenant: copertura di integrazione contro un server reale con policy reali |
| **RT4** P1 | Scan plan cache | Piano risolto per `(tipo struct, insieme colonne)` invece che per riga: `fieldMap` è già memoizzata per tipo in una `sync.Map`, quindi è la continuazione naturale. `[][]int` indicizzato per posizione di colonna, `nil` = scarta; buffer di target allocato una volta per result set. **Zero cambi di API, e solleva mysql/sqlite/clickhouse che non hanno alcun fastpath.** Va costruito prima di qualunque idea di render-once, ed è ciò che i benchmark vedranno muoversi | M | `FieldByIndex` su un path attraverso un puntatore embedded nil va in panic (esposizione già presente). *Non* fare la parte `MakeSlice`+`SetLen`: `Rows` non dà il conteggio, quindi si cresce comunque a raddoppio |
| **RT5** P1 | Codegen per tutti i dialetti | `drops.Scanner` a un metodo issata nel root (`pg.Scanner` resta come alias); `SetFastScan`/`HasFastScan` portati su mysql, sqlite, clickhouse; tabella `target` + flag `-dialect` (default `pg`, invocazioni esistenti invariate); il `db` delle query generate da `sqlgen` diventa **un'interfaccia stretta** invece di `*pg.DB`, così una query generata funziona contro un DB legato a transazione e contro un doppio di test. Oggi `go generate` emette codice tipizzato su `pg.Scanner` che **non compila** per tre dei quattro dialetti pubblicizzati | M | ordine: dopo P0-3 (o tre dialetti ereditano il bug posizionale) e dopo la decisione di tier |
| **RT6** P1 | Tassonomia errori cross-dialetto | *(non era in nessuna proposta)* `errors.Is(err, drops.ErrUniqueViolation)` deve funzionare su ogni dialetto. Oggi ogni dialetto ha i suoi sentinel: la cosa più comune che il codice applicativo fa con un errore di database va scritta per dialetto. Sentinel radice in `drops`, i sentinel di dialetto ci si avvolgono sopra con `%w` | S | non aggiungere dipendenze: la classificazione SQLSTATE duck-typed di `pg/errors.go` è già il modello |
| **RT7** P1 | `PoolerMode` | Neon, Supabase e RDS Proxy sono dove vive la maggior parte dei nuovi deploy Postgres, e drops spedisce LISTEN, un changefeed che ci poggia sopra e advisory lock senza menzionare i pooler da nessuna parte. **Dichiarato, mai sniffato** (drops non può rilevarlo, e indovinare sbagliato è un'outage). Default `PoolerSession`, si opta *dentro* le restrizioni. In `PoolerTransaction`: `SupportsListen` → false, `Listen`/`Subscribe` → `ErrPoolerIncompatible` con il rimedio (l'outbox). `docs/pooling.md` con la tabella feature × modalità | S | — |
| **RT8** P2 | Health check delle repliche | `StartHealthChecks(ctx, interval) func()` — **start esplicito che restituisce una stop func**, come `StartPoolMetrics`, mai una goroutine lanciata da un costruttore. `pickReplica` salta i nodi down e ricade sul primary. `pickLSNReplica` tratta già un errore di query come "non allineata" e butta via il segnale. `Health()` per un endpoint di readiness. **Non-goal esplicito:** niente failover automatico, è il lavoro del cluster manager | M | flapping su errori transitori: soglia di fallimenti consecutivi, non ejection al primo errore |
| **RT9** P2 | Invalidazione della query cache | Contatore di generazione per tabella ripiegato nella chiave. Nessuna enumerazione di chiavi, nessuno SCAN → funziona identico su memory, Redis, memcached (di cui drops ha scritto a mano il protocollo, e che non ha iterazione) e tiered. Il set di tag deriva dalle tabelle in FROM/JOIN che il builder già conosce. Granularità tabella = sovra-invalidazione, che è la direzione che non può servire dati sbagliati | M | **nominare il modo di fallimento:** un read del contatore fallito deve mancare la cache, mai servire una chiave coniata prima di una scrittura sconosciuta; e va deciso cosa succede se il bump riesce ma la scrittura fa rollback |
| **RT10** P1 | Audit di goroutine e cancellazione | *(non era in nessuna proposta)* `StartPoolMetrics`, il worker outbox, il changefeed e ogni `Start*` futuro: verificare che onorino `ctx`, che non possano leakare, e che siano coperti da `-race` con un test che li avvia e li ferma. Regola 14 è nei vincoli e nessuna proposta la controllava | S | — |
| **RT11** P1 | Contratto di lifecycle su `Driver` | *(non era in nessuna proposta)* `drops.Driver` non ha `Close`, e `pg.DB.Close` lo duck-typa. Chi scrive un driver non ha modo di sapere che `Close` è atteso: il risultato è un pool leakato. Va documentato nel doc comment di `Driver` e nella guida "scrivere un driver" | S | — |
| **RT12** P1 | `ctx` sul path di retry | *(non era in nessuna proposta)* `InTx` con `RetryPolicy` deve controllare `ctx.Err()` tra i tentativi e definire cosa succede quando il deadline scade a metà backoff. Con P0-8 il loop serializable-retry diventa il path ctx-sensitive più caldo della libreria | S | — |
| **RT13** P1 | Fuzzing di quoting ed escaping | *(non era in nessuna proposta)* L'intera claim di sicurezza di drops è che i valori non arrivano sul filo come testo: `quoteIdent`, `mustIdent`, `quoteLiteral`, `Builder.AddArg`, `drops.Raw`. `testing.F` è nella stdlib. Per una libreria che emette SQL questa è un'omissione più grande di metà dei P2 | S | — |
| **RT14** P2 | Copertura `Scanner`/`Valuer` | *(non era in nessuna proposta)* Test e documentazione per `sql.Null*`, `driver.Valuer`, `sql.Scanner` custom, e un `nil` su un campo non-puntatore: i casi esatti dove uno scanner a reflection sbaglia in silenzio, e quelli che RT4 deve preservare | S | — |
| — | ~~`Batcher`~~ | **Cancellato dalla roadmap**, non rimandato. La proposta si smonta da sola: `CreateMany` rende già un solo INSERT, il worker outbox batcha, `mirror.Pump` batcha. Resta una coda lunga, servita da una capacità opzionale che un solo driver non ancora costruito può soddisfare — cioè esattamente la superficie spedita-e-irraggiungibile che P0-11 esiste per eliminare | — | — |

### 4.6 DX

| # | Voce | Design | Effort | Rischio |
|---|---|---|---|---|
| **DX1** P0-adiacente | **Epic "adopt"** | *(consolida quattro proposte indipendenti che avevano quattro layout di modulo diversi)* Un solo emitter in `internal/schemagen`, guidato da `dropsgen -schema`, `dropsgen -snapshot -decls` e dal verbo `drops pull`, così un progetto che usa entrambi non ottiene due schemi da un database. Emette `pg.NewTable`/`pg.Add` con modificatori, `AddUnique`/`PrimaryKey`/`AddCheck`/`References`/`ForeignKeyN`, `pg.NewIndex(...).AddIndex`, e le relazioni **in un `relations.go` separato** così una modifica a mano sopravvive alla rigenerazione. **Il bug che le altre tre proposte non nominavano:** `sqlToGoType` ricade su `return "string"` per qualunque tipo non riconosciuto, quindi `numeric(10,2)` diventa silenziosamente una stringa Go — codice generato che compila e corrompe dati. Un tipo SQL non mappabile è un **errore** che nomina il tipo e il flag `-type-map` che lo sovrascrive. Singolarizzazione con tabella di regole esplicita e sovrascrivibile. **Criterio di accettazione: introspect → generate produce un diff vuoto** (ed è per questo che S8, i nomi di vincolo, sale a P1) | L | round-trip mai totale: emettere un commento che nomina ciò che non ha potuto portare, invece di uno schema che dichiara in silenzio meno del database |
| **DX2** P1 | `RequireWhere` **di default** | Una DELETE o UPDATE senza WHERE compila, gira e svuota la tabella. `RequireWhere()` è una copia superficiale nell'idioma di `WithHook`; `pg/delete.go` ripiega già i `defaultFilters` nella lista di predicati, quindi tenant-scoped e soft-delete non sono toccati. `AllRows()` è **l'unica** deroga, greppabile in review. **Opt-in non protegge nessuno: chi farebbe l'errore è chi non chiama `RequireWhere()`.** Con zero tag il default è gratis da cambiare — si accende adesso, con `WithoutWhereGuard()` per il caso bulk-tool deliberato. Dopo v1 questa scelta è bloccata. È anche la feature più citabile del documento: il paragone con `ErrMissingWhereClause` di GORM si scrive da solo | S | l'analyzer `go/analysis` è **rimandato**: modulo separato, plugin golangci-lint e dipendenza x/tools per un guadagno marginale sopra la guardia a runtime |
| **DX3** P1 | Factory seeded + `Truncate` | `NewSeededFactory(e, seed, func(r *rand.Rand, seq int) T)` come **tipo distinto** (non lo stesso `Factory` con due contratti di concorrenza a seconda del costruttore: `*rand.Rand` non è concurrent-safe, e un generatore in gara non è deterministico). `Seeder.Truncate(tables ...*Table)` → un solo `TRUNCATE a, b RESTART IDENTITY CASCADE`, che rende riproducibili le chiavi generate e schiva l'ordinamento FK. **`drops/fake` non si fa:** l'ecosistema Go ha gofakeit, nessuno lo abbandonerà per una word list dentro un toolkit SQL, e riscriverlo per preservare lo slogan è lo slogan che guida l'architettura. Il template prende un `*rand.Rand`, quindi compone con qualunque faker l'utente ha già | S | il determinismo è una promessa: una volta che qualcuno pinna un seed, cambiare l'output di un generatore rompe la sua build |
| **DX4** P1 | Un workstream di documentazione, non quattro | *(consolida quattro proposte con la stessa causa radice)* I fatti verificati: `readme.md` dice "four methods" e `Driver` ne ha tre; dice "Two dialects ship today" sopra quattro bullet; mette MySQL sotto "What's not here" sessanta righe dopo averlo descritto; `docs/dialects.md` segna MySQL assente per paginazione, migrazioni e outbox mentre `mysql/page.go`, `mysql/generate.go` e `mysql/outbox.go` esistono, e segna ClickHouse completo dove c'è solo un runner; `docs/entities.md` mostra `WithAudit` concatenato quando è una **free function** (Go non ha metodi generici) — quello snippet non compila; `TestTx`, `Seeder`, `NewFactory` e `MermaidDiagram` compaiono in **zero** file `.md`; `doc.go`, che è la landing page di pkg.go.dev, omette mysql, sqlite, mirror, vector e otel. **La correzione non è patchare il readme.** Si riscrive corto — sotto 250 righe, un paragrafo di posizionamento, un esempio funzionante, una tabella di tier esplicita, link ai docs. Un readme lungo è un passivo quando è sbagliato, e a 1.142 righe tornerà sbagliato. Poi la regola diventa obbligatoria: **ogni claim portante è un `Example` compilato, o viene cancellato** | M | — |
| **DX5** P1 | Test CI sulla matrice dei dialetti | *(la metà durevole di DX4)* Un test che cammina le righe di `docs/dialects.md` e asserisce che il simbolo nominato esiste nel package nominato. Tre stati, non due: supportato / parziale / assente. `symbolExists` **non** deve tirare `golang.org/x/tools` nel root: si guida da output di `go doc`, o da un registro di simboli committato, o vive in un modulo suo | S | — |
| **DX6** P1 | Package di fiducia | *(espande una voce filata come faccenda P2)* `.github/` contiene **un** file. Servono: `SECURITY.md`, `CONTRIBUTING.md` che punta a `make check`, template issue/PR, `govulncheck` in CI, tag firmati, job di coverage (`make cover` esiste e nessun job lo esegue), dependabot **solo** su `integration/` (nel root non c'è nulla da guardare, e questo è il punto). Più le cose che la voce originale ometteva: un impegno di supporto dichiarato, un confine di scope scritto, e una dichiarazione pubblica su **come il codice è prodotto e come è verificato** — 49k righe di test sono l'artefatto di fiducia più forte che drops ha e non sono menzionate da nessuna parte. Correggere l'incoerenza di toolchain: `integration/go.mod` dichiara `go 1.25.0`, la CI pinna 1.24, il root punta a 1.22 — passa solo perché Go scarica in silenzio una toolchain più nuova, e chi ha `GOTOOLCHAIN=local` non riesce proprio a lanciare `make integration` | S | boilerplate non mantenuto è a sua volta un segnale: `CONTRIBUTING` corto e puntato |
| **DX7** P1 | Enforcement dell'API | *(non era in nessuna proposta)* Vedi P0-12 atto 3. Senza, il contratto di compatibilità è un paragrafo in un file markdown | M | lo strumento porta x/tools: modulo `analysis/` |
| **DX8** P2 | `drops diagram --format mermaid\|dot` | Il pezzo di Studio che ha valore reale a costo quasi nullo: `pg.MermaidDiagram` vede già i metadati di `Relation`, e l'output incollato in una PR renderizza nativamente su GitHub | S | — |
| **DX9** P2 | Integrazione con l'ecosistema | *(non era in nessuna proposta)* Niente su net/http, chi, echo; niente su come un `*pg.DB` e un tenant request-scoped arrivano a un handler; niente su testcontainers-go, che è **come** gli sviluppatori Go testano contro Postgres; niente handler di health/readiness; niente su `log/slog` malgrado `hook_logger.go`; `otel/` esiste, 553 righe, e non compare in nessuna pagina di docs. **Per una libreria la cui storia di tenancy dipende interamente da valori che arrivano in un `context.Context`, l'assenza di un pattern di middleware documentato è un buco strutturale**: il differenziatore non funziona finché qualcuno non lo cabla, e niente mostra come | M | — |
| **DX10** P2 | `AGENTS.md` | La metà gratis dell'idea MCP: quali verbi vogliono un DSN, che `push` è per database di scratch, che `meta/*_snapshot.json` è generato e `_journal.json` no. Serve agli umani almeno quanto agli agent | S | — |
| — | ~~`drops studio`~~ | **Cancellato.** Studio conta per Drizzle perché il tooling TS vive in un browser. Gli sviluppatori Go hanno già psql, pgcli, TablePlus, DataGrip, DBeaver, e nessuno sceglie una libreria per una versione peggiore di quelli. Era anche la voce più costosa dell'intera lista: una SPA embeddata, una superficie HTTP read/write legata a un DSN di produzione, e un carico di manutenzione frontend permanente. Sostituita da DX8 | — | — |
| — | ~~`drops mcp`~~ | **Cancellato.** "Nessun ORM Go ce l'ha oggi" è lo stesso ragionamento di "Drizzle ce l'ha", invertito. Un CLI con `--format json` ed exit code stabili è già adattabile da qualunque agent. Resta DX10 | — | — |
| — | ~~`dropsgen -validate`~~ | **Cancellato.** drizzle-zod ha valore in TS perché uno schema Zod paga quattro volte (parsing di richiesta, form, OpenAPI). In Go il `ValidateUser` generato ri-controlla vincoli che il database applica comunque al round trip successivo, non sa valutare le CHECK, e crea una seconda fonte di verità che marcisce tra due `go generate`. In più la regola derivativa centrale era sbagliata: `''` è un valore perfettamente legale in una colonna `text NOT NULL`, quindi il validatore generato rifiuterebbe dati che il database accetta, dentro codice che si dice all'utente di non modificare. `Entity.Validate` accetta già un `func(*T) error` | — | — |
| — | ~~`dropsgen -from gorm`~~ | **Riformulato.** La mappa di sette tag è una frazione del vocabolario GORM (`index`, `uniqueIndex`, `column`, `embedded`, `serializer`, `check`, `precision`, `foreignKey`, `many2many`, `constraint`, i marcatori `->`/`<-`/`-`), e la policy proposta era di **errore** su ogni chiave non mappata: su un codebase reale da cinquanta modelli fallisce al modello uno. Inoltre buona parte della verità di schema GORM sta nelle chiamate `AutoMigrate`, non nei tag. **La strada affidabile è quella che DX1 costruisce già:** eseguire `AutoMigrate` dell'app contro un database di scratch e poi fare `drops pull` | — | — |

---

## 5. Cosa NON copiamo da Drizzle

Questa sezione non è una scusa: è il filtro della §2 applicato, con la sostituzione Go
accanto. In quasi ogni riga la risposta Go è **più piccola** di quella TS, non più povera.

| Feature Drizzle | Perché non traduce in Go | Cosa facciamo invece |
|---|---|---|
| `drizzle.config.ts` + `defineConfig()` | Un file di config che è un programma nel linguaggio ospite è la risposta TS a non avere uno step di compilazione per il tooling. Go ce l'ha. Dividere la verità tra config e dichiarazioni crea due posti da controllare, e una chiave `dialect:` è ridondante quando l'utente ha già importato `drops/pg` (regole 1, 9) | `pgcmd.Env{Schema, Dir, Open}`: quattro campi, type-checked, driver visibile al call site. Variazione per ambiente = `os.Getenv` + un secondo `main.go`. **Niente `drops.json`** |
| `dbCredentials` (dieci shape ristrette per dialetto) | Una discriminated union su `dialect`+`driver`. Riprodurla in Go = type parameter che simulano structural typing (regola 6) o una struct gigante di `*T` opzionali (regola 8). Il valore è uno squiggle rosso nell'editor | Un `--dsn` per sottocomando, parsato dal driver che lo possiede, con errore tipizzato che nomina il componente mancante. L'equivalente Go dello squiggle è un errore chiaro, non una union a livello di tipo |
| I ~40 package driver (node-postgres, postgres.js, mysql2, better-sqlite3, bun:sqlite, sql.js, PGlite...) | Un artefatto npm: JS ha una dozzina di client concorrenti per database e nessuna interfaccia di connessione standard. Go ha `database/sql`, un driver canonico per database, e `drops.Driver` a tre metodi che chiunque implementa in 30 righe | `stdlib.New(*sql.DB)` copre ogni driver `database/sql` esistente e futuro. **Esattamente un** adapter scritto a mano vale la pena: pgx, per COPY/LISTEN/pool-stats/pipelining, che `database/sql` strutturalmente non esprime (P0-11) |
| Driver serverless HTTP/WS: Neon HTTP, Neon WS, PlanetScale, TiDB serverless, Xata, Vercel Postgres, AWS Data API, SQLite Cloud | Esistono perché Cloudflare Workers e runtime simili non hanno socket TCP, quindi l'SQL va tunnelato su `fetch`. Un binario Go in Lambda, Cloud Run, Fly o un container ha TCP normale | pgx o `database/sql` su TCP verso gli stessi vendor. Il bisogno realmente trasferibile — sapere **quali feature drops sopravvivono al transaction pooling** — è RT7, ed è documentazione più un predicato |
| Cloudflare D1, Durable Objects SQLite, Expo/OP-SQLite/React Native, bundled migrations, `useMigrations` | Runtime che non ospitano Go. Il macchinario di bundling esiste per aggirare l'assenza di un filesystem | `embed.FS`, che è nella stdlib e che drops usa già: `Migrator.AddFS` e `TreeMigrator.AddFS` prendono un `fs.FS`, quindi le migrazioni entrano nel binario senza generatore né bundler. **Strettamente più semplice** di ciò che Drizzle ha dovuto costruire |
| Bundle tree-shakeable da ~31KB | Nessun analogo. Il linker Go scarta già i package non referenziati, e la dimensione del binario non è ciò che blocca un deploy Go | Niente. Il tema adiacente reale è il cold start, che è dominato dall'apertura di connessioni → RT7 |
| Integrazione Effect + nove entry point `effect-*` | Effect esiste per dare a TypeScript errori tipizzati e concorrenza strutturata. Go ha entrambi dal 1.0 (regole 2, 9) | Valori di ritorno `error`, `errors.Is`/`As` sui sentinel che drops già definisce, `context.Context` come primo parametro ovunque |
| `$inferSelect` / `$inferInsert` / `InferSelectModel` | Computazione a livello di tipo pura. Riprodurla richiede mapped e conditional type; il tentativo Go finisce su `reflect` o `map[string]any` (regole 5, 6) | **La struct Go È il tipo modello**: è scritta, è controllata, non può essere inferita male perché non c'è niente da inferire. Il problema di drift che l'inferenza risolve lo risolve il panic di `NewEntity` allo startup, che elenca le colonne non legate e la chiamata di deroga. `dropsgen -schema` chiude l'altra direzione |
| `.$type<T>()` | Branding a livello di tipo, senza effetto runtime né DDL: esiste perché TS inferisce il tipo di colonna strutturalmente e a volte troppo largo (regola 6) | `pg.Custom[UserID]("id", "text")` — il type parameter di `*pg.Col[T]` **è** il tipo Go. `Col[UserID].Eq` rifiuta una `string` nuda, il che è più forte del branding |
| `optional: false` sulle relazioni to-one | Documentato come privo di significato a runtime: un'asserzione a livello di tipo che nulla applica (regole 1, 6) | La dichiarazione del campo Go lo dice già: `Author User` è richiesta, `Author *User` è opzionale, senza annotazione e senza gap tra tipo e runtime. Se la garanzia dev'essere vera, sta nello schema come FK NOT NULL, dove il database la applica |
| `defineRelations()` / `defineRelationsPart()` | Risolve import circolari tra file TS e la necessità di **un** valore su cui il type system possa camminare per l'autocomplete. Go non ha nessuno dei due problemi: l'ordine di init dei var è risolto dal compilatore, e un ciclo di import è un errore di compilazione. `defineRelationsPart` è l'ammissione che il pattern non scala | `NewRelations(t)` per tabella, accanto alla tabella. `Table.Rel(name)` trasforma ogni relazione in un identificatore Go che il compilatore verifica e che un rename refattorizza — la stessa discoverability, presa da simboli invece che da una camminata di tipi. L'artefatto unico rivedibile è `drops diagram` |
| `db.query.<table>.findMany({ with: {...} })` | Un namespace le cui chiavi sono nomi di tabella e il cui tipo di ritorno varia col `with`: servono mapped type. In Go diventa un registro `map[string]any` (regola 1) o codegen che non aggiunge nulla | `Entity[T].Query(db)` è già un builder tipizzato su una tabella, e la variabile package-level (`UserEntity`) **è** il namespace: un identificatore Go, verificato dal compilatore, raggiungibile con go-to-definition. Il nesting è `RelConfig.With`/`Load`/`LoadRel`, a profondità illimitata già oggi |
| Where object-syntax: `where: { age: { gt: 18 } }` con AND/OR/NOT e un escape RAW | In Go è `map[string]any` con un vocabolario di operatori a stringa risolto a runtime: zero controlli su nome colonna, operatore e tipo del valore, più un escape RAW che deve rientrare in un parser di stringhe (regole 1, 5) | I predicati sono già valori componibili: `pg.And(UserAge.Gt(18), UserVerified.Eq(true))` si mette in una variabile, si ritorna da una funzione, ed è tipizzato. Per il bisogno reale sotto (filtri che arrivano da query parameter HTTP) la risposta è codegen: una struct filtro per entità con campi tipizzati e un `ToPredicate`, cioè **un** file generato e leggibile invece di un risolutore a runtime |
| `.$dynamic()` | Esiste solo per uscire dal guard a livello di tipo che rimuove un metodo dopo la prima chiamata. Go non ha quel vincolo: `Where` due volte è già legale e già in AND (regole 6, 10) | Flusso di controllo normale — `if cond { s.Where(p) }` — più `Clone()` (Q1), che risolve il bisogno sottostante (frammenti riusabili non contaminanti) senza alcun mode switch |
| Il tagged template `` sql`` `` + `sql.raw`/`join`/`identifier`/`param` | La sintassi non ha equivalente Go e simularla richiede parsing di stringhe a runtime. La **capacità** dietro — un frammento componibile che lega valori e quota identificatori — è reale | `drops.Raw` (pre-formato, documentato non sicuro), `drops.Param`, `drops.ExprFunc` esistono già; `drops.Exprf`/`Ident`/`Join` (Q3) chiudono il gap ergonomico, nell'idioma `fmt`, con **deferred error** invece di panic |
| `customType({ dataType, toDriver, fromDriver })` | La metà conversione reinventa una coppia di interfacce che la stdlib spedisce e che ogni tool database Go già parla (regola 9) | `driver.Valuer` + `sql.Scanner` sul tipo Go, più `pg.Custom[T](name, typeSQL)` e `pg.Typed[T]` (S10). Tre esempi in-tree: `Money`, `Point`, `Secret[T]`. **Vantaggio netto:** un tipo custom scritto per drops funziona anche con `database/sql` e pgx, il che abbassa il costo di andarsene |
| `casing: 'snake_case'` e i preset per entità | Il nome SQL verrebbe da un'impostazione globale invece che dal call site (regola 1). Drizzle stessa sta rifacendo la cosa, perché ORM e kit devono applicare la regola identica o l'SQL generato smette di combaciare col DDL migrato — esattamente il fallimento che la configurazione implicita produce | Si scrive il nome: `pg.Text("first_name")`. Se ripeterlo pesa, `dropsgen -schema` deriva l'intera dichiarazione dai tag `drop:` e la emette in un file dove la mappatura è visibile |
| `bigserial({ mode: 'number'\|'bigint' })`, i mode integer/text/blob di SQLite | Compensano JavaScript: limite di interi sicuri a 53 bit, e nessun modo di distinguere un timestamp da un intero al confine di storage (regola 6) | Già presente: `pg.BigSerial` ritorna `*Col[int64]`, `sqlite.Timestamp` ritorna `*Col[time.Time]`, `sqlite.JSON` ritorna `*Col[json.RawMessage]`. **Il tipo Go È il mode** |
| `db.batch([...])` con tupla di risultati tipizzata | La tupla dipende da mapped type su un array eterogeneo; Go non esprime "una slice i cui elementi hanno tipi diversi per indice" senza tipizzare tutto ad `any`, che butta via l'unica ragione per cui la feature è piacevole. Il movente dell'ammortamento di round trip è anche più debole in Go, dove il processo è longevo e accanto al database | Per il bulk write drops ha già INSERT multi-riga, `CreateMany`/`UpsertMany` e `CopyFrom`. Il pipelining resta fuori roadmap finché un benchmark non lo giustifica |
| `.prepare()` + `sql.placeholder()` come leva di performance principale | Metà è già lavoro del driver e duplicarla lo ostacola: pgx mantiene una cache di statement per connessione, `database/sql` prepara le query parametrizzate da sé. Un `Stmt` di drops si romperebbe anche sotto transaction pooling in modi che drops non vede | La metà che drops possiede davvero è il render del builder, non misurato. Riaperto solo se P0-13 lo dimostra — e comunque **mai** con `Stmt.One(db, ctx, dest, args ...any)`, che butta via ogni garanzia di compilazione (regola 10) |
| JIT row mapper (`jit: true`, via `Function`) | Il meccanismo è `eval` a runtime, che Go non ha e non deve simulare | Due cose, entrambe migliori: il piano di scan risolto una volta per shape e riusato (RT4), e scanner generati su tutti e quattro i dialetti (RT5). Il piano è verificato a compile time e il codice generato è leggibile |
| Codecs / `refineCodecs` | Esiste perché node-postgres, postgres.js e pglite ritornano rappresentazioni JS diverse per lo stesso tipo di colonna, e JS non ha un'interfaccia che un valore possa implementare. Ricrearlo in Go è una tabella di lookup globale nascosta (regola 1) | `Scanner`/`Valuer` + il layer di colonne tipizzate esistente (`pg/array.go`, `json.go`, `money.go`, `geo.go`, `cast.go`) |
| Prompt interattivi rename-or-create (hanji, select con le frecce) | Richiede una TTY, si blocca all'infinito in CI, e produce una decisione che non lascia artefatto rivedibile. Drizzle stessa lo tratta come un errore: l'intero protocollo hints di 1.0 esiste per disfarlo. **Copiare ciò che il tuo metro sta attivamente rimuovendo non è parità** | `[]pg.Rename` come valori Go verificati dal compilatore (P0-7). La decisione è visibile nel diff della PR che la fa, sopravvive alla code review, e funziona identica su laptop e in CI |
| Il protocollo `--hints` / `--hints-file` (status `missing_hints`, exit 2, tuple ad arità variabile) | Ottimo fix per un problema che Go non deve creare. Serve a spostare una decisione dalla UI del terminale ai dati; in Go la decisione **era già dati**, perché il chiamante è un programma Go | `[]pg.Rename` e `PushOptions.Allow []pg.Destructive` (P0-7). Struct con campi nominati, verificate dal compilatore, che round-trippano in `encoding/json` gratis se una pipeline vuole salvarle — senza che drops inventi un protocollo per renderlo possibile |
| `drizzle-kit studio` + estensione Chrome + Studio Gateway + il componente embeddable a licenza | La regola 11 permette esplicitamente un binario self-contained, quindi non è violazione di principio: è **costo opportunità**. Nessuno sceglie un toolkit di schema per un browser di tabelle, e tutti hanno già psql/TablePlus/DBeaver. Il Gateway è una dipendenza da servizio ospitato; il componente a licenza è un modello di business | `drops diagram --format mermaid\|dot` (DX8): la parte di Studio che appartiene a una code review, resa da uno snapshot, senza database e senza browser |
| `drizzle-kit skills` (otto SKILL.md versionati con protocollo di staleness) | Il meccanismo di distribuzione è npm-specifico: un artefatto di package che si copia in un progetto e verifica la versione contro il binario installato. Go non ha quel canale, e inventarlo è reinventare il module graph | `AGENTS.md` committato, versionato col repository (DX10) |
| `drizzle-kit up` (upgrader del formato snapshot) | Non è un gap: drops ha esattamente una versione di snapshot in circolazione per dialetto e zero tag rilasciati. Non c'è nulla da cui aggiornare | È un **obbligo della regola 13 da schedulare**, non una feature da costruire. `Snapshot.Version` è già sul filo e `UnmarshalSnapshot` è già l'unico punto di lettura dove un dispatch di versione andrà. Va annotato nel doc comment adesso, così l'obbligo è registrato prima di essere dovuto |
| `drizzle-kit drop` (cancellazione interattiva di una migrazione) | Drizzle l'ha rimosso in 1.0, e aveva ragione: il messaggio sostitutivo è letteralmente "rimuovi la cartella a mano". Un comando il cui unico lavoro è riparare un file indice mutabile centrale è una prova contro il file indice | `rm` sui tre file, e `drops check` (M6) che segnala lo stato risultante su una PR. Il rilevamento batte un comando di cancellazione |
| Layout 1.0 folder-per-migration (niente journal) | La diagnosi è corretta — un `_journal.json` centrale è un merge conflict garantito. Ma adottare il fix costa l'unica cosa che nessun concorrente Go ha: l'interoperabilità byte-level con drizzle-kit. Scambiare un fossato reale per un conflitto che git segnala rumorosamente e che un umano risolve in trenta secondi è un cattivo affare, e cambiare formato prima che esista un v1 spedirebbe l'interop nel nulla per sempre | Si affronta il danno sottostante: `CheckChain` (M6) prende la metà invisibile della collisione — due snapshot che rivendicano lo stesso `prevId` — che è ciò che git non vede e ciò che corrompe davvero la baseline del diff. Da riconsiderare solo a un eventuale `/v2`, e la decisione va **registrata nel doc comment** |
| `extensionsFilters: ['postgis']` | Una lista di vendor hardcoded che deve crescere per sempre, un paper cut alla volta. `'postgis'` come stringa magica in una libreria Go è la regola 1 nella sua forma pura | La restrizione di proprietà (P0-6) la sussume completamente e in generale: una tabella che lo Schema Go non dichiara non è di drops da cancellare, che venga da PostGIS, da un altro servizio o da un DBA alle 3 di notte. **Una regola, nessuna lista** |
| Preset RLS Neon/Supabase (`authenticatedRole`, `anonRole`, `authUid`, `auth.users` pre-dichiarate) | Identificatori di vendor cotti dentro la libreria. Nel modulo root sono API permanente che non può restringersi (regola 13), e accoppiano un toolkit SQL generico ai nomi di ruolo *attuali* di due aziende di hosting | Due righe nel package dell'utente: `const SupabaseAuthenticated = "authenticated"`, usata in `pg.NewPolicy(...).To(...)`. Il problema di proprietà che i preset risolvono davvero lo risolve `Existing()` (S9), che mette il confine sulla dichiarazione |
| `pgTableCreator((name) => 'project1_' + name)` | Un'intera astrazione per ciò che in Go è una chiamata di funzione, e che impedisce a chi legge una dichiarazione di vedere il nome SQL reale (regola 1) | `func projTable(name string) *pg.Table { return pg.NewTable("proj1_" + name) }` — tre righe nel package dell'utente, prefisso visibile. Lo scoping delle migrazioni è `DiffOptions.Only`; la risposta di PostgreSQL, uno schema separato, è già `pg.NewSchemaTable` |
| `breakpoints: bool` | Un knob senza bisogno utente (regola 8). drops scrive sempre i breakpoint e onora già il flag per-entry in lettura, che è la superficie di compatibilità che conta | Niente. Si continua a scriverli sempre e a leggere il flag del journal per i file generati da drizzle-kit |
| Active record (`user.save()`, accessor lazy, model base) | Drizzle lo rifiuta deliberatamente e drops deve fare lo stesso: un metodo su una riga che apre una connessione nasconde da dove viene la query (regola 1), un `save()` senza `ctx` non può onorare cancellazione o deadline (regola 2), un accessor lazy che spara una query da una lettura di campo è irrevisionabile per la concorrenza (regola 14) | `Entity[T]` è un legame tra struct e tabella, tenuto in una variabile package-level, con ogni operazione che prende `(db, ctx)` esplicitamente. L'ordine db-first esiste **proprio** perché una entity serva un pool e una transazione, cosa che una riga active-record legata alla sua connessione non può fare |
| Identity map / unit of work / dirty tracking | Drizzle ne elenca l'assenza come punto di forza, e il ragionamento si trasferisce identico: una scrittura che avviene al flush invece che al call site è illeggibile (regola 1), e una cache mutabile session-scoped condivisa tra goroutine è una data race da documentare via (regola 14) | Statement espliciti. `Entity.Update` è un UPDATE cieco documentato come tale; `Entity.Patch` con `Set`/`Inc`/`Dec`/`SetIfChanged` copre gli aggiornamenti parziali lato SQL, atomicamente, senza read-modify-write — che è ciò per cui si va a cercare il dirty tracking, ed è strettamente meglio sotto concorrenza. Il caso lost-update lo copre il locking ottimistico con `ErrStaleObject` |
| Lazy loading | L'assenza più deliberata di Drizzle, ed è quella giusta. È il generatore di N+1 che ha allontanato la gente dagli altri ORM — e drops **spedisce un rilevatore di N+1**: costruire generatore e rilevatore nella stessa libreria sarebbe incoerente | Eager loading esplicito e batched, richiesto per nome o per handle verificato. La comodità che si cerca davvero nel lazy loading — riempire relazioni su righe già in memoria — è `LoadInto` (O1): esplicita, prende `ctx`, zero I/O nascosto |
| Rimozione totale di RQB v1 in 1.0, senza deprecazione | Non è una feature da eguagliare: è l'opposto di ciò che drops deve fare. Cancellare un'API centrale in una 1.0 è il comportamento che rende difficile dipendere da una libreria (regola 13) | La lettura strategica conta più di quella tecnica: è la finestra di churn utenti più grande nella storia di Drizzle, e il gruppo più esposto è quello che gira query relazionali Postgres in produzione — esattamente il carico che il dialetto pg di drops serve meglio. Fissa anche l'asticella: i breaking necessari (P0-3, P0-5, P0-8, RT1) atterrano **adesso**, non diventano una storia v2 |
| `drizzle-pulse` (SDK realtime su WAL) | Un prodotto separato che compete con Supabase Realtime ed Electric; costruire un runtime HTTP server+client dentro drops viola regola 4 e la disciplina di scope della regola 11 | Già presente a un livello più basso e più onesto: `pg/listen.go` avvolge LISTEN/NOTIFY e `pg/changefeed.go` installa trigger ed emette `TypedChange[T]`. Chi vuole streammarli su HTTP scrive l'handler — che è la divisione del lavoro corretta per un toolkit SQL |
| Tooling adiacente first-party (brocli, waddler, tento, hanji) | Un pattern organizzativo — costruirsi il substrato per tenere l'ORM senza dipendenze — non una capacità. In Go il substrato esiste e si chiama libreria standard (regola 9) | drops usa già `flag` invece di cobra: è la stessa decisione, presa più a buon mercato |
| `getTableColumns` / `getTableConfig` / `is()` | Non è un gap: drops ce l'ha, e in più posti | Già presente e tipizzato: `Table.Columns()`, `Col(name)`, `Indexes()`, `Policies()`, `Checks()`, `CompositePrimaryKey()`, `CompositeUniques()`, `CompositeForeignKeys()`, e su `Column`: `IsNotNull`, `IsPrimaryKey`, `HasDefault`, `ForeignKey`, `IsManaged`, `IsOptimisticVersion`. L'unica ergonomia mancante è il destructuring "tutte tranne questa", che diventa `pg.ExceptCols` (O2) |
| Il footgun di aliasing nelle callback (`orderBy`/`where.RAW`/`extras`) | È un difetto documentato di Drizzle, non una capacità: fallisce in silenzio e genera SQL sbagliato | Niente da costruire. Vale la pena citarlo nella documentazione come confronto di design: `Column.key()` fa collassare le copie alias sulla colonna dichiarata, quindi l'errore equivalente è **impossibile** invece che documentato |
| `QueryBuilder` standalone, `db.execute()`, `.toSQL()` | Già serviti, e con meno superficie | `pg.New(nil)` è un builder senza driver (usato nei test di drops); `ToSQL()` è su tutti e quattro i builder; `db.Exec`/`db.Query` prendono SQL raw; `drops.All[T]`/`One[T]` e `pg.ScanOne`/`ScanAll` tipizzano i risultati |

---

## 6. Il fossato

Le sezioni 3 e 4 sono manutenzione: portano drops al livello di ciò che uno sviluppatore
Go dà per scontato. Il fossato è dove drops deve **divergere e vincere**, e la roadmap non
deve mai spendere capacità qui per inseguire.

### 6.1 Zero dipendenze nel root, e il submodulo che lo rende vero

Enforced meccanicamente, non promesso. È l'intera conversazione con chi ha una review di
supply chain, ed è ciò che rende implementabili SQLcommenter, il tracing otel-shaped, la
cache Redis e il protocollo memcached senza tirare nulla.

**Ma finora è un fossato attorno a un castello vuoto**, perché ogni utente si scrive un
adapter pgx da un doc comment. P0-11 lo chiude: un `pgxdriver/` taggato che implementa
*ogni* capacità opzionale, e un paragrafo nel readme che spiega il design a due moduli.
Senza quel paragrafo, "zero dipendenze" si legge come una scusa; con quello, si legge come
una scelta.

### 6.2 I pattern di produzione: questo è il vero prodotto

Outbox transazionale con drain parallelo e ordinamento per aggregato sotto advisory lock
non bloccante. Saga con compensazione tipizzata. Event store con snapshot di aggregato.
Idempotency key. Backfill online riprendibile con gating sul lag di replica. Sharding.
Routing su replica LSN-aware con read-your-writes. Advisory lock. Changefeed. Tenancy,
authz, audit **nella stessa transazione della mutazione**. Cifratura envelope con DEK per
riga e KMS a due metodi. Budget per entità. Rilevatore N+1 sul contratto `Hook` ordinario.

Nessun ORM in nessun ecosistema ha questo insieme. In Go lo si assembla da riverqueue,
watermill, un event store, un plugin di audit e SQL scritto a mano.

**Il posizionamento che ne segue è: un team che sceglie drops non sta scegliendo un query
builder, sta scegliendo di non assemblare sei librerie.** I gap di query e migrazione
valgono la pena di essere chiusi *precisamente perché* la superficie circostante già
giustifica la scelta — ma oggi il readme non lo dice, e chi valuta non ci arriva mai.

### 6.3 Ampiezza di dialetti — con dei tier, non con una matrice tutta verde

pg + pgvector + PostGIS, mysql, sqlite, clickhouse con la DDL engine-centric (MergeTree,
ORDER BY, PARTITION BY, TTL, codec), qdrant, e `mirror.DeriveClickHouse` che deriva una
tabella analitica da una dichiarazione OLTP. Nessun concorrente Go ci arriva vicino.

**Ma quattro dialetti a quattro profondità diverse, mantenuti da una persona, non sono un
elenco di feature: sono quattro prodotti parziali** — ed è per questo che la matrice in
`docs/dialects.md` è sbagliata in entrambe le direzioni. Una tabella con una colonna
"parziale" e un tier sperimentale onesto è **più** persuasiva per chi valuta di quattro
colonne verdi che smonterà in un pomeriggio (DX5).

### 6.4 Codegen sopra reflection — e la disciplina di togliere

`cmd/dropsgen` copre già entrambe le direzioni: fastpath bind/scan senza reflection dalle
annotazioni di struct, e un compilatore `.sql` in stile sqlc che emette funzioni tipizzate.
ent dà il primo e non il secondo, sqlc il secondo e non il primo.

Perché sia un fossato e non una promessa a metà servono: la lista colonne esplicita (P0-3),
il write path (O7), e i quattro dialetti (RT5).

**E qui va la regola che non era nel set originale.** Novanta proposte, tutte additive, su
114.712 righe scritte in tre mesi. Se si costruisse ogni P0 e P1 originale sarebbero circa
sei mesi a tempo pieno per un solo maintainer, e il risultato sarebbe una superficie
*più grande* e ancora senza versione, con lo stesso problema di fiducia.

**Prima del v1 si decide cosa esce.** Candidati, per righe: `mirror` (~9.8k), `clickhouse`
(~7.5k), `cache` (~5k), `qdrant` (~2.9k), `vector` (~1.4k) — ~26.600 righe di feature che
suonano differenzianti e su cui **nessuno** sceglie tra GORM, ent, sqlc e bun, e che
collettivamente ritardano ogni voce che decide davvero il confronto. "Uscire" non significa
per forza cancellare: significa **tier sperimentale dichiarato** (§P0-12 atto 2), escluso
dal contratto di compatibilità e dal budget di manutenzione.

Applicato: `mysql/relations.go` si cancella (dichiara relazioni che nessun codice carica);
`AutoTable` si riduce a comodità per i test o si deprecia; `Bind<T>`/`Cols<T>` si collegano
o si cancellano; Studio, MCP, `-validate` e `drops/fake` non si costruiscono.

### 6.5 Il posizionamento che manca del tutto

Novanta proposte ottimizzavano feature senza mai rispondere a "perché dovrei scegliere
questo invece di sqlc". drops è oggi presentata come un superset di tutto — ORM, query
builder, motore di migrazioni, codegen, cache, tracing, outbox, saga, event store, vector
search, mirror ClickHouse — che a questo pubblico si legge come **framework**, e gli
sviluppatori Go rifiutano i framework più in fretta di quanto rifiutino le API brutte.

Serve un paragrafo per concorrente, e **ognuno deve concedere qualcosa di vero**:

- **vs sqlc** — sqlc è migliore se tutte le tue query sono statiche. Esistiamo per quelle
  che non lo sono, e `LoadInto` (O1) ti fa usare entrambi.
- **vs ent** — codegen più leggero, nessun runtime a grafo; ma lo schema-as-code di ent è
  più maturo.
- **vs GORM** — esplicito: nessuna inferenza sul valore zero, nessun cascade di
  associazioni, `RequireWhere` acceso di default.
- **vs squirrel** — tipizzato; e squirrel è in maintenance mode.
- **vs bun** — il concorrente più vicino. Divergiamo su tenancy e migrazioni.

Un confronto che non concede nulla viene letto come marketing e scartato.

---

## 7. Sequenza consigliata

Nessuna data. Ogni fase finisce in qualcosa di spedibile e annunciabile.

### Fase 0 — Il gate di correttezza *(niente tag prima della fine)*

P0-1 · P0-2 · P0-3 · P0-4 · P0-5 · P0-6 · P0-7 · RT2 (`dropstest.Driver`, che serve ai test
su SQL renderizzato) · RT10 · RT13.

Più la decisione di tier (§6.4) e la cancellazione di `mysql/relations.go`.

**Spedibile:** niente pubblicamente. **Prova di fine fase:** una suite avversariale che,
per ogni kind di relazione × ogni opzione di `RelConfig` × ogni executor, asserisce che il
predicato di scoping è presente **nell'SQL renderizzato**. Un test round-trip su fixture
mono-tenant passa mentre perde: non conta.

### Fase 1 — Esiste un comando, ed esiste una versione

P0-10 (CLI + envelope) · P0-11 (`pgxdriver`) · P0-12 (tag `v0.6.0`, contratto,
audit di superficie, `debug.ReadBuildInfo`, workflow di release) · M8 (render status
dentro `pgcmd`) · DX4 (readme riscritto corto) · DX5 · DX6.

**Annuncio:** *"drops v0.6.0: `go install`, `drops migrate up`, e un driver pgx che
implementa COPY, LISTEN e le pool stats."* È il primo momento in cui drops è valutabile
da qualcuno che non legge il sorgente.

### Fase 2 — La prova, e la storia di adozione

P0-13 (benchmark interni **e** comparativi) · RT4 (scan plan cache — è ciò che i benchmark
vedranno muoversi) · DX1 (epic adopt) · M3 · M5 · S8 · O1 (`LoadInto`) · RT1 · DX2
(`RequireWhere` di default).

**Annuncio:** *"Adotta drops su un database che hai già: due comandi, e il primo `generate`
produce un diff vuoto"* + la tabella di benchmark contro pgx a mano, sqlc, ent, bun e GORM,
col comando per riprodurla. Insieme sono la narrativa di adozione incrementale della §6.5:
`drops pull` per chi ha il database come verità, `LoadInto` per chi vuole tenere le proprie
query, `RequireWhere` come la riga citabile.

### Fase 3 — Postgres serio

P0-8 (isolation) · P0-9 (SKIP LOCKED, con `Outbox.Drain` riscritto sul builder) · Q5
(savepoint) · RT3 (`InTxAs`) · S1 · S3 · S4 · S5 · Q1 · Q2 · Q3 · Q7 · O3 · O4 · RT6 · RT7
· RT11 · RT12.

**Annuncio:** *"Postgres multi-tenant end-to-end: predicati nell'applicazione, policy RLS
nel database, transazioni serializzabili con retry, e code di lavoro senza SQL raw."*
P0-1, RT3 e P0-8 vanno presentati come **una** storia, non come tre feature scorrelate.

### Fase 4 — Migrazioni di cui fidarsi

M1 · M2 · M4 · M6 · M7 · S2 · S6 · S7 · S9 · Q4 · Q6 · O2 · O5 · O6 · O7 (dopo P0-5) ·
RT5 (dopo la decisione di tier) · DX7 (enforcement API).

**Annuncio:** *"Migrazioni a branch con verifica della history, consenso tipizzato per il
DDL distruttivo, e un `drops check` che gira su ogni pull request."*

### Fase 5 — Rifinitura, e la strada per v1

Q8 · Q9 · Q10 · Q11 · O8 · O9 · O10 · O11 · O12 · S10 · S11 · S13 · RT8 · RT9 · RT14 ·
DX3 · DX8 · DX9 · DX10 · M9 (secondo il tier).

Più i due artefatti che decidono se qualcuno mette dati di produzione dietro drops:
un'**applicazione di riferimento** completa e deployabile che esercita migrazioni, entità,
relazioni, tenancy e CLI end-to-end (valida l'API, genera i numeri, e cattura le
contraddizioni del readme in un colpo solo), e una **exit story** scritta — "i tipi drops
implementano `driver.Valuer`/`sql.Scanner`, il tuo SQL è visibile, ecco come si va via" —
perché un'uscita credibile è ciò che rende l'adozione reversibile, e quindi possibile.

**Disciplina di release, in ogni fase.** Ogni fase finisce in un tag. Ogni tag ha una voce
di changelog scritta prima del tag, mai dopo. Ogni breaking change nei package stabili
richiede un bump minor e una riga nel changelog; il job di apidiff lo fa rispettare.
`pgxdriver` è taggato separatamente (`pgxdriver/v0.x.y`), che la convenzione per-directory
di Go rende gratis.

---

## 8. Metriche di successo

Non "quante feature", ma "come sapremmo di essere un'alternativa credibile".

### Gate — vanno tutte verdi prima del primo tag

| Metrica | Soglia |
|---|---|
| Test su SQL renderizzato per lo scoping | Il predicato tenant/guard presente in **ogni** query di ogni kind di relazione × opzione `RelConfig` × executor |
| Test di equivalenza scanner | Reflection e fastpath generato producono lo stesso risultato per struct con embedding non esportato e nomi di colonna in collisione, su tutti e quattro i dialetti |
| Test di equivalenza binding | Path a reflection e `SetFastBind` producono binding identiche per lo stesso struct |
| `Push` su database condiviso | Zero `DROP TABLE` per tabelle non dichiarate; ogni DROP trattenuto compare come notice `unmanaged-table` |
| Test di redazione | Nessun comando fa comparire un DSN, un argomento legato o un valore di colonna `AsPII` nell'output, testato con una password sentinella |
| Fuzz su quoting | `testing.F` verde su `quoteIdent`, `quoteLiteral`, `mustIdent` |
| `-race` sui lifecycle | Ogni `Start*` si avvia e si ferma pulita sotto `-race`, nessuna goroutine sopravvive alla stop func |

### Adozione — cosa deve essere vero perché qualcuno provi drops

| Metrica | Soglia |
|---|---|
| Time-to-first-migration | `go install` → `drops migrate up` su un database vuoto: **< 10 minuti** partendo dal readme, senza scrivere un `main.go` per il caso di sola migrazione |
| Time-to-adopt | Database esistente → schema Go che compila → **`generate` produce un diff vuoto**: **< 30 minuti, due comandi** |
| Round-trip di introspezione | `pull` seguito da `generate` su un database di produzione reale: zero statement, zero rename di vincoli |
| Idempotenza di push | Due `push` consecutivi su schema invariato: il secondo è `no_changes`, **inclusi** enum, sequence, view e policy |
| Capacità raggiungibili | `pg.SupportsCopy`, `SupportsListen`, `PoolStats().ok` tutte `true` con `pgxdriver` — cioè zero interfacce dichiarate senza implementazione nel repository |
| Verbi CLI usabili in CI | `generate`, `check`, `diff`, `export` girano **senza database** con `Env.Open` nil |

### Credibilità — cosa deve essere vero perché qualcuno ci metta dati dentro

| Metrica | Soglia |
|---|---|
| Versione | `go get ...@latest` risolve a un tag semver; `drops version` e `changelog.md` concordano |
| Contratto di compatibilità | Scritto, e **imposto** da un job apidiff su ogni PR per i package stabili |
| Benchmark | Tabella pubblicata drops vs pgx-a-mano, sqlc, ent, bun, GORM sulle stesse query, con ns/op, allocs/op e il comando per riprodurla |
| Accuratezza della documentazione | Zero claim in `readme.md` e `docs/` che il test di matrice (DX5) contraddice; ogni claim portante è un `Example` compilato |
| Superficie esportata | Contata, e ridotta prima del tag: tutto ciò che non è nel contratto è sotto `internal/` o de-esportato |
| Postura di sicurezza | `SECURITY.md`, `govulncheck` in CI, tag firmati, threat model scritto per `drops.Raw`/`Exprf` e per le garanzie di redazione PII |
| Coverage | Pubblicata; le 49k righe di test esistenti smettono di essere un asset invisibile |
| Dogfooding | Un'applicazione di riferimento deployabile che esercita migrazioni, entità, relazioni, tenancy e CLI end-to-end |
| Posizionamento | Un paragrafo per concorrente (sqlc, ent, GORM, bun, squirrel), **ognuno con una concessione reale** |

### Il segnale che conta più di tutti

Il primo issue aperto da qualcuno che **non** è il maintainer, che riguarda una feature
usata in produzione — non "come si installa" e non "il readme dice X ma il codice fa Y".
