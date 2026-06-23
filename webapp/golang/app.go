package main

import (
	"context"
	crand "crypto/rand"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	gsm "github.com/bradleypeabody/gorilla-sessions-memcache"
	"github.com/go-chi/chi/v5"
	mysql "github.com/go-sql-driver/mysql"
	"github.com/gorilla/sessions"
	"github.com/jmoiron/sqlx"
	"golang.org/x/sync/singleflight"
)

var (
	db    *sqlx.DB
	store *gsm.MemcacheStore
)

const (
	postsPerPage  = 20
	ISO8601Format = "2006-01-02T15:04:05-07:00"
	UploadLimit   = 10 * 1024 * 1024 // 10mb
	ImageDir      = "../public/image"
)

type User struct {
	ID          int       `db:"id"`
	AccountName string    `db:"account_name"`
	Passhash    string    `db:"passhash"`
	Authority   int       `db:"authority"`
	DelFlg      int       `db:"del_flg"`
	CreatedAt   time.Time `db:"created_at"`
}

// imgdata 列は DB から削除済み (sql/02_drop_imgdata.sql)。ファイルは ImageDir 配下に保存
type Post struct {
	ID           int       `db:"id"`
	UserID       int       `db:"user_id"`
	Body         string    `db:"body"`
	Mime         string    `db:"mime"`
	CreatedAt    time.Time `db:"created_at"`
	CommentCount int
	Comments     []Comment
	User         User
	CSRFToken    string
}

type Comment struct {
	ID        int       `db:"id"`
	PostID    int       `db:"post_id"`
	UserID    int       `db:"user_id"`
	Comment   string    `db:"comment"`
	CreatedAt time.Time `db:"created_at"`
	User      User
}

var memcacheClient *memcache.Client

// comments(post_id) の COUNT を永続キャッシュ。post 追加では 0、comment 追加で +1。
var (
	commentCountByPost = map[int]int{}
	commentCountByUser = map[int]int{}
	commentCountMu     sync.RWMutex
)

func reloadCommentCountCache(ctx context.Context) error {
	type row struct {
		PostID int `db:"post_id"`
		Count  int `db:"count"`
	}
	var rows []row
	if err := db.SelectContext(ctx, &rows, "SELECT `post_id`, COUNT(*) AS `count` FROM `comments` GROUP BY `post_id`"); err != nil {
		return err
	}
	byPost := make(map[int]int, len(rows))
	for _, r := range rows {
		byPost[r.PostID] = r.Count
	}

	type urow struct {
		UserID int `db:"user_id"`
		Count  int `db:"count"`
	}
	var urows []urow
	if err := db.SelectContext(ctx, &urows, "SELECT `user_id`, COUNT(*) AS `count` FROM `comments` GROUP BY `user_id`"); err != nil {
		return err
	}
	byUser := make(map[int]int, len(urows))
	for _, r := range urows {
		byUser[r.UserID] = r.Count
	}

	commentCountMu.Lock()
	commentCountByPost = byPost
	commentCountByUser = byUser
	commentCountMu.Unlock()
	return nil
}

func incCommentCount(postID, userID int) {
	commentCountMu.Lock()
	commentCountByPost[postID]++
	commentCountByUser[userID]++
	commentCountMu.Unlock()
}

func getCommentCountForPost(id int) int {
	commentCountMu.RLock()
	c := commentCountByPost[id]
	commentCountMu.RUnlock()
	return c
}

func getCommentCountForUser(id int) int {
	commentCountMu.RLock()
	c := commentCountByUser[id]
	commentCountMu.RUnlock()
	return c
}

// users 全件オンメモリ。1007 件 + α、変更は POST /register, /admin/banned, /initialize のみ
var (
	usersByID      = map[int]User{}
	usersByName    = map[string]User{}
	usersCacheMu   sync.RWMutex
	usersCacheInit = false
)

