# mcp-books

An MCP server written in Go that gives AI agents access to the `books_md/` library: 163 technical books converted to markdown. Agents can find the right book for a topic, browse a book's chapter index, read chapters and sections, and run ranked or exact search across every book.

## Build and run

```sh
make build          # -> bin/mcp-books   (or: go build -o bin/mcp-books ./src)
make check          # load + index the library, list books without usable text
make test           # end-to-end test of every tool over a real MCP session

bin/mcp-books                               # stdio transport (default)
bin/mcp-books -http 127.0.0.1:8090          # streamable HTTP at http://127.0.0.1:8090/mcp
bin/mcp-books -books /path/to/books_md      # or set MCP_BOOKS_DIR
bin/mcp-books -call find_books -args '{"topic":"raft leader election"}'   # run one tool in the terminal
```

With Task: `task serve`, `task check`, `task test`, `task call TOOL=get_book_toc ARGS='{"book":"learning go"}'`.

If `-books` isn't given, the server tries `$MCP_BOOKS_DIR`, then `books_md` next to the binary or one level up, then `./books_md`. Indexing runs in the background at startup (about 1 second, roughly 100k passages), so the MCP handshake gets an answer right away.

## Docker

The image contains the server binary and the whole `books_md/` library (about 140 MB, distroless, runs as a non-root user).

```sh
docker build -t mcp-books .                                   # or: task docker:build
docker run -d --name mcp-books -p 127.0.0.1:8090:8090 mcp-books   # HTTP at http://127.0.0.1:8090/mcp
docker compose up -d --build                                  # same, with restart policy (task docker:up)
```

- **stdio:** `docker run -i --rm mcp-books -http ""` runs the server over stdio, so an agent can launch the container itself:
  ```json
  { "books": { "command": "docker", "args": ["run", "-i", "--rm", "mcp-books", "-http", ""] } }
  ```
- **Updating books:** the books are copied in at build time, so rebuild the image after changing `books_md/`. To use a folder from the host without rebuilding, mount it over the built-in copy: `-v /path/to/books_md:/data/books_md:ro`.
- **Memory:** `GOMEMLIMIT=400MiB` is set in the image. After startup the container uses about 250 MB (Go heap ~165 MB plus cached book files).

## Connect an agent

- **Claude Code**, in this folder: `.mcp.json` is already set up. Start `claude` here and approve the `books` server.
- **Claude Code**, globally:
  `claude mcp add books -s user -- /run/media/rana/DEV/code/Experment/mcp_books/bin/mcp-books -books /run/media/rana/DEV/code/Experment/mcp_books/books_md`
- **Claude Desktop / Cursor / other clients**: add to the client's `mcpServers` config:
  ```json
  { "books": { "command": "/run/media/rana/DEV/code/Experment/mcp_books/bin/mcp-books",
               "args": ["-books", "/run/media/rana/DEV/code/Experment/mcp_books/books_md"] } }
  ```

## Tools

| Tool | What it answers |
|---|---|
| `find_books(topic \| topics[], category?, limit?)` | Which books and sections cover a topic. With a list of topics (the parts of a project) it also ranks books by how many topics they cover. Each hit shows the matched words and a section id. |
| `find_chapters(query, book?, category?, limit?)` | Searches chapter and section titles across all books ("which books have a chapter on file locking?"). Chapters rank first. |
| `get_book_toc(book, depth?, filter?, offset?)` | A book's chapter and section index with `sN` ids and sizes, plus its category, pages and status. `filter` searches section titles. |
| `read_section(book, section, offset?, max_chars?)` | Reads a chapter or section by id (`s42`) or title. Long sections come in pages and start with a list of their sub-sections. |
| `search_books(query, book?, category?, limit?)` | Ranked passage search, one result per section. |
| `grep_books(pattern, book?, category?, regex?, case_sensitive?, limit?)` | Exact or regex line search, for API names, flags and errors. |
| `read_lines(book, start_line, count?)` | Lines by number, for example around a grep hit. |
| `list_books(category?, query?, all?)` | With no arguments, the category overview. With arguments, the matching books and their ids. |

There is also a `books://catalog` resource that holds the whole catalog as markdown.

- **Book references:** `book` accepts the id (the file stem), a unique part of the title (`"learning go"`) or an acronym (`tlpi`). An unknown name returns the closest matches.
- **Filters:** in `search_books` and `grep_books`, `book` accepts a comma-separated list, and a fragment such as `rust` selects every matching book.
- **Logging:** the server prints one line at startup. Add `-v` to log every HTTP request.

## How it works (`src/library`)

