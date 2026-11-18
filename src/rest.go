// The REST v3 surface: repositories, branches, commits, contents, labels, issues, comments and
// pull requests, in the shapes GitHub's clients read. Handlers receive the shared Ctx from
// main.go and answer through its JSON helpers; ownership is the store's rule (the token's login
// owns every repository here, and the same name under another owner is a 404).
package main

import (
	"encoding/base64"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Json = map[string]any

// ------------------------------------------------------------------ JSON views

func userJson() Json {
	return Json{"login": VIEWER.Login, "id": VIEWER.ID, "type": "User", "name": VIEWER.Name, "email": VIEWER.Email}
}

func globalID(kind string, parts ...any) string {
	strs := make([]string, len(parts))
	for i, part := range parts {
		switch v := part.(type) {
		case string:
			strs[i] = v
		case int:
			strs[i] = strconv.Itoa(v)
		}
	}
	return base64.StdEncoding.EncodeToString([]byte(kind + ":" + strings.Join(strs, ":")))
}

func repoJson(repo *Repo) Json {
	return Json{
		"id":             repo.ID,
		"node_id":        globalID("Repository", repo.ID),
		"name":           repo.Name,
		"full_name":      repo.Owner + "/" + repo.Name,
		"owner":          Json{"login": repo.Owner, "id": VIEWER.ID, "type": "User"},
		"private":        repo.IsPrivate,
		"description":    repo.Description,
		"default_branch": repo.DefaultBranch,
		"has_issues":     true,
		"created_at":     repo.CreatedAt,
		"updated_at":     repo.CreatedAt,
		"html_url":       "https://forge.invalid/" + repo.Owner + "/" + repo.Name,
		"clone_url":      "https://forge.invalid/" + repo.Owner + "/" + repo.Name + ".git",
	}
}

func labelJson(repo *Repo, name string) Json {
	color := "ededed"
	if label := repo.Labels[name]; label != nil {
		color = label.Color
	}
	return Json{"name": name, "color": color, "description": nil, "default": false}
}

func issueJson(repo *Repo, issue *IssueRec) Json {
	labels := make([]any, len(issue.Labels))
	for i, name := range issue.Labels {
		labels[i] = labelJson(repo, name)
	}
	return Json{
		"id":         issue.ID,
		"node_id":    globalID("Issue", repo.ID, issue.Number),
		"number":     issue.Number,
		"title":      issue.Title,
		"body":       issue.Body,
		"state":      issue.State,
		"labels":     labels,
		"user":       Json{"login": VIEWER.Login, "id": VIEWER.ID, "type": "User"},
		"comments":   len(issue.Comments),
		"created_at": repo.CreatedAt,
		"updated_at": repo.CreatedAt,
	}
}

func commentJson(comment *CommentRec) Json {
	now := isoNow()
	return Json{
		"id":         comment.ID,
		"node_id":    globalID("IssueComment", comment.ID),
		"body":       comment.Body,
		"user":       Json{"login": VIEWER.Login, "id": VIEWER.ID, "type": "User"},
		"created_at": now,
		"updated_at": now,
	}
}

func pullJson(repo *Repo, pull *PullRec) Json {
	headSha := repo.TipOf(pull.HeadRef)
	if headSha == "" {
		headSha = ZeroSHA
	}
	baseSha := repo.TipOf(pull.BaseRef)
	if baseSha == "" {
		baseSha = ZeroSHA
	}
	var mergeable any
	if pull.State == "open" {
		mergeable = pullMergeable(repo, pull)
	}
	var mergedAt any
	if pull.MergedAt != nil {
		mergedAt = *pull.MergedAt
	}
	var mergeCommitSha any
	if pull.MergeCommitSha != nil {
		mergeCommitSha = *pull.MergeCommitSha
	}
	return Json{
		"id":               pull.ID,
		"node_id":          globalID("PullRequest", repo.ID, pull.Number),
		"number":           pull.Number,
		"title":            pull.Title,
		"body":             pull.Body,
		"state":            pull.State,
		"merged":           pull.Merged,
		"merged_at":        mergedAt,
		"merge_commit_sha": mergeCommitSha,
		"mergeable":        mergeable,
		"user":             Json{"login": VIEWER.Login, "id": VIEWER.ID, "type": "User"},
		"head":             Json{"label": repo.Owner + ":" + pull.HeadRef, "ref": pull.HeadRef, "sha": headSha},
		"base":             Json{"label": repo.Owner + ":" + pull.BaseRef, "ref": pull.BaseRef, "sha": baseSha},
		"created_at":       repo.CreatedAt,
		"updated_at":       repo.CreatedAt,
	}
}

// ------------------------------------------------------------------ helpers

// jsParseInt mirrors JavaScript's Number.parseInt: leading optional sign and digits, or NaN.
func jsParseInt(s string) (int, bool) {
	s = strings.TrimLeft(s, " \t\n\r")
	if s == "" {
		return 0, false
	}
	i := 0
	neg := false
	if s[0] == '+' || s[0] == '-' {
		neg = s[0] == '-'
		i++
	}
	start := i
	value := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		value = value*10 + int(s[i]-'0')
		i++
	}
	if i == start {
		return 0, false
	}
	if neg {
		value = -value
	}
	return value, true
}