func reloadUsersCache(ctx context.Context) error {
	var users []User
	if err := db.SelectContext(ctx, &users, "SELECT * FROM `users`"); err != nil {
		return err
	}
	byID := make(map[int]User, len(users))
	byName := make(map[string]User, len(users))
	for _, u := range users {
		byID[u.ID] = u
		byName[u.AccountName] = u
	}
	usersCacheMu.Lock()
	usersByID = byID
	usersByName = byName
	usersCacheInit = true
	usersCacheMu.Unlock()
	return nil
}

func userByID(id int) (User, bool) {
	usersCacheMu.RLock()
	u, ok := usersByID[id]
	usersCacheMu.RUnlock()
	return u, ok
}

func userByName(name string) (User, bool) {
	usersCacheMu.RLock()
	u, ok := usersByName[name]
	usersCacheMu.RUnlock()
	return u, ok
}

func upsertUser(u User) {
	usersCacheMu.Lock()
	usersByID[u.ID] = u
	usersByName[u.AccountName] = u
	usersCacheMu.Unlock()
}

// 起動時に一度だけパースしておく（毎リクエスト ParseFiles するコストを排除）
var (
	tmplIndex       *template.Template
	tmplUser        *template.Template
	tmplPosts       *template.Template
	tmplPost        *template.Template
	tmplLogin       *template.Template
	tmplRegister    *template.Template
	tmplAdminBanned *template.Template
)

func templPath(filename string) string {
	return path.Join("templates", filename)
}

func bodyHTML(body string) template.HTML {
	escaped := template.HTMLEscapeString(body)
	escaped = strings.ReplaceAll(escaped, "\r\n", "<br>")
	escaped = strings.ReplaceAll(escaped, "\n", "<br>")
	return template.HTML(escaped)
}

func parseTemplates() {
	fmap := template.FuncMap{
		"imageURL": imageURL,
		"bodyHTML": bodyHTML,
	}
	tmplIndex = template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(
		templPath("layout.html"), templPath("index.html"), templPath("posts.html"), templPath("post.html"),
	))
	tmplUser = template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(
		templPath("layout.html"), templPath("user.html"), templPath("posts.html"), templPath("post.html"),
	))
	tmplPosts = template.Must(template.New("posts.html").Funcs(fmap).ParseFiles(
		templPath("posts.html"), templPath("post.html"),
	))
	tmplPost = template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(
		templPath("layout.html"), templPath("post_id.html"), templPath("post.html"),
	))
	tmplLogin = template.Must(template.ParseFiles(templPath("layout.html"), templPath("login.html")))
	tmplRegister = template.Must(template.ParseFiles(templPath("layout.html"), templPath("register.html")))
	tmplAdminBanned = template.Must(template.ParseFiles(templPath("layout.html"), templPath("banned.html")))
}

func init() {
	memdAddr := os.Getenv("ISUCONP_MEMCACHED_ADDRESS")
	if memdAddr == "" {
		memdAddr = "localhost:11211"
	}
	memcacheClient = memcache.New(memdAddr)
	store = gsm.NewMemcacheStore(memcacheClient, "iscogram_", []byte("sendagaya"))
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
}

func dbInitialize(ctx context.Context) {
	sqls := []string{
		"DELETE FROM users WHERE id > 1000",
		"DELETE FROM posts WHERE id > 10000",
		"DELETE FROM comments WHERE id > 100000",
		"UPDATE users SET del_flg = 0",
		"UPDATE users SET del_flg = 1 WHERE id % 50 = 0",
	}
	for _, sql := range sqls {
		db.ExecContext(ctx, sql)
	}
	// /initialize 後はキャッシュを全部仕込み直す
	_ = reloadUsersCache(ctx)
	_ = reloadCommentCountCache(ctx)
	invalidateIndexCache()
}

func tryLogin(ctx context.Context, accountName, password string) *User {
	u, ok := userByName(accountName)
	if !ok || u.DelFlg != 0 {
		return nil
	}
	if calculatePasshash(u.AccountName, password) == u.Passhash {
		return &u
	}
	return nil
}

