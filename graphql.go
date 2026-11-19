package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type gqlToken struct {
	kind  string
	value string
}

func gqlIsNameStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func gqlIsNameChar(c byte) bool {
	return gqlIsNameStart(c) || (c >= '0' && c <= '9')
}

func gqlIsDigit(c byte) bool { return c >= '0' && c <= '9' }

func gqlDecodeBase64(text string) string {
	encodings := []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	}
	for _, enc := range encodings {
		if data, err := enc.DecodeString(text); err == nil {
			return string(data)
		}
	}
	return ""
}

func gqlB64(text string) string {
	return base64.StdEncoding.EncodeToString([]byte(text))
}

func tokenizeGraphQL(source string) []gqlToken {
	tokens := make([]gqlToken, 0, 64)
	i := 0
	for i < len(source) {
		c := source[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			i++
			continue
		}
		if c == '#' {
			for i < len(source) && source[i] != '\n' {
				i++
			}
			continue
		}
		if strings.HasPrefix(source[i:], `"""`) {
			i += 3
			var text strings.Builder
			for i < len(source) && !strings.HasPrefix(source[i:], `"""`) {
				if strings.HasPrefix(source[i:], `\"""`) {
					text.WriteString(`"""`)
					i += 4
				} else {
					text.WriteByte(source[i])
					i++
				}
			}
			if strings.HasPrefix(source[i:], `"""`) {
				i += 3
			}
			tokens = append(tokens, gqlToken{kind: "string", value: text.String()})
			continue
		}
		if c == '"' {
			i++
			var text strings.Builder
			for i < len(source) && source[i] != '"' {
				if source[i] == '\\' {
					i++
					if i >= len(source) {
						break
					}
					esc := source[i]
					i++
					switch esc {
					case 'n':
						text.WriteByte('\n')
					case 't':
						text.WriteByte('\t')
					case 'r':
						text.WriteByte('\r')
					case 'b':
						text.WriteByte('\b')
					case 'f':
						text.WriteByte('\f')
					case 'u':
						if i+4 <= len(source) {
							if v, err := strconv.ParseUint(source[i:i+4], 16, 16); err == nil {
								text.WriteRune(rune(v))
								i += 4
							}
						}
					default:
						text.WriteByte(esc)
					}
				} else {
					text.WriteByte(source[i])
					i++
				}
			}
			if i < len(source) {
				i++
			}
			tokens = append(tokens, gqlToken{kind: "string", value: text.String()})
			continue
		}
		if gqlIsNameStart(c) {
			j := i + 1
			for j < len(source) && gqlIsNameChar(source[j]) {
				j++
			}
			tokens = append(tokens, gqlToken{kind: "name", value: source[i:j]})
			i = j
			continue
		}
		if gqlIsDigit(c) || (c == '-' && i+1 < len(source) && gqlIsDigit(source[i+1])) {
			j := i
			if source[j] == '-' {
				j++
			}
			for j < len(source) && gqlIsDigit(source[j]) {
				j++
			}
			if j+1 < len(source) && source[j] == '.' && gqlIsDigit(source[j+1]) {
				j++
				for j < len(source) && gqlIsDigit(source[j]) {
					j++
				}
			}
			if j < len(source) && (source[j] == 'e' || source[j] == 'E') {
				j++
				if j < len(source) && (source[j] == '+' || source[j] == '-') {
					j++
				}
				for j < len(source) && gqlIsDigit(source[j]) {
					j++
				}
			}
			tokens = append(tokens, gqlToken{kind: "number", value: source[i:j]})
			i = j
			continue
		}
		if strings.HasPrefix(source[i:], "...") {
			tokens = append(tokens, gqlToken{kind: "punct", value: "..."})
			i += 3
			continue
		}
		if strings.ContainsRune("$!:&{()[]@=|}", rune(c)) {
			tokens = append(tokens, gqlToken{kind: "punct", value: string(c)})
			i++
			continue
		}
		i++
	}
	tokens = append(tokens, gqlToken{kind: "eof", value: ""})
	return tokens
}

type gqlValue struct {
	kind   string
	value  string
	values []*gqlValue
	fields map[string]*gqlValue
}

type gqlArg struct {
	name  string
	value *gqlValue
}

type gqlSelection struct {
	kind           string
	alias          string
	name           string
	typeCondition  string
	args           []gqlArg
	selections     []*gqlSelection
}

type gqlVarDef struct {
	name         string
	typeName     string
	defaultValue *gqlValue
}

type gqlOperation struct {
	kind       string
	name       string
	vars       []gqlVarDef
	selections []*gqlSelection
}

type gqlFragment struct {
	name          string
	typeCondition string
	selections    []*gqlSelection
}

type gqlParser struct {
	tokens    []gqlToken
	pos       int
	fragments map[string]*gqlFragment
}

func newGqlParser(tokens []gqlToken) *gqlParser {
	return &gqlParser{tokens: tokens, fragments: make(map[string]*gqlFragment)}
}

func (p *gqlParser) peek() gqlToken {
	if p.pos < len(p.tokens) {
		return p.tokens[p.pos]
	}
	return gqlToken{kind: "eof", value: ""}
}

func (p *gqlParser) next() gqlToken {
	t := p.peek()
	if t.kind != "eof" {
		p.pos++
	}
	return t
}

func (p *gqlParser) eat(value string) bool {
	t := p.peek()
	if t.kind == "punct" && t.value == value {
		p.pos++
		return true
	}
	return false
}

func (p *gqlParser) expect(value string) error {
	if !p.eat(value) {
		return fmt.Errorf("Expected %q in GraphQL document", value)
	}
	return nil
}

func (p *gqlParser) skipName(value string) bool {
	t := p.peek()
	if t.kind == "name" && t.value == value {
		p.pos++
		return true
	}
	return false
}

func (p *gqlParser) expectName() (string, error) {
	t := p.next()
	if t.kind != "name" {
		return "", fmt.Errorf("Expected a name in GraphQL document, got %q", t.value)
	}
	return t.value, nil
}