func pageParams(ctx *Ctx) (int, int) {
	page, ok := jsParseInt(ctx.URL.Query().Get("page"))
	if !ok || page < 1 {
		page = 1
	}
	perPage, ok := jsParseInt(ctx.URL.Query().Get("per_page"))
	if !ok || perPage < 1 {
		perPage = 30
	}
	if perPage > 100 {
		perPage = 100
	}
	return page, perPage
}

func paginate[T any](ctx *Ctx, items []T) []T {
	page, perPage := pageParams(ctx)
	start := (page - 1) * perPage
	if start+perPage < len(items) {
		base := ctx.Origin + ctx.URL.EscapedPath()
		next := base + "?per_page=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(page+1)
		lastPage := (len(items) + perPage - 1) / perPage
		last := base + "?per_page=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(lastPage)
		ctx.W.Header().Set("Link", "<"+next+">; rel=\"next\", <"+last+">; rel=\"last\"")
	}
	if start >= len(items) {
		return []T{}
	}
	end := start + perPage
	if end > len(items) {
		end = len(items)
	}
	return items[start:end]
}

func bodyObject(ctx *Ctx) Json {
	parsed := ctx.JSON()
	if obj, ok := parsed.(map[string]any); ok {
		return obj
	}
	return Json{}
}

func bodyString(body Json, key string) string {
	if v, ok := body[key].(string); ok {
		return v
	}
	return ""
}

func branchOf(value any) string {
	s, ok := value.(string)
	if !ok {
		return ""
	}
	if colon := strings.IndexByte(s, ':'); colon >= 0 {
		return s[colon+1:]
	}
	return s
}

var shaPattern = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// ------------------------------------------------------------------ the router

func handleRest(ctx *Ctx, segments []string) {
	forge := ctx.Forge
	if len(segments) == 0 {
		if ctx.R.Method != "GET" {
			ctx.NotFound()
			return
		}
		ctx.Send(200, Json{
			"current_user_url":       ctx.Origin + "/api/v3/user",
			"repository_url":         ctx.Origin + "/api/v3/repos/{owner}/{repo}",
			"user_repositories_url":  ctx.Origin + "/api/v3/user/repos",
			"issue_search_url":       ctx.Origin + "/api/v3/search/issues",
		})
		return
	}
	head := segments[0]
	rest := segments[1:]

	if head == "meta" && len(segments) == 1 {
		if ctx.R.Method != "GET" {
			ctx.NotFound()
			return
		}
		ctx.Send(200, Json{"installed_version": "3.10.0-forge", "verifiable_password_authentication": false})
		return
	}

	if head == "user" && len(segments) == 1 {
		if ctx.R.Method != "GET" {
			ctx.NotFound()
			return
		}
		ctx.Send(200, userJson())
		return
	}

	if head == "user" && len(rest) == 1 && rest[0] == "repos" {
		switch ctx.R.Method {
		case "GET":
			repos := forge.ListRepos()
			views := make([]Json, len(repos))
			for i, repo := range repos {
				views[i] = repoJson(repo)
			}
			ctx.Send(200, paginate(ctx, views))
		case "POST":
			body := bodyObject(ctx)
			name := bodyString(body, "name")
			if name == "" {
				ctx.Fail(422, "Validation Failed: name is required")
				return
			}
			isPrivate := true
			if priv, ok := body["private"].(bool); ok {
				isPrivate = priv
			}
			created, ok := forge.CreateRepo(name, bodyString(body, "description"), isPrivate)
			if !ok {
				ctx.Fail(422, "Validation Failed: name already exists on this account")
				return
			}
			if body["auto_init"] == true {
				_, _, _, _ = forge.WriteFile(created, created.DefaultBranch, "README.md", []byte("# "+name+"\n"), "Initial commit", nil, "")
			}
			ctx.Send(201, repoJson(created))
		default:
			ctx.NotFound()
		}
		return
	}

	if head == "repos" && len(rest) >= 2 {
		owner := pathUnescape(rest[0])
		name := pathUnescape(rest[1])
		repo := forge.GetRepo(owner, name)
		if repo == nil {
			ctx.Fail(404, "Not Found")
			return
		}
		handleRepo(ctx, repo, rest[2:])
		return
	}

	ctx.NotFound()
}