func validateUser(accountName, password string) bool {
	return regexp.MustCompile(`\A[0-9a-zA-Z_]{3,}\z`).MatchString(accountName) &&
		regexp.MustCompile(`\A[0-9a-zA-Z_]{6,}\z`).MatchString(password)
}

// crypto/sha512 で hex digest。元はシェルアウト
func digest(src string) string {
	h := sha512.Sum512([]byte(src))
	return hex.EncodeToString(h[:])
}

func calculateSalt(accountName string) string {
	return digest(accountName)
}

func calculatePasshash(accountName, password string) string {
	return digest(password + ":" + calculateSalt(accountName))
}

func getSession(r *http.Request) *sessions.Session {
	session, _ := store.Get(r, "isuconp-go.session")
	return session
}

func getSessionUser(r *http.Request) User {
	session := getSession(r)
	uid, ok := session.Values["user_id"]
	if !ok || uid == nil {
		return User{}
	}
	// session の user_id 型は INSERT の戻り値で int64、Get 系で int になることがある
	var id int
	switch v := uid.(type) {
	case int:
		id = v
	case int64:
		id = int(v)
	default:
		return User{}
	}
	if u, found := userByID(id); found {
		return u
	}
	return User{}
}

func getFlash(w http.ResponseWriter, r *http.Request, key string) string {
	session := getSession(r)
	value, ok := session.Values[key]
	if !ok || value == nil {
		return ""
	}
	delete(session.Values, key)
	session.Save(r, w)
	return value.(string)
}

// makePosts: N+1 を IN 句でまとめる
func makePosts(ctx context.Context, results []Post, csrfToken string, allComments bool) ([]Post, error) {
	if len(results) == 0 {
		return nil, nil
	}

	userIDSet := make(map[int]struct{}, len(results))
	for _, p := range results {
		userIDSet[p.UserID] = struct{}{}
	}
	users, err := fetchUsersByIDs(ctx, mapKeys(userIDSet))
	if err != nil {
		return nil, err
	}

	kept := make([]Post, 0, postsPerPage)
	for _, p := range results {
		u, ok := users[p.UserID]
		if !ok || u.DelFlg != 0 {
			continue
		}
		kept = append(kept, p)
		if len(kept) >= postsPerPage {
			break
		}
	}
	if len(kept) == 0 {
		return nil, nil
	}

	postIDs := make([]int, 0, len(kept))
	for _, p := range kept {
		postIDs = append(postIDs, p.ID)
	}

	counts, err := fetchCommentCounts(ctx, postIDs)
	if err != nil {
		return nil, err
	}
	commentsByPost, err := fetchCommentsByPostIDs(ctx, postIDs, allComments)
	if err != nil {
		return nil, err
	}

	commentUserIDSet := map[int]struct{}{}
	for _, cs := range commentsByPost {
		for _, c := range cs {
			commentUserIDSet[c.UserID] = struct{}{}
		}
	}
	var commentUsers map[int]User
	if len(commentUserIDSet) > 0 {
		commentUsers, err = fetchUsersByIDs(ctx, mapKeys(commentUserIDSet))
		if err != nil {
			return nil, err
		}
	} else {
		commentUsers = map[int]User{}
	}

	out := make([]Post, 0, len(kept))
	for _, p := range kept {
		cs := commentsByPost[p.ID]
		for i := range cs {
			cs[i].User = commentUsers[cs[i].UserID]
		}
		// reverse to match Ruby `.reverse`
		for i, j := 0, len(cs)-1; i < j; i, j = i+1, j-1 {
			cs[i], cs[j] = cs[j], cs[i]
		}
		p.Comments = cs
		p.CommentCount = counts[p.ID]
		p.User = users[p.UserID]
		p.CSRFToken = csrfToken
		out = append(out, p)
	}
	return out, nil
}

