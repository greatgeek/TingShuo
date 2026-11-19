-- 生词本共享与收藏：共享=把本（含目录/词/例句）上架到资源版块；
-- 收藏两种模式：live 只读关注（实时看作者最新内容）、copy 复制独立副本。
BEGIN;

CREATE TABLE notebook_share (
    notebook_id TEXT PRIMARY KEY REFERENCES notebook (id) ON DELETE CASCADE,
    owner_id    TEXT NOT NULL REFERENCES "user" (id) ON DELETE CASCADE,
    intro       TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_nbshare_created ON notebook_share (created_at DESC);

CREATE TABLE notebook_favorite (
    id                 TEXT PRIMARY KEY,
    user_id            TEXT NOT NULL REFERENCES "user" (id) ON DELETE CASCADE,
    share_notebook_id  TEXT NOT NULL REFERENCES notebook_share (notebook_id) ON DELETE CASCADE,
    mode               TEXT NOT NULL CHECK (mode IN ('live', 'copy')),
    copied_notebook_id TEXT REFERENCES notebook (id) ON DELETE CASCADE,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, share_notebook_id),
    CHECK (mode = 'live' OR copied_notebook_id IS NOT NULL)
);
CREATE INDEX idx_nbfav_user ON notebook_favorite (user_id, created_at DESC);
CREATE INDEX idx_nbfav_share ON notebook_favorite (share_notebook_id);

COMMIT;