func handleRepo(ctx *Ctx, repo *Repo, segments []string) {
	method := ctx.R.Method
	if len(segments) == 0 {
		switch method {
		case "GET":
			ctx.Send(200, repoJson(repo))
		case "PATCH":
			body := bodyObject(ctx)
			if desc, ok := body["description"].(string); ok {
				repo.Description = desc
			}
			if priv, ok := body["private"].(bool); ok {
				repo.IsPrivate = priv
			}
			if name, ok := body["name"].(string); ok && name != "" && name != repo.Name {
				// Renaming moves the map key; nothing in the suite renames, but keep the store coherent.
				delete(ctx.Forge.Repos, ctx.Forge.repoKey(repo.Owner, repo.Name))
				repo.Name = name
				ctx.Forge.Repos[ctx.Forge.repoKey(repo.Owner, repo.Name)] = repo
			}
			ctx.Send(200, repoJson(repo))
		case "DELETE":
			ctx.Forge.DeleteRepo(repo.Owner, repo.Name)
			ctx.SendNoContent()
		default:
			ctx.NotFound()
		}
		return
	}
	switch segments[0] {
	case "branches":
		handleBranches(ctx, repo, segments[1:])
	case "commits":
		handleCommits(ctx, repo, segments[1:])
	case "contents":
		handleContents(ctx, repo, strings.Join(segments[1:], "/"))
	case "git":
		handleGitData(ctx, repo, segments[1:])
	case "labels":
		handleLabels(ctx, repo, segments[1:])
	case "issues":
		handleIssues(ctx, repo, segments[1:])
	case "pulls":
		handlePulls(ctx, repo, segments[1:])
	default:
		ctx.NotFound()
	}
}

// ------------------------------------------------------------------ branches & commits

func handleBranches(ctx *Ctx, repo *Repo, segments []string) {
	if len(segments) == 0 {
		if ctx.R.Method != "GET" {
			ctx.NotFound()
			return
		}
		type branchView struct {
			name string
			sha  string
		}
		var list []branchView
		for ref, sha := range repo.Refs {
			if strings.HasPrefix(ref, "refs/heads/") {
				list = append(list, branchView{name: ref[len("refs/heads/"):], sha: sha})
			}
		}
		sort.Slice(list, func(i, j int) bool { return list[i].name < list[j].name })
		views := make([]Json, len(list))
		for i, b := range list {
			views[i] = Json{"name": b.name, "commit": Json{"sha": b.sha}, "protected": false}
		}
		ctx.Send(200, paginate(ctx, views))
		return
	}
	if ctx.R.Method != "GET" {
		ctx.NotFound()
		return
	}
	parts := make([]string, len(segments))
	for i, s := range segments {
		parts[i] = pathUnescape(s)
	}
	branch := strings.Join(parts, "/")
	sha := repo.TipOf(branch)
	if sha == "" {
		ctx.Fail(404, "Branch not found")
		return
	}
	ctx.Send(200, Json{"name": branch, "commit": Json{"sha": sha}, "protected": false})
}

var identPattern = regexp.MustCompile(`^(.*) <(.*)> (\d+) `)

