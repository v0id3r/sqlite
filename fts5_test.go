// Copyright 2026 The Sqlite Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sqlite

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode"
)

// synonymTokenizer lowercases ASCII words and emits each synonym as a token
// colocated with the word it follows.
type synonymTokenizer struct {
	synonyms map[string]string
	mu       sync.Mutex
	flags    []int
	fail     bool
}

func (s *synonymTokenizer) Tokenize(text string, flags int, emit func(token string, tflags, start, end int) error) error {
	s.mu.Lock()
	s.flags = append(s.flags, flags)
	s.mu.Unlock()
	if s.fail {
		return errors.New("tokenizer failed")
	}
	start := -1
	for i, r := range text + " " {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start < 0 {
			continue
		}
		word := strings.ToLower(text[start:i])
		if err := emit(word, 0, start, i); err != nil {
			return err
		}
		if syn, ok := s.synonyms[word]; ok {
			if err := emit(syn, FTS5TokenColocated, start, i); err != nil {
				return err
			}
		}
		start = -1
	}
	return nil
}

func (s *synonymTokenizer) seen(flags int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Contains(s.flags, flags)
}

func openFTS5(t *testing.T, tok *synonymTokenizer, gotArgs *[]string) *sql.DB {
	t.Helper()
	d := &Driver{}
	if err := d.RegisterFTS5Tokenizer("syn", func(args []string) (FTS5Tokenizer, error) {
		if gotArgs != nil {
			*gotArgs = args
		}
		return tok, nil
	}); err != nil {
		t.Fatal(err)
	}
	db := openOn(t, d)
	db.SetMaxOpenConns(1)
	return db
}

func TestFTS5TokenizerColocatedSynonyms(t *testing.T) {
	tok := &synonymTokenizer{synonyms: map[string]string{"first": "1st"}}
	var args []string
	db := openFTS5(t, tok, &args)
	if _, err := db.Exec(`CREATE VIRTUAL TABLE t USING fts5(x, tokenize = 'syn alpha beta')`); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(args, []string{"alpha", "beta"}) {
		t.Fatalf("factory args = %q, want [alpha beta]", args)
	}
	if _, err := db.Exec(`INSERT INTO t(x) VALUES ('The First Day'), ('second day')`); err != nil {
		t.Fatal(err)
	}
	if !tok.seen(FTS5TokenizeDocument) {
		t.Fatalf("flags = %v, want a document call", tok.flags)
	}
	for query, want := range map[string]string{
		"1st":   "The [First] Day",
		"first": "The [First] Day",
		"day":   "The First [Day]",
	} {
		var got string
		if err := db.QueryRow(`SELECT highlight(t, 0, '[', ']') FROM t WHERE t MATCH ? ORDER BY rowid LIMIT 1`, query).Scan(&got); err != nil {
			t.Fatalf("MATCH %q: %v", query, err)
		}
		if got != want {
			t.Errorf("MATCH %q highlight = %q, want %q", query, got, want)
		}
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM t WHERE t MATCH 'fir*'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 || !tok.seen(FTS5TokenizeQuery|FTS5TokenizePrefix) {
		t.Fatalf("prefix query count = %d, flags = %v; want 1 row and a query|prefix call", n, tok.flags)
	}
	if _, err := db.Exec(`INSERT INTO t(t) VALUES ('integrity-check')`); err != nil {
		t.Fatal(err)
	}
}

func TestFTS5TokenizerErrors(t *testing.T) {
	tok := &synonymTokenizer{}
	db := openFTS5(t, tok, nil)
	if _, err := db.Exec(`CREATE VIRTUAL TABLE t USING fts5(x, tokenize = 'syn')`); err != nil {
		t.Fatal(err)
	}
	tok.fail = true
	if _, err := db.Exec(`INSERT INTO t(x) VALUES ('anything')`); err == nil {
		t.Fatal("insert succeeded, want the tokenizer error")
	}

	d := &Driver{}
	if err := d.RegisterFTS5Tokenizer("bad", func([]string) (FTS5Tokenizer, error) { return nil, errors.New("no") }); err != nil {
		t.Fatal(err)
	}
	if err := d.RegisterFTS5Tokenizer("bad", func([]string) (FTS5Tokenizer, error) { return nil, nil }); err == nil {
		t.Fatal("duplicate registration succeeded")
	}
	if _, err := openOn(t, d).Exec(`CREATE VIRTUAL TABLE t USING fts5(x, tokenize = 'bad')`); err == nil {
		t.Fatal("create succeeded, want the factory error")
	}
}

func TestFTS5TokenizerPackageLevel(t *testing.T) {
	name := "syn_" + strings.ReplaceAll(uniqueDriverName(t), "-", "_")
	tok := &synonymTokenizer{synonyms: map[string]string{"car": "automobile"}}
	if err := RegisterFTS5Tokenizer(name, func([]string) (FTS5Tokenizer, error) { return tok, nil }); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE VIRTUAL TABLE t USING fts5(x, tokenize = '` + name + `')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO t(x) VALUES ('a red car')`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM t WHERE t MATCH 'automobile'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("count = %d, err = %v; want 1", n, err)
	}
}
