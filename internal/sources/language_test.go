package sources

import (
	"testing"

	"literaturereview/internal/model"
)

func TestIsEnglish(t *testing.T) {
	cases := []struct {
		p    *model.Paper
		want bool
	}{
		{&model.Paper{Language: "en"}, true},
		{&model.Paper{Language: "eng"}, true},
		{&model.Paper{Language: "de", Title: "The adoption of AI"}, false},
		{&model.Paper{Title: "Generative AI adoption in the public sector", Abstract: "We study how organisations in the public sector adopt generative AI and the governance mechanisms that are used by these organisations."}, true},
		{&model.Paper{Title: "Künstliche Intelligenz in der öffentlichen Verwaltung", Abstract: "In dieser Studie untersuchen wir, wie Organisationen der öffentlichen Verwaltung generative KI einführen und welche Governance-Mechanismen dabei eine Rolle spielen."}, false},
		{&model.Paper{Title: "Adopción de la inteligencia artificial generativa en el sector público", Abstract: "Este estudio analiza cómo las organizaciones del sector público adoptan la inteligencia artificial generativa y los mecanismos de gobernanza que utilizan para ello."}, false},
		{&model.Paper{Title: "Penerapan kecerdasan buatan generatif pada organisasi sektor publik", Abstract: "Penelitian ini membahas bagaimana organisasi sektor publik mengadopsi kecerdasan buatan generatif dan mekanisme tata kelola yang digunakan dalam penerapannya."}, false},
		{&model.Paper{Title: "生成式人工智能在公共部门的应用研究", Abstract: "本文研究公共部门组织如何采用生成式人工智能以及所使用的治理机制。"}, false},
		{&model.Paper{Title: "Short title only"}, true},
		{&model.Paper{Title: "Enterprise LLMOps: Advancing Large Language Models Operations Practice", Abstract: ""}, true},
	}
	for _, c := range cases {
		if got := IsEnglish(c.p); got != c.want {
			t.Errorf("%q → %v, want %v", c.p.Title, got, c.want)
		}
	}
}