func commitJson(repo *Repo, sha string) Json {
	var parsed *CommitInfo
	if obj := repo.Objects.Get(sha); obj != nil && obj.Type == "commit" {
		c := parseCommit(obj.Data)
		parsed = &c
	}
	identOf := func(line string) Json {
		m := identPattern.FindStringSubmatch(line)
		name, email, date := "", "", ""
		if m != nil {
			name, email = m[1], m[2]
			sec, _ := strconv.ParseInt(m[3], 10, 64)
			date = time.Unix(sec, 0).UTC().Format("2006-01-02T15:04:05.000Z")
		} else {
			date = time.Unix(0, 0).UTC().Format("2006-01-02T15:04:05.000Z")
		}
		return Json{"name": name, "email": email, "date": date}
	}
	message, tree := "", ZeroSHA
	var parents []string
	var author, committer string
	if parsed != nil {
		message = strings.TrimSuffix(parsed.Message, "\n")
		tree = parsed.Tree
		parents = parsed.Parents
		author, committer = parsed.Author, parsed.Committer
	}
	parentViews := make([]any, len(parents))
	for i, p := range parents {
		parentViews[i] = Json{"sha": p}
	}
	return Json{
		"sha": sha,
		"commit": Json{
			"message":   message,
			"tree":      Json{"sha": tree},
			"author":    identOf(author),
			"committer": identOf(committer),
		},
		"parents": parentViews,
	}
}

func handleCommits(ctx *Ctx, repo *Repo, segments []string) {
	if ctx.R.Method != "GET" {
		ctx.NotFound()
		return
	}
	if len(segments) == 1 {
		sha := pathUnescape(segments[0])
		if !repo.Objects.Has(sha) {
			ctx.Fail(404, "Not Found")
			return
		}
		ctx.Send(200, commitJson(repo, sha))
		return
	}
	shaParam := ctx.URL.Query().Get("sha")
	var tip string
	switch {
	case shaParam == "":
		tip = repo.TipOf(repo.DefaultBranch)
	case shaPattern.MatchString(shaParam):
		if repo.Objects.Has(shaParam) {
			tip = shaParam
		}
	default:
		tip = repo.TipOf(shaParam)
	}
	if tip == "" {
		ctx.Send(200, []any{})
		return
	}
	list := ancestors(repo.Objects, tip)
	views := make([]Json, len(list))
	for i, sha := range list {
		views[i] = commitJson(repo, sha)
	}
	ctx.Send(200, paginate(ctx, views))
}

// ------------------------------------------------------------------ contents

func handleContents(ctx *Ctx, repo *Repo, rawPath string) {
	rawParts := strings.Split(rawPath, "/")
	parts := make([]string, len(rawParts))
	for i, s := range rawParts {
		parts[i] = pathUnescape(s)
	}
	path := strings.Join(parts, "/")
	if rawPath == "" {
		path = ""
	}
	method := ctx.R.Method

	if method == "GET" {
		refParam := ctx.URL.Query().Get("ref")
		var tip string
		switch {
		case refParam == "":
			tip = repo.TipOf(repo.DefaultBranch)
		case shaPattern.MatchString(refParam) && repo.Objects.Has(refParam):
			tip = refParam
		default:
			tip = repo.TipOf(refParam)
		}
		if tip == "" {
			ctx.Fail(404, "Not Found")
			return
		}
		tree := repo.TreeOfCommit(tip)
		if path == "" {
			listTree(ctx, repo, tree, "")
			return
		}
		found := treeLookup(repo.Objects, tree, strings.Split(path, "/"))
		if found == nil {
			ctx.Fail(404, "Not Found")
			return
		}
		if found.Mode == "40000" {
			listTree(ctx, repo, found.Sha, path)
			return
		}
		blob := repo.Objects.Get(found.Sha)
		if blob == nil {
			ctx.Fail(404, "Not Found")
			return
		}
		name := parts[len(parts)-1]
		ctx.Send(200, Json{
			"type":     "file",
			"encoding": "base64",
			"content":  base64.StdEncoding.EncodeToString(blob.Data),
			"name":     name,
			"path":     path,
			"sha":      found.Sha,
			"size":     len(blob.Data),
		})
		return
	}

	if method == "PUT" {
		body := bodyObject(ctx)
		message := bodyString(body, "message")
		if message == "" {
			message = "Update file"
		}
		branch := bodyString(body, "branch")
		if branch == "" {
			branch = repo.DefaultBranch
		}
		var content []byte
		if encoded, ok := body["content"].(string); ok {
			decoded, err := base64.StdEncoding.DecodeString(encoded)
			if err == nil {
				content = decoded
			}
		}
		if content == nil {
			content = []byte{}
		}
		var expectSha *string
		if sha, ok := body["sha"].(string); ok {
			expectSha = &sha
		}
		commitSha, blobSha, created, err := ctx.Forge.WriteFile(repo, branch, path, content, message, expectSha, repo.DefaultBranch)
		if err != nil {
			if _, stale := err.(*StaleShaError); stale {
				ctx.Fail(409, "Conflict: the file sha does not match")
				return
			}
			panic(err)
		}
		name := parts[len(parts)-1]
		status := 200
		if created {
			status = 201
		}
		ctx.Send(status, Json{
			"content": Json{"name": name, "path": path, "sha": blobSha, "size": len(content), "encoding": "base64", "type": "file"},
			"commit":  Json{"sha": commitSha, "message": message},
		})
		return
	}

	if method == "DELETE" {
		body := bodyObject(ctx)
		message := bodyString(body, "message")
		if message == "" {
			message = "Delete file"
		}
		branch := bodyString(body, "branch")
		if branch == "" {
			branch = repo.DefaultBranch
		}
		var expectSha *string
		if sha, ok := body["sha"].(string); ok {
			expectSha = &sha
		}
		commitSha, _, _, err := ctx.Forge.WriteFile(repo, branch, path, nil, message, expectSha, "")
		if err != nil {
			if _, stale := err.(*StaleShaError); stale {
				ctx.Fail(409, "Conflict: the file sha does not match")
				return
			}
			if _, notFound := err.(*NotFoundError); notFound {
				ctx.Fail(404, "Not Found")
				return
			}
			panic(err)
		}
		ctx.Send(200, Json{"content": nil, "commit": Json{"sha": commitSha, "message": message}})
		return
	}

	ctx.NotFound()
}

