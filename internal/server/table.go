package server

import (
	"archive/zip"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"literaturereview/internal/ai"
	"literaturereview/internal/model"
)

// tableColumns defines the master literature table, in order.
var tableColumns = []string{
	"#", "Cite key", "First author", "Authors", "Year", "Title", "Venue", "Venue tier", "JUFO level", "Norway level", "CORE rank", "SJR quartile", "Venue flags", "Scopus/WoS-level (OpenAlex core)", "DOAJ", "Venue h-index", "Language", "DOI", "URL", "Citations", "Open access", "Found in",
	"Claude score", "Claude verdict", "Claude rationale", "Jev score", "Jev verdict", "Jev detail", "Screener agreement", "Decision by",
	"Objective", "Method", "Context & data", "Key findings", "Theory", "Contribution", "Limitations", "Relevance to RQ", "Keywords",
	"PDF", "PDF method", "PDF file", "Full text chars", "Abstract",
}

func tableRow(i int, p *model.Paper) []string {
	e := p.Extract
	if e == nil {
		e = &model.Extraction{}
	}
	pdf := string(p.Download.Status)
	if pdf == "" {
		pdf = "not attempted"
	}
	return []string{
		fmt.Sprint(i + 1), p.BibKey, p.FirstAuthorSurname(), strings.Join(p.Authors, "; "), yearStr(p.Year), p.Title, p.Venue, venueTier(p), jufoStr(p), levelStr(p, "no"), coreRank(p), sjrQ(p), venueFlags(p), venueBool(p, "core"), venueBool(p, "doaj"), hIndex(p), p.Language, p.DOI, p.URL,
		fmt.Sprint(p.Citations), yesNo(p.OpenAccess), strings.Join(p.FoundIn, ", "),
		fmt.Sprint(p.AIScore), string(p.AIVerdict), p.AIRationale, jevScore(p), jevVerdict(p), p.Jev.Detail(), p.Agreement(), p.DecisionBy,
		e.Objective, e.Method, e.Context, e.Findings, e.Theory, e.Contribution, e.Limitations, e.Relevance, strings.Join(e.Keywords, ", "),
		pdf, p.Download.Method, p.Download.Path, fmt.Sprint(p.TextChars), p.Abstract,
	}
}

func jufoStr(p *model.Paper) string {
	if p.VenueRating == nil || p.VenueRating.JufoLevel < 0 {
		return ""
	}
	return fmt.Sprint(p.VenueRating.JufoLevel)
}

func venueTier(p *model.Paper) string {
	if p.VenueRating == nil {
		return ""
	}
	return p.VenueRating.Tier
}

func levelStr(p *model.Paper, which string) string {
	if p.VenueRating == nil || p.VenueRating.NorwayLevel < 0 {
		return ""
	}
	return fmt.Sprint(p.VenueRating.NorwayLevel)
}

func sjrQ(p *model.Paper) string {
	if p.VenueRating == nil {
		return ""
	}
	return p.VenueRating.SJRQuartile
}

func coreRank(p *model.Paper) string {
	if p.VenueRating == nil {
		return ""
	}
	return p.VenueRating.CORERank
}

func venueBool(p *model.Paper, which string) string {
	if p.VenueRating == nil {
		return ""
	}
	v := p.VenueRating.OACore
	if which == "doaj" {
		v = p.VenueRating.DOAJ
	}
	return yesNo(v)
}

func hIndex(p *model.Paper) string {
	if p.VenueRating == nil || p.VenueRating.HIndex == 0 {
		return ""
	}
	return fmt.Sprint(p.VenueRating.HIndex)
}

func venueFlags(p *model.Paper) string {
	if p.VenueRating == nil {
		return ""
	}
	var parts []string
	if p.VenueRating.Basket != "" {
		parts = append(parts, p.VenueRating.Basket)
	}
	if p.VenueRating.ISConf != "" {
		parts = append(parts, p.VenueRating.ISConf)
	}
	return strings.Join(parts, "; ")
}

func jevScore(p *model.Paper) string {
	if !p.HasJev() {
		return ""
	}
	return fmt.Sprint(p.Jev.Score)
}

func jevVerdict(p *model.Paper) string {
	if !p.HasJev() {
		return ""
	}
	return string(p.Jev.Verdict)
}

