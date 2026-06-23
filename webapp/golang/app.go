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

func parseTemplates() {
	fmap := template.FuncMap{"imageURL": imageURL}
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
	// /initialize の後はインデックスページキャッシュも飛ばしておく
	invalidateIndexCache()
}

func tryLogin(ctx context.Context, accountName, password string) *User {
	u := User{}
	err := db.GetContext(ctx, &u, "SELECT * FROM users WHERE account_name = ? AND del_flg = 0", accountName)
	if err != nil {
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
	ctx := r.Context()
	session := getSession(r)
	uid, ok := session.Values["user_id"]
	if !ok || uid == nil {
		return User{}
	}
	u := User{}
	if err := db.GetContext(ctx, &u, "SELECT * FROM `users` WHERE `id` = ?", uid); err != nil {
		return User{}
	}
	return u
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
	var users []User
	q := "SELECT * FROM `users` WHERE `id` IN (" + placeholders(len(ids)) + ")"
	if err := db.SelectContext(ctx, &users, q, intsToArgs(ids)...); err != nil {
		return nil, err
	}
	out := make(map[int]User, len(users))
	for _, u := range users {
		out[u.ID] = u
	}
	return out, nil
}

func fetchCommentCounts(ctx context.Context, ids []int) (map[int]int, error) {
	if len(ids) == 0 {
		return map[int]int{}, nil
	}
	type row struct {
		PostID int `db:"post_id"`
		Count  int `db:"count"`
	}
	var rows []row
	q := "SELECT `post_id`, COUNT(*) AS `count` FROM `comments` WHERE `post_id` IN (" +
		placeholders(len(ids)) + ") GROUP BY `post_id`"
	if err := db.SelectContext(ctx, &rows, q, intsToArgs(ids)...); err != nil {
		return nil, err
	}
	out := make(map[int]int, len(ids))
	for _, r := range rows {
		out[r.PostID] = r.Count
	}
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
	indexCacheTTL  = 500 * time.Millisecond
)

func invalidateIndexCache() {
	indexCacheMu.Lock()
	indexCache = nil
	indexCacheMu.Unlock()
}

func cachedIndexPosts(ctx context.Context) ([]Post, error) {
	indexCacheMu.RLock()
	if indexCache != nil && time.Since(indexCacheTime) < indexCacheTTL {
		c := indexCache
		indexCacheMu.RUnlock()
		return c, nil
	}
	indexCacheMu.RUnlock()

	var results []Post
	err := db.SelectContext(ctx, &results,
		"SELECT STRAIGHT_JOIN p.`id`, p.`user_id`, p.`body`, p.`mime`, p.`created_at` "+
			"FROM `posts` p JOIN `users` u ON p.`user_id` = u.`id` "+
			"WHERE u.`del_flg` = 0 "+
			"ORDER BY p.`created_at` DESC LIMIT 20")
	if err != nil {
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

	exists := 0
	db.GetContext(ctx, &exists, "SELECT 1 FROM users WHERE `account_name` = ?", accountName)
	if exists == 1 {
		session := getSession(r)
		session.Values["notice"] = "アカウント名がすでに使われています"
		session.Save(r, w)
		http.Redirect(w, r, "/register", http.StatusFound)
		return
	}

	query := "INSERT INTO `users` (`account_name`, `passhash`) VALUES (?,?)"
	result, err := db.ExecContext(ctx, query, accountName, calculatePasshash(accountName, password))
	if err != nil {
		log.Print(err)
		return
	}

	session := getSession(r)
	uid, err := result.LastInsertId()
	if err != nil {
		log.Print(err)
		return
	}
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
	user := User{}
	if err := db.GetContext(ctx, &user, "SELECT * FROM `users` WHERE `account_name` = ? AND `del_flg` = 0", accountName); err != nil {
		log.Print(err)
		return
	}
	if user.ID == 0 {
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

	commentCount := 0
	if err := db.GetContext(ctx, &commentCount,
		"SELECT COUNT(*) AS count FROM `comments` WHERE `user_id` = ?", user.ID); err != nil {
		log.Print(err)
		return
	}
	postCount := 0
	if err := db.GetContext(ctx, &postCount,
		"SELECT COUNT(*) AS count FROM `posts` WHERE `user_id` = ?", user.ID); err != nil {
		log.Print(err)
		return
	}
	commentedCount := 0
	if postCount > 0 {
		if err := db.GetContext(ctx, &commentedCount,
			"SELECT COUNT(*) AS count FROM `comments` c JOIN `posts` p ON c.`post_id` = p.`id` WHERE p.`user_id` = ?", user.ID); err != nil {
			log.Print(err)
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
	t, err := time.Parse(ISO8601Format, maxCreatedAt)
	if err != nil {
		log.Print(err)
		return
	}
	var results []Post
	err = db.SelectContext(ctx, &results,
		"SELECT STRAIGHT_JOIN p.`id`, p.`user_id`, p.`body`, p.`mime`, p.`created_at` "+
			"FROM `posts` p JOIN `users` u ON p.`user_id` = u.`id` "+
			"WHERE u.`del_flg` = 0 AND p.`created_at` <= ? "+
			"ORDER BY p.`created_at` DESC LIMIT 20", t.Format(ISO8601Format))
	if err != nil {
		log.Print(err)
		return
	}
	posts, err := makePosts(ctx, results, getCSRFToken(r), false)
	if err != nil {
		log.Print(err)
		return
	}
	if len(posts) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	tmplPosts.Execute(w, posts)
}

func getPostsID(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pid, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	var results []Post
	if err := db.SelectContext(ctx, &results,
		"SELECT `id`, `user_id`, `body`, `mime`, `created_at` FROM `posts` WHERE `id` = ?", pid); err != nil {
		log.Print(err)
		return
	}
	posts, err := makePosts(ctx, results, getCSRFToken(r), true)
	if err != nil {
		log.Print(err)
		return
	}
	if len(posts) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	me := getSessionUser(r)
	tmplPost.Execute(w, struct {
		Post Post
		Me   User
	}{posts[0], me})
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
	invalidateIndexCache()

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
		return
	}
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
		return
	}
	for _, id := range r.Form["uid[]"] {
		db.ExecContext(ctx, "UPDATE `users` SET `del_flg` = ? WHERE `id` = ?", 1, id)
	}
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
