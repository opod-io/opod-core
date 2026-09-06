package api

import (
	"encoding/json"
	"testing"
)

func TestParseEmbeddingInput_Single(t *testing.T) {
	got, err := parseEmbeddingInput(json.RawMessage(`"hello world"`))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 1 || got[0] != "hello world" {
		t.Fatalf("got = %v, want [\"hello world\"]", got)
	}
}

func TestParseEmbeddingInput_Array(t *testing.T) {
	got, err := parseEmbeddingInput(json.RawMessage(`["a","b","c"]`))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Fatalf("got = %v", got)
	}
}

func TestParseEmbeddingInput_EmptyStringsFiltered(t *testing.T) {
	got, err := parseEmbeddingInput(json.RawMessage(`["a","","b",""]`))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("got = %v, want [a b]", got)
	}
}

func TestParseEmbeddingInput_Invalid(t *testing.T) {
	_, err := parseEmbeddingInput(json.RawMessage(`{"not":"a string"}`))
	if err == nil {
		t.Fatal("expected error for object input, got nil")
	}
}

func TestParseEmbeddingInput_EmptyString(t *testing.T) {
	got, err := parseEmbeddingInput(json.RawMessage(`""`))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != nil {
		t.Fatalf("got = %v, want nil", got)
	}
}
