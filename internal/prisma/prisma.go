// Package prisma derives PRISMA 2020 flow-diagram counts from a project and
// renders the diagram as SVG plus a methods paragraph.
package prisma

import (
	"fmt"
	"html"
	"sort"
	"strings"

	"literaturereview/internal/model"
	"literaturereview/internal/sources"
)

// Counts are the boxes of the PRISMA 2020 flow diagram.
type Counts struct {
	PerDatabase    map[string]int // records identified per database
	Databases      int            // sum of PerDatabase
	Citation       int            // records identified via citation searching
	Duplicates     int            // removed before screening
	Automation     int            // removed before screening by filters (preprint, language)
	Screened       int            // records screened (unique after removals)
	Excluded       int            // excluded at title/abstract screening
	ExcludedReason map[string]int // breakdown of exclusions
	Undecided      int            // still undecided (reported separately)
	Sought         int            // reports sought for retrieval (kept)
	NotRetrieved   int            // reports not retrieved (no PDF)
	Assessed       int            // reports assessed (PDF obtained)
	Included       int            // studies included in the review
}

// Compute derives the counts. Numbers recorded since the stats counters
// existed are used when present; otherwise they are reconstructed from the
// query log and paper flags, which under-counts duplicates and filtered
// records for older projects.
func Compute(p *model.Project) Counts {
	c := Counts{PerDatabase: map[string]int{}, ExcludedReason: map[string]int{}}
	for _, q := range p.Queries {
		if strings.HasPrefix(q.Query, "snowball") {
			c.Citation += q.Results
			continue
		}
		c.PerDatabase[q.Source] += q.Results
	}
	for _, n := range c.PerDatabase {
		c.Databases += n
	}
	if p.Stats.Retrieved > c.Databases {
		c.Databases = p.Stats.Retrieved
	}
	if p.Stats.Snowballed > c.Citation {
		c.Citation = p.Stats.Snowballed
	}
	c.Automation = p.Stats.Preprints + p.Stats.NonEnglish
	c.Screened = len(p.Papers)
	identified := c.Databases + c.Citation
	if p.Stats.Duplicates > 0 {
		c.Duplicates = p.Stats.Duplicates
	} else if identified > c.Screened+c.Automation {
		c.Duplicates = identified - c.Screened - c.Automation
	}
	for _, pp := range p.Papers {
		switch pp.Decision {
		case model.DecisionKeep:
			c.Sought++
			if pp.Download.Status == model.DownloadOK {
				c.Assessed++
			} else {
				c.NotRetrieved++
			}
		case model.DecisionDrop:
			c.Excluded++
			switch {
			case pp.Preprint || sources.IsPreprint(pp):
				c.ExcludedReason["Preprint or repository record (not peer reviewed)"]++
			case pp.NonEnglish:
				c.ExcludedReason["Not in English"]++
			case pp.DecisionBy == "user":
				c.ExcludedReason["Excluded by the researcher"]++
			default:
				c.ExcludedReason["Not relevant to the research question (screeners)"]++
			}
		default:
			c.Undecided++
		}
	}
	c.Included = c.Sought
	return c
}

// ReasonLines returns the exclusion breakdown sorted by count.
func (c Counts) ReasonLines() []string {
	type kv struct {
		k string
		v int
	}
	var kvs []kv
	for k, v := range c.ExcludedReason {
		kvs = append(kvs, kv{k, v})
	}
	sort.Slice(kvs, func(i, j int) bool {
		if kvs[i].v != kvs[j].v {
			return kvs[i].v > kvs[j].v
		}
		return kvs[i].k < kvs[j].k
	})
	var out []string
	for _, x := range kvs {
		out = append(out, fmt.Sprintf("%s (n = %d)", x.k, x.v))
	}
	return out
}

// DatabaseLines lists per-database counts, largest first.
func (c Counts) DatabaseLines(label func(string) string) []string {
	type kv struct {
		k string
		v int
	}
	var kvs []kv
	for k, v := range c.PerDatabase {
		kvs = append(kvs, kv{k, v})
	}
	sort.Slice(kvs, func(i, j int) bool { return kvs[i].v > kvs[j].v })
	var out []string
	for _, x := range kvs {
		out = append(out, fmt.Sprintf("%s (n = %d)", label(x.k), x.v))
	}
	return out
}