func (p *gqlParser) parseDocument() ([]*gqlOperation, error) {
	ops := make([]*gqlOperation, 0, 2)
	for p.peek().kind != "eof" {
		if p.eat("{") {
			selections, err := p.parseSelectionList()
			if err != nil {
				return nil, err
			}
			ops = append(ops, &gqlOperation{kind: "query", selections: selections})
			continue
		}
		if p.peek().kind == "name" && p.peek().value == "fragment" {
			if err := p.parseFragmentDefinition(); err != nil {
				return nil, err
			}
			continue
		}
		kind := "query"
		t := p.peek()
		if t.kind == "name" && (t.value == "query" || t.value == "mutation") {
			p.next()
			kind = t.value
		}
		name := ""
		if p.peek().kind == "name" {
			name = p.next().value
		}
		vars, err := p.parseVariableDefinitions()
		if err != nil {
			return nil, err
		}
		if err := p.skipDirectives(); err != nil {
			return nil, err
		}
		selections := make([]*gqlSelection, 0)
		if p.eat("{") {
			selections, err = p.parseSelectionList()
			if err != nil {
				return nil, err
			}
		} else if p.peek().kind == "name" {
			selections, err = p.parseSelectionList()
			if err != nil {
				return nil, err
			}
		}
		ops = append(ops, &gqlOperation{kind: kind, name: name, vars: vars, selections: selections})
	}
	return ops, nil
}

func (p *gqlParser) parseFragmentDefinition() error {
	if _, err := p.expectName(); err != nil {
		return err
	}
	name, err := p.expectName()
	if err != nil {
		return err
	}
	if !p.skipName("on") {
		return fmt.Errorf("Expected \"on\" in GraphQL fragment definition")
	}
	condition, err := p.expectName()
	if err != nil {
		return err
	}
	if err := p.skipDirectives(); err != nil {
		return err
	}
	if err := p.expect("{"); err != nil {
		return err
	}
	selections, err := p.parseSelectionList()
	if err != nil {
		return err
	}
	p.fragments[name] = &gqlFragment{name: name, typeCondition: condition, selections: selections}
	return nil
}

func (p *gqlParser) parseVariableDefinitions() ([]gqlVarDef, error) {
	defs := make([]gqlVarDef, 0)
	if !p.eat("(") {
		return defs, nil
	}
	for p.peek().kind != "eof" && !p.eat(")") {
		if !p.eat("$") {
			p.next()
			continue
		}
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		if err := p.expect(":"); err != nil {
			return nil, err
		}
		var typeName strings.Builder
		for {
			t := p.peek()
			if !(t.kind == "name" || (t.kind == "punct" && (t.value == "[" || t.value == "]" || t.value == "!"))) {
				break
			}
			typeName.WriteString(p.next().value)
		}
		var defaultValue *gqlValue
		if p.eat("=") {
			defaultValue, err = p.parseValue()
			if err != nil {
				return nil, err
			}
		}
		defs = append(defs, gqlVarDef{name: name, typeName: typeName.String(), defaultValue: defaultValue})
	}
	return defs, nil
}