func mapKeys(m map[int]struct{}) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}

func intsToArgs(ids []int) []any {
	args := make([]any, len(ids))
	for i, v := range ids {
		args[i] = v
	}
	return args
}

func fetchUsersByIDs(ctx context.Context, ids []int) (map[int]User, error) {
	if len(ids) == 0 {
		return map[int]User{}, nil
	}
	out := make(map[int]User, len(ids))
	usersCacheMu.RLock()
	for _, id := range ids {
		if u, ok := usersByID[id]; ok {
			out[id] = u
		}
	}
	usersCacheMu.RUnlock()
	return out, nil
}

func fetchCommentCounts(ctx context.Context, ids []int) (map[int]int, error) {
	out := make(map[int]int, len(ids))
	commentCountMu.RLock()
	for _, id := range ids {
		out[id] = commentCountByPost[id]
	}
	commentCountMu.RUnlock()
	return out, nil
}

func fetchCommentsByPostIDs(ctx context.Context, ids []int, allComments bool) (map[int][]Comment, error) {
	if len(ids) == 0 {
		return map[int][]Comment{}, nil
	}
	var comments []Comment
	q := "SELECT * FROM `comments` WHERE `post_id` IN (" + placeholders(len(ids)) +
		") ORDER BY `post_id`, `created_at` DESC"
	if err := db.SelectContext(ctx, &comments, q, intsToArgs(ids)...); err != nil {
		return nil, err
	}
	out := make(map[int][]Comment, len(ids))
	for _, c := range comments {
		out[c.PostID] = append(out[c.PostID], c)
	}
	if !allComments {
		for k, v := range out {
			if len(v) > 3 {
				out[k] = v[:3]
			}
		}
	}
	return out, nil
}

func imageURL(p Post) string {
	ext := ""
	switch p.Mime {
	case "image/jpeg":
		ext = ".jpg"
	case "image/png":
		ext = ".png"
	case "image/gif":
		ext = ".gif"
	}
	return "/image/" + strconv.Itoa(p.ID) + ext
}

func mimeExt(mime string) string {
	switch mime {
	case "image/jpeg":
		return "jpg"
	case "image/png":
		return "png"
	case "image/gif":
		return "gif"
	}
	return ""
}

func isLogin(u User) bool {
	return u.ID != 0
}

func getCSRFToken(r *http.Request) string {
	session := getSession(r)
	csrfToken, ok := session.Values["csrf_token"]
	if !ok {
		return ""
	}
	return csrfToken.(string)
}

func secureRandomStr(b int) string {
	k := make([]byte, b)
	if _, err := crand.Read(k); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", k)
}

// GET / の短 TTL キャッシュ (per-process; Go なら全 goroutine 共有)
var (
	indexCacheMu   sync.RWMutex
	indexCache     []Post
	indexCacheTime time.Time
	indexCacheTTL  = 2 * time.Second
)

func invalidateIndexCache() {
	indexCacheMu.Lock()
	indexCache = nil
	indexCacheMu.Unlock()
}

var indexFlight singleflight.Group

// post_id ごとに make_posts 結果 (all_comments=true) を短期キャッシュ
type cachedPost struct {
	post    Post
	expires time.Time
}

var (
	postCacheMap    sync.Map // map[int]*cachedPost
	postCacheFlight singleflight.Group
	postCacheTTL    = 1 * time.Second
)

// GET /posts?max_created_at=X を string キーでキャッシュ
type cachedPostsList struct {
	posts   []Post
	expires time.Time
}

var (
	postsListCacheMap    sync.Map // map[string]*cachedPostsList
	postsListCacheFlight singleflight.Group
	postsListCacheTTL    = 1 * time.Second
)

func invalidatePostCache(id int) {
	postCacheMap.Delete(id)
}

