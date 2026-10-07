package sources

import (
	"strings"
	"unicode"

	"literaturereview/internal/model"
)

// englishStop are frequent English function words; a text with a normal share
// of them is English. Non-English texts of any length score near zero.
var englishStop = map[string]bool{"the": true, "of": true, "and": true, "to": true, "in": true, "a": true, "is": true, "that": true, "for": true, "with": true, "as": true, "on": true, "are": true, "this": true, "by": true, "we": true, "be": true, "from": true, "or": true, "an": true, "which": true, "these": true, "their": true, "has": true, "have": true, "was": true, "were": true, "it": true, "its": true, "can": true, "not": true, "our": true, "results": true, "study": true, "paper": true, "research": true, "between": true, "based": true, "using": true, "approach": true}

// otherStop flags common function words of other major publication languages.
var otherStop = map[string]bool{
	// German
	"und": true, "der": true, "die": true, "das": true, "mit": true, "für": true, "nicht": true, "eine": true, "einer": true, "werden": true, "wird": true, "sind": true, "auf": true, "dem": true, "des": true, "zur": true, "zum": true, "über": true,
	// Spanish / Portuguese
	"los": true, "las": true, "una": true, "para": true, "por": true, "como": true, "más": true, "con": true, "del": true, "este": true, "esta": true, "dos": true, "não": true, "uma": true, "são": true, "também": true, "pela": true,
	// French
	"les": true, "une": true, "dans": true, "pour": true, "sur": true, "est": true, "sont": true, "cette": true, "aux": true, "nous": true, "être": true, "ainsi": true,
	// Italian
	"della": true, "delle": true, "degli": true, "nel": true, "nella": true, "sono": true, "anche": true, "come": true, "più": true, "gli": true,
	// Indonesian / Malay
	"yang": true, "dan": true, "dengan": true, "untuk": true, "dari": true, "pada": true, "adalah": true, "ini": true, "dalam": true, "penelitian": true,
	// Turkish
	"ve": true, "bir": true, "için": true, "ile": true, "olarak": true, "bu": true, "çalışma": true,
	// Finnish / Swedish / Dutch
	"ja": true, "että": true, "sekä": true, "och": true, "att": true, "för": true, "är": true, "het": true, "een": true, "van": true, "voor": true, "zijn": true,
	// Polish / Czech / Russian (latin-transliterated rarely); Cyrillic handled by script check
	"oraz": true, "jest": true, "przez": true, "które": true, "nie": true,
}

// IsEnglish reports whether a record is (very probably) written in English.
// It trusts a reported language code; otherwise it inspects title + abstract.
func IsEnglish(p *model.Paper) bool {
	if lang := strings.ToLower(strings.TrimSpace(p.Language)); lang != "" && lang != "xx" && lang != "und" {
		return lang == "en" || lang == "eng" || strings.HasPrefix(lang, "en-") || strings.HasPrefix(lang, "en_")
	}
	text := p.Title + " " + p.Abstract
	// Script check: mostly non-Latin letters → not English.
	letters, nonLatin := 0, 0
	for _, r := range text {
		if unicode.IsLetter(r) {
			letters++
			if !unicode.Is(unicode.Latin, r) {
				nonLatin++
			}
		}
	}
	if letters > 20 && nonLatin*3 > letters {
		return false
	}
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && r != '\'' })
	en, other := 0, 0
	for _, w := range words {
		if englishStop[w] {
			en++
		} else if otherStop[w] {
			other++
		}
	}
	// Titles alone carry few function words: judge short texts only by the
	// presence of foreign function words.
	if len(words) < 25 {
		return !(other >= 2 && other > en)
	}
	// English prose has ~25–40 % stop words; other languages ~0 % English ones.
	if other > en {
		return false
	}
	return float64(en)/float64(len(words)) >= 0.06
}

// FilterNonEnglish splits papers into (english, nonEnglish), flagging the latter.
func FilterNonEnglish(ps []*model.Paper) (kept, dropped []*model.Paper) {
	for _, p := range ps {
		if IsEnglish(p) {
			kept = append(kept, p)
		} else {
			p.NonEnglish = true
			dropped = append(dropped, p)
		}
	}
	return
}