func (p *gqlParser) skipDirectives() error {
	for p.eat("@") {
		if _, err := p.expectName(); err != nil {
			return err
		}
		if p.eat("(") {
			for p.peek().kind != "eof" && !p.eat(")") {
				if _, err := p.expectName(); err != nil {
					return err
				}
				if err := p.expect(":"); err != nil {
					return err
				}
				if _, err := p.parseValue(); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (p *gqlParser) parseArguments() ([]gqlArg, error) {
	args := make([]gqlArg, 0)
	if !p.eat("(") {
		return args, nil
	}
	for p.peek().kind != "eof" && !p.eat(")") {
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		if err := p.expect(":"); err != nil {
			return nil, err
		}
		value, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		args = append(args, gqlArg{name: name, value: value})
	}
	return args, nil
}

func (p *gqlParser) parseSelectionList() ([]*gqlSelection, error) {
	list := make([]*gqlSelection, 0)
	for p.peek().kind != "eof" {
		t := p.peek()
		if t.kind == "punct" && t.value == "}" {
			p.pos++
			break
		}
		sel, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		list = append(list, sel)
	}
	return list, nil
}

func (p *gqlParser) parseSelection() (*gqlSelection, error) {
	if p.eat("...") {
		if err := p.skipDirectives(); err != nil {
			return nil, err
		}
		if p.skipName("on") {
			condition, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if err := p.skipDirectives(); err != nil {
				return nil, err
			}
			selections := make([]*gqlSelection, 0)
			if p.eat("{") {
				selections, err = p.parseSelectionList()
				if err != nil {
					return nil, err
				}
			}
			return &gqlSelection{kind: "InlineFragment", typeCondition: condition, selections: selections}, nil
		}
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		if err := p.skipDirectives(); err != nil {
			return nil, err
		}
		return &gqlSelection{kind: "FragmentSpread", name: name}, nil
	}
	name, err := p.expectName()
	if err != nil {
		return nil, err
	}
	alias := ""
	if p.eat(":") {
		alias = name
		name, err = p.expectName()
		if err != nil {
			return nil, err
		}
	}
	args, err := p.parseArguments()
	if err != nil {
		return nil, err
	}
	if err := p.skipDirectives(); err != nil {
		return nil, err
	}
	selections := make([]*gqlSelection, 0)
	if p.eat("{") {
		selections, err = p.parseSelectionList()
		if err != nil {
			return nil, err
		}
	}
	return &gqlSelection{kind: "Field", alias: alias, name: name, args: args, selections: selections}, nil
}

func (p *gqlParser) parseValue() (*gqlValue, error) {
	t := p.peek()
	if t.kind == "punct" && t.value == "$" {
		p.pos++
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		return &gqlValue{kind: "variable", value: name}, nil
	}
	if t.kind == "string" {
		p.pos++
		return &gqlValue{kind: "literal", value: t.value}, nil
	}
	if t.kind == "number" {
		p.pos++
		if n, err := strconv.ParseFloat(t.value, 64); err == nil {
			return &gqlValue{kind: "literal", value: strconv.FormatFloat(n, 'g', -1, 64)}, nil
		}
		return &gqlValue{kind: "literal", value: t.value}, nil
	}
	if t.kind == "name" {
		p.pos++
		switch t.value {
		case "true", "false":
			return &gqlValue{kind: "literal", value: t.value}, nil
		case "null":
			return &gqlValue{kind: "literal", value: "null"}, nil
		default:
			return &gqlValue{kind: "enum", value: t.value}, nil
		}
	}
	if t.kind == "punct" && t.value == "[" {
		p.pos++
		values := make([]*gqlValue, 0)
		for p.peek().kind != "eof" {
			if p.peek().kind == "punct" && p.peek().value == "]" {
				p.pos++
				break
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			values = append(values, v)
		}
		return &gqlValue{kind: "list", values: values}, nil
	}
	if t.kind == "punct" && t.value == "{" {
		p.pos++
		fields := make(map[string]*gqlValue)
		for p.peek().kind != "eof" {
			cur := p.peek()
			if cur.kind == "punct" && cur.value == "}" {
				p.pos++
				break
			}
			key, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if err := p.expect(":"); err != nil {
				return nil, err
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			fields[key] = v
		}
		return &gqlValue{kind: "object", fields: fields}, nil
	}
	return nil, fmt.Errorf("Unexpected token %q in GraphQL document", t.value)
}

func gqlTyped(typeName string, props map[string]any) map[string]any {
	out := make(map[string]any, len(props)+1)
	out["__t"] = typeName
	for k, v := range props {
		out[k] = v
	}
	return out
}

var gqlTypeFields = map[string][]string{
	"Query":                     {"viewer", "repository", "node", "__type"},
	"Mutation":                  {"createIssue", "closeIssue", "reopenIssue", "createPullRequest", "mergePullRequest", "addComment"},
	"User":                      {"login"},
	"Ref":                       {"name", "target"},
	"Commit":                    {"oid"},
	"Repository":                {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "isPrivate", "description", "createdAt", "issue", "issues", "pullRequests", "pullRequest", "url"},
	"IssueConnection":           {"nodes", "totalCount", "pageInfo"},
	"PageInfo":                  {"hasNextPage", "endCursor"},
	"Issue":                     {"id", "number", "title", "body", "state", "createdAt", "updatedAt", "closedAt", "author", "labels", "comments"},
	"IssueLabelConnection":      {"nodes"},
	"IssueLabel":                {"name"},
	"IssueCommentConnection":    {"nodes"},
	"IssueComment":              {"id", "body", "createdAt", "author"},
	"IssueCommentEdge":          {"node"},
	"PullRequestConnection":     {"nodes", "totalCount", "pageInfo"},
	"PullRequest":               {"id", "number", "title", "body", "state", "merged", "mergeable", "headRefName", "baseRefName", "mergedAt", "createdAt", "files"},
	"PullRequestFileConnection": {"nodes"},
	"PullRequestFile":           {"path", "additions", "deletions", "changeType"},
	"CreateIssuePayload":        {"issue"},
	"CloseIssuePayload":         {"issue"},
	"ReopenIssuePayload":        {"issue"},
	"CreatePullRequestPayload":  {"pullRequest"},
	"MergePullRequestPayload":   {"pullRequest"},
	"AddCommentPayload":         {"commentEdge"},
	"__Type":                    {"name", "kind", "fields", "enumValues", "ofType"},
	"__Field":                   {"name", "type"},
	"__EnumValue":               {"name"},
}

var gqlEnumValues = map[string][]string{
	"SearchType": {"ISSUE", "REPOSITORY", "USER", "DISCUSSION"},
}

type gqlError struct {
	Message string `json:"message"`
	Path    []any  `json:"path,omitempty"`
}

type gqlCtx struct {
	store      *Store
	variables  map[string]any
	operation  *gqlOperation
	fragments  map[string]*gqlFragment
	errors     []gqlError
	inFragment map[string]bool
}

func (c *gqlCtx) addError(message string, path []any) {
	c.errors = append(c.errors, gqlError{Message: message, Path: path})
}

func (c *gqlCtx) errorsValue() []gqlError {
	out := make([]gqlError, 0, len(c.errors))
	seen := make(map[string]bool)
	for _, e := range c.errors {
		key := e.Message + "\x00" + fmt.Sprint(e.Path)
		if !seen[key] {
			seen[key] = true
			out = append(out, e)
		}
	}
	return out
}

func (c *gqlCtx) coerce(v *gqlValue) any {
	if v == nil {
		return nil
	}
	switch v.kind {
	case "variable":
		if value, ok := c.variables[v.value]; ok {
			return value
		}
		for i := range c.operation.vars {
			if c.operation.vars[i].name == v.value && c.operation.vars[i].defaultValue != nil {
				return c.coerce(c.operation.vars[i].defaultValue)
			}
		}
		return nil
	case "list":
		out := make([]any, 0, len(v.values))
		for _, item := range v.values {
			out = append(out, c.coerce(item))
		}
		return out
	case "object":
		out := make(map[string]any, len(v.fields))
		for key, item := range v.fields {
			out[key] = c.coerce(item)
		}
		return out
	case "literal":
		if v.value == "true" {
			return true
		}
		if v.value == "false" {
			return false
		}
		if v.value == "null" {
			return nil
		}
		if n, err := strconv.ParseFloat(v.value, 64); err == nil {
			return n
		}
		return v.value
	default:
		return v.value
	}
}

func gqlString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func gqlInt(v any) int {
	switch n := v.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0
		}
		return int(math.Trunc(n))
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i)
		}
	case string:
		if i, err := strconv.Atoi(n); err == nil {
			return i
		}
		if f, err := strconv.ParseFloat(n, 64); err == nil {
			return int(math.Trunc(f))
		}
	}
	return 0
}

func gqlMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func gqlStateList(raw any) []string {
	out := make([]string, 0)
	switch v := raw.(type) {
	case []any:
		for _, item := range v {
			out = append(out, strings.ToUpper(gqlString(item)))
		}
	case string:
		out = append(out, strings.ToUpper(v))
	}
	return out
}

func gqlFirstLimit(raw any) int {
	if raw == nil {
		return 100
	}
	var n float64
	switch v := raw.(type) {
	case float64:
		n = v
	case int:
		n = float64(v)
	case string:
		parsed, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 100
		}
		n = parsed
	default:
		return 100
	}
	if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
		return 100
	}
	n = math.Floor(n)
	if n > 100 {
		n = 100
	}
	return int(n)
}

var gqlCursorPattern = regexp.MustCompile(`cursor:(\d+)`)

func gqlCursorOffset(raw any) int {
	text, ok := raw.(string)
	if !ok || text == "" {
		return 0
	}
	decoded := gqlDecodeBase64(text)
	match := gqlCursorPattern.FindStringSubmatch(decoded)
	if len(match) < 2 {
		return 0
	}
	n, err := strconv.Atoi(match[1])
	if err != nil {
		return 0
	}
	return n
}

