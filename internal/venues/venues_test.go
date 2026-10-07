package venues

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStatic(t *testing.T) {
	if r := Static("MIS Quarterly"); r.Basket != "AIS basket of 8" {
		t.Fatalf("%+v", r)
	}
	if r := Static("Information & Management"); r.Basket != "AIS basket of 11" {
		t.Fatalf("%+v", r)
	}
	if r := Static("Proceedings of the 32nd European Conference on Information Systems (ECIS 2024)"); r.ISConf != "ECIS" {
		t.Fatalf("%+v", r)
	}
	if r := Static("Power System Technology"); r.Basket != "" || r.ISConf != "" || r.JufoLevel != -1 {
		t.Fatalf("%+v", r)
	}
}

func TestJufoLookupAndCache(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch {
		case r.URL.Path == "/etsi.php" && r.URL.Query().Get("issn") == "0276-7783":
			w.Write([]byte(`[{"Jufo_ID":"63427","Name":"Mis quarterly","Type":"Lehti/sarja"}]`))
		case r.URL.Path == "/etsi.php" && r.URL.Query().Get("nimi") != "":
			w.Write([]byte(`[{"Jufo_ID":"99","Name":"Decision support systems","Type":"Lehti/sarja"},{"Jufo_ID":"98","Name":"Decision support systems and tools","Type":"Lehti/sarja"}]`))
		case r.URL.Path == "/kanava/63427":
			w.Write([]byte(`[{"Name":"Mis quarterly","Level":"3","Jufo_2024":"3"}]`))
		case r.URL.Path == "/kanava/99":
			w.Write([]byte(`[{"Name":"Decision support systems","Jufo_2023":"2","Jufo_2024":"3"}]`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	j := NewJufo("test", filepath.Join(t.TempDir(), "cache.json"))
	j.Base = srv.URL
	r := j.Rate(context.Background(), "MIS Quarterly", "0276-7783")
	if r.JufoLevel != 3 || r.Basket != "AIS basket of 8" || r.JufoID != "63427" || r.Label() != "AIS basket of 8 · JUFO 3" {
		t.Fatalf("%+v", r)
	}
	r2 := j.Rate(context.Background(), "Decision Support Systems", "")
	if r2.JufoLevel != 3 || r2.JufoID != "99" || r2.Basket != "AIS basket of 11" { // exact name match, latest year
		t.Fatalf("%+v", r2)
	}
	n := calls
	if r3 := j.Rate(context.Background(), "MIS Quarterly", "0276-7783"); r3.JufoLevel != 3 || calls != n {
		t.Fatal("cache miss")
	}
	j2 := NewJufo("test", j.CachePath)
	j2.Base = "http://127.0.0.1:1" // unreachable: must come from the persisted cache
	if r4 := j2.Rate(context.Background(), "MIS Quarterly", "0276-7783"); r4.JufoLevel != 3 {
		t.Fatal("persisted cache not used")
	}
	if r5 := j.Rate(context.Background(), "Unknown Venue Of Nowhere", ""); r5.JufoLevel != -1 {
		t.Fatalf("unknown: %+v", r5)
	}
	_ = strings.ToLower
}

func TestCanonicalAndSimilarity(t *testing.T) {
	n, a := Canonical("Proceedings of the 32nd European Conference on Information Systems (ECIS 2024)")
	if a != "ECIS" || Similarity(n, "European conference on information systems") < 0.99 {
		t.Fatalf("canonical %q acro %q", n, a)
	}
	if r := Static("Proceedings of the 32nd European Conference on Information Systems (ECIS 2024)"); r.ISConf != "ECIS" {
		t.Fatalf("%+v", r)
	}
	if Similarity("Decision Support Systems", "Decision support systems and tools") > 0.8 {
		t.Fatal("too similar")
	}
}

func TestNoFalseFuzzyMatch(t *testing.T) {
	if Similarity("Journal of Information Systems Engineering and Management", "Journal of management information systems") >= 0.9 {
		t.Fatal("JISEM must not match JMIS")
	}
	canon, _ := Canonical("Proceedings of the 31st International Conference on Information Systems (ICIS 2023)")
	if Similarity(canon, "International conference on information systems") < 0.9 {
		t.Fatal("ICIS proceedings must match ICIS")
	}
	if Similarity("European Modern Studies Journal", "European journal of information systems") >= 0.9 {
		t.Fatal("false match")
	}
}

func TestTier(t *testing.T) {
	cases := []struct {
		r    Rating
		want string
	}{
		{Rating{JufoLevel: 3, NorwayLevel: -1, DenmarkLevel: -1}, "top"},
		{Rating{JufoLevel: -1, NorwayLevel: -1, DenmarkLevel: -1, Basket: "AIS basket of 8"}, "top"},
		{Rating{JufoLevel: -1, NorwayLevel: -1, DenmarkLevel: -1, CORERank: "A*"}, "top"},
		{Rating{JufoLevel: 2, NorwayLevel: -1, DenmarkLevel: -1}, "good"},
		{Rating{JufoLevel: -1, NorwayLevel: -1, DenmarkLevel: -1, ISConf: "ECIS"}, "good"},
		{Rating{JufoLevel: -1, NorwayLevel: -1, DenmarkLevel: -1, OACore: true}, "basic"},
		{Rating{JufoLevel: 0, NorwayLevel: -1, DenmarkLevel: -1}, "below"},
		{Rating{JufoLevel: -1, NorwayLevel: -1, DenmarkLevel: -1, OAFound: true, SourceType: "journal", HIndex: 5}, "below"},
		{Rating{JufoLevel: -1, NorwayLevel: -1, DenmarkLevel: -1, OAFound: true, SourceType: "conference", HIndex: 5}, "unknown"},
		{Rating{JufoLevel: -1, NorwayLevel: -1, DenmarkLevel: -1, Venue: "2024 IEEE Cloud Summit", Publisher: "Institute of Electrical and Electronics Engineers"}, "basic"},
		{Rating{JufoLevel: -1, NorwayLevel: -1, DenmarkLevel: -1}, "unknown"},
	}
	for _, c := range cases {
		if got := c.r.Tier(); got != c.want {
			t.Errorf("%+v → %s, want %s", c.r, got, c.want)
		}
	}
}

func TestOpenAlexAndCORE(t *testing.T) {
	oa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/sources/issn:0167-9236":
			w.Write([]byte(`{"id":"S1","display_name":"Decision Support Systems","type":"journal","is_core":true,"is_in_doaj":false,"works_count":5000,"summary_stats":{"h_index":232,"2yr_mean_citedness":8.4}}`))
		case r.URL.Path == "/sources":
			w.Write([]byte(`{"results":[{"id":"S2","display_name":"European Conference on Information Systems","type":"conference","is_core":false,"works_count":1388,"summary_stats":{"h_index":40}},{"id":"S3","display_name":"Something else entirely","type":"journal"}]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer oa.Close()
	o := NewOpenAlex("ua", "")
	o.Base = oa.URL
	if s := o.Lookup(context.Background(), "Decision Support Systems", "0167-9236"); !s.IsCore || s.HIndex != 232 {
		t.Fatalf("%+v", s)
	}
	if s := o.Lookup(context.Background(), "Proceedings of the 31st European Conference on Information Systems (ECIS 2023)", ""); !s.Found || s.ID != "S2" {
		t.Fatalf("%+v", s)
	}
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("1,AAAI Conference on Artificial Intelligence,AAAI,CORE2023,A*,Yes,1,,\n2,International Conference on Information Systems,ICIS,CORE2023,A,Yes,2,,\n3,Some Workshop on Things,SWT,CORE2023,C,Yes,3,,\n"))
	}))
	defer core.Close()
	c := NewCORE("ua", filepath.Join(t.TempDir(), "core.csv"))
	c.URL = core.URL
	if r := c.Rank(context.Background(), "Proceedings of the AAAI Conference on Artificial Intelligence"); r != "A*" {
		t.Fatalf("core rank %q", r)
	}
	if r := c.Rank(context.Background(), "International Conference on Information Systems (ICIS 2022)"); r != "A" {
		t.Fatalf("core rank by acronym %q", r)
	}
	if r := c.Rank(context.Background(), "Power System Technology"); r != "" {
		t.Fatalf("unexpected rank %q", r)
	}
	rt := &Rater{OA: o, CORE: c}
	if r := rt.Rate(context.Background(), "Proceedings of the AAAI Conference on Artificial Intelligence", ""); r.Tier() != "top" || !strings.Contains(r.Label(), "CORE A*") {
		t.Fatalf("%+v %s", r, r.Label())
	}
}

func TestScimago(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scimago.csv")
	_ = os.WriteFile(path, []byte("Rank;Sourceid;Title;Type;Issn;SJR;SJR Best Quartile;H index\n1;1;MIS Quarterly;journal;02767783, 21629730;7,13;Q1;236\n2;2;Journal of Information Systems Engineering and Management;journal;24680338;0,23;Q3;11\n"), 0o644)
	sc := NewScimago(path)
	if q, sjr, h, ok := sc.Lookup("", "0276-7783"); !ok || q != "Q1" || sjr < 7 || h != 236 {
		t.Fatalf("by issn: %s %v %d %v", q, sjr, h, ok)
	}
	if q, _, _, ok := sc.Lookup("Journal of Information Systems Engineering and Management", ""); !ok || q != "Q3" {
		t.Fatalf("by title: %s %v", q, ok)
	}
	if _, _, _, ok := sc.Lookup("Unknown", ""); ok {
		t.Fatal("unexpected hit")
	}
	rt := &Rater{Scimago: sc}
	if r := rt.Rate(context.Background(), "MIS Quarterly", "0276-7783"); r.Tier() != "top" || !strings.Contains(r.Label(), "SJR Q1") {
		t.Fatalf("%+v %s", r, r.Label())
	}
	if r := rt.Rate(context.Background(), "Journal of Information Systems Engineering and Management", ""); r.Tier() != "unknown" || !strings.Contains(r.Label(), "SJR Q3") {
		t.Fatalf("Q3 must not raise the tier: %s %s", r.Tier(), r.Label())
	}
}

func TestCrossRefISSNResolution(t *testing.T) {
	cr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"message":{"items":[{"title":"Journal of Information Systems Engineering and Management","ISSN":["2468-4376"]},{"title":"Journal of Management Information Systems","ISSN":["0742-1222"]}]}}`))
	}))
	defer cr.Close()
	c := NewCrossRefJournals("ua", "")
	c.Base = cr.URL
	issn, err := c.ISSN(context.Background(), "Journal of Information Systems Engineering & Management")
	if err != nil || issn != "2468-4376" {
		t.Fatalf("%s %v", issn, err)
	}
	jufo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/etsi.php" && r.URL.Query().Get("issn") == "2468-4376" {
			w.Write([]byte(`[]`))
			return
		}
		// name search returns JMIS; the strict matcher must reject it
		w.Write([]byte(`[{"Jufo_ID":"60910","Name":"Journal of management information systems"}]`))
	}))
	defer jufo.Close()
	j := NewJufo("ua", filepath.Join(t.TempDir(), "c.json"))
	j.Base = jufo.URL
	rt := &Rater{Jufo: j, CrossRef: c}
	r := rt.Rate(context.Background(), "Journal of Information Systems Engineering and Management", "")
	if r.ISSN != "2468-4376" || r.JufoLevel != -1 || r.Tier() != "unknown" {
		t.Fatalf("JISEM must stay unknown, got %+v", r)
	}
}

func TestOpenAlexCooldown(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(429)
	}))
	defer srv.Close()
	o := NewOpenAlex("ua", "")
	o.Base = srv.URL
	o.Lookup(context.Background(), "Some Journal", "")
	o.Lookup(context.Background(), "Another Journal", "")
	if calls != 1 {
		t.Fatalf("search must pause after a 429, calls=%d", calls)
	}
}
