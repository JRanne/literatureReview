package model

import "testing"

func TestEnsureBibKeys(t *testing.T) {
	p := &Project{Papers: []*Paper{
		{Title: "The Adoption of Generative AI", Authors: []string{"Maija Virtanen"}, Year: 2024},
		{Title: "Adoption Patterns Revisited", Authors: []string{"M. Virtanen"}, Year: 2024},
		{Title: "On the Role of Trust", Authors: []string{"Virtanen, Maija"}, Year: 2024},
		{Title: "Untitled", BibKey: "custom"},
	}}
	p.EnsureBibKeys()
	if p.Papers[0].BibKey != "virtanen2024adoption" || p.Papers[1].BibKey != "virtanen2024adoptiona" || p.Papers[2].BibKey != "virtanen2024role" || p.Papers[3].BibKey != "custom" {
		t.Fatalf("keys: %s %s %s %s", p.Papers[0].BibKey, p.Papers[1].BibKey, p.Papers[2].BibKey, p.Papers[3].BibKey)
	}
	q := &Project{Papers: []*Paper{{Title: "Generative AI and Ökonomie", Authors: []string{"Cevher Özden"}, Year: 2025}, {Title: "Étude", Authors: []string{"Jürgen Müller-Lüdenscheidt"}, Year: 2020}}}
	q.EnsureBibKeys()
	if q.Papers[0].BibKey != "ozden2025generative" || q.Papers[1].BibKey != "mullerludenscheidt2020etude" {
		t.Fatalf("diacritics: %s %s", q.Papers[0].BibKey, q.Papers[1].BibKey)
	}
	p.EnsureBibKeys() // idempotent
	if p.Papers[1].BibKey != "virtanen2024adoptiona" {
		t.Fatal("keys changed on second call")
	}
}