func gqlPageInfo(end, total, count int) map[string]any {
	var cursor any
	if count > 0 {
		cursor = gqlB64("cursor:" + itoa(end))
	}
	return gqlTyped("PageInfo", map[string]any{
		"hasNextPage": end < total,
		"endCursor":   cursor,
	})
}

func gqlID(typeName string, parts ...any) string {
	strs := make([]string, 0, len(parts))
	for _, p := range parts {
		switch v := p.(type) {
		case string:
			strs = append(strs, v)
		case int:
			strs = append(strs, itoa(v))
		default:
			strs = append(strs, fmt.Sprint(v))
		}
	}
	return gqlB64(typeName + ":" + strings.Join(strs, ":"))
}

type gqlGid struct {
	Type  string
	Parts []string
}

func gqlDecodeGid(raw any) *gqlGid {
	text, ok := raw.(string)
	if !ok || text == "" {
		return nil
	}
	decoded := gqlDecodeBase64(text)
	if decoded == "" {
		return nil
	}
	parts := strings.Split(decoded, ":")
	if len(parts) == 0 || parts[0] == "" || !gqlIsNameStart(parts[0][0]) {
		return nil
	}
	return &gqlGid{Type: parts[0], Parts: parts[1:]}
}

func gqlRepoValue(repo *Repo) map[string]any {
	return gqlTyped("Repository", map[string]any{"repo": repo})
}

func gqlIssueValue(repo *Repo, issue *Issue) map[string]any {
	return gqlTyped("Issue", map[string]any{"repo": repo, "issue": issue})
}

func gqlPullValue(repo *Repo, pull *Pull) map[string]any {
	return gqlTyped("PullRequest", map[string]any{"repo": repo, "pull": pull})
}

func gqlCommentValue(comment *Comment) map[string]any {
	return gqlTyped("IssueComment", map[string]any{"comment": comment})
}

func (c *gqlCtx) repoByID(raw any) *Repo {
	id := gqlInt(raw)
	if id <= 0 {
		if text, ok := raw.(string); ok {
			text = strings.TrimPrefix(text, "repo:")
			id = gqlInt(text)
		}
	}
	if id <= 0 {
		return nil
	}
	keys := make([]string, 0, len(c.store.Repos))
	for key := range c.store.Repos {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if c.store.Repos[key].ID == id {
			return c.store.Repos[key]
		}
	}
	return nil
}

