// Package githubtest provides an in-process fake of the GitHub REST endpoints
// Octomaton uses, for tests. It verifies App JWTs and installation tokens and
// records everything it is asked to do.
package githubtest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	keyOnce sync.Once
	key     *rsa.PrivateKey
)

// Key returns an RSA key shared by all tests in the process.
func Key() *rsa.PrivateKey {
	keyOnce.Do(func() {
		var err error
		if key, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
	})
	return key
}

// CheckRun is a check run stored by the fake.
type CheckRun struct {
	ID          int64
	SuiteID     int64
	Repo        string
	Name        string
	HeadSHA     string
	Status      string
	Conclusion  string
	ExternalID  string
	DetailsURL  string
	Title       string
	Summary     string
	Text        string
	StartedAt   string
	CompletedAt string
	Actions     []map[string]any
	// Updates counts PATCH requests received for this check run.
	Updates int
}

// PullRequest is a pull request served by the fake.
type PullRequest struct {
	Number            int
	State             string
	Draft             bool
	HeadRef           string
	HeadSHA           string
	HeadRepo          string
	BaseRef           string
	BaseSHA           string
	Author            string
	AuthorAssociation string
}

// Repository is an installation repository served by the fake.
type Repository struct {
	ID            int64
	Owner         string
	Name          string
	DefaultBranch string
	Archived      bool
}

// Reaction records a reaction added to a comment.
type Reaction struct {
	Repo      string
	CommentID int64
	Content   string
}

// IssueComment records a comment posted on an issue or pull request.
type IssueComment struct {
	Repo   string
	Number int
	Body   string
}

// TokenRequest records a POST /app/installations/{id}/access_tokens call.
type TokenRequest struct {
	InstallationID int64
	RepositoryIDs  []int64
	Permissions    map[string]string
}

// Server is a fake GitHub API.
type Server struct {
	*httptest.Server
	AppID int64

	mu sync.Mutex
	// Files maps "owner/repo@ref:path" to file content.
	Files map[string]string
	// PullRequestFiles maps "owner/repo#number" to changed file names.
	PullRequestFiles map[string][]string
	// Comparisons maps "owner/repo:base...head" to changed file names.
	Comparisons map[string][]string
	// Permissions maps "owner/repo:login" to a permission level.
	Permissions map[string]string
	failFiles   bool
	failTokens  func(TokenRequest) bool

	checkRuns     map[int64]*CheckRun
	suites        map[string]int64
	nextID        int64
	tokens        map[string]int64
	tokenRequests []TokenRequest
	requests      []string
	pullRequests  map[string]PullRequest
	branches      map[string]string
	installations map[int64]string
	instRepos     map[int64][]Repository
	reactions     []Reaction
	comments      []IssueComment
}

