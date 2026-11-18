// The forge's HTTP server and entry lifecycle, in Go: one process serving git smart HTTP
// (/<owner>/<repo>.git/...), the REST v3 API (/api/v3/...), GraphQL v4 (POST /api/graphql) and
// MCP (POST /mcp), all over the single in-memory store in store.go. This file owns the wire
// frame every surface shares — the bearer-token check, the JSON body parse, the
// X-GitHub-Request-Id header and the JSON 404 — and the entry <-> env lifecycle: bind an
// ephemeral port, POST {"port": N} to $MAKEFUN_ENV_URL/register, long-poll GET
// $MAKEFUN_ENV_URL/done until it answers {"done": true}, then exit 0.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const jsonType = "application/json; charset=utf-8"
const maxBodyBytes = 256 * 1024 * 1024

// httpError maps to an HTTP status with a GitHub-shaped {message} body.
type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

// Ctx is everything a surface handler needs: the store, the request frame and the answer helpers.
type Ctx struct {
	Forge  *Forge
	R      *http.Request
	W      http.ResponseWriter
	URL    *url.URL
	Body   []byte
	Origin string // the entry's own origin as the caller addressed it (for absolute pagination links)
}

// JSON parses the JSON body; a malformed body is a 400, as the wire contract requires.
func (ctx *Ctx) JSON() any {
	var parsed any
	if err := json.Unmarshal(ctx.Body, &parsed); err != nil {
		panic(&httpError{status: 400, msg: "Problems parsing JSON"})
	}
	return parsed
}

func (ctx *Ctx) Send(status int, value any) {
	data, _ := json.Marshal(value)
	data = append(data, '\n')
	if ctx.W.Header().Get("Content-Type") == "" {
		ctx.W.Header().Set("Content-Type", jsonType)
	}
	ctx.W.Header().Set("Content-Length", strconv.Itoa(len(data)))
	ctx.W.WriteHeader(status)
	_, _ = ctx.W.Write(data)
}

// SendRawJSON writes a marshaled JSON body without the trailing newline Send adds.
func (ctx *Ctx) SendRawJSON(status int, value any) {
	data, _ := json.Marshal(value)
	ctx.W.Header().Set("Content-Type", "application/json")
	ctx.W.Header().Set("Content-Length", strconv.Itoa(len(data)))
	ctx.W.WriteHeader(status)
	_, _ = ctx.W.Write(data)
}

func (ctx *Ctx) SendRaw(status int, data []byte) {
	ctx.W.Header().Set("Content-Length", strconv.Itoa(len(data)))
	ctx.W.WriteHeader(status)
	_, _ = ctx.W.Write(data)
}

func (ctx *Ctx) SendNoContent() {
	ctx.W.WriteHeader(204)
}

func (ctx *Ctx) Fail(status int, message string) {
	ctx.Send(status, Json{"message": message})
}

func (ctx *Ctx) NotFound() {
	ctx.Fail(404, "Not Found")
}

func pathUnescape(s string) string {
	if decoded, err := url.PathUnescape(s); err == nil {
		return decoded
	}
	return s
}

var requestCounter int64

var gitPathPattern = regexp.MustCompile(`^/([^/]+)/([^/]+)\.git/(info/refs|git-upload-pack|git-receive-pack)$`)

