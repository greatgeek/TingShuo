package ttsclient

import "testing"

func TestRubyMarkup(t *testing.T) {
	got := rubyMarkup([]RubyToken{
		{Surface: "きれいな"},
		{Surface: "町", Reading: "まち"},
		{Surface: "、"},
		{Surface: "人", Reading: "人"},
	})
	want := `きれいな<sub alias="まち">町</sub>、人`
	if got != want {
		t.Fatalf("rubyMarkup = %q, want %q", got, want)
	}
}

func TestRubyMarkupEscapes(t *testing.T) {
	got := rubyMarkup([]RubyToken{{Surface: `<&>`, Reading: `"x"`}})
	want := `<sub alias="&#34;x&#34;">&lt;&amp;&gt;</sub>`
	if got != want {
		t.Fatalf("rubyMarkup = %q, want %q", got, want)
	}
}
