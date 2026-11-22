package library

import (
	"runtime"
	"testing"
	"time"
)

// TestMemoryReport prints load time and live heap after indexing.
// Run: go test ./src/library -run TestMemoryReport -v
func TestMemoryReport(t *testing.T) {
	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	t0 := time.Now()
	lib, err := Load("../../books_md")
	if err != nil {
		t.Skip(err)
	}
	load := time.Since(t0)
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	peak := ms.HeapSys
	runtime.GC()
	runtime.ReadMemStats(&ms)
	chunks, terms := lib.Stats()
	t.Logf("load %s | live heap %d MB | heap sys %d MB | total alloc %d MB | GCs %d | chunks %d terms %d",
		load.Round(time.Millisecond), ms.HeapAlloc>>20, peak>>20, (ms.TotalAlloc-before.TotalAlloc)>>20, ms.NumGC, chunks, terms)
	runtime.KeepAlive(lib)
}

var benchLib *Library

func lib(b *testing.B) *Library {
	if benchLib == nil {
		l, err := Load("../../books_md")
		if err != nil {
			b.Skip(err)
		}
		benchLib = l
	}
	return benchLib
}

func BenchmarkSearch(b *testing.B) {
	l := lib(b)
	b.ReportAllocs()
	for b.Loop() {
		l.Search("file descriptor close on exec", nil, 40)
	}
}

func BenchmarkSearchCommon(b *testing.B) {
	l := lib(b)
	b.ReportAllocs()
	for b.Loop() {
		l.Search("data system performance", nil, 40)
	}
}

func BenchmarkRankBooks(b *testing.B) {
	l := lib(b)
	b.ReportAllocs()
	for b.Loop() {
		l.RankBooks("raft leader election", nil, 6, 3)
	}
}

func BenchmarkFindSections(b *testing.B) {
	l := lib(b)
	b.ReportAllocs()
	for b.Loop() {
		l.FindSections("file locking", nil, 15)
	}
}