func listTree(ctx *Ctx, repo *Repo, treeSha, prefix string) {
	obj := repo.Objects.Get(treeSha)
	if treeSha == "" || obj == nil || obj.Type != "tree" {
		ctx.Fail(404, "Not Found")
		return
	}
	entries := parseTree(obj.Data)
	views := make([]Json, len(entries))
	for i, entry := range entries {
		isDir := entry.Mode == "40000"
		size := 0
		if !isDir {
			if blob := repo.Objects.Get(entry.Sha); blob != nil {
				size = len(blob.Data)
			}
		}
		entryPath := entry.Name
		if prefix != "" {
			entryPath = prefix + "/" + entry.Name
		}
		typ := "file"
		if isDir {
			typ = "dir"
		}
		views[i] = Json{"type": typ, "name": entry.Name, "path": entryPath, "sha": entry.Sha, "size": size}
	}
	sort.Slice(views, func(i, j int) bool { return views[i]["name"].(string) < views[j]["name"].(string) })
	ctx.Send(200, views)
}

// ------------------------------------------------------------------ git data (refs)

func handleGitData(ctx *Ctx, repo *Repo, segments []string) {
	if segments[0] != "refs" {
		ctx.NotFound()
		return
	}
	if len(segments) == 1 {
		if ctx.R.Method != "POST" {
			ctx.NotFound()
			return
		}
		body := bodyObject(ctx)
		ref := bodyString(body, "ref")
		sha := bodyString(body, "sha")
		if !strings.HasPrefix(ref, "refs/") || !repo.Objects.Has(sha) {
			ctx.Fail(422, "Validation Failed: invalid ref or sha")
			return
		}
		repo.Refs[ref] = sha
		objType := "commit"
		if obj := repo.Objects.Get(sha); obj != nil {
			objType = obj.Type
		}
		ctx.Send(201, Json{"ref": ref, "object": Json{"sha": sha, "type": objType}})
		return
	}
	if ctx.R.Method != "GET" {
		ctx.NotFound()
		return
	}
	parts := make([]string, len(segments)-1)
	for i, s := range segments[1:] {
		parts[i] = pathUnescape(s)
	}
	ref := "refs/" + strings.Join(parts, "/")
	sha := repo.Refs[ref]
	if sha == "" {
		ctx.Fail(404, "Reference not found")
		return
	}
	objType := "commit"
	if obj := repo.Objects.Get(sha); obj != nil {
		objType = obj.Type
	}
	ctx.Send(200, Json{"ref": ref, "object": Json{"sha": sha, "type": objType}})
}

// ------------------------------------------------------------------ labels