func newHandler(forge *Forge, token string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestCounter++
		w.Header().Set("X-GitHub-Request-Id", "forge-"+strconv.FormatInt(requestCounter, 36)+"-"+strconv.FormatInt(time.Now().UnixMilli(), 36))
		defer func() {
			if recovered := recover(); recovered != nil {
				status := 400
				message := "Internal error"
				switch err := recovered.(type) {
				case *httpError:
					status = err.status
					message = err.msg
				case error:
					message = err.Error()
				case string:
					message = err
				}
				data, _ := json.Marshal(Json{"message": message})
				data = append(data, '\n')
				w.Header().Set("Content-Type", jsonType)
				w.Header().Set("Content-Length", strconv.Itoa(len(data)))
				w.WriteHeader(status)
				_, _ = w.Write(data)
			}
		}()

		path := r.URL.EscapedPath()
		method := r.Method

		// The env's boot check calls this twice: 200 {"ok":true}. It carries no token.
		if method == "GET" && path == "/healthz" {
			w.Header().Set("Content-Type", jsonType)
			w.WriteHeader(200)
			_, _ = w.Write([]byte("{\"ok\":true}\n"))
			return
		}

		var body []byte
		if method != "GET" && method != "HEAD" {
			data, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
			if err != nil {
				panic(&httpError{status: 400, msg: "Failed to read request body"})
			}
			if len(data) > maxBodyBytes {
				panic(&httpError{status: 413, msg: "Request body too large"})
			}
			body = data
		}

		// Every other surface requires the trial's bearer token.
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			panic(&httpError{status: 401, msg: "Bad credentials"})
		}

		host := r.Host
		if host == "" {
			host = "entry"
		}
		ctx := &Ctx{Forge: forge, R: r, W: w, URL: r.URL, Body: body, Origin: "http://" + host}

		// One request at a time through the store: the surfaces were written for a
		// single-threaded event loop, and the maps are not safe for concurrent use.
		forge.Mu.Lock()
		defer forge.Mu.Unlock()

		if path == "/api/v3" || strings.HasPrefix(path, "/api/v3/") {
			w.Header().Set("X-OAuth-Scopes", "repo, read:org")
			raw := strings.Split(strings.TrimPrefix(path, "/api/v3"), "/")
			var segments []string
			for _, segment := range raw {
				if segment != "" {
					segments = append(segments, segment)
				}
			}
			handleRest(ctx, segments)
			return
		}
		if path == "/api/graphql" && method == "POST" {
			handleGraphQL(ctx)
			return
		}
		if path == "/mcp" && method == "POST" {
			handleMcp(ctx)
			return
		}
		if match := gitPathPattern.FindStringSubmatch(path); match != nil {
			owner := pathUnescape(match[1])
			repoName := pathUnescape(match[2])
			endpoint := match[3]
			switch {
			case endpoint == "info/refs" && method == "GET":
				handleInfoRefs(ctx, owner, repoName)
			case endpoint == "git-upload-pack" && method == "POST":
				handleUploadPack(ctx, owner, repoName)
			case endpoint == "git-receive-pack" && method == "POST":
				handleReceivePack(ctx, owner, repoName)
			default:
				ctx.NotFound()
			}
			return
		}

		ctx.NotFound()
	}
}

// ------------------------------------------------------------------ lifecycle

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[entry] "+format+"\n", args...)
	os.Exit(1)
}

func postRegister(envURL string, port int) {
	backoff := 250 * time.Millisecond
	client := &http.Client{Timeout: 5 * time.Second}
	payload, _ := json.Marshal(Json{"port": port})
	for {
		resp, err := client.Post(envURL+"/register", "application/json", bytes.NewReader(payload))
		if err == nil {
			status := resp.StatusCode
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if status == 400 || status == 405 {
				fatalf("register refused with HTTP %d", status)
			}
			if (status >= 200 && status < 300) || status == 409 {
				return
			}
		}
		time.Sleep(backoff)
		if backoff < 2*time.Second {
			backoff *= 2
		}
	}
}

func waitForDone(envURL string) {
	backoff := 250 * time.Millisecond
	client := &http.Client{Timeout: 5 * time.Second}
	for {
		resp, err := client.Get(envURL + "/done")
		if err == nil {
			data, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			var parsed struct {
				Done bool `json:"done"`
			}
			if resp.StatusCode == 200 && json.Unmarshal(data, &parsed) == nil && parsed.Done {
				return
			}
		}
		time.Sleep(backoff)
		if backoff < 2*time.Second {
			backoff *= 2
		}
	}
}

func main() {
	envURL := os.Getenv("MAKEFUN_ENV_URL")
	if envURL == "" {
		fatalf("MAKEFUN_ENV_URL is not set")
	}
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")
	forge := NewForge()
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		fatalf("listen: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	server := &http.Server{
		Handler:           newHandler(forge, token),
		ReadHeaderTimeout: 80 * time.Second,
		IdleTimeout:       75 * time.Second,
	}
	fmt.Printf("[entry] listening on port %d\n", port)
	go func() {
		_ = server.Serve(listener)
	}()
	postRegister(envURL, port)
	fmt.Printf("[entry] registered; waiting for the env to say done\n")
	waitForDone(envURL)
	fmt.Printf("[entry] done\n")
	os.Exit(0)
}
