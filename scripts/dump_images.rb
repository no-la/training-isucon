#!/usr/bin/env ruby
# 既存の posts.imgdata を /home/isucon/private_isu/webapp/public/image/ にダンプする
# 実行: サーバー上で `ruby scripts/dump_images.rb`

require 'mysql2'
require 'fileutils'

OUT_DIR = '/home/isucon/private_isu/webapp/public/image'
FileUtils.mkdir_p(OUT_DIR)

client = Mysql2::Client.new(
  host: 'localhost', username: 'isuconp', password: 'isuconp', database: 'isuconp',
  encoding: 'utf8mb4'
)

EXT = { 'image/jpeg' => 'jpg', 'image/png' => 'png', 'image/gif' => 'gif' }

n = 0
client.query('SELECT id, mime, imgdata FROM posts', stream: true, cache_rows: false).each do |row|
  ext = EXT[row['mime']]
  next unless ext
  path = "#{OUT_DIR}/#{row['id']}.#{ext}"
  next if File.exist?(path) && File.size(path) == row['imgdata'].bytesize
  File.binwrite(path, row['imgdata'])
  n += 1
  puts "  #{n}: #{path}" if n % 1000 == 0
end
puts "wrote #{n} files"