func handleLabels(ctx *Ctx, repo *Repo, segments []string) {
	if len(segments) == 0 {
		switch ctx.R.Method {
		case "GET":
			views := make([]Json, 0, len(repo.LabelOrder))
			for _, name := range repo.LabelOrder {
				views = append(views, labelJson(repo, name))
			}
			ctx.Send(200, views)
		case "POST":
			body := bodyObject(ctx)
			name := bodyString(body, "name")
			if name == "" {
				ctx.Fail(422, "Validation Failed: name is required")
				return
			}
			if repo.Labels[name] != nil {
				ctx.Fail(422, "Validation Failed: label already exists")
				return
			}
			color := "ededed"
			if c, ok := body["color"].(string); ok {
				color = strings.TrimPrefix(c, "#")
			}
			repo.Labels[name] = &LabelRec{Name: name, Color: color}
			repo.LabelOrder = append(repo.LabelOrder, name)
			ctx.Send(201, labelJson(repo, name))
		default:
			ctx.NotFound()
		}
		return
	}
	ctx.NotFound()
}

// ------------------------------------------------------------------ issues

func parseLabels(body Json, repo *Repo) ([]string, bool) {
	raw, ok := body["labels"].([]any)
	if !ok {
		return nil, true
	}
	var labels []string
	for _, entry := range raw {
		if s, ok := entry.(string); ok && s != "" {
			labels = append(labels, s)
		}
	}
	for _, label := range labels {
		if repo.Labels[label] == nil {
			return nil, false
		}
	}
	return labels, true
}

