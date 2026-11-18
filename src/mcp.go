// The MCP endpoint: JSON-RPC 2.0 over streamable HTTP at POST /mcp. It speaks the modern
// 2026-07-28 revision through server/discover (no handshake, no session) and the legacy
// initialize handshake for older clients. The tools act on the same store as REST: an issue
// created here is the same object GET /api/v3 reads back, with the same ids.
package main

import (
	"encoding/json"
	"sort"
	"strings"
)

var mcpServerInfo = Json{"name": "forge", "version": "1.0.0"}

var mcpSupportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

type toolError struct{ msg string }

func (e *toolError) Error() string { return e.msg }

type toolDef struct {
	name        string
	description string
	properties  map[string]Json
	propOrder   []string
	required    []string
	run         func(forge *Forge, args map[string]any) (map[string]any, error)
}

func needRepo(forge *Forge, args map[string]any) *Repo {
	owner, _ := args["owner"].(string)
	name, _ := args["repo"].(string)
	repo := forge.GetRepo(owner, name)
	if repo == nil {
		panic(&toolError{msg: "repository " + owner + "/" + name + " not found"})
	}
	return repo
}

func toolResult(view any) map[string]any {
	textBytes, _ := json.Marshal(view)
	return map[string]any{
		"content":           []any{Json{"type": "text", "text": string(textBytes)}},
		"structuredContent": view,
		"isError":           false,
	}
}