func cachedPostDetail(ctx context.Context, id int, csrfToken string) (*Post, error) {
	if v, ok := postCacheMap.Load(id); ok {
		c := v.(*cachedPost)
		if time.Now().Before(c.expires) {
			p := c.post
			p.CSRFToken = csrfToken
			return &p, nil
		}
	}

	v, err, _ := postCacheFlight.Do(strconv.Itoa(id), func() (any, error) {
		// double-check
		if v, ok := postCacheMap.Load(id); ok {
			c := v.(*cachedPost)
			if time.Now().Before(c.expires) {
				return c, nil
			}
		}

		var results []Post
		if err := db.SelectContext(ctx, &results,
			"SELECT `id`, `user_id`, `body`, `mime`, `created_at` FROM `posts` WHERE `id` = ?", id); err != nil {
			return nil, err
		}
		posts, err := makePosts(ctx, results, "", true)
		if err != nil {
			return nil, err
		}
		if len(posts) == 0 {
			return nil, nil
		}
		entry := &cachedPost{post: posts[0], expires: time.Now().Add(postCacheTTL)}
		postCacheMap.Store(id, entry)
		return entry, nil
	})
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, nil
	}
	c := v.(*cachedPost)
	p := c.post
	p.CSRFToken = csrfToken
	return &p, nil
}

func cachedIndexPosts(ctx context.Context) ([]Post, error) {
	indexCacheMu.RLock()
	if indexCache != nil && time.Since(indexCacheTime) < indexCacheTTL {
		c := indexCache
		indexCacheMu.RUnlock()
		return c, nil
	}
	indexCacheMu.RUnlock()

	v, err, _ := indexFlight.Do("index", func() (any, error) {
		// double-check: 先行 goroutine が更新してる可能性
		indexCacheMu.RLock()
		if indexCache != nil && time.Since(indexCacheTime) < indexCacheTTL {
			c := indexCache
			indexCacheMu.RUnlock()
			return c, nil
		}
		indexCacheMu.RUnlock()

		var results []Post
		if err := db.SelectContext(ctx, &results,
			"SELECT STRAIGHT_JOIN p.`id`, p.`user_id`, p.`body`, p.`mime`, p.`created_at` "+
				"FROM `posts` p JOIN `users` u ON p.`user_id` = u.`id` "+
				"WHERE u.`del_flg` = 0 "+
				"ORDER BY p.`created_at` DESC LIMIT 20"); err != nil {
			return nil, err
		}
		posts, err := makePosts(ctx, results, "", false)
		if err != nil {
			return nil, err
		}
		indexCacheMu.Lock()
		indexCache = posts
		indexCacheTime = time.Now()
		indexCacheMu.Unlock()
		return posts, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]Post), nil
}

func getInitialize(w http.ResponseWriter, r *http.Request) {
	dbInitialize(r.Context())
	w.WriteHeader(http.StatusOK)
}

func getLogin(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)
	if isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	tmplLogin.Execute(w, struct {
		Me    User
		Flash string
	}{me, getFlash(w, r, "notice")})
}

func postLogin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	u := tryLogin(ctx, r.FormValue("account_name"), r.FormValue("password"))
	if u != nil {
		session := getSession(r)
		session.Values["user_id"] = u.ID
		session.Values["csrf_token"] = secureRandomStr(16)
		session.Save(r, w)
		http.Redirect(w, r, "/", http.StatusFound)
	} else {
		session := getSession(r)
		session.Values["notice"] = "アカウント名かパスワードが間違っています"
		session.Save(r, w)
		http.Redirect(w, r, "/login", http.StatusFound)
	}
}

func getRegister(w http.ResponseWriter, r *http.Request) {
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	tmplRegister.Execute(w, struct {
		Me    User
		Flash string
	}{User{}, getFlash(w, r, "notice")})
}

