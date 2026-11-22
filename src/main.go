// Command mcp-books is an MCP server for a local library of markdown books.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcp_books/src/library"
)

const version = "1.0.0"

func main() {
	booksDir := flag.String("books", "", "path to the books_md directory (default: $MCP_BOOKS_DIR, ../books_md next to the binary, or ./books_md)")
	httpAddr := flag.String("http", "", "serve streamable HTTP on this address (e.g. 127.0.0.1:8090) instead of stdio")
	check := flag.Bool("check", false, "load and index the library, print a summary and exit")
	callTool := flag.String("call", "", "call one tool from the terminal and print its output, e.g. -call find_books -args '{\"topic\":\"raft\"}'")
	callArgs := flag.String("args", "{}", "JSON arguments for -call")
	verbose := flag.Bool("v", false, "log every HTTP request")
	flag.IntVar(&pageChars, "page", envInt("MCP_BOOKS_PAGE", 0), "result size in bytes for every client ($MCP_BOOKS_PAGE; default: 32000 for Claude, Cursor, VS Code and Codex, 9000 for others)")
	flag.Parse()
	log.SetOutput(os.Stderr)
	log.SetFlags(log.Ltime)
	log.SetPrefix("mcp-books ")

	// The heap is mostly long-lived book text and queries allocate little,
	// so a lower GOGC keeps memory near the live size at almost no CPU cost.
	// GOGC / GOMEMLIMIT in the environment still take precedence.
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(40)
	}

	root, err := resolveBooksDir(*booksDir)
	if err != nil {
		log.Fatal(err)
	}

	if *check {
		lib, err := library.Load(root)
		if err != nil {
			log.Fatal(err)
		}
		chunks, terms := lib.Stats()
		fmt.Printf("root: %s\nbooks: %d\npassages: %d\nterms: %d\nload: %s\n", lib.Root, len(lib.Books), chunks, terms, lib.LoadTime.Round(time.Millisecond))
		for _, b := range lib.Books {
			if b.Status != library.StatusOK {
				fmt.Printf("  [%s] %s\n", b.Status, b.RelPath)
			}
		}
		return
	}

	if *callTool != "" {
		if err := runCall(root, *callTool, *callArgs); err != nil {
			log.Fatal(err)
		}
		return
	}

	// Index in the background so the MCP handshake is answered immediately.
	st := newState()
	go func() {
		lib, err := library.Load(root)
		if err != nil {
			log.Printf("loading %s failed: %v", root, err)
		} else {
			// Indexing leaves a few hundred MB of garbage: collect it and give
			// the pages back to the OS so the resident size matches live data.
			debug.FreeOSMemory()
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			log.Printf("ready: %d books indexed in %s, heap %d MB", len(lib.Books), lib.LoadTime.Round(time.Millisecond), ms.HeapAlloc>>20)
		}
		st.set(lib, err)
	}()

	newServer := func() *mcp.Server {
		s := mcp.NewServer(&mcp.Implementation{Name: "mcp-books", Title: "Books Library", Version: version},
			&mcp.ServerOptions{Instructions: instructions, InitializedHandler: logClient})
		registerTools(s, st)
		return s
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *httpAddr != "" {
		shared := newServer()
		getServer := func(*http.Request) *mcp.Server { return shared }
		streamable := mcp.NewStreamableHTTPHandler(getServer, nil)
		sse := mcp.NewSSEHandler(getServer, nil) // for clients that only speak the older SSE transport
		mux := http.NewServeMux()
		mux.Handle("/sse", sse)
		mux.Handle("/sse/", sse)
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/", "/mcp", "/mcp/":
				streamable.ServeHTTP(w, r)
			default:
				http.NotFound(w, r)
			}
		})
		var handler http.Handler = mux
		if *verbose {
			handler = logRequests(mux)
		}
		srv := &http.Server{Addr: *httpAddr, Handler: handler}
		go func() { <-ctx.Done(); _ = srv.Close() }()
		log.Printf("listening on http://%s/mcp", *httpAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
		return
	}
	if err := newServer().Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}

// runCall runs one tool through an in-memory MCP session and prints the result.
func runCall(root, name, args string) error {
	var in map[string]any
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return fmt.Errorf("-args must be a JSON object: %w", err)
	}
	lib, err := library.Load(root)
	if err != nil {
		return err
	}
	st := newState()
	st.set(lib, nil)
	ctx := context.Background()
	srv := mcp.NewServer(&mcp.Implementation{Name: "mcp-books", Version: version}, nil)
	registerTools(srv, st)
	t1, t2 := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, t1, nil); err != nil {
		return err
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "cli", Version: version}, nil).Connect(ctx, t2, nil)
	if err != nil {
		return err
	}
	defer func() { _ = cs.Close() }()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: in})
	if err != nil {
		return err
	}
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			fmt.Println(t.Text)
		}
	}
	if res.IsError {
		os.Exit(1)
	}
	return nil
}

// statusWriter captures the response status; Flush keeps SSE streaming.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) { w.status = code; w.ResponseWriter.WriteHeader(code) }
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// logRequests logs one line per HTTP request (enabled with -v).
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		log.Printf("%s %s -> %d (%s)", r.Method, r.URL.RequestURI(), sw.status, r.UserAgent())
	})
}

func resolveBooksDir(flagVal string) (string, error) {
	var candidates []string
	if flagVal != "" {
		candidates = append(candidates, flagVal)
	} else {
		if env := os.Getenv("MCP_BOOKS_DIR"); env != "" {
			candidates = append(candidates, env)
		}
		if exe, err := os.Executable(); err == nil {
			if exe, err = filepath.EvalSymlinks(exe); err == nil {
				d := filepath.Dir(exe)
				candidates = append(candidates, filepath.Join(d, "books_md"), filepath.Join(d, "..", "books_md"))
			}
		}
		candidates = append(candidates, "books_md")
	}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && fi.IsDir() {
			return filepath.Abs(c)
		}
	}
	return "", fmt.Errorf("books directory not found (tried %v); pass -books or set MCP_BOOKS_DIR", candidates)
}

const instructions = `Library of ~130 readable technical books (Linux/systems, C, Go, Rust, Java, Python, databases, distributed systems, networking, compilers, algorithms, DevOps, security, AI, math). Use these tools, not the books_md files.

- A specific question: read_passages(query) returns the full text of the best passages from all books in one call.
- Which books or chapters cover a topic: find_books(topic), or find_books(topics=[...]) for the parts of a project;
  find_chapters(query) for chapter titles; get_book_toc(book) for one book's index.
- Reading: read_section(book, "sN"), or several at once with "s12,s15,s20". If a result ends with
  [NOT the end of sN ...], keep reading with the given offset before answering.
- Exact names, flags, errors: grep_books(pattern), then read_lines around a hit.

Results are sized to what your client accepts, so call again when a result says there is more instead of
answering from part of it. Use specific terms ("flock fcntl file locking", not "Linux stuff"). Cite as: Book title, Chapter/Section (sN).`

func envInt(name string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n > 0 {
		return n
	}
	return def
}

// logClient prints which client connected and the result size it gets.
func logClient(_ context.Context, req *mcp.InitializedRequest) {
	if p := req.Session.InitializeParams(); p != nil && p.ClientInfo != nil {
		log.Printf("client %s %s: results up to %d bytes", p.ClientInfo.Name, p.ClientInfo.Version, clientBudget(p.ClientInfo))
	}
}
