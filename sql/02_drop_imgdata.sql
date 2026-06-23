-- iter6: imgdata は disk に出してあるので DB から削除し buffer pool を空ける
ALTER TABLE posts DROP COLUMN imgdata;
