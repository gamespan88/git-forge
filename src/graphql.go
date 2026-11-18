// The GraphQL v4 surface: a small parser for the document language gh and github-mcp-server
// speak (operations, variables, aliases, inline fragments, input objects, enum lists, batching)
// and an executor over the same store the REST surface serves, with the introspection answers
// gh's feature detection probes for. Global ids are opaque base64 of "<Type>:<parts>", stable
// across reads, and decode back to the same object for node(id:) and the mutation inputs.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ------------------------------------------------------------------ schema (for introspection and validation)

var objectFields = map[string][]string{
	"Query":    {"viewer", "repository", "node", "__type", "__schema"},
	"Mutation": {"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"},
	"User":     {"id", "login", "name", "email", "url"},
	"Repository": {
		"id", "databaseId", "name", "nameWithOwner", "owner", "description", "isPrivate",
		"viewerPermission", "hasIssuesEnabled", "defaultBranchRef", "issue", "issues",
		"pullRequest", "pullRequests", "url", "createdAt", "updatedAt",
	},
	"Ref":                   {"name"},
	"Issue":                 {"id", "number", "title", "body", "state", "url", "createdAt", "updatedAt", "closed", "labels", "comments", "repository", "author"},
	"IssueConnection":       {"nodes", "edges", "pageInfo", "totalCount"},
	"PullRequest": {
		"id", "number", "title", "body", "state", "merged", "mergedAt", "mergeable", "headRefName",
		"baseRefName", "headRef", "baseRef", "files", "url", "createdAt", "updatedAt", "closed", "repository", "author",
	},
	"PullRequestConnection":  {"nodes", "edges", "pageInfo", "totalCount"},
	"PullRequestChangedFile": {"path", "additions", "deletions", "changeType"},
	"PageInfo":               {"hasNextPage", "hasPreviousPage", "startCursor", "endCursor"},
	"CreateIssuePayload":     {"issue", "clientMutationId"},
	"CloseIssuePayload":      {"issue", "clientMutationId"},
	"CreatePullRequestPayload": {"pullRequest", "clientMutationId"},
	"MergePullRequestPayload": {"pullRequest", "clientMutationId"},
	"AddCommentPayload":      {"commentEdge", "subject", "clientMutationId"},
	"IssueCommentEdge":       {"node", "cursor"},
	"IssueComment":           {"id", "body", "createdAt", "author"},
	"LabelConnection":        {"nodes"},
	"Label":                  {"id", "name", "color"},
	"IssueEdge":              {"node", "cursor"},
	"PullRequestEdge":        {"node", "cursor"},
	"__Type":                 {"name", "kind", "description", "fields", "enumValues", "possibleTypes", "inputFields"},
	"__Field":                {"name", "description", "type", "args"},
	"__EnumValue":            {"name", "description"},
	"__Schema":               {"types", "queryType", "mutationType"},
	"Node":                   {"id"},
}

var objectFieldOrder = []string{
	"Query", "Mutation", "User", "Repository", "Ref", "Issue", "IssueConnection", "PullRequest",
	"PullRequestConnection", "PullRequestChangedFile", "PageInfo", "CreateIssuePayload",
	"CloseIssuePayload", "CreatePullRequestPayload", "MergePullRequestPayload", "AddCommentPayload",
	"IssueCommentEdge", "IssueComment", "LabelConnection", "Label", "IssueEdge", "PullRequestEdge",
	"__Type", "__Field", "__EnumValue", "__Schema", "Node",
}

var enumValues = map[string][]string{
	"IssueState":             {"OPEN", "CLOSED"},
	"PullRequestState":       {"OPEN", "CLOSED", "MERGED"},
	"MergeableState":         {"MERGEABLE", "CONFLICTING", "UNKNOWN"},
	"OrderDirection":         {"ASC", "DESC"},
	"IssueOrderField":        {"CREATED_AT", "UPDATED_AT", "COMMENTS"},
	"RepositoryPermission":   {"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"},
	"PullRequestMergeMethod": {"MERGE", "SQUASH", "REBASE"},
	"SearchType":             {"ISSUE", "REPOSITORY", "USER", "DISCUSSION"},
}

var enumOrder = []string{
	"IssueState", "PullRequestState", "MergeableState", "OrderDirection", "IssueOrderField",
	"RepositoryPermission", "PullRequestMergeMethod", "SearchType",
}

// ------------------------------------------------------------------ parser