func (c *gqlCtx) repoFromID(raw any) (*Repo, error) {
	text := gqlString(raw)
	if text != "" {
		if parts := strings.Split(text, "/"); len(parts) >= 2 {
			if repo := c.store.GetRepo(parts[0], parts[1]); repo != nil {
				return repo, nil
			}
		}
		if repo := c.repoByID(text); repo != nil {
			return repo, nil
		}
		if d := gqlDecodeGid(text); d != nil && len(d.Parts) > 0 {
			if d.Type == "Repository" || d.Type == "repo" {
				if repo := c.repoByID(d.Parts[0]); repo != nil {
					return repo, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("Repository not found")
}

func (c *gqlCtx) issueByGid(raw any) (*Repo, *Issue) {
	d := gqlDecodeGid(raw)
	if d == nil || d.Type != "Issue" || len(d.Parts) < 2 {
		return nil, nil
	}
	repo := c.repoByID(d.Parts[0])
	if repo == nil {
		return nil, nil
	}
	return repo, repo.FindIssue(gqlInt(d.Parts[1]))
}

func (c *gqlCtx) pullByGid(raw any) (*Repo, *Pull) {
	d := gqlDecodeGid(raw)
	if d == nil || d.Type != "PullRequest" || len(d.Parts) < 2 {
		return nil, nil
	}
	repo := c.repoByID(d.Parts[0])
	if repo == nil {
		return nil, nil
	}
	return repo, repo.FindPull(gqlInt(d.Parts[1]))
}

func (c *gqlCtx) commentByGid(raw any) *Comment {
	d := gqlDecodeGid(raw)
	if d == nil || d.Type != "IssueComment" || len(d.Parts) < 1 {
		return nil
	}
	id := gqlInt(d.Parts[0])
	keys := make([]string, 0, len(c.store.Repos))
	for key := range c.store.Repos {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		repo := c.store.Repos[key]
		for i := range repo.Issues {
			for j := range repo.Issues[i].Comments {
				if repo.Issues[i].Comments[j].ID == id {
					return &repo.Issues[i].Comments[j]
				}
			}
		}
	}
	return nil
}

func (c *gqlCtx) nodeValue(raw any) any {
	if d := gqlDecodeGid(raw); d != nil {
		switch d.Type {
		case "Repository":
			if len(d.Parts) > 0 {
				if repo := c.repoByID(d.Parts[0]); repo != nil {
					return gqlRepoValue(repo)
				}
			}
		case "Issue":
			if repo, issue := c.issueByGid(raw); issue != nil {
				return gqlIssueValue(repo, issue)
			}
		case "PullRequest":
			if repo, pull := c.pullByGid(raw); pull != nil {
				return gqlPullValue(repo, pull)
			}
		case "IssueComment":
			if comment := c.commentByGid(raw); comment != nil {
				return gqlCommentValue(comment)
			}
		}
	}
	return nil
}

func gqlTypeValue(raw any) any {
	name := gqlString(raw)
	switch name {
	case "Issue", "PullRequest", "Repository":
		fields := make([]any, 0, len(gqlTypeFields[name]))
		for _, f := range gqlTypeFields[name] {
			fields = append(fields, f)
		}
		return gqlTyped("__Type", map[string]any{
			"name":       name,
			"kind":       "OBJECT",
			"fieldsList": fields,
			"enumList":   []any{},
		})
	case "SearchType":
		values := make([]any, 0, len(gqlEnumValues[name]))
		for _, v := range gqlEnumValues[name] {
			values = append(values, v)
		}
		return gqlTyped("__Type", map[string]any{
			"name":       name,
			"kind":       "ENUM",
			"fieldsList": []any{},
			"enumList":   values,
		})
	}
	return nil
}

func gqlTypeDesc(kind, name string, ofType any) map[string]any {
	var n any
	if name != "" {
		n = name
	}
	return gqlTyped("__Type", map[string]any{"kind": kind, "name": n, "ofType": ofType})
}

func gqlObjectListDesc(name string) map[string]any {
	return gqlTypeDesc("LIST", "", gqlTypeDesc("OBJECT", name, nil))
}

func gqlFieldType(ownerType, fieldName string) map[string]any {
	if ownerType == "__Type" {
		switch fieldName {
		case "name", "kind":
			return gqlTypeDesc("SCALAR", "String", nil)
		case "fields":
			return gqlObjectListDesc("__Field")
		case "enumValues":
			return gqlObjectListDesc("__EnumValue")
		case "ofType":
			return gqlTypeDesc("OBJECT", "__Type", nil)
		}
	}
	if ownerType == "__Field" {
		if fieldName == "type" {
			return gqlTypeDesc("OBJECT", "__Type", nil)
		}
		return gqlTypeDesc("SCALAR", "String", nil)
	}
	if ownerType == "__EnumValue" {
		return gqlTypeDesc("SCALAR", "String", nil)
	}
	if fieldName == "nodes" {
		inner := strings.TrimSuffix(ownerType, "Connection")
		return gqlObjectListDesc(inner)
	}
	switch fieldName {
	case "id":
		return gqlTypeDesc("SCALAR", "ID", nil)
	case "databaseId", "number", "additions", "deletions", "totalCount":
		return gqlTypeDesc("SCALAR", "Int", nil)
	case "merged", "isPrivate", "hasIssuesEnabled", "hasNextPage":
		return gqlTypeDesc("SCALAR", "Boolean", nil)
	case "mergeable":
		return gqlTypeDesc("ENUM", "MergeableState", nil)
	case "state":
		if ownerType == "PullRequest" {
			return gqlTypeDesc("ENUM", "PullRequestState", nil)
		}
		return gqlTypeDesc("ENUM", "IssueState", nil)
	case "viewerPermission":
		return gqlTypeDesc("ENUM", "RepositoryPermission", nil)
	case "changeType":
		return gqlTypeDesc("ENUM", "PullRequestFileChangeType", nil)
	case "title", "body", "name", "nameWithOwner", "createdAt", "updatedAt", "closedAt", "mergedAt",
		"login", "path", "headRefName", "baseRefName", "endCursor", "description", "url":
		return gqlTypeDesc("SCALAR", "String", nil)
	case "oid":
		return gqlTypeDesc("SCALAR", "GitObjectID", nil)
	case "owner":
		return gqlTypeDesc("OBJECT", "User", nil)
	case "author":
		return gqlTypeDesc("OBJECT", "User", nil)
	case "defaultBranchRef":
		return gqlTypeDesc("OBJECT", "Ref", nil)
	case "target":
		return gqlTypeDesc("OBJECT", "Commit", nil)
	case "issue":
		return gqlTypeDesc("OBJECT", "Issue", nil)
	case "issues":
		return gqlTypeDesc("OBJECT", "IssueConnection", nil)
	case "pullRequest":
		return gqlTypeDesc("OBJECT", "PullRequest", nil)
	case "pullRequests":
		return gqlTypeDesc("OBJECT", "PullRequestConnection", nil)
	case "files":
		return gqlTypeDesc("OBJECT", "PullRequestFileConnection", nil)
	case "labels":
		return gqlTypeDesc("OBJECT", "IssueLabelConnection", nil)
	case "comments":
		return gqlTypeDesc("OBJECT", "IssueCommentConnection", nil)
	case "pageInfo":
		return gqlTypeDesc("OBJECT", "PageInfo", nil)
	case "node":
		return gqlTypeDesc("OBJECT", "IssueComment", nil)
	case "commentEdge":
		return gqlTypeDesc("OBJECT", "IssueCommentEdge", nil)
	case "__type":
		return gqlTypeDesc("OBJECT", "__Type", nil)
	}
	return gqlTypeDesc("SCALAR", "String", nil)
}

func (c *gqlCtx) resolveLabels(repo *Repo, raw any) []*Label {
	ids, ok := raw.([]any)
	if !ok {
		return []*Label{}
	}
	out := make([]*Label, 0)
	seen := make(map[int]bool)
	for _, item := range ids {
		text := gqlString(item)
		key := text
		if d := gqlDecodeGid(text); d != nil && len(d.Parts) > 0 {
			key = d.Parts[len(d.Parts)-1]
		}
		var label *Label
		if found, ok := repo.Labels[key]; ok {
			label = &found
		} else {
			for _, found := range repo.Labels {
				if itoa(found.ID) == key {
					copied := found
					label = &copied
					break
				}
			}
		}
		if label != nil && !seen[label.ID] {
			seen[label.ID] = true
			out = append(out, label)
		}
	}
	return out
}

func (c *gqlCtx) createIssueMutation(input map[string]any) (any, error) {
	repo, err := c.repoFromID(input["repositoryId"])
	if err != nil {
		return nil, err
	}
	title := gqlString(input["title"])
	if title == "" {
		return nil, fmt.Errorf("Title is required")
	}
	ts := c.store.NowISO()
	repo.IssueSeq++
	c.store.NextIssueID++
	issue := Issue{
		ID:          c.store.NextIssueID - 1,
		Number:      repo.IssueSeq,
		Title:       title,
		Body:        gqlString(input["body"]),
		State:       "open",
		StateReason: nil,
		Labels:      make([]Label, 0),
		CreatedAt:   ts,
		UpdatedAt:   ts,
		ClosedAt:    nil,
		Comments:    []Comment{},
		Version:     0,
	}
	for _, label := range c.resolveLabels(repo, input["labelIds"]) {
		issue.Labels = append(issue.Labels, *label)
	}
	repo.Issues = append(repo.Issues, issue)
	added := &repo.Issues[len(repo.Issues)-1]
	added.Version++
	return gqlTyped("CreateIssuePayload", map[string]any{"issue": gqlIssueValue(repo, added)}), nil
}

func (c *gqlCtx) changeIssueStateMutation(raw any, state string) (any, error) {
	repo, issue := c.issueByGid(raw)
	if issue == nil {
		return nil, fmt.Errorf("Issue not found")
	}
	ts := c.store.NowISO()
	issue.State = state
	if state == "closed" {
		issue.ClosedAt = &ts
	} else {
		issue.ClosedAt = nil
	}
	issue.UpdatedAt = ts
	typeName := "ReopenIssuePayload"
	if state == "closed" {
		typeName = "CloseIssuePayload"
	}
	return gqlTyped(typeName, map[string]any{"issue": gqlIssueValue(repo, issue)}), nil
}

func (c *gqlCtx) createPullRequestMutation(input map[string]any) (any, error) {
	repo, err := c.repoFromID(input["repositoryId"])
	if err != nil {
		return nil, err
	}
	headRaw := gqlString(input["headRefName"])
	baseRef := gqlString(input["baseRefName"])
	headOwner := repo.Owner
	headRef := headRaw
	if colon := strings.IndexByte(headRaw, ':'); colon >= 0 {
		headOwner = headRaw[:colon]
		headRef = headRaw[colon+1:]
	}
	headSha := repo.Refs["refs/heads/"+headRef]
	baseSha := repo.Refs["refs/heads/"+baseRef]
	if headSha == "" || baseSha == "" {
		return nil, fmt.Errorf("Head or base ref does not exist")
	}
	for i := range repo.Pulls {
		existing := &repo.Pulls[i]
		if existing.State == "open" && existing.HeadRef == headRef && existing.BaseRef == baseRef {
			return gqlTyped("CreatePullRequestPayload", map[string]any{
				"pullRequest": gqlPullValue(repo, existing),
			}), nil
		}
	}
	draft, _ := input["draft"].(bool)
	ts := c.store.NowISO()
	repo.PullSeq++
	c.store.NextPullID++
	pull := Pull{
		ID:        c.store.NextPullID - 1,
		Number:    repo.PullSeq,
		Title:     gqlString(input["title"]),
		Body:      gqlString(input["body"]),
		State:     "open",
		HeadOwner: headOwner,
		HeadRef:   headRef,
		HeadSha:   headSha,
		BaseOwner: repo.Owner,
		BaseRef:   baseRef,
		BaseSha:   baseSha,
		Merged:    false,
		MergedAt:  nil,
		Draft:     draft,
		CreatedAt: ts,
		UpdatedAt: ts,
		ClosedAt:  nil,
	}
	repo.Pulls = append(repo.Pulls, pull)
	return gqlTyped("CreatePullRequestPayload", map[string]any{
		"pullRequest": gqlPullValue(repo, &repo.Pulls[len(repo.Pulls)-1]),
	}), nil
}

func (c *gqlCtx) mergePullRequestMutation(input map[string]any) (any, error) {
	repo, pull := c.pullByGid(input["pullRequestId"])
	if pull == nil {
		return nil, fmt.Errorf("Pull request not found")
	}
	if !pull.Merged {
		result := MergePull(c.store, repo, pull, "SQUASH")
		if !result.OK {
			return nil, fmt.Errorf("Pull request is not mergeable")
		}
	}
	return gqlTyped("MergePullRequestPayload", map[string]any{
		"pullRequest": gqlPullValue(repo, pull),
	}), nil
}

func (c *gqlCtx) addCommentMutation(input map[string]any) (any, error) {
	repo, issue := c.issueByGid(input["subjectId"])
	if issue == nil {
		return nil, fmt.Errorf("Subject not found")
	}
	_ = repo
	ts := c.store.NowISO()
	comment := Comment{ID: c.store.NextCommentID, Body: gqlString(input["body"]), CreatedAt: ts, UpdatedAt: ts}
	c.store.NextCommentID++
	issue.Comments = append(issue.Comments, comment)
	issue.UpdatedAt = ts
	return gqlTyped("AddCommentPayload", map[string]any{
		"commentEdge": gqlTyped("IssueCommentEdge", map[string]any{
			"node": gqlCommentValue(&issue.Comments[len(issue.Comments)-1]),
		}),
	}), nil
}

func (c *gqlCtx) issuesField(repo *Repo, args map[string]any) any {
	list := make([]*Issue, 0, len(repo.Issues))
	states := gqlStateList(args["states"])
	for i := range repo.Issues {
		issue := &repo.Issues[i]
		if len(states) > 0 {
			matched := false
			for _, state := range states {
				if state == strings.ToUpper(issue.State) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		list = append(list, issue)
	}
	direction := "DESC"
	orderBy := gqlMap(args["orderBy"])
	if orderBy != nil {
		if d := strings.ToUpper(gqlString(orderBy["direction"])); d != "" {
			direction = d
		}
	}
	ascending := direction == "ASC"
	sort.SliceStable(list, func(i, j int) bool {
		if ascending {
			if list[i].CreatedAt != list[j].CreatedAt {
				return list[i].CreatedAt < list[j].CreatedAt
			}
			return list[i].Number < list[j].Number
		}
		if list[i].CreatedAt != list[j].CreatedAt {
			return list[i].CreatedAt > list[j].CreatedAt
		}
		return list[i].Number > list[j].Number
	})
	total := len(list)
	offset := gqlCursorOffset(args["after"])
	first := gqlFirstLimit(args["first"])
	if offset > total {
		offset = total
	}
	end := total
	if offset+first < total {
		end = offset + first
	}
	nodes := make([]any, 0, end-offset)
	for _, issue := range list[offset:end] {
		nodes = append(nodes, gqlIssueValue(repo, issue))
	}
	return gqlTyped("IssueConnection", map[string]any{
		"nodes":      nodes,
		"totalCount": total,
		"pageInfo":   gqlPageInfo(offset+(end-offset), total, end-offset),
	})
}

func (c *gqlCtx) pullRequestsField(repo *Repo, args map[string]any) any {
	list := make([]*Pull, 0, len(repo.Pulls))
	states := gqlStateList(args["states"])
	for i := range repo.Pulls {
		pull := &repo.Pulls[i]
		if len(states) > 0 {
			matched := false
			for _, state := range states {
				if state == strings.ToUpper(pull.State) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		list = append(list, pull)
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].Number > list[j].Number })
	total := len(list)
	offset := gqlCursorOffset(args["after"])
	first := gqlFirstLimit(args["first"])
	if offset > total {
		offset = total
	}
	end := total
	if offset+first < total {
		end = offset + first
	}
	nodes := make([]any, 0, end-offset)
	for _, pull := range list[offset:end] {
		nodes = append(nodes, gqlPullValue(repo, pull))
	}
	return gqlTyped("PullRequestConnection", map[string]any{
		"nodes":      nodes,
		"totalCount": total,
		"pageInfo":   gqlPageInfo(offset+(end-offset), total, end-offset),
	})
}

func (c *gqlCtx) resolveField(typeName string, obj map[string]any, name string, args map[string]any) (any, error) {
	switch typeName {
	case "Query":
		switch name {
		case "viewer":
			return gqlTyped("User", map[string]any{"login": c.store.Login}), nil
		case "repository":
			repo := c.store.GetRepo(gqlString(args["owner"]), gqlString(args["name"]))
			if repo == nil {
				return nil, nil
			}
			return gqlRepoValue(repo), nil
		case "node":
			return c.nodeValue(args["id"]), nil
		case "__type":
			return gqlTypeValue(args["name"]), nil
		}
	case "Mutation":
		switch name {
		case "createIssue":
			return c.createIssueMutation(gqlMap(args["input"]))
		case "closeIssue":
			return c.changeIssueStateMutation(gqlMap(args["input"])["issueId"], "closed")
		case "reopenIssue":
			return c.changeIssueStateMutation(gqlMap(args["input"])["issueId"], "open")
		case "createPullRequest":
			return c.createPullRequestMutation(gqlMap(args["input"]))
		case "mergePullRequest":
			return c.mergePullRequestMutation(gqlMap(args["input"]))
		case "addComment":
			return c.addCommentMutation(gqlMap(args["input"]))
		}
	case "Repository":
		repo, _ := obj["repo"].(*Repo)
		if repo == nil {
			return nil, nil
		}
		switch name {
		case "id":
			return gqlID("Repository", repo.ID), nil
		case "databaseId":
			return repo.ID, nil
		case "name":
			return repo.Name, nil
		case "nameWithOwner":
			return repo.Owner + "/" + repo.Name, nil
		case "owner":
			return gqlTyped("User", map[string]any{"login": repo.Owner}), nil
		case "defaultBranchRef":
			branch := defaultBranch(repo)
			tip := repo.Refs["refs/heads/"+branch]
			if tip == "" {
				return nil, nil
			}
			return gqlTyped("Ref", map[string]any{
				"name":   branch,
				"target": gqlTyped("Commit", map[string]any{"oid": tip}),
			}), nil
		case "viewerPermission":
			return "ADMIN", nil
		case "hasIssuesEnabled":
			return true, nil
		case "isPrivate":
			return repo.Private, nil
		case "description":
			return repo.Description, nil
		case "createdAt":
			return repo.CreatedAt, nil
		case "url":
			return "https://github.com/" + repo.Owner + "/" + repo.Name, nil
		case "issue":
			issue := repo.FindIssue(gqlInt(args["number"]))
			if issue == nil {
				return nil, nil
			}
			return gqlIssueValue(repo, issue), nil
		case "pullRequest":
			pull := repo.FindPull(gqlInt(args["number"]))
			if pull == nil {
				return nil, nil
			}
			return gqlPullValue(repo, pull), nil
		case "issues":
			return c.issuesField(repo, args), nil
		case "pullRequests":
			return c.pullRequestsField(repo, args), nil
		}
	case "Issue":
		repo, _ := obj["repo"].(*Repo)
		issue, _ := obj["issue"].(*Issue)
		if issue == nil {
			return nil, nil
		}
		switch name {
		case "id":
			return gqlID("Issue", repo.ID, issue.Number), nil
		case "number":
			return issue.Number, nil
		case "title":
			return issue.Title, nil
		case "body":
			return issue.Body, nil
		case "state":
			return strings.ToUpper(issue.State), nil
		case "createdAt":
			return issue.CreatedAt, nil
		case "updatedAt":
			return issue.UpdatedAt, nil
		case "closedAt":
			return issue.ClosedAt, nil
		case "author":
			return gqlTyped("User", map[string]any{"login": c.store.Login}), nil
		case "labels":
			nodes := make([]any, 0)
			limit := gqlFirstLimit(args["first"])
			for i, label := range issue.Labels {
				if i >= limit {
					break
				}
				nodes = append(nodes, gqlTyped("IssueLabel", map[string]any{"name": label.Name}))
			}
			return gqlTyped("IssueLabelConnection", map[string]any{"nodes": nodes}), nil
		case "comments":
			nodes := make([]any, 0)
			limit := gqlFirstLimit(args["first"])
			for i := range issue.Comments {
				if i >= limit {
					break
				}
				nodes = append(nodes, gqlCommentValue(&issue.Comments[i]))
			}
			return gqlTyped("IssueCommentConnection", map[string]any{"nodes": nodes}), nil
		}
	case "PullRequest":
		repo, _ := obj["repo"].(*Repo)
		pull, _ := obj["pull"].(*Pull)
		if pull == nil {
			return nil, nil
		}
		switch name {
		case "id":
			return gqlID("PullRequest", repo.ID, pull.Number), nil
		case "number":
			return pull.Number, nil
		case "title":
			return pull.Title, nil
		case "body":
			return pull.Body, nil
		case "state":
			return strings.ToUpper(pull.State), nil
		case "merged":
			return pull.Merged, nil
		case "mergeable":
			plan := planMerge(repo, pull)
			if plan != nil && !plan.conflict {
				return "MERGEABLE", nil
			}
			return "CONFLICTING", nil
		case "headRefName":
			return pull.HeadRef, nil
		case "baseRefName":
			return pull.BaseRef, nil
		case "mergedAt":
			return pull.MergedAt, nil
		case "createdAt":
			return pull.CreatedAt, nil
		case "files":
			nodes := make([]any, 0)
			limit := gqlFirstLimit(args["first"])
			for i, file := range PullChangedFiles(repo, pull) {
				if i >= limit {
					break
				}
				nodes = append(nodes, gqlTyped("PullRequestFile", map[string]any{
					"path":       file["filename"],
					"additions":  file["additions"],
					"deletions":  file["deletions"],
					"changeType": strings.ToUpper(gqlString(file["status"])),
				}))
			}
			return gqlTyped("PullRequestFileConnection", map[string]any{"nodes": nodes}), nil
		}
	case "IssueComment":
		comment, _ := obj["comment"].(*Comment)
		if comment == nil {
			return nil, nil
		}
		switch name {
		case "id":
			return gqlID("IssueComment", comment.ID), nil
		case "body":
			return comment.Body, nil
		case "createdAt":
			return comment.CreatedAt, nil
		case "author":
			return gqlTyped("User", map[string]any{"login": c.store.Login}), nil
		}
	case "__Type":
		switch name {
		case "name":
			return obj["name"], nil
		case "kind":
			return obj["kind"], nil
		case "fields":
			list := make([]any, 0)
			owner, _ := obj["name"].(string)
			if fields, ok := obj["fieldsList"].([]any); ok {
				for _, raw := range fields {
					fieldName := gqlString(raw)
					list = append(list, gqlTyped("__Field", map[string]any{
						"name":      fieldName,
						"type":      gqlFieldType(owner, fieldName),
						"ownerType": owner,
					}))
				}
			}
			return list, nil
		case "enumValues":
			list := make([]any, 0)
			if values, ok := obj["enumList"].([]any); ok {
				for _, raw := range values {
					list = append(list, gqlTyped("__EnumValue", map[string]any{"name": raw}))
				}
			}
			return list, nil
		case "ofType":
			return obj["ofType"], nil
		}
	case "__Field":
		if name == "name" {
			return obj["name"], nil
		}
		if name == "type" {
			return obj["type"], nil
		}
	case "__EnumValue":
		if name == "name" {
			return obj["name"], nil
		}
	}
	if value, ok := obj[name]; ok {
		return value, nil
	}
	return nil, nil
}

func gqlAppendPath(path []any, key string) []any {
	out := make([]any, len(path), len(path)+1)
	copy(out, path)
	return append(out, key)
}

func gqlFragmentMatches(condition, typeName string) bool {
	if condition == typeName {
		return true
	}
	if condition == "Node" {
		switch typeName {
		case "Repository", "Issue", "PullRequest", "IssueComment":
			return true
		}
	}
	return false
}

func (c *gqlCtx) executeSelectionSet(parent any, selections []*gqlSelection, path []any) any {
	obj, ok := parent.(map[string]any)
	if !ok || obj == nil {
		return nil
	}
	typeName := gqlString(obj["__t"])
	out := make(map[string]any)
	for _, sel := range selections {
		switch sel.kind {
		case "FragmentSpread":
			fragment := c.fragments[sel.name]
			if fragment == nil || c.inFragment[sel.name] || !gqlFragmentMatches(fragment.typeCondition, typeName) {
				continue
			}
			if c.inFragment == nil {
				c.inFragment = make(map[string]bool)
			}
			c.inFragment[sel.name] = true
			part := c.executeSelectionSet(parent, fragment.selections, path)
			delete(c.inFragment, sel.name)
			if m, ok := part.(map[string]any); ok {
				for k, v := range m {
					out[k] = v
				}
			}
			continue
		case "InlineFragment":
			if gqlFragmentMatches(sel.typeCondition, typeName) {
				part := c.executeSelectionSet(parent, sel.selections, path)
				if m, ok := part.(map[string]any); ok {
					for k, v := range m {
						out[k] = v
					}
				}
			}
			continue
		}
		key := sel.name
		if sel.alias != "" {
			key = sel.alias
		}
		fieldPath := gqlAppendPath(path, key)
		if sel.name == "__typename" {
			out[key] = typeName
			continue
		}
		allowed := gqlTypeFields[typeName]
		found := false
		for _, f := range allowed {
			if f == sel.name {
				found = true
				break
			}
		}
		if !found {
			c.addError(fmt.Sprintf("Cannot query field %q on type %q.", sel.name, typeName), fieldPath)
			continue
		}
		args := make(map[string]any)
		for _, arg := range sel.args {
			args[arg.name] = c.coerce(arg.value)
		}
		value, err := c.resolveField(typeName, obj, sel.name, args)
		if err != nil {
			c.addError(err.Error(), fieldPath)
			out[key] = nil
			continue
		}
		if len(sel.selections) > 0 && value != nil {
			if list, ok := value.([]any); ok {
				items := make([]any, 0, len(list))
				for _, item := range list {
					items = append(items, c.executeSelectionSet(item, sel.selections, fieldPath))
				}
				out[key] = items
			} else {
				out[key] = c.executeSelectionSet(value, sel.selections, fieldPath)
			}
		} else {
			out[key] = value
		}
	}
	return out
}

func (a *App) runGraphQLOperation(store *Store, op *gqlOperation, variables map[string]any, fragments map[string]*gqlFragment) map[string]any {
	ctx := &gqlCtx{
		store:      store,
		variables:  variables,
		operation:  op,
		fragments:  fragments,
		inFragment: make(map[string]bool),
	}
	rootType := "Query"
	if op.kind == "mutation" {
		rootType = "Mutation"
	}
	data := ctx.executeSelectionSet(gqlTyped(rootType, nil), op.selections, nil)
	result := map[string]any{"data": data}
	if errors := ctx.errorsValue(); len(errors) > 0 {
		result["errors"] = errors
	}
	return result
}

func gqlErrorResult(message string) map[string]any {
	return map[string]any{
		"data":   nil,
		"errors": []gqlError{{Message: message}},
	}
}

func (a *App) handleOneGraphQL(payload any) any {
	payloadMap, ok := payload.(map[string]any)
	if !ok {
		return gqlErrorResult("No operation found in GraphQL document")
	}
	source := gqlString(payloadMap["query"])
	parser := newGqlParser(tokenizeGraphQL(source))
	operations, err := parser.parseDocument()
	if err != nil {
		return gqlErrorResult(err.Error())
	}
	if len(operations) == 0 {
		return gqlErrorResult("No operation found in GraphQL document")
	}
	operation := operations[0]
	if operationName, ok := payloadMap["operationName"].(string); ok {
		selected := (*gqlOperation)(nil)
		for _, candidate := range operations {
			if candidate.name == operationName {
				selected = candidate
				break
			}
		}
		if selected == nil {
			return gqlErrorResult(fmt.Sprintf("Unknown operation %q", operationName))
		}
		operation = selected
	}
	variables, _ := payloadMap["variables"].(map[string]any)
	if variables == nil {
		variables = make(map[string]any)
	}
	return a.runGraphQLOperation(a.Store, operation, variables, parser.fragments)
}

func (a *App) handleGraphQL(w http.ResponseWriter, r *http.Request, body any) {
	var result any
	if items, ok := body.([]any); ok {
		results := make([]any, 0, len(items))
		for _, item := range items {
			results = append(results, a.handleOneGraphQL(item))
		}
		result = results
	} else {
		result = a.handleOneGraphQL(body)
	}
	a.sendJSON(w, http.StatusOK, encodeJSON(result))
}
