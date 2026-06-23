require 'sinatra/base'
require 'mysql2'
require 'rack-flash'
require 'openssl'
require 'fileutils'
require 'rack/session/dalli'

module Isuconp
  class App < Sinatra::Base
    use Rack::Session::Dalli, autofix_keys: true, secret: ENV['ISUCONP_SESSION_SECRET'] || 'sendagaya', memcache_server: ENV['ISUCONP_MEMCACHED_ADDRESS'] || 'localhost:11211'
    use Rack::Flash
    set :public_folder, File.expand_path('../../public', __FILE__)

    # refs: https://github.com/advisories/GHSA-hxx2-7vcw-mqr3
    set :host_authorization, { permitted_hosts: [] }

    UPLOAD_LIMIT = 10 * 1024 * 1024 # 10mb

    POSTS_PER_PAGE = 20

    IMAGE_DIR = File.expand_path('../../public/image', __FILE__)
    IMAGE_EXT = { 'image/jpeg' => 'jpg', 'image/png' => 'png', 'image/gif' => 'gif' }.freeze

    helpers do
      def config
        @config ||= {
          db: {
            host: ENV['ISUCONP_DB_HOST'] || 'localhost',
            port: ENV['ISUCONP_DB_PORT'] && ENV['ISUCONP_DB_PORT'].to_i,
            username: ENV['ISUCONP_DB_USER'] || 'root',
            password: ENV['ISUCONP_DB_PASSWORD'],
            database: ENV['ISUCONP_DB_NAME'] || 'isuconp',
          },
        }
      end

      def db
        return Thread.current[:isuconp_db] if Thread.current[:isuconp_db]
        client = Mysql2::Client.new(
          host: config[:db][:host],
          port: config[:db][:port],
          username: config[:db][:username],
          password: config[:db][:password],
          database: config[:db][:database],
          encoding: 'utf8mb4',
          reconnect: true,
        )
        client.query_options.merge!(symbolize_keys: true, database_timezone: :local, application_timezone: :local)
        Thread.current[:isuconp_db] = client
        client
      end

      def db_initialize
        sql = []
        sql << 'DELETE FROM users WHERE id > 1000'
        sql << 'DELETE FROM posts WHERE id > 10000'
        sql << 'DELETE FROM comments WHERE id > 100000'
        sql << 'UPDATE users SET del_flg = 0'
        sql << 'UPDATE users SET del_flg = 1 WHERE id % 50 = 0'
        sql.each do |s|
          db.prepare(s).execute
        end
      end

      def try_login(account_name, password)
        user = db.prepare('SELECT * FROM users WHERE account_name = ? AND del_flg = 0').execute(account_name).first

        if user && calculate_passhash(user[:account_name], password) == user[:passhash]
          return user
        else
          return nil
        end
      end

      def validate_user(account_name, password)
        if !(/\A[0-9a-zA-Z_]{3,}\z/.match(account_name) && /\A[0-9a-zA-Z_]{6,}\z/.match(password))
          return false
        end

        return true
      end

      def digest(src)
        OpenSSL::Digest::SHA512.hexdigest(src)
      end

      def calculate_salt(account_name)
        digest account_name
      end

      def calculate_passhash(account_name, password)
        digest "#{password}:#{calculate_salt(account_name)}"
      end

      def get_session_user()
        if session[:user]
          db.prepare('SELECT * FROM `users` WHERE `id` = ?').execute(
            session[:user][:id]
          ).first
        else
          nil
        end
      end

      def make_posts(results, all_comments: false)
        rows = results.to_a
        return [] if rows.empty?

        user_ids = rows.map { |p| p[:user_id] }.uniq
        users = fetch_users_by_ids(user_ids)
        # del_flg=0 のユーザーの投稿だけ通し、POSTS_PER_PAGE まで残す
        kept = rows.select { |p| u = users[p[:user_id]]; u && u[:del_flg] == 0 }.first(POSTS_PER_PAGE)
        return [] if kept.empty?

        post_ids = kept.map { |p| p[:id] }
        comment_counts = fetch_comment_counts_by_post_ids(post_ids)
        comments_by_post = fetch_comments_by_post_ids(post_ids, all_comments: all_comments)

        # comments に紐づく user を一括取得
        comment_user_ids = comments_by_post.values.flatten.map { |c| c[:user_id] }.uniq
        comment_users = fetch_users_by_ids(comment_user_ids)

        kept.map do |post|
          comments = (comments_by_post[post[:id]] || []).map do |c|
            c[:user] = comment_users[c[:user_id]]
            c
          end
          post[:comment_count] = comment_counts[post[:id]] || 0
          post[:comments] = comments.reverse
          post[:user] = users[post[:user_id]]
          post
        end
      end

      def fetch_users_by_ids(ids)
        return {} if ids.empty?
        placeholders = (['?'] * ids.length).join(',')
        rows = db.prepare("SELECT * FROM `users` WHERE `id` IN (#{placeholders})").execute(*ids).to_a
        rows.each_with_object({}) { |u, h| h[u[:id]] = u }
      end

      def fetch_comment_counts_by_post_ids(ids)
        return {} if ids.empty?
        placeholders = (['?'] * ids.length).join(',')
        rows = db.prepare("SELECT `post_id`, COUNT(*) AS `count` FROM `comments` WHERE `post_id` IN (#{placeholders}) GROUP BY `post_id`").execute(*ids).to_a
        rows.each_with_object(Hash.new(0)) { |r, h| h[r[:post_id]] = r[:count] }
      end

      def fetch_comments_by_post_ids(ids, all_comments:)
        return {} if ids.empty?
        placeholders = (['?'] * ids.length).join(',')
        rows = db.prepare("SELECT * FROM `comments` WHERE `post_id` IN (#{placeholders}) ORDER BY `post_id`, `created_at` DESC").execute(*ids).to_a
        grouped = rows.group_by { |c| c[:post_id] }
        return grouped if all_comments
        grouped.transform_values { |cs| cs.first(3) }
      end

      def image_url(post)
        ext = ""
        if post[:mime] == "image/jpeg"
          ext = ".jpg"
        elsif post[:mime] == "image/png"
          ext = ".png"
        elsif post[:mime] == "image/gif"
          ext = ".gif"
        end

        "/image/#{post[:id]}#{ext}"
      end
    end

    get '/initialize' do
      db_initialize
      return 200
    end

    get '/login' do
      if get_session_user()
        redirect '/', 302
      end
      erb :login, layout: :layout, locals: { me: nil }
    end

    post '/login' do
      if get_session_user()
        redirect '/', 302
      end

      user = try_login(params['account_name'], params['password'])
      if user
        session[:user] = {
          id: user[:id]
        }
        session[:csrf_token] = SecureRandom.hex(16)
        redirect '/', 302
      else
        flash[:notice] = 'アカウント名かパスワードが間違っています'
        redirect '/login', 302
      end
    end

    get '/register' do
      if get_session_user()
        redirect '/', 302
      end
      erb :register, layout: :layout, locals: { me: nil }
    end

    post '/register' do
      if get_session_user()
        redirect '/', 302
      end

      account_name = params['account_name']
      password = params['password']

      validated = validate_user(account_name, password)
      if !validated
        flash[:notice] = 'アカウント名は3文字以上、パスワードは6文字以上である必要があります'
        redirect '/register', 302
        return
      end

      user = db.prepare('SELECT 1 FROM users WHERE `account_name` = ?').execute(account_name).first
      if user
        flash[:notice] = 'アカウント名がすでに使われています'
        redirect '/register', 302
        return
      end

      query = 'INSERT INTO `users` (`account_name`, `passhash`) VALUES (?,?)'
      db.prepare(query).execute(
        account_name,
        calculate_passhash(account_name, password)
      )

      session[:user] = {
        id: db.last_id
      }
      session[:csrf_token] = SecureRandom.hex(16)
      redirect '/', 302
    end

    get '/logout' do
      session.delete(:user)
      redirect '/', 302
    end

    get '/' do
      me = get_session_user()

      results = db.query(
        'SELECT STRAIGHT_JOIN p.`id`, p.`user_id`, p.`body`, p.`created_at`, p.`mime` ' \
        'FROM `posts` p JOIN `users` u ON p.`user_id` = u.`id` ' \
        'WHERE u.`del_flg` = 0 ' \
        'ORDER BY p.`created_at` DESC LIMIT 20'
      )
      posts = make_posts(results)

      erb :index, layout: :layout, locals: { posts: posts, me: me }
    end

    get '/@:account_name' do
      user = db.prepare('SELECT * FROM `users` WHERE `account_name` = ? AND `del_flg` = 0').execute(
        params[:account_name]
      ).first

      if user.nil?
        return 404
      end

      results = db.prepare('SELECT `id`, `user_id`, `body`, `mime`, `created_at` FROM `posts` WHERE `user_id` = ? ORDER BY `created_at` DESC LIMIT 20').execute(
        user[:id]
      )
      posts = make_posts(results)

      comment_count = db.prepare('SELECT COUNT(*) AS count FROM `comments` WHERE `user_id` = ?').execute(
        user[:id]
      ).first[:count]

      post_ids = db.prepare('SELECT `id` FROM `posts` WHERE `user_id` = ?').execute(
        user[:id]
      ).map{|post| post[:id]}
      post_count = post_ids.length

      commented_count = 0
      if post_count > 0
        placeholder = (['?'] * post_ids.length).join(",")
        commented_count = db.prepare("SELECT COUNT(*) AS count FROM `comments` WHERE `post_id` IN (#{placeholder})").execute(
          *post_ids
        ).first[:count]
      end

      me = get_session_user()

      erb :user, layout: :layout, locals: { posts: posts, user: user, post_count: post_count, comment_count: comment_count, commented_count: commented_count, me: me }
    end

    get '/posts' do
      max_created_at = params['max_created_at']
      results = db.prepare(
        'SELECT STRAIGHT_JOIN p.`id`, p.`user_id`, p.`body`, p.`mime`, p.`created_at` ' \
        'FROM `posts` p JOIN `users` u ON p.`user_id` = u.`id` ' \
        'WHERE u.`del_flg` = 0 AND p.`created_at` <= ? ' \
        'ORDER BY p.`created_at` DESC LIMIT 20'
      ).execute(
        max_created_at.nil? ? nil : Time.iso8601(max_created_at).localtime
      )
      posts = make_posts(results)

      erb :posts, layout: false, locals: { posts: posts }
    end

    get '/posts/:id' do
      results = db.prepare('SELECT * FROM `posts` WHERE `id` = ?').execute(
        params[:id]
      )
      posts = make_posts(results, all_comments: true)

      return 404 if posts.length == 0

      post = posts[0]

      me = get_session_user()

      erb :post, layout: :layout, locals: { post: post, me: me }
    end

    post '/' do
      me = get_session_user()

      if me.nil?
        redirect '/login', 302
      end

      if params['csrf_token'] != session[:csrf_token]
        return 422
      end

      if params['file']
        mime = ''
        # 投稿のContent-Typeからファイルのタイプを決定する
        if params["file"][:type].include? "jpeg"
          mime = "image/jpeg"
        elsif params["file"][:type].include? "png"
          mime = "image/png"
        elsif params["file"][:type].include? "gif"
          mime = "image/gif"
        else
          flash[:notice] = '投稿できる画像形式はjpgとpngとgifだけです'
          redirect '/', 302
        end

        if params['file'][:tempfile].read.length > UPLOAD_LIMIT
          flash[:notice] = 'ファイルサイズが大きすぎます'
          redirect '/', 302
        end

        params['file'][:tempfile].rewind
        imgdata = params["file"][:tempfile].read
        query = 'INSERT INTO `posts` (`user_id`, `mime`, `body`) VALUES (?,?,?)'
        db.prepare(query).execute(
          me[:id],
          mime,
          params["body"],
        )
        pid = db.last_id

        ext = IMAGE_EXT[mime]
        if ext
          FileUtils.mkdir_p(IMAGE_DIR) unless Dir.exist?(IMAGE_DIR)
          File.binwrite("#{IMAGE_DIR}/#{pid}.#{ext}", imgdata)
        end

        redirect "/posts/#{pid}", 302
      else
        flash[:notice] = '画像が必須です'
        redirect '/', 302
      end
    end

    get '/image/:id.:ext' do
      # nginx try_files で disk から返す。imgdata は DB 上に存在しないため、
      # ここに来るのは disk ミス時のみ。POST 直後の race 程度。
      404
    end

    post '/comment' do
      me = get_session_user()

      if me.nil?
        redirect '/login', 302
      end

      if params["csrf_token"] != session[:csrf_token]
        return 422
      end

      unless /\A[0-9]+\z/.match(params['post_id'])
        return 'post_idは整数のみです'
      end
      post_id = params['post_id']

      query = 'INSERT INTO `comments` (`post_id`, `user_id`, `comment`) VALUES (?,?,?)'
      db.prepare(query).execute(
        post_id,
        me[:id],
        params['comment']
      )

      redirect "/posts/#{post_id}", 302
    end

    get '/admin/banned' do
      me = get_session_user()

      if me.nil?
        redirect '/login', 302
      end

      if me[:authority] == 0
        return 403
      end

      users = db.query('SELECT * FROM `users` WHERE `authority` = 0 AND `del_flg` = 0 ORDER BY `created_at` DESC')

      erb :banned, layout: :layout, locals: { users: users, me: me }
    end

    post '/admin/banned' do
      me = get_session_user()

      if me.nil?
        redirect '/', 302
      end

      if me[:authority] == 0
        return 403
      end

      if params['csrf_token'] != session[:csrf_token]
        return 422
      end

      query = 'UPDATE `users` SET `del_flg` = ? WHERE `id` = ?'

      params['uid'].each do |id|
        db.prepare(query).execute(1, id.to_i)
      end

      redirect '/admin/banned', 302
    end
  end
end