// NewServer starts a fake GitHub API for appID; it is closed when the test ends.
func NewServer(t testing.TB, appID int64) *Server {
	s := &Server{
		AppID:            appID,
		Files:            map[string]string{},
		PullRequestFiles: map[string][]string{},
		Comparisons:      map[string][]string{},
		Permissions:      map[string]string{},
		checkRuns:        map[int64]*CheckRun{},
		suites:           map[string]int64{},
		tokens:           map[string]int64{},
		nextID:           1000,
		pullRequests:     map[string]PullRequest{},
		branches:         map[string]string{},
		installations:    map[int64]string{},
		instRepos:        map[int64][]Repository{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", s.createToken)
	mux.HandleFunc("GET /repos/{owner}/{repo}/contents/{path...}", s.auth(s.getContents))
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}/files", s.auth(s.listPullRequestFiles))
	mux.HandleFunc("GET /repos/{owner}/{repo}/compare/{basehead}", s.auth(s.compare))
	mux.HandleFunc("POST /repos/{owner}/{repo}/check-runs", s.auth(s.createCheckRun))
	mux.HandleFunc("PATCH /repos/{owner}/{repo}/check-runs/{id}", s.auth(s.updateCheckRun))
	mux.HandleFunc("GET /repos/{owner}/{repo}/check-runs/{id}", s.auth(s.getCheckRun))
	mux.HandleFunc("GET /repos/{owner}/{repo}/check-suites/{id}/check-runs", s.auth(s.listSuiteCheckRuns))
	mux.HandleFunc("GET /repos/{owner}/{repo}/collaborators/{user}/permission", s.auth(s.permission))
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}", s.auth(s.getPullRequest))
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/ref/{ref...}", s.auth(s.getRef))
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{ref}/check-runs", s.auth(s.listRefCheckRuns))
	mux.HandleFunc("POST /repos/{owner}/{repo}/issues/comments/{id}/reactions", s.auth(s.createReaction))
	mux.HandleFunc("POST /repos/{owner}/{repo}/issues/{number}/comments", s.auth(s.createComment))
	mux.HandleFunc("GET /app/installations", s.listInstallations)
	mux.HandleFunc("GET /installation/repositories", s.auth(s.listInstallationRepos))
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests = append(s.requests, r.Method+" "+r.URL.Path)
		s.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

// AddFile stores a file of repo ("owner/name") at ref.
func (s *Server) AddFile(repo, ref, path, content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Files[repo+"@"+ref+":"+path] = content
}

// SetFailFiles makes content requests fail with HTTP 500.
func (s *Server) SetFailFiles(fail bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failFiles = fail
}

// SetFailTokens makes token requests for which fail returns true fail with HTTP 422.
func (s *Server) SetFailTokens(fail func(TokenRequest) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failTokens = fail
}

// SetPermission sets login's permission level on repo.
func (s *Server) SetPermission(repo, login, level string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Permissions[repo+":"+login] = level
}

// SetPullRequest stores a pull request of repo.
func (s *Server) SetPullRequest(repo string, pr PullRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pullRequests[fmt.Sprintf("%s#%d", repo, pr.Number)] = pr
}

// SetBranch points a branch of repo at sha ("" deletes it).
func (s *Server) SetBranch(repo, branch, sha string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sha == "" {
		delete(s.branches, repo+":"+branch)
		return
	}
	s.branches[repo+":"+branch] = sha
}

// AddInstallation registers an installation of the App on an account with its repositories.
func (s *Server) AddInstallation(id int64, account string, repos ...Repository) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.installations[id] = account
	s.instRepos[id] = append(s.instRepos[id], repos...)
}

// Reactions returns the reactions added to comments.
func (s *Server) Reactions() []Reaction {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Reaction(nil), s.reactions...)
}

// Comments returns the comments posted.
func (s *Server) Comments() []IssueComment {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]IssueComment(nil), s.comments...)
}

// SetPullRequestFiles sets the files changed by a pull request.
func (s *Server) SetPullRequestFiles(repo string, number int, files []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.PullRequestFiles[fmt.Sprintf("%s#%d", repo, number)] = files
}

// SetComparison sets the files changed between base and head.
func (s *Server) SetComparison(repo, base, head string, files []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Comparisons[repo+":"+base+"..."+head] = files
}

// AddCheckRun stores an existing check run and returns its ID.
func (s *Server) AddCheckRun(cr CheckRun) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	cr.ID = s.nextID
	cr.SuiteID = s.suiteLocked(cr.Repo, cr.HeadSHA)
	s.checkRuns[cr.ID] = &cr
	return cr.ID
}

// CheckRuns returns copies of all check runs, ordered by ID.
func (s *Server) CheckRuns() []CheckRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]CheckRun, 0, len(s.checkRuns))
	for _, cr := range s.checkRuns {
		out = append(out, *cr)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// CheckRun returns a copy of the check run with the given ID.
func (s *Server) CheckRun(id int64) (CheckRun, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cr, ok := s.checkRuns[id]
	if !ok {
		return CheckRun{}, false
	}
	return *cr, true
}

// SuiteID returns the check suite of repo's commit sha.
func (s *Server) SuiteID(repo, sha string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.suiteLocked(repo, sha)
}

// TokenRequests returns the installation token requests received.
func (s *Server) TokenRequests() []TokenRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]TokenRequest(nil), s.tokenRequests...)
}

