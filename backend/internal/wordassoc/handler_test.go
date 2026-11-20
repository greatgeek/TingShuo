package wordassoc

import "testing"

func TestSanitizeItemsDropsChinese(t *testing.T) {
	seen := map[string]bool{}
	got := sanitizeItems([]assocItem{
		{Surface: "尤其是", Reading: "とくに", Meaning: "特别是"},
		{Surface: "特别是", Reading: "とくに", Meaning: "尤其是"},
		{Surface: "殊に", Reading: "ことに", Meaning: "特别；格外"},
	}, "特に", seen, 5)
	if len(got) != 1 || got[0].Surface != "殊に" {
		t.Fatalf("got %+v, want only 殊に", got)
	}
}

func TestPruneInvalid(t *testing.T) {
	g := assocGroups{
		Synonyms: []assocItem{
			{Surface: "尤其是", Reading: "とくに"},
			{Surface: "殊に", Reading: "ことに"},
		},
		Related: []assocItem{{Surface: "鄉里", Reading: "ふるさと"}},
	}
	if !pruneInvalid(&g) {
		t.Fatal("pruneInvalid = false, want true")
	}
	if len(g.Synonyms) != 1 || g.Synonyms[0].Surface != "殊に" {
		t.Fatalf("synonyms = %+v", g.Synonyms)
	}
	if len(g.Related) != 0 {
		t.Fatalf("related = %+v, want empty", g.Related)
	}
	if pruneInvalid(&g) {
		t.Fatal("second prune should be no-op")
	}
}

func TestHealCachePruneAndReadingInOnePass(t *testing.T) {
	g := assocGroups{
		Synonyms: []assocItem{
			{Surface: "好物", Reading: "このもの"},
			{Surface: "尤其是", Reading: "とくに"},
		},
	}
	if !healCache(&g) {
		t.Fatal("healCache = false, want true")
	}
	if len(g.Synonyms) != 1 || g.Synonyms[0].Surface != "好物" {
		t.Fatalf("synonyms = %+v, want only 好物", g.Synonyms)
	}
	if g.Synonyms[0].Reading != "こうぶつ" {
		t.Fatalf("reading = %q, want こうぶつ", g.Synonyms[0].Reading)
	}
	if healCache(&g) {
		t.Fatal("second heal should be no-op")
	}
}