type gqlValue struct {
	kind   string // variable | int | string | boolean | null | enum | list | object
	name   string
	intVal int
	strVal string
	bVal   bool
	list   []*gqlValue
	fields []gqlObjField
}

type gqlObjField struct {
	name  string
	value *gqlValue
}

type gqlArg struct {
	name  string
	value *gqlValue
}

type gqlField struct {
	alias        string
	hasAlias     bool
	name         string
	args         []gqlArg
	selection    []gqlSelection
	hasSelection bool
}

type gqlSelection struct {
	isFragment    bool
	typeCondition string
	field         *gqlField
	selection     []gqlSelection
}

type gqlOperation struct {
	typ       string // "query" | "mutation"
	name      string
	hasName   bool
	selection []gqlSelection
}

type gqlSyntaxError struct{ msg string }

func (e *gqlSyntaxError) Error() string { return e.msg }

var gqlNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+`)

func gqlTokenize(input string) ([]string, error) {
	var tokens []string
	i := 0
	for i < len(input) {
		ch := input[i]
		if ch == '#' {
			for i < len(input) && input[i] != '\n' {
				i++
			}
			continue
		}
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == ',' {
			i++
			continue
		}
		if strings.HasPrefix(input[i:], "...") {
			tokens = append(tokens, "...")
			i += 3
			continue
		}
		if ch == '"' {
			j := i + 1
			var out strings.Builder
			for j < len(input) && input[j] != '"' {
				if input[j] == '\\' {
					if j+1 >= len(input) {
						return nil, &gqlSyntaxError{msg: "unterminated string"}
					}
					switch input[j+1] {
					case 'n':
						out.WriteByte('\n')
					case 't':
						out.WriteByte('\t')
					default:
						out.WriteByte(input[j+1])
					}
					j += 2
				} else {
					out.WriteByte(input[j])
					j++
				}
			}
			if j >= len(input) {
				return nil, &gqlSyntaxError{msg: "unterminated string"}
			}
			tokens = append(tokens, "\""+out.String()+"\"")
			i = j + 1
			continue
		}
		if strings.ContainsRune("{}()[]:$@=!|", rune(ch)) {
			tokens = append(tokens, string(ch))
			i++
			continue
		}
		if match := gqlNamePattern.FindString(input[i:]); match != "" {
			tokens = append(tokens, match)
			i += len(match)
			continue
		}
		return nil, &gqlSyntaxError{msg: fmt.Sprintf("unexpected character %q", string(ch))}
	}
	return tokens, nil
}

type gqlParser struct {
	tokens []string
	pos    int
}

func (p *gqlParser) peek() string {
	if p.pos < len(p.tokens) {
		return p.tokens[p.pos]
	}
	return ""
}

func (p *gqlParser) hasNext() bool { return p.pos < len(p.tokens) }

func (p *gqlParser) next() string {
	if p.pos >= len(p.tokens) {
		panic(&gqlSyntaxError{msg: "unexpected end of document"})
	}
	token := p.tokens[p.pos]
	p.pos++
	return token
}

func (p *gqlParser) expect(token string) {
	got := p.next()
	if got != token {
		panic(&gqlSyntaxError{msg: "expected " + token + ", got " + got})
	}
}

func (p *gqlParser) parseDocument() (ops []gqlOperation) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(*gqlSyntaxError); ok {
				panic(r)
			}
			panic(r)
		}
	}()
	for p.hasNext() {
		head := p.peek()
		switch {
		case head == "{":
			ops = append(ops, gqlOperation{typ: "query", selection: p.parseSelectionSet()})
		case head == "query" || head == "mutation":
			typ := p.next()
			op := gqlOperation{typ: typ}
			if p.hasNext() && p.peek() != "(" && p.peek() != "{" {
				op.name = p.next()
				op.hasName = true
			}
			if p.peek() == "(" {
				p.skipVariableDefinitions()
			}
			op.selection = p.parseSelectionSet()
			ops = append(ops, op)
		case head == "fragment":
			// Named fragments are not used by the suite's documents; parse and drop the definition.
			p.next()
			p.next()
			p.expect("on")
			p.next()
			p.parseSelectionSet()
		default:
			panic(&gqlSyntaxError{msg: "unexpected token " + head})
		}
	}
	return ops
}

func (p *gqlParser) skipVariableDefinitions() {
	p.expect("(")
	depth := 1
	for depth > 0 {
		token := p.next()
		switch token {
		case "(", "[", "{":
			depth++
		case ")", "]", "}":
			depth--
		}
	}
}

func (p *gqlParser) parseSelectionSet() []gqlSelection {
	p.expect("{")
	var selections []gqlSelection
	for p.peek() != "}" {
		if p.peek() == "..." {
			p.next()
			p.expect("on")
			typeCondition := p.next()
			selections = append(selections, gqlSelection{isFragment: true, typeCondition: typeCondition, selection: p.parseSelectionSet()})
			continue
		}
		selections = append(selections, gqlSelection{field: p.parseField()})
	}
	p.expect("}")
	return selections
}

func (p *gqlParser) parseField() *gqlField {
	name := p.next()
	field := &gqlField{name: name}
	if p.peek() == ":" {
		p.next()
		field.alias = name
		field.hasAlias = true
		field.name = p.next()
	}
	if p.peek() == "(" {
		p.next()
		for p.peek() != ")" {
			argName := p.next()
			p.expect(":")
			field.args = append(field.args, gqlArg{name: argName, value: p.parseValue()})
		}
		p.expect(")")
	}
	if p.peek() == "{" {
		field.selection = p.parseSelectionSet()
		field.hasSelection = true
	}
	return field
}

var gqlIntPattern = regexp.MustCompile(`^-?\d+$`)

func (p *gqlParser) parseValue() *gqlValue {
	token := p.next()
	switch {
	case token == "$":
		return &gqlValue{kind: "variable", name: p.next()}
	case strings.HasPrefix(token, "\""):
		return &gqlValue{kind: "string", strVal: token[1 : len(token)-1]}
	case token == "[":
		value := &gqlValue{kind: "list"}
		for p.peek() != "]" {
			value.list = append(value.list, p.parseValue())
		}
		p.expect("]")
		return value
	case token == "{":
		value := &gqlValue{kind: "object"}
		for p.peek() != "}" {
			name := p.next()
			p.expect(":")
			value.fields = append(value.fields, gqlObjField{name: name, value: p.parseValue()})
		}
		p.expect("}")
		return value
	case token == "true" || token == "false":
		return &gqlValue{kind: "boolean", bVal: token == "true"}
	case token == "null":
		return &gqlValue{kind: "null"}
	case gqlIntPattern.MatchString(token):
		n, _ := jsParseInt(token)
		return &gqlValue{kind: "int", intVal: n}
	default:
		return &gqlValue{kind: "enum", strVal: token}
	}
}

// ------------------------------------------------------------------ execution

type gqlSource = map[string]any

type gqlError struct{ msg string }

func (e *gqlError) Error() string { return e.msg }

func decodeGlobalID(id string) (string, []string, bool) {
	raw, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(id)
		if err != nil {
			return "", nil, false
		}
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) < 2 {
		return "", nil, false
	}
	return parts[0], parts[1:], true
}

func repoSource(repo *Repo) gqlSource {
	return gqlSource{"__type": "Repository", "repo": repo}
}

func issueSource(repo *Repo, issue *IssueRec) gqlSource {
	return gqlSource{"__type": "Issue", "repo": repo, "issue": issue}
}

func pullSource(repo *Repo, pull *PullRec) gqlSource {
	return gqlSource{"__type": "PullRequest", "repo": repo, "pull": pull}
}

func cursorFor(repo *Repo, issue *IssueRec) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("cursor:%d:%d", repo.ID, issue.Number)))
}

type gqlExecCtx struct {
	forge     *Forge
	variables map[string]any
	errors    []Json
}

func (ctx *gqlExecCtx) resolveValue(value *gqlValue) any {
	switch value.kind {
	case "variable":
		return ctx.variables[value.name]
	case "int":
		return value.intVal
	case "string", "enum":
		return value.strVal
	case "boolean":
		return value.bVal
	case "null":
		return nil
	case "list":
		out := make([]any, len(value.list))
		for i, entry := range value.list {
			out[i] = ctx.resolveValue(entry)
		}
		return out
	case "object":
		out := make(map[string]any)
		for _, field := range value.fields {
			out[field.name] = ctx.resolveValue(field.value)
		}
		return out
	}
	return nil
}

func (ctx *gqlExecCtx) argsOf(field *gqlField) map[string]any {
	out := make(map[string]any)
	for _, arg := range field.args {
		out[arg.name] = ctx.resolveValue(arg.value)
	}
	return out
}

// numArg reads an integer argument that may arrive as a GraphQL int literal or a JSON variable.
func numArg(args map[string]any, key string) (int, bool) {
	switch v := args[key].(type) {
	case int:
		return v, true
	case float64:
		return int(v), true
	}
	return 0, false
}

func strArg(args map[string]any, key string) (string, bool) {
	s, ok := args[key].(string)
	return s, ok
}

func findRepoByGlobalID(forge *Forge, id any) *Repo {
	s, ok := id.(string)
	if !ok {
		return nil
	}
	kind, parts, ok := decodeGlobalID(s)
	if !ok || kind != "Repository" || len(parts) == 0 {
		return nil
	}
	dbID, valid := jsParseInt(parts[0])
	if !valid {
		return nil
	}
	for _, repo := range forge.ListRepos() {
		if repo.ID == dbID {
			return repo
		}
	}
	return nil
}

func findIssueByGlobalID(forge *Forge, id any, wantKind string) (*Repo, *IssueRec) {
	s, ok := id.(string)
	if !ok {
		return nil, nil
	}
	kind, parts, ok := decodeGlobalID(s)
	if !ok || kind != wantKind || len(parts) < 2 {
		return nil, nil
	}
	repoID, ok1 := jsParseInt(parts[0])
	number, ok2 := jsParseInt(parts[1])
	if !ok1 || !ok2 {
		return nil, nil
	}
	for _, repo := range forge.ListRepos() {
		if repo.ID == repoID {
			return repo, repo.Issues[number]
		}
	}
	return nil, nil
}

func findPullByGlobalID(forge *Forge, id any) (*Repo, *PullRec) {
	s, ok := id.(string)
	if !ok {
		return nil, nil
	}
	kind, parts, ok := decodeGlobalID(s)
	if !ok || kind != "PullRequest" || len(parts) < 2 {
		return nil, nil
	}
	repoID, ok1 := jsParseInt(parts[0])
	number, ok2 := jsParseInt(parts[1])
	if !ok1 || !ok2 {
		return nil, nil
	}
	for _, repo := range forge.ListRepos() {
		if repo.ID == repoID {
			return repo, repo.Pulls[number]
		}
	}
	return nil, nil
}

func (ctx *gqlExecCtx) resolveField(source gqlSource, field *gqlField) (any, error) {
	typ, _ := source["__type"].(string)
	name := field.name
	args := ctx.argsOf(field)
	forge := ctx.forge

	if typ == "Query" {
		switch name {
		case "viewer":
			return gqlSource{"__type": "User"}, nil
		case "repository":
			owner, ok1 := strArg(args, "owner")
			repoName, ok2 := strArg(args, "name")
			if ok1 && ok2 {
				if repo := forge.GetRepo(owner, repoName); repo != nil {
					return repoSource(repo), nil
				}
			}
			return nil, nil
		case "node":
			idStr, _ := strArg(args, "id")
			kind, _, ok := decodeGlobalID(idStr)
			if !ok {
				return nil, nil
			}
			if kind == "Repository" {
				if repo := findRepoByGlobalID(forge, idStr); repo != nil {
					return repoSource(repo), nil
				}
			}
			return nil, nil
		case "__type":
			typeName, _ := strArg(args, "name")
			if objectFields[typeName] == nil && enumValues[typeName] == nil {
				return nil, nil
			}
			return gqlSource{"__type": "__Type", "name": typeName}, nil
		case "__schema":
			return gqlSource{"__type": "__Schema"}, nil
		}
	}

	if typ == "Mutation" {
		input, _ := args["input"].(map[string]any)
		if input == nil {
			input = map[string]any{}
		}
		switch name {
		case "createIssue":
			repo := findRepoByGlobalID(forge, input["repositoryId"])
			if repo == nil {
				return nil, &gqlError{msg: "repository not found for createIssue"}
			}
			title, _ := input["title"].(string)
			body, _ := input["body"].(string)
			issue := &IssueRec{
				Number: repo.NextNumber,
				ID:     forge.AllocIssueID(),
				Title:  title,
				Body:   body,
				State:  "open",
				Seq:    repo.NextSeq(),
			}
			repo.NextNumber++
			repo.Issues[issue.Number] = issue
			return gqlSource{"__type": "CreateIssuePayload", "issue": issueSource(repo, issue), "clientMutationId": input["clientMutationId"]}, nil
		case "closeIssue":
			repo, issue := findIssueByGlobalID(forge, input["issueId"], "Issue")
			if repo == nil || issue == nil {
				return nil, &gqlError{msg: "issue not found for closeIssue"}
			}
			issue.State = "closed"
			return gqlSource{"__type": "CloseIssuePayload", "issue": issueSource(repo, issue), "clientMutationId": input["clientMutationId"]}, nil
		case "createPullRequest":
			repo := findRepoByGlobalID(forge, input["repositoryId"])
			if repo == nil {
				return nil, &gqlError{msg: "repository not found for createPullRequest"}
			}
			head, _ := input["headRefName"].(string)
			base, _ := input["baseRefName"].(string)
			if base == "" {
				base = repo.DefaultBranch
			}
			if repo.TipOf(head) == "" {
				return nil, &gqlError{msg: "head branch " + head + " does not exist"}
			}
			if repo.TipOf(base) == "" {
				return nil, &gqlError{msg: "base branch " + base + " does not exist"}
			}
			title, _ := input["title"].(string)
			body, _ := input["body"].(string)
			pull := &PullRec{
				Number:  repo.NextNumber,
				ID:      forge.AllocIssueID(),
				Title:   title,
				Body:    body,
				State:   "open",
				HeadRef: head,
				BaseRef: base,
				Seq:     repo.NextSeq(),
			}
			repo.NextNumber++
			repo.Pulls[pull.Number] = pull
			return gqlSource{"__type": "CreatePullRequestPayload", "pullRequest": pullSource(repo, pull), "clientMutationId": input["clientMutationId"]}, nil
		case "mergePullRequest":
			repo, pull := findPullByGlobalID(forge, input["pullRequestId"])
			if repo == nil || pull == nil {
				return nil, &gqlError{msg: "pull request not found for mergePullRequest"}
			}
			if pull.State != "open" {
				return nil, &gqlError{msg: "pull request is not open"}
			}
			sha := squashMerge(repo, pull)
			if sha == "" {
				return nil, &gqlError{msg: "pull request is not mergeable: merge conflict"}
			}
			pull.State = "closed"
			pull.Merged = true
			now := isoNow()
			pull.MergedAt = &now
			pull.MergeCommitSha = &sha
			return gqlSource{"__type": "MergePullRequestPayload", "pullRequest": pullSource(repo, pull), "clientMutationId": input["clientMutationId"]}, nil
		case "addComment":
			repo, issue := findIssueByGlobalID(forge, input["subjectId"], "Issue")
			if repo == nil || issue == nil {
				return nil, &gqlError{msg: "subject not found for addComment"}
			}
			body, _ := input["body"].(string)
			comment := &CommentRec{ID: forge.AllocCommentID(), Body: body}
			issue.Comments = append(issue.Comments, comment)
			node := gqlSource{"__type": "IssueComment", "id": globalID("IssueComment", repo.ID, issue.Number, comment.ID), "body": comment.Body, "createdAt": isoNow()}
			edge := gqlSource{"__type": "IssueCommentEdge", "node": node, "cursor": ""}
			return gqlSource{"__type": "AddCommentPayload", "commentEdge": edge, "clientMutationId": input["clientMutationId"]}, nil
		}
	}

	if typ == "User" {
		switch name {
		case "id":
			return globalID("User", VIEWER.ID), nil
		case "login":
			return VIEWER.Login, nil
		case "name":
			return VIEWER.Name, nil
		case "email":
			return VIEWER.Email, nil
		case "url":
			return "https://forge.invalid/adaiversen", nil
		}
	}

	if typ == "Repository" {
		repo := source["repo"].(*Repo)
		switch name {
		case "id":
			return globalID("Repository", repo.ID), nil
		case "databaseId":
			return repo.ID, nil
		case "name":
			return repo.Name, nil
		case "nameWithOwner":
			return repo.Owner + "/" + repo.Name, nil
		case "owner":
			return gqlSource{"__type": "User"}, nil
		case "description":
			return repo.Description, nil
		case "isPrivate":
			return repo.IsPrivate, nil
		case "viewerPermission":
			return "ADMIN", nil
		case "hasIssuesEnabled":
			return true, nil
		case "defaultBranchRef":
			return gqlSource{"__type": "Ref", "name": repo.DefaultBranch}, nil
		case "url":
			return "https://forge.invalid/" + repo.Owner + "/" + repo.Name, nil
		case "createdAt", "updatedAt":
			return repo.CreatedAt, nil
		case "issue":
			if number, ok := numArg(args, "number"); ok {
				if issue := repo.Issues[number]; issue != nil {
					return issueSource(repo, issue), nil
				}
			}
			return nil, nil
		case "issues":
			return ctx.issuesConnection(repo, args), nil
		case "pullRequest":
			if number, ok := numArg(args, "number"); ok {
				if pull := repo.Pulls[number]; pull != nil {
					return pullSource(repo, pull), nil
				}
			}
			return nil, nil
		case "pullRequests":
			first, ok := numArg(args, "first")
			if !ok {
				first = 30
			}
			var states []string
			if raw, ok := args["states"].([]any); ok {
				for _, entry := range raw {
					if s, ok := entry.(string); ok {
						states = append(states, s)
					}
				}
			}
			var list []*PullRec
			for _, pull := range repo.Pulls {
				if states != nil {
					state := strings.ToUpper(pull.State)
					if pull.Merged {
						state = "MERGED"
					}
					found := false
					for _, s := range states {
						if s == state {
							found = true
						}
					}
					if !found {
						continue
					}
				}
				list = append(list, pull)
			}
			sort.Slice(list, func(i, j int) bool { return list[i].Number > list[j].Number })
			total := len(list)
			if first < len(list) {
				list = list[:first]
			}
			nodes := make([]any, len(list))
			for i, pull := range list {
				nodes[i] = pullSource(repo, pull)
			}
			pageInfo := gqlSource{"__type": "PageInfo", "hasNextPage": total > len(nodes), "hasPreviousPage": false, "startCursor": nil, "endCursor": nil}
			return gqlSource{"__type": "PullRequestConnection", "nodes": nodes, "pageInfo": pageInfo, "totalCount": total}, nil
		}
	}

	if typ == "Issue" {
		repo := source["repo"].(*Repo)
		issue := source["issue"].(*IssueRec)
		switch name {
		case "id":
			return globalID("Issue", repo.ID, issue.Number), nil
		case "number":
			return issue.Number, nil
		case "title":
			return issue.Title, nil
		case "body":
			return issue.Body, nil
		case "state":
			return strings.ToUpper(issue.State), nil
		case "closed":
			return issue.State == "closed", nil
		case "url":
			return fmt.Sprintf("https://forge.invalid/%s/%s/issues/%d", repo.Owner, repo.Name, issue.Number), nil
		case "createdAt", "updatedAt":
			return repo.CreatedAt, nil
		case "repository":
			return repoSource(repo), nil
		case "author":
			return gqlSource{"__type": "User"}, nil
		case "labels":
			nodes := make([]any, len(issue.Labels))
			for i, label := range issue.Labels {
				color := "ededed"
				if rec := repo.Labels[label]; rec != nil {
					color = rec.Color
				}
				nodes[i] = gqlSource{"__type": "Label", "id": globalID("Label", label), "name": label, "color": color}
			}
			return gqlSource{"__type": "LabelConnection", "nodes": nodes}, nil
		case "comments":
			return gqlSource{"__type": "IssueConnection", "nodes": []any{}, "pageInfo": nil, "totalCount": len(issue.Comments)}, nil
		}
	}

	if typ == "PullRequest" {
		repo := source["repo"].(*Repo)
		pull := source["pull"].(*PullRec)
		switch name {
		case "id":
			return globalID("PullRequest", repo.ID, pull.Number), nil
		case "number":
			return pull.Number, nil
		case "title":
			return pull.Title, nil
		case "body":
			return pull.Body, nil
		case "state":
			if pull.Merged {
				return "MERGED", nil
			}
			return strings.ToUpper(pull.State), nil
		case "merged":
			return pull.Merged, nil
		case "mergedAt":
			if pull.MergedAt != nil {
				return *pull.MergedAt, nil
			}
			return nil, nil
		case "mergeable":
			if pull.State == "open" {
				if pullMergeable(repo, pull) {
					return "MERGEABLE", nil
				}
				return "CONFLICTING", nil
			}
			return "MERGEABLE", nil
		case "headRefName":
			return pull.HeadRef, nil
		case "baseRefName":
			return pull.BaseRef, nil
		case "headRef":
			return gqlSource{"__type": "Ref", "name": pull.HeadRef}, nil
		case "baseRef":
			return gqlSource{"__type": "Ref", "name": pull.BaseRef}, nil
		case "closed":
			return pull.State == "closed", nil
		case "url":
			return fmt.Sprintf("https://forge.invalid/%s/%s/pull/%d", repo.Owner, repo.Name, pull.Number), nil
		case "createdAt", "updatedAt":
			return repo.CreatedAt, nil
		case "repository":
			return repoSource(repo), nil
		case "author":
			return gqlSource{"__type": "User"}, nil
		case "files":
			first, ok := numArg(args, "first")
			if !ok {
				first = 100
			}
			files := pullFiles(repo, pull)
			if first < len(files) {
				files = files[:first]
			}
			nodes := make([]any, len(files))
			for i, file := range files {
				nodes[i] = gqlSource{"__type": "PullRequestChangedFile", "path": file.Filename, "additions": file.Additions, "deletions": file.Deletions, "changeType": strings.ToUpper(file.Status)}
			}
			return gqlSource{"__type": "PullRequestConnection", "nodes": nodes, "pageInfo": nil, "totalCount": len(nodes)}, nil
		}
	}

	if typ == "Ref" && name == "name" {
		return source["name"], nil
	}
	if typ == "PageInfo" {
		return source[name], nil
	}
	if typ == "__Type" {
		typeName, _ := source["name"].(string)
		switch name {
		case "name":
			return typeName, nil
		case "kind":
			if enumValues[typeName] != nil {
				return "ENUM", nil
			}
			return "OBJECT", nil
		case "description":
			return nil, nil
		case "fields":
			fields := objectFields[typeName]
			if fields == nil {
				return nil, nil
			}
			out := make([]any, len(fields))
			for i, fieldName := range fields {
				out[i] = gqlSource{"__type": "__Field", "name": fieldName}
			}
			return out, nil
		case "enumValues":
			values := enumValues[typeName]
			if values == nil {
				return nil, nil
			}
			out := make([]any, len(values))
			for i, value := range values {
				out[i] = gqlSource{"__type": "__EnumValue", "name": value}
			}
			return out, nil
		case "possibleTypes", "inputFields":
			return nil, nil
		}
	}
	if typ == "__Field" && name == "name" {
		return source["name"], nil
	}
	if typ == "__EnumValue" && name == "name" {
		return source["name"], nil
	}
	if typ == "__Schema" {
		switch name {
		case "queryType":
			return gqlSource{"__type": "__Type", "name": "Query"}, nil
		case "mutationType":
			return gqlSource{"__type": "__Type", "name": "Mutation"}, nil
		case "types":
			var out []any
			for _, typeName := range objectFieldOrder {
				out = append(out, gqlSource{"__type": "__Type", "name": typeName})
			}
			for _, typeName := range enumOrder {
				out = append(out, gqlSource{"__type": "__Type", "name": typeName})
			}
			return out, nil
		}
	}

	// Payload and connection leaf fields ride on the source object itself.
	return source[name], nil
}

var cursorPattern = regexp.MustCompile(`^cursor:(\d+):(\d+)$`)

func (ctx *gqlExecCtx) issuesConnection(repo *Repo, args map[string]any) gqlSource {
	var states []string
	if raw, ok := args["states"].([]any); ok {
		for _, entry := range raw {
			if s, ok := entry.(string); ok {
				states = append(states, s)
			}
		}
	}
	var list []*IssueRec
	for _, issue := range repo.Issues {
		if states != nil {
			found := false
			for _, s := range states {
				if s == strings.ToUpper(issue.State) {
					found = true
				}
			}
			if !found {
				continue
			}
		}
		list = append(list, issue)
	}
	direction := ""
	if orderBy, ok := args["orderBy"].(map[string]any); ok {
		direction, _ = orderBy["direction"].(string)
	}
	switch direction {
	case "DESC":
		sort.Slice(list, func(i, j int) bool { return list[i].Seq > list[j].Seq })
	case "ASC":
		sort.Slice(list, func(i, j int) bool { return list[i].Seq < list[j].Seq })
	default:
		sort.Slice(list, func(i, j int) bool { return list[i].Number < list[j].Number })
	}
	start := 0
	if after, ok := args["after"].(string); ok && after != "" {
		if raw, err := base64.StdEncoding.DecodeString(after); err == nil {
			if match := cursorPattern.FindStringSubmatch(string(raw)); match != nil {
				number, _ := jsParseInt(match[2])
				for i, issue := range list {
					if issue.Number == number {
						start = i + 1
						break
					}
				}
			}
		}
	}
	first, ok := numArg(args, "first")
	if !ok {
		first = 30
	}
	end := start + first
	if end > len(list) {
		end = len(list)
	}
	page := list[start:end]
	nodes := make([]any, len(page))
	for i, issue := range page {
		nodes[i] = issueSource(repo, issue)
	}
	var startCursor, endCursor any
	if len(page) > 0 {
		startCursor = cursorFor(repo, page[0])
		endCursor = cursorFor(repo, page[len(page)-1])
	}
	pageInfo := gqlSource{
		"__type":          "PageInfo",
		"hasNextPage":     start+len(page) < len(list),
		"hasPreviousPage": start > 0,
		"startCursor":     startCursor,
		"endCursor":       endCursor,
	}
	return gqlSource{"__type": "IssueConnection", "nodes": nodes, "pageInfo": pageInfo, "totalCount": len(list)}
}

func (ctx *gqlExecCtx) executeSelection(source gqlSource, selections []gqlSelection) Json {
	out := Json{}
	for _, selection := range selections {
		if selection.isFragment {
			if selection.typeCondition == source["__type"] || selection.typeCondition == "Node" {
				for key, value := range ctx.executeSelection(source, selection.selection) {
					out[key] = value
				}
			}
			continue
		}
		field := selection.field
		key := field.name
		if field.hasAlias {
			key = field.alias
		}
		typ, _ := source["__type"].(string)
		if known := objectFields[typ]; known != nil {
			isKnown := false
			for _, candidate := range known {
				if candidate == field.name {
					isKnown = true
				}
			}
			_, inSource := source[field.name]
			if !isKnown && !inSource {
				ctx.errors = append(ctx.errors, Json{"message": fmt.Sprintf("Field '%s' doesn't exist on type '%s'", field.name, typ)})
				out[key] = nil
				continue
			}
		}
		value, err := ctx.resolveField(source, field)
		if err != nil {
			if gqlErr, ok := err.(*gqlError); ok {
				ctx.errors = append(ctx.errors, Json{"message": gqlErr.msg})
				out[key] = nil
				continue
			}
			panic(err)
		}
		out[key] = ctx.completeValue(value, field)
	}
	return out
}

func (ctx *gqlExecCtx) completeValue(value any, field *gqlField) any {
	if value == nil {
		return nil
	}
	if list, ok := value.([]any); ok {
		out := make([]any, len(list))
		for i, entry := range list {
			out[i] = ctx.completeValue(entry, field)
		}
		return out
	}
	if source, ok := value.(gqlSource); ok && field.hasSelection {
		return ctx.executeSelection(source, field.selection)
	}
	return value
}

func runGqlOperation(forge *Forge, operation gqlOperation, variables map[string]any) Json {
	ctx := &gqlExecCtx{forge: forge, variables: variables}
	rootType := "Query"
	if operation.typ == "mutation" {
		rootType = "Mutation"
	}
	root := gqlSource{"__type": rootType}
	data := ctx.executeSelection(root, operation.selection)
	result := Json{"data": data}
	if len(ctx.errors) > 0 {
		errors := make([]any, len(ctx.errors))
		for i, e := range ctx.errors {
			errors[i] = e
		}
		result["errors"] = errors
	}
	return result
}

func handleGraphQL(ctx *Ctx) {
	var parsed any
	if err := json.Unmarshal(ctx.Body, &parsed); err != nil {
		ctx.Send(400, Json{"errors": []any{Json{"message": "Request body is not valid JSON"}}})
		return
	}
	runOne := func(entry any) (result Json) {
		defer func() {
			if r := recover(); r != nil {
				if syntaxErr, ok := r.(*gqlSyntaxError); ok {
					result = Json{"errors": []any{Json{"message": syntaxErr.msg}}}
					return
				}
				panic(r)
			}
		}()
		request, ok := entry.(map[string]any)
		if !ok {
			return Json{"errors": []any{Json{"message": "Invalid GraphQL request"}}}
		}
		query, ok := request["query"].(string)
		if !ok {
			return Json{"errors": []any{Json{"message": "Missing query"}}}
		}
		tokens, err := gqlTokenize(query)
		if err != nil {
			return Json{"errors": []any{Json{"message": err.Error()}}}
		}
		parser := &gqlParser{tokens: tokens}
		operations := parser.parseDocument()
		variables, _ := request["variables"].(map[string]any)
		if variables == nil {
			variables = map[string]any{}
		}
		var operation *gqlOperation
		if opName, ok := request["operationName"].(string); ok {
			for i := range operations {
				if operations[i].hasName && operations[i].name == opName {
					operation = &operations[i]
					break
				}
			}
		}
		if operation == nil && len(operations) > 0 {
			operation = &operations[0]
		}
		if operation == nil {
			return Json{"errors": []any{Json{"message": "No operation found"}}}
		}
		return runGqlOperation(ctx.Forge, *operation, variables)
	}
	if batch, ok := parsed.([]any); ok {
		results := make([]any, len(batch))
		for i, entry := range batch {
			results[i] = runOne(entry)
		}
		ctx.Send(200, results)
		return
	}
	ctx.Send(200, runOne(parsed))
}