func handleIssues(ctx *Ctx, repo *Repo, segments []string) {
	method := ctx.R.Method
	if len(segments) == 0 {
		switch method {
		case "GET":
			state := ctx.URL.Query().Get("state")
			if state == "" {
				state = "open"
			}
			direction := ctx.URL.Query().Get("direction")
			if direction == "" {
				direction = "desc"
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
			sort.Slice(list, func(i, j int) bool {
				if direction == "asc" {
					return list[i].Seq < list[j].Seq
				}
				return list[i].Seq > list[j].Seq
			})
			views := make([]Json, len(list))
			for i, issue := range list {
				views[i] = issueJson(repo, issue)
			}
			ctx.Send(200, paginate(ctx, views))
		case "POST":
			body := bodyObject(ctx)
			title := bodyString(body, "title")
			if title == "" {
				ctx.Fail(422, "Validation Failed: title is required")
				return
			}
			labels, valid := parseLabels(body, repo)
			if !valid {
				bad := ""
				if raw, ok := body["labels"].([]any); ok {
					for _, entry := range raw {
						if s, ok := entry.(string); ok && repo.Labels[s] == nil {
							bad = s
							break
						}
					}
				}
				ctx.Fail(422, "Validation Failed: label '"+bad+"' does not exist")
				return
			}
			issue := &IssueRec{
				Number: repo.NextNumber,
				ID:     ctx.Forge.AllocIssueID(),
				Title:  title,
				Body:   bodyString(body, "body"),
				State:  "open",
				Labels: labels,
				Seq:    repo.NextSeq(),
			}
			repo.NextNumber++
			repo.Issues[issue.Number] = issue
			ctx.Send(201, issueJson(repo, issue))
		default:
			ctx.NotFound()
		}
		return
	}

	number, valid := jsParseInt(segments[0])
	var issue *IssueRec
	if valid {
		issue = repo.Issues[number]
	}
	if issue == nil {
		ctx.Fail(404, "Issue not found")
		return
	}

	if len(segments) == 1 {
		switch method {
		case "GET":
			ctx.Send(200, issueJson(repo, issue))
		case "PATCH":
			body := bodyObject(ctx)
			if state, ok := body["state"].(string); ok && (state == "open" || state == "closed") {
				issue.State = state
			}
			if title, ok := body["title"].(string); ok {
				issue.Title = title
			}
			if b, ok := body["body"].(string); ok {
				issue.Body = b
			}
			if _, present := body["labels"]; present {
				labels, validLabels := parseLabels(body, repo)
				if !validLabels {
					bad := ""
					if raw, ok := body["labels"].([]any); ok {
						for _, entry := range raw {
							if s, ok := entry.(string); ok && repo.Labels[s] == nil {
								bad = s
								break
							}
						}
					}
					ctx.Fail(422, "Validation Failed: label '"+bad+"' does not exist")
					return
				}
				issue.Labels = labels
			}
			ctx.Send(200, issueJson(repo, issue))
		default:
			ctx.NotFound()
		}
		return
	}

	if len(segments) == 2 && segments[1] == "comments" {
		switch method {
		case "GET":
			views := make([]Json, len(issue.Comments))
			for i, comment := range issue.Comments {
				views[i] = commentJson(comment)
			}
			ctx.Send(200, paginate(ctx, views))
		case "POST":
			body := bodyObject(ctx)
			comment := &CommentRec{ID: ctx.Forge.AllocCommentID(), Body: bodyString(body, "body")}
			issue.Comments = append(issue.Comments, comment)
			ctx.Send(201, commentJson(comment))
		default:
			ctx.NotFound()
		}
		return
	}
	ctx.NotFound()
}

// ------------------------------------------------------------------ pull requests

func handlePulls(ctx *Ctx, repo *Repo, segments []string) {
	method := ctx.R.Method
	if len(segments) == 0 {
		switch method {
		case "GET":
			state := ctx.URL.Query().Get("state")
			if state == "" {
				state = "open"
			}
			var list []*PullRec
			for _, pull := range repo.Pulls {
				if state == "open" && pull.State != "open" {
					continue
				}
				if state == "closed" && pull.State != "closed" {
					continue
				}
				list = append(list, pull)
			}
			sort.Slice(list, func(i, j int) bool { return list[i].Seq < list[j].Seq })
			views := make([]Json, len(list))
			for i, pull := range list {
				views[i] = pullJson(repo, pull)
			}
			ctx.Send(200, paginate(ctx, views))
		case "POST":
			body := bodyObject(ctx)
			head := branchOf(body["head"])
			base := branchOf(body["base"])
			if base == "" {
				base = repo.DefaultBranch
			}
			if head == "" || repo.TipOf(head) == "" {
				ctx.Fail(422, "Validation Failed: head branch does not exist")
				return
			}
			if repo.TipOf(base) == "" {
				ctx.Fail(422, "Validation Failed: base branch does not exist")
				return
			}
			if head == base {
				ctx.Fail(422, "Validation Failed: head and base are the same")
				return
			}
			for _, existing := range repo.Pulls {
				if existing.State == "open" && existing.HeadRef == head && existing.BaseRef == base {
					ctx.Fail(422, "Validation Failed: a pull request already exists for this branch")
					return
				}
			}
			pull := &PullRec{
				Number:  repo.NextNumber,
				ID:      ctx.Forge.AllocIssueID(),
				Title:   bodyString(body, "title"),
				Body:    bodyString(body, "body"),
				State:   "open",
				HeadRef: head,
				BaseRef: base,
				Seq:     repo.NextSeq(),
			}
			repo.NextNumber++
			repo.Pulls[pull.Number] = pull
			ctx.Send(201, pullJson(repo, pull))
		default:
			ctx.NotFound()
		}
		return
	}

	number, valid := jsParseInt(segments[0])
	var pull *PullRec
	if valid {
		pull = repo.Pulls[number]
	}
	if pull == nil {
		ctx.Fail(404, "Pull Request not found")
		return
	}

	if len(segments) == 1 {
		if method == "GET" {
			ctx.Send(200, pullJson(repo, pull))
			return
		}
		ctx.NotFound()
		return
	}

	if len(segments) == 2 && segments[1] == "files" {
		if method != "GET" {
			ctx.NotFound()
			return
		}
		files := pullFiles(repo, pull)
		headSha := repo.TipOf(pull.HeadRef)
		if headSha == "" {
			headSha = ZeroSHA
		}
		views := make([]Json, len(files))
		for i, file := range files {
			views[i] = Json{
				"filename":  file.Filename,
				"status":    file.Status,
				"additions": file.Additions,
				"deletions": file.Deletions,
				"changes":   file.Changes,
				"sha":       headSha,
			}
		}
		ctx.Send(200, paginate(ctx, views))
		return
	}

	if len(segments) == 2 && segments[1] == "merge" {
		if method != "PUT" {
			ctx.NotFound()
			return
		}
		if pull.State != "open" {
			ctx.Fail(405, "Pull Request is not mergeable")
			return
		}
		sha := squashMerge(repo, pull)
		if sha == "" {
			ctx.Fail(405, "Pull Request is not mergeable: merge conflict")
			return
		}
		pull.State = "closed"
		pull.Merged = true
		now := isoNow()
		pull.MergedAt = &now
		pull.MergeCommitSha = &sha
		ctx.Send(200, Json{"sha": sha, "merged": true, "message": "Pull Request successfully merged"})
		return
	}

	ctx.NotFound()
}