func postRegister(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	accountName, password := r.FormValue("account_name"), r.FormValue("password")
	if !validateUser(accountName, password) {
		session := getSession(r)
		session.Values["notice"] = "アカウント名は3文字以上、パスワードは6文字以上である必要があります"
		session.Save(r, w)
		http.Redirect(w, r, "/register", http.StatusFound)
		return
	}

	if _, exists := userByName(accountName); exists {
		session := getSession(r)
		session.Values["notice"] = "アカウント名がすでに使われています"
		session.Save(r, w)
		http.Redirect(w, r, "/register", http.StatusFound)
		return
	}

	passhash := calculatePasshash(accountName, password)
	query := "INSERT INTO `users` (`account_name`, `passhash`) VALUES (?,?)"
	result, err := db.ExecContext(ctx, query, accountName, passhash)
	if err != nil {
		log.Print(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	uid64, err := result.LastInsertId()
	if err != nil {
		log.Print(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	uid := int(uid64)
	upsertUser(User{ID: uid, AccountName: accountName, Passhash: passhash, Authority: 0, DelFlg: 0, CreatedAt: time.Now()})

	session := getSession(r)
	session.Values["user_id"] = uid
	session.Values["csrf_token"] = secureRandomStr(16)
	session.Save(r, w)
	http.Redirect(w, r, "/", http.StatusFound)
}

func getLogout(w http.ResponseWriter, r *http.Request) {
	session := getSession(r)
	delete(session.Values, "user_id")
	session.Options = &sessions.Options{MaxAge: -1}
	session.Save(r, w)
	http.Redirect(w, r, "/", http.StatusFound)
}

func getIndex(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := getSessionUser(r)
	csrfToken := getCSRFToken(r)

	cached, err := cachedIndexPosts(ctx)
	if err != nil {
		log.Print(err)
		return
	}
	// CSRFToken はリクエストごとに差し替え
	posts := make([]Post, len(cached))
	for i, p := range cached {
		p.CSRFToken = csrfToken
		posts[i] = p
	}

	tmplIndex.Execute(w, struct {
		Posts     []Post
		Me        User
		CSRFToken string
		Flash     string
	}{posts, me, csrfToken, getFlash(w, r, "notice")})
}

func getAccountName(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	accountName := r.PathValue("accountName")
	user, ok := userByName(accountName)
	if !ok || user.DelFlg != 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	var results []Post
	if err := db.SelectContext(ctx, &results,
		"SELECT `id`, `user_id`, `body`, `mime`, `created_at` FROM `posts` "+
			"WHERE `user_id` = ? ORDER BY `created_at` DESC LIMIT 20", user.ID); err != nil {
		log.Print(err)
		return
	}
	posts, err := makePosts(ctx, results, getCSRFToken(r), false)
	if err != nil {
		log.Print(err)
		return
	}

	commentCount := getCommentCountForUser(user.ID)
	postCount := 0
	if err := db.GetContext(ctx, &postCount,
		"SELECT COUNT(*) AS count FROM `posts` WHERE `user_id` = ?", user.ID); err != nil {
		log.Print(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	commentedCount := 0
	if postCount > 0 {
		if err := db.GetContext(ctx, &commentedCount,
			"SELECT COUNT(*) AS count FROM `comments` c JOIN `posts` p ON c.`post_id` = p.`id` WHERE p.`user_id` = ?", user.ID); err != nil {
			log.Print(err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	me := getSessionUser(r)
	tmplUser.Execute(w, struct {
		Posts          []Post
		User           User
		PostCount      int
		CommentCount   int
		CommentedCount int
		Me             User
	}{posts, user, postCount, commentCount, commentedCount, me})
}

func getPosts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	m, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Print(err)
		return
	}
	maxCreatedAt := m.Get("max_created_at")
	if maxCreatedAt == "" {
		return
	}
	posts, err := getCachedPostsList(ctx, maxCreatedAt)
	if err != nil {
		log.Print(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if len(posts) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	tmplPosts.Execute(w, posts)
}

func getCachedPostsList(ctx context.Context, maxCreatedAt string) ([]Post, error) {
	if v, ok := postsListCacheMap.Load(maxCreatedAt); ok {
		c := v.(*cachedPostsList)
		if time.Now().Before(c.expires) {
			return c.posts, nil
		}
	}

	v, err, _ := postsListCacheFlight.Do(maxCreatedAt, func() (any, error) {
		if v, ok := postsListCacheMap.Load(maxCreatedAt); ok {
			c := v.(*cachedPostsList)
			if time.Now().Before(c.expires) {
				return c.posts, nil
			}
		}

		t, err := time.Parse(ISO8601Format, maxCreatedAt)
		if err != nil {
			return nil, err
		}
		var results []Post
		if err := db.SelectContext(ctx, &results,
			"SELECT STRAIGHT_JOIN p.`id`, p.`user_id`, p.`body`, p.`mime`, p.`created_at` "+
				"FROM `posts` p JOIN `users` u ON p.`user_id` = u.`id` "+
				"WHERE u.`del_flg` = 0 AND p.`created_at` <= ? "+
				"ORDER BY p.`created_at` DESC LIMIT 20", t.Format(ISO8601Format)); err != nil {
			return nil, err
		}
		posts, err := makePosts(ctx, results, "", false)
		if err != nil {
			return nil, err
		}
		postsListCacheMap.Store(maxCreatedAt, &cachedPostsList{
			posts:   posts,
			expires: time.Now().Add(postsListCacheTTL),
		})
		return posts, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]Post), nil
}

func getPostsID(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pid, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	p, err := cachedPostDetail(ctx, pid, getCSRFToken(r))
	if err != nil {
		log.Print(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if p == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	me := getSessionUser(r)
	tmplPost.Execute(w, struct {
		Post Post
		Me   User
	}{*p, me})
}

func postIndex(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		session := getSession(r)
		session.Values["notice"] = "画像が必須です"
		session.Save(r, w)
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	mime := ""
	if file != nil {
		contentType := header.Header["Content-Type"][0]
		switch {
		case strings.Contains(contentType, "jpeg"):
			mime = "image/jpeg"
		case strings.Contains(contentType, "png"):
			mime = "image/png"
		case strings.Contains(contentType, "gif"):
			mime = "image/gif"
		default:
			session := getSession(r)
			session.Values["notice"] = "投稿できる画像形式はjpgとpngとgifだけです"
			session.Save(r, w)
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
	}

	filedata, err := io.ReadAll(file)
	if err != nil {
		log.Print(err)
		return
	}
	if len(filedata) > UploadLimit {
		session := getSession(r)
		session.Values["notice"] = "ファイルサイズが大きすぎます"
		session.Save(r, w)
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	// imgdata 列は削除済み。ファイル本体は disk へ書く
	query := "INSERT INTO `posts` (`user_id`, `mime`, `body`) VALUES (?,?,?)"
	result, err := db.ExecContext(ctx, query, me.ID, mime, r.FormValue("body"))
	if err != nil {
		log.Print(err)
		return
	}
	pid, err := result.LastInsertId()
	if err != nil {
		log.Print(err)
		return
	}

	if ext := mimeExt(mime); ext != "" {
		_ = os.MkdirAll(ImageDir, 0755)
		if err := os.WriteFile(fmt.Sprintf("%s/%d.%s", ImageDir, pid, ext), filedata, 0644); err != nil {
			log.Printf("disk write: %v", err)
		}
	}
	// INDEX_CACHE は TTL 任せ。POST のたび invalidate すると thundering herd で MySQL に殺到する

	http.Redirect(w, r, "/posts/"+strconv.FormatInt(pid, 10), http.StatusFound)
}

// nginx try_files で disk から返るためここに来るのは disk ミス時のみ
func getImage(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNotFound)
}

func postComment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}
	postID, err := strconv.Atoi(r.FormValue("post_id"))
	if err != nil {
		log.Print("post_idは整数のみです")
		return
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO `comments` (`post_id`, `user_id`, `comment`) VALUES (?,?,?)",
		postID, me.ID, r.FormValue("comment")); err != nil {
		log.Print(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	incCommentCount(postID, me.ID)
	invalidatePostCache(postID)
	http.Redirect(w, r, fmt.Sprintf("/posts/%d", postID), http.StatusFound)
}

func getAdminBanned(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if me.Authority == 0 {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	var users []User
	if err := db.SelectContext(ctx, &users,
		"SELECT * FROM `users` WHERE `authority` = 0 AND `del_flg` = 0 ORDER BY `created_at` DESC"); err != nil {
		log.Print(err)
		return
	}
	tmplAdminBanned.Execute(w, struct {
		Users     []User
		Me        User
		CSRFToken string
	}{users, me, getCSRFToken(r)})
}

func postAdminBanned(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if me.Authority == 0 {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}
	if err := r.ParseForm(); err != nil {
		log.Print(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	for _, id := range r.Form["uid[]"] {
		db.ExecContext(ctx, "UPDATE `users` SET `del_flg` = ? WHERE `id` = ?", 1, id)
	}
	_ = reloadUsersCache(ctx)
	invalidateIndexCache()
	http.Redirect(w, r, "/admin/banned", http.StatusFound)
}

func main() {
	host := os.Getenv("ISUCONP_DB_HOST")
	if host == "" {
		host = "localhost"
	}
	port := os.Getenv("ISUCONP_DB_PORT")
	if port == "" {
		port = "3306"
	}
	if _, err := strconv.Atoi(port); err != nil {
		log.Fatalf("Failed to read DB port number from an environment variable ISUCONP_DB_PORT.\nError: %s", err.Error())
	}
	user := os.Getenv("ISUCONP_DB_USER")
	if user == "" {
		user = "root"
	}
	password := os.Getenv("ISUCONP_DB_PASSWORD")
	dbname := os.Getenv("ISUCONP_DB_NAME")
	if dbname == "" {
		dbname = "isuconp"
	}

	cfg := mysql.NewConfig()
	cfg.User = user
	cfg.Passwd = password
	cfg.Net = "tcp"
	cfg.Addr = fmt.Sprintf("%s:%s", host, port)
	cfg.DBName = dbname
	cfg.Params = map[string]string{"charset": "utf8mb4"}
	cfg.ParseTime = true
	cfg.Loc = time.Local
	cfg.InterpolateParams = true
	dsn := cfg.FormatDSN()

	var err error
	db, err = sqlx.Open("mysql", dsn)
	if err != nil {
		log.Fatalf("Failed to connect to DB: %s.", err.Error())
	}
	defer db.Close()
	db.SetMaxOpenConns(100)
	db.SetMaxIdleConns(100)
	db.SetConnMaxLifetime(time.Hour)

	parseTemplates()

	// users / comment_count キャッシュ初期化
	if err := reloadUsersCache(context.Background()); err != nil {
		log.Printf("warm users cache: %v", err)
	}
	if err := reloadCommentCountCache(context.Background()); err != nil {
		log.Printf("warm comment_count cache: %v", err)
	}

	r := chi.NewRouter()

	r.Get("/initialize", getInitialize)
	r.Get("/login", getLogin)
	r.Post("/login", postLogin)
	r.Get("/register", getRegister)
	r.Post("/register", postRegister)
	r.Get("/logout", getLogout)
	r.Get("/", getIndex)
	r.Get("/posts", getPosts)
	r.Get("/posts/{id}", getPostsID)
	r.Post("/", postIndex)
	r.Get("/image/{id}.{ext}", getImage)
	r.Post("/comment", postComment)
	r.Get("/admin/banned", getAdminBanned)
	r.Post("/admin/banned", postAdminBanned)
	r.Get(`/@{accountName:[0-9a-zA-Z_]+}`, getAccountName)
	r.Mount("/", http.FileServer(http.Dir("../public")))

	log.Fatal(http.ListenAndServe(":8080", r))
}