// Requests returns "METHOD path" for every request received.
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

func (s *Server) suiteLocked(repo, sha string) int64 {
	k := repo + "@" + sha
	if id, ok := s.suites[k]; ok {
		return id
	}
	s.nextID++
	s.suites[k] = s.nextID
	return s.nextID
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func notFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
}

// verifyJWT checks an RS256 App JWT signed with Key() and issued for the App.
func (s *Server) verifyJWT(token string) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return fmt.Errorf("malformed JWT")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&Key().PublicKey, crypto.SHA256, digest[:], sig); err != nil {
		return fmt.Errorf("bad JWT signature: %w", err)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return err
	}
	var claims struct {
		Iss string `json:"iss"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return err
	}
	if claims.Iss != strconv.FormatInt(s.AppID, 10) {
		return fmt.Errorf("JWT issued for app %q", claims.Iss)
	}
	if time.Unix(claims.Exp, 0).Before(time.Now()) {
		return fmt.Errorf("JWT expired")
	}
	return nil
}

func (s *Server) appAuth(w http.ResponseWriter, r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") || s.verifyJWT(strings.TrimPrefix(auth, "Bearer ")) != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "A JSON web token could not be decoded"})
		return false
	}
	return true
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	if !s.appAuth(w, r) {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var body struct {
		RepositoryIDs []int64           `json:"repository_ids"`
		Permissions   map[string]string `json:"permissions"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	req := TokenRequest{InstallationID: id, RepositoryIDs: body.RepositoryIDs, Permissions: body.Permissions}
	if s.failTokens != nil && s.failTokens(req) {
		s.mu.Unlock()
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "The permissions requested are not granted to this installation."})
		return
	}
	s.tokenRequests = append(s.tokenRequests, req)
	token := fmt.Sprintf("ghs_test_%d_%d", id, len(s.tokenRequests))
	s.tokens[token] = id
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{
		"token":      token,
		"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	})
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "token ")
		s.mu.Lock()
		_, ok := s.tokens[token]
		s.mu.Unlock()
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
			return
		}
		next(w, r)
	}
}

func repoOf(r *http.Request) string { return r.PathValue("owner") + "/" + r.PathValue("repo") }

func (s *Server) getContents(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	fail := s.failFiles
	content, ok := s.Files[repoOf(r)+"@"+r.URL.Query().Get("ref")+":"+r.PathValue("path")]
	s.mu.Unlock()
	if fail {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": "Server Error"})
		return
	}
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"type":     "file",
		"encoding": "base64",
		"name":     r.PathValue("path"),
		"path":     r.PathValue("path"),
		"content":  base64.StdEncoding.EncodeToString([]byte(content)),
	})
}

