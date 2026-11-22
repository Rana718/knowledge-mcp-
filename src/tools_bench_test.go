package main

import (
	"context"
	"testing"

	"mcp_books/src/library"
)

var bl *library.Library

func benchLib(b *testing.B) *library.Library {
	if bl == nil {
		l, err := library.Load("../books_md")
		if err != nil {
			b.Skip(err)
		}
		bl = l
	}
	return bl
}

// rare pattern: scans every book
func BenchmarkGrep(b *testing.B) {
	l := benchLib(b)
	for b.Loop() {
		if _, err := grepBooks(context.Background(), l, grepIn{Pattern: "posix_spawn_file_actions", Limit: 30}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkToolFindBooks(b *testing.B) {
	l := benchLib(b)
	for b.Loop() {
		if _, err := findBooks(context.Background(), l, findBooksIn{Topic: "raft leader election"}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkToolFindBooksMany(b *testing.B) {
	l := benchLib(b)
	in := findBooksIn{Topics: []string{"posix_spawn fork exec", "file locking flock fcntl", "topological sort cycle detection", "hash function collision", "static shared library linker"}}
	for b.Loop() {
		if _, err := findBooks(context.Background(), l, in); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkToolSearch(b *testing.B) {
	l := benchLib(b)
	for b.Loop() {
		if _, err := searchBooks(context.Background(), l, searchIn{Query: "close on exec flag file descriptor"}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkToolSearchCommon(b *testing.B) {
	l := benchLib(b)
	for b.Loop() {
		if _, err := searchBooks(context.Background(), l, searchIn{Query: "how does the system handle data performance"}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkToolFindChapters(b *testing.B) {
	l := benchLib(b)
	for b.Loop() {
		if _, err := findChapters(context.Background(), l, findSectionsIn{Query: "file locking"}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGrepRegex(b *testing.B) {
	l := benchLib(b)
	for b.Loop() {
		if _, err := grepBooks(context.Background(), l, grepIn{Pattern: `sync\.(Wait|Mutex)`, Regex: true}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGrepCommonWord(b *testing.B) {
	l := benchLib(b)
	for b.Loop() {
		if _, err := grepBooks(context.Background(), l, grepIn{Pattern: "the"}); err != nil {
			b.Fatal(err)
		}
	}
}