- **Catalog.** `INDEX.json` supplies titles, categories, page counts and source PDFs. Any `.md` file that isn't listed there is still picked up from the folder tree.
- **Structure.** Markdown headings are parsed and cleaned. The parser skips code blocks, NOTE/TIP boxes, figure and listing captions, page-number headings, and the shell prompts and `#include` lines that the PDF converter turned into headings. Letter-spaced titles (`P R O C E S S E S`) are rebuilt into words using the book's own vocabulary. A chapter title without a number gets one from its first `N.1` section, and a bare "Chapter 7" label is merged with the title that follows it. Heading levels in converted PDFs are inconsistent, so each heading also gets a normalized rank. "Chapter N" and "Part N" headings are placed above ordinary headings, so a chapter covers all of its topics.
- **Search.** Every book is split into passages of about 2 KB along section boundaries. Each passage is indexed together with its section and parent titles. Scoring uses BM25 with a bonus for passages that match more of the query terms and for exact phrase matches. Dot-leader lines from printed contents pages are skipped.
- **Book status.** `ok` means usable. `no_text` means a scanned PDF with no extracted text, which needs OCR. `stub` means a short placeholder. Agents are told to prefer `ok` books.

## Performance

Measured on this library (122 MB of markdown, 163 books, 12-core machine):

| | Before tuning | Now |
|---|---|---|
| RAM while idle (RSS) | 690 MB | ~200 MB |
| Peak RAM at startup | 690 MB | ~320 MB |
| Startup CPU time / wall time | ~6 s / 0.9 s | ~2.6 s / 0.6 s |
| `search_books` (full tool call) | 11–21 ms | ~0.5–1 ms |
| `find_books`, one topic / five topics | 4–5 ms / 2.6 ms+ | ~0.7 ms / ~1 ms |
| `find_chapters` | 0.9 ms | ~0.15 ms |
| `grep_books`, plain text over all books | 5.3 s | ~10–12 ms |
| `grep_books`, regex over all books | 1.5 s | ~7–8 ms |

What changed, following *100 Go Mistakes* (#21 slice preallocation, #28 maps, sync.Pool) and *Learning Go* (GC workload, GOGC/GOMEMLIMIT):

- **Text and lines:** each book is one string plus a 4-byte offset per line. Files are not copied a second time.
- **Search index:** posting lists are varint-encoded in a single byte slice (14 MB for 6.7M postings) and built with exact preallocation. The index holds no pointers apart from the term dictionary, so GC has little to scan.
- **Tokenizing:** no string is allocated per word, and stop words share the dictionary lookup.
- **Queries:** score buffers are reused through `sync.Pool`, and only the top results are kept in a heap.
- **Grep:**
  - Plain-text patterns jump to the pattern's rarest byte (by measured byte frequency in the books) with a vectorized `IndexByte`, instead of running a `(?i)` regexp on every line.
  - For regexes, the required literal (e.g. `sync.` in `sync\.(Wait|Mutex)`) is found first, and the regexp runs only on those lines.
  - Books are searched in parallel batches, and the search stops once the limit is reached.
- **Other tools:**
  - `find_chapters` uses an inverted index of title words.
  - Multi-topic `find_books` ranks its topics in parallel.
  - Exact-phrase checks reuse the fast case-insensitive finder instead of lowercasing passages.
- **GC:** after indexing the server returns the garbage to the OS (`debug.FreeOSMemory`) and runs with `GOGC=40`. Set `GOGC` or `GOMEMLIMIT` in the environment to override.

Run `task bench` to measure on your machine.

## Library notes

- **No text (6 books).** These were scanned PDFs, and converting them produced no text: Computer System Architecture (Mano), Rust Crash Course, Data Structures and Algorithms in C (Drozdek), Introduction to Automata Theory (Hopcroft), Docker Up and Running (Schenker), and Higher Engineering Mathematics (Grewal). Re-convert them with OCR to make them searchable.
- **ByteByteGo case studies.** The 30 files in `ByteByteGo_Case_Studies` all contain the same generic template, not the real case studies, so they are marked `stub`.
- **Code samples.** Some books lost the spaces inside code blocks during the PDF conversion (`funcdiv(numint,denomint)int{`). That comes from the source conversion and can't be fixed in the server.
- **Reorganization done on 2026-11-22.** The original index is kept as `books_md/INDEX.json.bak`.
  - Moved `CP_1` and `cp2` (competitive programming books) from Compilers to `Algorithms_and_Data_Structures`.
  - Moved `edelcomp` (Computational Topology) to `12_Mathematics_and_General_Science`.
  - Renamed the files with unclear names: `pp`, `1809_05923v2`, `978_3_030_61601_4`, `MAT_505` and `TOC_PETERLINZ`.
  - Cleaned up 34 titles in `INDEX.json`.