func (s *Server) listPullRequestFiles(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	files, ok := s.PullRequestFiles[repoOf(r)+"#"+r.PathValue("number")]
	s.mu.Unlock()
	if !ok {
		notFound(w)
		return
	}
	perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	if perPage <= 0 {
		perPage = 30
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page <= 0 {
		page = 1
	}
	start := min((page-1)*perPage, len(files))
	end := min(start+perPage, len(files))
	if end < len(files) {
		next := *r.URL
		q := next.Query()
		q.Set("page", strconv.Itoa(page+1))
		next.RawQuery = q.Encode()
		w.Header().Set("Link", fmt.Sprintf(`<%s%s>; rel="next"`, s.URL, next.RequestURI()))
	}
	out := make([]map[string]any, 0, end-start)
	for _, f := range files[start:end] {
		out = append(out, map[string]any{"filename": f, "status": "modified"})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) compare(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	files, ok := s.Comparisons[repoOf(r)+":"+r.PathValue("basehead")]
	s.mu.Unlock()
	if !ok {
		notFound(w)
		return
	}
	out := make([]map[string]any, 0, len(files))
	for _, f := range files {
		out = append(out, map[string]any{"filename": f, "status": "modified"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ahead", "files": out})
}

type checkRunBody struct {
	Name        *string          `json:"name"`
	HeadSHA     *string          `json:"head_sha"`
	Status      *string          `json:"status"`
	Conclusion  *string          `json:"conclusion"`
	ExternalID  *string          `json:"external_id"`
	DetailsURL  *string          `json:"details_url"`
	StartedAt   *string          `json:"started_at"`
	CompletedAt *string          `json:"completed_at"`
	Actions     []map[string]any `json:"actions"`
	Output      *struct {
		Title   *string `json:"title"`
		Summary *string `json:"summary"`
		Text    *string `json:"text"`
	} `json:"output"`
}

func (b checkRunBody) apply(cr *CheckRun) {
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = *v
		}
	}
	set(&cr.Name, b.Name)
	set(&cr.HeadSHA, b.HeadSHA)
	set(&cr.Status, b.Status)
	set(&cr.Conclusion, b.Conclusion)
	set(&cr.ExternalID, b.ExternalID)
	set(&cr.DetailsURL, b.DetailsURL)
	set(&cr.StartedAt, b.StartedAt)
	set(&cr.CompletedAt, b.CompletedAt)
	if b.Conclusion != nil {
		cr.Status = "completed"
	}
	if b.Actions != nil {
		cr.Actions = b.Actions
	}
	if b.Output != nil {
		set(&cr.Title, b.Output.Title)
		set(&cr.Summary, b.Output.Summary)
		set(&cr.Text, b.Output.Text)
	}
}

func (s *Server) checkRunJSON(cr *CheckRun) map[string]any {
	out := map[string]any{
		"id":          cr.ID,
		"name":        cr.Name,
		"head_sha":    cr.HeadSHA,
		"status":      cr.Status,
		"external_id": cr.ExternalID,
		"details_url": cr.DetailsURL,
		"app":         map[string]any{"id": s.AppID},
		"check_suite": map[string]any{"id": cr.SuiteID},
		"output":      map[string]any{"title": cr.Title, "summary": cr.Summary, "text": cr.Text},
	}
	if cr.Conclusion != "" {
		out["conclusion"] = cr.Conclusion
	}
	return out
}

func (s *Server) createCheckRun(w http.ResponseWriter, r *http.Request) {
	var body checkRunBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == nil || body.HeadSHA == nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Invalid request"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	cr := &CheckRun{ID: s.nextID, Repo: repoOf(r), Status: "queued"}
	body.apply(cr)
	cr.SuiteID = s.suiteLocked(cr.Repo, cr.HeadSHA)
	s.checkRuns[cr.ID] = cr
	writeJSON(w, http.StatusCreated, s.checkRunJSON(cr))
}

func (s *Server) updateCheckRun(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var body checkRunBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Invalid request"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cr, ok := s.checkRuns[id]
	if !ok || cr.Repo != repoOf(r) {
		notFound(w)
		return
	}
	body.apply(cr)
	cr.Updates++
	writeJSON(w, http.StatusOK, s.checkRunJSON(cr))
}

func (s *Server) getCheckRun(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	s.mu.Lock()
	defer s.mu.Unlock()
	cr, ok := s.checkRuns[id]
	if !ok || cr.Repo != repoOf(r) {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, s.checkRunJSON(cr))
}

func (s *Server) listSuiteCheckRuns(w http.ResponseWriter, r *http.Request) {
	suite, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	s.mu.Lock()
	defer s.mu.Unlock()
	latest := map[string]*CheckRun{}
	for _, cr := range s.checkRuns {
		if cr.SuiteID != suite || cr.Repo != repoOf(r) {
			continue
		}
		if prev, ok := latest[cr.Name]; !ok || cr.ID > prev.ID {
			latest[cr.Name] = cr
		}
	}
	runs := make([]map[string]any, 0, len(latest))
	for _, name := range sortedNames(latest) {
		runs = append(runs, s.checkRunJSON(latest[name]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(runs), "check_runs": runs})
}

func sortedNames(m map[string]*CheckRun) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func (s *Server) permission(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	level, ok := s.Permissions[repoOf(r)+":"+r.PathValue("user")]
	s.mu.Unlock()
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"permission": level, "user": map[string]any{"login": r.PathValue("user")}})
}

func (s *Server) getPullRequest(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	pr, ok := s.pullRequests[repoOf(r)+"#"+r.PathValue("number")]
	s.mu.Unlock()
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"number":             pr.Number,
		"state":              pr.State,
		"draft":              pr.Draft,
		"author_association": pr.AuthorAssociation,
		"html_url":           fmt.Sprintf("https://github.com/%s/pull/%d", repoOf(r), pr.Number),
		"user":               map[string]any{"login": pr.Author},
		"head":               map[string]any{"ref": pr.HeadRef, "sha": pr.HeadSHA, "repo": map[string]any{"full_name": pr.HeadRepo}},
		"base":               map[string]any{"ref": pr.BaseRef, "sha": pr.BaseSHA},
	})
}

