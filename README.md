# litreview — AI-assisted literature review

A single-binary Go web application that runs a literature search across open
academic databases, uses Claude to design the queries and screen the results,
lets you keep or drop each paper, and then downloads the PDFs of the kept
papers through a chain of access methods ending with your university's proxy.

```
research question ──► Claude writes per-database queries
                  ──► OpenAlex · Semantic Scholar · CrossRef · PubMed · CORE  (peer-reviewed only by default)
                  ──► de-duplication (DOI, then title)
                  ──► Claude screens every paper (score 0–100, keep / unsure / drop, rationale)
                  ──► you toggle keep / drop
                  ──► PDF fetch chain for kept papers
                  ──► bibliography export (.bib / .json) + Claude-drafted review section
```

## Requirements

* Go 1.22+ (stdlib only, no cgo, no external modules)
* Optional: `pdftotext` (poppler-utils) for full-text evidence in the table and synthesis
* Claude access, either of:
  * the **Claude Code CLI** logged in with your Claude subscription (`claude /login`), or
  * an **Anthropic API key**
* A contact e-mail (Unpaywall requires one; OpenAlex and CrossRef serve polite-pool requests faster)

## Run

```bash
cp .env.example .env     # edit it
set -a; source .env; set +a
go run ./cmd/litreview
# open http://localhost:8080
```

Or build once: `go build -o litreview ./cmd/litreview && ./litreview -addr :8080 -data data`.

All state lives in `DATA_DIR` as one JSON file per project plus `DATA_DIR/<project>/pdfs/`.

## Using your Claude subscription instead of an API key

If the `claude` CLI is installed and logged in, the app runs it in headless
mode (`claude -p --output-format json --tools ""`) for every Claude call. Usage
is billed against your plan's limits, not an API account.

* `AI_BACKEND=auto` (default): use `ANTHROPIC_API_KEY` if set, otherwise the CLI.
* `AI_BACKEND=claude-cli`: always use the CLI. `CLAUDE_CLI_MODEL=sonnet` (or
  `opus`, or a full model id) overrides your account's default model.
* `AI_BACKEND=api`: always use the API.

Each CLI call carries a few seconds of start-up overhead; screening runs in
batches of 20 papers, so a 100-paper review takes roughly 2–3 minutes. The
header of every page shows which backend and model are active. If the CLI
reports "Not logged in", run `claude /login` once in a terminal.

## Workflow

1. **Setup** – enter the research question, optional inclusion / exclusion
   criteria, year range, results per query, and tick the databases.
2. **AI search & screening** (background job, progress shown live) –
   Claude writes 2–3 queries per database in that database's own syntax
   (arXiv fielded booleans, PubMed `[tiab]` tags, keyword search for the rest).
   Sources are queried concurrently with per-API rate limiting; results are
   merged by DOI and normalised title. Claude then screens the unique papers
   in batches of 20 against your question and criteria.
3. **Keep / drop** – cards sorted by AI score show title, authors, year,
   venue, DOI, citation count, open-access badge, which databases found it,
   the AI verdict with a one-sentence rationale, and the abstract. Toggle
   **Keep** / **Drop** per paper (clicking the active choice clears it), filter
   by state, or use the bulk buttons (accept AI suggestions, keep/drop all
   undecided). The AI's keep/drop verdicts are pre-applied; "unsure" is left
   undecided for you.
4. **Download PDFs** – press *Proceed*. Three workers run the fetch chain for
   every kept paper and record each attempt. Failed papers get a *Retry*
   button; successful ones link to the stored file.
5. **Table & synthesis** – the master literature table (one row per included
   paper with Claude-extracted objective, method, context, findings, theory,
   contribution, limitations, relevance; Excel/CSV), and an **academic
   background section** drafted from the included papers.

## PRISMA 2020 flow diagram

*Open PRISMA* on the results step draws the PRISMA 2020 flow diagram as SVG
(downloadable) from the counts the app records: records identified per
database and via citation searching, removed before screening (duplicates,
preprint and language filters), screened, excluded with reasons (screeners,
preprints, non-English, researcher), reports sought and not retrieved,
reports assessed (full text obtained) and studies included. A methods
paragraph describing databases, search strings, rounds, screeners and counts
is generated alongside. There is no full-text exclusion box because the app
does not screen full texts.

## Concept matrix

*Open concept matrix* builds a Webster & Watson concept-centric matrix for
the included papers: Claude proposes 8–14 concepts from the master-table
extractions and keyword clusters (with definitions), then marks which
concepts each paper substantively addresses, with a short note per cell. The
matrix is shown in the app and exported to Excel and CSV. *Re-assign papers*
keeps the concepts; *Propose new concepts* starts over.