func yearStr(y int) string {
	if y == 0 {
		return ""
	}
	return fmt.Sprint(y)
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

type tableData struct {
	Base      baseData
	P         *model.Project
	Papers    []*model.Paper
	Extracted int
	Running   bool
	Error     string
}

func (s *Server) handleTable(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var data tableData
	if err := s.store.Update(id, func(p *model.Project) error {
		p.EnsureBibKeys()
		data = tableData{Base: s.base(), P: p, Papers: p.Kept(), Running: p.Job != nil && p.Job.Running}
		for _, pp := range data.Papers {
			if pp.Extract != nil {
				data.Extracted++
			}
		}
		return nil
	}); err != nil {
		http.NotFound(w, r)
		return
	}
	if data.P.Stage == model.StageSetup || data.P.Stage == model.StageSearching || data.P.Stage == model.StageScreening {
		http.Redirect(w, r, "/projects/"+id+"?error="+urlEsc("the master table becomes available after you have decided which papers to keep and proceeded to download"), http.StatusSeeOther)
		return
	}
	data.Error = r.URL.Query().Get("error")
	s.render(w, "table", data)
}

func (s *Server) handleExtract(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	force := r.FormValue("force") == "1"
	var snap model.Project
	var todo []*model.Paper
	if err := s.store.View(id, func(p *model.Project) {
		snap = *p
		for _, pp := range p.Kept() {
			if force || pp.Extract == nil {
				c := *pp
				todo = append(todo, &c)
			}
		}
	}); err != nil {
		http.NotFound(w, r)
		return
	}
	if len(todo) == 0 {
		http.Redirect(w, r, "/projects/"+id+"/table?error="+urlEsc("all kept papers are already extracted; use Re-extract to redo them"), http.StatusSeeOther)
		return
	}
	err := s.runJob(id, "extract", func(ctx context.Context, lg jobLogger) error {
		lg.phase(1, 1, "Extracting key aspects", len(todo))
		client := s.client(ai.StepExtraction, &snap)
		lg.log("Extracting key aspects of %d kept papers with Claude (%s, %s), %d per call, 3 calls at a time…", len(todo), client.BackendName(), client.ModelLabel(), 6)
		per := 9000
		ev := map[string]ai.Evidence{}
		for _, e := range s.evidenceFor(ctx, id, todo, per) {
			ev[e.Paper.ID] = e
		}
		full := 0
		for _, e := range ev {
			if e.FullText {
				full++
			}
		}
		lg.log("Reading %d papers from full text and %d from abstracts.", full, len(todo)-full)
		err := client.ExtractPapers(ctx, &snap, todo, ev, func(done, total int) {
			lg.progress(done, total)
			lg.log("Extracted %d/%d", done, total)
			// Persist incrementally so a cancel keeps what was done.
			_ = s.store.Update(id, func(p *model.Project) error {
				for _, t := range todo {
					if t.Extract != nil {
						if pp := p.Paper(t.ID); pp != nil {
							pp.Extract = t.Extract
						}
					}
				}
				return nil
			})
		})
		if err != nil {
			return err
		}
		lg.log("Master table complete.")
		return nil
	})
	if err != nil {
		http.Redirect(w, r, "/projects/"+id+"/table?error="+urlEsc(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/projects/"+id+"/table", http.StatusSeeOther)
}

func (s *Server) handleTableExport(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var kept []*model.Paper
	var title string
	if err := s.store.View(id, func(p *model.Project) {
		title = p.Title
		for _, pp := range p.Kept() {
			c := *pp
			kept = append(kept, &c)
		}
	}); err != nil {
		http.NotFound(w, r)
		return
	}
	rows := [][]string{tableColumns}
	for i, p := range kept {
		rows = append(rows, tableRow(i, p))
	}
	switch {
	case strings.HasSuffix(r.URL.Path, ".csv"):
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="master-table.csv"`)
		_, _ = w.Write([]byte("\xEF\xBB\xBF")) // BOM so Excel opens UTF-8 correctly
		cw := csv.NewWriter(w)
		_ = cw.WriteAll(rows)
	case strings.HasSuffix(r.URL.Path, ".xlsx"):
		w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
		w.Header().Set("Content-Disposition", `attachment; filename="master-table.xlsx"`)
		if err := writeXLSX(w, "Master table", title, rows); err != nil {
			http.Error(w, err.Error(), 500)
		}
	default:
		http.NotFound(w, r)
	}
}

// writeXLSXRows is writeXLSX for arbitrary rows: the first fixedCols get
// wider columns, the rest are narrow (matrix-style).
func writeXLSXRows(w io.Writer, sheet, docTitle string, rows [][]string, fixedCols int) error {
	return writeXLSXWith(w, sheet, docTitle, rows, func(i int, col string) int {
		if i < fixedCols {
			if col == "Title" {
				return 50
			}
			if col == "Year" {
				return 7
			}
			return 18
		}
		return 22
	})
}

// writeXLSX emits a minimal single-sheet workbook with inline strings,
// a frozen header row and sensible column widths — no external deps.
func writeXLSX(w io.Writer, sheet, docTitle string, rows [][]string) error {
	return writeXLSXWith(w, sheet, docTitle, rows, func(i int, col string) int {
		switch col {
		case "#", "Year", "Citations", "Claude score", "Jev score", "Open access", "PDF", "Full text chars", "JUFO level", "Norway level", "CORE rank", "SJR quartile", "DOAJ", "Venue h-index":
			return 7
		case "Title", "Abstract":
			return 50
		case "Objective", "Method", "Context & data", "Key findings", "Theory", "Contribution", "Limitations", "Relevance to RQ", "Claude rationale", "Jev detail", "Authors":
			return 36
		}
		return 14
	})
}

func writeXLSXWith(w io.Writer, sheet, docTitle string, rows [][]string, width func(i int, col string) int) error {
	zw := zip.NewWriter(w)
	add := func(name, body string) error {
		f, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = io.WriteString(f, body)
		return err
	}
	if err := add("[Content_Types].xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
<Default Extension="xml" ContentType="application/xml"/>
<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>
<Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>
<Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>
<Override PartName="/docProps/core.xml" ContentType="application/vnd.openxmlformats-package.core-properties+xml"/>
</Types>`); err != nil {
		return err
	}
	if err := add("_rels/.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>
<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/package/2006/relationships/metadata/core-properties" Target="docProps/core.xml"/>
</Relationships>`); err != nil {
		return err
	}
	if err := add("docProps/core.xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:dcterms="http://purl.org/dc/terms/" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">
<dc:title>`+xmlEsc(docTitle)+`</dc:title><dc:creator>litreview</dc:creator>
<dcterms:created xsi:type="dcterms:W3CDTF">`+time.Now().UTC().Format("2006-01-02T15:04:05Z")+`</dcterms:created>
</cp:coreProperties>`); err != nil {
		return err
	}
	if err := add("xl/workbook.xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
<sheets><sheet name="`+xmlEsc(sheet)+`" sheetId="1" r:id="rId1"/></sheets></workbook>`); err != nil {
		return err
	}
	if err := add("xl/_rels/workbook.xml.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>
<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>
</Relationships>`); err != nil {
		return err
	}
	if err := add("xl/styles.xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
<fonts count="2"><font><sz val="11"/><name val="Calibri"/></font><font><b/><sz val="11"/><name val="Calibri"/></font></fonts>
<fills count="2"><fill><patternFill patternType="none"/></fill><fill><patternFill patternType="gray125"/></fill></fills>
<borders count="1"><border><left/><right/><top/><bottom/><diagonal/></border></borders>
<cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs>
<cellXfs count="3">
<xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/>
<xf numFmtId="0" fontId="1" fillId="0" borderId="0" xfId="0" applyFont="1"/>
<xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0" applyAlignment="1"><alignment wrapText="1" vertical="top"/></xf>
</cellXfs></styleSheet>`); err != nil {
		return err
	}
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetViews><sheetView workbookViewId="0"><pane ySplit="1" topLeftCell="A2" activePane="bottomLeft" state="frozen"/></sheetView></sheetViews><cols>`)
	header := rows[0]
	for i, col := range header {
		fmt.Fprintf(&sb, `<col min="%d" max="%d" width="%d" customWidth="1"/>`, i+1, i+1, width(i, col))
	}
	sb.WriteString(`</cols><sheetData>`)
	for r, row := range rows {
		fmt.Fprintf(&sb, `<row r="%d">`, r+1)
		for c, val := range row {
			style := 2
			if r == 0 {
				style = 1
			}
			fmt.Fprintf(&sb, `<c r="%s%d" t="inlineStr" s="%d"><is><t xml:space="preserve">%s</t></is></c>`, colName(c), r+1, style, xmlEsc(val))
		}
		sb.WriteString(`</row>`)
	}
	sb.WriteString(`</sheetData><autoFilter ref="A1:` + colName(len(header)-1) + fmt.Sprint(len(rows)) + `"/></worksheet>`)
	if err := add("xl/worksheets/sheet1.xml", sb.String()); err != nil {
		return err
	}
	return zw.Close()
}

func colName(i int) string {
	name := ""
	for i >= 0 {
		name = string(rune('A'+i%26)) + name
		i = i/26 - 1
	}
	return name
}

func xmlEsc(s string) string {
	var sb strings.Builder
	for _, r := range s {
		switch r {
		case '&':
			sb.WriteString("&amp;")
		case '<':
			sb.WriteString("&lt;")
		case '>':
			sb.WriteString("&gt;")
		case '"':
			sb.WriteString("&quot;")
		case '\t', '\n', '\r':
			sb.WriteRune(r)
		default:
			if r < 0x20 {
				continue
			}
			sb.WriteRune(r)
		}
	}
	return sb.String()
}
