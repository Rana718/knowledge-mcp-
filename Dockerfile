# syntax=docker/dockerfile:1

# ---- build ----
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY src ./src
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mcp-books ./src

# ---- run ----
# distroless: no shell or package manager, runs as a non-root user
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/mcp-books /usr/local/bin/mcp-books
# the whole library is baked into the image
COPY books_md /data/books_md

# soft memory cap for the Go runtime (live data is ~165 MB)
ENV GOMEMLIMIT=400MiB
EXPOSE 8090
ENTRYPOINT ["/usr/local/bin/mcp-books", "-books", "/data/books_md"]
# default: streamable HTTP at http://<host>:8090/mcp
# for stdio, run with:  docker run -i --rm mcp-books -http ""
CMD ["-http", "0.0.0.0:8090"]