func argString(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

var mcpTools = []toolDef{
	{
		name:        "create_repo",
		description: "Create a new repository for the authenticated user.",
		properties: map[string]Json{
			"name":        {"type": "string", "description": "The repository name."},
			"description": {"type": "string", "description": "A short description of the repository."},
			"private":     {"type": "boolean", "description": "Whether the repository is private."},
		},
		propOrder: []string{"name", "description", "private"},
		required:  []string{"name"},
		run: func(forge *Forge, args map[string]any) (map[string]any, error) {
			name := argString(args, "name")
			if name == "" {
				return nil, &toolError{msg: "name is required"}
			}
			isPrivate := true
			if priv, ok := args["private"].(bool); ok {
				isPrivate = priv
			}
			created, ok := forge.CreateRepo(name, argString(args, "description"), isPrivate)
			if !ok {
				return nil, &toolError{msg: "repository " + name + " already exists"}
			}
			return toolResult(repoJson(created)), nil
		},
	},
	{
		name:        "get_repo",
		description: "Get a repository by owner and name.",
		properties: map[string]Json{
			"owner": {"type": "string", "description": "The repository owner login."},
			"repo":  {"type": "string", "description": "The repository name."},
		},
		propOrder: []string{"owner", "repo"},
		required:  []string{"owner", "repo"},
		run: func(forge *Forge, args map[string]any) (map[string]any, error) {
			return toolResult(repoJson(needRepo(forge, args))), nil
		},
	},
	{
		name:        "create_issue",
		description: "Create an issue in a repository.",
		properties: map[string]Json{
			"owner": {"type": "string", "description": "The repository owner login."},
			"repo":  {"type": "string", "description": "The repository name."},
			"title": {"type": "string", "description": "The issue title."},
			"body":  {"type": "string", "description": "The issue body."},
		},
		propOrder: []string{"owner", "repo", "title", "body"},
		required:  []string{"owner", "repo", "title"},
		run: func(forge *Forge, args map[string]any) (map[string]any, error) {
			repo := needRepo(forge, args)
			issue := &IssueRec{
				Number: repo.NextNumber,
				ID:     forge.AllocIssueID(),
				Title:  argString(args, "title"),
				Body:   argString(args, "body"),
				State:  "open",
				Seq:    repo.NextSeq(),
			}
			repo.NextNumber++
			repo.Issues[issue.Number] = issue
			return toolResult(issueJson(repo, issue)), nil
		},
	},
	{
		name:        "list_issues",
		description: "List the issues of a repository, optionally filtered by state.",
		properties: map[string]Json{
			"owner": {"type": "string", "description": "The repository owner login."},
			"repo":  {"type": "string", "description": "The repository name."},
			"state": {"type": "string", "description": "Filter by state: open, closed or all."},
		},
		propOrder: []string{"owner", "repo", "state"},
		required:  []string{"owner", "repo"},
		run: func(forge *Forge, args map[string]any) (map[string]any, error) {
			repo := needRepo(forge, args)
			state := argString(args, "state")
			if state == "" {
				state = "open"
			}
			var list []*IssueRec
			for _, issue := range repo.Issues {
				if state == "open" || state == "closed" {
					if issue.State != state {
						continue
					}
				}
				list = append(list, issue)
			}
			sort.Slice(list, func(i, j int) bool { return list[i].Seq < list[j].Seq })
			views := make([]any, len(list))
			for i, issue := range list {
				views[i] = issueJson(repo, issue)
			}
			return toolResult(views), nil
		},
	},
	{
		name:        "get_issue",
		description: "Get one issue by its number.",
		properties: map[string]Json{
			"owner":        {"type": "string", "description": "The repository owner login."},
			"repo":         {"type": "string", "description": "The repository name."},
			"issue_number": {"type": "number", "description": "The issue number."},
		},
		propOrder: []string{"owner", "repo", "issue_number"},
		required:  []string{"owner", "repo", "issue_number"},
		run: func(forge *Forge, args map[string]any) (map[string]any, error) {
			repo := needRepo(forge, args)
			number, ok := numArg(args, "issue_number")
			var issue *IssueRec
			if ok {
				issue = repo.Issues[number]
			}
			if issue == nil {
				return nil, &toolError{msg: "issue not found"}
			}
			return toolResult(issueJson(repo, issue)), nil
		},
	},
	{
		name:        "create_pull_request",
		description: "Open a pull request from a head branch into a base branch.",
		properties: map[string]Json{
			"owner": {"type": "string", "description": "The repository owner login."},
			"repo":  {"type": "string", "description": "The repository name."},
			"title": {"type": "string", "description": "The pull request title."},
			"body":  {"type": "string", "description": "The pull request body."},
			"head":  {"type": "string", "description": "The head branch name."},
			"base":  {"type": "string", "description": "The base branch name."},
		},
		propOrder: []string{"owner", "repo", "title", "body", "head", "base"},
		required:  []string{"owner", "repo", "title", "head", "base"},
		run: func(forge *Forge, args map[string]any) (map[string]any, error) {
			repo := needRepo(forge, args)
			head := argString(args, "head")
			base := argString(args, "base")
			if repo.TipOf(head) == "" {
				return nil, &toolError{msg: "head branch " + head + " does not exist"}
			}
			if repo.TipOf(base) == "" {
				return nil, &toolError{msg: "base branch " + base + " does not exist"}
			}
			pull := &PullRec{
				Number:  repo.NextNumber,
				ID:      forge.AllocIssueID(),
				Title:   argString(args, "title"),
				Body:    argString(args, "body"),
				State:   "open",
				HeadRef: head,
				BaseRef: base,
				Seq:     repo.NextSeq(),
			}
			repo.NextNumber++
			repo.Pulls[pull.Number] = pull
			return toolResult(pullJson(repo, pull)), nil
		},
	},
	{
		name:        "list_pull_requests",
		description: "List the pull requests of a repository, optionally filtered by state.",
		properties: map[string]Json{
			"owner": {"type": "string", "description": "The repository owner login."},
			"repo":  {"type": "string", "description": "The repository name."},
			"state": {"type": "string", "description": "Filter by state: open, closed or all."},
		},
		propOrder: []string{"owner", "repo", "state"},
		required:  []string{"owner", "repo"},
		run: func(forge *Forge, args map[string]any) (map[string]any, error) {
			repo := needRepo(forge, args)
			state := argString(args, "state")
			if state == "" {
				state = "open"
			}
			var list []*PullRec
			for _, pull := range repo.Pulls {
				if state == "open" || state == "closed" {
					if pull.State != state {
						continue
					}
				}
				list = append(list, pull)
			}
			sort.Slice(list, func(i, j int) bool { return list[i].Seq < list[j].Seq })
			views := make([]any, len(list))
			for i, pull := range list {
				views[i] = pullJson(repo, pull)
			}
			return toolResult(views), nil
		},
	},
	{
		name:        "merge_pull_request",
		description: "Merge a pull request into its base branch (squash).",
		properties: map[string]Json{
			"owner":       {"type": "string", "description": "The repository owner login."},
			"repo":        {"type": "string", "description": "The repository name."},
			"pull_number": {"type": "number", "description": "The pull request number."},
		},
		propOrder: []string{"owner", "repo", "pull_number"},
		required:  []string{"owner", "repo", "pull_number"},
		run: func(forge *Forge, args map[string]any) (map[string]any, error) {
			repo := needRepo(forge, args)
			number, ok := numArg(args, "pull_number")
			var pull *PullRec
			if ok {
				pull = repo.Pulls[number]
			}
			if pull == nil {
				return nil, &toolError{msg: "pull request not found"}
			}
			if pull.State != "open" {
				return nil, &toolError{msg: "pull request is not open"}
			}
			sha := squashMerge(repo, pull)
			if sha == "" {
				return nil, &toolError{msg: "pull request is not mergeable: merge conflict"}
			}
			pull.State = "closed"
			pull.Merged = true
			now := isoNow()
			pull.MergedAt = &now
			pull.MergeCommitSha = &sha
			return toolResult(pullJson(repo, pull)), nil
		},
	},
}

// The tools/list payload, built once so two calls answer byte-for-byte identical bytes.
var mcpToolsList []any

func init() {
	for _, tool := range mcpTools {
		mcpToolsList = append(mcpToolsList, Json{
			"name":        tool.name,
			"description": tool.description,
			"inputSchema": Json{"type": "object", "properties": tool.properties, "required": tool.required},
		})
	}
}

type rpcOutcome struct {
	status  int
	payload map[string]any // nil: no body (a notification's 202)
}

func rpcError(id any, code int, message string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": Json{"code": code, "message": message}}
}

func rpcResult(id any, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func handleRpc(forge *Forge, message map[string]any) (outcome rpcOutcome) {
	method, _ := message["method"].(string)
	id, hasID := message["id"]
	params, _ := message["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}
	isNotification := !hasID

	switch method {
	case "server/discover":
		return rpcOutcome{status: 200, payload: rpcResult(id, Json{
			"protocolVersion":   "2026-07-28",
			"supportedVersions": mcpSupportedVersions,
			"capabilities":      Json{"tools": Json{}},
			"serverInfo":        mcpServerInfo,
			"_meta":             Json{"io.modelcontextprotocol/serverInfo": mcpServerInfo},
		})}
	case "initialize":
		offered, _ := params["protocolVersion"].(string)
		negotiated := "2025-11-25"
		for _, version := range mcpSupportedVersions {
			if version == offered {
				negotiated = offered
			}
		}
		return rpcOutcome{status: 200, payload: rpcResult(id, Json{
			"protocolVersion": negotiated,
			"capabilities":    Json{"tools": Json{}},
			"serverInfo":      mcpServerInfo,
		})}
	case "ping":
		return rpcOutcome{status: 200, payload: rpcResult(id, Json{})}
	case "tools/list":
		return rpcOutcome{status: 200, payload: rpcResult(id, Json{"tools": mcpToolsList})}
	case "tools/call":
		name, _ := params["name"].(string)
		var tool *toolDef
		for i := range mcpTools {
			if mcpTools[i].name == name {
				tool = &mcpTools[i]
			}
		}
		if tool == nil {
			return rpcOutcome{status: 200, payload: rpcError(id, -32602, "Unknown tool: "+name)}
		}
		args, _ := params["arguments"].(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		var missing []string
		for _, key := range tool.required {
			value, present := args[key]
			if !present || value == nil || value == "" {
				missing = append(missing, key)
			}
		}
		if len(missing) > 0 {
			return rpcOutcome{status: 200, payload: rpcError(id, -32602, "Missing required argument(s): "+strings.Join(missing, ", "))}
		}
		result, err := func() (result map[string]any, err error) {
			defer func() {
				if r := recover(); r != nil {
					if toolErr, ok := r.(*toolError); ok {
						err = toolErr
						return
					}
					panic(r)
				}
			}()
			return tool.run(forge, args)
		}()
		if err != nil {
			if toolErr, ok := err.(*toolError); ok {
				return rpcOutcome{status: 200, payload: rpcResult(id, map[string]any{
					"content": []any{Json{"type": "text", "text": toolErr.msg}},
					"isError": true,
				})}
			}
			panic(err)
		}
		return rpcOutcome{status: 200, payload: rpcResult(id, result)}
	default:
		if isNotification || strings.HasPrefix(method, "notifications/") {
			return rpcOutcome{status: 202, payload: nil}
		}
		return rpcOutcome{status: 200, payload: rpcError(id, -32601, "Method not found: "+method)}
	}
}

func handleMcp(ctx *Ctx) {
	var parsed any
	if err := json.Unmarshal(ctx.Body, &parsed); err != nil {
		ctx.SendRawJSON(400, rpcError(nil, -32700, "Parse error: the body is not JSON"))
		return
	}
	if batch, ok := parsed.([]any); ok {
		var results []any
		for _, entry := range batch {
			message, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			outcome := handleRpc(ctx.Forge, message)
			if outcome.payload != nil {
				results = append(results, outcome.payload)
			}
		}
		if results == nil {
			results = []any{}
		}
		ctx.Send(200, results)
		return
	}
	message, ok := parsed.(map[string]any)
	if !ok {
		ctx.SendRawJSON(400, rpcError(nil, -32700, "Parse error: the body is not a JSON-RPC message"))
		return
	}
	outcome := handleRpc(ctx.Forge, message)
	if outcome.payload == nil {
		ctx.W.WriteHeader(outcome.status)
		return
	}
	ctx.Send(outcome.status, outcome.payload)
}