## Venue quality (JUFO, Norway, CORE, OpenAlex, AIS lists)

*Rate venues* on the keep/drop step rates each kept paper's venue from
several free registries and condenses them into a tier:

* **Finnish Publication Forum (JUFO)** level 0–3 via its REST API, looked
  up by ISSN or by a conference-aware fuzzy name match ("Proceedings of the
  32nd … (ECIS 2024)" matches "European conference on information
  systems"); the record also carries the **Norwegian** and **Danish** levels.
* **OpenAlex source metadata**: the curated *core* source list
  (≈ Scopus/Web of Science coverage), DOAJ membership, h-index, two-year
  mean citedness, source type and publisher.
* **CORE conference ranking** (A*, A, B, C) from the portal's CSV export,
  cached for 90 days.
* **Scimago Journal Rank** best quartile (Q1–Q4) and SJR from a CSV you
  upload on the Library access page (Scimago blocks automated downloads).
* **CrossRef's journal registry** resolves an ISSN from a journal name when
  the record came without one, so the ISSN-keyed lookups apply; name
  matching is strict (near-identical names only) to avoid confusing, say,
  JISEM with the Journal of Management Information Systems.
* Built-in lists: AIS Senior Scholars' basket of 8 / 11, major IS
  conferences (ICIS, ECIS, HICSS, PACIS, AMCIS, ACIS, WI, DESRIST, MCIS,
  SCIS) and reputable publishers/societies (IEEE, ACM, Springer, Elsevier,
  Wiley, SAGE, Emerald, INFORMS, AAAI, SciTePress, …).

Tiers: **top** (JUFO 3, basket of 8, CORE A*), **good** (JUFO 2, Norway 2,
basket of 11, major IS conference, CORE A), **basic** (JUFO 1, Norway or
Denmark ≥ 1, CORE B/C, OpenAlex core, DOAJ with h ≥ 20, reputable publisher),
**below** (JUFO 0, or a known journal with no index, no DOAJ listing and
h < 15), **unknown** (no registry knows it). Badges show the tier and the
signals; the *Quality venues*, *Below bar* and *Unknown venue* filters and
the *Drop below-bar venues* / *Drop unknown venues* actions use them; the
master table carries tier, JUFO, Norway, CORE, core/DOAJ and h-index
columns. Lookups are cached in `DATA_DIR/venues.json` and
`DATA_DIR/core-ranks.csv`.

## Keyword map (co-occurrence network)

*Open keyword map* on the results step draws the co-occurrence network of
the included papers' keywords: OpenAlex's curated keywords (fetched by DOI
with *Fetch database keywords*) plus the keywords Claude extracted for the
master table, normalised (synonyms such as LLM/LLMs/large language models
folded together). Nodes are sized by the number of papers, links by
co-occurrence, and clusters come from label propagation over
association-strength-normalised links, the normalisation VOSviewer uses. The
graph is drawn in the browser with vis-network; *VOSviewer map* and
*VOSviewer network* download the two tab-separated files VOSviewer opens via
File → Open, and *CSV* lists keyword, papers, cluster, links and total link
strength. Minimum occurrences, maximum keywords and scope (included papers or
all candidates) are adjustable.

## Send to Zotero

*Send to Zotero* on the results step exports the included papers with full
metadata, tags, the citation key (in *Extra*, so Better BibTeX reuses it), a
child note with the extracted aspects, and the PDF. Two routes:

* **Zotero desktop** (local API on port 23119, Zotero 7 with "Allow other
  applications on this computer to communicate with Zotero" enabled): the app
  creates the collection you name (or reuses it), adds items, notes and
  stores the PDFs in Zotero's own storage, so nothing counts against the
  cloud quota. The first write triggers Zotero's permission dialog; choose
  *Always Allow*. The export is idempotent: re-running it reuses existing
  items (matched by DOI or title) and only adds missing notes and PDFs.
* **Zotero Web API**: with an API key (zotero.org/settings/keys, library +
  write access) the app creates the collection itself, adds items and notes
  and uploads the PDFs to Zotero storage; the desktop syncs them down. The
  key is stored in `DATA_DIR/zotero.json` (mode 0600).

## Background section (academic synthesis)

The synthesis is written as the Background / related-work section of a
journal article, about 1,800–2,800 words: key concepts and definitions (with
the question's terminology mapped to the literature's), a thematic synthesis
that compares across studies, theoretical lenses, methodological
observations, and research gaps leading to the research question(s).

* **Evidence**: when `pdftotext` (poppler) is installed, downloaded PDFs are
  converted to text and the model reads full-text excerpts (start and end of
  each paper, ~5k characters per paper, 150k in total); otherwise abstracts.
  The extracted aspects of the master table are supplied as structured
  evidence, so run *Extract key aspects* first.
* **Citations**: every paper carries a stable key (`virtanen2024adoption`),
  used identically in the text and in `bibliography.bib`. The draft cites in
  pandoc syntax (`[@key]`); the LaTeX export converts to `\citep{}` /
  `\citet{}` and escapes special characters. Keys that are not in the
  included set are flagged in the job log.
* **Method paragraph**: databases, number of queries and rounds, snowballing,
  candidates screened, the two-screener procedure and the number of included
  studies are passed as facts so the opening paragraph is accurate.
* **Model**: `SYNTHESIS_MODEL=opus` (CLI backend) or a full model id (API
  backend) uses a stronger model for this single call while screening stays
  on Sonnet.

Exports: `background.md` (with a pandoc YAML header pointing at
`bibliography.bib`), `background.tex`, `bibliography.bib`, and from the
master table *Export LaTeX…*: a `longtable` of the included studies with a
selectable column set (default study, method, context, key findings,
theory; `?cols=` accepts study, year, venue, tier, objective, method,
context, findings, theory, contribution, limitations, relevance, keywords,
score) whose study column cites with `\citet{key}`. The concept matrix also
exports as a `longtable` with rotated concept headers (`concept-matrix.tex`).

## Reaching enough papers: target, expansion, snowballing

Set a **target** number of included papers (default 25). After each round the
app counts included papers; while below the target it asks Claude to study
which queries yielded relevant papers and which terminology the best-ranked
papers use, adds broader queries (retrieving twice as many per query), runs
them, screens only the new papers, and repeats up to two extra rounds. Each
query shows its round and its *relevant yield*.

**Snowball from kept papers** follows the citation graph via OpenAlex:
backward (the kept papers' reference lists) and forward (works citing them),
de-duplicates against what is known and screens the candidates. For niche
topics this is usually where most includes come from. Both are also manual
buttons on the search-strategy step.

If the question uses specialised or vendor terminology that the literature
does not, write inclusion criteria that state the standard terms; both
screeners judge against question *and* criteria.

## Peer-reviewed only, discipline, adjudication

* **Peer-reviewed only** (default on): records from preprint servers and
  repositories (arXiv, SSRN, OSF, bioRxiv/medRxiv, Research Square, Zenodo,
  figshare, ResearchGate, …; detected by DOI prefix, venue and OpenAlex type)
  are excluded at search and snowball time. arXiv is not offered as a
  database. Preprints already in a project carry a *preprint* badge and a
  *Drop N preprints* action.
* **English only** (default on): records whose database-reported language is
  not English, or, when none is reported, whose title and abstract fail a
  script and function-word check (catches Finnish or Russian theses with
  English titles), are excluded at search and snowball time, flagged with a
  *non-English* badge when already present, and removable with *Drop N
  non-English*.
* **Discipline and audience** (e.g. "Information Systems research;
  organisational and socio-technical perspective; target venues
  ECIS/ICIS/JAIS") is injected into every prompt: query vocabulary and venues,
  screening judgement, extraction (theories and levels of analysis named as
  the field does) and the register of the background section.
* **Adjudicate undecided**: a third-reviewer pass. Claude re-reads each
  undecided abstract together with both screeners' assessments and the
  criteria and commits to keep or drop (inclusive standard at abstract stage),
  with a confidence; decisions are applied at ≥ 60 % confidence, the rest stay
  undecided with the reason shown on the card. Threshold actions (keep ≥ X /
  drop < Y on the combined score) handle the remainder in bulk. Your own
  decisions are never touched.

## Two independent screeners (Claude + Jev)

Screening mirrors the two-reviewer practice of systematic reviews. Claude reads
each abstract and returns a 0–100 score, verdict and one-sentence rationale.
When `JEV_API_KEY` is set, TypeSafe's [Jev](https://typesafe.ai) decision model
independently answers three typed questions per paper: a 5-level relevance
rubric (probability-weighted score with confidence), *include for full-text
review?* (probability), and *matches an exclusion criterion?* (probability).
These are combined into a second 0–100 score (half relevance level, half
include probability; a confident exclusion match caps it at 39).

Decisions are pre-filled the way two human reviewers screen abstracts: a
paper is dropped only when **both** drop it, kept when one keeps it and the
other does not object (keep or unsure), and left undecided when they clash
(keep vs drop, flagged **screeners disagree**, listed under the *Conflicts*
filter) or both are unsure. Your own keep/drop clicks are marked *you* and
survive re-screening. `SCREENER=claude` or `SCREENER=jev` runs one screener
only. Jev costs about $0.04 per million input tokens, i.e. fractions of a cent
per review.

## Undo and redo

Every action that changes the project's data (a keep/drop click, a bulk
action, adjudication, settings or query edits, searches, regenerating the
background section or the concept matrix) first saves a snapshot. The stage
bar then shows **Undo: <action>** and, after an undo, **Redo**. Up to 30
steps are kept per project in `DATA_DIR/<id>.history.json`. Undo restores
decisions, queries, settings and generated texts; it does not delete
downloaded files or retract a Zotero export, and it is disabled while a job
runs. *Accept AI suggestions* and the other bulk actions never override a
decision you made by hand.

## Going back and regenerating parts

The step bar is navigable. Nothing is thrown away unless you press *Clear all
results*:

* **Setup** – edit question, criteria, years, databases; save without losing
  papers.
* **Search strategy** – see every query with the *exact API request URL*,
  the total hits the database reported and how many were retrieved. Edit
  queries by hand (one per line, in the database's syntax) and re-run them,
  regenerate them with AI, or re-screen all papers against changed criteria.
  Re-running a search adds new papers (de-duplicated against known ones) and
  screens only those; existing papers and your decisions stay.
* **Keep / drop** – change decisions at any time; the download button fetches
  only PDFs that are still missing.
* **PDFs** – retry individual failures or download newly kept papers.
* **Table & synthesis** – re-extract key aspects, regenerate the draft.

## PDF access chain

The chain is tried in this order and stops at the first valid PDF
(validated by the `%PDF-` signature; HTML landing pages are followed via
`citation_pdf_url` and PDF links):

| # | Method | Needs | Notes |
|---|--------|-------|-------|
| 1 | open-access metadata | links from the databases | OpenAlex `best_oa_location`, Semantic Scholar `openAccessPdf`, CORE `downloadUrl`, arXiv links |
| 3 | Europe PMC | PMCID | full-text render endpoint |
| 4 | Unpaywall | DOI + `CONTACT_EMAIL` | legal OA copies: publisher OA, repositories, preprints |
| 5 | publisher (direct) | DOI or URL | resolves `doi.org`; succeeds for OA articles and, when you are on the campus network / VPN, for subscribed content |
| 6 | university proxy | prefix + session cookie (Library access page) | genuine library access via EZproxy; targets with `&` use EZproxy's `qurl=` form |
| 7 | headless browser | a Chromium-based browser | last resort: loads the page in headless Chromium with your logged-in profile, follows JavaScript, clicks the PDF/download link and captures the PDF response. One paper at a time. `BROWSER_FETCH=off` disables it. |

Sci-Hub is deliberately **not** part of the chain: it redistributes
copyrighted articles without authorisation. Everything above uses either open
licences or your institution's subscriptions.

### Configuring university (EZproxy) access

Open **Library access** in the app header (`/access`), enter the proxy
prefix (University of Turku: `https://ezproxy.utu.fi/login?url=`) and press
**Open browser login**. The app launches Chromium with a private profile on
your library's login page; you sign in with Haka as usual; the app reads the
resulting `ezproxy` session cookie over the DevTools pipe, stores it, closes
the window and runs an access test. No password passes through the app. The
same profile is reused by the headless-browser download stage. Alternatively
paste the cookie by hand and press *Save & test access*. Settings are stored in `DATA_DIR/settings.json` (mode 0600)
and override `PROXY_PREFIX` / `PROXY_COOKIE`. No restart is needed.

For the publishers that matter most (Springer, Elsevier, Wiley, Taylor &
Francis, SAGE, ACM, IEEE, Emerald, INFORMS, World Scientific, SSRN, Nature,
PLOS, Frontiers) the app derives the direct PDF endpoint from the DOI and
tries it both directly and through the proxy, with browser-like request
headers. Papers that still fail show an *open via proxy* link and an upload
field on the PDFs step, and *Retry all missing* re-runs the chain after you
have fixed access.

Manual steps, if you prefer environment variables:

1. Find your library's proxy prefix. It is the part before the target URL in
   proxied links, e.g. `https://ezproxy.example.fi/login?url=`.
2. Log in to the proxy in your browser (open any proxied link and
   authenticate).
3. Open the browser's developer tools → *Application* / *Storage* → *Cookies*
   for the proxy domain and copy the session cookie, typically named
   `ezproxy` (some sites use `ezproxyl` / `ezproxyn` – copy all of them,
   separated by `; `).
4. Set `PROXY_PREFIX` and `PROXY_COOKIE=ezproxy=<value>` and restart.

Sessions expire (often after a few hours of inactivity); when the chain
reports "proxy redirected to login", refresh the cookie. Some publishers also
require a browser-like client for subscribed content; those papers will show
up as failed with the HTTP status in the attempt log, and you can fetch them
manually.

## Configuration

| Variable | Default | Purpose |
|----------|---------|---------|
| `JEV_API_KEY` | – | enables Jev as second screener (also `TYPESAFE_API_KEY`) |
| `JEV_MODEL` | `jev-latest` | Jev model id |
| `SCREENER` | `both` / `claude` | `both`, `claude`, or `jev` |
| `SYNTHESIS_MODEL` | backend default | model for the background-section call, e.g. `opus` |
| `AI_BACKEND` | `auto` | `auto`, `api` or `claude-cli` (see above) |
| `ANTHROPIC_API_KEY` | – | API backend; leave empty to use the CLI / subscription |
| `ANTHROPIC_MODEL` | `claude-sonnet-5-5` | model for the API backend |
| `CLAUDE_CLI` | `claude` | path to the Claude Code binary |
| `CLAUDE_CLI_MODEL` | account default | `--model` for the CLI backend |
| `CONTACT_EMAIL` | – | Unpaywall (required), OpenAlex / CrossRef polite pools, PubMed `email` |
| `S2_API_KEY` | – | Semantic Scholar key; the anonymous pool is shared and often returns 429 (free, strongly recommended) |
| `OPENALEX_API_KEY` | – | OpenAlex key; the free tier has a daily credit budget (1,000 credits, 10 per search) that searches, keyword fetches, snowballing and venue ratings all draw on |
| `CORE_API_KEY` | – | enables the CORE database |
| `PROXY_PREFIX`, `PROXY_COOKIE` | – | EZproxy access (see above) |
| `ADDR` | `:8080` | listen address |
| `DATA_DIR` | `data` | projects and PDFs |

## Project layout

```
cmd/litreview/            entry point
internal/config/          environment → Config
internal/model/           Project, Paper, Query, JobStatus …
internal/store/           JSON persistence, in-memory cache, locking
internal/browserlogin/    Chromium over the DevTools pipe: interactive proxy login (cookie capture) and headless PDF download
internal/prisma/          PRISMA 2020 counts, SVG diagram, methods paragraph
internal/venues/          AIS basket / IS conference lists and JUFO REST client with cache
internal/keywords/        keyword normalisation, co-occurrence network, clustering, VOSviewer export
internal/zotero/          Zotero desktop connector client and Web API client (collections, items, file upload)
internal/fulltext/        pdftotext wrapper: PDF → cleaned text, excerpts
internal/jev/             TypeSafe Jev client: typed relevance / include / exclude questions, score combination
internal/ai/              Backend interface (API key or headless claude CLI): GenerateQueries, ScreenPapers, Synthesize
internal/sources/         one adapter per database + Merge/de-duplication
internal/fetch/           Fetcher interface, chain runner, OA / publisher / proxy fetchers
internal/server/          HTTP handlers, background jobs, html/template views (htmx for toggles & polling)
```

## Tests

```bash
go test ./...                                     # offline unit + template tests
LIVE=1 CONTACT_EMAIL=you@x.fi go test ./internal/sources ./internal/fetch -run Live -v   # hits real APIs
```

## Notes and limits

* Politeness and quotas: OpenAlex/CrossRef/Unpaywall get `mailto`, PubMed is
  limited to 3 req/s, Semantic Scholar to one request per 3 s without a key,
  and 429 responses are retried with `Retry-After`/exponential back-off. A
  long `Retry-After` (a daily quota) fails fast with an explanation, and the
  search-strategy step shows a red banner naming any database whose queries
  all failed in the last round, so an empty-looking run is never silent.
* Jobs run in-process; a restart marks a running job as interrupted and
  returns the project to the previous stage. Decisions and downloaded files
  are preserved.
* Some hosts block non-browser downloads even for open papers (SSRN and
  Oxford Academic return 403, Springer serves an HTML page for paywalled
  chapters). These appear as failed with the status code in the attempt log;
  the university-proxy step or a manual download covers them.
* Abstracts are what the databases expose: CrossRef and some publishers do not
  deposit them, in which case screening relies on title and venue (the card
  says so).
* The screening prompt asks for calibrated 0–100 scores; treat them as a
  triage aid, not a replacement for reading.