func (s *Server) getRef(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("ref")
	branch, ok := strings.CutPrefix(ref, "heads/")
	s.mu.Lock()
	sha, found := s.branches[repoOf(r)+":"+branch]
	s.mu.Unlock()
	if !ok || !found {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ref": "refs/" + ref, "object": map[string]any{"type": "commit", "sha": sha}})
}

func (s *Server) listRefCheckRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s.mu.Lock()
	defer s.mu.Unlock()
	var runs []map[string]any
	for _, id := range s.sortedCheckRunIDs() {
		cr := s.checkRuns[id]
		if cr.Repo != repoOf(r) || cr.HeadSHA != r.PathValue("ref") {
			continue
		}
		if name := q.Get("check_name"); name != "" && cr.Name != name {
			continue
		}
		if app := q.Get("app_id"); app != "" && app != strconv.FormatInt(s.AppID, 10) {
			continue
		}
		runs = append(runs, s.checkRunJSON(cr))
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(runs), "check_runs": runs})
}

func (s *Server) sortedCheckRunIDs() []int64 {
	ids := make([]int64, 0, len(s.checkRuns))
	for id := range s.checkRuns {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func (s *Server) createReaction(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var body struct {
		Content string `json:"content"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	s.reactions = append(s.reactions, Reaction{Repo: repoOf(r), CommentID: id, Content: body.Content})
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{"id": 1, "content": body.Content})
}

func (s *Server) createComment(w http.ResponseWriter, r *http.Request) {
	number, _ := strconv.Atoi(r.PathValue("number"))
	var body struct {
		Body string `json:"body"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	s.comments = append(s.comments, IssueComment{Repo: repoOf(r), Number: number, Body: body.Body})
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{"id": 1, "body": body.Body})
}

func (s *Server) listInstallations(w http.ResponseWriter, r *http.Request) {
	if !s.appAuth(w, r) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]int64, 0, len(s.installations))
	for id := range s.installations {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		out = append(out, map[string]any{"id": id, "account": map[string]any{"login": s.installations[id]}})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) listInstallationRepos(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "token ")
	s.mu.Lock()
	defer s.mu.Unlock()
	repos := s.instRepos[s.tokens[token]]
	out := make([]map[string]any, 0, len(repos))
	for _, repo := range repos {
		full := repo.Owner + "/" + repo.Name
		out = append(out, map[string]any{
			"id":             repo.ID,
			"name":           repo.Name,
			"full_name":      full,
			"owner":          map[string]any{"login": repo.Owner},
			"default_branch": repo.DefaultBranch,
			"archived":       repo.Archived,
			"clone_url":      "https://github.com/" + full + ".git",
			"html_url":       "https://github.com/" + full,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(out), "repositories": out})
}
