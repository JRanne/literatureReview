# litreview

**AI-assisted systematic literature review, from research question to
background section.**

litreview is a single-binary web app you run on your own machine. You type a
research question; Claude designs a search for each academic database, the app
runs it, and two screeners (Claude, optionally plus TypeSafe Jev) rate every
paper. You decide what to keep. The app then downloads the PDFs (open access
first, then your university library), builds a literature table, a concept
matrix, a keyword map and a PRISMA 2020 diagram, and drafts an academic
background section with citations that match the exported BibTeX.

```
research question
  → Claude writes queries per database ─→ OpenAlex · Semantic Scholar · CrossRef · PubMed · CORE
  → de-duplication (DOI, then title)
  → screening: Claude (+ Jev) score every paper, keep / unsure / drop
  → you keep or drop
  → PDF download: open access → publisher → university proxy → headless browser
  → literature table · concept matrix · keyword map · PRISMA · background section
  → exports: .bib · .md · .tex · .xlsx · Zotero
```

Every step stays editable: change the question, edit queries, re-screen,
override any decision, undo. Nothing is thrown away unless you ask.

---

## Contents

- [Quick start](#quick-start)
- [Setting up .env](#setting-up-env)
- [Doing a review, step by step](#doing-a-review-step-by-step)
- [How to…](#how-to)
- [Reference](#reference)
- [Troubleshooting](#troubleshooting)
- [Development](#development)

---

## Quick start

### 1. Requirements

- **Go 1.27 or later** (the standard library only: no cgo, no external modules).
- **Access to Claude**, one of:
  - the **Claude Code CLI**, logged in with your Claude subscription
    (`claude /login`). No API key is needed and usage counts against your plan.
  - an **Anthropic API key**.
- **A contact e-mail address.** Unpaywall requires one, and OpenAlex and
  CrossRef answer faster when you send one.
- Optional:
  - `pdftotext` (poppler-utils), so the table and the synthesis read full texts
    instead of abstracts.
  - a Chromium-based browser (Chromium, Chrome, Brave), for university library
    login and headless PDF downloads.

### 2. Configure

```bash
cp .example.env .env
$EDITOR .env          # set at least CONTACT_EMAIL
```

The only required value is your e-mail address. With a logged-in Claude CLI
that is all you need to start:

```bash
CONTACT_EMAIL=you@university.fi
```

[Setting up .env](#setting-up-env) below walks through every value and where
to get each key.

### 3. Run

```bash
go build -o litreview ./cmd/litreview
./litreview                      # or: ./litreview -addr :8080 -data data
```

Open **http://localhost:8080**. The page header shows which AI backend and
model are active; "no AI backend" means neither the CLI nor an API key was
found.

All state lives in `data/`: one JSON file per project, plus
`data/<project>/pdfs/`. Back up that folder to keep your reviews.

---

## Setting up .env

All settings live in a file called `.env` in the folder you start the app
from. The repository ships a commented template, `.example.env`; copy it and
fill in the values you need:

```bash
cp .example.env .env
```

**Format rules**

- One `KEY=value` per line, with no spaces around `=` and no quotes needed:
  `CONTACT_EMAIL=you@utu.fi`.
- Leave a value empty (`S2_API_KEY=`) to use the default or switch the
  feature off.
- A `#` after a space starts a comment: `ADDR=:8080   # listen address`.
- A variable already set in your shell wins over the file, so
  `ADDR=:9090 ./litreview` overrides `.env` once.
- `.env` holds your keys and is gitignored. Never commit it, and do not paste
  it into issues or chats.
- Restart the server after editing `.env`; it is read only at start-up.

### Step 1: your e-mail (required)

```bash
CONTACT_EMAIL=you@university.fi
```

Unpaywall, one of the main sources of free PDFs, refuses requests without an
address. OpenAlex, CrossRef and PubMed put requests with an address in a
faster queue. The address is sent only to these services.

### Step 2: Claude (choose one)

**A. Claude subscription (Pro or Max), no API key.** Install the Claude Code
CLI and log in once:

```bash
claude /login
```

Leave `ANTHROPIC_API_KEY` empty and `AI_BACKEND=auto`. The app finds the
`claude` binary on your `PATH` and uses your plan. Set
`CLAUDE_CLI=/full/path/to/claude` if it is not on your `PATH`.

**B. Anthropic API key (pay per use).** Create a key in the
[Claude Console](https://platform.claude.com/docs/en/get-api-key) under
*Settings → API keys* (it is shown only once, so copy it right away) and set:

```bash
ANTHROPIC_API_KEY=sk-ant-...
```

With both available, `AI_BACKEND=auto` prefers the API key. Set
`AI_BACKEND=claude-cli` to force the subscription, or `AI_BACKEND=api` to force
the key.

### Step 3: database keys (free, recommended)

Searching works without keys, but the anonymous limits are low and a review
easily runs into them. All three keys are free:

| Setting | Where to get it | Why |
|---------|-----------------|-----|
| `S2_API_KEY` | request one on the [Semantic Scholar API page](https://www.semanticscholar.org/product/api) (arrives by e-mail) | the shared anonymous pool often answers "429 Too Many Requests" |
| `OPENALEX_API_KEY` | create a free account at [openalex.org](https://openalex.org) and copy the key from *Settings → API key* ([docs](https://developers.openalex.org/guides/authentication)) | without a key OpenAlex allows only a small daily budget; searches, snowballing, keyword fetches and venue ratings all use it |
| `CORE_API_KEY` | register at [core.ac.uk/services/api](https://core.ac.uk/services/api) | the CORE database is offered only when this is set |

CrossRef and PubMed need no key.

### Step 4: models per step (optional)

By default every step uses the base model: Sonnet (`CLAUDE_CLI_MODEL` or
`ANTHROPIC_MODEL`). To save cost on the many screening calls and get a
stronger background section:

```bash
MODEL_SCREENING=haiku       # always the newest Haiku
MODEL_SYNTHESIS=opus        # always the newest Opus
```

`haiku`, `sonnet`, `opus` and `fable` always mean the newest model of that
family, so they move to new Claude releases without any change. An exact id
such as `claude-opus-5-5` stays on that version.

The other steps are `MODEL_QUERIES`, `MODEL_ADJUDICATION`, `MODEL_EXTRACTION`
and `MODEL_CONCEPTS`. Each project can still override these in the app. See
[Choose a model per step](#choose-a-model-per-step).

### Step 5: second screener (optional)

For an independent second opinion on every paper, get a key from the
[TypeSafe console](https://console.typesafe.ai/settings/keys):

```bash
JEV_API_KEY=apikey_...
```

Screening then runs both Claude and Jev (`SCREENER=both`). See
[Use two screeners](#use-two-screeners).

### Step 6: university library (optional, usually not in .env)

Leave `PROXY_PREFIX` and `PROXY_COOKIE` empty and use **Library access** in
the app instead: it logs in through a browser window and keeps the session
up to date (see [Use your university library](#use-your-university-library)).
The `.env` values are only a fallback for machines without a browser.

### Check that it worked

Start the app and read the first log lines:

```
loaded 24 variable(s) from .env
AI backend: claude CLI in headless mode (/usr/bin/claude, model: sonnet)
AI model for Screening: claude-haiku-5-5
litreview listening on http://localhost:8080 (data: data)
```

- `loaded N variable(s)` counts the lines read from `.env` (empty ones
  included) that your shell had not already set. No such line means `.env`
  was not found: start the app from the folder that contains it.
- A `WARNING: CONTACT_EMAIL is not set` line means step 1 is missing.
- `WARNING: no AI backend` means neither a key nor a logged-in CLI was found.
- The server refuses to start on a misspelt `MODEL_*` name and lists the
  valid ones.
- The page header shows the active backend and model.

## Doing a review, step by step

A project moves through five steps, shown as a bar at the top of the page.
You can click back to any earlier step at any time.

### 1 · Setup

Fill in the form on the start page:

| Field | What to enter |
|-------|---------------|
| **Research question** | The question as you would write it in the paper. |
| **Discipline and audience** | For example *"Information Systems research; organisational perspective; target venues ECIS/ICIS/JAIS"*. This is added to every prompt and shapes the query vocabulary, screening judgement, extraction and the writing register. |
| **Inclusion / exclusion criteria** | Plain sentences. Both screeners judge against the question *and* these. If your question uses vendor or niche terms, state the standard academic terms here. |
| **Year from / to** | Publication range. |
| **Results per query** | 5–200 (default 25). |
| **Target included papers** | The number you want to end up with (default 25; 0 turns it off). While the review is below this, the search widens itself (see [Get more papers](#get-more-papers)). |
| **Databases** | Tick the sources. CORE needs `CORE_API_KEY`. |
| **Peer-reviewed only** | On by default. Drops preprints and repositories (arXiv, SSRN, OSF, Zenodo…). |
| **English only** | On by default. |

Press **Create & start AI search** to go straight to searching, or **Create
only** to adjust things first.

**Language models.** Open *Language models* at the bottom of the form to
choose a model for each step: query design, screening, adjudication, table
extraction, concept matrix and synthesis. Leave a step on *Default* to use the
server setting. Screening makes most of the calls, so *Latest Haiku* or
*Latest Sonnet* there saves the most; *Latest Opus* suits the background
section. You can change these later under *Setup → Language models*. See
[Choose a model per step](#choose-a-model-per-step).

### 2 · Search strategy

The search runs as a background job; the page shows live progress and a log.

1. Claude writes 2–3 queries per database in that database's own syntax
   (PubMed `[tiab]` tags, keyword search elsewhere).
2. The databases are queried in parallel, within each API's rate limit.
3. Results are merged by DOI and normalised title.
4. Every unique paper is screened in batches of 20.

When the job finishes, this step lists every query with its round, the exact
API request (**open API request ↗**), the number of hits the database reported,
how many were retrieved, and its *relevant yield*. From here you can:

- **edit queries by hand** (one per line, in the database's syntax), then
  **Save & run search**;
- **Regenerate queries & search** with AI;
- **Re-run these queries**;
- **Expand search (AI)**: Claude studies which queries worked and what wording
  the best papers use, then adds broader queries;
- **Snowball from kept papers**: follows references and citing works via
  OpenAlex;
- **Re-screen all papers** after you changed the criteria.

Re-running a search only adds new papers and screens only those. Existing
papers and your decisions stay.

A red banner names any database whose queries all failed (quota, rate limit),
so an empty-looking run never goes unexplained.

### 3 · Keep / drop

Papers appear as cards sorted by score. Each card shows the title, authors,
year, venue, DOI, citations, an open-access badge, which databases found it,
and each screener's score, verdict and one-sentence rationale.

- The AI's **keep** and **drop** verdicts are filled in for you; **unsure** is
  left undecided.
- Click **Keep** or **Drop** to decide. Clicking the active choice clears it.
  Your own decisions are marked *you* and are never overwritten by
  re-screening or bulk actions.
- **Filters**: all, keep, drop, undecided, conflicts (the screeners
  disagree), quality venues, below-bar venues and unknown venues.
- **Bulk actions**:
  - **Accept AI suggestions**, **Keep undecided**, **Drop undecided**.
  - **Threshold**: keep at or above X and drop below Y on the combined score.
  - **Adjudicate undecided (AI third reviewer)**: Claude re-reads each
    undecided paper with both screeners' assessments and commits to keep or
    drop. Decisions at 60 % confidence or more are applied; the rest stay
    undecided with the reason shown. Choose the *inclusive* (default) or
    *strict* standard.
  - **Rate venues**, then **Drop below-bar venues** / **Drop unknown venues**
    (see [Judge venue quality](#judge-venue-quality)).
  - **Drop N preprints** / **Drop N non-English** for papers found before you
    switched those filters on.

When you are done, press **Download N missing PDFs →**.

### 4 · PDFs

Three workers run the [PDF access chain](#pdf-access-chain) for every kept
paper and record each attempt.

- Successful papers link to the stored file.
- Failed papers show the attempt log, a **Retry** button, an *open via proxy*
  link and an **Upload** field for a PDF you fetched yourself.
- **Retry all N missing** re-runs the chain, for example after you logged in to
  the library.
- Papers marked **needs login** failed only because the library session
  expired. Press **Log in via browser**; they are retried automatically once
  the session is back.
- **Remove N without PDF from analysis** excludes the kept papers that have no
  full text. PRISMA reports them as *reports not retrieved*, not as screening
  exclusions. Undo brings them back.

For library access, set it up once under **Library access** in the header
(see [Use your university library](#use-your-university-library)).

### 5 · Table & synthesis

1. **Extract key aspects.** Claude fills the **master table**: one row per
   included paper with objective, method, context, findings, theory,
   contribution, limitations, relevance and keywords. With `pdftotext`
   installed it reads the full text. Export to `.xlsx`, `.csv` or a LaTeX
   `longtable`.
2. **Draft background section.** Claude writes the Background / related-work
   section of a journal article (about 1,800–2,800 words): key concepts and
   definitions, a thematic synthesis that compares studies, theoretical
   lenses, methodological observations, and the research gaps that lead to
   your question. Pick the model in the selector next to the button.
   **Regenerate** replaces the draft (Undo restores it).
3. Open the companion views from this step: **PRISMA**, **Concept matrix**,
   **Keyword map**, **Master table** and **Zotero**.

**Citations.** Every paper has a stable key such as `virtanen2024adoption`,
used identically in the draft and in `bibliography.bib`. The draft cites in
pandoc syntax (`[@key]`). The LaTeX export converts this to `\citep{}` /
`\citet{}`. The job log flags any key that is not among the included papers.

**Exports:**

| File | Contents |
|------|----------|
| `background.md` | the draft, with a pandoc YAML header pointing at `bibliography.bib` |
| `background.tex` | the draft in LaTeX |
| `bibliography.bib` / **Export .bib** | BibTeX for the included papers |
| **Export JSON** | the whole project |
| `table .xlsx`, `table .tex` | master table; `?cols=` picks LaTeX columns (study, year, venue, tier, objective, method, context, findings, theory, contribution, limitations, relevance, keywords, score) |
| `matrix .xlsx`, `matrix .tex` | concept matrix, with rotated concept headers in LaTeX |

To turn the Markdown draft into a document:

```bash
pandoc background.md --citeproc -o background.docx
```

---

## How to…

### Get more papers

- **Set a target.** After each round the app counts the included papers. While
  below the target, it asks Claude which queries yielded relevant papers and
  what wording the best papers use, adds broader queries (retrieving twice as
  many per query), screens only the new papers, and repeats for up to two extra
  rounds.
- **Snowball from kept papers.** Backward (reference lists) and forward (works
  citing them) through OpenAlex. On niche topics this is usually where most
  included papers come from.
- **Expand search (AI)** on demand from the search-strategy step.
- **Loosen the criteria** and press **Re-screen all papers**.

### Choose a model per step

Each step that calls Claude can run on its own model:

| Step | Server default | What it does |
|------|----------------|--------------|
| Query design | `MODEL_QUERIES` | writes and expands the queries |
| Screening | `MODEL_SCREENING` | scores every record; most of the calls |
| Adjudication | `MODEL_ADJUDICATION` | third-reviewer pass over undecided papers |
| Table extraction | `MODEL_EXTRACTION` | fills the master table |
| Concept matrix | `MODEL_CONCEPTS` | proposes and assigns concept columns |
| Synthesis | `MODEL_SYNTHESIS` | drafts the background section |

The app chooses the model for a step in this order:

1. the project's choice, made on the new-review form or under *Setup →
   Language models* (saved per project and undoable);
2. the server default `MODEL_<STEP>`, which accepts any model id or a family
   name (`haiku`, `sonnet`, `opus`, `fable`);
3. for adjudication and the concept matrix, `MODEL_SYNTHESIS` (`SYNTHESIS_MODEL`
   still works as an alias);
4. the backend model (`ANTHROPIC_MODEL` or `CLAUDE_CLI_MODEL`, default Sonnet).

A model you pick for one synthesis draft does not carry over to adjudication
or the concept matrix, so trying Fable on a draft never moves a bulk step onto
Fable. A misspelt `MODEL_*` variable stops the server at start-up and lists
the valid names. The job log names the model each step used.

**Which models are offered, and how the list stays current:**

- **Latest Haiku / Sonnet / Opus / Fable** always use the newest model in that
  family. When Anthropic releases a new Opus, every step set to *Latest Opus*
  moves to it with no change on your side. This is the recommended choice.
- **Pinned models** (API key only) stay on one exact version, for example to
  keep a review reproducible. With an API key, the app reads the models your
  account can use from Anthropic's Models API (`GET /v1/models`) and refreshes
  the list every six hours, so new releases appear in the selectors by
  themselves. Without network access it falls back to a built-in list.
- **With the Claude CLI** (subscription), only the *Latest* choices are shown:
  the CLI resolves `haiku`, `sonnet`, `opus` and `fable` to its newest model
  itself, and it has no way to list exact versions. A pinned id you set
  earlier, or through `MODEL_<STEP>`, still works and stays selected.

Example `.env` for cheap screening and a strong draft, both following new
releases:

```bash
MODEL_SCREENING=haiku
MODEL_SYNTHESIS=opus
```

### Use your university library

1. Click **Library access** in the header (`/access`).
2. Enter your library's EZproxy prefix, for example
   `https://ezproxy.utu.fi/login?url=` (University of Turku).
3. Press **Open browser login**. A Chromium window with a private profile
   opens on the library login page. Sign in as usual (for example with Haka).
4. The app reads the `ezproxy` session cookie over the DevTools pipe, saves
   it, closes the window and runs an access test. Your password never passes
   through the app.

The same browser profile is reused for headless downloads, and for renewing an
expired session silently while your university's single sign-on still
remembers you. The settings are stored in `data/settings.json` (mode 0600) and
override `PROXY_PREFIX` / `PROXY_COOKIE`. No restart is needed.

Without the browser login, paste the cookie by hand: log in to the proxy in
your own browser, copy the `ezproxy` cookie (and `ezproxyl` / `ezproxyn` if
present, separated by `; `) from the developer tools, paste it, and press
**Save & test access**.

The app's Chromium profile is separate from your everyday browser. Logging in
there does not log the app in.

### Judge venue quality

**Rate venues** on the keep/drop step looks up each kept paper's venue in
several free registries and condenses the results into a tier:

- **Finnish Publication Forum (JUFO)** level 0–3, by ISSN or a
  conference-aware fuzzy name match; the record also carries the **Norwegian**
  and **Danish** levels;
- **OpenAlex** source metadata: core source list (≈ Scopus/Web of Science),
  DOAJ, h-index, two-year mean citedness;
- **CORE conference ranking** (A*, A, B, C);
- **Scimago** best quartile and SJR, from a CSV you upload on the Library
  access page (Scimago blocks automated downloads);
- **CrossRef** journal registry, to find an ISSN when a record has none;
- built-in lists: the AIS Senior Scholars' basket of 8 / 11, major IS
  conferences (ICIS, ECIS, HICSS, PACIS, AMCIS, ACIS, WI, DESRIST, MCIS, SCIS)
  and reputable publishers.

| Tier | Meaning |
|------|---------|
| **top** | JUFO 3, basket of 8, CORE A* |
| **good** | JUFO 2, Norway 2, basket of 11, major IS conference, CORE A |
| **basic** | JUFO 1, Norway or Denmark ≥ 1, CORE B/C, OpenAlex core, DOAJ with h ≥ 20, reputable publisher |
| **below** | JUFO 0, or a known journal with no index, no DOAJ listing and h < 15 |
| **unknown** | no registry knows the venue |

The tiers drive the venue filters, the *Drop below-bar / unknown venues*
actions and the venue columns of the master table. Lookups are cached in
`data/venues.json` and `data/core-ranks.csv`.

### Build a concept matrix

Open **Concept matrix** on the table & synthesis step. Claude proposes 8–14
concepts from the master-table extractions and keyword clusters, each with a
definition, then marks which concepts each paper substantively addresses, with
a short note per cell (a Webster & Watson concept-centric matrix). Run
**Extract key aspects** first. **Re-assign papers** keeps the concepts;
**Propose new concepts** starts over.

### Draw a keyword map

Open **Keyword map**. It draws the co-occurrence network of the included
papers' keywords: OpenAlex's curated keywords (press **Fetch database
keywords**) plus those Claude extracted, with synonyms folded together. Node
size is the number of papers, link width the co-occurrence count, and clusters
use the association-strength normalisation VOSviewer uses. Adjust minimum
occurrences, maximum keywords and scope (included papers or all candidates).
Downloads: **VOSviewer map** and **VOSviewer network** (open both in VOSviewer
via File → Open), and a CSV.

### Report with PRISMA 2020

Open **PRISMA** for the PRISMA 2020 flow diagram (SVG, downloadable) built from
the counts the app records: records per database and from citation searching,
duplicates, preprint and language filters, screened, excluded with reasons,
reports sought and not retrieved, and studies included. A methods paragraph
(databases, search strings, rounds, screeners, counts) is generated alongside.
There is no full-text exclusion box, because the app does not screen full
texts.

### Send papers to Zotero

Open **Zotero** on the last step. It exports the included papers with full
metadata, tags, the citation key (in *Extra*, so Better BibTeX reuses it), a
note with the extracted aspects, and the PDF.

- **Zotero desktop** (Zotero 7, with *Allow other applications on this
  computer to communicate with Zotero* enabled): the app creates or reuses
  the collection you name and stores the PDFs in Zotero's local storage, so
  nothing counts against your cloud quota. The first write opens Zotero's
  permission dialog; choose *Always Allow*.
- **Zotero Web API**: with an API key from zotero.org/settings/keys (library
  and write access), the app uploads items and PDFs to Zotero storage and your
  desktop syncs them down. The key is stored in `data/zotero.json` (mode 0600).

Re-running the export is safe: it matches existing items by DOI or title and
adds only the missing notes and PDFs.

To add individual references outside a review, use the `addref` command:

```bash
go build -o addref ./cmd/addref
./addref -collection "My paper" 10.2307/249689 "Survey of hallucination in natural language generation"
./addref -collection "My paper" -refs refs.txt       # one DOI or title per line
```

It looks each reference up in OpenAlex, fetches the PDF through the same
access chain, and files it in Zotero desktop. It reuses `data/settings.json`
for library access and can run while the web app is running.

### Use two screeners

Screening mirrors the two-reviewer practice of systematic reviews. Claude
returns a 0–100 score, verdict and rationale for each abstract. With
`JEV_API_KEY` set, TypeSafe's [Jev](https://typesafe.ai) decision model
independently answers three questions per paper (a 5-level relevance rubric,
*include for full-text review?* and *matches an exclusion criterion?*). These
are combined into a second 0–100 score.

Decisions are pre-filled the way two human reviewers would:

- **dropped** only when both screeners drop it;
- **kept** when one keeps it and the other does not object;
- **undecided** when they clash (flagged *screeners disagree*, under the
  *Conflicts* filter) or both are unsure.

`SCREENER=claude` or `SCREENER=jev` runs a single screener. Jev costs fractions
of a cent per review.

### Undo a mistake

Every action that changes a project saves a snapshot first: decisions, bulk
actions, adjudication, settings, model choices, query edits, searches and
regenerated texts. The step bar shows **↶ Undo: <action>** and, after an undo,
**↷ Redo**. Up to 30 steps per project are kept in `data/<id>.history.json`.
Undo does not delete downloaded files or retract a Zotero export, and it is
disabled while a job runs.

---

## Reference

### Configuration

Set these in `.env` or in the shell; the shell wins. Copy `.example.env` as a
starting point and see [Setting up .env](#setting-up-env).

| Variable | Default | Purpose |
|----------|---------|---------|
| `CONTACT_EMAIL` | – | **required.** Sent to Unpaywall, OpenAlex, CrossRef and PubMed |
| `AI_BACKEND` | `auto` | `auto` (API key if set, otherwise the CLI), `api`, or `claude-cli` |
| `ANTHROPIC_API_KEY` | – | API backend. Leave empty to use the CLI and your subscription |
| `ANTHROPIC_MODEL` | `claude-sonnet-5-5` | base model for the API backend |
| `CLAUDE_CLI` | `claude` | path to the Claude Code binary |
| `CLAUDE_CLI_MODEL` | `sonnet` | base model for the CLI backend (your account's default model is never used implicitly) |
| `MODEL_QUERIES`, `MODEL_SCREENING`, `MODEL_ADJUDICATION`, `MODEL_EXTRACTION`, `MODEL_CONCEPTS`, `MODEL_SYNTHESIS` | base model | default model per step ([details](#choose-a-model-per-step)) |
| `SYNTHESIS_MODEL` | – | alias for `MODEL_SYNTHESIS` |
| `JEV_API_KEY` | – | enables Jev as the second screener (also read as `TYPESAFE_API_KEY`) |
| `JEV_MODEL` | `jev-latest` | Jev model id |
| `SCREENER` | `both` with a Jev key, else `claude` | `both`, `claude`, or `jev` |
| `S2_API_KEY` | – | Semantic Scholar key (free, strongly recommended) |
| `OPENALEX_API_KEY` | – | OpenAlex key; raises the small keyless daily budget (free account) |
| `CORE_API_KEY` | – | enables the CORE database |
| `PROXY_PREFIX`, `PROXY_COOKIE` | – | EZproxy access; the Library access page overrides these |
| `BROWSER_BIN` | first of chromium, google-chrome, brave | browser for library login and headless downloads |
| `BROWSER_FETCH` | `auto` | `off` disables the headless-browser download step |
| `ZOTERO_LOCAL` | `http://127.0.0.1:23119` | Zotero desktop connector |
| `ADDR` | `:8080` | listen address (also `-addr`) |
| `DATA_DIR` | `data` | projects, PDFs and settings (also `-data`) |

**Claude through your subscription.** With the `claude` CLI installed and
logged in, the app runs `claude -p --output-format json --tools ""` for every
call: plain text completion with no file or shell access. Each call has a few
seconds of start-up overhead. Screening runs 3 batches of 20 at a time, so 100
papers take roughly 2–3 minutes.

### PDF access chain

Tried in order, stopping at the first valid PDF (checked by its `%PDF-`
signature; HTML landing pages are followed via `citation_pdf_url` and PDF
links):

| # | Method | Needs | Notes |
|---|--------|-------|-------|
| 1 | open-access metadata | links from the databases | OpenAlex, Semantic Scholar, CORE |
| 2 | Europe PMC | PMCID | full-text render endpoint |
| 3 | Unpaywall | DOI + `CONTACT_EMAIL` | legal open-access copies |
| 4 | publisher, direct | DOI or URL | open-access articles, and subscribed content when you are on the campus network or VPN |
| 5 | university proxy | Library access set up | EZproxy; direct PDF endpoints are derived for Springer, Elsevier, Wiley, Taylor & Francis, SAGE, ACM, IEEE, Emerald, INFORMS and others |
| 6 | headless browser | a Chromium-based browser | last resort: loads the page with the app's logged-in profile, follows JavaScript and captures the PDF; one paper at a time |

Sci-Hub is deliberately **not** in the chain: it redistributes copyrighted
articles without authorisation. Everything above uses open licences or your
institution's subscriptions.

### Where data lives

```
data/
  <id>.json               one project: settings, queries, papers, decisions, drafts
  <id>.history.json       undo / redo snapshots
  <id>/pdfs/              downloaded PDFs (and extracted text)
  settings.json           library access (mode 0600)
  zotero.json             Zotero key (mode 0600)
  venues.json, core-ranks.csv   venue lookup caches
```

---

## Troubleshooting

| Symptom | Fix |
|---------|-----|
| Header says **no AI backend** | Install the Claude Code CLI and run `claude /login`, or set `ANTHROPIC_API_KEY`. |
| Claude errors with **Not logged in** | Run `claude /login` once in a terminal. |
| Server refuses to start: **`MODEL_…` names no review step** | A misspelt variable; the message lists the valid names. |
| A database returns nothing, red banner on the search step | Usually a rate limit or daily quota. Set `S2_API_KEY` / `OPENALEX_API_KEY`, or wait and **Re-run these queries**. |
| Many papers marked **needs login** | The library session expired. Press **Log in via browser** on the PDFs step; they retry automatically. |
| A paper keeps failing with HTTP 403 | Some hosts block non-browser downloads. Use the *open via proxy* link and **Upload** the PDF. |
| The table and synthesis only read abstracts | Install `pdftotext` (poppler-utils), then **Re-extract all** on the master table. |
| A card says it has no abstract | CrossRef and some publishers do not deposit abstracts; screening then relies on title and venue. |
| The server restarted during a job | The job is marked interrupted and the project returns to the previous step. Decisions and files are kept; start the job again. |

Screening scores are calibrated 0–100 values meant for triage. They do not
replace reading the papers.

---

## Development

```bash
go test ./...              # offline: unit, template and end-to-end tests with fake Claude, database and fetcher
go test -race ./...        # run before every commit; the job runners are concurrent
LIVE=1 CONTACT_EMAIL=you@x.fi go test ./internal/sources ./internal/fetch -run Live -v   # hits real APIs
```

Changes are test-driven: write the failing test first and keep its output,
then make it pass. Never loosen a test to get green. Fix a flaky or racy test
at its root cause rather than adding a retry, sleep or skip. Update this README
in the same change as a user-visible feature.

### Layout

```
cmd/litreview/            the web app
cmd/addref/               files single references in Zotero
internal/ai/              Claude backends (API, headless CLI), per-step model routing, prompts
internal/config/          environment → Config
internal/dotenv/          .env loader
internal/model/           Project, Paper, Query, decisions, citation keys
internal/store/           JSON persistence, locking, undo history
internal/sources/         one adapter per database, merge and de-duplication
internal/fetch/           PDF access chain: open access, publisher, proxy, browser
internal/browserlogin/    Chromium over the DevTools pipe: library login, headless download
internal/jev/             TypeSafe Jev client
internal/fulltext/        pdftotext wrapper
internal/venues/          JUFO, CORE, Scimago, OpenAlex, CrossRef, AIS lists
internal/keywords/        keyword normalisation, co-occurrence clustering, VOSviewer export
internal/prisma/          PRISMA 2020 counts, SVG and methods paragraph
internal/zotero/          Zotero desktop and Web API clients
internal/server/          HTTP handlers (one file per step), background jobs, templates
```

## License

[MIT](LICENSE). Retrieved papers remain subject to their publishers' terms;
the app only uses open licences and your own institution's subscriptions.