// SVG renders the PRISMA 2020 flow diagram (identification via databases and
// via other methods, screening, included).
func SVG(c Counts, label func(string) string) string {
	esc := html.EscapeString
	var b strings.Builder
	W, H := 980, 700
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" font-family="Helvetica, Arial, sans-serif" font-size="13">`, W, H, W, H)
	b.WriteString(`<style>.box{fill:#fff;stroke:#1d2430;stroke-width:1.2}.side{fill:#eef2fb;stroke:#1d2430;stroke-width:1.2}.lbl{fill:#1d2430}.b{font-weight:700}.band{fill:#2458d6}.bandt{fill:#fff;font-weight:700;font-size:12px}</style>`)
	// vertical phase bands
	bands := []struct {
		y, h int
		t    string
	}{{20, 150, "Identification"}, {200, 300, "Screening"}, {530, 140, "Included"}}
	for _, bd := range bands {
		fmt.Fprintf(&b, `<rect x="10" y="%d" width="26" height="%d" rx="4" class="band"/>`, bd.y, bd.h)
		fmt.Fprintf(&b, `<text transform="translate(27,%d) rotate(-90)" text-anchor="middle" class="bandt">%s</text>`, bd.y+bd.h/2, bd.t)
	}
	box := func(x, y, w, h int, lines []string, cls string) {
		fmt.Fprintf(&b, `<rect x="%d" y="%d" width="%d" height="%d" rx="6" class="%s"/>`, x, y, w, h, cls)
		ly := y + 20
		for i, l := range lines {
			cl := "lbl"
			if i == 0 {
				cl = "lbl b"
			}
			fmt.Fprintf(&b, `<text x="%d" y="%d" class="%s">%s</text>`, x+10, ly, cl, esc(l))
			ly += 17
		}
	}
	arrow := func(x1, y1, x2, y2 int) {
		fmt.Fprintf(&b, `<line x1="%d" y1="%d" x2="%d" y2="%d" stroke="#1d2430" stroke-width="1.2" marker-end="url(#ah)"/>`, x1, y1, x2, y2)
	}
	b.WriteString(`<defs><marker id="ah" markerWidth="8" markerHeight="8" refX="6" refY="4" orient="auto"><path d="M0,0 L8,4 L0,8 z" fill="#1d2430"/></marker></defs>`)
	// Identification
	dbLines := append([]string{fmt.Sprintf("Records identified from databases (n = %d):", c.Databases)}, c.DatabaseLines(label)...)
	box(60, 30, 300, 24+17*len(dbLines), dbLines, "box")
	box(380, 30, 250, 60, []string{"Records removed before screening:", fmt.Sprintf("Duplicate records (n = %d)", c.Duplicates), fmt.Sprintf("Filters: preprint / language (n = %d)", c.Automation)}, "side")
	box(660, 30, 290, 60, []string{"Records identified from other methods:", fmt.Sprintf("Citation searching (n = %d)", c.Citation)}, "box")
	arrow(360, 60, 380, 60)
	// Screening
	box(60, 220, 300, 44, []string{fmt.Sprintf("Records screened (n = %d)", c.Screened), fmt.Sprintf("title/abstract, two AI screeners + researcher")}, "box")
	exLines := append([]string{fmt.Sprintf("Records excluded (n = %d):", c.Excluded)}, c.ReasonLines()...)
	if c.Undecided > 0 {
		exLines = append(exLines, fmt.Sprintf("Still undecided (n = %d)", c.Undecided))
	}
	box(380, 220, 400, 24+17*len(exLines), exLines, "side")
	arrow(210, 30+24+17*len(dbLines), 210, 220)
	arrow(360, 242, 380, 242)
	box(60, 330, 300, 40, []string{fmt.Sprintf("Reports sought for retrieval (n = %d)", c.Sought)}, "box")
	box(380, 330, 300, 40, []string{fmt.Sprintf("Reports not retrieved (n = %d)", c.NotRetrieved)}, "side")
	arrow(210, 264, 210, 330)
	arrow(360, 350, 380, 350)
	box(60, 420, 300, 60, []string{fmt.Sprintf("Reports assessed for eligibility (n = %d)", c.Assessed), "full text obtained; no full-text exclusion stage"}, "box")
	arrow(210, 370, 210, 420)
	// Included
	box(60, 560, 300, 44, []string{fmt.Sprintf("Studies included in review (n = %d)", c.Included), fmt.Sprintf("with full text (n = %d)", c.Assessed)}, "box")
	arrow(210, 480, 210, 560)
	if c.Citation > 0 {
		arrow(805, 90, 805, 560)
		box(680, 560, 270, 44, []string{"Records from citation searching", "were screened with the others"}, "side")
	}
	b.WriteString(`</svg>`)
	return b.String()
}

// Methods writes a PRISMA-style search and selection paragraph.
func Methods(p *model.Project, c Counts, label func(string) string, screeners string) string {
	var sb strings.Builder
	dbs := c.DatabaseLines(label)
	fmt.Fprintf(&sb, "The literature was identified by searching %s", strings.Join(dbNames(c, label), ", "))
	if p.YearFrom > 0 || p.YearTo > 0 {
		fmt.Fprintf(&sb, " for publications from %d to %d", p.YearFrom, p.YearTo)
	}
	nq := 0
	for _, q := range p.Queries {
		if !strings.HasPrefix(q.Query, "snowball") {
			nq++
		}
	}
	fmt.Fprintf(&sb, ", using %d search strings", nq)
	if p.Rounds > 1 {
		fmt.Fprintf(&sb, " over %d rounds", p.Rounds)
	}
	fmt.Fprintf(&sb, " (%s). ", strings.Join(dbs, "; "))
	if c.Citation > 0 {
		fmt.Fprintf(&sb, "Backward and forward citation searching via OpenAlex added %d records. ", c.Citation)
	}
	fmt.Fprintf(&sb, "After removing %d duplicates", c.Duplicates)
	if c.Automation > 0 {
		fmt.Fprintf(&sb, " and %d records excluded by automated filters (preprint servers and repositories, non-English)", c.Automation)
	}
	fmt.Fprintf(&sb, ", %d records were screened on title and abstract. %s", c.Screened, screeners)
	fmt.Fprintf(&sb, " %d records were excluded at this stage (%s). ", c.Excluded, strings.Join(c.ReasonLines(), "; "))
	fmt.Fprintf(&sb, "Full texts were sought for the %d remaining records; %d could not be retrieved. ", c.Sought, c.NotRetrieved)
	fmt.Fprintf(&sb, "%d studies were included in the review, %d of them with full text available.", c.Included, c.Assessed)
	if c.Undecided > 0 {
		fmt.Fprintf(&sb, " (%d records are still undecided and are not counted as included.)", c.Undecided)
	}
	return sb.String()
}

func dbNames(c Counts, label func(string) string) []string {
	type kv struct {
		k string
		v int
	}
	var kvs []kv
	for k, v := range c.PerDatabase {
		kvs = append(kvs, kv{k, v})
	}
	sort.Slice(kvs, func(i, j int) bool { return kvs[i].v > kvs[j].v })
	var out []string
	for _, x := range kvs {
		out = append(out, label(x.k))
	}
	return out
}
