package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcp_books/src/library"
)

// TestEndToEnd drives every tool through a real MCP client session against
// the books_md library. Run with: go test ./src -v  (set MCP_BOOKS_DIR to override).
func TestEndToEnd(t *testing.T) {
	dir := os.Getenv("MCP_BOOKS_DIR")
	if dir == "" {
		dir = "../books_md"
	}
	if _, err := os.Stat(dir); err != nil {
		t.Skip("books_md not found")
	}
	lib, err := library.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	st := newState()
	st.set(lib, nil)

	ctx := context.Background()
	srv := mcp.NewServer(&mcp.Implementation{Name: "mcp-books", Version: version}, &mcp.ServerOptions{Instructions: instructions})
	registerTools(srv, st)
	ct, cl := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, ct, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, cl, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	call := func(name string, args map[string]any, want ...string) string {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		text := res.Content[0].(*mcp.TextContent).Text
		if res.IsError {
			t.Fatalf("%s(%v) returned error: %s", name, args, text)
		}
		for _, w := range want {
			if !strings.Contains(text, w) {
				t.Errorf("%s(%v): output lacks %q\n%s", name, args, w, text)
			}
		}
		if testing.Verbose() {
			t.Logf("=== %s %v\n%s", name, args, truncateStr(text, 2500))
		}
		return text
	}

	call("list_books", nil, "readable books", "Rust")
	call("list_books", map[string]any{"category": "Go"}, "Learning_Go_Idiomatic_Approach_Jon_Bodner")
	call("find_books", map[string]any{"topic": "raft consensus leader election"}, "Database_Internals_Alex_Petrov", "matched 3/4")
	call("find_books", map[string]any{"topics": []string{"file locking flock fcntl", "topological sort dependency graph"}},
		"Books covering the most", "The_Linux_Programming_Interface_Michael_Kerrisk")
	call("get_book_toc", map[string]any{"book": "learning go"}, "Chapter 5. Functions", "1040 pages")
	call("get_book_toc", map[string]any{"book": "The_Linux_Programming_Interface_Michael_Kerrisk"},
		"Chapter 24: PROCESS CREATION", "Chapter 55: FILE LOCKING")
	call("get_book_toc", map[string]any{"book": "Learning_Go_Idiomatic_Approach_Jon_Bodner", "filter": "closure"}, "Closures")
	call("read_section", map[string]any{"book": "Learning_Go_Idiomatic_Approach_Jon_Bodner", "section": "Chapter 5. Functions", "max_chars": 1500},
		"Sub-sections:", "Closures", "continue:")
	call("search_books", map[string]any{"query": "B-tree page split", "limit": 5}, "Database Internals")
	call("search_books", map[string]any{"query": "pin unpin future", "book": "rust"}, "Pin")
	call("grep_books", map[string]any{"pattern": "sync.WaitGroup", "limit": 5}, "lines in")
	call("grep_books", map[string]any{"pattern": "^Chapter 5\\.", "regex": true, "book": "Learning_Go_Idiomatic_Approach_Jon_Bodner"}, "3454")
	call("find_chapters", map[string]any{"query": "file locking"}, "Chapter 55: FILE LOCKING")
	call("find_chapters", map[string]any{"query": "topological sort", "category": "Algorithms"}, "Topological")
	call("read_lines", map[string]any{"book": "Learning_Go_Idiomatic_Approach_Jon_Bodner", "start_line": 595, "count": 5}, "595:")

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "get_book_toc", Arguments: map[string]any{"book": "lerning golang idomatic"}})
	if err != nil || !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "Closest") {
		t.Errorf("unknown book should return suggestions, got %v %+v", err, res)
	}
}
